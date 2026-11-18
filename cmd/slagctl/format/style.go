package format

import "charm.land/lipgloss/v2"

// Style is a lipgloss style for slagctl's line-oriented output. Lip Gloss
// renders full-color escape codes unconditionally and leaves downsampling to
// the writer, but slagctl prints styled strings with plain fmt calls and
// returns them for tables and one-line summaries, so Render downsamples to the
// color profile Lip Gloss detected for stdout (lipgloss.Writer): plain text
// when stdout is not a terminal or TERM is dumb, no color under NO_COLOR.
type Style struct{ style lipgloss.Style }

// Render applies the style to strs, downsampled for stdout's color profile.
func (s Style) Render(strs ...string) string {
	return lipgloss.Sprint(s.style.Render(strs...))
}

var (
	// HeaderStyle renders table header cells.
	HeaderStyle = Style{lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))}

	// State styles
	StateReady  = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("10"))} // bright green
	StateActive = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("11"))} // bright yellow
	StateFailed = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("9"))}  // bright red
	StateNew    = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("8"))}  // gray

	// DimStyle for secondary information.
	DimStyle = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("8"))}

	// WarnStyle for warnings printed to stderr.
	WarnStyle = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)}

	// SuccessStyle for positive confirmations.
	SuccessStyle = Style{lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)}
)
