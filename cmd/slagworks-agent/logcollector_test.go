package main

import (
	"fmt"
	"testing"
)

func TestLogCollector_BufferAndDrain(t *testing.T) {
	lc := newLogCollector()
	lc.Append("agent", "line 1")
	lc.Append("dmesg", "line 2")
	lines := lc.Drain()
	if len(lines) != 2 {
		t.Fatalf("expected 2, got %d", len(lines))
	}
	if lines[0].Source != "agent" {
		t.Fatal("expected agent source")
	}
	if lines[1].Source != "dmesg" {
		t.Fatal("expected dmesg source")
	}
	if lines[0].Line != "line 1" {
		t.Fatalf("expected 'line 1', got %q", lines[0].Line)
	}
	// Drain again empty
	if len(lc.Drain()) != 0 {
		t.Fatal("expected empty after drain")
	}
}

func TestLogCollector_FlushSignalOnThreshold(t *testing.T) {
	lc := newLogCollector()
	for i := range 50 {
		lc.Append("dmesg", fmt.Sprintf("line %d", i))
	}
	select {
	case <-lc.FlushCh():
	default:
		t.Fatal("expected flush signal after 50 lines")
	}
}

func TestLogCollector_Len(t *testing.T) {
	lc := newLogCollector()
	if lc.Len() != 0 {
		t.Fatalf("expected 0, got %d", lc.Len())
	}
	lc.Append("agent", "hello")
	if lc.Len() != 1 {
		t.Fatalf("expected 1, got %d", lc.Len())
	}
	lc.Drain()
	if lc.Len() != 0 {
		t.Fatalf("expected 0 after drain, got %d", lc.Len())
	}
}

func TestLogCollector_TimestampFormat(t *testing.T) {
	lc := newLogCollector()
	lc.Append("agent", "hello")
	lines := lc.Drain()
	if lines[0].Timestamp == "" {
		t.Fatal("expected non-empty timestamp")
	}
	// RFC3339 should contain a T separator
	if len(lines[0].Timestamp) < 20 {
		t.Fatalf("timestamp looks too short: %q", lines[0].Timestamp)
	}
}
