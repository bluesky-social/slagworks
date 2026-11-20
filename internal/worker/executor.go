package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// PowerControllerFactory creates a power.Controller for a machine.
// The worker calls this when it needs to execute a power operation.
//
// Transport selection is explicit, not auto-detected from topology: if
// pduRT is non-nil (the op carries a pdu_runtime envelope, i.e. the
// operator passed `--via pdu`), the factory builds a PDUController over
// pduRT.Outlets. If amtRT is non-nil (amt_runtime: `--via amt`, or global
// auto-selected amt for a kind=amt machine), it builds an AMTController
// against amtRT.Host. Otherwise it builds a BMC controller from the
// machine's BMC IP + rt creds. An empty BMCIP fails the operation rather
// than implicitly choosing PDU; the runtime envelopes are the explicit
// transport signal. At most one of pduRT/amtRT is non-nil (the executor
// fails an op carrying both).
//
// rt carries op-time BMC settings (cipher suite, etc.) resolved by the
// global server when it created the op.
type PowerControllerFactory func(ctx context.Context, m *model.MachineSnapshot, rt model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error)

// BMCReconcileFunc runs a remote BMC user reconcile over IPMI LAN for a
// bmc_reconcile op. monitorPassword is the stored monitor password to
// re-assert; empty generates a fresh one. unconfirmed is the stored entry's
// unconfirmed passwords (bmcreconcile.DesiredUser.Unconfirmed). Production is
// bmcreconcile.ReconcileRemote (ipmitool lanplus); tests stub it. Same
// contract as bmcreconcile.Reconcile: a nil Result means nothing was
// written.
type BMCReconcileFunc func(ctx context.Context, t bmcreconcile.LANTarget, monitorPassword string, unconfirmed []string) (*bmcreconcile.Result, error)

// BMCTimeFunc runs a bmc_reconcile op's time pass (NTP, timezone, clock)
// over Redfish and IPMI LAN. Production is bmcreconcile.ReconcileTime;
// tests stub it.
type BMCTimeFunc func(ctx context.Context, t bmcreconcile.TimeTarget) bmcreconcile.TimeResult

// Claimer atomically claims a pending operation for the worker.
// Returns the claimed operation on success, (nil, nil) when the
// op is no longer pending (already claimed, doesn't exist, etc.),
// or an error on transport failure (WS disconnected, RPC timeout).
//
// Production implementation lives at WSConn.ClaimOp; the executor
// uses an interface so unit tests can inject a stub without
// standing up a full WS server. The claim flows over the WS using
// the connection's authenticated identity — the executor does not
// pass a worker_id field on the wire.
type Claimer interface {
	ClaimOp(ctx context.Context, opID uuid.UUID) (*model.Operation, error)
}

// Executor watches for pending power operations and executes them.
type Executor struct {
	cache        *Cache
	factory      PowerControllerFactory
	bmcReconcile BMCReconcileFunc
	bmcTime      BMCTimeFunc
	claimer      Claimer
	workerID     string
	interval     time.Duration
	log          *slog.Logger
	metrics      *SiteMetrics // nil-safe; claim-outcome counters
}

type ExecutorConfig struct {
	Cache   *Cache
	Factory PowerControllerFactory
	// BMCReconcile runs bmc_reconcile ops. Nil selects
	// bmcreconcile.ReconcileRemote.
	BMCReconcile BMCReconcileFunc
	// BMCTime runs bmc_reconcile ops' time pass. Nil selects
	// bmcreconcile.ReconcileTime.
	BMCTime  BMCTimeFunc
	Claimer  Claimer
	WorkerID string
	Interval time.Duration
	Metrics  *SiteMetrics // optional claim-outcome instrumentation
}

func NewExecutor(cfg ExecutorConfig) *Executor {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.BMCReconcile == nil {
		cfg.BMCReconcile = bmcreconcile.ReconcileRemote
	}
	if cfg.BMCTime == nil {
		cfg.BMCTime = bmcreconcile.ReconcileTime
	}
	return &Executor{
		cache:        cfg.Cache,
		factory:      cfg.Factory,
		bmcReconcile: cfg.BMCReconcile,
		bmcTime:      cfg.BMCTime,
		claimer:      cfg.Claimer,
		workerID:     cfg.WorkerID,
		interval:     cfg.Interval,
		log:          slog.Default(),
		metrics:      cfg.Metrics,
	}
}

func (e *Executor) SetLogger(l *slog.Logger) {
	e.log = l
}

func (e *Executor) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	// wake fires when an op_delta/op_resync adds a pending op, so a new op
	// is driven in ms instead of waiting up to e.interval (5s). The ticker
	// stays as a backstop (a missed wake, or a re-drive after a transient
	// claim failure). nil OpWake (older Cache construction / tests) leaves
	// the ticker as the sole driver.
	var wake <-chan struct{}
	if e.cache != nil {
		wake = e.cache.OpWake
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			e.processOps(ctx)
		case <-wake:
			e.processOps(ctx)
		}
	}
}

func (e *Executor) processOps(ctx context.Context) {
	for _, op := range e.cache.PendingPowerOps() {
		if ctx.Err() != nil {
			return
		}
		e.executeOp(ctx, op)
	}
	for _, op := range e.cache.PendingLifecycleOps() {
		if ctx.Err() != nil {
			return
		}
		e.executeOp(ctx, op)
	}
	// Claims that committed on global but whose reply was lost, where
	// global's claimed row reached the cache after executeOp's rollback and
	// replaced its pending mark (B4).
	for _, op := range e.cache.ReclaimableOps(e.workerID) {
		if ctx.Err() != nil {
			return
		}
		e.executeOp(ctx, op)
	}
}

func (e *Executor) executeOp(ctx context.Context, op model.Operation) {
	log := e.log.With("op_id", op.ID, "type", op.Type, "machine_id", op.MachineID)

	// Mark as claimed locally first to prevent PendingPowerOps and
	// PendingLifecycleOps from returning it on the next tick while
	// we're still executing. This also forgets an earlier unanswered
	// claim: this attempt's outcome decides.
	e.cache.MarkOpClaimed(op.ID.String())

	// Claim the operation via central API
	if err := e.claimOp(ctx, op); err != nil {
		if errors.Is(err, errClaimLost) {
			// Another worker won the race. Keep the local mark at
			// claimed so this worker stops re-attempting the claim
			// every tick. If the mark is ever wrong (op actually
			// still pending globally), the next snapshot push
			// full-replaces c.ops and restores truth — and the
			// global also invalidates the site snapshot on every
			// successful claim, so that push arrives promptly.
			log.Info("operation claimed by another worker, backing off")
			return
		}
		// Transport/store failure: roll back to pending so the next
		// tick — after WS reconnect — retries the claim. Safe even if
		// the claim actually COMMITTED and only the reply was lost:
		// ClaimOperation's idempotent re-claim branch hands back an op
		// this worker already holds (claimed, by us, no phase yet), so
		// the retry resumes the op instead of stalling it (B4). Global's
		// row for such a claim (the reconnect's op_resync, or the late
		// claim's op_delta) can reach the cache after this rollback and
		// replace the pending mark; RollBackClaim records the claim as
		// unanswered so ReclaimableOps still returns the op for the
		// retry.
		log.Warn("failed to claim operation", "err", err)
		e.cache.RollBackClaim(op.ID.String())
		return
	}

	switch op.Type {
	case model.OpCommission, model.OpDeploy:
		e.runLifecycleOp(ctx, op)
	case model.OpBMCReconcile:
		e.runBMCReconcile(ctx, op)
	default:
		e.runPowerOp(ctx, op)
	}
}

// runPowerOp executes a power operation after it has been claimed.
//
// It records the op type as the op's phase before opening the controller,
// as runBMCReconcile does: the phase is the execution evidence the
// orphaned-claim sweep (store.ExpireOrphanedClaims) looks for, and a power
// op can outlive the sweep's 2-minute claimed-no-phase grace while the
// worker is still driving it (bmclib calls of up to 30s each on a slow BMC,
// a PDU cycle's 60s off-window plus up to 150s of polling).
func (e *Executor) runPowerOp(ctx context.Context, op model.Operation) {
	log := e.log.With("op_id", op.ID, "type", op.Type, "machine_id", op.MachineID)

	m := e.cache.MachineByID(op.MachineID)
	if m == nil {
		log.Error("machine not found in cache")
		e.cache.RecordOpUpdate(op.ID.String(), "failed", json.RawMessage(`{"error":"machine not in cache"}`))
		return
	}

	e.cache.UpdateOpPhase(op.ID.String(), string(op.Type))

	ctrl, ok := e.openController(ctx, log, op, m)
	if !ok {
		return
	}
	defer func() { _ = ctrl.Close(ctx) }()

	// Set boot device if specified in operation params. A malformed
	// params object here fails the op cleanly (the runtime extractions
	// above would already have caught it, but decode defensively).
	params, err := model.DecodeParams(op.Params)
	if err != nil {
		e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
		return
	}
	if params.BootDevice != "" {
		var dev power.BootDevice
		switch params.BootDevice {
		case "pxe":
			dev = power.BootPXE
		case "disk":
			dev = power.BootDisk
		default:
			e.cache.RecordOpUpdate(op.ID.String(), "failed",
				marshalError(fmt.Errorf("unknown boot_device %q", params.BootDevice)))
			return
		}
		if e.opRemoved(log, op, "set boot device") {
			return
		}
		if err := ctrl.SetBootDevice(ctx, dev); err != nil {
			log.Error("setting boot device", "err", err, "device", params.BootDevice)
			e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
			return
		}
	}

	if e.opRemoved(log, op, string(op.Type)) {
		return
	}
	log.Info("executing power operation")
	switch op.Type {
	case model.OpPowerOn:
		err = ctrl.PowerOn(ctx)
	case model.OpPowerOff:
		err = ctrl.PowerOff(ctx)
	case model.OpPowerCycle:
		err = ctrl.PowerCycle(ctx)
	case model.OpSetBootDevice:
		err = runSetBootDevice(ctx, ctrl, op.Params)
	default:
		err = fmt.Errorf("unsupported power operation type: %s", op.Type)
	}

	if err != nil {
		log.Error("power operation failed", "err", err)
		e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
		return
	}

	log.Info("power operation completed")
	e.cache.RecordOpUpdate(op.ID.String(), "completed", nil)
}

// runSetBootDevice executes an OpSetBootDevice: arm a boot-device override
// via the BootManager capability. The controller must implement
// power.BootManager — a non-capable controller (bmclib, PDU) fails with an explicit "not supported" so the operator sees why,
// matching the boot-status read's Supported:false. The serial-verify gate
// already ran in runPowerOp before we get here (this IS a mutation).
func runSetBootDevice(ctx context.Context, ctrl power.Controller, rawParams json.RawMessage) error {
	bm, ok := ctrl.(power.BootManager)
	if !ok {
		return fmt.Errorf("boot-device set not supported by this host's power controller; transport=%s", ctrl.Transport())
	}
	params, err := model.DecodeParams(rawParams)
	if err != nil {
		return fmt.Errorf("decode set_boot_device params: %w", err)
	}
	var dev power.BootDevice
	switch params.Device {
	case "pxe":
		dev = power.BootPXE
	case "disk":
		dev = power.BootDisk
	case "default":
		dev = power.BootDefault
	default:
		return fmt.Errorf("invalid boot device %q (want pxe/disk/default)", params.Device)
	}
	return bm.SetBootOverride(ctx, dev, params.Persistent)
}

// openController decodes the op's transport envelopes, enforces the
// transport invariants, builds the controller, and runs the chassis-serial
// gate where it applies. On any failure it records the op as failed and
// returns ok=false; on success the caller owns ctrl and must Close it.
//
// Transport rules:
//   - pdu_runtime and amt_runtime are mutually exclusive.
//   - The BMC path (neither envelope) needs a BMC IP, and is refused for an
//     AMT machine (snapshot AMT): an AMT endpoint must never be driven as
//     IPMI/Redfish (plan D1/D2). Global never creates such an op; this is
//     the worker-side backstop.
//   - The chassis-serial gate is a BMC-identity check: PDUs have no
//     downstream chassis identity and AMT exposes no serial read
//     (power.ErrChassisSerialUnsupported), so it runs on the BMC path only.
func (e *Executor) openController(ctx context.Context, log *slog.Logger, op model.Operation, m *model.MachineSnapshot) (power.Controller, bool) {
	fail := func(err error) (power.Controller, bool) {
		log.Error("power transport", "err", err)
		e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
		return nil, false
	}
	rt, err := extractBMCRuntime(op.Params)
	if err != nil {
		return fail(err)
	}
	pduRT, err := extractPDURuntime(op.Params)
	if err != nil {
		return fail(err)
	}
	amtRT, err := extractAMTRuntime(op.Params)
	if err != nil {
		return fail(err)
	}
	switch {
	case pduRT != nil && amtRT != nil:
		return fail(errors.New("op carries both pdu_runtime and amt_runtime; refusing to guess the transport"))
	case pduRT == nil && amtRT == nil && m.AMT:
		return fail(errors.New("machine's OOB endpoint is Intel AMT (bmc-creds-admin kind=amt) but the op carries no amt_runtime; refusing to drive AMT as IPMI/Redfish (re-issue with --via amt)"))
	case pduRT == nil && amtRT == nil && m.BMCIP == "":
		return fail(errors.New("no BMC configured and no --via pdu"))
	}

	ctrl, err := e.factory(ctx, m, rt, pduRT, amtRT)
	if err != nil {
		return fail(err)
	}
	if ctrl == nil {
		return fail(errors.New("no power controller available"))
	}
	if pduRT == nil && amtRT == nil {
		if err := verifyChassisSerial(ctx, ctrl, rt, log); err != nil {
			_ = ctrl.Close(ctx)
			return fail(err)
		}
	}
	return ctrl, true
}

// runLifecycleOp drives a commission or deploy operation: set PXE boot and
// power cycle the machine. The agent running on the machine reports completion
// back to the site worker asynchronously; the executor's job ends here.
//
// Phases: power_cycle is recorded before the controller is opened and covers
// every BMC/PDU/AMT call up to and including the cycle; pxe_booting follows
// once the cycle has returned. power_cycle comes first because the phase is
// the execution evidence the orphaned-claim sweep (store.ExpireOrphanedClaims)
// looks for, and the controller calls can outlast its 2-minute
// claimed-no-phase grace while the worker is still driving the op (bmclib
// calls of up to 30s each on a slow BMC, a PDU cycle's 60s off-window plus up
// to 150s of polling). Both deploy and commission boot the machine the same
// way and share the boot-prefix spine (lifecycle.go: commissionSpine reuses
// phases 1-6), so both emit these markers. PrepareDeployLogs sets the
// current-op pointer for stale-insert rejection on the per-machine log ring;
// it applies to any booting op, not just deploy.
func (e *Executor) runLifecycleOp(ctx context.Context, op model.Operation) {
	log := e.log.With("op_id", op.ID, "type", op.Type, "machine_id", op.MachineID)

	m := e.cache.MachineByID(op.MachineID)
	if m == nil {
		log.Error("machine not found in cache")
		e.cache.RecordOpUpdate(op.ID.String(), "failed", json.RawMessage(`{"error":"machine not in cache"}`))
		return
	}

	e.cache.PrepareDeployLogs(op.MachineID.String(), op.ID.String())
	e.cache.UpdateOpPhase(op.ID.String(), "power_cycle")

	ctrl, ok := e.openController(ctx, log, op, m)
	if !ok {
		return
	}
	defer func() { _ = ctrl.Close(ctx) }()

	if e.opRemoved(log, op, "set PXE boot device") {
		return
	}
	if err := ctrl.SetBootDevice(ctx, power.BootPXE); err != nil {
		log.Error("setting PXE boot device", "err", err)
		e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
		return
	}

	state, err := ctrl.PowerStatus(ctx)
	if err != nil {
		log.Warn("could not determine power state, assuming on", "err", err)
		state = power.StateOn
	}

	if e.opRemoved(log, op, "power on or cycle") {
		return
	}
	if state == power.StateOff {
		log.Info("PXE boot set, powering on", "op_type", op.Type)
		if err := ctrl.PowerOn(ctx); err != nil {
			log.Error("power on failed", "err", err)
			e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
			return
		}
	} else {
		log.Info("PXE boot set, cycling power", "op_type", op.Type)
		if err := ctrl.PowerCycle(ctx); err != nil {
			log.Error("power cycle failed", "err", err)
			e.cache.RecordOpUpdate(op.ID.String(), "failed", marshalError(err))
			return
		}
	}

	// The agent on the machine will report back completion via the reporter.
	// Mark op as running so the executor doesn't re-drive it on the next tick.
	// pxe_booting keeps the watch TUI's lifecycle pane moving until the
	// target's first DHCP or the agent's first report.
	e.cache.UpdateOpPhase(op.ID.String(), "pxe_booting")
	e.cache.MarkOpStatus(op.ID.String(), model.OpRunning)
	log.Info("machine booting to PXE, waiting for agent report", "op_type", op.Type)
}

// opRemoved re-reads the op layer, the sole authority for actuation, and
// reports whether op has left it since the claim. The executor works from
// the copy of the op it took before the claim, but the WS read loop, a
// separate goroutine, can apply global's op_removed (or an op_resync
// without the op) while the executor is inside a BMC round trip: the
// operator aborted the machine, or a NetBox drift fail or a timeout sweep
// failed the op. The executor calls this before each write to the host or
// its BMC and, when the op is gone, stops without reporting: global already
// holds the op's terminal row. A write that went out before the removal is
// not undone.
func (e *Executor) opRemoved(log *slog.Logger, op model.Operation, next string) bool {
	if e.cache.GetOperation(op.ID.String()) != nil {
		return false
	}
	log.Warn("global ended the operation while this worker was driving it (aborted, or failed by a drift check or a timeout sweep); stopping before the next host write",
		"next", next)
	return true
}

// errClaimLost signals the op was already claimed (another worker
// won the race) — distinguishable from transport/store errors so
// executeOp can keep the local mark at claimed instead of rolling
// back to pending and retrying forever.
var errClaimLost = errors.New("operation already claimed or not pending")

// claimOp attempts to atomically claim the op via the Claimer
// (production: the WS RPC ClaimOp on the active site-worker
// connection). Returns an error in three shapes:
//
//   - hard transport error (WS not connected, RPC timeout): the
//     executor leaves the op pending so the next tick — after WS
//     reconnect — can retry.
//   - errClaimLost ("already claimed" / "not pending"; Claimer
//     returned nil/nil): the executor keeps the local op marked
//     claimed so it stops re-attempting; the next snapshot push
//     carries the authoritative state either way.
//   - any other Claimer.ClaimOp error: bubbled up.
//
// Claim shape change: the connection's authenticated worker_id is
// sourced server-side, so we don't pass it on the wire. See
// commit message + design discussion for the WS migration.
func (e *Executor) claimOp(ctx context.Context, op model.Operation) error {
	if e.claimer == nil {
		return fmt.Errorf("executor: no Claimer configured")
	}
	// Bound the claim RPC. The Claimer (WSConn.ClaimOp in production)
	// blocks on a response from the hub; without a deadline here, a
	// silent peer (TCP-up but app-stuck, or a hub bug that drops the
	// reply) would hang the entire executor loop forever, blocking
	// every subsequent op on this worker. 30s is generous: the hub's
	// claim is a single PG round-trip, typically sub-millisecond.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	claimed, err := e.claimer.ClaimOp(ctx, op.ID)
	if err != nil {
		e.metrics.claim(ClaimErr)
		return fmt.Errorf("claim_op: %w", err)
	}
	if claimed == nil {
		e.metrics.claim(ClaimLost)
		return errClaimLost
	}
	e.metrics.claim(ClaimWon)
	return nil
}

func marshalError(err error) json.RawMessage {
	return model.OpResult{Error: err.Error()}.Marshal()
}
