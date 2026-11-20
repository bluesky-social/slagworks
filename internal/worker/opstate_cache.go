package worker

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
)

// opState is the mutable op-layer of the site cache. Post-Tier-2 it is fed
// by the op-event STREAM (op_delta / op_resync / op_removed), NOT by the
// snapshot: operations left SiteSnapshot. It owns under its OWN RWMutex
// (mu): the versioned ops map, the op-driven machineState overlay,
// failedOps, phaseStart, the tombstone set, the phone-home latch, and the
// untrusted flag — and, critically, the guard for the snapshotView pointer
// swap.
//
// The combined readers (BootActionForMAC and friends) need BOTH the
// immutable machine identity from the view AND the op/state overlay to be
// from one generation. To keep that pair tear-free without a shared Cache
// lock, opState holds a pointer to the same atomic.Pointer[snapshotView]
// that Cache publishes reads from: a combined read takes mu.RLock and loads
// the view INSIDE it, while an identity ApplySnapshot swaps the view under
// mu.Lock (replaceFromSnapshot). Because the swap and the overlay-clear
// happen under the same write lock a reader holds for read, a reader can
// never observe a new view paired with a stale overlay (or vice versa).
//
// Fields:
//   - ops: opID string -> *Operation, fed by the stream (versioned). A
//     delta with version <= the held version (or <= a tombstone) is
//     dropped (last-writer-wins). op_resync full-replaces; op_removed
//     drops + tombstones.
//   - machineState: op-driven machine-state overlay, authoritative over
//     the snapshot baseline when present. Set by claim/completion deltas
//     (transient/durable), by RecordPhoneHome (running), and by op_removed
//     (durable). Cleared on identity apply ONLY when the baseline caught
//     up (conditional clear — the re-wipe-window fix).
//   - failedOps: opID -> failed marker; blocks UpdateOpPhase after terminal
//     failure. Local, preserved across stream applies, and keeps the op's
//     Status failed against a delta or resync that still shows it active
//     (keepLocalFailure) until op_removed clears it.
//   - phaseStart: opID -> phase timing, for phase-duration logging. Local.
//   - phoneHomed: deploy op IDs that phone-homed LOCALLY (RecordPhoneHome).
//     A resync or delta that brings the op back as claimed-deploying must
//     not downgrade the running overlay this op earned — see applyOpResync
//     and applyOpDelta.
//     Persisted with the op set, so it survives a restart.
//   - phoneHomeSettled: deploy op ID -> machine ID for every deploy whose
//     phone-home is settled as far as this worker can prove: it recorded the
//     phone-home itself (recordPhoneHome, which queues the durable
//     observation), or global reported that exact op completed. It is the
//     only thing that lets validateReportBinding answer a phone-home retry
//     with success and no side effects, and unlike phoneHomed it outlives
//     the op's removal: a lost 2xx is retried after the op is gone. Bounded
//     FIFO (tombstoneCap) and persisted with the op set.
//   - tombstones: opID -> terminal version; a late lower-version delta for
//     a removed op is discarded so a concurrent-publisher race can't
//     resurrect a zombie active op (which the fail-closed boot rule would
//     read as authorization to actuate). Bounded FIFO (tombstoneCap).
//   - untrusted: true when the op set was rehydrated from LocalDB (stale by
//     construction — global may have failed/completed an op during the
//     outage). While untrusted the op set may force BootLocal (conservative)
//     but never actuate (BootDeploy/BootCommission) and the executor is
//     paused. The first op_resync clears it.
//   - resynced: true once an op_resync has been applied in this process, so
//     the op set is global's active set and not merely "nothing received
//     yet". An agent report for an op the set does not hold is refused as
//     final (model.ErrOperationNotActive, which stops the agent) only when
//     the worker knows the op is over: it holds the op's tombstone, or the
//     set is resynced. A worker started without a persisted op layer is
//     neither untrusted nor resynced until its first op_resync, and must
//     not tell a healthy deploy's agent to stop in that window.
//   - claimUnanswered: op IDs whose most recent claim this process sent
//     without learning the outcome (rollBackClaim: transport error, RPC
//     timeout). That claim may have COMMITTED on global, and global's row
//     for the op (claimed by this worker, no phase) can then reach the cache
//     after the rollback and replace its pending mark; reclaimableOps keeps
//     such an op in front of the executor so it re-claims it (B4). Cleared
//     when the next claim attempt starts (markClaimAttempt) and on
//     op_removed. Deliberately not pruned by op_resync: an op whose machine
//     briefly leaves the site's scope drops out of a resync and comes back
//     still claimed by this worker. An entry outlives its op only when the
//     worker misses that op's op_removed while disconnected; entries come
//     only from claims lost to transport failures, so the set stays small,
//     and a stale entry is harmless: it matches only its own op ID, which
//     global never reuses. In memory only, never persisted: after a restart
//     the worker cannot tell an unanswered claim from one it won and acted
//     on, so such an op is ExpireOrphanedClaims' to fail, not this worker's
//     to drive again.
type opState struct {
	mu sync.RWMutex

	view *atomic.Pointer[snapshotView]

	ops          map[string]*model.Operation
	machineState map[uuid.UUID]model.MachineState
	failedOps    map[string]bool
	phaseStart   map[string]phaseState
	phoneHomed   map[string]bool

	phoneHomeSettled      map[string]uuid.UUID
	phoneHomeSettledOrder []string

	claimUnanswered map[string]bool

	tombstones     map[string]int
	tombstoneOrder []string

	untrusted bool
	resynced  bool

	// wake is fired (buffered, non-blocking) when a delta/resync adds a
	// PENDING op, so the executor picks it up in ms instead of waiting on
	// its 5s backstop tick. nil in tests that don't wire an executor.
	wake chan struct{}

	// selfSerial is this worker's own verified hardware serial (SMBIOS,
	// P4a). Used by pendingPowerOps as the P4b worker-side defense-in-depth:
	// an outlet op whose DownstreamSerial matches selfSerial is skipped (the
	// worker would be cutting its own feed) UNLESS the op carries AllowSelf
	// for cycle/on. Redundant with the global claim-time interlock (INV-2),
	// but cheap insurance against a global bug or a lagged claimant-serial
	// view. "" (unverified) never matches a real downstream, so the skip is
	// inert for an unverified worker. selfSerial is the SMBIOS spelling and
	// DownstreamSerial is NetBox's; global verified one against the other
	// case-insensitively, so the match is model.SameHardwareSerial.
	// Immutable after construction.
	selfSerial string
}

// tombstoneCap bounds the tombstone set (opID -> terminal version). FIFO
// eviction by insertion order. Concurrent ops are dozens; 1024 is ample
// headroom while capping memory against an unbounded op history.
const tombstoneCap = 1024

type phaseState struct {
	phase   string
	started time.Time
}

// keepTerminalWait holds a locally reached awaiting_phone_home on a deploy
// op that global's incoming row still shows active at an earlier phase.
// Local phase writes don't bump Version, so global's echo of an EARLIER
// phase update (delayed on a degraded stream, or a resync built before the
// deploy_complete update reached global) carries a higher Version yet an
// older phase. Accepting it knocks the op off awaiting_phone_home, the
// post-install PXE gets BootDeploy, and the host re-deploys over the disk it
// just installed (leaf-5.pop1 2026-10-24). Only completion
// (op_removed) or failure may leave awaiting_phone_home — the same rule
// updatePhase enforces against local phase writes. A pending row is held
// too: the host can install on the pending-op boot clause while the claim
// keeps failing, and global never moves an op back to pending, so a pending
// row for a latched op is always older than the latch.
func keepTerminalWait(existing, incoming *model.Operation) {
	if existing == nil || existing.Phase != "awaiting_phone_home" {
		return
	}
	if incoming.Type != model.OpDeploy {
		return
	}
	if incoming.Status != model.OpPending && incoming.Status != model.OpClaimed &&
		incoming.Status != model.OpRunning {
		return
	}
	if incoming.Phase == "failed" {
		return
	}
	incoming.Phase = "awaiting_phone_home"
}

// keepLocalFailure holds a failure the agent reported (markFailed) on an op
// global's incoming row still shows active. markFailed writes Status=failed
// without a Version bump, and global applies the worker's failure op_updates
// one write at a time: each phase write publishes an op_delta whose row is
// still claimed, before the failure itself publishes op_removed. Those
// echoes, and a resync built before global processed the failure (or after
// the best-effort op_update was lost), carry a higher Version yet an active
// status. Accepting one revives the op: the boot decision actuates the host
// again while updatePhase, which failedOps still blocks, drops every phase
// the new agent run reports, so a deploy can never latch awaiting_phone_home
// and re-images the host on every boot. Only op_removed, which clears
// failedOps, ends a local failure; a completed or failed row from global
// still applies. Caller holds mu.
func (o *opState) keepLocalFailure(incoming *model.Operation) {
	if !o.failedOps[incoming.ID.String()] {
		return
	}
	if incoming.Status == model.OpCompleted || incoming.Status == model.OpFailed {
		return
	}
	incoming.Status = model.OpFailed
}

// keepPhoneHome holds the completion a local phone-home recorded
// (RecordPhoneHome: Status completed, overlay running, the phoneHomed latch)
// on an op global's incoming row still shows active, and reports whether it
// did, in which case the caller keeps the machine's overlay at running too.
// The completion is written without a Version bump, so a row global built
// before it processed the phone-home (the claim's delayed op_delta, or a
// resync) carries a higher Version, an active status and the claim's
// transient state. Accepting it undoes the phone-home: the machine reads
// deploying again, so the installed OS's retried phone-home is processed in
// full and queues a second completion, and with the op active again a late
// agent report can still fail a deploy that finished. A completed or failed
// row from global applies; op_removed clears the latch. Caller holds mu.
func (o *opState) keepPhoneHome(incoming *model.Operation) bool {
	if !o.phoneHomed[incoming.ID.String()] {
		return false
	}
	if incoming.Status == model.OpCompleted || incoming.Status == model.OpFailed {
		return false
	}
	incoming.Status = model.OpCompleted
	return true
}

// newOpState builds an empty op-layer whose combined reads load and whose
// identity applies swap through the given view pointer (Cache.view). wake
// is the executor wake channel (may be nil in tests). untrusted marks a
// rehydrated-from-LocalDB set that must not actuate until the first
// op_resync.
func newOpState(view *atomic.Pointer[snapshotView], wake chan struct{}, untrusted bool, selfSerial string) *opState {
	return &opState{
		view:         view,
		ops:          make(map[string]*model.Operation),
		machineState: make(map[uuid.UUID]model.MachineState),
		failedOps:    make(map[string]bool),
		phaseStart:   make(map[string]phaseState),
		phoneHomed:   make(map[string]bool),
		tombstones:   make(map[string]int),

		claimUnanswered:  make(map[string]bool),
		phoneHomeSettled: make(map[string]uuid.UUID),

		wake:       wake,
		untrusted:  untrusted,
		selfSerial: selfSerial,
	}
}

// fireWake signals the executor (buffered, non-blocking). Caller need not
// hold mu.
func (o *opState) fireWake() {
	if o.wake == nil {
		return
	}
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// --- Stream apply (op_delta / op_resync / op_removed) ---

// applyOpDelta applies one op_delta: last-writer-wins by Version. Drops the
// delta if the op is already held at an equal-or-higher version, or if a
// tombstone for this op has an equal-or-higher version (a late delta racing
// an op_removed must not resurrect a zombie active op). On accept, the op
// is stored and, when machineState is non-empty, the overlay is set to it.
// Returns true if a PENDING op was added/updated (caller fires the wake).
//
// Like applyOpResync, it preserves what a local phone-home earned
// (keepPhoneHome): a delta that still shows a phoneHomed op active carries
// a status and a state from before the phone-home. The usual one is the
// claim's own delta (claimed, deploying), arriving after a host that
// installed on the pending-op boot clause already phoned home. A delta that
// ends the op (completed or failed) is global's verdict and applies whole.
func (o *opState) applyOpDelta(op model.Operation, machineState model.MachineState) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := op.ID.String()

	if tv, ok := o.tombstones[id]; ok && op.Version <= tv {
		return false // removed op; stale delta cannot resurrect it
	}
	if existing, ok := o.ops[id]; ok && op.Version <= existing.Version {
		return false // last-writer-wins
	}

	cp := op
	o.keepLocalFailure(&cp)
	keepTerminalWait(o.ops[id], &cp)
	if o.keepPhoneHome(&cp) {
		machineState = model.MachineStateRunning
	}
	o.ops[id] = &cp
	if machineState != "" {
		o.machineState[op.MachineID] = machineState
	}
	return cp.Status == model.OpPending
}

// applyOpResync full-replaces the op set from a resync and rebuilds the
// machine-state overlay from the resync's derived states, then clears the
// untrusted flag. It PRESERVES a running overlay a local phone-home earned:
// if a deploy op that locally phone-homed (phoneHomed) comes back in the
// resync as claimed-deploying (global hasn't processed the completion yet),
// forcing the overlay back to deploying would re-wipe the freshly-installed
// disk. So for any active op in phoneHomed we pin the overlay to running,
// overriding the resync's transient state, and keep the op completed
// (keepPhoneHome). Entries in phoneHomed whose op is no longer active
// (global completed it) are pruned.
func (o *opState) applyOpResync(ops []model.Operation, states map[string]model.MachineState) {
	o.mu.Lock()
	defer o.mu.Unlock()

	next := make(map[string]*model.Operation, len(ops))
	anyPending := false
	for i := range ops {
		op := ops[i] // copy
		// Tombstone guard, belt-and-suspenders against any future
		// register/resync reorder: an op we already saw REMOVED at a
		// version >= this entry's version must not be resurrected by a
		// stale resync (a zombie active op is exactly what the fail-closed
		// boot rule reads as authorization to actuate). A HIGHER-version
		// entry is a genuine re-appearance — global is authoritative — and
		// applies normally.
		if tv, ok := o.tombstones[op.ID.String()]; ok && op.Version <= tv {
			continue
		}
		o.keepLocalFailure(&op)
		keepTerminalWait(o.ops[op.ID.String()], &op)
		o.keepPhoneHome(&op)
		next[op.ID.String()] = &op
		if op.Status == model.OpPending {
			anyPending = true
		}
	}
	o.ops = next

	// Rebuild the overlay from the resync's derived transient states.
	overlay := make(map[uuid.UUID]model.MachineState, len(states))
	for idStr, st := range states {
		if id, err := uuid.Parse(idStr); err == nil {
			overlay[id] = st
		}
	}
	// Preserve a local phone-home's running overlay against a resync that
	// still shows the op as active-deploying. Prune phoneHomed entries
	// whose op left the active set.
	for opID := range o.phoneHomed {
		op, ok := next[opID]
		if !ok {
			delete(o.phoneHomed, opID)
			continue
		}
		overlay[op.MachineID] = model.MachineStateRunning
	}
	o.machineState = overlay

	o.untrusted = false
	o.resynced = true
	// fireWake is a lock-free channel send, so calling it while mu is held
	// (before the deferred Unlock) is safe.
	if anyPending {
		o.fireWake()
	}
}

// applyOpRemoved drops a terminal op from the active set, records a
// tombstone (opID -> terminal version) so a late lower-version delta can't
// resurrect it, and — when the terminal transition wrote a durable machine
// state — moves the overlay to that value so it converges toward the
// baseline the accompanying identity rebuild delivers (instead of going
// stale at the last transient). Pending-expiry carries no state and touches
// no overlay.
func (o *opState) applyOpRemoved(opID, machineIDStr string, version int, machineState model.MachineState) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.putTombstone(opID, version)
	delete(o.ops, opID)
	delete(o.failedOps, opID)
	delete(o.phaseStart, opID)
	delete(o.phoneHomed, opID)
	delete(o.claimUnanswered, opID)

	if machineState != "" {
		if id, err := uuid.Parse(machineIDStr); err == nil {
			o.machineState[id] = machineState
			// Only a deploy's completion removes an op with running (a
			// failure never writes it, and no other op type completes to
			// it), so this is global's word that the op completed. The op
			// need not be held: a removal can overtake the op's delta.
			if machineState == model.MachineStateRunning {
				o.settlePhoneHome(opID, id)
			}
		}
	}
}

// putTombstone records opID -> version, evicting the oldest entry FIFO when
// at capacity. Caller holds mu. A repeat opID keeps the max version.
func (o *opState) putTombstone(opID string, version int) {
	if cur, ok := o.tombstones[opID]; ok {
		if version > cur {
			o.tombstones[opID] = version
		}
		return
	}
	if len(o.tombstoneOrder) >= tombstoneCap {
		oldest := o.tombstoneOrder[0]
		o.tombstoneOrder = o.tombstoneOrder[1:]
		delete(o.tombstones, oldest)
	}
	o.tombstones[opID] = version
	o.tombstoneOrder = append(o.tombstoneOrder, opID)
}

// settlePhoneHome records that the phone-home of deploy op opID on machineID
// is settled (see the phoneHomeSettled field), evicting the oldest entry
// FIFO when at capacity. Caller holds mu.
func (o *opState) settlePhoneHome(opID string, machineID uuid.UUID) {
	if _, ok := o.phoneHomeSettled[opID]; ok {
		return
	}
	if len(o.phoneHomeSettledOrder) >= tombstoneCap {
		oldest := o.phoneHomeSettledOrder[0]
		o.phoneHomeSettledOrder = o.phoneHomeSettledOrder[1:]
		delete(o.phoneHomeSettled, oldest)
	}
	o.phoneHomeSettled[opID] = machineID
	o.phoneHomeSettledOrder = append(o.phoneHomeSettledOrder, opID)
}

// snapshotForPersist returns copies of the op set, overlay and phone-home
// latch for LocalDB persistence. Caller must NOT hold mu.
func (o *opState) snapshotForPersist() PersistedOpState {
	o.mu.RLock()
	defer o.mu.RUnlock()
	ops := make([]model.Operation, 0, len(o.ops))
	for _, op := range o.ops {
		ops = append(ops, *op)
	}
	overlay := make(map[string]model.MachineState, len(o.machineState))
	for id, st := range o.machineState {
		overlay[id.String()] = st
	}
	phoneHomed := make([]string, 0, len(o.phoneHomed))
	for opID := range o.phoneHomed {
		phoneHomed = append(phoneHomed, opID)
	}
	settled := make([]SettledPhoneHome, 0, len(o.phoneHomeSettledOrder))
	for _, opID := range o.phoneHomeSettledOrder {
		settled = append(settled, SettledPhoneHome{OpID: opID, MachineID: o.phoneHomeSettled[opID]})
	}
	return PersistedOpState{Ops: ops, Overlay: overlay, PhoneHomed: phoneHomed, PhoneHomeSettled: settled}
}

// rehydrate loads a persisted op layer into an untrusted opState on cold
// start. Caller holds no other lock. The set stays untrusted until the
// first op_resync, which applies the restored awaiting_phone_home phases
// (keepTerminalWait) and phoneHomed pins exactly as it would have before
// the restart.
func (o *opState) rehydrate(st PersistedOpState) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := range st.Ops {
		op := st.Ops[i]
		o.ops[op.ID.String()] = &op
	}
	for idStr, ms := range st.Overlay {
		if id, err := uuid.Parse(idStr); err == nil {
			o.machineState[id] = ms
		}
	}
	for _, opID := range st.PhoneHomed {
		o.phoneHomed[opID] = true
	}
	for _, e := range st.PhoneHomeSettled {
		o.settlePhoneHome(e.OpID, e.MachineID)
	}
	o.untrusted = true
}

// replaceFromSnapshot swaps the view pointer for a newly-applied identity
// snapshot and CONDITIONALLY clears the machine-state overlay: overlay[id]
// is cleared ONLY where the applied baseline State for that machine EQUALS
// the overlay value (global's durable state has provably caught up). This
// replaces the old unconditional clear and closes the re-wipe window — a
// running overlay set by phone-home persists until the identity snapshot
// carries baseline=running, so there is never a window where
// `baseline=deploying + no active op` is visible. The op set is untouched
// (it is fed by the stream, not the snapshot). failedOps/phaseStart are
// preserved. Holding mu.Lock across the view.Store keeps combined readers
// from observing a torn (view, overlay) pair.
func (o *opState) replaceFromSnapshot(v *snapshotView) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.view.Store(v)
	for id, overlayState := range o.machineState {
		m, ok := v.machines[id]
		if !ok {
			continue
		}
		if m.State == overlayState {
			delete(o.machineState, id)
			// The durable baseline caught up; any phone-home latch for
			// this machine's ops is moot. Drop latches for its ops so the
			// map stays bounded.
			for opID, op := range o.ops {
				if op.MachineID == id {
					delete(o.phoneHomed, opID)
				}
			}
		}
	}
}

// effectiveState returns the machine's state with any op-driven overlay
// applied. Caller must hold mu (read or write).
func (o *opState) effectiveState(id uuid.UUID, base model.MachineState) model.MachineState {
	if s, ok := o.machineState[id]; ok {
		return s
	}
	return base
}

// --- Combined readers (view + op overlay, one generation under mu) ---

// bootAction is the internal result of the boot-action decision, carrying
// an optional fail-closed warning to emit AFTER releasing the lock (slow
// slog IO must not block a pending writer under RLock).
type bootAction struct {
	action model.BootAction
	warn   bool
	mac    string
	state  model.MachineState
}

// bootActionForMAC returns the boot action for a MAC. See the doc on
// Cache.BootActionForMAC for the decision table and the two re-wipe-guard
// clauses, plus the Tier-2 fail-closed + untrusted rules.
func (o *opState) bootActionForMAC(mac string) model.BootAction {
	r := o.bootActionLocked(mac)
	if r.warn {
		slog.Warn("boot fail-closed: transient machine state with no active op (op/identity mismatch); serving BootLocal — resync or next op_delta heals it",
			"mac", r.mac, "effective_state", string(r.state))
	}
	return r.action
}

func (o *opState) bootActionLocked(mac string) bootAction {
	o.mu.RLock()
	defer o.mu.RUnlock()
	v := o.view.Load()

	m, ok := v.machineByMAC(mac)
	if !ok {
		return bootAction{action: model.BootLocal}
	}
	if !m.PXEEligible {
		return bootAction{action: model.BootLocal}
	}

	activeOp := o.activeLifecycleOpLocked(m.ID)
	state := o.effectiveState(m.ID, m.State)

	// The install is done and the host is rebooting into its fresh disk, in
	// every machine state, not only deploying: a host can PXE on the
	// pending-op clause below and finish the install before the claim's
	// op_delta moves the machine off commissioned/failed. Serving the
	// installer here would run it over the disk it just wrote.
	if activeOp != nil && activeOp.Type == model.OpDeploy && activeOp.Phase == "awaiting_phone_home" {
		return bootAction{action: model.BootLocal}
	}

	switch state {
	case model.MachineStateCommissioning:
		// Fail-closed: a transient commissioning state must be backed by
		// a matching active commission op in opState — the op layer is the
		// sole authority for lifecycle actuation. No matching op → the
		// baseline alone must never actuate (a dropped/late delta could
		// leave a stale transient baseline via a kept identity trigger).
		if activeOp != nil && activeOp.Type == model.OpCommission {
			if o.untrusted {
				return bootAction{action: model.BootLocal}
			}
			return bootAction{action: model.BootCommission}
		}
		return bootAction{action: model.BootLocal, warn: true, mac: mac, state: state}
	case model.MachineStateDeploying:
		if activeOp != nil && activeOp.Type == model.OpDeploy {
			if o.untrusted {
				return bootAction{action: model.BootLocal}
			}
			return bootAction{action: model.BootDeploy}
		}
		return bootAction{action: model.BootLocal, warn: true, mac: mac, state: state}
	case model.MachineStateNew:
		// Commission is destructive-adjacent inventory authority, so even a
		// new machine must have an explicit operation before it receives the
		// agent. This guarantees every boot carries one machine/op binding.
		if activeOp != nil && activeOp.Type == model.OpCommission && !o.untrusted {
			return bootAction{action: model.BootCommission}
		}
		return bootAction{action: model.BootLocal}
	case model.MachineStateFailed:
		// Failed retries require an explicit op (operator chose commission
		// or deploy as the recovery path).
		if activeOp != nil && activeOp.Type == model.OpCommission {
			if o.untrusted {
				return bootAction{action: model.BootLocal}
			}
			return bootAction{action: model.BootCommission}
		}
		if activeOp != nil && activeOp.Type == model.OpDeploy {
			if o.untrusted {
				return bootAction{action: model.BootLocal}
			}
			return bootAction{action: model.BootDeploy}
		}
		return bootAction{action: model.BootLocal}
	case model.MachineStateCommissioned:
		if activeOp != nil && activeOp.Type == model.OpDeploy {
			if o.untrusted {
				return bootAction{action: model.BootLocal}
			}
			return bootAction{action: model.BootDeploy}
		}
		return bootAction{action: model.BootLocal}
	case model.MachineStateRunning:
		// Redeploy-from-running is out of scope; running machines always
		// boot from disk for now.
		return bootAction{action: model.BootLocal}
	default:
		return bootAction{action: model.BootLocal}
	}
}

// activeLifecycleOpLocked returns the first active (not terminal)
// deploy/commission op for a machine, or nil. Caller holds mu.
func (o *opState) activeLifecycleOpLocked(machineID uuid.UUID) *model.Operation {
	for _, op := range o.ops {
		if op.MachineID != machineID {
			continue
		}
		if op.Status == model.OpCompleted || op.Status == model.OpFailed {
			continue
		}
		if op.Type != model.OpCommission && op.Type != model.OpDeploy {
			continue // ignore power ops
		}
		return op
	}
	return nil
}

// machineByID returns a copy of the machine snapshot by ID with the
// op-driven state overlay applied, or nil. Combined reader.
func (o *opState) machineByID(id uuid.UUID) *model.MachineSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if m, ok := o.view.Load().machines[id]; ok {
		cp := cloneMachineSnapshot(m)
		cp.State = o.effectiveState(id, m.State)
		return cp
	}
	return nil
}

// machineByMAC returns a copy of the machine snapshot for a MAC with the
// op-driven state overlay applied, or nil. Combined reader.
func (o *opState) machineByMAC(mac string) *model.MachineSnapshot {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if m, ok := o.view.Load().machineByMAC(mac); ok {
		cp := cloneMachineSnapshot(m)
		cp.State = o.effectiveState(m.ID, m.State)
		return cp
	}
	return nil
}

// opNotActiveError is a report rejection that is final: the operation is
// over. It matches model.ErrOperationNotActive under errors.Is, which is
// what the report endpoint maps to 410 Gone.
type opNotActiveError string

func (e opNotActiveError) Error() string { return string(e) }

func (e opNotActiveError) Is(target error) bool { return target == model.ErrOperationNotActive }

// validateReportBinding resolves the exact machine/op pair carried by an
// agent report. Hardware discovered by the agent is never allowed to select a
// different machine: the op stream and identity snapshot must agree first.
// The bool reports an already-acknowledged phone-home retry, which callers
// return as success without repeating its outbox or state side effects. It
// is true only for a deploy op whose phone-home is settled: this worker
// recorded it, or global reported that exact op completed.
//
// A rejection wraps model.ErrOperationNotActive only when the op is known to
// be over: it is held with a terminal status, its tombstone is held, or it is
// absent from a resynced set. That answer stops the agent, so every rejection
// the worker cannot stand behind as final (an unsynchronized op layer, a
// machine missing from the snapshot, a machine/op or event/type mismatch)
// stays a plain error.
func (o *opState) validateReportBinding(machineID, opID uuid.UUID, event string) (*model.MachineSnapshot, *model.Operation, bool, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.untrusted {
		return nil, nil, false, fmt.Errorf("operation state is not synchronized")
	}
	m, ok := o.view.Load().machines[machineID]
	if !ok {
		return nil, nil, false, fmt.Errorf("machine %s is not in the site snapshot", machineID)
	}
	// The installed OS retries phone-home every few seconds until its first
	// 2xx and never again, so a 2xx must mean the deploy's completion is
	// already on its way to global or already there. A retry of a lost 2xx
	// can arrive after the op was completed and removed, hence the settled
	// set and not the op set. The machine being held as running is no such
	// evidence: a worker whose view predates a redeploy holds the machine
	// running from the previous deploy.
	//
	// Everything else takes the checks below and is refused unless the op is
	// held active at awaiting_phone_home. A refused phone-home is retried,
	// and passes once this worker's op view catches up. Forwarding it to
	// global instead was rejected: global completes any active deploy op a
	// phone_home observation names, so the awaiting_phone_home gate exists
	// only here, and a worker that does not hold the op cannot apply it.
	if event == "phone_home" {
		if settledFor, ok := o.phoneHomeSettled[opID.String()]; ok && settledFor == machineID {
			mcp := cloneMachineSnapshot(m)
			mcp.State = o.effectiveState(machineID, m.State)
			return mcp, nil, true, nil
		}
	}
	op, ok := o.ops[opID.String()]
	if !ok {
		if _, removed := o.tombstones[opID.String()]; !removed && !o.resynced {
			return nil, nil, false, fmt.Errorf("operation %s is not known on this site yet: no operation resync since the worker started", opID)
		}
		return nil, nil, false, opNotActiveError(fmt.Sprintf("operation %s is not active on this site", opID))
	}
	if op.Status == model.OpCompleted || op.Status == model.OpFailed {
		return nil, nil, false, opNotActiveError(fmt.Sprintf("operation %s is terminal (%s)", opID, op.Status))
	}
	if op.MachineID != machineID {
		return nil, nil, false, fmt.Errorf("operation %s belongs to machine %s, not %s", opID, op.MachineID, machineID)
	}
	if op.Type != model.OpDeploy && op.Type != model.OpCommission {
		return nil, nil, false, fmt.Errorf("operation %s has non-lifecycle type %s", opID, op.Type)
	}
	wantType := model.OperationType("")
	switch event {
	case "commission_report":
		wantType = model.OpCommission
	case "deploy_complete", "phone_home":
		wantType = model.OpDeploy
	}
	if wantType != "" && op.Type != wantType {
		return nil, nil, false, fmt.Errorf("event %q requires %s operation, got %s", event, wantType, op.Type)
	}
	// A phone-home is the installed OS reporting that it booted, and it
	// completes the deploy (RecordPhoneHome). It is valid only once the
	// agent's deploy_complete has latched awaiting_phone_home, which is
	// also the only phase that hands the host BootLocal; accepting it
	// earlier records a deploy as successful over a disk the agent has not
	// finished (or started) writing. The op may still be pending: a host
	// can install on the pending-op boot clause before any claim lands.
	if event == "phone_home" && op.Phase != "awaiting_phone_home" {
		return nil, nil, false, fmt.Errorf("deploy operation %s is at phase %q, not awaiting_phone_home: a phone-home is accepted only after the agent reports deploy_complete", opID, op.Phase)
	}
	mcp, ocp := cloneMachineSnapshot(m), *op
	mcp.State = o.effectiveState(machineID, m.State)
	return mcp, &ocp, false, nil
}

// stateString returns the machine's current effective state as a string,
// or "" if the machine isn't in the view. Combined reader.
func (o *opState) stateString(id uuid.UUID) string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if m, ok := o.view.Load().machines[id]; ok {
		return string(o.effectiveState(id, m.State))
	}
	return ""
}

// deployOpForMAC returns a copy of the active deploy op for a MAC, or nil.
// Combined reader.
func (o *opState) deployOpForMAC(mac string) *model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	m, ok := o.view.Load().machineByMAC(mac)
	if !ok {
		return nil
	}
	for _, op := range o.ops {
		if op.MachineID == m.ID && op.Type == model.OpDeploy &&
			op.Status != model.OpCompleted && op.Status != model.OpFailed {
			cp := *op
			return &cp
		}
	}
	return nil
}

// commissionOpForMAC returns a copy of the active commission op for a MAC,
// or nil. Combined reader.
func (o *opState) commissionOpForMAC(mac string) *model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	m, ok := o.view.Load().machineByMAC(mac)
	if !ok {
		return nil
	}
	for _, op := range o.ops {
		if op.MachineID == m.ID && op.Type == model.OpCommission &&
			op.Status != model.OpCompleted && op.Status != model.OpFailed {
			cp := *op
			return &cp
		}
	}
	return nil
}

// recordPhoneHome sets the running overlay for a machine (if it exists in
// the view), latches the deploy op as locally phone-homed, and returns the
// active deploy op ID to complete, or uuid.Nil if none. Combined
// reader/writer: reads the view and writes the overlay + scans ops under
// one mu.Lock.
func (o *opState) recordPhoneHome(machineID uuid.UUID) uuid.UUID {
	o.mu.Lock()
	defer o.mu.Unlock()
	// Record the local machine-state advance as an overlay rather than
	// mutating the (immutable) view: the machine stays `running` locally
	// until the identity snapshot carries baseline=running, at which point
	// the conditional clear drops the overlay. This is what keeps
	// BootActionForMAC returning BootLocal on the post-install reboot.
	if _, ok := o.view.Load().machines[machineID]; ok {
		o.machineState[machineID] = model.MachineStateRunning
	}
	for _, op := range o.ops {
		if op.MachineID == machineID && op.Type == model.OpDeploy &&
			op.Status != model.OpCompleted && op.Status != model.OpFailed {
			// Latch this op as locally phone-homed: a reconnect op_resync
			// that brings it back as claimed-deploying must not downgrade
			// the running overlay and re-wipe the fresh disk.
			o.phoneHomed[op.ID.String()] = true
			o.settlePhoneHome(op.ID.String(), machineID)
			return op.ID
		}
	}
	return uuid.Nil
}

// --- Op-set readers (ops only, under mu.RLock) ---

// pendingPowerOps returns copies of pending power-plane ops. Returns nil
// while untrusted (rehydrated set may be stale; executor is paused until
// the first op_resync).
//
// View-membership filter: the hub slug-scopes op_delta/op_resync (the
// confidentiality gate lives there), but this worker-side gate stays as
// defense in depth — and it covers the residual skew where the hub's
// snapshot knows a machine this worker's APPLIED view doesn't yet. The
// executor must never claim an op whose machine isn't in the view — it
// would win the claim, find the machine absent locally (MachineByID
// nil), and fail a live op. Filtering happens HERE at the read site,
// deliberately NOT at apply time: a delta can arrive before the identity
// snapshot that adds its machine, and dropping it at apply would strand
// the op until reconnect. Storing not-yet-in-view ops is cheap (the
// active set is dozens); they stay invisible to the executor until the
// view knows the machine.
func (o *opState) pendingPowerOps() []model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.untrusted {
		return nil
	}
	v := o.view.Load()
	var ops []model.Operation
	for _, op := range o.ops {
		if op.Status != model.OpPending {
			continue
		}
		if _, ok := v.machines[op.MachineID]; !ok {
			continue // out-of-view: another site's op, not ours to drive
		}
		switch op.Type {
		case model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle, model.OpSetBootDevice, model.OpBMCReconcile:
			// set_boot_device and bmc_reconcile are BMC-plane ops: executeOp
			// drives them directly against the BMC (no PXE boot, no agent),
			// so they belong in this bucket, not lifecycle.
			//
			// P4b worker-side defense-in-depth: never serve an outlet op that
			// would cut THIS worker's own feed. Global's claim-time interlock
			// (INV-2) already declines it, but a global bug or a lagged
			// claimant-serial view could let it through — so skip a self-serial
			// outlet op here too. The exception mirrors INV-2 exactly: an
			// AllowSelf cycle/on IS served (the operator chose the self-cycle);
			// a self-serial off is ALWAYS skipped (INV-1). Only outlet ops
			// (PDURuntime.Outlet) carry a downstream serial to compare — a
			// machine BMC/PDU power op has none and is unaffected.
			if o.skipSelfOutletOp(*op) {
				continue
			}
			ops = append(ops, *op)
		default:
			// Lifecycle ops (commission, deploy) are returned by
			// pendingLifecycleOps. Any op type that matches neither getter is
			// orphaned forever — TestPendingOps_EveryOpTypeIsBucketed guards
			// against a new type silently falling through this crack.
		}
	}
	return ops
}

// skipSelfOutletOp reports whether an outlet op should be skipped by the
// worker-side self-power interlock: its DownstreamSerial matches this
// worker's selfSerial ignoring case (it feeds this very worker). The
// AllowSelf exception applies ONLY to cycle/on; a self-serial off is always
// skipped (INV-1). Non-outlet ops
// (PDURuntime absent or Outlet==false) and a worker with no verified serial
// ("" never matches) are never skipped. Caller holds mu (read is fine).
func (o *opState) skipSelfOutletOp(op model.Operation) bool {
	if o.selfSerial == "" {
		return false
	}
	params, err := op.DecodeParams()
	if err != nil {
		// A power op whose params won't decode isn't a well-formed outlet op;
		// let the executor's own claim path (and the global interlock) handle
		// it rather than silently swallowing it here.
		return false
	}
	rt := params.PDURuntime
	if rt == nil || !rt.Outlet {
		return false // not an outlet op: outside the worker-side interlock
	}
	if !model.SameHardwareSerial(rt.DownstreamSerial, o.selfSerial) {
		return false // feeds some other device
	}
	// Feeds this worker. Serve only an explicit AllowSelf cycle/on; skip off
	// and any non-override self-feed.
	if rt.AllowSelf && (op.Type == model.OpPowerCycle || op.Type == model.OpPowerOn) {
		return false
	}
	return true
}

// pendingLifecycleOps returns copies of pending lifecycle ops (commission,
// deploy) the executor should drive via PXE boot. Returns nil while
// untrusted (executor paused until the first op_resync). Same
// view-membership filter as pendingPowerOps — see the doc there.
func (o *opState) pendingLifecycleOps() []model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.untrusted {
		return nil
	}
	v := o.view.Load()
	var ops []model.Operation
	for _, op := range o.ops {
		if op.Status != model.OpPending {
			continue
		}
		if _, ok := v.machines[op.MachineID]; !ok {
			continue // out-of-view: another site's op, not ours to drive
		}
		switch op.Type {
		case model.OpCommission, model.OpDeploy:
			ops = append(ops, *op)
		default:
			// Power-plane ops (power_on/off/cycle, set_boot_device) are
			// returned by pendingPowerOps. See the bucketing guard test.
		}
	}
	return ops
}

// reclaimableOps returns copies of the ops whose most recent claim went
// unanswered (claimUnanswered) and whose row global has since delivered as
// claimed by workerID with no phase (opstate.CanReclaim): the claim committed
// and only its reply was lost. The executor re-claims them, which
// ClaimOperation's idempotent re-claim branch grants (B4). While the
// rollback's pending mark still stands the op is pending and the pending
// readers return it instead. Same untrusted and view-membership gates as
// pendingPowerOps. The executor sent the lost claim for an op a pending
// reader returned, so the op already passed their type bucketing and
// self-outlet skip.
func (o *opState) reclaimableOps(workerID string) []model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.untrusted {
		return nil
	}
	v := o.view.Load()
	var ops []model.Operation
	for opID := range o.claimUnanswered {
		op, ok := o.ops[opID]
		if !ok || !opstate.CanReclaim(op, workerID) {
			continue
		}
		if _, ok := v.machines[op.MachineID]; !ok {
			continue
		}
		ops = append(ops, *op)
	}
	return ops
}

// activeOpCount returns the number of non-terminal operations.
func (o *opState) activeOpCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	n := 0
	for _, op := range o.ops {
		switch op.Status {
		case model.OpPending, model.OpClaimed, model.OpRunning:
			n++
		case model.OpCompleted, model.OpFailed:
			// terminal — not active
		}
	}
	return n
}

// operationCount returns the number of cached operations.
func (o *opState) operationCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.ops)
}

// getOperation returns a copy of the op with the given ID, or nil.
func (o *opState) getOperation(id string) *model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if op, ok := o.ops[id]; ok {
		cp := *op
		return &cp
	}
	return nil
}

// traceIDForOp returns the op's TraceID, or the zero UUID if not present.
func (o *opState) traceIDForOp(opID string) uuid.UUID {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if op, ok := o.ops[opID]; ok {
		return op.TraceID
	}
	return uuid.UUID{}
}

// isOpFailed reports whether an op has been marked failed.
func (o *opState) isOpFailed(opID string) bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.failedOps[opID]
}

// activeDeployOpForMachine returns a copy of the active deploy op for a
// machine, or nil.
func (o *opState) activeDeployOpForMachine(machineID uuid.UUID) *model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, op := range o.ops {
		if op.MachineID == machineID && op.Type == model.OpDeploy &&
			op.Status != model.OpCompleted && op.Status != model.OpFailed {
			cp := *op
			return &cp
		}
	}
	return nil
}

// activeBootingOpForMachine returns a copy of the active deploy-or-commission
// op for a machine, or nil.
func (o *opState) activeBootingOpForMachine(machineID uuid.UUID) *model.Operation {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for _, op := range o.ops {
		if op.MachineID != machineID {
			continue
		}
		if op.Type != model.OpDeploy && op.Type != model.OpCommission {
			continue
		}
		if op.Status == model.OpCompleted || op.Status == model.OpFailed {
			continue
		}
		cp := *op
		return &cp
	}
	return nil
}

// --- Op mutators (write under mu.Lock) ---

// setStatus sets an op's status in-place (no-op if the op is absent).
func (o *opState) setStatus(opID string, status model.OperationStatus) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if op, ok := o.ops[opID]; ok {
		op.Status = status
	}
}

// markClaimAttempt marks an op claimed locally as the executor sends a claim
// for it, so the pending readers stop returning it, and forgets any earlier
// unanswered claim: this attempt's outcome now decides, and rollBackClaim
// records it again if it goes unanswered too.
func (o *opState) markClaimAttempt(opID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.claimUnanswered, opID)
	if op, ok := o.ops[opID]; ok {
		op.Status = model.OpClaimed
	}
}

// rollBackClaim handles a claim that got no answer: it sets the local mark
// back to pending so the pending readers return the op for a retry, and
// records the claim as unanswered so reclaimableOps returns the op if
// global's claimed row replaces that mark first. A no-op for an op that
// ended while the claim was in flight (op_removed left its tombstone). An op
// a resync merely omitted meanwhile (its machine left the site's scope) has
// no row to roll back but keeps the record, for when the row comes back.
func (o *opState) rollBackClaim(opID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ended := o.tombstones[opID]; ended {
		return
	}
	o.claimUnanswered[opID] = true
	if op, ok := o.ops[opID]; ok {
		op.Status = model.OpPending
	}
}

// markFailed marks an op failed, clears its phase timing, and (if present)
// sets its in-memory Status/Phase. Once marked failed, updatePhase for this
// op is rejected — this prevents phase progression after terminal failure —
// and stream applies keep it failed until op_removed (keepLocalFailure).
func (o *opState) markFailed(opID, phase string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failedOps[opID] = true
	delete(o.phaseStart, opID)
	if op, ok := o.ops[opID]; ok {
		op.Status = model.OpFailed
		op.Phase = phase
	}
}

// phaseUpdate carries the result of updatePhase: whether the caller should
// emit the phase log + outbox update, and the fields needed to build the
// structured envelope with the SAME timestamp used for the phase-start
// bookkeeping.
type phaseUpdate struct {
	proceed     bool
	now         time.Time
	prev        phaseState
	hasPrev     bool
	machineID   string // op.MachineID.String(); "" if the op is absent
	machineUUID uuid.UUID
	traceID     uuid.UUID
}

// updatePhase advances an op's phase under mu, applying the failed-op and
// awaiting_phone_home clobber guards and the same-phase dedup. It
// returns proceed=false (zero value) when the update is rejected/deduped;
// otherwise proceed=true with the envelope fields the caller emits.
func (o *opState) updatePhase(opID, phase string) phaseUpdate {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.failedOps[opID] {
		return phaseUpdate{}
	}

	// A deploy op awaiting phone-home is in a terminal wait: the install is
	// done and the host is (re)booting its freshly installed disk.
	// BootActionForMAC keys BootLocal on this phase, so any write that moves
	// the op off it hands the next PXE BootDeploy and the host reinstalls
	// over that disk. The writers that can still arrive here are the
	// post-install firmware PXE (DHCP OnACK, the boot-script handler), the
	// executor's power_cycle/pxe_booting when it drives an op whose install
	// finished while the op was still pending, and an agent phase report
	// processed after deploy_complete. Completion goes through
	// RecordPhoneHome, not a phase write, so the only phase allowed to leave
	// is the agent's "failed" (HandleProgressReport writes it just before
	// MarkOpFailed). A same-phase write falls through to the dedup below.
	if op, ok := o.ops[opID]; ok && op.Phase == "awaiting_phone_home" &&
		phase != "awaiting_phone_home" && phase != "failed" {
		return phaseUpdate{}
	}

	var machineID string
	if op, ok := o.ops[opID]; ok {
		op.Phase = phase
		machineID = op.MachineID.String()
	}

	now := time.Now().UTC()
	prev, hasPrev := o.phaseStart[opID]
	if hasPrev && prev.phase == phase {
		return phaseUpdate{}
	}
	o.phaseStart[opID] = phaseState{phase: phase, started: now}

	var traceID, machineUUID uuid.UUID
	if op, ok := o.ops[opID]; ok {
		traceID = op.TraceID
		machineUUID = op.MachineID
	}
	return phaseUpdate{
		proceed:     true,
		now:         now,
		prev:        prev,
		hasPrev:     hasPrev,
		machineID:   machineID,
		machineUUID: machineUUID,
		traceID:     traceID,
	}
}
