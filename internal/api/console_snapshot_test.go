package api

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// feedANSI runs a byte sequence through a fresh 80x24 emulator and returns its
// rendered screen — the reference for "what a client emulator would show".
func feedANSI(seq []byte) string {
	e := vt.NewEmulator(80, 24)
	defer e.Close() //nolint:errcheck
	_, _ = e.Write(seq)
	return e.String()
}

// TestRenderCastScreenANSI_ReproducesScreen is the core guarantee: replaying
// the cast chunks server-side and shipping renderCastScreenANSI's output must
// make a fresh client emulator render the SAME screen as feeding the raw cast
// would have — but in a fraction of the bytes. This is what lets the watch TUI
// attach to a long-running op without streaming (and re-emulating) the whole
// ring buffer.
func TestRenderCastScreenANSI_ReproducesScreen(t *testing.T) {
	const w, h = 80, 24
	// A cast that uses cursor addressing and overwrites earlier content — the
	// case a naive byte-append (or mid-stream replay) corrupts.
	chunks := [][]byte{
		[]byte("\x1b[2J\x1b[H"),        // clear
		[]byte("\x1b[1;1HBIOS POST"),   // top-left
		[]byte("\x1b[5;10Hbooting…"),   // mid
		[]byte("\x1b[1;1HiPXE 1.21.1"), // OVERWRITE the BIOS line in place
		[]byte("\x1b[24;1Hlast line"),  // bottom
	}

	rawScreen := feedANSI(concatAll(chunks))

	snapshot := renderCastScreenANSI(chunks, w, h)
	if len(snapshot) == 0 {
		t.Fatal("expected a non-empty snapshot for non-empty cast")
	}
	gotScreen := feedANSI(snapshot)

	if gotScreen != rawScreen {
		t.Errorf("snapshot screen mismatch:\n got=%q\nwant=%q", gotScreen, rawScreen)
	}
	// The overwrite must have taken effect — iPXE, not BIOS, on the top line.
	if !strings.Contains(gotScreen, "iPXE 1.21.1") {
		t.Errorf("top line should show the overwriting text, got:\n%q", gotScreen)
	}
	if strings.Contains(gotScreen, "BIOS POST") {
		t.Errorf("overwritten BIOS text must not survive, got:\n%q", gotScreen)
	}
}

// TestRenderCastScreenANSI_Empty: no events -> no snapshot frame (the caller
// skips emitting one).
func TestRenderCastScreenANSI_Empty(t *testing.T) {
	if got := renderCastScreenANSI(nil, 80, 24); got != nil {
		t.Errorf("empty cast should render nil snapshot, got %q", got)
	}
	if got := renderCastScreenANSI([][]byte{}, 80, 24); got != nil {
		t.Errorf("empty cast should render nil snapshot, got %q", got)
	}
}

// TestRenderCastScreenANSI_Compresses: the whole point — a large cast that
// repeatedly redraws the same screen compresses to roughly one screen's worth
// of bytes, not the cast's full size. Guards the freeze fix from regressing
// into "ship the whole ring again".
func TestRenderCastScreenANSI_Compresses(t *testing.T) {
	const w, h = 80, 24
	var chunks [][]byte
	// 5000 redraws of a status line = a big ring buffer, one live screen.
	for range 5000 {
		chunks = append(chunks, []byte("\x1b[1;1Hstatus: still booting, tick\x1b[K"))
	}
	rawSize := len(concatAll(chunks))
	snapshot := renderCastScreenANSI(chunks, w, h)
	if len(snapshot) == 0 {
		t.Fatal("expected a snapshot")
	}
	if len(snapshot) >= rawSize/10 {
		t.Errorf("snapshot (%d bytes) should be far smaller than the raw cast (%d bytes)",
			len(snapshot), rawSize)
	}
}

// TestRenderCastScreenANSI_DSRQueryNoHang is the regression for the real-world
// hang: SOL output from a booted host contains DSR cursor-position queries
// (ESC[6n) — BusyBox/ash emit one at every shell prompt. The emulator answers
// by writing into its own readable stream; without a drain, Write blocks
// forever once that buffer fills, hanging the whole snapshot replay (and,
// client-side, the TUI loop). This feeds many ESC[6n queries and asserts the
// render completes within a deadline.
func TestRenderCastScreenANSI_DSRQueryNoHang(t *testing.T) {
	var chunks [][]byte
	// A shell-prompt pattern with a DSR query, repeated — mirrors a cast that
	// reached an interactive busybox prompt that redraws repeatedly.
	for range 2000 {
		chunks = append(chunks, []byte("\r\n# \x1b[6n"))
	}
	chunks = append(chunks, []byte("\x1b[2J\x1b[1;1Hfinal screen"))

	done := make(chan []byte, 1)
	go func() { done <- renderCastScreenANSI(chunks, 80, 24) }()
	select {
	case frame := <-done:
		if len(frame) == 0 {
			t.Fatal("expected a non-empty frame")
		}
		if !strings.Contains(feedANSI(frame), "final screen") {
			t.Errorf("snapshot should reflect the final screen, got:\n%q", feedANSI(frame))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("renderCastScreenANSI HUNG on DSR cursor-position queries (ESC[6n)")
	}
}

func concatAll(chunks [][]byte) []byte {
	var out []byte
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}
