package tui

import (
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

func globalEnv(extra map[string]any) slogkit.Envelope {
	return slogkit.Envelope{
		TS:     at(time.Second),
		Source: slogkit.SourceGlobal,
		Msg:    "operation event",
		Extra:  extra,
	}
}

// TestTranslate_Completion: a source=global status=completed envelope becomes
// an opTerminalMsg{failed:false}. This is the D6 completion signal.
func TestTranslate_Completion(t *testing.T) {
	msg := translateEnvelope(1, globalEnv(map[string]any{"status": "completed"}))
	term, ok := msg.(opTerminalMsg)
	if !ok {
		t.Fatalf("want opTerminalMsg, got %T", msg)
	}
	if term.failed {
		t.Error("completed should not be failed")
	}
}

// TestTranslate_Failure: status=failed carries the error string into the
// terminal message's reason (pinned in the error pane).
func TestTranslate_Failure(t *testing.T) {
	msg := translateEnvelope(1, globalEnv(map[string]any{
		"status": "failed", "error": "sgdisk exit 2",
	}))
	term, ok := msg.(opTerminalMsg)
	if !ok {
		t.Fatalf("want opTerminalMsg, got %T", msg)
	}
	if !term.failed {
		t.Error("status=failed should be failed")
	}
	if term.reason != "sgdisk exit 2" {
		t.Errorf("reason = %q, want 'sgdisk exit 2'", term.reason)
	}
}

// TestTranslate_Claim: a claim envelope becomes a claimedMsg with the worker.
func TestTranslate_Claim(t *testing.T) {
	msg := translateEnvelope(1, globalEnv(map[string]any{"claimed_by": "pop1-sw-2"}))
	c, ok := msg.(claimedMsg)
	if !ok {
		t.Fatalf("want claimedMsg, got %T", msg)
	}
	if c.worker != "pop1-sw-2" {
		t.Errorf("worker = %q, want pop1-sw-2", c.worker)
	}
}

// TestTranslate_LogLine: an ordinary agent envelope (with a phase) becomes a
// logEnvelopeMsg carrying the whole envelope, so the reducer can both log it
// and advance the lifecycle off the phase field.
func TestTranslate_LogLine(t *testing.T) {
	env := slogkit.Envelope{
		TS:     at(2 * time.Second),
		Source: slogkit.SourceAgent,
		Phase:  "partitioning",
		Msg:    "exec",
	}
	msg := translateEnvelope(42, env)
	le, ok := msg.(logEnvelopeMsg)
	if !ok {
		t.Fatalf("want logEnvelopeMsg, got %T", msg)
	}
	if le.id != 42 {
		t.Errorf("id = %d, want 42", le.id)
	}
	if le.env.Phase != "partitioning" {
		t.Errorf("phase not carried: %q", le.env.Phase)
	}
}

// TestTranslate_GlobalNonTerminalIsLog: a source=global envelope WITHOUT a
// status/claimed_by (e.g. an incidental global info line) is just a log line,
// not a terminal signal.
func TestTranslate_GlobalNonTerminalIsLog(t *testing.T) {
	env := slogkit.Envelope{TS: at(0), Source: slogkit.SourceGlobal, Msg: "snapshot rebuilt"}
	if _, ok := translateEnvelope(1, env).(logEnvelopeMsg); !ok {
		t.Errorf("non-terminal global envelope should be a log line, got %T", translateEnvelope(1, env))
	}
}
