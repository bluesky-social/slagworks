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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opsmetrics"
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

	getErr      error
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

// putOp registers an active op of the given type and returns its ID.
func (s *fakeStore) putOp(typ model.OperationType) uuid.UUID {
	id := uuid.New()
	s.opByID[id] = &model.Operation{ID: id, Type: typ, Status: model.OpClaimed}
	return id
}

func (s *fakeStore) GetOperationIfExists(id uuid.UUID) (*model.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	op := s.opByID[id]
	if op == nil {
		return nil, nil
	}
	cp := *op
	return &cp, nil
}

func fakeTerminal(op *model.Operation) bool {
	return op != nil && (op.Status == model.OpCompleted || op.Status == model.OpFailed)
}

// SetOperationPhase mirrors store.SetOperationPhase: a repeated phase and
// a terminal op change nothing.
func (s *fakeStore) SetOperationPhase(opID uuid.UUID, phase string) (store.PhaseTransition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phaseErr != nil {
		return store.PhaseTransition{}, s.phaseErr
	}
	prev := s.phases[opID]
	t := store.PhaseTransition{NewPhase: phase}
	if prev != phase && !fakeTerminal(s.opByID[opID]) {
		s.phases[opID] = phase
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
	if fakeTerminal(s.opByID[opID]) {
		return nil, nil
	}
	s.completed[opID] = append([]byte(nil), result...)
	s.completedState[opID] = targetState
	op := model.Operation{ID: opID}
	if o := s.opByID[opID]; o != nil {
		o.Status = model.OpCompleted
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
	if fakeTerminal(s.opByID[opID]) {
		return nil, nil
	}
	s.failed[opID] = append([]byte(nil), result...)
	op := model.Operation{ID: opID}
	if o := s.opByID[opID]; o != nil {
		o.Status = model.OpFailed
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
	opID := st.putOp(model.OpDeploy)

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
	opID := st.putOp(model.OpDeploy)

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

// TestOpUpdate_NeverAcceptableIsTerminal: an update no retry can apply
// returns an error wrapping ws.ErrTerminal, so the worker drops it instead
// of resending it forever and holding the op's later updates behind it.
// Nothing is written.
func TestOpUpdate_NeverAcceptableIsTerminal(t *testing.T) {
	st := newFake()
	known := st.putOp(model.OpDeploy)
	for _, tc := range []struct {
		name string
		msg  ws.OpUpdateMsg
	}{
		{"malformed op_id", ws.OpUpdateMsg{OperationID: "not-a-uuid", Phase: "wat"}},
		{"unknown op", ws.OpUpdateMsg{OperationID: uuid.NewString(), Phase: "partitioning"}},
		{"unsupported status", ws.OpUpdateMsg{OperationID: known.String(), Status: "weird"}},
		{"running status", ws.OpUpdateMsg{OperationID: known.String(), Status: string(model.OpRunning)}},
		{"neither phase nor status", ws.OpUpdateMsg{OperationID: known.String()}},
		{"NUL in phase", ws.OpUpdateMsg{OperationID: known.String(), Phase: "part\x00itioning"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := NewOpUpdate(st).HandleOpUpdate(context.Background(), tc.msg)
			if !errors.Is(err, ws.ErrTerminal) {
				t.Fatalf("got %v, want an error wrapping ws.ErrTerminal", err)
			}
		})
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.phases) != 0 || len(st.completed) != 0 || len(st.failed) != 0 {
		t.Errorf("rejected updates wrote: phases=%v completed=%v failed=%v", st.phases, st.completed, st.failed)
	}
}

// TestOpUpdate_StoreErrorsAreTransient: a failed read or write returns an
// error that does not wrap ws.ErrTerminal, so the worker keeps the update
// and resends it.
func TestOpUpdate_StoreErrorsAreTransient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject func(*fakeStore)
		msg    func(uuid.UUID) ws.OpUpdateMsg
	}{
		{"read", func(s *fakeStore) { s.getErr = errors.New("db down") },
			func(id uuid.UUID) ws.OpUpdateMsg {
				return ws.OpUpdateMsg{OperationID: id.String(), Phase: "partitioning"}
			}},
		{"phase write", func(s *fakeStore) { s.phaseErr = errors.New("db down") },
			func(id uuid.UUID) ws.OpUpdateMsg {
				return ws.OpUpdateMsg{OperationID: id.String(), Phase: "partitioning"}
			}},
		{"completion", func(s *fakeStore) { s.completeErr = errors.New("db down") },
			func(id uuid.UUID) ws.OpUpdateMsg {
				return ws.OpUpdateMsg{OperationID: id.String(), Status: string(model.OpCompleted)}
			}},
		{"failure", func(s *fakeStore) { s.failErr = errors.New("db down") },
			func(id uuid.UUID) ws.OpUpdateMsg {
				return ws.OpUpdateMsg{OperationID: id.String(), Status: string(model.OpFailed)}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newFake()
			opID := st.putOp(model.OpPowerCycle)
			tc.inject(st)
			err := NewOpUpdate(st).HandleOpUpdate(context.Background(), tc.msg(opID))
			if err == nil || errors.Is(err, ws.ErrTerminal) {
				t.Fatalf("got %v, want a transient (non-terminal) error", err)
			}
		})
	}
}

// TestOpUpdate_PhaseErrorFailsWholeUpdate: when the phase write of a
// terminal update fails, the status is not written either and the update
// is retried; the resend then applies the phase and the status in order.
func TestOpUpdate_PhaseErrorFailsWholeUpdate(t *testing.T) {
	st := newFake()
	st.phaseErr = errors.New("db down")
	opID := st.putOp(model.OpDeploy)
	h := NewOpUpdate(st)
	msg := ws.OpUpdateMsg{
		OperationID: opID.String(),
		Phase:       "partitioning",
		Status:      string(model.OpFailed),
		Result:      json.RawMessage(`{"error":"boom","phase":"partitioning"}`),
	}

	err := h.HandleOpUpdate(context.Background(), msg)
	if err == nil || errors.Is(err, ws.ErrTerminal) {
		t.Fatalf("phase failure must fail the update transiently, got %v", err)
	}
	st.mu.Lock()
	if _, ok := st.failed[opID]; ok {
		t.Error("status was written although the phase before it failed")
	}
	st.phaseErr = nil
	st.mu.Unlock()

	if err := h.HandleOpUpdate(context.Background(), msg); err != nil {
		t.Fatalf("resend: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phases[opID] != "partitioning" {
		t.Errorf("phase after resend = %q, want partitioning", st.phases[opID])
	}
	if _, ok := st.failed[opID]; !ok {
		t.Error("status not written by the resend")
	}
}

// TestOpUpdate_ResendIsIdempotent: the worker resends an update whose
// acknowledgement it did not receive. A resend of an update already
// applied changes nothing: no second phase-transition envelope, op_delta
// or phase metric, no second op_removed, outcome count or identity
// rebuild, and a phase resent after the op went terminal moves nothing.
func TestOpUpdate_ResendIsIdempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	st := newFake()
	opID := st.putOp(model.OpDeploy)
	logs := &bytes.Buffer{}
	pub := &fakePublisher{}
	invalidations := 0
	h := NewOpUpdate(st)
	h.SetLogger(newJSONLoggerForTest(logs))
	h.SetPublisher(pub)
	h.SetMetrics(opsmetrics.New(reg))
	h.SetInvalidator(func() { invalidations++ })

	send := func(msg ws.OpUpdateMsg) {
		t.Helper()
		if err := h.HandleOpUpdate(context.Background(), msg); err != nil {
			t.Fatalf("%+v: %v", msg, err)
		}
	}
	first := ws.OpUpdateMsg{OperationID: opID.String(), Phase: "partitioning"}
	second := ws.OpUpdateMsg{OperationID: opID.String(), Phase: "rsyncing_os"}
	done := ws.OpUpdateMsg{OperationID: opID.String(), Status: string(model.OpCompleted)}
	send(first)
	send(second)
	send(second) // ack of second lost: resent
	send(done)
	send(done)  // ack of the completion lost: resent
	send(first) // an older phase arriving after the op went terminal

	transitions := 0
	for _, r := range decodeJSONLines(t, logs) {
		if r["msg"] == "phase transition" {
			transitions++
		}
	}
	if transitions != 2 {
		t.Errorf("phase-transition envelopes = %d, want 2", transitions)
	}
	if len(pub.deltas) != 2 {
		t.Errorf("op_deltas = %d, want 2", len(pub.deltas))
	}
	if len(pub.removed) != 1 || invalidations != 1 {
		t.Errorf("op_removed = %d, identity rebuilds = %d, want 1 and 1", len(pub.removed), invalidations)
	}
	if got := metricValue(t, reg, "slagworks_operation_outcomes_total",
		map[string]string{"op_type": "deploy", "outcome": "completed"}); got != 1 {
		t.Errorf("completed outcomes counted %v times, want 1", got)
	}
	if got := metricValue(t, reg, "slagworks_op_phase_duration_seconds",
		map[string]string{"op_type": "deploy", "phase": "partitioning"}); got != 1 {
		t.Errorf("exited-phase durations observed %v times, want 1", got)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.phases[opID] != "rsyncing_os" {
		t.Errorf("phase = %q, want rsyncing_os", st.phases[opID])
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
		{Source: slogkit.SourceSite, Level: slogkit.LevelInfo, Msg: "phase", TraceID: uuid.New()},
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

// TestLogBatch_RejectsSourceGlobalFromWorker pins that a worker cannot
// relay a source=global envelope. Global writes its op-lifecycle envelopes
// (claimed / completed / failed) itself, inside the store transaction that
// makes the transition, and `slagctl operation watch` treats a source=global
// status=completed envelope as the op's authoritative terminal signal. A
// worker-supplied copy must therefore never reach log_envelopes, the live
// sink, or stdout, or a forged envelope makes the watch report "completed"
// and exit 0 for an op that is still running.
func TestLogBatch_RejectsSourceGlobalFromWorker(t *testing.T) {
	logs := newFakeLogStore()
	stdout := &bytes.Buffer{}
	sinkRing := newCaptureSink()
	h := NewLogBatch(logs, stdout, sinkRing)
	forged := slogkit.Envelope{
		Source: slogkit.SourceGlobal, Level: slogkit.LevelInfo, Host: "slagworksd-global",
		Msg: "operation completed", MachineID: uuid.New(), OpID: uuid.New(),
		Extra: map[string]any{"status": "completed"},
	}
	agent := slogkit.Envelope{Source: slogkit.SourceAgent, Level: slogkit.LevelInfo, Msg: "partitioning"}

	err := h.HandleLogBatch(context.Background(), "worker-A",
		ws.LogBatchMsg{Envelopes: []slogkit.Envelope{agent, forged}})

	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, e := range logs.envelopes {
		if e.Source == slogkit.SourceGlobal {
			t.Fatalf("worker batch persisted a source=global envelope (msg=%q extra=%v); the watch TUI would read it as the op's terminal signal", e.Msg, e.Extra)
		}
	}
	if len(logs.envelopes) != 0 {
		t.Errorf("rejected batch persisted %d envelopes, want 0", len(logs.envelopes))
	}
	if len(sinkRing.got) != 0 {
		t.Errorf("rejected batch reached the live sink: %d envelopes", len(sinkRing.got))
	}
	if stdout.Len() != 0 {
		t.Errorf("rejected batch was re-emitted on stdout: %q", stdout.String())
	}
	if err == nil {
		t.Fatal("expected HandleLogBatch to return an error for a batch carrying a source=global envelope, got nil")
	}
	if !strings.Contains(err.Error(), "worker-A") {
		t.Errorf("error should name the offending worker, got %q", err)
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
