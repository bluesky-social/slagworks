// Package opstate holds the operation state machine's decision logic: which
// status/state transitions are legal, the preconditions and error taxonomy
// that gate a claim, the auto-lock policy that tracks in-service hosts, and
// the per-op-status branching that a failure applies to machine state.
//
// This is the WHAT of the state machine. The HOW — reading and writing op and
// machine rows, the row locks (FOR UPDATE), the compare-and-swap UPDATEs, and
// the transaction boundaries that make claim/complete/fail atomic — lives in
// internal/store. Store methods call the functions here inside their
// transactions; nothing in this package touches a database. The split keeps
// the state-machine invariants (documented in docs/dev/state-machine.md) in
// one reviewable place while leaving the concurrency-critical SQL where its
// atomicity is enforced.
//
// The taxonomy constants (TerminalStatuses, ActiveStatuses) are the canonical
// status sets the store's guards key on; they are operator-visible via slagctl
// and consumed by workers, so they are wire semantics, not implementation
// detail.
package opstate

import (
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TerminalStatuses is the set of op statuses that must never be written over.
// The store's CAS guards refuse to move a terminal op (a late worker report
// for a failure already recorded must not revive it as completed).
var TerminalStatuses = []model.OperationStatus{model.OpCompleted, model.OpFailed}

// ActiveStatuses is the "in flight" set. The schema's
// idx_one_active_op_per_machine allows at most one op in this set per machine;
// snapshot builds, abort, and drift-fail all scope to it.
var ActiveStatuses = []model.OperationStatus{model.OpPending, model.OpClaimed, model.OpRunning}

// IsPowerOp reports whether an op type is a power op. Power ops (on/off/cycle)
// never advance lifecycle machine state — they don't pull it forward on claim
// nor pull it down on failure.
func IsPowerOp(t model.OperationType) bool {
	return t == model.OpPowerOn || t == model.OpPowerOff || t == model.OpPowerCycle
}

// LeavesStateOnFail reports whether failing an op of this type must leave
// machine state (and lock) untouched: power ops and the out-of-band BMC
// ops — set_boot_device (arms a boot override) and bmc_reconcile
// (rewrites BMC user accounts). None of them is a lifecycle op, and they
// routinely target in-service (running, auto-locked) hosts; failing one
// must not flip such a host to failed and unlock it.
func LeavesStateOnFail(t model.OperationType) bool {
	return IsPowerOp(t) || t == model.OpSetBootDevice || t == model.OpBMCReconcile
}

// ClaimTransition returns (targetTransientState, allowedRestingStates) for an
// op type at claim time. Power ops return ("", nil) — they don't advance
// machine state.
//
// `running` is an allowed source for both lifecycle ops: deploy from running
// is a redeploy, commission from running is a re-inventory. Both are in the
// redesign spec's transition diagram. A running machine is auto-locked on
// entry (see StateColumns), so the lock gate — enforced at both create and
// claim — is what prevents an accidental wipe of an in-service host; the
// state check alone permits the transition.
//
// `commissioned` is also a valid commission source: re-running inventory on a
// host that commissioned but hasn't deployed yet (e.g. to pick up a
// commission-side fix like the cable reconcile) shouldn't require a
// deploy→running→unlock detour. `commissioned` is a resting, unlocked state,
// so no unlock dance. This set MUST match the allowedStates passed to
// CreateMachineOp in the commission API handler.
func ClaimTransition(opType model.OperationType) (model.MachineState, []model.MachineState) {
	switch opType {
	case model.OpCommission:
		return model.MachineStateCommissioning,
			[]model.MachineState{
				model.MachineStateNew,
				model.MachineStateCommissioned,
				model.MachineStateFailed,
				model.MachineStateRunning,
			}
	case model.OpDeploy:
		return model.MachineStateDeploying,
			[]model.MachineState{
				model.MachineStateCommissioned,
				model.MachineStateFailed,
				model.MachineStateRunning,
			}
	default:
		return "", nil
	}
}

// CanReclaim reports whether workerID may idempotently re-claim op (B4). The
// claim RPC is at-least-once: a worker whose claim COMMITTED but whose reply
// was lost on a WS drop retries on its next executor tick. If it already holds
// the claim and no execution progress has been recorded (phase lands only
// after actuation starts), the op is handed back so the deploy proceeds
// instead of stalling until a sweep. A different worker, or any recorded
// phase, is not a re-claim.
func CanReclaim(op *model.Operation, workerID string) bool {
	return op.Status == model.OpClaimed && op.ClaimedBy == workerID && op.Phase == ""
}

// CheckClaimPreconditions validates that a machine in the given state, with
// the given lock flag, may have opType claimed against it. restingStates is
// the allowed-source set from ClaimTransition; an empty set means the op type
// does not gate on machine state (power ops), so the check passes.
//
// This mirrors CreateMachineOp's create-time gate at claim time, closing the
// small race window where machine state or the lock could move between op
// create and op claim. The returned errors are operator-visible; their text
// is part of the CLI contract.
func CheckClaimPreconditions(machineID uuid.UUID, opType model.OperationType, state model.MachineState, locked bool, restingStates []model.MachineState) error {
	if len(restingStates) == 0 {
		return nil
	}
	if !slices.Contains(restingStates, state) {
		return fmt.Errorf("machine in state %q, cannot claim %s op (expected one of %v)",
			state, opType, restingStates)
	}
	// Lock check, mirroring CreateMachineOp's gate. The unique index largely
	// forecloses a stale-op race (at most one non-terminal op per machine),
	// but checking locked here keeps the create-time and claim-time gates
	// symmetric: a machine auto-locked on → running must not be claimable by
	// any op the operator didn't unlock for. Defense-in-depth, cheap.
	if locked {
		return fmt.Errorf("machine %s is locked; cannot claim %s op", machineID, opType)
	}
	return nil
}

// StateColumns returns the machine-row column updates a lifecycle transition
// to target implies, reconciling the auto-lock that guards in-service hosts.
// Centralizing the lock policy here keeps it in one place rather than spread
// across the state-mutating store sites (CompleteOperationWithState,
// CompleteActiveOperation, ClaimOperation, FailOperation):
//
//   - → running: lock. A running machine is an in-service host; it's
//     hands-off by default, so the lock gate in CreateMachineOp (and the
//     claim-time guard) rejects any new op until the operator runs
//     `slagctl machine unlock`. This is what makes a redeploy a deliberate
//     two-step act.
//   - → failed: unlock. The machine is no longer in service, so the operator
//     can retry (commission/deploy) without an unlock dance. The lock tracks
//     the running state; it is not a sticky operator bit. Known tradeoff
//     (deliberate): if an operator manually locks a host mid-op to quarantine
//     it, a subsequent → failed transition clears that lock too. We accept
//     this — the lock's defined meaning here is "in-service, hands off," not
//     "operator quarantine." If a sticky quarantine lock is ever wanted, it
//     needs a separate bit, not this one.
//   - any other target (new, commissioned, transient states): state only,
//     lock untouched.
func StateColumns(target model.MachineState) map[string]any {
	updates := map[string]any{"state": target}
	switch target {
	case model.MachineStateRunning:
		updates["locked"] = true
	case model.MachineStateFailed:
		updates["locked"] = false
	default:
		// new, commissioned, and the transient states (commissioning,
		// deploying) leave the lock untouched.
	}
	return updates
}

// FailOutcome is the machine-state effect of failing an op.
type FailOutcome struct {
	// ChangeState is false when the machine state must be left untouched (a
	// pending op that never advanced state, or a power op that never touches
	// lifecycle state).
	ChangeState bool
	// Target is the machine state to transition to when ChangeState is true.
	Target model.MachineState
}

// FailStateOutcome decides the machine-state effect of failing an op, per the
// four spec rules (docs/dev/state-machine.md, FailOperation branching). The
// rules are evaluated in order:
//
//  1. wasPending (op never claimed, so never advanced machine state) →
//     no change. Failing it must not pull state down.
//  2. power op, set_boot_device, or bmc_reconcile (LeavesStateOnFail) →
//     no change. These never advance lifecycle state.
//  3. commission op AND !hasCommissionData (initial-commission failure) →
//     revert to `new`, where the operator can retry without a `failed → new`
//     SQL flip.
//  4. otherwise → `failed`.
//
// The same logic applies whether the failure is operator-driven (abort),
// worker-reported, or timeout-swept.
func FailStateOutcome(wasPending bool, opType model.OperationType, hasCommissionData bool) FailOutcome {
	if wasPending {
		return FailOutcome{ChangeState: false}
	}
	if LeavesStateOnFail(opType) {
		return FailOutcome{ChangeState: false}
	}
	if opType == model.OpCommission && !hasCommissionData {
		return FailOutcome{ChangeState: true, Target: model.MachineStateNew}
	}
	return FailOutcome{ChangeState: true, Target: model.MachineStateFailed}
}
