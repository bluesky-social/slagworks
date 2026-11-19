package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
)

func (s *Store) CreateOperation(op *model.Operation) error {
	if op.ID == uuid.Nil {
		op.ID = uuid.New()
	}
	if op.TraceID == uuid.Nil {
		op.TraceID = uuid.New()
	}
	return s.db.Create(op).Error
}

func (s *Store) GetOperation(id uuid.UUID) (*model.Operation, error) {
	var op model.Operation
	if err := s.db.First(&op, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &op, nil
}

// GetOperationIfExists reads an op by ID, returning (nil, nil) when no row
// matches (rather than the gorm not-found sentinel) so callers can
// distinguish "op is gone" (nil, nil) from a real DB failure (nil, err)
// without importing gorm. The ws hub's P4b outlet-op interlock uses it to
// read a pending op's Params BEFORE the claim CAS: a genuinely-absent op
// falls through to the CAS (which reports already-claimed), while a store
// error fails the eligibility check closed.
func (s *Store) GetOperationIfExists(id uuid.UUID) (*model.Operation, error) {
	var op model.Operation
	err := s.db.First(&op, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// OpOutcome is the post-mutation result of an op state transition, for
// the op-event publisher (Tier 2 Phase 2). Op is the operation row after
// the mutation (bumped Version) so the delta/removed message carries the
// authoritative row rather than a hand-assembled one that could drift.
// MachineState is the DURABLE machine state the transition wrote
// (running/commissioned/failed/new) — the value the accompanying identity
// rebuild will deliver — or "" when the transition changed no machine
// state (a pending op that never advanced state, a power op, or a
// phase-only update). The publisher fires an identity rebuild only when
// MachineState is non-empty.
type OpOutcome struct {
	Op           model.Operation
	MachineState model.MachineState
}

// ListActiveOperations returns every operation in opstate.ActiveStatuses
// (pending/claimed/running) across the whole fleet. The op-event stream's
// hello resync sends this set to a (re)connecting worker; the worker
// filters by identity-view membership. Bounded by concurrent-op count
// (dozens), not machine count.
func (s *Store) ListActiveOperations() ([]model.Operation, error) {
	var out []model.Operation
	err := s.db.Where("status IN ?", opstate.ActiveStatuses).Find(&out).Error
	return out, err
}

// ListActiveOperationsClaimedBy returns every operation in
// opstate.ActiveStatuses (pending/claimed/running) whose claimed_by
// equals workerID. Used by the site-worker delete admin path to refuse
// deleting a worker with in-flight claims (deleting the row while an op
// is claimed by it would strand that op — the worker that owns it can no
// longer authenticate). Terminal-status rows (completed/failed) are not
// considered: their claimed_by is a plain string with no FK, so they
// harmlessly reference a since-deleted worker.
func (s *Store) ListActiveOperationsClaimedBy(workerID string) ([]model.Operation, error) {
	var out []model.Operation
	err := s.db.Where("status IN ? AND claimed_by = ?", opstate.ActiveStatuses, workerID).Find(&out).Error
	return out, err
}

func (s *Store) ListOperations(machineID *uuid.UUID) ([]model.Operation, error) {
	var ops []model.Operation
	q := s.db
	if machineID != nil {
		q = q.Where("machine_id = ?", *machineID)
	}
	return ops, q.Order("created_at desc").Find(&ops).Error
}

// ClaimOperation atomically claims a pending operation for a worker
// AND advances the machine's state to the corresponding transient
// state (commission → commissioning, deploy → deploying). Power ops
// do not advance machine state. The transition-legality decisions live
// in internal/opstate; this method owns the SQL, row locks, and CAS
// that make the claim atomic.
//
// Returns:
//   - (op, nil): claim succeeded; op is fresh, machine state advanced.
//   - (nil, nil): op was no longer pending (raced or already terminal).
//   - (nil, err): claim refused because the machine was not in a
//     resting state from which this op can transition (e.g. machine
//     is already in `running` and a stale pending deploy is found —
//     forward-path scope doesn't support that). Or any DB error.
//
// Resting-state guard: at claim time, the machine MUST be in a state
// from which the op type can transition forward. This is a
// re-validation of CreateMachineOp's allowed-source check, against
// the very small race window where state could have moved between
// op create and claim. The check costs one row read; without it we'd
// silently flip a `running` machine into `deploying` on a leaked op.
func (s *Store) ClaimOperation(opID uuid.UUID, workerID string) (*model.Operation, error) {
	var claimed *model.Operation
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var op model.Operation
		// FOR UPDATE: take the same op row lock FailOperation takes, so
		// claim and fail are serialized. Without both sides locking, a
		// claim racing an abort/timeout-fail can leave the machine stuck
		// in a transient state (see FailOperation's wasPending comment).
		// Locked regardless of status — the re-claim branch below needs
		// to see claimed rows too.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", opID).
			First(&op).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		// Idempotent re-claim (B4): the claim RPC is at-least-once — a
		// worker whose claim COMMITTED but whose reply was lost on a WS
		// drop retries the claim on its next executor tick. When the op
		// is still re-claimable by this worker (opstate.CanReclaim), hand
		// it back so the deploy proceeds instead of stalling until a
		// sweep. claimed_at is bumped so ExpireOrphanedClaims' grace
		// window restarts from the re-claim, not the original claim. A
		// different worker, or any recorded phase, falls through and
		// reads as claim-lost.
		if opstate.CanReclaim(&op, workerID) {
			now := time.Now()
			if err := tx.Model(&op).Updates(map[string]any{
				"claimed_at": now,
				"version":    gorm.Expr("version + 1"),
			}).Error; err != nil {
				return err
			}
			op.ClaimedAt = &now
			// Mirror the version bump into the returned struct: the
			// op-delta publisher builds its message from this value, and
			// a stale (pre-bump) version would make workers discard the
			// re-claim delta under last-writer-wins.
			op.Version++
			claimed = &op
			return nil
		}
		if op.Status != model.OpPending {
			// Claimed by another worker, in progress, or terminal.
			return nil
		}

		targetState, restingStates := opstate.ClaimTransition(op.Type)
		if len(restingStates) > 0 {
			var m model.Machine
			// FOR UPDATE: row-lock the machine so its state/locked can't
			// change between this guard read and setMachineState below. A
			// concurrent lock API call could otherwise flip locked=true
			// after we read locked=false, and we'd claim+advance a machine
			// the operator just locked. Lock order is Operation → Machine
			// (op row was locked above), consistent across all txns here,
			// so no deadlock.
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("state", "locked").Where("id = ?", op.MachineID).
				First(&m).Error; err != nil {
				return fmt.Errorf("fetch machine for claim: %w", err)
			}
			if err := opstate.CheckClaimPreconditions(op.MachineID, op.Type, m.State, m.Locked, restingStates); err != nil {
				return err
			}
		}

		// Atomic compare-and-swap: only flip pending → claimed if the
		// status hasn't changed since the SELECT above. Without this,
		// READ COMMITTED isolation lets two workers both see the op as
		// pending, both UPDATE, and both believe they own the claim.
		now := time.Now()
		res := tx.Model(&op).
			Where("status = ?", model.OpPending).
			Updates(map[string]any{
				"claimed_by": workerID,
				"claimed_at": now,
				"status":     model.OpClaimed,
				"version":    gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Lost the race — another worker claimed it between our
			// SELECT and our UPDATE. Treat the same as "op not pending."
			return nil
		}

		if targetState != "" {
			// Route through the centralized helper so lock policy stays
			// in one place. Claim only ever targets transient states
			// (deploying/commissioning) today, which the helper leaves the
			// lock untouched for — but centralizing keeps it correct if a
			// claim ever targets running/failed.
			if err := setMachineState(tx, op.MachineID, targetState); err != nil {
				return err
			}
		}

		// Re-fetch so the returned op carries the updated columns.
		if err := tx.Where("id = ?", opID).First(&op).Error; err != nil {
			return err
		}
		// D6: emit the claim onto the logs stream in this same tx, so the TUI
		// learns claimed_by from the stream. After the re-fetch so ClaimedBy
		// is populated; only reached when the CAS above flipped a row.
		if err := emitClaimEvent(tx, &op, now); err != nil {
			return fmt.Errorf("emit claim event: %w", err)
		}
		claimed = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// setMachineState updates a machine's lifecycle state within tx, applying the
// column set (state + auto-lock reconciliation) that opstate.StateColumns
// decides for the target. This is the single write path for machine.state;
// the lock policy itself lives in opstate.StateColumns.
func setMachineState(tx *gorm.DB, machineID uuid.UUID, target model.MachineState) error {
	return tx.Model(&model.Machine{}).Where("id = ?", machineID).
		Updates(opstate.StateColumns(target)).Error
}

// CompleteOperation marks an operation as completed (idempotent).
// Does NOT transition machine state — callers that need that must
// go through CompleteOperationWithState or CompleteActiveOperation.
// No-op if the op is already terminal (completed OR failed); we
// don't revive failed ops.
func (s *Store) CompleteOperation(opID uuid.UUID, result []byte) error {
	now := time.Now()
	return s.db.Model(&model.Operation{}).
		Where("id = ? AND status NOT IN ?", opID, opstate.TerminalStatuses).
		Updates(map[string]any{
			"status":       model.OpCompleted,
			"completed_at": now,
			"result":       result,
			"version":      gorm.Expr("version + 1"),
		}).Error
}

// CompleteOperationWithState is CompleteOperation that also moves the
// associated machine into targetState. Atomic in a single transaction.
// Used by the hub's op_update handler to close a deploy (→ deployed)
// or any other op that knows its desired post-op state.
//
// If the op is already completed, no state transition happens (the
// machine row is not touched). If the op exists but the machine
// row doesn't (shouldn't happen — FK), returns the error.
//
// Returns the post-mutation OpOutcome (the completed op row with bumped
// Version + the durable MachineState it wrote) for the op-event
// publisher, or (nil, nil) when nothing changed (already terminal / lost
// race / op absent) so the caller publishes nothing. OpOutcome.MachineState
// is empty for a power-op completion (targetState "") — no identity
// rebuild fires for those.
func (s *Store) CompleteOperationWithState(
	opID uuid.UUID,
	targetState model.MachineState,
	result []byte,
) (*OpOutcome, error) {
	now := time.Now()
	var outcome *OpOutcome
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var op model.Operation
		// NOT IN (completed, failed): don't revive a terminal op.
		// A late-arriving op_update for a failure that was already
		// recorded must not silently flip the machine to success.
		err := tx.Where("id = ? AND status NOT IN ?", opID, opstate.TerminalStatuses).
			First(&op).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// Atomic compare-and-swap on the UPDATE, mirroring FailOperation:
		// the SELECT above is not enough under READ COMMITTED, where a
		// concurrent FailOperation can flip the op to failed between our
		// SELECT and UPDATE. Without the guarded UPDATE we'd revive that
		// failed op as completed AND (now that completion also drives the
		// lock) wrongly lock a machine that should be failed+unlocked.
		res := tx.Model(&op).
			Where("status NOT IN ?", opstate.TerminalStatuses).
			Updates(map[string]any{
				"status":       model.OpCompleted,
				"completed_at": now,
				"result":       result,
				"version":      gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Lost the race — op went terminal between SELECT and UPDATE.
			// The other writer is authoritative; don't touch machine state.
			return nil
		}
		// D6: emit the terminal envelope in this tx, before any state-change
		// early return, so completion rides the logs stream regardless of
		// whether this op type pulls machine state.
		if err := emitTerminalEvent(tx, &op, model.OpCompleted, "", now); err != nil {
			return fmt.Errorf("emit terminal event: %w", err)
		}
		// Empty targetState means "don't touch machine state" — used
		// by power-op completions where the op itself doesn't imply
		// a lifecycle transition.
		if targetState != "" {
			if err := setMachineState(tx, op.MachineID, targetState); err != nil {
				return err
			}
		}
		post, err := refetchOp(tx, opID)
		if err != nil {
			return err
		}
		outcome = &OpOutcome{Op: *post, MachineState: targetState}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// refetchOp reads the op row after a mutation so the returned OpOutcome
// carries the authoritative post-mutation row (bumped Version) rather
// than a hand-assembled one that could drift from the DB.
func refetchOp(tx *gorm.DB, opID uuid.UUID) (*model.Operation, error) {
	var op model.Operation
	if err := tx.Where("id = ?", opID).First(&op).Error; err != nil {
		return nil, err
	}
	return &op, nil
}

// CompleteActiveOperation finds the in-flight operation of the given
// type for the given machine, completes it, and transitions the
// machine state to the named target. Atomic in a transaction.
//
// "In-flight" means status in (pending, claimed, running). If none
// is found, returns nil without error (idempotent: a commission_report
// may arrive after the op already completed, e.g. via a retry).
//
// The schema's idx_one_active_op_per_machine unique index prevents
// two active ops from existing for the same machine, so we don't
// need to worry about ambiguous matches.
//
// Used by the commission handler to close the loop between
// commission_report success and machine state advancing to Ready.
func (s *Store) CompleteActiveOperation(
	machineID uuid.UUID,
	opType model.OperationType,
	targetState model.MachineState,
	result []byte,
) (*OpOutcome, error) {
	now := time.Now()
	var outcome *OpOutcome
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var op model.Operation
		err := tx.Where("machine_id = ? AND type = ? AND status IN ?",
			machineID, opType, opstate.ActiveStatuses).
			First(&op).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		// Guarded UPDATE (see CompleteOperationWithState): a concurrent
		// FailOperation must not be overwritten and revived as completed,
		// which would also wrongly drive the lock via setMachineState.
		res := tx.Model(&op).
			Where("status NOT IN ?", opstate.TerminalStatuses).
			Updates(map[string]any{
				"status":       model.OpCompleted,
				"completed_at": now,
				"result":       result,
				"version":      gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		// D6: terminal envelope on the logs stream, in this tx.
		if err := emitTerminalEvent(tx, &op, model.OpCompleted, "", now); err != nil {
			return fmt.Errorf("emit terminal event: %w", err)
		}
		if err := setMachineState(tx, machineID, targetState); err != nil {
			return err
		}
		post, err := refetchOp(tx, op.ID)
		if err != nil {
			return err
		}
		outcome = &OpOutcome{Op: *post, MachineState: targetState}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// PhaseTransition describes the result of a SetOperationPhase call.
// Caller emits a single "phase transition" log envelope when Changed
// is true; ignores when false (the worker emitted a duplicate phase
// event, or the op is already terminal, and the row didn't change).
type PhaseTransition struct {
	Changed   bool
	PrevPhase string
	NewPhase  string
	Duration  time.Duration
	MachineID uuid.UUID
	TraceID   uuid.UUID
	// Op is the post-mutation operation row (bumped Version) when
	// Changed is true, so the op-event publisher can send a phase
	// op_delta carrying the authoritative row rather than a
	// hand-assembled one. Nil when the phase was unchanged (dedup).
	Op *model.Operation
}

// SetOperationPhase updates the phase field of an operation and
// bumps phase_entered_at to now. Does NOT change status; used by the
// hub's op_update handler to route worker-emitted progress strings
// ("pxe_booting", "partitioning", etc.) into Postgres where slagctl
// watch picks them up.
//
// phase_entered_at is the deadline anchor for per-phase timeouts
// (currently PhoneHomeTimeout against phase=awaiting_phone_home).
// Bumping unconditionally — even when phase is unchanged — would
// make the deadline forever-renewable on noisy workers; instead we
// only bump when phase actually changes.
//
// A terminal (completed/failed) op is left as it is: the worker can
// still report a phase after the op ended (a report in flight when the
// operator aborted, or executor progress after op_removed), and the
// phase the op ended in, phase_entered_at and version belong to the
// terminal record. Moving the version would also publish an op_delta
// above the tombstone workers keep for the removed op.
//
// Returns the transition shape (prev_phase, duration in prev) so the
// caller can emit a "phase transition" log envelope.
func (s *Store) SetOperationPhase(opID uuid.UUID, phase string) (PhaseTransition, error) {
	var t PhaseTransition
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var op model.Operation
		// FOR UPDATE: the "did the phase change" decision and the
		// PrevPhase/Duration below come from this read, so it must see the
		// row the UPDATE writes. Under READ COMMITTED a plain SELECT lets
		// two overlapping deliveries of one report (outbox redelivery on a
		// new connection, an HA site-worker pair) both read the old phase
		// and both write: two Changed=true results, two op_deltas, two
		// envelopes, version +2. With the lock the second waits, then
		// reads the first's phase and returns Changed=false.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id", "phase", "phase_entered_at", "machine_id", "trace_id").
			Where("id = ?", opID).First(&op).Error; err != nil {
			return err
		}
		unchanged := PhaseTransition{Changed: false, NewPhase: phase, MachineID: op.MachineID, TraceID: op.TraceID}
		if op.Phase == phase {
			t = unchanged
			return nil
		}
		now := time.Now()
		// NOT IN (completed, failed): leave a terminal op as it is. The
		// guard sits on the UPDATE, not the read above, because the read
		// takes no lock and an abort or timeout fail can commit between
		// the two (the same reason CompleteOperationWithState guards its
		// UPDATE).
		res := tx.Model(&model.Operation{}).
			Where("id = ? AND status NOT IN ?", opID, opstate.TerminalStatuses).
			Updates(map[string]any{
				"phase":            phase,
				"phase_entered_at": now,
				"version":          gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			t = unchanged
			return nil
		}
		post, err := refetchOp(tx, opID)
		if err != nil {
			return err
		}
		t = PhaseTransition{
			Changed:   true,
			PrevPhase: op.Phase,
			NewPhase:  phase,
			Duration:  now.Sub(op.PhaseEnteredAt),
			MachineID: op.MachineID,
			TraceID:   op.TraceID,
			Op:        post,
		}
		return nil
	})
	return t, err
}

// FailOperation marks an operation as failed and updates the machine
// state per the per-op-status branching rules from the redesign spec
// (the decision lives in opstate.FailStateOutcome). No-op if the op is
// already terminal.
//
// Same logic applies whether the failure is operator-driven (abort),
// worker-reported, or timeout-swept. The timeout sweeps call into
// FailOperation directly.
func (s *Store) FailOperation(opID uuid.UUID, result []byte) error {
	_, err := s.failOperationGuarded(opID, result, nil)
	return err
}

// FailOperationOutcome is FailOperation returning the post-mutation
// OpOutcome for the op-event publisher: the failed op row (bumped
// Version) + the DURABLE machine state the fail wrote
// (failed/new), or "" when the fail changed no machine state (a pending
// op that never advanced, or a power op). Returns (nil, nil) when the
// fail was a no-op (op absent/terminal/lost race). See failOperationGuarded.
func (s *Store) FailOperationOutcome(opID uuid.UUID, result []byte) (*OpOutcome, error) {
	return s.failOperationGuarded(opID, result, nil)
}

// failOperationGuarded is FailOperation with an optional extra
// predicate on the row SELECT. A sweep that decided to fail an op
// based on a condition it read OUTSIDE this transaction (e.g.
// ExpireOrphanedClaims' claimed-no-phase check) passes the same
// condition as the guard; if the op moved on in the meantime (a
// re-claim recorded a phase), the SELECT misses and the fail is a
// clean no-op instead of killing a live op.
func (s *Store) failOperationGuarded(opID uuid.UUID, result []byte, guard func(*gorm.DB) *gorm.DB) (*OpOutcome, error) {
	now := time.Now()
	var outcome *OpOutcome
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var op model.Operation
		// FOR UPDATE row-locks the op for the duration of the tx. Without
		// it, a concurrent ClaimOperation can flip pending → claimed
		// between this SELECT and the CAS UPDATE below: the CAS still
		// succeeds (claimed is not terminal), but we'd act on the stale
		// wasPending=true and skip the machine state pull-down — while
		// claim already advanced the machine to a transient state, leaving
		// it stuck there with a failed op. The row lock serializes
		// FailOperation against ClaimOperation (which takes the same lock).
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status NOT IN ?", opID, opstate.TerminalStatuses)
		if guard != nil {
			q = guard(q)
		}
		if err := q.First(&op).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		wasPending := op.Status == model.OpPending
		// Atomic compare-and-swap: only flip to failed if status hasn't
		// changed since the SELECT. Closes the race where a concurrent
		// CompleteOperation or another FailOperation lands between our
		// SELECT and our UPDATE under READ COMMITTED.
		res := tx.Model(&op).
			Where("status NOT IN ?", opstate.TerminalStatuses).
			Updates(map[string]any{
				"status":       model.OpFailed,
				"completed_at": now,
				"result":       result,
				"version":      gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Lost the race — op went terminal between our SELECT and
			// our UPDATE. The other writer (likely a successful
			// completion) is authoritative; don't pull machine state.
			return nil
		}

		// D6: emit the terminal envelope in this tx, BEFORE the early returns
		// below. This must fire for every fail path — including a pending op
		// that fails before any worker claimed it (pending-expiry sweep), the
		// exact case the phase stream alone misses (no worker ran → no phase
		// envelope). The failure cause rides as result's text if present.
		if err := emitTerminalEvent(tx, &op, model.OpFailed, failResultMessage(result), now); err != nil {
			return fmt.Errorf("emit terminal event: %w", err)
		}

		// The failed op leaves the active set regardless of whether it
		// pulled machine state; refetch it (bumped Version) so the
		// publisher's op_removed carries the authoritative row.
		post, err := refetchOp(tx, opID)
		if err != nil {
			return err
		}

		// Rules 1 + 2: pending ops, power ops, and the out-of-band BMC ops
		// (set_boot_device, bmc_reconcile) never pull machine state. For rules 3 + 4 (commission-vs-other), the
		// initial-commission revert-to-new decision needs to know whether
		// the machine has ever been commissioned, so read commission_data
		// under the same lock order (Operation → Machine) before asking
		// opstate for the target.
		if wasPending || opstate.LeavesStateOnFail(op.Type) {
			// No durable machine-state write: op_removed carries no state,
			// and the publisher fires no identity rebuild.
			outcome = &OpOutcome{Op: *post}
			return nil
		}
		hasCommissionData := true
		if op.Type == model.OpCommission {
			var m model.Machine
			// FOR UPDATE keeps the machine-row locking uniform with the rest
			// of this txn and preserves the Operation → Machine lock order.
			// (The op-row lock above + one-active-op index already largely
			// foreclose a concurrent commission_data write, so this is
			// defense-in-depth more than a live race.)
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Select("commission_data").Where("id = ?", op.MachineID).
				First(&m).Error; err != nil {
				return err
			}
			hasCommissionData = len(m.CommissionData) > 0
		}
		failOut := opstate.FailStateOutcome(wasPending, op.Type, hasCommissionData)
		if !failOut.ChangeState {
			outcome = &OpOutcome{Op: *post}
			return nil
		}
		// setMachineState auto-unlocks on → failed: a failed machine is no
		// longer in service, so the operator retries without an unlock step.
		// (target = new for initial-commission failure leaves the lock as-is,
		// which is correct — a `new` machine was never locked.)
		if err := setMachineState(tx, op.MachineID, failOut.Target); err != nil {
			return err
		}
		outcome = &OpOutcome{Op: *post, MachineState: failOut.Target}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return outcome, nil
}

// failOpBatch routes each op through failOperationGuarded, continuing
// past per-op errors so one transient failure (deadlock retry, PG
// hiccup) doesn't strand the rest of a stuck set until the next sweep
// tick. A timeout sweep passes the predicate it selected ops with as
// guard, so each fail re-checks it under the op row lock and skips an
// op that moved on after the unlocked SELECT (a worker claimed it, its
// phase advanced). nil fails any op that is still non-terminal.
// Returns the post-mutation outcomes (one per op that actually
// transitioned) for the op-event publisher, and the joined errors.
func (s *Store) failOpBatch(ops []model.Operation, reason string, guard func(*gorm.DB) *gorm.DB) ([]OpOutcome, error) {
	// json.Marshal, not string concat: reason is a free-form param and
	// a quote/backslash in it would corrupt the result column's JSON.
	result, err := json.Marshal(map[string]string{"error": reason})
	if err != nil {
		return nil, err
	}
	var outcomes []OpOutcome
	var errs []error
	for _, op := range ops {
		out, err := s.failOperationGuarded(op.ID, result, guard)
		if err != nil {
			errs = append(errs, fmt.Errorf("op %s: %w", op.ID, err))
			continue
		}
		if out != nil {
			outcomes = append(outcomes, *out)
		}
	}
	return outcomes, errors.Join(errs...)
}

// ExpireOrphanedClaims fails operations that have sat in `claimed`
// with NO phase for longer than the grace window (B4). Phase is the
// first execution evidence a worker records (power_cycle lands seconds
// after a healthy claim), so claimed-no-phase past the grace means the
// claim reply was lost AND the claiming worker hasn't re-claimed —
// i.e. the worker died right after the claim committed. Failing fast
// surfaces it to the operator instead of stranding the op for the full
// stuck-op timeout (30 min).
//
// The healthy lost-reply case never gets here: the worker rolls its
// local mark back to pending and re-claims on the next executor tick
// (ClaimOperation's idempotent re-claim branch), putting a phase on
// the op long before the grace expires.
//
// The fail is guarded — the same claimed-no-phase predicate is
// re-checked inside FailOperation's transaction — so a re-claim that
// lands between this SELECT and the fail (worker reconnected at just
// the wrong moment and started actuating) makes the fail a no-op
// instead of killing a live op.
func (s *Store) ExpireOrphanedClaims(grace time.Duration) ([]OpOutcome, error) {
	cutoff := time.Now().Add(-grace)
	var orphans []model.Operation
	if err := s.db.Where("status = ? AND phase = '' AND claimed_at < ?",
		model.OpClaimed, cutoff).
		Find(&orphans).Error; err != nil {
		return nil, err
	}
	result := []byte(`{"error":"claiming worker disconnected before execution started"}`)
	guard := func(q *gorm.DB) *gorm.DB {
		return q.Where("status = ? AND phase = '' AND claimed_at < ?",
			model.OpClaimed, cutoff)
	}
	var outcomes []OpOutcome
	var errs []error
	for _, op := range orphans {
		out, err := s.failOperationGuarded(op.ID, result, guard)
		if err != nil {
			errs = append(errs, fmt.Errorf("op %s: %w", op.ID, err))
			continue
		}
		if out != nil {
			outcomes = append(outcomes, *out)
		}
	}
	return outcomes, errors.Join(errs...)
}

// ExpireStuckOperations finds operations stuck in claimed/running state
// past the timeout and routes each through FailOperation so the
// per-op-status branching (initial-commission revert, etc.) applies.
//
// Excludes ops in phase `awaiting_phone_home` — those have their own
// (longer) deadline anchored on phase_entered_at, swept by
// ExpirePhoneHomeOperations. Without this exclusion a deploy that
// hits awaiting_phone_home near the OpTimeout would fail-sweep
// before phone-home ever has a chance to land. The predicate is
// re-checked inside each fail (see failOpBatch), so an op that enters
// awaiting_phone_home after the SELECT is spared too.
//
// Returns the count of expired ops.
func (s *Store) ExpireStuckOperations(timeout time.Duration) ([]OpOutcome, error) {
	cutoff := time.Now().Add(-timeout)
	stuck := func(q *gorm.DB) *gorm.DB {
		return q.Where("status IN ? AND claimed_at < ? AND phase <> ?",
			[]model.OperationStatus{model.OpClaimed, model.OpRunning},
			cutoff, "awaiting_phone_home")
	}
	var stuckOps []model.Operation
	if err := stuck(s.db).Find(&stuckOps).Error; err != nil {
		return nil, err
	}
	return s.failOpBatch(stuckOps, "operation timed out", stuck)
}

// ExpirePendingOperations sweeps operations that have been pending
// (never claimed by a worker) past the cutoff and routes each through
// FailOperation. Pending ops have not advanced machine state, so
// FailOperation's rule 1 leaves machine state untouched — the machine
// stays in its prior resting state. The predicate is re-checked inside
// each fail (see failOpBatch), so an op a worker claims after the
// SELECT stays claimed instead of being failed under that worker.
//
// Default tier: 24h. The motivation is operator hygiene: if a worker
// is offline at op-create time and never picks it up, the op should
// eventually clear out of the active set so the next deploy can be
// scheduled (idx_one_active_op_per_machine).
func (s *Store) ExpirePendingOperations(timeout time.Duration) ([]OpOutcome, error) {
	cutoff := time.Now().Add(-timeout)
	pending := func(q *gorm.DB) *gorm.DB {
		return q.Where("status = ? AND created_at < ?", model.OpPending, cutoff)
	}
	var stuckOps []model.Operation
	if err := pending(s.db).Find(&stuckOps).Error; err != nil {
		return nil, err
	}
	return s.failOpBatch(stuckOps, "pending op timed out", pending)
}

// ExpirePhoneHomeOperations sweeps deploy operations stuck in phase
// `awaiting_phone_home` past the cutoff and routes each through
// FailOperation. The deadline is anchored on phase_entered_at, NOT
// claimed_at — once the agent reports deploy_complete, the rest of
// the operation is the host actually booting the installed OS, which
// is dominated by BIOS POST + bootloader + cloud-init time, not
// agent runtime.
//
// Default tier: 30 min. Tight enough to surface real "OS won't boot"
// failures fast; loose enough not to flap on slow hardware (POST +
// PXE chain to disk + systemd + cloud-init can run 8-15 minutes on
// older Supermicro/AMI hosts). The predicate is re-checked inside each
// fail (see failOpBatch).
func (s *Store) ExpirePhoneHomeOperations(timeout time.Duration) ([]OpOutcome, error) {
	cutoff := time.Now().Add(-timeout)
	waiting := func(q *gorm.DB) *gorm.DB {
		return q.Where("status IN ? AND phase = ? AND phase_entered_at < ?",
			[]model.OperationStatus{model.OpClaimed, model.OpRunning},
			"awaiting_phone_home", cutoff)
	}
	var stuckOps []model.Operation
	if err := waiting(s.db).Find(&stuckOps).Error; err != nil {
		return nil, err
	}
	return s.failOpBatch(stuckOps, "phone-home timed out", waiting)
}

// AbortMachine cancels any active operations on a machine. Each op is
// routed through FailOperation so the per-op-status branching applies
// (pending → no state change; initial-commission → revert to new;
// otherwise → failed). Power ops in flight are also failed but don't
// pull machine state.
//
// Returns the machine row after the abort AND the post-mutation
// outcomes (one per op that transitioned) for the op-event publisher.
// On error the outcomes still list every op failed before the error:
// those ops are terminal in the database and a retried abort no longer
// sees them, so the caller publishes them whatever the error.
func (s *Store) AbortMachine(machineID uuid.UUID) (*model.Machine, []OpOutcome, error) {
	var existing model.Machine
	if err := s.db.Where("id = ?", machineID).First(&existing).Error; err != nil {
		return nil, nil, fmt.Errorf("machine not found: %w", err)
	}
	var activeOps []model.Operation
	if err := s.db.Where("machine_id = ? AND status IN ?", machineID, opstate.ActiveStatuses).
		Find(&activeOps).Error; err != nil {
		return nil, nil, err
	}
	var outcomes []OpOutcome
	for _, op := range activeOps {
		out, err := s.failOperationGuarded(op.ID, []byte(`{"error":"aborted by operator"}`), nil)
		if err != nil {
			return nil, outcomes, fmt.Errorf("fail op %s during abort: %w", op.ID, err)
		}
		if out != nil {
			outcomes = append(outcomes, *out)
		}
	}
	var out model.Machine
	if err := s.db.Where("id = ?", machineID).First(&out).Error; err != nil {
		return nil, outcomes, err
	}
	return &out, outcomes, nil
}

// FailInFlightOpsForMachine fails every non-terminal op on the machine
// with the given reason (B7: external NetBox drift mid-operation blows
// up the world — an op created and actuated against data that just
// changed under it must not keep running). Pending ops are included:
// they were validated against the old data and would boot against the
// new.
//
// Returns the post-mutation outcomes (one per op that transitioned), like
// the expiry sweeps, so the mirror publishes an op_removed per op. Without
// that publish a worker holds a zombie ACTIVE op until reconnect — and
// under the fail-closed boot rule an active op is exactly what authorizes
// actuation. The mirror's identity rebuild fires separately (it already
// invalidates on every drift write).
func (s *Store) FailInFlightOpsForMachine(machineID uuid.UUID, reason string) ([]OpOutcome, error) {
	var active []model.Operation
	if err := s.db.Where("machine_id = ? AND status IN ?", machineID, opstate.ActiveStatuses).
		Find(&active).Error; err != nil {
		return nil, err
	}
	// No guard: "still active" is exactly the non-terminal check every
	// fail already makes under the op row lock.
	return s.failOpBatch(active, reason, nil)
}

// CreateMachineOp inserts a pending lifecycle op against a machine.
// Does NOT advance machine state — that happens at claim time, in
// ClaimOperation, when a worker picks up the op. The machine stays
// in its resting state until then.
//
// Validation order, deliberately allowed-source-first so operators
// get a clear "must commission first" error rather than the generic
// "machine has an active operation":
//  1. Machine exists / not locked / pxe_eligible (for lifecycle ops).
//  2. Allowed-source state check (caller passes the set).
//  3. No other in-flight op on this machine
//     (idx_one_active_op_per_machine).
//
// Note: the same allowed-source list is re-validated at claim time
// (see opstate.ClaimTransition + ClaimOperation). Validation here is the
// fast-fail for operator-initiated requests; the claim-time check
// closes the small race window where state could move between
// op-create and op-claim.
func (s *Store) CreateMachineOp(machineID uuid.UUID, opType model.OperationType, params json.RawMessage, allowedStates ...model.MachineState) (*model.Operation, error) {
	var op model.Operation
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var m model.Machine
		// FOR KEY SHARE is the lock the operation INSERT below takes through
		// fk_operations_machine anyway; taking it here instead means a
		// machine delete (DeleteMachineIfIdle holds the row FOR UPDATE)
		// cannot land between this read and the insert. The delete either
		// waits for this create and sees its op, or commits first and this
		// read reports the machine gone, instead of the checks below passing
		// against a doomed row and the insert failing the foreign key.
		if err := tx.Clauses(clause.Locking{Strength: "KEY SHARE"}).
			Select("state", "locked", "pxe_eligible").Where("id = ?", machineID).First(&m).Error; err != nil {
			return fmt.Errorf("machine not found: %w", err)
		}
		if m.Locked {
			return fmt.Errorf("machine is locked (typically because it is in service); run `slagctl machine unlock %s` first", machineID)
		}
		if !m.PXEEligible && (opType == model.OpCommission || opType == model.OpDeploy) {
			return fmt.Errorf("machine is not PXE-eligible; run `slagctl machine pxe-enable %s` (or pass --pxe-enable to commission/deploy)", machineID)
		}
		if len(allowedStates) > 0 {
			allowed := slices.Contains(allowedStates, m.State)
			if !allowed {
				return fmt.Errorf("machine is in state %q; %s requires one of %v", m.State, opType, allowedStates)
			}
		}
		var active model.Operation
		err := tx.Where("machine_id = ? AND status IN ?", machineID, opstate.ActiveStatuses).
			First(&active).Error
		if err == nil {
			return fmt.Errorf("machine already has an active %s operation; watch it with `slagctl operation watch %s` or cancel it with `slagctl machine abort %s`",
				active.Type, active.ID, machineID)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		op = model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			TraceID:   uuid.New(),
			Type:      opType,
			Params:    params,
		}
		return tx.Create(&op).Error
	})
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// CreatePowerOp creates a power operation without checking for active
// lifecycle operations. Power ops (on/off/cycle) must always be issuable
// even when a commission or deploy op is pending, so the operator can
// recover a stuck machine. No state guard; no machine state update.
func (s *Store) CreatePowerOp(machineID uuid.UUID, opType model.OperationType, params json.RawMessage) (*model.Operation, error) {
	var op model.Operation
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var active model.Operation
		err := tx.Where("machine_id = ? AND type IN ? AND status IN ?", machineID,
			[]model.OperationType{model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle},
			opstate.ActiveStatuses).
			First(&active).Error
		if err == nil {
			return fmt.Errorf("machine already has an active %s operation; watch it with `slagctl operation watch %s` or cancel it with `slagctl machine abort %s`",
				active.Type, active.ID, machineID)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		op = model.Operation{
			ID:        uuid.New(),
			MachineID: machineID,
			TraceID:   uuid.New(),
			Type:      opType,
			Params:    params,
		}
		return tx.Create(&op).Error
	})
	if err != nil {
		return nil, err
	}
	return &op, nil
}
