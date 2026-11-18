package hubhandlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// newJSONLoggerForTest builds a slog.Logger that emits JSON lines into
// the supplied buffer so tests can decode and assert envelope shape.
func newJSONLoggerForTest(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// decodeJSONLines splits the captured slog output by newline and
// JSON-decodes each non-empty line into a map.
func decodeJSONLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

type fakeStore struct {
	mu sync.Mutex

	phases    map[uuid.UUID]string
	completed map[uuid.UUID][]byte
	// completedState mirrors CompleteOperationWithState's state arg
	// for assertions.
	completedState map[uuid.UUID]model.MachineState
	failed         map[uuid.UUID][]byte

	opByID map[uuid.UUID]*model.Operation

	phaseErr    error
	completeErr error
	failErr     error
}

func newFake() *fakeStore {
	return &fakeStore{
		phases:         make(map[uuid.UUID]string),
		completed:      make(map[uuid.UUID][]byte),
		completedState: make(map[uuid.UUID]model.MachineState),
		failed:         make(map[uuid.UUID][]byte),
		opByID:         make(map[uuid.UUID]*model.Operation),
	}
}

// fakeLogStore implements LogStore for the LogBatch tests.
type fakeLogStore struct {
	mu        sync.Mutex
	envelopes []slogkit.Envelope
	err       error
}

func newFakeLogStore() *fakeLogStore { return &fakeLogStore{} }

func (s *fakeLogStore) InsertBatch(_ context.Context, envelopes []slogkit.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.envelopes = append(s.envelopes, envelopes...)
	return nil
}

func (s *fakeStore) GetOperation(id uuid.UUID) (*model.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.opByID[id]
	if op == nil {
		return nil, errors.New("not found")
	}
	cp := *op
	return &cp, nil
}

func (s *fakeStore) SetOperationPhase(opID uuid.UUID, phase string) (store.PhaseTransition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phaseErr != nil {
		return store.PhaseTransition{}, s.phaseErr
	}
	prev := s.phases[opID]
	s.phases[opID] = phase
	t := store.PhaseTransition{NewPhase: phase}
	if prev != phase {
		t.Changed = true
		t.PrevPhase = prev
	}
	if op := s.opByID[opID]; op != nil {
		t.MachineID = op.MachineID
		t.TraceID = op.TraceID
	}
	if t.Changed {
		// Post-mutation row for the phase op_delta publish. Reflect the
		// new phase so a publish-inspecting test sees it.
		post := model.Operation{ID: opID, Phase: phase}
		if op := s.opByID[opID]; op != nil {
			post = *op
			post.Phase = phase
		}
		post.Version++
		t.Op = &post
	}
	return t, nil
}

func (s *fakeStore) CompleteOperationWithState(
	opID uuid.UUID,
	targetState model.MachineState,
	result []byte,
) (*store.OpOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	s.completed[opID] = append([]byte(nil), result...)
	s.completedState[opID] = targetState
	op := model.Operation{ID: opID}
	if o := s.opByID[opID]; o != nil {
		op = *o
	}
	op.Status = model.OpCompleted
	return &store.OpOutcome{Op: op, MachineState: targetState}, nil
}

func (s *fakeStore) FailOperationOutcome(opID uuid.UUID, result []byte) (*store.OpOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return nil, s.failErr
	}
	s.failed[opID] = append([]byte(nil), result...)
	op := model.Operation{ID: opID}
	if o := s.opByID[opID]; o != nil {
		op = *o
	}
	op.Status = model.OpFailed
	// The fake reports a durable fail (failed) so terminal-fail tests can
	// assert the identity rebuild fires. A pending-op fail would carry ""
	// but no existing test exercises that through this fake.
	return &store.OpOutcome{Op: op, MachineState: model.MachineStateFailed}, nil
}

// --- OpUpdate tests ---

func TestOpUpdate_PhaseOnly(t *testing.T) {
	st := newFake()
	h := NewOpUpdate(st)
	opID := uuid.New()

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "partitioning",
	})
	if err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phases[opID] != "partitioning" {
		t.Errorf("phase: got %q, want partitioning", st.phases[opID])
	}
	if len(st.completed) != 0 || len(st.failed) != 0 {
		t.Errorf("no terminal transition should happen on phase-only update")
	}
}

// TestOpUpdate_PhaseTransitionEnvelope: a real phase change
// emits one structured log record at INFO with msg="phase transition"
// carrying op_id, machine_id, trace_id, phase, prev_phase, duration_ms.
// A duplicate phase emits no record.
func TestOpUpdate_PhaseTransitionEnvelope(t *testing.T) {
	st := newFake()
	opID := uuid.New()
	machineID := uuid.New()
	traceID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, MachineID: machineID, TraceID: traceID}

	logs := &bytes.Buffer{}
	logger := newJSONLoggerForTest(logs)

	h := NewOpUpdate(st)
	h.SetLogger(logger)

	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "pxe_booting",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "partitioning",
	}); err != nil {
		t.Fatal(err)
	}
	// Duplicate: must NOT produce a third envelope.
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "partitioning",
	}); err != nil {
		t.Fatal(err)
	}

	records := decodeJSONLines(t, logs)
	var transitions []map[string]any
	for _, r := range records {
		if r["msg"] == "phase transition" {
			transitions = append(transitions, r)
		}
	}
	if len(transitions) != 2 {
		t.Fatalf("expected 2 phase-transition envelopes, got %d: %v", len(transitions), transitions)
	}

	// First: pxe_booting with empty prev_phase.
	if transitions[0]["phase"] != "pxe_booting" {
		t.Errorf("first phase = %v, want pxe_booting", transitions[0]["phase"])
	}
	if transitions[0]["prev_phase"] != "" {
		t.Errorf("first prev_phase = %v, want empty", transitions[0]["prev_phase"])
	}
	if transitions[0]["machine_id"] != machineID.String() {
		t.Errorf("first machine_id = %v, want %v", transitions[0]["machine_id"], machineID)
	}
	if transitions[0]["trace_id"] != traceID.String() {
		t.Errorf("first trace_id = %v, want %v", transitions[0]["trace_id"], traceID)
	}

	// Second: partitioning with prev_phase=pxe_booting.
	if transitions[1]["phase"] != "partitioning" {
		t.Errorf("second phase = %v, want partitioning", transitions[1]["phase"])
	}
	if transitions[1]["prev_phase"] != "pxe_booting" {
		t.Errorf("second prev_phase = %v, want pxe_booting", transitions[1]["prev_phase"])
	}
	if _, ok := transitions[1]["duration_ms"]; !ok {
		t.Errorf("second envelope missing duration_ms: %v", transitions[1])
	}
}

func TestOpUpdate_CompletedStatus_DeployTransitionsToDeployed(t *testing.T) {
	st := newFake()
	h := NewOpUpdate(st)
	opID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, Type: model.OpDeploy}

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "completed",
		Status:      string(model.OpCompleted),
		Result:      json.RawMessage(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phases[opID] != "completed" {
		t.Errorf("phase: got %q, want completed", st.phases[opID])
	}
	if string(st.completed[opID]) != `{"ok":true}` {
		t.Errorf("result: got %q", st.completed[opID])
	}
	if st.completedState[opID] != model.MachineStateRunning {
		t.Errorf("target state: got %q, want running", st.completedState[opID])
	}
	if _, bad := st.failed[opID]; bad {
		t.Errorf("failed path should not fire for completed status")
	}
}

func TestOpUpdate_CompletedStatus_PowerOpLeavesStateUntouched(t *testing.T) {
	st := newFake()
	h := NewOpUpdate(st)
	opID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, Type: model.OpPowerCycle}

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Status:      string(model.OpCompleted),
	})
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if got := st.completedState[opID]; got != "" {
		t.Errorf("power op should not transition machine state, got %q", got)
	}
}

func TestOpUpdate_FailedStatus(t *testing.T) {
	st := newFake()
	h := NewOpUpdate(st)
	opID := uuid.New()

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Status:      string(model.OpFailed),
		Result:      json.RawMessage(`{"error":"disk blank check failed"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if string(st.failed[opID]) != `{"error":"disk blank check failed"}` {
		t.Errorf("failed result: got %q", st.failed[opID])
	}
}

func TestOpUpdate_UnknownStatusIgnored(t *testing.T) {
	st := newFake()
	h := NewOpUpdate(st)
	opID := uuid.New()

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Status:      "weird",
	})
	if err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.completed) != 0 || len(st.failed) != 0 {
		t.Errorf("unknown status should not transition op")
	}
}

func TestOpUpdate_MalformedOpID(t *testing.T) {
	h := NewOpUpdate(newFake())
	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: "not-a-uuid",
		Phase:       "wat",
	})
	if err == nil {
		t.Fatal("expected error on malformed op_id")
	}
}

func TestOpUpdate_PhaseErrorNotPropagatedWhenStatusPresent(t *testing.T) {
	// Phase-write failure shouldn't short-circuit a terminal status
	// update: op_update is fire-and-forget, so dropping the status
	// here would strand the machine in its transient state forever.
	// Phase error should log but still process the status.
	st := newFake()
	st.phaseErr = errors.New("db down")
	opID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, Type: model.OpDeploy}
	h := NewOpUpdate(st)

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "completed",
		Status:      string(model.OpCompleted),
	})
	if err != nil {
		t.Fatalf("phase error should be logged, not propagated: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.completed[opID]; !ok {
		t.Error("status transition should have happened despite phase failure")
	}
}

func TestOpUpdate_PhaseOnlyErrorPropagatesWhenNoStatus(t *testing.T) {
	// Phase-only call with no status: if the phase write fails, that
	// IS the whole operation. Surface the error so the hub's dispatch
	// wrapper logs it.
	st := newFake()
	st.phaseErr = errors.New("db down")
	h := NewOpUpdate(st)

	err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: uuid.NewString(),
		Phase:       "partitioning",
	})
	if err == nil {
		t.Fatal("expected error from SetOperationPhase when no status fallthrough")
	}
}

// --- LogBatch tests ---

func TestLogBatch_HappyPath(t *testing.T) {
	logs := newFakeLogStore()
	stdout := &bytes.Buffer{}
	sinkRing := newCaptureSink()
	h := NewLogBatch(logs, stdout, sinkRing)
	envs := []slogkit.Envelope{
		{Source: slogkit.SourceAgent, Level: slogkit.LevelInfo, Msg: "starting", TraceID: uuid.New()},
		{Source: slogkit.SourceAgent, Level: slogkit.LevelInfo, Msg: "phase", TraceID: uuid.New()},
	}
	err := h.HandleLogBatch(context.Background(), "worker-A", ws.LogBatchMsg{Envelopes: envs})
	if err != nil {
		t.Fatal(err)
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if len(logs.envelopes) != 2 {
		t.Errorf("InsertBatch call count: got %d envs, want 2", len(logs.envelopes))
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"msg":"starting"`)) {
		t.Errorf("stdout missing emitted envelope: %q", stdout.String())
	}
	if len(sinkRing.got) != 2 {
		t.Errorf("sink ring should receive both envelopes, got %d", len(sinkRing.got))
	}
}

func TestLogBatch_EmptyIsNoOp(t *testing.T) {
	logs := newFakeLogStore()
	stdout := &bytes.Buffer{}
	h := NewLogBatch(logs, stdout, nil)
	if err := h.HandleLogBatch(context.Background(), "w", ws.LogBatchMsg{}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty for empty batch, got %q", stdout.String())
	}
	if len(logs.envelopes) != 0 {
		t.Errorf("InsertBatch should not run for empty batch")
	}
}

func TestLogBatch_StorePersistFailureIsReturned(t *testing.T) {
	logs := newFakeLogStore()
	logs.err = errors.New("pg down")
	h := NewLogBatch(logs, nil, nil)
	err := h.HandleLogBatch(context.Background(), "w",
		ws.LogBatchMsg{Envelopes: []slogkit.Envelope{{Source: slogkit.SourceAgent, Msg: "x"}}})
	if err == nil {
		t.Fatal("expected error from store, got nil")
	}
}

type captureSink struct {
	got []slogkit.Envelope
}

func newCaptureSink() *captureSink { return &captureSink{} }
func (s *captureSink) Push(e slogkit.Envelope) {
	s.got = append(s.got, e)
}

// fakePublisher records op-stream publishes for assertions.
type fakePublisher struct {
	deltas   []model.Operation
	removed  []model.Operation
	deltaSt  []model.MachineState
	removeSt []model.MachineState
}

func (p *fakePublisher) PublishOpDelta(op model.Operation, st model.MachineState) {
	p.deltas = append(p.deltas, op)
	p.deltaSt = append(p.deltaSt, st)
}
func (p *fakePublisher) PublishOpRemoved(op model.Operation, st model.MachineState) {
	p.removed = append(p.removed, op)
	p.removeSt = append(p.removeSt, st)
}

// TestOpUpdate_PublishesAndInvalidates asserts the Tier-2 split: a real
// phase transition publishes an op_delta (and does NOT invalidate identity
// — phases ride the op stream alone); an idempotent phase repeat publishes
// nothing; a terminal completed publishes op_removed AND, because it wrote
// durable machines.state, fires exactly one identity rebuild.
func TestOpUpdate_PublishesAndInvalidates(t *testing.T) {
	st := newFake()
	opID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, Type: model.OpDeploy}

	var invalidations int
	pub := &fakePublisher{}
	h := NewOpUpdate(st)
	h.SetInvalidator(func() { invalidations++ })
	h.SetPublisher(pub)

	// First phase: a real change -> op_delta, no invalidate.
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(), Phase: "partitioning",
	}); err != nil {
		t.Fatal(err)
	}
	if len(pub.deltas) != 1 {
		t.Fatalf("phase change should publish one op_delta, got %d", len(pub.deltas))
	}
	if pub.deltaSt[0] != "" {
		t.Fatalf("phase op_delta must carry no machine_state, got %q", pub.deltaSt[0])
	}
	if invalidations != 0 {
		t.Fatalf("phase transition must NOT invalidate identity, got %d", invalidations)
	}

	// Same phase again: no change -> no publish.
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(), Phase: "partitioning",
	}); err != nil {
		t.Fatal(err)
	}
	if len(pub.deltas) != 1 {
		t.Fatalf("idempotent phase repeat must not publish, got %d", len(pub.deltas))
	}

	// Terminal completed -> op_removed + one identity rebuild (durable
	// running state written).
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(), Status: string(model.OpCompleted),
	}); err != nil {
		t.Fatal(err)
	}
	if len(pub.removed) != 1 {
		t.Fatalf("terminal completed should publish one op_removed, got %d", len(pub.removed))
	}
	if pub.removeSt[0] != model.MachineStateRunning {
		t.Fatalf("deploy completion op_removed must carry running, got %q", pub.removeSt[0])
	}
	if invalidations != 1 {
		t.Fatalf("durable completion should invalidate identity once, got %d", invalidations)
	}
}

// TestOpUpdate_NilInvalidatorSafe: a handler with no invalidator wired
// (the persisting-only / test default) must not panic.
func TestOpUpdate_NilInvalidatorSafe(t *testing.T) {
	st := newFake()
	opID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, Type: model.OpDeploy}
	h := NewOpUpdate(st) // no SetInvalidator
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(), Phase: "partitioning",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: opID.String(), Status: string(model.OpFailed),
	}); err != nil {
		t.Fatal(err)
	}
}
