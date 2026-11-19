package format

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
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
// Column widths are computed from plain text (ANSI stripped) so that
// invisible escape sequences don't inflate padding. The original styled
// cell content is preserved in the output.
func PrintTable(w io.Writer, headers []string, rows [][]string) {
	numCols := len(headers)
	colWidths := make([]int, numCols)
	for i, h := range headers {
		if n := utf8.RuneCountInString(h); n > colWidths[i] {
			colWidths[i] = n
		}
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= numCols {
				break
			}
			if n := utf8.RuneCountInString(StripANSI(cell)); n > colWidths[i] {
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
				plainWidth := utf8.RuneCountInString(StripANSI(cell))
				line.WriteString(strings.Repeat(" ", colWidths[i]-plainWidth+minPad))
			}
		}
		return line.String()
	}

	fmt.Fprintln(w, HeaderStyle.Render(writeLine(headers)))
	for _, row := range rows {
		fmt.Fprintln(w, writeLine(row))
	}
}

// Untrusted makes text that a provisioned host controls safe to print to the
// operator's terminal. Agent log envelopes reach global through the site's
// unauthenticated /logs/ingest, and commission data and progress reports
// (phase, error) are the target's own report, so their strings can carry
// escape sequences that erase the screen, forge or overwrite lines, plant
// hyperlinks (OSC 8) or write the clipboard (OSC 52).
//
// Untrusted replaces each C0 control, DEL and C1 control with a visible
// escape (\n, \r, \x1b, \u009b) and each byte that is not valid UTF-8 with
// \xNN, so the operator sees that something was there and the terminal acts
// on none of it. A tab becomes one space: it is common in tool output and
// harmless to the terminal, but it breaks table and box alignment. The result
// is for display only and is not reversible (a literal backslash is left as
// is); --json output carries the original value.
//
// Apply it to the raw value before any styling: slagctl's own escapes are
// added afterwards and are not affected.
func Untrusted(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, isTerminalControl) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\t':
			b.WriteByte(' ')
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r < 0xa0:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// isTerminalControl reports a rune a terminal interprets rather than prints:
// a C0 control, DEL or a C1 control.
func isTerminalControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
}

// StripANSI removes ANSI escape sequences from a string.
func StripANSI(s string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}
