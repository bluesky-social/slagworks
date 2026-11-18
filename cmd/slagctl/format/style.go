package format

import "github.com/charmbracelet/lipgloss"

var (
	// HeaderStyle renders table header cells.
	HeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))

	// State styles
	StateReady  = lipgloss.NewStyle().Foreground(lipgloss.Color("10")) // bright green
	StateActive = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // bright yellow
	StateFailed = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))  // bright red
	StateNew    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // gray

	// DimStyle for secondary information.
	DimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	// WarnStyle for warnings printed to stderr.
	WarnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)

	// SuccessStyle for positive confirmations.
	SuccessStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
)
