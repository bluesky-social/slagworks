package tui

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// plain strips the styling from rendered output. Lip Gloss emits full-color
// escape codes on every Render (the terminal-facing downsampling is Bubble
// Tea's), so the goldens and the text assertions pin the visible layout of the
// stripped string, and color_test asserts the styling itself.
func plain(s string) string { return ansi.Strip(s) }

// updateGolden regenerates the committed golden frames:
//
//	go test ./cmd/slagctl/tui/ -update
//
// Review = reading the .golden diff. Registered as a real test flag so it
// parses cleanly alongside go test's own flags.
var updateGolden = flag.Bool("update", false, "regenerate golden frames")

// goldenScreen compares got against testdata/<name>.golden, regenerating on
// -update. Trailing whitespace per line is preserved (the vt screen pads to
// 80 cols, and that padding is part of what we render).
func goldenScreen(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("screen mismatch vs %s\n--- got ---\n%s\n--- want ---\n%s",
			path, got, want)
	}
}

// TestConsole_RendersSimpleText is the determinism floor: a fixed byte
// sequence through Apply produces a stable 80x24 screen, with no clock or
// goroutine involved (the whole point of the pure-Apply design — plan D2).
func TestConsole_RendersSimpleText(t *testing.T) {
	c := newConsolePane()
	c.Apply([]byte("slagworks-node login: "))
	screen := c.Screen()

	// First line carries the text; the emulator pads to 80 cols and fills
	// 24 rows. Assert the content lands on row 1 without asserting exact
	// padding (that's what the golden pins).
	first, _, _ := strings.Cut(screen, "\n")
	if !strings.Contains(first, "slagworks-node login:") {
		t.Errorf("first line = %q, want it to contain the login prompt", first)
	}
	if got := strings.Count(screen, "\n"); got != consoleHeight-1 {
		t.Errorf("screen has %d newlines, want %d (a %d-row grid)",
			got, consoleHeight-1, consoleHeight)
	}
	goldenScreen(t, "console_simple", screen)
}

// TestConsole_DSRQueryDoesNotHang is the regression for the live-TUI freeze:
// SOL output from a booted host carries DSR cursor-position queries (ESC[6n) —
// BusyBox/ash emit one at every shell prompt. The vt emulator answers a query
// by writing into its own readable stream; without a drain, the next Apply →
// emu.Write blocks forever once that buffer fills, wedging the Bubble Tea loop.
// newConsolePane drains the emulator, so feeding many ESC[6n must not hang.
func TestConsole_DSRQueryDoesNotHang(t *testing.T) {
	done := make(chan struct{})
	go func() {
		c := newConsolePane()
		for range 2000 {
			c.Apply([]byte("\r\n# \x1b[6n")) // shell prompt + cursor-position query
		}
		c.Apply([]byte("\x1b[2J\x1b[1;1Hdone"))
		_ = c.Screen()
		close(done)
	}()
	select {
	case <-done:
		// ok
	case <-time.After(5 * time.Second):
		t.Fatal("console pane HUNG on DSR cursor-position queries (ESC[6n) — drain missing")
	}
}

// TestConsole_DSRSuppressedNoGoroutine guards that the DSR fix is goroutine-free
// (responses suppressed at the source, not drained by a background reader): open
// many panes feeding DSR queries and assert the goroutine count doesn't grow, so
// the pane needs no Close/cleanup and can't leak.
func TestConsole_DSRSuppressedNoGoroutine(t *testing.T) {
	c0 := newConsolePane()
	c0.Apply([]byte("# \x1b[6n"))
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()
	for range 200 {
		c := newConsolePane()
		c.Apply([]byte("# \x1b[6n booted")) // a DSR query each time
		_ = c.Screen()
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	t.Logf("goroutines before=%d after=%d (200 panes)", before, after)
	if after > before+2 {
		t.Errorf("DSR handling must not spawn goroutines: before=%d after=%d", before, after)
	}
}

// TestConsole_CursorAddressingCollapses proves the reason we use a vt
// emulator instead of byte-tailing: cursor-addressing escapes (here, "move
// to row 1 col 1 and overwrite") must collapse to the final screen state,
// not accumulate. A naive append would show both the old and new text.
func TestConsole_CursorAddressingCollapses(t *testing.T) {
	c := newConsolePane()
	// Write "FIRST", then ESC[H (cursor home) + overwrite with "SECOND".
	c.Apply([]byte("FIRST"))
	c.Apply([]byte("\x1b[H" + "SECOND"))
	first, _, _ := strings.Cut(c.Screen(), "\n")
	if strings.Contains(first, "FIRST") && !strings.HasPrefix(strings.TrimSpace(first), "SECOND") {
		t.Errorf("cursor-home overwrite did not collapse: first line = %q", first)
	}
	if !strings.HasPrefix(first, "SECOND") {
		t.Errorf("expected overwritten line to start with SECOND, got %q", first)
	}
}

// TestConsole_RealSOLCapture renders a real BMC SOL capture (a full deploy's
// BIOS POST + boot, with all the firmware cursor-dance ANSI) and snapshots
// the resulting screen. This is the proof that the console pane handles real
// hardware output, not just toy input. The fixture is a raw byte capture
// from a Supermicro host, checked into testdata/ with its MAC and management
// addresses replaced by same-length placeholders.
func TestConsole_RealSOLCapture(t *testing.T) {
	const fixture = "testdata/real_sol_capture.raw"
	b, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("fixture %s missing (checked into the repo; broken checkout?): %v", fixture, err)
	}
	c := newConsolePane()
	c.Apply(b)
	screen := c.Screen()
	// Sanity: a real capture must produce a 24-row grid and not be empty.
	if got := strings.Count(screen, "\n"); got != consoleHeight-1 {
		t.Errorf("screen has %d newlines, want %d", got, consoleHeight-1)
	}
	if strings.TrimSpace(screen) == "" {
		t.Error("real SOL capture rendered an empty screen")
	}
	goldenScreen(t, "console_real_sol", screen)
}

// TestStaticScreen_NoSignal covers the "serial console not wired up" state:
// a console pane that has never received SOL bytes renders TV-snow static into
// the console box instead of 24 rows of blank space. The snow is seeded by now,
// so a fixed test clock produces a byte-stable grid (the golden) while a live
// TUI's 10Hz heartbeat re-rolls it into flicker. The first real byte flips
// hasSignal and the frame stops calling staticScreen.
func TestStaticScreen_NoSignal(t *testing.T) {
	c := newConsolePane()
	if c.hasSignal() {
		t.Fatal("fresh console pane should report no signal (bytesApplied==0)")
	}

	// Determinism: same seed → identical snow, run twice.
	seed := fixedClock(at(42 * time.Second))()
	a := staticScreen(seed, consoleWidth, consoleHeight)
	b := staticScreen(seed, consoleWidth, consoleHeight)
	if a != b {
		t.Fatal("staticScreen is not deterministic for a fixed now — goldens would be unstable")
	}

	// Different seed → different snow (the flicker the live TUI relies on).
	other := staticScreen(fixedClock(at(43*time.Second))(), consoleWidth, consoleHeight)
	if a == other {
		t.Error("staticScreen did not change across ticks — no flicker in a live TUI")
	}

	// Shape: exactly consoleHeight rows of consoleWidth visible columns. The
	// dim-gray style wraps each row in escape codes, so measure visible width
	// with lipgloss.Width, not len.
	styled := staticScreen(seed, consoleWidth, consoleHeight)
	lines := strings.Split(styled, "\n")
	if got := len(lines); got != consoleHeight {
		t.Errorf("static has %d rows, want %d", got, consoleHeight)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w != consoleWidth {
			t.Errorf("static row %d width = %d, want %d", i, w, consoleWidth)
		}
	}
	// Content: it must actually be snow (contain block-shade chars), not the
	// blank box it replaced, and not a single solid fill.
	snow := plain(styled)
	if strings.TrimSpace(snow) == "" {
		t.Error("static screen is blank — the empty box it was meant to replace")
	}
	for _, shade := range []string{"░", "▒", "▓", "█"} {
		if !strings.Contains(snow, shade) {
			t.Errorf("static screen missing shade %q — snow should span the block palette", shade)
		}
	}

	// Golden pins the exact snow for the fixed seed.
	goldenScreen(t, "console_static", snow)

	// Once real SOL bytes arrive, hasSignal flips and the frame renders the
	// emulator screen, not static.
	c.Apply([]byte("slagworks-node login: "))
	if !c.hasSignal() {
		t.Error("hasSignal should be true after Apply received SOL bytes")
	}
	if strings.Contains(c.Screen(), "░") && !strings.Contains(c.Screen(), "slagworks-node login:") {
		t.Error("after bytes arrive the live screen should render, not static")
	}
}
