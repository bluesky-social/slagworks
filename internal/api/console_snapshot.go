package api

import (
	"context"
	"fmt"
	"image"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/bluesky-social/slagworks/internal/model"
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
// The replay costs CPU in proportion to the cast's history, but global's
// memory stays bounded by one page of rows plus one screen: each page is
// written into the emulator as it is read and then dropped, and the emulator
// keeps no scrollback. Cast events are kept forever, so anything held per
// row would grow for the life of the machine, on every attach.
//
// This is faithful ONLY while nothing trims the cast: the replay starts at the
// true session start (t_us=0), so every screen-setup escape is present. Adding
// retention requires screen keyframes first — replaying from a
// mid-stream survivor would miss the escapes that set the screen up.

// castSnapshot replays cast from t_us=0 up to the live edge, pageSize rows at
// a time, and returns the reconstructed-screen frame (nil for a cast with no
// events) and the cursor just past the last row replayed, where a follow
// stream continues. The emulator lives only for this call, so a follow
// stream that runs for hours afterward does not keep it alive.
func (s *Server) castSnapshot(ctx context.Context, cast *model.ConsoleCast, pageSize int) ([]byte, int64, error) {
	screen := newCastScreen(cast.Width, cast.Height)
	defer screen.Close()
	cursor := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		rows, err := s.consoleStore.CastEventsSince(ctx, cast.ID, cursor, pageSize)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			screen.Write(r.Data)
			cursor = r.TUs + 1
		}
		if len(rows) < pageSize {
			return screen.ANSI(), cursor, nil // caught up to the live edge
		}
	}
}

// castScreen is a throwaway width x height emulator that cast bytes are
// written into in order, one chunk at a time, and that then renders the
// resulting screen as a snapshot.
type castScreen struct {
	emu     *vt.Emulator
	height  int
	written bool
}

func newCastScreen(width, height int) *castScreen {
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	emu := newQuietEmulator(width, height)
	// The snapshot reads only the visible screen. vt otherwise keeps a
	// DefaultScrollbackSize (10,000-line) scrollback of full cell rows, about
	// 90 MiB once a long cast has scrolled that far. 1 is the smallest size
	// vt accepts (0 selects its default). It caps the main screen only; vt
	// exposes no setter for the alternate screen's scrollback, which a SOL
	// console reaches only through a full-screen program scrolling in it.
	emu.SetScrollbackSize(1)
	return &castScreen{emu: emu, height: height}
}

// Write feeds the next chunk of cast bytes.
func (s *castScreen) Write(b []byte) {
	s.written = true
	_, _ = s.emu.Write(b) // vt consumes everything; never a meaningful error
}

// ANSI returns an ANSI byte sequence that, fed into a fresh emulator of the
// same size, reproduces the screen the written bytes produced (cell text +
// final cursor position). Colors/attributes are flattened to plain text: the
// historical screen renders uncolored, which is acceptable for a SOL monitor
// (live bytes after the snapshot carry full attributes). Returns nil when
// nothing was written (no events), so the caller can skip emitting a snapshot
// frame.
func (s *castScreen) ANSI() []byte {
	if !s.written {
		return nil
	}
	return reproduceScreenANSI(s.emu.String(), s.emu.CursorPosition(), s.height)
}

func (s *castScreen) Close() {
	_ = s.emu.Close()
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
