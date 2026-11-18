package api

import (
	"fmt"
	"image"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// newQuietEmulator builds a vt.Emulator that does NOT answer terminal status
// queries. SOL output from a booted host contains DSR queries — cursor-position
// reports (ESC[6n), which BusyBox/ash emit at every shell prompt, and operating-
// status (ESC[5n). The default emulator answers each by writing the response
// into its own readable stream (io.Reader); since we never read that stream
// (we render the cast for display, we never talk back to a historical machine),
// the response buffer fills and the NEXT Write() BLOCKS FOREVER — hanging the
// whole cast replay once it reaches a shell prompt. (This is the real cause of
// the watch-TUI "catching up… freezes" symptom; it is size-independent.)
//
// Rather than drain the response pipe with a goroutine (which then races the
// unsynchronized e.closed flag on Close — a data race in the vt library), we
// suppress the responses at the source: register no-op handlers for the DSR
// commands that return true (handled), so the default response-writing handlers
// never run. Last-registered CSI handler wins and a true return short-circuits
// (vt handlers.go handleCsi), so this cleanly overrides the defaults. No
// goroutine, no Close, no race.
func newQuietEmulator(width, height int) *vt.Emulator {
	emu := vt.NewEmulator(width, height)
	suppress := func(ansi.Params) bool { return true }
	emu.RegisterCsiHandler('n', suppress)                       // DSR (CPR ESC[6n, status ESC[5n)
	emu.RegisterCsiHandler(ansi.Command('?', 0, 'n'), suppress) // DEC private DSR (ESC[?6n)
	return emu
}

// This file builds the SOL "screen snapshot" that a freshly-attaching watch
// client gets instead of the full console ring buffer. The cast table stores
// every raw SOL byte ever seen for a machine (no trimming); a
// host that has booted through BIOS POST + iPXE accumulates hundreds of KB of
// cursor-addressing ANSI. Streaming all of it to the client floods the
// single-threaded vt emulator inside its Bubble Tea loop and freezes the UI.
// But the client doesn't NEED the whole history to render a live
// monitor: SOL is full-screen cursor-addressed, so the only thing that matters
// on attach is the current 80x24 screen.
//
// So the server replays the entire cast through its OWN throwaway emulator,
// renders the resulting screen, and emits a compact ANSI sequence that
// reconstructs that exact screen in the client's emulator — a few KB regardless
// of ring size. The client feeds it through the same onBytes path as live
// bytes (no wire-format change), then tails live frames from the live edge.
//
// This is faithful ONLY while nothing trims the cast: the replay starts at the
// true session start (t_us=0), so every screen-setup escape is present. Adding
// retention requires screen keyframes first — replaying from a
// mid-stream survivor would miss the escapes that set the screen up.

// renderCastScreenANSI replays the ordered raw cast event byte chunks through a
// throwaway width x height vt emulator and returns an ANSI byte sequence that,
// fed into a fresh emulator of the same size, reproduces the resulting screen
// (cell text + final cursor position). Colors/attributes are flattened to plain
// text: the historical screen renders uncolored, which is acceptable for a SOL
// monitor (live bytes after the snapshot carry full attributes). Returns an
// empty slice when there is nothing to render (no events), so the caller can
// skip emitting a snapshot frame.
func renderCastScreenANSI(chunks [][]byte, width, height int) []byte {
	if len(chunks) == 0 {
		return nil
	}
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	emu := newQuietEmulator(width, height)
	defer emu.Close() //nolint:errcheck
	for _, c := range chunks {
		_, _ = emu.Write(c) // vt consumes everything; never a meaningful error
	}
	return reproduceScreenANSI(emu.String(), emu.CursorPosition(), height)
}

// reproduceScreenANSI turns a rendered screen (vt.Emulator.String(): height
// lines separated by \n) plus the final cursor position into the ANSI needed to
// repaint it: clear, then one cursor-positioned write per non-blank line, then
// park the cursor where the source emulator left it so resuming live bytes draw
// in the right place. Trailing spaces are trimmed (position-addressed, so they
// don't matter) and blank lines are skipped to keep the frame small.
func reproduceScreenANSI(screen string, cursor image.Point, height int) []byte {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H") // clear screen, cursor home
	lines := strings.Split(screen, "\n")
	for y := 0; y < height && y < len(lines); y++ {
		line := strings.TrimRight(lines[y], " ")
		if line == "" {
			continue
		}
		// 1-indexed row;col cursor address, then the line text.
		fmt.Fprintf(&b, "\x1b[%d;1H%s", y+1, line)
	}
	// Restore the live cursor (vt positions are 0-indexed; ANSI is 1-indexed).
	row := cursor.Y + 1
	col := cursor.X + 1
	if row < 1 {
		row = 1
	}
	if col < 1 {
		col = 1
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", row, col)
	return []byte(b.String())
}
