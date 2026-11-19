package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// State/status icons.
const (
	IconReady  = "●" // U+25CF
	IconNew    = "○" // U+25CB
	IconActive = "⟳" // U+27F3
	IconFailed = "✗" // U+2717
)

// MachineStateLabel returns a colored icon+state string for display.
// State set: new / commissioning / commissioned / deploying / running /
// failed (see internal/model/machine.go).
func MachineStateLabel(state string) string {
	switch state {
	case "commissioned":
		return StateReady.Render(IconReady + " commissioned")
	case "running":
		return StateReady.Render(IconReady + " running")
	case "commissioning":
		return StateActive.Render(IconActive + " commissioning")
	case "deploying":
		return StateActive.Render(IconActive + " deploying")
	case "failed":
		return StateFailed.Render(IconFailed + " failed")
	case "new":
		return StateNew.Render(IconNew + " new")
	default:
		return state
	}
}

// PowerStateLabel returns a colored icon+state string for a BMC power
// state ("on" / "off" / "unknown", per internal/power.State). The
// capitalized form ("On") matches what operators see in BMC UIs.
func PowerStateLabel(state string) string {
	switch state {
	case "on":
		return StateReady.Render(IconReady + " On")
	case "off":
		return StateNew.Render(IconNew + " Off")
	case "unknown":
		return StateFailed.Render(IconFailed + " Unknown")
	default:
		return state
	}
}

// OperationStatusLabel returns a colored icon+status string for display.
func OperationStatusLabel(status string) string {
	switch status {
	case "completed":
		return StateReady.Render(IconReady + " completed")
	case "running":
		return StateActive.Render(IconActive + " running")
	case "claimed":
		return StateActive.Render(IconActive + " claimed")
	case "pending":
		return StateNew.Render(IconNew + " pending")
	case "failed":
		return StateFailed.Render(IconFailed + " failed")
	default:
		return status
	}
}

// HumanDuration returns a human-readable relative duration string.
func HumanDuration(t time.Time) string {
	d := time.Since(t)
	if d < 5*time.Second {
		return "just now"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// ShortDuration renders a compact, no-suffix elapsed string for a
// "time since last update" column: e.g. "3s", "12s", "5m", "1h2m",
// "2d3h". Unlike HumanDuration it carries no "ago" and keeps two
// significant units for h/d so a stalled op reads precisely ("1h2m"
// not "1h"). Negative inputs (clock skew) clamp to "0s".
func ShortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		if h == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// FormatBytes returns a human-readable file size string.
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// PrintJSON encodes v as indented JSON to w.
func PrintJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// PrintTable writes a formatted table with a bold header row.
// Column widths are measured in terminal display cells (ansi.StringWidth):
// escape sequences count as zero and wide characters such as CJK as two,
// so every column starts at the same terminal column on every line. The
// original styled cell content is preserved in the output.
func PrintTable(w io.Writer, headers []string, rows [][]string) {
	numCols := len(headers)
	colWidths := make([]int, numCols)
	for i, h := range headers {
		if n := ansi.StringWidth(h); n > colWidths[i] {
			colWidths[i] = n
		}
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= numCols {
				break
			}
			if n := ansi.StringWidth(cell); n > colWidths[i] {
				colWidths[i] = n
			}
		}
	}

	const minPad = 2
	writeLine := func(cells []string) string {
		var line strings.Builder
		for i, cell := range cells {
			if i >= numCols {
				break
			}
			line.WriteString(cell)
			if i < numCols-1 {
				line.WriteString(strings.Repeat(" ", colWidths[i]-ansi.StringWidth(cell)+minPad))
			}
		}
		return line.String()
	}

	fmt.Fprintln(w, HeaderStyle.Render(writeLine(headers)))
	for _, row := range rows {
		fmt.Fprintln(w, writeLine(row))
	}
}
