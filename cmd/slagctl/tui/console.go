// Package tui implements the slagctl deploy/commission monitoring TUI
// (slagctl machine deploy -w / operation watch).
//
// This file is the SOL console pane: a fixed 80x24 terminal-emulator view
// fed by the asciinema cast stream. It renders BMC serial output (BIOS POST,
// boot, OS console) faithfully by feeding decoded bytes into a vt screen
// model — a naive byte-append would corrupt on the cursor-addressing ANSI
// that firmware emits.
package tui

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Console geometry. The server-side cast is 80x24 by default
// (model/console_cast.go), so the pane maps 1:1 with no resampling.
const (
	consoleWidth  = 80
	consoleHeight = 24
)

// consolePane owns the vt emulator and renders the SOL screen. The emulator
// is single-owned (touched only from Update via Apply), so a plain
// vt.Emulator suffices — no mutex, no SafeEmulator.
//
// This is the first slice: emulator + Apply + Screen + golden harness. The
// catch-up placeholder (plan D2 — render "catching up…" while an initial
// backfill is still draining, to avoid flashing intermediate states) lands
// with the View/composition slice, not here, so there's no dead state field
// in the meantime.
type consolePane struct {
	emu *vt.Emulator
	// bytesApplied is a cheap monotonic counter for status/debug; not
	// rendered, but lets tests and (later) the summary box show progress.
	bytesApplied int64
}

func newConsolePane() *consolePane {
	// Suppress terminal status-query responses (DSR). SOL output from a booted
	// host contains DSR cursor-position queries (ESC[6n) — BusyBox/ash emit one
	// at every shell prompt. The default emulator answers by writing into its
	// own io.Reader; since the TUI never reads that stream, the buffer fills and
	// the next Apply()→emu.Write BLOCKS FOREVER, wedging the entire Bubble Tea
	// Update loop (frozen panes, dead keys) the moment a shell prompt arrives.
	// Registering no-op DSR handlers (return true = handled) stops the response
	// at the source — no pipe write, no backpressure, no goroutine to leak or
	// race on Close. We never talk back to a SOL session through this emulator.
	emu := vt.NewEmulator(consoleWidth, consoleHeight)
	suppress := func(ansi.Params) bool { return true }
	emu.RegisterCsiHandler('n', suppress)
	emu.RegisterCsiHandler(ansi.Command('?', 0, 'n'), suppress)
	return &consolePane{emu: emu}
}

// Apply writes a batch of decoded SOL bytes into the emulator. Called only
// from the model's Update (single-owner invariant). Returns the receiver for
// chaining in tests.
func (c *consolePane) Apply(data []byte) {
	if len(data) == 0 {
		return
	}
	// vt.Emulator.Write never returns a meaningful error for terminal byte
	// streams (it consumes everything); ignore per the emulator contract.
	_, _ = c.emu.Write(data)
	c.bytesApplied += int64(len(data))
}

// Screen returns the current 80x24 rendered screen as a string. This is what
// the pane contributes to the composed View; golden tests snapshot it
// directly.
func (c *consolePane) Screen() string {
	return c.emu.String()
}

// hasSignal reports whether the SOL feed has produced any bytes at all. Until
// it has, the console box would otherwise be 24 rows of blank space — an empty
// box that reads as "broken" without saying so. The frame substitutes TV-snow
// static for that empty state (see staticScreen), so an unwired serial console
// shows "no signal" noise instead of a blank rectangle.
func (c *consolePane) hasSignal() bool {
	return c.bytesApplied > 0
}
