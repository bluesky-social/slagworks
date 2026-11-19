package execkit

import (
	"bytes"
	"strings"
	"testing"
)

// TestBoundedBuffer_UnderCap retains all bytes when total stays under
// the cap; String returns plain output, no truncation marker.
func TestBoundedBuffer_UnderCap(t *testing.T) {
	b := &boundedBuffer{cap: 100}
	if _, err := b.Write([]byte("hello world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := b.String(); got != "hello world" {
		t.Errorf("String() = %q, want %q", got, "hello world")
	}
}

// TestBoundedBuffer_OverCap truncates to cap bytes and appends a
// trailing marker reporting the true total. Without the marker the
// operator reading a truncated buffer would silently misjudge what
// the command actually said.
func TestBoundedBuffer_OverCap(t *testing.T) {
	b := &boundedBuffer{cap: 10}
	big := bytes.Repeat([]byte("A"), 50)
	if _, err := b.Write(big); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := b.String()
	if !strings.HasPrefix(got, "AAAAAAAAAA") {
		t.Errorf("String() prefix = %q, want %q", got[:10], "AAAAAAAAAA")
	}
	if !strings.Contains(got, "truncated, total 50 bytes") {
		t.Errorf("String() should contain truncation marker, got: %q", got)
	}
}

// TestBoundedBuffer_MultipleWrites accumulates across writes until cap
// is reached, then drops the rest while continuing to count toward
// total.
func TestBoundedBuffer_MultipleWrites(t *testing.T) {
	b := &boundedBuffer{cap: 5}
	for i := range 4 {
		if _, err := b.Write([]byte("xy")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	got := b.String()
	if !strings.HasPrefix(got, "xyxyx") {
		t.Errorf("prefix %q, want %q", got, "xyxyx")
	}
	if !strings.Contains(got, "total 8 bytes") {
		t.Errorf("missing truncation marker: %q", got)
	}
}

// TestBoundedBuffer_ZeroCap is the degenerate "capture nothing"
// configuration; the marker still reports the true total written.
func TestBoundedBuffer_ZeroCap(t *testing.T) {
	b := &boundedBuffer{cap: 0}
	if _, err := b.Write([]byte("anything")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "truncated, total 8 bytes") {
		t.Errorf("got %q", got)
	}
}

// TestTailBuffer_UnderCap retains everything when total stays under the cap.
func TestTailBuffer_UnderCap(t *testing.T) {
	b := &tailBuffer{cap: 100}
	for _, s := range []string{"line one\n", "line two\n"} {
		if _, err := b.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := b.String(); got != "line one\nline two\n" {
		t.Errorf("String() = %q, want both lines verbatim", got)
	}
}

// TestTailBuffer_OverCapKeepsTail drops the head, never the end: the last
// line survives, a partial first line is discarded, a whole one is kept, and
// a leading marker reports the true total.
func TestTailBuffer_OverCapKeepsTail(t *testing.T) {
	writes := []string{"aaaaaaaaaa\n", "bbbbbbbbbb\n", "LAST\n"} // 27 bytes
	cases := []struct {
		name string
		cap  int
		want string
	}{
		// The window's first byte is mid-way through the "b" line: drop it.
		{"cut mid-line", 12, "...[truncated, total 27 bytes]\nLAST\n"},
		// The window starts exactly at the "b" line: keep it whole.
		{"cut on line boundary", 16, "...[truncated, total 27 bytes]\nbbbbbbbbbb\nLAST\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &tailBuffer{cap: tc.cap}
			for _, s := range writes {
				if _, err := b.Write([]byte(s)); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if got := b.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTailBuffer_SingleWriteLargerThanCap covers one write that alone
// exceeds the cap (a pipe read can deliver a large chunk at once).
func TestTailBuffer_SingleWriteLargerThanCap(t *testing.T) {
	b := &tailBuffer{cap: 8}
	if _, err := b.Write([]byte(strings.Repeat("x", 40) + "\nEND\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := b.String(), "...[truncated, total 45 bytes]\nEND\n"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestTailBuffer_OneLineLongerThanCap keeps the end of a single line that
// alone exceeds the cap (one long ERROR line) instead of discarding it as a
// partial line and leaving only the marker.
func TestTailBuffer_OneLineLongerThanCap(t *testing.T) {
	b := &tailBuffer{cap: 8}
	if _, err := b.Write([]byte("ERROR: " + strings.Repeat("x", 20) + " fix it\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got, want := b.String(), "...[truncated, total 35 bytes]\n fix it\n"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
