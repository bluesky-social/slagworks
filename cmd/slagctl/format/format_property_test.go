package format_test

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
)

// Property-based coverage for slagctl's table, duration, size and
// untrusted-text formatters. The oracles do not share the package's width
// measurement: every table cell is assembled from fragments whose visible text
// and display width are written down here.

// cellFragment is a piece of table cell content: its bytes, the text a
// terminal shows for it, and how many display cells that text occupies.
type cellFragment struct {
	raw, plain string
	width      int
}

var cellFragments = []cellFragment{
	{"a", "a", 1},
	{"cpu1.pop1", "cpu1.pop1", 9},
	{"-", "-", 1},
	{" ", " ", 1},
	{"deploy", "deploy", 6},
	{"中", "中", 2},
	{"主机", "主机", 4},
	{"\u00e9", "\u00e9", 1},         // precomposed e-acute
	{"e\u0301", "e\u0301", 1},       // e + combining acute
	{"\U0001F600", "\U0001F600", 2}, // an emoji, two cells wide
	{"\x1b[1;38;5;10m● ready\x1b[0m", "● ready", 7},
	{"\x1b[1mbold\x1b[0m", "bold", 4},
	{"\x1b]8;;https://example.com/x\x1b\\link\x1b]8;;\x1b\\", "link", 4},
}

type tableCell struct {
	raw, plain string
	width      int
}

func genCell(t *rapid.T, label string) tableCell {
	frags := rapid.SliceOfN(rapid.SampledFrom(cellFragments), 0, 6).Draw(t, label)
	var c tableCell
	for _, f := range frags {
		c.raw += f.raw
		c.plain += f.plain
		c.width += f.width
	}
	return c
}

// TestPrintTable_Property_ColumnsAlign: each printed line is its cells' visible
// text, every column but the last padded to the widest cell in that column
// plus two spaces, so every column starts at the same terminal column on every
// line, header included. Cells mix wide characters, combining sequences, SGR
// styling and OSC 8 hyperlinks.
func TestPrintTable_Property_ColumnsAlign(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ncol := rapid.IntRange(1, 4).Draw(t, "ncol")
		header := make([]tableCell, ncol)
		headers := make([]string, ncol)
		for i := range header {
			h := rapid.StringMatching(`[A-Z]{1,8}`).Draw(t, fmt.Sprintf("h%d", i))
			header[i] = tableCell{h, h, len(h)}
			headers[i] = h
		}
		nrow := rapid.IntRange(0, 5).Draw(t, "nrow")
		cells := [][]tableCell{header}
		rows := make([][]string, nrow)
		for r := range rows {
			row := make([]tableCell, ncol)
			rows[r] = make([]string, ncol)
			for c := range row {
				row[c] = genCell(t, fmt.Sprintf("c%d_%d", r, c))
				rows[r][c] = row[c].raw
			}
			cells = append(cells, row)
		}

		var buf bytes.Buffer
		format.PrintTable(&buf, headers, rows)

		widths := make([]int, ncol)
		for _, row := range cells {
			for c, cell := range row {
				widths[c] = max(widths[c], cell.width)
			}
		}
		var want strings.Builder
		for _, row := range cells {
			for c, cell := range row {
				want.WriteString(cell.plain)
				if c < ncol-1 {
					want.WriteString(strings.Repeat(" ", widths[c]-cell.width+2))
				}
			}
			want.WriteByte('\n')
		}
		// The header style emits SGR only when stdout is a terminal.
		if got := ansi.Strip(buf.String()); got != want.String() {
			t.Fatalf("table text mismatch\ngot:\n%s\nwant:\n%s", got, want.String())
		}
		// Every cell's own bytes survive, styling and hyperlinks included.
		for _, row := range rows {
			for _, cell := range row {
				if !strings.Contains(buf.String(), cell) {
					t.Fatalf("cell %q missing from output %q", cell, buf.String())
				}
			}
		}
	})
}

// TestUntrusted_Property_NeutralizesTerminalControls: whatever bytes a host
// sends, the result is valid UTF-8 with no rune a terminal interprets (C0,
// DEL, C1), and text with no control or invalid byte passes unchanged.
func TestUntrusted_Property_NeutralizesTerminalControls(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.OneOf(
			rapid.String(),
			rapid.Map(rapid.SliceOf(rapid.Byte()), func(b []byte) string { return string(b) }),
			rapid.Map(rapid.SliceOf(rapid.SampledFrom([]string{
				"ok", " ", "中", "é", "\\x1b", "\t", "\n", "\r\n", "\r", "\x00", "\x07", "\x1b",
				"\x1b[2J", "\x1b]52;c;cHduZWQ=\x07", "\x1b]8;;http://x/\x1b\\", "\x7f", "\u0085", "\u009b",
				"\xff", "\xc3", "\xe4\xb8",
			})), func(p []string) string { return strings.Join(p, "") }),
		).Draw(t, "s")
		got := format.Untrusted(s)
		if !utf8.ValidString(got) {
			t.Fatalf("Untrusted(%q) = %q is not valid UTF-8", s, got)
		}
		for _, r := range got {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Fatalf("Untrusted(%q) = %q still carries terminal control %U", s, got, r)
			}
		}
		clean := utf8.ValidString(s)
		for _, r := range s {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				clean = false
			}
		}
		if clean && got != s {
			t.Fatalf("Untrusted(%q) = %q, want the printable input unchanged", s, got)
		}
	})
}

// TestShortDuration_Property_RoundTrip: ShortDuration's output parses back
// (via an independent parser) to the input truncated to the documented two
// units.
func TestShortDuration_Property_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		d := time.Duration(rapid.Int64Range(-int64(time.Hour), int64(400*24*time.Hour)).Draw(t, "d"))
		got := format.ShortDuration(d)
		var want time.Duration
		switch {
		case d < 0:
			want = 0
		case d < time.Hour:
			want = d.Truncate(time.Second)
		case d < 24*time.Hour:
			want = d.Truncate(time.Minute)
		default:
			want = d.Truncate(time.Hour)
		}
		back := parseShort(t, got)
		if back != want {
			t.Fatalf("ShortDuration(%v) = %q parses to %v, want %v", d, got, back, want)
		}
	})
}

func parseShort(t *rapid.T, s string) time.Duration {
	var total time.Duration
	n := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			n = n*10 + int(r-'0')
		case r == 'd':
			total += time.Duration(n) * 24 * time.Hour
			n = 0
		case r == 'h':
			total += time.Duration(n) * time.Hour
			n = 0
		case r == 'm':
			total += time.Duration(n) * time.Minute
			n = 0
		case r == 's':
			total += time.Duration(n) * time.Second
			n = 0
		default:
			t.Fatalf("unexpected rune %q in %q", r, s)
		}
	}
	return total
}

// TestFormatBytes_Property_UnitAndValue: the rendered value times its unit is
// within rounding of the input, and the unit is the largest power of 1024 not
// above it.
func TestFormatBytes_Property_UnitAndValue(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		b := rapid.OneOf(
			rapid.Int64Range(0, 1<<20),
			rapid.Int64Range(0, math.MaxInt64),
			rapid.SampledFrom([]int64{
				1023, 1024, 1025, 1<<20 - 1, 1 << 20, 1<<30 - 1, 1 << 30, 1<<40 - 1, 1 << 40,
				1<<50 - 1, 1 << 50, 1<<60 - 1, 1 << 60, math.MaxInt64,
			}),
		).Draw(t, "b")
		s := format.FormatBytes(b)
		if b < 1024 {
			if s != fmt.Sprintf("%d B", b) {
				t.Fatalf("FormatBytes(%d) = %q", b, s)
			}
			return
		}
		var v float64
		var u string
		if _, err := fmt.Sscanf(s, "%f %s", &v, &u); err != nil {
			t.Fatalf("FormatBytes(%d) = %q unparseable: %v", b, s, err)
		}
		exp := strings.Index("KMGTPE", u[:1]) + 1
		if exp == 0 || u[1:] != "B" {
			t.Fatalf("FormatBytes(%d) = %q: bad unit", b, s)
		}
		// Integer bounds: float64 cannot hold every int64, and 1<<60-1 would
		// round up to the next unit's boundary.
		intUnit := int64(1) << (10 * exp)
		if b < intUnit || (exp < 6 && b/intUnit >= 1024) {
			t.Fatalf("FormatBytes(%d) = %q: unit %s not the largest power below the value", b, s, u)
		}
		unit := float64(intUnit)
		if math.Abs(v*unit-float64(b)) > 0.05*unit+1 {
			t.Fatalf("FormatBytes(%d) = %q: %v*%v far from input", b, s, v, unit)
		}
	})
}
