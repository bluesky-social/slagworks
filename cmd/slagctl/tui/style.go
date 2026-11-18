package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TUI color palette, aligned with cmd/slagctl/format/style.go (green=ready,
// yellow=active, red=failed, gray=dim) so the TUI matches the rest of slagctl.
//
// Styling is applied as a post-layout pass over already-laid-out plain strings:
// lipgloss escape codes inflate len() but lipgloss.Width() ignores them, and
// all layout math in this package that must survive coloring uses
// lipgloss.Width, never len. Crucially lipgloss emits NO codes on a non-TTY
// (verified), so `go test` output is plain and the goldens stay byte-stable —
// the color is purely a terminal-time effect. Tests that want to be belt-and-
// suspenders can call forceNoColor().
var (
	styleDone   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green ✓
	styleActive = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // yellow current
	styleFailed = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red ✗
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // gray pending/skipped
	styleAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
	styleLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // summary labels
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)

// forceNoColor pins lipgloss to the ASCII profile process-wide, so styled
// output is plain regardless of the terminal. Used by golden tests as defense
// in depth (lipgloss already strips color on a non-TTY, but a test run under a
// TTY or with FORCE_COLOR set would otherwise bleed escape codes into the
// snapshot).
func forceNoColor() {
	lipgloss.SetColorProfile(termenv.Ascii)
}
