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
