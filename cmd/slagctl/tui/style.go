package tui

import (
	"charm.land/lipgloss/v2"
)

// TUI color palette, aligned with cmd/slagctl/format/style.go (green=ready,
// yellow=active, red=failed, gray=dim) so the TUI matches the rest of slagctl.
//
// Styling is applied as a post-layout pass over already-laid-out plain strings:
// lipgloss escape codes inflate len() but lipgloss.Width() ignores them, and
// all layout math in this package that must survive coloring uses
// lipgloss.Width, never len. Render always emits full-color escape codes;
// Bubble Tea downsamples them to the color profile of the terminal it draws
// on, so color depth is purely a terminal-time effect. Golden tests compare
// the ANSI-stripped frame and assert color separately.
var (
	styleDone   = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // green ✓
	styleActive = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // yellow current
	styleFailed = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // red ✗
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // gray pending/skipped
	styleAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true)
	styleLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("245")) // summary labels
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
)
