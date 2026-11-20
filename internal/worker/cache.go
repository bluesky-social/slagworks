package worker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/imagepull"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// Cache is an in-memory, snapshot-driven view of slagworks state for
// one site. It implements model.MachineProvider.
//
// The snapshot-derived read state (machines, DHCP, images, scripts, ssh
// keys, operations) is full-replaced on every ApplySnapshot call — the
// server-side snapshot is authoritative for the site, there is no merge
// logic.
//
// Concurrency:
//
//   - The purely-immutable snapshot read state lives in a *snapshotView
//     published behind view (an atomic.Pointer). Pure reads (the DHCP /
//     TFTP / HTTP-boot serving hot path) load it lock-free and are never
//     blocked by an apply, including the apply's minutes-long image sync
//     (which runs AFTER the view swap).
//   - The mutable op-layer lives in a separate *opState component
//     (opstate_cache.go) under its OWN RWMutex: the ops map (rebuilt each
//     apply, with local status/phase mutations layered on top between
//     applies), machineState (RecordPhoneHome's local machine-state
//     overlay, cleared each apply), failedOps and phaseStart (preserved
//     across applies). That mutex ALSO guards the view-pointer swap:
//     opState holds a pointer to Cache.view and both loads it (combined
//     reads) and stores into it (replaceFromSnapshot) under its own lock,
//     so combined readers (BootActionForMAC and friends) that need both a
//     machine and its op see a (view, op-overlay) pair from the same
//     generation — no torn read across a swap. This is the role the
//     pre-extraction stateMu played; it transferred wholesale to opState.mu
//     when the op layer became its own component (Tier 2 Phase 1).
//   - applyMu serializes ApplySnapshot end-to-end so two applies can't
//     interleave (kills the old release-mid-apply double-fire).
//   - subsMu (RWMutex) guards only the snapshot-subscriber registry
//     (snapshotSubs / nextSnapshotSubID). It is orthogonal to the op layer:
//     applyMu already serializes applies, so subsMu just protects the
//     registry map against a concurrent OnSnapshotApplied.
//   - The durable outbox is a separate *outbox (outbox.go) with its own
//     lock, persistence, backoff, and wake channel.
//
// Lock order inside ApplySnapshot: applyMu (whole body) -> opState.mu (swap
// view + reset op overlay, release) -> subsMu.RLock (snapshot callbacks,
// release) -> imageSyncMu (image sync, release). No code acquires applyMu
// while holding opState.mu/subsMu/outboxMu/imageSyncMu; no reader/mutator
// holds more than one of opState.mu/subsMu/outboxMu/imageSyncMu; subscriber
// callbacks run with none of these held. persistMu (persistOpState) is
// never taken while holding opState.mu; it wraps only opState.mu.RLock and
// the LocalDB write. Pure readers take no lock.
type Cache struct {
	// applyMu serializes ApplySnapshot end-to-end. Never taken by a reader.
	applyMu sync.Mutex

	// viewFromDisk is true while the view is the one rehydrated from
	// LocalDB, until the first live ApplySnapshot replaces it. Guarded by
	// applyMu (NewCache sets it before the Cache is shared).
	viewFromDisk bool

	// view is the immutable snapshot-derived read state. Pure reads load it
	// lock-free. ApplySnapshot swaps it via opState.replaceFromSnapshot,
	// which holds opState.mu across the store so combined readers see a
	// tear-free (view, op-overlay) pair. Never nil (starts at emptyView()).
	view atomic.Pointer[snapshotView]

	// opState owns the mutable op-layer (ops, machineState overlay,
	// failedOps, phaseStart) under its own RWMutex, and guards the view
	// swap. See opstate_cache.go. Never nil.
	opState *opState

	// outbox is the durable send queue for observations and op updates
	// (see outbox.go). It owns its own lock, persistence, backoff, and
	// wake channel. Never nil.
	outbox *outbox

	// Wake is the outbox drain wake channel (aliases outbox.wakeCh). The
	// WS drain loop parks on this; it fires (buffered, non-blocking)
	// whenever an observation or op update is enqueued or a backed-off row
	// becomes due. Exported for the WS drain loop and tests.
	Wake chan struct{}

	// OpWake is the executor pending-op wake channel. It fires (buffered,
	// non-blocking) when an op_delta / op_resync adds a PENDING op, so the
	// executor drives it in ms instead of waiting on its 5s backstop tick
	// (Tier 2 Phase 2). The executor selects on this alongside its ticker.
	OpWake chan struct{}

	// localDB persists the machine cache for crash recovery. nil when
	// running without persistence (tests). The outbox holds its own
	// reference for observation persistence.
	localDB *LocalDB

	// persistMu serializes persistOpState's read-then-write. The WS read
	// loop (stream applies) and the agent / phone-home HTTP handlers (the
	// latch mutations) both persist the op layer; without it a write of an
	// older read could land after a newer one and drop the latch from
	// disk. Taken before opState.mu (RLock, inside snapshotForPersist) and
	// never while holding it.
	persistMu sync.Mutex

	// ImagesDir is the on-disk directory for boot images. When set,
	// ApplySnapshot calls SyncImages after the view swap.
	ImagesDir string
	// imageSyncFn runs the image sync; defaults to imagepull.SyncImages.
	// Tests inject a slow/blocking sync to exercise apply overlap and the
	// single-fire property.
	imageSyncFn func(imagesDir string, images map[string]*model.Image) error
	imageSyncMu sync.Mutex // serializes image downloads

	// snapshotSubs holds callbacks invoked at the end of every successful
	// ApplySnapshot call (CastManager pump reconcile, DHCP pool refresh).
	// Callbacks run outside all locks to avoid forcing downstream code to be
	// lock-aware. Keyed by subscription ID for unsubscribe. Guarded by
	// subsMu.
	subsMu            sync.RWMutex
	snapshotSubs      map[uint64]func()
	nextSnapshotSubID uint64

	// logSink receives structured envelopes for site-originated
	// phase / progress events. nil during tests; production wires
	// this to the same logring the WS forwarder drains, so cache
	// events flow through the structured-logs pipeline alongside
	// agent and SOL envelopes.
	logSink slogkit.Sink
}

// NewCache returns a Cache rehydrated from localDB's persisted state.
// A nil localDB disables persistence (unit tests). The worker's own verified
// hardware serial is left empty — use NewCacheWithSerial to wire it (the P4b
// worker-side self-power interlock reads it). An empty selfSerial never
// matches a real downstream, so the interlock skip is simply inert.
func NewCache(localDB *LocalDB) *Cache {
	return NewCacheWithSerial(localDB, "")
}

// NewCacheWithSerial is NewCache carrying the worker's own verified hardware
// serial (SMBIOS, P4a). pendingPowerOps uses it for the P4b worker-side
// defense-in-depth: skip an outlet op that would cut this worker's own feed
// (unless AllowSelf cycle/on). Production wires the serial here from
// siteConfig.serial; tests that don't exercise the self-power path use
// NewCache (selfSerial "").
func NewCacheWithSerial(localDB *LocalDB, selfSerial string) *Cache {
	ob := newOutbox(localDB)
	c := &Cache{
		outbox:  ob,
		Wake:    ob.wakeCh,
		OpWake:  make(chan struct{}, 1),
		localDB: localDB,
	}
	// opState is fed by the op-event STREAM (not the snapshot). A set
	// rehydrated from LocalDB starts UNTRUSTED (may be stale — global
	// could have failed/completed an op during the outage); the first
	// op_resync clears the flag. A fresh Cache with no persisted op set
	// starts trusted-empty (nothing to be stale about; the resync fills it).
	c.opState = newOpState(&c.view, c.OpWake, false, selfSerial)
	c.imageSyncFn = func(imagesDir string, images map[string]*model.Image) error {
		return imagepull.SyncImages(slog.Default(), imagesDir, images)
	}

	v := emptyView()
	if localDB != nil {
		// Snapshot meta (site info, DHCP config, images, scripts,
		// ssh keys) rehydrated first so DHCP/TFTP can serve from
		// last-known-good state even before the WS connection to global
		// completes its first sync. Operations are NOT here — they
		// rehydrate separately (op_state), UNTRUSTED, below.
		if meta := localDB.LoadSnapshotMeta(); meta != nil {
			c.viewFromDisk = true
			v.seqNum = meta.SeqNum
			v.siteInfo = meta.Site
			v.dhcpCfg = meta.DHCP
			v.oobCfg = meta.OOB
			for i := range meta.Images {
				img := meta.Images[i]
				v.images[img.Name] = &img
			}
			v.sshKeys = meta.SSHKeys
			// Rehydrate the OOB reservation set so a worker that restarts
			// before its first WS sync still serves DHCP for the whole OOB
			// fleet from last-known-good. ApplySnapshot full-replaces this
			// on the first live sync.
			for _, r := range meta.Reservations {
				v.oobReservations[r.MAC] = r.IP
			}
		}
		if cached := localDB.LoadMachineCache(); cached != nil {
			for id, m := range cached {
				v.machines[id] = m
				v.indexMachineInterfaces(id, m.Interfaces)
			}
		}
		// Rehydrate the op set + overlay UNTRUSTED: a persisted active
		// deploy op must not actuate a re-wipe on cold start before the
		// first op_resync (see opState.untrusted). Absent (fresh install,
		// or a DB written by a pre-Tier-2 binary) leaves the empty set.
		if os := localDB.LoadOpState(); os != nil {
			c.opState.rehydrate(*os)
		}
	}
	c.view.Store(v)

	return c
}

// --- Op-event stream apply (Tier 2 Phase 2) ---
//
// The site cache's op layer is fed by these three methods from the WS
// client's read loop, replacing the ops-in-snapshot path. Each persists
// the resulting op set + overlay to LocalDB so a crash rehydrates them
// (untrusted). See opstate_cache.go for the version/tombstone/untrusted
// semantics.

// ApplyOpDelta applies one op_delta (last-writer-wins by version, tombstone
// guard) and fires the executor wake if it added a pending op.
func (c *Cache) ApplyOpDelta(op model.Operation, machineState model.MachineState) {
	if c.opState.applyOpDelta(op, machineState) {
		c.opState.fireWake()
	}
	c.persistOpState()
}

// ApplyOpResync full-replaces the op set from a resync, rebuilds the
// overlay (preserving a local phone-home's running overlay), and clears
// the untrusted flag.
func (c *Cache) ApplyOpResync(ops []model.Operation, states map[string]model.MachineState) {
	c.opState.applyOpResync(ops, states)
	c.persistOpState()
}

// ApplyOpRemoved drops a terminal op, tombstones it, and moves the overlay
// to the carried durable machine state (if any).
func (c *Cache) ApplyOpRemoved(opID, machineID string, version int, machineState model.MachineState) {
	c.opState.applyOpRemoved(opID, machineID, version, machineState)
	c.persistOpState()
}

// persistOpState writes the current op layer to LocalDB. Called after every
// stream apply and after the local mutations that keep an installed host
// from being re-imaged (UpdateOpPhase to awaiting_phone_home,
// RecordPhoneHome), so a crash rehydrates the last-known op layer
// (untrusted) with those latches intact. Best-effort; a nil localDB (tests)
// is a no-op.
func (c *Cache) persistOpState() {
	if c.localDB == nil {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.localDB.SaveOpState(c.opState.snapshotForPersist())
}

// LastSeqNum returns the SeqNum of the most recently applied snapshot,
// or 0 if no snapshot has been applied yet. The WS client sends this
// in its hello{} payload so global can short-circuit to no_change when
// the worker is already up to date.
func (c *Cache) LastSeqNum() int64 {
	return c.view.Load().seqNum
}

// HelloSeqNum is the last_seq_num the worker reports in its WS hello. It is
// LastSeqNum, except 0 while the view is still the one rehydrated from
// LocalDB: the hub answers a matching last_seq_num with no_change, and the
// persisted copy may have been saved by an older binary that dropped fields
// it didn't know, so a restarted worker always asks for the full snapshot
// (which ApplySnapshot accepts at equal SeqNum over a disk view).
func (c *Cache) HelloSeqNum() int64 {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	if c.viewFromDisk {
		return 0
	}
	return c.view.Load().seqNum
}

// --- MachineProvider implementation ---

// BootActionForMAC returns the boot action based on machine state, the
// presence of a pending op, and the active op's phase. Unknown MACs
// always return BootLocal (never PXE boot an unregistered machine).
//
// Decision table (forward-path scope; redeploy-from-running is punted):
//
//   - any state, deploy op phase == awaiting_phone_home → BootLocal
//     (post-deploy reboot; the install is done, the OS should boot)
//   - state new + pending commission op             → BootCommission
//   - state failed + pending commission op          → BootCommission
//   - state commissioned + pending deploy op        → BootDeploy
//   - state failed + pending deploy op              → BootDeploy
//   - state commissioning (op already claimed)      → BootCommission
//   - state deploying + deploy op                   → BootDeploy
//   - anything else (running with no ops, etc.)     → BootLocal
//
// Two clauses are critical and not obvious:
//
//   - "awaiting_phone_home → BootLocal" prevents the post-deploy PXE
//     loop. The machine stays in `deploying` until phone-home; without
//     phase-aware dispatch, the post-deploy reboot would re-serve the
//     deploy initrd and infinite-loop. It holds in every machine state
//     because the pending-op clauses let an install finish before the
//     claim's op_delta moves the machine off commissioned/failed.
//   - The pending-op clauses are what make the state-on-claim model
//     work: the host PXEs while the machine is in a resting state,
//     boot-action sees the pending op and serves the right initrd,
//     the agent boots and claims the op (which advances the state).
//
// Combined read: machine identity comes from the view, machine State and
// the active op come from the op-layer overlay. opState holds opState.mu
// around the view.Load so the (machine, op) pair is from one generation —
// a swap can't slip between reading the machine and the op.
func (c *Cache) BootActionForMAC(mac string) model.BootAction {
	return c.opState.bootActionForMAC(mac)
}

func (c *Cache) MachineIDForMAC(mac string) (uuid.UUID, bool) {
	m, ok := c.view.Load().machineByMAC(mac)
	if !ok {
		return uuid.Nil, false
	}
	return m.ID, true
}

// --- Local observation recording ---

func (c *Cache) appendObservation(obs model.Observation) {
	c.outbox.appendObservation(obs)
}

func (c *Cache) appendOpUpdate(u model.OperationUpdate) {
	c.outbox.appendOpUpdate(u)
}

// NotifyLogWrite signals that new deploy logs have been written to SQLite.
// Called by writers that insert logs directly via localDB.
func (c *Cache) NotifyLogWrite() {
	c.outbox.wake()
}

// recordDeployComplete records the agent's "install done, rebooting"
// signal. The deploy op is NOT completed here — it stays active
// with phase=`awaiting_phone_home` while we wait for the installed
// OS to phone home. Local machine state does not change.
//
// data is the JSON-serialized payload that travels with the
// deploy_complete observation — currently the agent's BMCCreds from
// the BMC reconcile phase. nil is allowed; the observation goes out
// without a Data field.
//
// The boot-action decision table reads `phase=awaiting_phone_home →
// BootLocal`, which lets the post-install reboot chain to disk
// instead of looping back into the deploy initrd. The op transitions
// to `completed` only when phone-home arrives (RecordPhoneHome).
func (c *Cache) recordDeployComplete(machineID, operationID uuid.UUID, data []byte) {
	obs := model.Observation{
		MachineID:   machineID,
		OperationID: operationID,
		Event:       "deploy_complete",
		Timestamp:   time.Now(),
	}
	if len(data) > 0 {
		obs.Data = json.RawMessage(data)
	}
	c.appendObservation(obs)
}

// recordBMCReconcileReport queues the bmc_reconcile_report observation for
// a remote bmc_reconcile op. Like deploy_complete it carries a plaintext
// monitor password, so it travels only on the observation channel: the
// row sits in the worker's local outbox (SQLite) until global acks it,
// then is deleted. It never becomes an op_update, whose result global
// persists verbatim in the operations table.
func (c *Cache) recordBMCReconcileReport(machineID, operationID uuid.UUID, rep model.BMCReconcileReport) error {
	data, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	c.appendObservation(model.Observation{
		MachineID:   machineID,
		OperationID: operationID,
		Event:       "bmc_reconcile_report",
		Timestamp:   time.Now(),
		Data:        data,
	})
	return nil
}

// RecordPhoneHome is the canonical signal that the installed OS
// booted successfully. It completes the active deploy op and updates
// the local cache state.
//
// The completion travels on two channels. The phone_home observation
// carries the deploy op's ID and is the durable one: it sits in the
// SQLite outbox until global acks it, and global's observation handler
// completes the op (deploying → running) from it. That matters because
// the installed OS phones home exactly once after a 2xx, so nothing
// re-sends the completion if it is lost after we answer. The op_update
// with status=completed is memory-only (lost on a worker restart, on a
// connection that dies before the hub reads it, or on a failed hub
// handler call); it stays as the existing path, and whichever of the
// two reaches global first completes the op while the other is a no-op.
//
// If no active deploy op is found (late phone-home, op already
// completed by timeout, etc.), the observation still goes out with no
// operation ID: global acks it without a state transition.
func (c *Cache) RecordPhoneHome(machineID uuid.UUID) {
	// opState records the local machine-state advance as an overlay rather
	// than mutating the (immutable) view: the machine stays `running`
	// locally until the next snapshot carries the authoritative state, at
	// which point ApplySnapshot clears the overlay. This is what keeps
	// BootActionForMAC returning BootLocal on the post-install reboot.
	deployOpID := c.opState.recordPhoneHome(machineID)

	c.appendObservation(model.Observation{
		MachineID:   machineID,
		OperationID: deployOpID,
		Event:       "phone_home",
		Timestamp:   time.Now(),
	})

	if deployOpID != uuid.Nil {
		c.RecordOpUpdate(deployOpID.String(), string(model.OpCompleted), nil)
	}

	// The completion op_update is not durable, and global may be
	// unreachable: persist the running overlay and the phoneHomed latch so a
	// restart before global completes the op does not lose them. The first
	// op_resync after the restart can still carry the op as
	// claimed-deploying, and applyOpResync pins the machine running only if
	// phoneHomed survived.
	c.persistOpState()
}

// recordCommissionReport queues a commission_report observation. Unlike
// the old code path, this does NOT flip local state to Ready — global
// performs the NetBox decomposition + state transition and the next
// snapshot push carries the authoritative state change.
func (c *Cache) recordCommissionReport(machineID, operationID uuid.UUID, data []byte) {
	c.appendObservation(model.Observation{
		MachineID:   machineID,
		OperationID: operationID,
		Event:       "commission_report",
		Timestamp:   time.Now(),
		Data:        json.RawMessage(data),
	})
}

// RecordInventoryReport queues an inventory_import observation for a
// machine. Unlike RecordCommissionReport, this does not modify local
// machine state — global handles device matching and state transitions.
func (c *Cache) RecordInventoryReport(machineID uuid.UUID, data []byte) {
	c.appendObservation(model.Observation{
		MachineID: machineID,
		Event:     "inventory_import",
		Timestamp: time.Now(),
		Data:      json.RawMessage(data),
	})
}

// dhcpLeaseObservation is the wire shape of a dhcp_lease observation's
// Data field. Mirrors the LeaseEvent the dhcp handler emitted, plus
// the classification/enrichment only the worker's cache can supply
// (kind via ReservedIPForMAC, machine_id via MachineByMAC — see
// docs/plans/2026-07-13-dhcp-lease-visibility.md Phase 3).
type dhcpLeaseObservation struct {
	Event     string `json:"event"`
	MAC       string `json:"mac"`
	IP        string `json:"ip"`
	Kind      string `json:"kind"`
	Pool      string `json:"pool"`
	ExpiresAt string `json:"expires_at"`
}

// RecordDHCPLease queues a dhcp_lease observation for global's
// lease-history ledger. machineID is uuid.Nil for a MAC that doesn't
// resolve to a known machine — legal on the promiscuous OOB segment
// (unregistered BMCs/PDUs still get leases), and NOT an error case:
// the ledger fold keys on (site, worker, mac, ip), not machine
// identity.
func (c *Cache) RecordDHCPLease(machineID uuid.UUID, event, mac, ip, kind, pool string, expiresAt time.Time) {
	data, err := json.Marshal(dhcpLeaseObservation{
		Event:     event,
		MAC:       mac,
		IP:        ip,
		Kind:      kind,
		Pool:      pool,
		ExpiresAt: expiresAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		// Marshaling a struct of plain strings/times cannot fail in
		// practice; guard anyway so a future field addition that CAN
		// fail (e.g. a bad json.RawMessage) degrades to a dropped
		// observation instead of a panic.
		slog.Warn("dhcp_lease observation marshal failed, dropping", "err", err)
		return
	}
	c.appendObservation(model.Observation{
		MachineID: machineID,
		Event:     "dhcp_lease",
		Timestamp: time.Now(),
		Data:      data,
	})
}

// --- Snapshot apply (full-replace) ---

// ApplySnapshot replaces all snapshot-derived state with the given
// snapshot. Local-only state (outbox, phase tracking, failure markers) is
// preserved; the machine-state and op status/phase overrides are reset
// (the snapshot is authoritative).
//
// applyMu serializes the whole method so two applies cannot interleave
// (that interleave was the source of the old release-mid-apply
// double-fire). The sequence is:
//
//  1. seqnum-gate (reject stale) — no side effect if rejected.
//  2. build the next immutable snapshotView from snap.
//  3. opState.replaceFromSnapshot: under opState.mu, swap the view pointer
//     AND reset the op overlay (ops rebuilt from snap, machineState
//     cleared) as one transition, so combined readers never see a torn
//     (view, op-overlay) pair. failedOps/phaseStart are preserved.
//  4. snapshot the subscriber callbacks under subsMu.
//  5. image sync (serialized by imageSyncMu) — with NO other lock held, so
//     the minutes-long download blocks no reader.
//  6. persist to LocalDB.
//  7. fire subscribers exactly once, with no lock held.
//
// Steps 5-7 are the current order (the old code synced, persisted, then
// fired subscribers last); image-sync and persist failures are non-fatal
// and leave the freshly-published snapshot visible (see below).
//
// Machines whose Interfaces slice is empty (soft-decommissioned per the
// NetBox-SSoT model) simply don't appear in macIndex — so BootActionForMAC
// returns BootLocal for their old MACs.
//
// Reports whether snap was applied (false: rejected as stale).
func (c *Cache) ApplySnapshot(snap *model.SiteSnapshot) bool {
	return c.applySnapshot(snap, false)
}

// AdoptSnapshot applies snap as global's current view of the site whatever
// its SeqNum, skipping ApplySnapshot's stale check. The WS client calls it
// for the first snapshot on each connection, which global sends as its
// current view (the hello reply, or a fan-out at least as new).
//
// ApplySnapshot's check exists for snapshots reordered within one
// connection. Across connections global's per-site counter can go
// backwards: the site config was deleted and recreated (the new row starts
// again at 0), global's database was restored from a backup, or the worker
// was pointed at a different global. A worker holding a higher SeqNum
// would otherwise drop every snapshot until global's counter passed it,
// and keep serving the stale site meanwhile, across restarts.
func (c *Cache) AdoptSnapshot(snap *model.SiteSnapshot) {
	c.applySnapshot(snap, true)
}

func (c *Cache) applySnapshot(snap *model.SiteSnapshot, adopt bool) bool {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()

	// Ignore out-of-order (stale) snapshots: on reconnect, the hub can
	// race a hello-reply snapshot with a concurrent Push fan-out. If the
	// Push arrived first and we applied SeqNum=N+1, the hello-reply
	// SeqNum=N must not clobber the newer state. Equal SeqNum is also a
	// no-op (same content, by definition — global only bumps on
	// content-hash change). No side effect on the rejected path.
	//
	// Exception: the first equal-SeqNum snapshot over a view rehydrated
	// from LocalDB is applied. The persisted copy may have been decoded
	// and saved by an OLDER worker binary that dropped fields it didn't
	// know (a global-first rollout pushes the new snapshot to old workers,
	// which then upgrade and restart). Global's content hash already
	// covers the new field, so it never bumps SeqNum again, and without
	// this the worker would run on the field-stripped copy indefinitely
	// (serial console capture never started on pop1, 2026-10-24).
	//
	// An adopted snapshot skips the check (see AdoptSnapshot).
	cur := c.view.Load().seqNum
	if !adopt && snap.SeqNum != 0 && (snap.SeqNum < cur || (snap.SeqNum == cur && !c.viewFromDisk)) {
		return false
	}
	if adopt && snap.SeqNum < cur {
		slog.Warn("adopting global's snapshot although its sequence is lower than the one this worker holds",
			"held_seq", cur, "global_seq", snap.SeqNum,
			"hint", "expected after the site config was deleted and recreated or global's database was restored; "+
				"if neither happened, check that SLAGWORKS_API_URL names the intended global")
	}
	c.viewFromDisk = false

	// Build the next immutable view. Nothing is published until the swap.
	v := &snapshotView{
		seqNum:          snap.SeqNum,
		siteInfo:        snap.Site,
		dhcpCfg:         snap.DHCP,
		oobCfg:          snap.OOB,
		machines:        make(map[uuid.UUID]*model.MachineSnapshot, len(snap.Machines)),
		macIndex:        make(map[string]uuid.UUID),
		ambiguousMACs:   make(map[string]struct{}),
		ipByMAC:         make(map[string]string),
		oobReservations: make(map[string]string, len(snap.Reservations)),
		images:          make(map[string]*model.Image, len(snap.Images)),
		sshKeys:         append([]string(nil), snap.SSHKeys...),
	}
	for i := range snap.Machines {
		m := snap.Machines[i] // copy
		v.machines[m.ID] = &m
		v.indexMachineInterfaces(m.ID, m.Interfaces)
	}
	for mac := range v.ambiguousMACs {
		slog.Warn("duplicate machine-interface MAC excluded from identity and DHCP reservation",
			"mac", mac)
	}

	// OOB reservation set. Keyed by MAC; the builder already lowercases,
	// matching ipByMAC and the DHCP handler's net.HardwareAddr.String()
	// lookups. reservedIP gives ipByMAC (machine) precedence over this set
	// on conflict — both come from the same NetBox interfaces, so a genuine
	// disagreement means a NetBox data problem, not a normal case. We don't
	// fail the snapshot over it (the machine value is authoritative and
	// usable), but we surface it loudly so the operator sees the drift
	// instead of it hiding behind the silent precedence. Compare bare host
	// IPs (ipByMAC may carry CIDR form).
	for _, r := range snap.Reservations {
		v.oobReservations[r.MAC] = r.IP
		if machineCIDR, ok := v.ipByMAC[r.MAC]; ok {
			machineIP := machineCIDR
			if ip, _, err := net.ParseCIDR(machineCIDR); err == nil {
				machineIP = ip.String()
			}
			if machineIP != r.IP {
				slog.Warn("OOB reservation disagrees with machine-interface IP; machine value wins",
					"mac", r.MAC, "machine_ip", machineIP, "oob_ip", r.IP)
			}
		}
	}

	for i := range snap.Images {
		img := snap.Images[i]
		v.images[img.Name] = &img
	}

	// Publish the identity view under opState.mu, and CONDITIONALLY clear
	// the machine-state overlay: overlay[id] clears ONLY where the applied
	// baseline State equals the overlay value (the durable state caught
	// up) — the re-wipe-window fix. The op set is NOT touched here: it is
	// fed by the op-event stream, not the snapshot. Combined readers hold
	// opState.mu (RLock) across their view.Load, so they never see a new
	// view paired with a stale overlay.
	c.opState.replaceFromSnapshot(v)

	// Snapshot the subscriber callbacks under subsMu; invoke them after
	// releasing all locks. Sorted by subscription ID for deterministic
	// order.
	c.subsMu.RLock()
	ids := make([]uint64, 0, len(c.snapshotSubs))
	for id := range c.snapshotSubs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	cbs := make([]func(), 0, len(ids))
	for _, id := range ids {
		cbs = append(cbs, c.snapshotSubs[id])
	}
	c.subsMu.RUnlock()

	// Sync images to disk with NO state lock held. SyncImages is idempotent
	// — it compares each image's on-disk link with the row's SHA256 and
	// only downloads versions it does not already hold.
	// Serialized by imageSyncMu (also held by no reader). Failure is
	// non-fatal: the published snapshot stays visible; the next apply (or
	// the next tick, since the row's SHA still mismatches) retries.
	if c.ImagesDir != "" && len(v.images) > 0 {
		images := make(map[string]*model.Image, len(v.images))
		maps.Copy(images, v.images)
		c.imageSyncMu.Lock()
		// Wrap in func+defer so a panic doesn't leak imageSyncMu — a
		// recovered panic upstream would otherwise permanently deadlock
		// subsequent ApplySnapshot calls.
		func() {
			defer c.imageSyncMu.Unlock()
			if err := c.imageSyncFn(c.ImagesDir, images); err != nil {
				slog.Error("image sync failed", "err", err)
			}
		}()
	}

	// Persist updated cache. One transactional SaveSnapshot writes machines
	// + meta atomically — a partial write (machines saved, meta not, or
	// vice versa) would silently desync the worker against the global on
	// next reconnect. See LocalDB.SaveSnapshot. The meta half is what lets a
	// cold start serve DHCP/TFTP from cached state while the WS connection
	// to global is still catching up. Failure is non-fatal (LocalDB logs
	// internally); the on-disk warm-cache is a best-effort restart
	// optimization, not a transaction boundary. applyMu guarantees no newer
	// snapshot has been published, so there is no stale-write guard to make.
	if c.localDB != nil {
		c.localDB.SaveSnapshot(snap)
	}

	// Fire subscribers exactly once for this applied snapshot, with no lock
	// held. Callbacks run goroutine-safely against public Cache methods.
	for _, cb := range cbs {
		cb()
	}
	return true
}

// OnSnapshotApplied registers a callback invoked at the end of every
// successful ApplySnapshot and returns an unsubscribe func. Multiple
// subscribers are supported (CastManager reconcile, DHCP pool refresh);
// callbacks run sequentially in subscription order, outside all cache
// locks.
func (c *Cache) OnSnapshotApplied(cb func()) (unsubscribe func()) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	id := c.nextSnapshotSubID
	c.nextSnapshotSubID++
	if c.snapshotSubs == nil {
		c.snapshotSubs = make(map[uint64]func())
	}
	c.snapshotSubs[id] = cb
	return func() {
		c.subsMu.Lock()
		defer c.subsMu.Unlock()
		delete(c.snapshotSubs, id)
	}
}

// OutboxDepth returns the number of in-memory observations awaiting
// drain+ack. Scrape-time gauge for the site worker's /metrics.
func (c *Cache) OutboxDepth() int {
	return c.outbox.depth()
}

// ActiveOpCount returns the number of non-terminal operations in the
// snapshot (pending, claimed, running). Scrape-time gauge for the
// site worker's /metrics.
func (c *Cache) ActiveOpCount() int {
	return c.opState.activeOpCount()
}

// --- Outbox draining (delegated to the outbox component) ---

// DrainOutbox returns pending observations (paired with SQLite row IDs
// for later acking) and operation updates, clearing the in-memory queues.
// See outbox.drain.
func (c *Cache) DrainOutbox() ([]PendingOutbox, []model.OperationUpdate) {
	return c.outbox.drain()
}

// RequeueRejected returns a non-terminally rejected observation to the
// outbox with per-row exponential backoff. See
// outbox.requeueRejected.
func (c *Cache) RequeueRejected(entry PendingOutbox) (attempts int, delay time.Duration, warn bool) {
	return c.outbox.requeueRejected(entry)
}

// RequeueOutbox prepends unsent items back onto the in-memory queues after
// a mid-batch send failure. See outbox.requeue.
func (c *Cache) RequeueOutbox(obs []PendingOutbox, updates []model.OperationUpdate) {
	c.outbox.requeue(obs, updates)
}

// AckObservation removes an observation from durable storage after global
// acked its receipt via rpc_response{ok:true}. See outbox.ackObservation.
func (c *Cache) AckObservation(rowID uint64) {
	c.outbox.ackObservation(rowID)
}

// ReloadOutboxFromDisk repopulates the in-memory outbox with unacked
// SQLite rows on WS reconnect. See outbox.reloadFromDisk; an error means
// the rows could not be read and the outbox is incomplete.
func (c *Cache) ReloadOutboxFromDisk() error {
	return c.outbox.reloadFromDisk()
}

// --- Operation queries ---

// PendingPowerOps returns operations in "pending" status with power types.
func (c *Cache) PendingPowerOps() []model.Operation {
	return c.opState.pendingPowerOps()
}

// PendingLifecycleOps returns operations in "pending" status with lifecycle
// types (commission, deploy) that the executor should drive via PXE boot.
func (c *Cache) PendingLifecycleOps() []model.Operation {
	return c.opState.pendingLifecycleOps()
}

// ReclaimableOps returns the ops whose last claim went unanswered and that
// global's row now shows as claimed by workerID with no phase: the claim
// committed and only its reply was lost, so the executor claims them again.
func (c *Cache) ReclaimableOps(workerID string) []model.Operation {
	return c.opState.reclaimableOps(workerID)
}

// --- Machine lookups ---

// MachineByID returns a copy of the machine snapshot by ID, or nil. The
// copy reflects any local machine-state override (RecordPhoneHome), so a
// caller reading .State sees the same value BootActionForMAC would act on.
// Per-machine boot config (SystemUUID, StorageScript, KernelParams,
// NetworkScript, Interfaces, FQDN) is read directly off the returned
// snapshot.
//
// Combined reader (view + op overlay): opState holds opState.mu around the
// view.Load so the (machine identity, state overlay) pair is from one
// generation.
func (c *Cache) MachineByID(id uuid.UUID) *model.MachineSnapshot {
	return c.opState.machineByID(id)
}

// MachineByMAC returns a copy of the machine snapshot for a MAC, or nil.
// The copy reflects any local machine-state override (RecordPhoneHome).
// Same field-access contract as MachineByID. Combined reader: opState
// holds opState.mu across the view.Load (see MachineByID).
func (c *Cache) MachineByMAC(mac string) *model.MachineSnapshot {
	return c.opState.machineByMAC(mac)
}

// cloneMachineSnapshot returns a copy whose mutable nested fields
// (script pointers, interface slice) do NOT alias the source. The
// snapshotView is immutable by contract — every reader depends on that
// — so the machine getters must not hand callers pointers into it. The
// deleted per-field accessors (StorageScriptForMAC et al.) copied their
// field before returning; the collapsed getters preserve that property
// for the whole snapshot. Called per boot-config request, not on the
// DHCP packet path, so the copy cost is irrelevant.
func cloneMachineSnapshot(m *model.MachineSnapshot) *model.MachineSnapshot {
	cp := *m
	if m.StorageScript != nil {
		sc := *m.StorageScript
		cp.StorageScript = &sc
	}
	if m.NetworkScript != nil {
		ns := *m.NetworkScript
		cp.NetworkScript = &ns
	}
	if m.Console != nil {
		c := *m.Console
		cp.Console = &c
	}
	if m.Interfaces != nil {
		cp.Interfaces = make([]model.SnapshotInterface, len(m.Interfaces))
		copy(cp.Interfaces, m.Interfaces)
	}
	return &cp
}

// --- Collection accessors ---

// MachineCount returns the number of cached machines.
func (c *Cache) MachineCount() int {
	return len(c.view.Load().machines)
}

// SupermicroMachineCount returns how many cached machines are Supermicro
// (model.MachineSnapshot.IsSupermicro). The site worker uses it to warn
// when the site has Supermicro hosts but SMCIPMITool isn't installed.
func (c *Cache) SupermicroMachineCount() int {
	n := 0
	for _, m := range c.view.Load().machines {
		if m.IsSupermicro() {
			n++
		}
	}
	return n
}

// ListActiveMachineIDs returns the UUIDs of all cached machines
// that have a BMC IP set. The CastManager uses this to decide which
// machines to maintain a persistent ipmitool session against.
//
// "Active" here means "has somewhere for SOL to dial AND credentials to
// dial with." Machines without a BMCIP are skipped — they wouldn't be
// reachable anyway. Machines whose creds don't resolve (no machine-row
// creds and no active deploy op carrying a bmc_runtime envelope) are
// also skipped: pumping them just spawns ipmitool against a credless
// host, fails, backs off, and re-emits an identical "no creds" envelope
// every cycle. Pop1 has ~100 such hosts (PDUs, leaf switches, every
// host not yet provisioned), and including them drowned the SOL stream
// we actually care about (the host being imaged). Gating on
// cred-resolution is the documented proper fix for that fleet spam,
// replacing the earlier single-host hostname allowlist.
//
// Locked / failed / etc. machines DO get SOL coverage when creds
// resolve; the operator values knowing what a stuck box is printing.
//
// Locking: candidates (BMCIP set) are collected from the lock-free view,
// then cred-resolution runs with NO cache lock held.
// resolveSOLCredsWithSource calls ActiveDeployOpForMachine, which acquires
// opState.mu (RLock) — and Go's RWMutex is non-reentrant (a pending writer
// would deadlock a nested RLock), so the resolution must not run while this
// method holds any lock. (candidates come from the view via a lock-free
// atomic load, so there is nothing to hold here anyway.)
func (c *Cache) ListActiveMachineIDs() []uuid.UUID {
	v := c.view.Load()
	candidates := make([]*model.MachineSnapshot, 0, len(v.machines))
	for _, m := range v.machines {
		if m.BMCIP == "" {
			continue
		}
		cp := *m
		candidates = append(candidates, &cp)
	}

	out := make([]uuid.UUID, 0, len(candidates))
	for _, m := range candidates {
		user, pass, _ := resolveSOLCredsWithSource(c, m)
		if user == "" || pass == "" {
			continue
		}
		out = append(out, m.ID)
	}
	return out
}

// MachineState returns the machine's current state as a string (with any
// local override applied), or "" if the machine isn't in the cache. Used
// by the cast pump's silence watchdog to decide whether a long quiet
// period warrants a force-reconnect.
func (c *Cache) MachineState(id uuid.UUID) string {
	return c.opState.stateString(id)
}

// SelfSerial returns this worker's own verified hardware serial (the one
// reported in its WS hello), or "" when unverified. Fixed at construction.
// The cast transport selection matches it against a console server's
// NetBox serial to decide whether a console line is local.
func (c *Cache) SelfSerial() string {
	return c.opState.selfSerial
}

func (c *Cache) ImageCount() int {
	return len(c.view.Load().images)
}

// GetImages returns a copy of the image map.
func (c *Cache) GetImages() map[string]*model.Image {
	images := c.view.Load().images
	out := make(map[string]*model.Image, len(images))
	for k, v := range images {
		cp := *v
		out[k] = &cp
	}
	return out
}

// GetDHCPConfig returns the current DHCP configuration from the
// applied snapshot.
func (c *Cache) GetDHCPConfig() model.DHCPConfig {
	return c.view.Load().dhcpCfg
}

// GetOOBConfig returns the current OOB-segment DHCP config from the
// snapshot (subnet/gateway/DNS/NTP/dynamic-pool). Zero value (Subnet=="")
// when the site has no slagworks-oob prefix. The site worker reads this to
// decide whether (and how) to bind the OOB interface.
func (c *Cache) GetOOBConfig() model.OOBConfig {
	return c.view.Load().oobCfg
}

// GetSiteInfo returns the site's name/slug from the applied snapshot.
func (c *Cache) GetSiteInfo() model.SiteInfo {
	return c.view.Load().siteInfo
}

func (c *Cache) GetSSHKeys() []string {
	keys := c.view.Load().sshKeys
	out := make([]string, len(keys))
	copy(out, keys)
	return out
}

// ReservedIPForMAC returns the IP reserved for a MAC, or nil if none.
//
// Two sources, checked in precedence order:
//  1. ipByMAC — slagworks machine-interface IPs (physical/non-ipmi only,
//     operator-allocated at commission/deploy). Wins when present.
//  2. oobReservations — the full OOB reservation set from the snapshot,
//     INCLUDING mgmt_only/BMC interfaces and non-machine devices (PDUs,
//     switches, etc). This is what lets the worker serve DHCP for the
//     whole OOB fleet, replacing dnsmasq.
//
// The machine path wins on conflict because it reflects an operator-
// initiated allocation; in practice the two agree (same NetBox data). The
// IP field may be CIDR notation ("10.10.1.42/24") — the host portion is
// extracted.
func (c *Cache) ReservedIPForMAC(mac string) net.IP {
	return c.view.Load().reservedIP(mac)
}

// IsOOBManagedNonMachine reports whether a MAC is a KNOWN OOB device
// (it has an OOB reservation) that is NOT a slagworks-managed machine —
// i.e. a BMC, PDU, or switch-management port. Used as the DHCP
// BootEligible NEGATIVE gate: such a device gets its reserved address but
// must NOT be handed a PXE bootfile (it is not a bootable host, and on the
// promiscuous OOB segment a bootfile to every idle BMC is the blast-radius
// we want to avoid). A truly-unknown MAC (no reservation, no machine) is
// NOT caught here, so promiscuous auto-commission of brand-new hardware
// still boots. A slagworks machine is NOT caught here, so a deploying OR
// rebooting host still boots (reaches bootlocal). See PLA OOB DHCP takeover.
func (c *Cache) IsOOBManagedNonMachine(mac string) bool {
	v := c.view.Load()
	_, reserved := v.oobReservations[mac]
	if !reserved {
		return false
	}
	_, isMachine := v.machineByMAC(mac)
	return !isMachine
}

// --- Operation state mutations ---

// MarkOpClaimed marks an operation as claimed in the local cache to prevent
// duplicate pickup by PendingPowerOps on the next executor tick. The executor
// calls it as it sends a claim, so it also forgets an earlier unanswered
// claim for the op (see RollBackClaim).
func (c *Cache) MarkOpClaimed(opID string) {
	c.opState.markClaimAttempt(opID)
}

// RollBackClaim handles a claim that got no answer (transport error, RPC
// timeout): the local mark goes back to pending so the next executor tick
// retries, and the claim is recorded as unanswered so the retry still happens
// if global's claimed row replaces that mark first (ReclaimableOps).
func (c *Cache) RollBackClaim(opID string) {
	c.opState.rollBackClaim(opID)
}

func (c *Cache) MarkOpStatus(opID string, status model.OperationStatus) {
	c.opState.setStatus(opID, status)
}

// RecordOpUpdate queues an operation status update for push to central.
func (c *Cache) RecordOpUpdate(opID, status string, result json.RawMessage) {
	c.opState.setStatus(opID, model.OperationStatus(status))

	c.appendOpUpdate(model.OperationUpdate{
		OperationID: opID,
		Status:      status,
		Result:      result,
	})
}

// OperationCount returns the number of cached operations.
func (c *Cache) OperationCount() int {
	return c.opState.operationCount()
}

// GetOperation returns a copy of the operation with the given ID, or nil.
func (c *Cache) GetOperation(id string) *model.Operation {
	return c.opState.getOperation(id)
}

// --- Deploy log / phase tracking (local-only) ---

// SetLogSink wires the cache to a structured-log sink. Called once at
// site daemon startup so phase / progress events the cache emits land
// in the same logring the WS forwarder drains.
func (c *Cache) SetLogSink(sink slogkit.Sink) { c.logSink = sink }

// PrepareDeployLogs records the current operation ID for stale-insert
// rejection. Called by the executor when starting a new deploy
// operation. The deploy_logs SQLite table this used to wipe is gone;
// log envelopes for the prior op simply age out of the in-memory ring
// once the WS forwarder ships them upstream.
func (c *Cache) PrepareDeployLogs(machineID, operationID string) {
	if c.localDB == nil {
		return
	}
	c.localDB.SetCurrentDeployOp(machineID, operationID)
}

// UpdateOpPhase updates the phase of an operation in-memory, writes a
// source:"phase" log entry, and queues the update for push to central.
// If the operation has been marked failed, the update is rejected. The
// awaiting_phone_home clobber guard and same-phase dedup live in
// opState.updatePhase (opstate_cache.go), which documents why.
func (c *Cache) UpdateOpPhase(opID, phase string) {
	upd := c.opState.updatePhase(opID, phase)
	if phase == "awaiting_phone_home" {
		// The latch is the op row's phase, and the phase op_update below is
		// not durable: if global is unreachable and the worker restarts
		// before global echoes the phase back, LocalDB still holds the
		// pre-latch row, keepTerminalWait has nothing to keep when the
		// first op_resync carries global's row, and the installed host's
		// next PXE gets BootDeploy. Persist it now. Not gated on proceed:
		// updatePhase writes the row's phase before its same-phase dedup
		// returns proceed=false, and that write must reach disk too.
		c.persistOpState()
	}
	if !upd.proceed {
		return
	}

	// Emit a structured envelope for the phase change. site is the
	// emitter; trace_id / op_id / machine_id come from the op so
	// `slagctl logs --trace=<id>` shows phase events
	// alongside agent / sol envelopes.
	if c.logSink != nil && upd.machineID != "" {
		opUUID, _ := uuid.Parse(opID)
		env := slogkit.Envelope{
			TS:        upd.now,
			Level:     slogkit.LevelInfo,
			Source:    slogkit.SourceSite,
			Msg:       "phase " + phase,
			MachineID: upd.machineUUID,
			OpID:      opUUID,
			TraceID:   upd.traceID,
			Phase:     phase,
		}
		if upd.hasPrev {
			env.Extra = map[string]any{
				"prev_phase":  upd.prev.phase,
				"prev_dur_ms": upd.now.Sub(upd.prev.started).Round(time.Millisecond).Milliseconds(),
			}
		}
		c.logSink.Push(env)
	}

	c.appendOpUpdate(model.OperationUpdate{
		OperationID: opID,
		Phase:       phase,
	})
}

// MarkOpFailed marks an operation as failed in-memory and queues a status
// update. Once marked failed, UpdateOpPhase for this op is rejected —
// this prevents phase progression after terminal failure — and an op_delta
// or op_resync that still shows the op active does not revive it; only
// op_removed ends the local failure.
func (c *Cache) MarkOpFailed(opID, phase, errMsg string) {
	c.opState.markFailed(opID, phase)

	result := model.OpResult{Error: errMsg, Phase: phase}.Marshal()
	c.appendOpUpdate(model.OperationUpdate{
		OperationID: opID,
		Status:      "failed",
		Phase:       phase,
		Result:      result,
	})
}

func (c *Cache) IsOpFailed(opID string) bool {
	return c.opState.isOpFailed(opID)
}

// ActiveDeployOpForMachine returns the active (not completed or failed)
// deploy operation for the given machine, or nil if no deploy is in progress.
func (c *Cache) ActiveDeployOpForMachine(machineID uuid.UUID) *model.Operation {
	return c.opState.activeDeployOpForMachine(machineID)
}

// ActiveBootingOpForMachine returns the active (not completed or failed)
// boot-driving operation for the given machine — deploy or commission, both
// of which PXE-boot the machine and share the boot-prefix phase spine. Returns
// nil if no such op is in progress. Used for boot-phase reporting (DHCP ACK,
// iPXE load) that should animate the lifecycle pane for either op type.
func (c *Cache) ActiveBootingOpForMachine(machineID uuid.UUID) *model.Operation {
	return c.opState.activeBootingOpForMachine(machineID)
}

// HandleProgressReport processes a unified progress report from a deploy
// agent. Logs go to SQLite (always, even for failed ops). Failures,
// phase updates, and terminal events are routed to the appropriate
// cache methods.
func (c *Cache) HandleProgressReport(report *model.ProgressReport) error {
	opID := report.OperationID
	machineID := report.MachineID

	mid, err := uuid.Parse(machineID)
	if err != nil {
		return fmt.Errorf("invalid machine_id %q: %w", machineID, err)
	}
	oid, err := uuid.Parse(opID)
	if err != nil {
		return fmt.Errorf("invalid op_id %q: %w", opID, err)
	}
	machine, _, alreadyAcknowledged, err := c.opState.validateReportBinding(mid, oid, report.Event)
	if err != nil {
		return err
	}
	if alreadyAcknowledged {
		return nil
	}
	if report.Event == "commission_report" {
		if err := c.validateCommissionIdentity(machine, report.CommissionData); err != nil {
			return err
		}
	}

	if len(report.Logs) > 0 && c.logSink != nil {
		// ProgressReport's log shape predates structured logs; the
		// agent now emits envelopes via /logs/ingest, so this path
		// only fires when older agents talk to a newer site.
		// Convert each line to an envelope as a best-effort bridge.
		mUUID, _ := uuid.Parse(machineID)
		opUUID, _ := uuid.Parse(opID)
		traceID := c.opState.traceIDForOp(opID)
		for _, l := range report.Logs {
			ts, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
			if ts.IsZero() {
				ts = time.Now().UTC()
			}
			c.logSink.Push(slogkit.Envelope{
				TS:        ts,
				Level:     slogkit.LevelInfo,
				Source:    slogkit.SourceAgent,
				Msg:       l.Line,
				MachineID: mUUID,
				OpID:      opUUID,
				TraceID:   traceID,
				Extra:     map[string]any{"legacy_source": l.Source},
			})
		}
	}

	// Handle failure — write "failed" phase log before marking failed
	// (MarkOpFailed sets failedOps which blocks UpdateOpPhase).
	if report.Failed {
		c.UpdateOpPhase(opID, "failed")
		c.MarkOpFailed(opID, report.Phase, report.Error)
		return nil
	}

	if report.Phase != "" {
		c.UpdateOpPhase(opID, report.Phase)
	}

	switch report.Event {
	case "deploy_complete":
		// Phase moves to awaiting_phone_home. Op stays active; it
		// completes when the installed OS phones home (handled in
		// RecordPhoneHome). Without this phase, the boot-action
		// would re-serve the deploy initrd on the post-install
		// reboot and loop forever — see BootActionForMAC's
		// "awaiting_phone_home → BootLocal" clause.
		c.UpdateOpPhase(opID, "awaiting_phone_home")
		var data []byte
		if report.BMCCreds != nil {
			b, err := json.Marshal(report.BMCCreds)
			if err != nil {
				slog.Error("marshal BMCCreds for deploy_complete observation",
					"machine_id", mid, "err", err)
			} else {
				data = b
			}
		}
		c.recordDeployComplete(mid, oid, data)
	case "commission_report":
		c.recordCommissionReport(mid, oid, report.CommissionData)
	case "phone_home":
		c.RecordPhoneHome(mid)
	}
	return nil
}

// validateCommissionIdentity treats inventory as corroborating evidence for
// the already-bound machine. It rejects serial/UUID drift, foreign modeled
// NICs, and reports that do not contain any physical NIC modeled on the
// machine that booted.
func (c *Cache) validateCommissionIdentity(machine *model.MachineSnapshot, data json.RawMessage) error {
	if len(data) == 0 || !json.Valid(data) {
		return fmt.Errorf("commission_data must be valid JSON")
	}
	var inv struct {
		System *struct {
			SerialNumber  string `json:"serial_number"`
			ChassisSerial string `json:"chassis_serial"`
			UUID          string `json:"uuid"`
		} `json:"system"`
		NICs []struct {
			MAC    string `json:"mac"`
			Driver string `json:"driver"`
		} `json:"nics"`
		Disks []struct {
			Serial string `json:"serial"`
			WWN    string `json:"wwn"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(data, &inv); err != nil {
		return fmt.Errorf("decode commission identity: %w", err)
	}
	if inv.System == nil {
		return fmt.Errorf("commission inventory has no SMBIOS system identity")
	}
	if expected := strings.TrimSpace(machine.NetBoxSerial); expected != "" {
		systemSerial := strings.TrimSpace(inv.System.SerialNumber)
		chassisSerial := strings.TrimSpace(inv.System.ChassisSerial)
		if !strings.EqualFold(expected, systemSerial) && !strings.EqualFold(expected, chassisSerial) {
			return fmt.Errorf("SMBIOS serial mismatch for machine %s: expected %q, got system %q chassis %q",
				machine.ID, expected, systemSerial, chassisSerial)
		}
	}
	if expected := strings.TrimSpace(machine.SystemUUID); expected != "" {
		want, wantErr := uuid.Parse(expected)
		got, gotErr := uuid.Parse(strings.TrimSpace(inv.System.UUID))
		if wantErr != nil || gotErr != nil || want != got {
			return fmt.Errorf("SMBIOS system UUID mismatch for machine %s: expected %q, got %q",
				machine.ID, expected, inv.System.UUID)
		}
	}

	view := c.view.Load()
	reportedUUID := strings.TrimSpace(inv.System.UUID)
	reportedSystemSerial := strings.TrimSpace(inv.System.SerialNumber)
	reportedChassisSerial := strings.TrimSpace(inv.System.ChassisSerial)
	for otherID, other := range view.machines {
		if otherID == machine.ID {
			continue
		}
		if reportedUUID != "" && other.SystemUUID != "" && strings.EqualFold(reportedUUID, other.SystemUUID) {
			return fmt.Errorf("SMBIOS system UUID %s is already owned by machine %s", reportedUUID, otherID)
		}
		if other.NetBoxSerial != "" &&
			(strings.EqualFold(other.NetBoxSerial, reportedSystemSerial) || strings.EqualFold(other.NetBoxSerial, reportedChassisSerial)) {
			return fmt.Errorf("SMBIOS serial %q is already owned by machine %s", other.NetBoxSerial, otherID)
		}
		for _, got := range inv.Disks {
			for _, owned := range other.Disks {
				if strings.TrimSpace(got.WWN) != "" && strings.EqualFold(strings.TrimSpace(got.WWN), strings.TrimSpace(owned.WWN)) {
					return fmt.Errorf("disk WWN %s is already owned by machine %s", got.WWN, otherID)
				}
				if strings.TrimSpace(got.Serial) != "" && strings.EqualFold(strings.TrimSpace(got.Serial), strings.TrimSpace(owned.Serial)) {
					return fmt.Errorf("disk serial %s is already owned by machine %s", got.Serial, otherID)
				}
			}
		}
	}

	matchedExpectedNIC := false
	for _, nic := range inv.NICs {
		mac := strings.ToLower(strings.TrimSpace(nic.MAC))
		if mac == "" || model.IsBMCUSBNetworkDriver(nic.Driver) {
			continue
		}
		if _, ambiguous := view.ambiguousMACs[mac]; ambiguous {
			return fmt.Errorf("physical NIC %s is owned by multiple machines", mac)
		}
		owner, ok := view.machineByMAC(mac)
		if !ok {
			continue
		}
		if owner.ID != machine.ID {
			return fmt.Errorf("physical NIC %s is owned by machine %s, not %s", mac, owner.ID, machine.ID)
		}
		matchedExpectedNIC = true
	}
	if !matchedExpectedNIC {
		return fmt.Errorf("commission inventory contains no physical NIC owned by machine %s", machine.ID)
	}
	return nil
}

// DeployOpForMAC returns the active deploy operation for the machine
// with the given MAC, or nil if no deploy is in progress.
func (c *Cache) DeployOpForMAC(mac string) *model.Operation {
	return c.opState.deployOpForMAC(mac)
}

// CommissionOpForMAC returns the active commission operation for the
// machine with the given MAC, or nil if no commission is in progress.
// Used by the boot handler to render trace_id into the commission
// kernel cmdline so the agent's structured log lines correlate with
// the op that triggered the boot.
func (c *Cache) CommissionOpForMAC(mac string) *model.Operation {
	return c.opState.commissionOpForMAC(mac)
}

// ActiveDeployMachines was deleted with the op-event stream (Tier 2
// Phase 2): it had no callers, and post-cutover the op set can hold
// fleet-wide ops (messages fan to all workers), so an unfiltered
// machine-ID list would leak other sites' machine IDs to any future
// caller. The executor-facing getters (PendingPowerOps /
// PendingLifecycleOps) filter by view membership instead.

// GetCurrentDeployOpID returns the operation ID that owns the current
// deploy logs for a machine, from the SQLite current_deploy_ops table.
func (c *Cache) GetCurrentDeployOpID(machineID uuid.UUID) string {
	if c.localDB == nil {
		return ""
	}
	return c.localDB.GetCurrentDeployOp(machineID.String())
}
