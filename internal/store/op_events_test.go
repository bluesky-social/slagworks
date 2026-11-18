package store

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// opEnvelopes returns the source=global log envelopes emitted for an op, in id
// order, by querying the log_envelopes table the transition tx wrote into.
// This is exactly what /logs/stream serves, so it's the right assertion
// surface for D6 ("the stream is the primary signal").
func opEnvelopes(t *testing.T, st *Store, opID uuid.UUID) []slogkit.Envelope {
	t.Helper()
	ls := logstore.New(st.DB())
	res, err := ls.Query(context.Background(), logstore.QueryFilter{
		OpID:    opID,
		Sources: []string{string(slogkit.SourceGlobal)},
	})
	if err != nil {
		t.Fatalf("query envelopes: %v", err)
	}
	out := make([]slogkit.Envelope, len(res))
	for i, r := range res {
		out[i] = r.Envelope
	}
	return out
}

func deployingMachineWithPendingOp(t *testing.T, st *Store) (*model.Machine, *model.Operation) {
	t.Helper()
	m := &model.Machine{
		ID:           uuid.New(),
		NetBoxSerial: "SN-d6-" + uuid.NewString()[:8],
		State:        model.MachineStateCommissioned,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	op := &model.Operation{
		MachineID: m.ID,
		Type:      model.OpDeploy,
		Status:    model.OpPending,
	}
	if err := st.CreateOperation(op); err != nil {
		t.Fatal(err)
	}
	return m, op
}

// TestD6_ClaimEmitsEnvelope: claiming an op writes a source=global "operation
// claimed" envelope carrying claimed_by, visible via the logs query.
func TestD6_ClaimEmitsEnvelope(t *testing.T) {
	st := testStore(t)
	_, op := deployingMachineWithPendingOp(t, st)

	if _, err := st.ClaimOperation(op.ID, "pop1-sw-2"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	envs := opEnvelopes(t, st, op.ID)
	if len(envs) != 1 {
		t.Fatalf("want 1 claim envelope, got %d: %+v", len(envs), envs)
	}
	e := envs[0]
	if e.Source != slogkit.SourceGlobal {
		t.Errorf("source = %q, want global", e.Source)
	}
	if e.OpID != op.ID {
		t.Errorf("op_id = %v, want %v", e.OpID, op.ID)
	}
	if e.TraceID != op.TraceID {
		t.Errorf("trace_id not propagated: %v vs %v", e.TraceID, op.TraceID)
	}
	if got := e.StringExtra("claimed_by"); got != "pop1-sw-2" {
		t.Errorf("claimed_by = %q, want pop1-sw-2", got)
	}
}

// TestD6_CompleteEmitsTerminalEnvelope: completing an op to a target state
// writes a source=global terminal envelope with status=completed.
func TestD6_CompleteEmitsTerminalEnvelope(t *testing.T) {
	st := testStore(t)
	m, op := deployingMachineWithPendingOp(t, st)
	if _, err := st.ClaimOperation(op.ID, "pop1-sw-2"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.CompleteOperationWithState(op.ID, model.MachineStateRunning, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	envs := opEnvelopes(t, st, op.ID)
	// claim + terminal.
	if len(envs) != 2 {
		t.Fatalf("want 2 envelopes (claim+terminal), got %d: %+v", len(envs), envs)
	}
	term := envs[1]
	if got := term.StringExtra("status"); got != "completed" {
		t.Errorf("terminal status = %q, want completed", got)
	}
	if term.Level != slogkit.LevelInfo {
		t.Errorf("completed level = %q, want info", term.Level)
	}
	// Sanity: machine actually advanced (we didn't just emit and skip state).
	gm, _ := st.GetMachine(m.ID)
	if gm.State != model.MachineStateRunning {
		t.Errorf("machine state = %q, want running", gm.State)
	}
}

// TestD6_FailEmitsTerminalEnvelopeWithError: failing a claimed op emits a
// source=global terminal envelope with status=failed and the error string.
func TestD6_FailEmitsTerminalEnvelopeWithError(t *testing.T) {
	st := testStore(t)
	_, op := deployingMachineWithPendingOp(t, st)
	if _, err := st.ClaimOperation(op.ID, "pop1-sw-2"); err != nil {
		t.Fatal(err)
	}

	if err := st.FailOperation(op.ID, []byte(`{"error":"sgdisk exit 2"}`)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	envs := opEnvelopes(t, st, op.ID)
	if len(envs) != 2 {
		t.Fatalf("want 2 envelopes (claim+terminal), got %d: %+v", len(envs), envs)
	}
	term := envs[1]
	if got := term.StringExtra("status"); got != "failed" {
		t.Errorf("terminal status = %q, want failed", got)
	}
	if got := term.StringExtra("error"); got != "sgdisk exit 2" {
		t.Errorf("error = %q, want 'sgdisk exit 2'", got)
	}
	if term.Level != slogkit.LevelError {
		t.Errorf("failed level = %q, want error", term.Level)
	}
}

// TestD6_FailBeforeClaimEmits is the case the phase stream alone misses: an op
// that fails while still pending (pending-expiry sweep) — no worker ran, so no
// phase envelope ever flew. D6 guarantees the terminal envelope still emits, so
// the TUI sees completion. This is the load-bearing reason emission lives at
// the transition primitive, not the sweep call sites.
func TestD6_FailBeforeClaimEmits(t *testing.T) {
	st := testStore(t)
	_, op := deployingMachineWithPendingOp(t, st)
	// No claim — fail straight from pending.
	if err := st.FailOperation(op.ID, []byte(`{"error":"pending expired"}`)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	envs := opEnvelopes(t, st, op.ID)
	if len(envs) != 1 {
		t.Fatalf("want 1 terminal envelope for pre-claim fail, got %d: %+v", len(envs), envs)
	}
	if got := envs[0].StringExtra("status"); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	if got := envs[0].StringExtra("error"); got != "pending expired" {
		t.Errorf("error = %q, want 'pending expired'", got)
	}
}

// TestD6_LostFailRaceEmitsNothing: a FailOperation that loses the CAS (op
// already terminal) must NOT emit a phantom envelope. Completing first, then
// failing, the fail is a no-op and adds no envelope beyond the completion's.
func TestD6_LostFailRaceEmitsNothing(t *testing.T) {
	st := testStore(t)
	_, op := deployingMachineWithPendingOp(t, st)
	if _, err := st.ClaimOperation(op.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CompleteOperationWithState(op.ID, model.MachineStateRunning, nil); err != nil {
		t.Fatal(err)
	}
	before := opEnvelopes(t, st, op.ID)
	// This fail loses the race — op is already completed.
	if err := st.FailOperation(op.ID, []byte(`{"error":"too late"}`)); err != nil {
		t.Fatalf("fail: %v", err)
	}
	after := opEnvelopes(t, st, op.ID)
	if len(after) != len(before) {
		t.Errorf("lost-race fail emitted a phantom envelope: before=%d after=%d", len(before), len(after))
	}
}
