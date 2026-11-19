package format_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/charmbracelet/x/ansi"
)

func TestHumanDuration(t *testing.T) {
	now := time.Now()
	tests := []struct {
		offset time.Duration
		want   string
	}{
		{2 * time.Second, "just now"},
		{45 * time.Second, "45s ago"},
		{90 * time.Second, "1m ago"},
		{3 * time.Hour, "3h ago"},
		{48 * time.Hour, "2d ago"},
	}
	for _, tt := range tests {
		got := format.HumanDuration(now.Add(-tt.offset))
		if got != tt.want {
			t.Errorf("HumanDuration(-%v) = %q, want %q", tt.offset, got, tt.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"}, // clock skew clamps to 0
		{0, "0s"},
		{3 * time.Second, "3s"},
		{12 * time.Second, "12s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m30s"},
		{5 * time.Minute, "5m"},
		{62 * time.Minute, "1h2m"},
		{time.Hour, "1h"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},
	}
	for _, tt := range tests {
		got := format.ShortDuration(tt.d)
		if got != tt.want {
			t.Errorf("ShortDuration(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
	}
	for _, tt := range tests {
		got := format.FormatBytes(tt.bytes)
		if got != tt.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestMachineStateLabel(t *testing.T) {
	tests := []struct {
		state    string
		wantIcon string
	}{
		{"commissioned", format.IconReady},
		{"running", format.IconReady},
		{"commissioning", format.IconActive},
		{"deploying", format.IconActive},
		{"failed", format.IconFailed},
		{"new", format.IconNew},
	}
	for _, tt := range tests {
		got := format.MachineStateLabel(tt.state)
		if !strings.Contains(got, tt.wantIcon) {
			t.Errorf("MachineStateLabel(%q): expected icon %q in output %q", tt.state, tt.wantIcon, got)
		}
		if !strings.Contains(got, tt.state) {
			t.Errorf("MachineStateLabel(%q): expected state name in output %q", tt.state, got)
		}
	}
}

func TestPrintTable_ANSIAlignment(t *testing.T) {
	// PrintTable must produce visually aligned columns even when cells
	// contain ANSI escape codes. lipgloss strips ANSI in non-TTY test
	// environments, so we inject raw ANSI strings to simulate real
	// terminal output. The test verifies that after stripping ANSI from
	// the rendered output, all column start positions match across rows.
	styled := func(s string) string {
		return "\033[1;38;5;10m" + s + "\033[0m"
	}

	var buf strings.Builder
	headers := []string{"ID", "STATE", "AGE"}
	rows := [][]string{
		{"3aedce78-e1a2-4d4a-b2c3-0a1d700e111e", styled("● ready"), "1h"},
		{"f00dcafe-0000-0000-0000-000000000000", styled("⟳ commissioning"), "2d"},
	}
	format.PrintTable(&buf, headers, rows)
	output := buf.String()
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), output)
	}

	// Strip ANSI and find column start positions in each line.
	var allStarts [][]int
	var strippedLines []string
	for _, line := range lines {
		s := ansi.Strip(line)
		strippedLines = append(strippedLines, s)
		allStarts = append(allStarts, columnStarts(s))
	}

	headerStarts := allStarts[0]
	if len(headerStarts) != 3 {
		t.Fatalf("expected 3 columns in header, got %d: %q", len(headerStarts), strippedLines[0])
	}

	for i, starts := range allStarts[1:] {
		if len(starts) != len(headerStarts) {
			t.Errorf("row %d: expected %d columns, got %d\nheader: %q\nrow:    %q",
				i, len(headerStarts), len(starts), strippedLines[0], strippedLines[i+1])
			continue
		}
		for col := range starts {
			if starts[col] != headerStarts[col] {
				t.Errorf("row %d, col %d: starts at rune %d, header at %d\nheader: %q\nrow:    %q",
					i, col, starts[col], headerStarts[col], strippedLines[0], strippedLines[i+1])
			}
		}
	}
}

// TestPrintTable_WideCellsKeepColumnsAligned: terminals lay text out in
// display cells, and a CJK character occupies two. "主机1" is 3 runes but 5
// cells wide, so padding it by rune count starts the next column two cells
// to the right of where the header and the ASCII row start it. Free-text
// cells reach PrintTable from operator input (an SSH key comment) and from
// NetBox object names.
func TestPrintTable_WideCellsKeepColumnsAligned(t *testing.T) {
	var buf strings.Builder
	format.PrintTable(&buf, []string{"HOST", "STATE"}, [][]string{
		{"主机1", "running"},
		{"cpu1", "new"},
	})
	assertTableText(t, buf.String(), []string{
		"HOST   STATE",
		"主机1  running",
		"cpu1   new",
	})
}

// TestPrintTable_OSCHyperlinkCellKeepsColumnsAligned: an OSC 8 hyperlink is
// invisible apart from its link text, so the cell is as wide as "link". An
// escape parser that ends every sequence at the first ASCII letter instead
// measures most of the URL as visible text and pads every other row to it.
func TestPrintTable_OSCHyperlinkCellKeepsColumnsAligned(t *testing.T) {
	hyperlink := "\x1b]8;;https://example.com/x\x1b\\link\x1b]8;;\x1b\\"
	var buf strings.Builder
	format.PrintTable(&buf, []string{"NAME", "STATE"}, [][]string{
		{hyperlink, "running"},
		{"cpu1", "new"},
	})
	assertTableText(t, buf.String(), []string{
		"NAME  STATE",
		"link  running",
		"cpu1  new",
	})
}

// assertTableText compares PrintTable output, with escape sequences removed,
// against the wanted lines. The header style emits SGR codes only when
// stdout is a terminal, so stripping keeps the comparison independent of how
// the test is run.
func assertTableText(t *testing.T, output string, wantLines []string) {
	t.Helper()
	got := strings.TrimSuffix(ansi.Strip(output), "\n")
	if want := strings.Join(wantLines, "\n"); got != want {
		t.Fatalf("table text mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// columnStarts returns the rune offsets where columns begin. A column start
// is position 0 (if non-space) or a non-space character preceded by 2+
// consecutive spaces (matching tabwriter's minimum padding).
func columnStarts(s string) []int {
	var starts []int
	runes := []rune(s)
	spaceRun := 0
	for i, r := range runes {
		if r == ' ' {
			spaceRun++
			continue
		}
		if i == 0 || spaceRun >= 2 {
			starts = append(starts, i)
		}
		spaceRun = 0
	}
	return starts
}

func TestOperationStatusLabel(t *testing.T) {
	tests := []struct {
		status   string
		wantIcon string
	}{
		{"completed", format.IconReady},
		{"running", format.IconActive},
		{"claimed", format.IconActive},
		{"pending", format.IconNew},
		{"failed", format.IconFailed},
	}
	for _, tt := range tests {
		got := format.OperationStatusLabel(tt.status)
		if !strings.Contains(got, tt.wantIcon) {
			t.Errorf("OperationStatusLabel(%q): expected icon %q in output %q", tt.status, tt.wantIcon, got)
		}
	}
}
