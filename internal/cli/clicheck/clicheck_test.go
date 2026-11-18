package clicheck

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunChecksTable(t *testing.T) {
	checks := []Check{
		{Name: "always-ok", Run: func(_ context.Context) Result { return OK("things look good") }},
		{Name: "always-warn", Run: func(_ context.Context) Result { return Warn("consider fixing") }},
	}
	var buf bytes.Buffer
	ok := RunChecks(context.Background(), &buf, checks, false)
	if !ok {
		t.Error("RunChecks returned false with no Fail checks")
	}
	out := buf.String()
	if !strings.Contains(out, statusGlyph(StatusOK)) {
		t.Errorf("expected the ok mark in output: %q", out)
	}
	if !strings.Contains(out, statusGlyph(StatusWarn)) {
		t.Errorf("expected the warn mark in output: %q", out)
	}
	if !strings.Contains(out, "always-ok") {
		t.Errorf("expected check name in output: %q", out)
	}
	if !strings.Contains(out, "things look good") {
		t.Errorf("expected detail in output: %q", out)
	}
}

func TestRunChecksFailExitCode(t *testing.T) {
	checks := []Check{
		{Name: "ok", Run: func(_ context.Context) Result { return OK("") }},
		{Name: "broken", Run: func(_ context.Context) Result { return Fail("it is broken") }},
	}
	var buf bytes.Buffer
	ok := RunChecks(context.Background(), &buf, checks, false)
	if ok {
		t.Error("RunChecks returned true with a Fail check")
	}
	out := buf.String()
	if !strings.Contains(out, statusGlyph(StatusFail)) {
		t.Errorf("expected the fail mark in output: %q", out)
	}
	if !strings.Contains(out, "it is broken") {
		t.Errorf("expected fail detail in output: %q", out)
	}
}

func TestRunChecksWarnDoesNotFail(t *testing.T) {
	checks := []Check{
		{Name: "w", Run: func(_ context.Context) Result { return Warn("just a warning") }},
	}
	var buf bytes.Buffer
	ok := RunChecks(context.Background(), &buf, checks, false)
	if !ok {
		t.Error("Warn-only run should return true (no failure)")
	}
}

func TestRunChecksJSON(t *testing.T) {
	checks := []Check{
		{Name: "alpha", Run: func(_ context.Context) Result { return OK("fine") }},
		{Name: "beta", Run: func(_ context.Context) Result { return Fail("oops") }},
	}
	var buf bytes.Buffer
	ok := RunChecks(context.Background(), &buf, checks, true)
	if ok {
		t.Error("RunChecks should return false with a Fail check")
	}

	var findings []finding
	if err := json.Unmarshal(buf.Bytes(), &findings); err != nil {
		t.Fatalf("JSON output did not parse: %v\noutput: %s", err, buf.String())
	}
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[0].Status != "ok" {
		t.Errorf("findings[0].status = %q, want ok", findings[0].Status)
	}
	if findings[1].Status != "fail" {
		t.Errorf("findings[1].status = %q, want fail", findings[1].Status)
	}
	if findings[1].Detail != "oops" {
		t.Errorf("findings[1].detail = %q, want oops", findings[1].Detail)
	}
}

// TestRunChecksTimeout: a check that outlives its context must come back as
// Fail quickly, not wedge the whole run. runOne derives its per-check
// deadline from the parent, so a tight parent keeps the test fast instead of
// waiting out DefaultCheckTimeout.
func TestRunChecksTimeout(t *testing.T) {
	slow := Check{
		Name: "slow",
		Run: func(ctx context.Context) Result {
			select {
			case <-time.After(10 * time.Second):
				return OK("never")
			case <-ctx.Done():
				return Fail("ctx cancelled (expected in test)")
			}
		},
	}

	parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := runOne(parent, slow)
	elapsed := time.Since(start)

	if r.Status != StatusFail {
		t.Errorf("expected StatusFail from timed-out check, got %v detail=%q", r.Status, r.Detail)
	}
	if elapsed > 2*time.Second {
		t.Errorf("timeout check took too long: %v", elapsed)
	}
}

func TestTableAlignment(t *testing.T) {
	// Verify that the longer name causes both lines to have matching indentation.
	checks := []Check{
		{Name: "ab", Run: func(_ context.Context) Result { return OK("short") }},
		{Name: "abcde", Run: func(_ context.Context) Result { return OK("long name") }},
	}
	var buf bytes.Buffer
	RunChecks(context.Background(), &buf, checks, false)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
	}
	// Both detail strings should start at the same column: mark + space +
	// name + pad + 2 spaces + detail.
	idxA := strings.Index(lines[0], "short")
	idxB := strings.Index(lines[1], "long name")
	if idxA != idxB {
		t.Errorf("detail columns do not align: %d vs %d\n  %q\n  %q", idxA, idxB, lines[0], lines[1])
	}
}
