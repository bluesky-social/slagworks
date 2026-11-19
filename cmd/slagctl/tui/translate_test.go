package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

var (
	watchedOp    = uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	otherOp      = uuid.MustParse("bbbbbbbb-0000-4000-8000-000000000002")
	watchMachine = uuid.MustParse("cccccccc-0000-4000-8000-000000000003")
)

// globalEnv carries no op_id, like a machine-scoped line; translateEnvelope
// attributes such an envelope to the watched op.
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
	msg := translateEnvelope(1, globalEnv(map[string]any{"status": "completed"}), watchedOp)
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
	}), watchedOp)
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
	msg := translateEnvelope(1, globalEnv(map[string]any{"claimed_by": "pop1-sw-2"}), watchedOp)
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
		OpID:   watchedOp,
		Phase:  "partitioning",
		Msg:    "exec",
	}
	msg := translateEnvelope(42, env, watchedOp)
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
	if le.otherOp {
		t.Error("the watched op's own envelope was marked as another op's")
	}
}

// The logs stream ORs machine_id and op_id (logstore.Query), so a watch also
// receives every envelope of any other op on the same machine: its phase
// lines, its claim and its source=global terminal envelope. Those describe a
// different op. They belong in the log pane, but they must not end the watch,
// set the claiming worker, or move the watched op's lifecycle spine.

// TestTranslate_OtherOpDoesNotDriveWatch: another op's claim, phase and
// terminal envelopes arrive while the watched deploy is partitioning. The
// deploy is still running, so the watch must keep going with its worker,
// spine and outcome untouched, and the watched op's own terminal must still
// end it afterwards.
func TestTranslate_OtherOpDoesNotDriveWatch(t *testing.T) {
	m := newModelAt(model.OpDeploy, "host", "aaaaaaaa", at(0), time.UTC, fixedClock(at(60*time.Second)))
	m.holdOnTerminal = true
	m = send(m, translateEnvelope(1, slogkit.Envelope{TS: at(5 * time.Second), Source: slogkit.SourceGlobal,
		MachineID: watchMachine, OpID: watchedOp, Msg: "operation claimed", Extra: map[string]any{"claimed_by": "sw-1"}}, watchedOp))
	m = send(m, translateEnvelope(2, slogkit.Envelope{TS: at(10 * time.Second), Source: slogkit.SourceAgent,
		MachineID: watchMachine, OpID: watchedOp, Phase: "partitioning", Msg: "partitioning"}, watchedOp))

	m = send(m, translateEnvelope(3, slogkit.Envelope{TS: at(12 * time.Second), Source: slogkit.SourceGlobal,
		MachineID: watchMachine, OpID: otherOp, Msg: "operation claimed", Extra: map[string]any{"claimed_by": "sw-2"}}, watchedOp))
	if m.worker != "sw-1" {
		t.Errorf("worker = %q, want sw-1 (another op's claim replaced it)", m.worker)
	}
	m = send(m, translateEnvelope(4, slogkit.Envelope{TS: at(15 * time.Second), Source: slogkit.SourceSite,
		MachineID: watchMachine, OpID: otherOp, Phase: "bmc_reconcile", Msg: "phase bmc_reconcile"}, watchedOp))
	if got := m.lifecycle.CurrentPhase(); got != "partitioning" {
		t.Errorf("current phase = %q, want partitioning (another op's phase moved the spine)", got)
	}
	m = send(m, translateEnvelope(5, slogkit.Envelope{TS: at(20 * time.Second), Source: slogkit.SourceGlobal,
		MachineID: watchMachine, OpID: otherOp, Msg: "operation completed", Extra: map[string]any{"status": "completed"}}, watchedOp))
	if m.done {
		t.Fatalf("watch ended (failed=%v) on another op's terminal envelope; outcome=%v", m.failed, m.runResult().Outcome)
	}
	if got := strings.Join(m.logs.Lines(), "\n"); !strings.Contains(got, "phase=bmc_reconcile") ||
		!strings.Contains(got, "operation completed") {
		t.Errorf("another op's envelopes should still show in the log pane:\n%s", got)
	}

	m = send(m, translateEnvelope(6, slogkit.Envelope{TS: at(50 * time.Second), Source: slogkit.SourceGlobal,
		MachineID: watchMachine, OpID: watchedOp, Msg: "operation failed",
		Extra: map[string]any{"status": "failed", "error": "sgdisk exit 2"}}, watchedOp))
	if r := m.runResult(); r.Outcome != OutcomeFailed || m.failReason != "sgdisk exit 2" {
		t.Errorf("watched op's own terminal: outcome=%v reason=%q, want OutcomeFailed / sgdisk exit 2", r.Outcome, m.failReason)
	}
}

// TestTranslate_LaterOpDoesNotRewriteHeldOutcome: an interactive watch holds
// the screen after the deploy completes, and the log stream keeps running.
// The next op on the same machine (a power cycle, say) then fails. The held
// screen and the exit code must still report the deploy's own outcome.
func TestTranslate_LaterOpDoesNotRewriteHeldOutcome(t *testing.T) {
	m := newModelAt(model.OpDeploy, "host", "aaaaaaaa", at(0), time.UTC, fixedClock(at(10*time.Minute)))
	m.holdOnTerminal = true
	m = send(m, translateEnvelope(1, slogkit.Envelope{TS: at(5 * time.Minute), Source: slogkit.SourceGlobal,
		MachineID: watchMachine, OpID: watchedOp, Msg: "operation completed", Extra: map[string]any{"status": "completed"}}, watchedOp))
	if r := m.runResult(); r.Outcome != OutcomeCompleted {
		t.Fatalf("setup: outcome = %v, want OutcomeCompleted", r.Outcome)
	}

	m = send(m, translateEnvelope(2, slogkit.Envelope{TS: at(6 * time.Minute), Source: slogkit.SourceGlobal,
		MachineID: watchMachine, OpID: otherOp, Msg: "operation failed",
		Extra: map[string]any{"status": "failed", "error": "bmc unreachable"}}, watchedOp))
	if r := m.runResult(); r.Outcome != OutcomeCompleted || m.failed || m.failReason != "" {
		t.Errorf("a later op's failure rewrote the held deploy outcome: outcome=%v failed=%v reason=%q",
			r.Outcome, m.failed, m.failReason)
	}
	if !m.endTime.Equal(at(5 * time.Minute)) {
		t.Errorf("endTime = %v, want the deploy's own terminal time", m.endTime)
	}
}

// TestTranslate_GlobalNonTerminalIsLog: a source=global envelope WITHOUT a
// status/claimed_by (e.g. an incidental global info line) is just a log line,
// not a terminal signal.
func TestTranslate_GlobalNonTerminalIsLog(t *testing.T) {
	env := slogkit.Envelope{TS: at(0), Source: slogkit.SourceGlobal, Msg: "snapshot rebuilt"}
	if _, ok := translateEnvelope(1, env, watchedOp).(logEnvelopeMsg); !ok {
		t.Errorf("non-terminal global envelope should be a log line, got %T", translateEnvelope(1, env, watchedOp))
	}
}
