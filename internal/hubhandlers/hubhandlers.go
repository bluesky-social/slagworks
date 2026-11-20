// Package hubhandlers provides the concrete ws.OpUpdateHandler and
// ws.LogBatchHandler implementations that persist worker-emitted
// progress into slagworks's Postgres state.
//
// OpUpdate's error is the worker's acknowledgement: the hub answers the
// op_update's request_id with it, and the worker keeps the update in its
// outbox until it gets an answer that is not a transient error (see
// ws.OpUpdateHandler). LogBatch's errors are only logged: log batches are
// fire-and-forget.
package hubhandlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opsmetrics"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Store is the subset of store.Store these handlers touch. The
// completion/fail methods return the post-mutation OpOutcome (bumped
// Version + the durable machine state, if any) so the handler publishes
// the authoritative row on the op stream rather than a hand-assembled one.
type Store interface {
	// GetOperationIfExists returns (nil, nil) for an op that does not
	// exist, so the handler can tell an update no retry can apply from a
	// failed read.
	GetOperationIfExists(id uuid.UUID) (*model.Operation, error)
	SetOperationPhase(opID uuid.UUID, phase string) (store.PhaseTransition, error)
	CompleteOperationWithState(opID uuid.UUID, targetState model.MachineState, result []byte) (*store.OpOutcome, error)
	FailOperationOutcome(opID uuid.UUID, result []byte) (*store.OpOutcome, error)
}

// OpPublisher is the op-event publish surface (satisfied by *ws.Hub). The
// handler publishes op_delta for a phase transition and op_removed for a
// terminal one, instead of invalidating the whole identity snapshot on
// every transition. Nil leaves the handler persisting-only (tests).
type OpPublisher interface {
	PublishOpDelta(op model.Operation, machineState model.MachineState)
	PublishOpRemoved(op model.Operation, machineState model.MachineState)
}

// LogStore is the subset of logstore.Store the LogBatch handler uses.
type LogStore interface {
	InsertBatch(ctx context.Context, envelopes []slogkit.Envelope) error
}

// completionState picks the target machine state for a successful op.
//
// For OpDeploy, the worker only emits an op_update with status=
// completed when phone-home arrives at the site worker (see
// Cache.RecordPhoneHome). deploy_complete from the agent only updates
// phase to awaiting_phone_home; the op stays claimed until phone-home
// completes it. The same phone-home first queues a phone_home
// observation naming the op, which internal/commission completes the op
// from; the worker delivers an op's entries in the order it recorded
// them, so this completion normally finds the op already terminal and
// changes nothing.
//
// commission is handled through internal/commission's observation
// path, not here, but we include it for completeness against ad-hoc
// completions.
func completionState(opType model.OperationType) model.MachineState {
	switch opType {
	case model.OpDeploy:
		return model.MachineStateRunning
	case model.OpCommission:
		return model.MachineStateCommissioned
	default:
		// Power ops don't transition the machine state beyond what
		// their side-effects already did. Return the existing state
		// by using a marker that CompleteOperationWithState will
		// treat as "don't change state". We don't have such a marker
		// so we instead short-circuit callers for unknown types.
		return ""
	}
}

// OpUpdate persists OpUpdateMsg into the operation row.
//
// Semantics by incoming field:
//   - Phase set               → SetOperationPhase (keeps status/result untouched)
//   - Status = "completed"    → CompleteOperationWithState with Result
//   - Status = "failed"       → FailOperationOutcome with Result (also transitions machine)
//
// The phase lands before the status of the same update. HandleOpUpdate
// returns nil only once every write the update asks for has committed or
// changed nothing (the op is already terminal, or already at that phase),
// which is what makes a resend of an update global already applied
// harmless: a repeated phase is not a transition (no phase_entered_at
// bump, no envelope, no op_delta) and a terminal op takes no further
// writes. An update no retry can apply (malformed op ID, unknown status,
// no phase and no status, a NUL in the phase, unknown op) returns an
// error wrapping
// ws.ErrTerminal; every other error is transient and the worker resends
// the whole update.
type OpUpdate struct {
	store   Store
	log     *slog.Logger
	metrics *opsmetrics.Metrics // nil = no metric observation
	// publisher fans op_delta (phase) / op_removed (terminal) to workers
	// on the op-event stream. Transient/phase transitions ride the stream
	// alone; a terminal transition that wrote DURABLE machines.state also
	// fires an identity rebuild via invalidate (see the rule below). Nil
	// leaves the handler persisting-only (tests).
	publisher OpPublisher
	// invalidate, when set, fires an identity snapshot rebuild. Post-Tier-2
	// it is called ONLY when a transition wrote durable machines.state (a
	// completion → running/commissioned, a fail → failed/new), so the
	// identity baseline catches up to the durable resting state. Phase
	// transitions no longer invalidate — they publish an op_delta.
	invalidate func()
}

func NewOpUpdate(st Store) *OpUpdate {
	return &OpUpdate{store: st, log: slog.Default()}
}

// SetMetrics wires the op-lifecycle metrics. Optional; nil skips
// observation (tests, persisting-only mode).
func (h *OpUpdate) SetMetrics(m *opsmetrics.Metrics) { h.metrics = m }

func (h *OpUpdate) SetLogger(l *slog.Logger) { h.log = l }

// SetPublisher wires the op-event publisher. Optional: nil leaves the
// handler persisting-only.
func (h *OpUpdate) SetPublisher(p OpPublisher) { h.publisher = p }

// SetInvalidator wires the durable-state identity-rebuild callback
// (typically a fan-out across all site_configs, matching the API
// server's invalidateAllSites). Optional; nil leaves the handler
// publishing-only for durable transitions (the 5-min reconcile catches up).
func (h *OpUpdate) SetInvalidator(f func()) { h.invalidate = f }

// invalidateIdentity fires an identity rebuild if wired. Called only on a
// durable machines.state write (see the rule in the plan). No-op when nil.
func (h *OpUpdate) invalidateIdentity() {
	if h.invalidate != nil {
		h.invalidate()
	}
}

func (h *OpUpdate) publishDelta(op model.Operation, st model.MachineState) {
	if h.publisher != nil {
		h.publisher.PublishOpDelta(op, st)
	}
}

func (h *OpUpdate) publishRemoved(op model.Operation, st model.MachineState) {
	if h.publisher != nil {
		h.publisher.PublishOpRemoved(op, st)
	}
}

// HandleOpUpdate ignores ctx because the store layer doesn't yet
// accept context. The ws hub's per-handler timeout is therefore
// best-effort against Postgres — a genuinely hanging query will
// stall the caller until the TCP layer notices.
func (h *OpUpdate) HandleOpUpdate(_ context.Context, msg ws.OpUpdateMsg) error {
	opID, err := uuid.Parse(msg.OperationID)
	if err != nil {
		return fmt.Errorf("parse op_id %q: %v: %w", msg.OperationID, err, ws.ErrTerminal)
	}
	switch msg.Status {
	case "", string(model.OpCompleted), string(model.OpFailed):
	default:
		return fmt.Errorf("op_update for %s carries unsupported status %q: %w", opID, msg.Status, ws.ErrTerminal)
	}
	if msg.Phase == "" && msg.Status == "" {
		return fmt.Errorf("op_update for %s carries neither a phase nor a status: %w", opID, ws.ErrTerminal)
	}
	if strings.ContainsRune(msg.Phase, 0) {
		// Postgres text cannot hold a NUL: the phase write would fail on
		// every resend and hold back the op's later updates.
		return fmt.Errorf("op_update for %s carries a NUL in its phase: %w", opID, ws.ErrTerminal)
	}
	op, err := h.store.GetOperationIfExists(opID)
	if err != nil {
		return fmt.Errorf("read op %s: %w", opID, err)
	}
	if op == nil {
		return fmt.Errorf("operation %s not found: %w", opID, ws.ErrTerminal)
	}

	// Phase update is independent of status: a worker may emit a
	// phase-only op_update ("partitioning") mid-run, AND a terminal
	// update carrying a final phase + failed status. Apply phase first so
	// even the terminal update's phase lands before status. A failed
	// phase write fails the whole update, so the worker resends it: the
	// phase then applies, and the status after it.
	if msg.Phase != "" {
		t, err := h.store.SetOperationPhase(opID, msg.Phase)
		switch {
		case err != nil:
			return fmt.Errorf("set phase %q on %s: %w", msg.Phase, opID, err)
		case t.Changed:
			if h.metrics != nil && t.PrevPhase != "" {
				// Label by the phase EXITED and the op type.
				h.metrics.PhaseDuration.WithLabelValues(string(op.Type), t.PrevPhase).
					Observe(t.Duration.Seconds())
			}
			// One canonical "phase transition" envelope per real change.
			// Slogkit hoists machine_id/op_id/trace_id from
			// these attrs to the envelope's top-level fields.
			h.log.Info("phase transition",
				"op_id", opID,
				"machine_id", t.MachineID,
				"trace_id", t.TraceID,
				"phase", t.NewPhase,
				"prev_phase", t.PrevPhase,
				"duration_ms", t.Duration.Milliseconds())
			// A phase transition changed op state but NO machine state:
			// publish an op_delta (the authoritative post-mutation row)
			// so the worker's opState + SOL-pump Reconcile see the live
			// phase, without an identity rebuild. Only on t.Changed — an
			// idempotent repeat must not re-publish. t.Op is the
			// post-mutation row.
			if t.Op != nil {
				h.publishDelta(*t.Op, "")
			}
		}
	}

	// Result may be present on either terminal status OR on a plain
	// phase update (unusual but valid). We only write Result as part
	// of the status transition so we don't clobber it post-completion.
	// Terminal transition: the op left the active set — publish op_removed
	// so the worker drops it. If it wrote DURABLE machines.state
	// (deploy→running, commission→commissioned, a fail → failed/new), the
	// message carries that state (worker overlay tracks it) AND we fire an
	// identity rebuild so the baseline catches up. A power-op completion or
	// a pending-op fail carries no state and rides the stream alone.
	// outcome is nil on a no-op (already terminal / lost race, a resend of
	// an update already applied): publish, count and log nothing.
	var outcome *store.OpOutcome
	switch msg.Status {
	case "":
		return nil
	case string(model.OpCompleted):
		// Deploy → running, commission → commissioned. Power ops don't
		// transition the machine, so we pass "" and the store skips the
		// state update.
		target := completionState(op.Type)
		outcome, err = h.store.CompleteOperationWithState(opID, target, resultBytes(msg.Result))
		if err != nil {
			return fmt.Errorf("complete op %s: %w", opID, err)
		}
		if outcome == nil {
			return nil
		}
		h.log.Info("operation completed",
			"op_id", opID, "type", op.Type, "machine_id", op.MachineID,
			"target_state", target)
	default: // model.OpFailed, checked above
		outcome, err = h.store.FailOperationOutcome(opID, resultBytes(msg.Result))
		if err != nil {
			return fmt.Errorf("fail op %s: %w", opID, err)
		}
		if outcome == nil {
			return nil
		}
		h.log.Info("operation failed",
			"op_id", opID, "type", op.Type, "machine_id", op.MachineID)
	}
	if h.metrics != nil {
		h.metrics.Outcomes.WithLabelValues(string(op.Type), msg.Status).Inc()
	}
	h.publishRemoved(outcome.Op, outcome.MachineState)
	if outcome.MachineState != "" {
		h.invalidateIdentity()
	}
	return nil
}

// LogBatch persists structured log envelopes from site workers and
// re-emits each envelope as one JSONL line on stdout (Stdout) so
// alloy-logs / Loki and `kubectl logs` see the full fleet stream.
//
// SoSink (the global daemon's own in-process forwarding ring) is
// optional; when set, every accepted envelope is also pushed there so
// global's outbound /api/v1/logs/stream subscribers see the same
// envelopes that just landed.
type LogBatch struct {
	logs   LogStore
	stdout io.Writer
	sink   slogkit.Sink
	log    *slog.Logger
}

func NewLogBatch(logs LogStore, stdout io.Writer, sink slogkit.Sink) *LogBatch {
	return &LogBatch{logs: logs, stdout: stdout, sink: sink, log: slog.Default()}
}

func (h *LogBatch) SetLogger(l *slog.Logger) { h.log = l }

// HandleLogBatch persists then re-emits. Persistence comes first so
// `slagctl logs` (which queries pg) cannot miss an envelope
// that already echoed to stdout for Loki.
func (h *LogBatch) HandleLogBatch(ctx context.Context, workerID string, msg ws.LogBatchMsg) error {
	if len(msg.Envelopes) == 0 {
		return nil
	}
	// A worker relays only what its site ring holds: its own
	// envelopes (source=site) and the agent's (source=agent, the only
	// source /logs/ingest accepts). Any other source rejects the whole frame,
	// before anything is persisted or re-emitted. source=global in
	// particular is global's own: op-lifecycle envelopes are written
	// inside the store transaction that makes the transition
	// (store/op_events.go), and the watch TUI treats a source=global
	// status=completed/failed envelope as the op's authoritative
	// terminal signal, so a worker-relayed one would be a forged op
	// outcome. Rejecting the frame rather than dropping the one
	// envelope matches the stale-binary case below: a healthy worker
	// never sends either, so the batch is not trusted.
	for _, env := range msg.Envelopes {
		switch env.Source {
		case slogkit.SourceAgent, slogkit.SourceSite:
		case "sol":
			// String literal: the SourceSOL constant no longer
			// exists. SOL bytes ride the asciinema cast pipeline;
			// a pre-cast-pipeline binary that still emits them
			// gets a redeploy hint.
			return fmt.Errorf("worker=%s emitted source=sol envelope; SOL rides the cast pipeline — redeploy the worker", workerID)
		default:
			return fmt.Errorf("worker=%s relayed a source=%q envelope (machine_id=%s op_id=%s); workers may relay only agent and site envelopes, so the batch was dropped — upgrade the worker if it predates /logs/ingest refusing non-agent envelopes, and look for a host posting forged envelopes to its /logs/ingest",
				workerID, env.Source, env.MachineID, env.OpID)
		}
	}
	if err := h.logs.InsertBatch(ctx, msg.Envelopes); err != nil {
		return fmt.Errorf("insert envelopes from worker=%s count=%d: %w",
			workerID, len(msg.Envelopes), err)
	}
	for _, env := range msg.Envelopes {
		if h.stdout != nil {
			buf, _ := json.Marshal(env)
			buf = append(buf, '\n')
			_, _ = h.stdout.Write(buf)
		}
		if h.sink != nil {
			h.sink.Push(env)
		}
	}
	return nil
}

// resultBytes returns the json.RawMessage as a []byte suitable for
// storing. nil in → nil out (leaves the DB column untouched).
func resultBytes(r json.RawMessage) []byte {
	if len(r) == 0 {
		return nil
	}
	return []byte(r)
}
