// Package commission handles observation events that workers forward
// from deploy/commission agents. For commission_report events it
// decomposes the inventory JSON into NetBox objects (devices,
// interfaces, modules) and persists the raw blob on the machine row.
// The ack back to the worker happens only after BOTH writes succeed —
// a failure anywhere leaves the worker's outbox entry unacked and the
// next reconnect replays it. SyncCommissionData is idempotent so
// retries are safe.
package commission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Store is the subset of store.Store the handler reads and writes.
type Store interface {
	GetMachine(id uuid.UUID) (*model.Machine, error)
	ValidateOperationBinding(opID, machineID uuid.UUID, opType model.OperationType, result json.RawMessage) (bool, error)
	CommitCommissionResult(opID, machineID uuid.UUID, data json.RawMessage) (*store.OpOutcome, bool, error)

	// CompleteOperationWithState / FailOperationOutcome finalize a
	// bmc_reconcile op from its bmc_reconcile_report observation. Both
	// are guarded against reviving a terminal op.
	CompleteOperationWithState(opID uuid.UUID, targetState model.MachineState, result []byte) (*store.OpOutcome, error)
	FailOperationOutcome(opID uuid.UUID, result []byte) (*store.OpOutcome, error)

	// WithPopAdvisoryLock serializes per-pop monitor secret writes.
	// AWS Secrets Manager has no compare-and-swap; without this lock,
	// two hosts in the same pop completing reconcile near-
	// simultaneously would both read-modify-write and one would lose
	// its entry.
	WithPopAdvisoryLock(ctx context.Context, popSlug string, fn func(context.Context) error) error

	// RecordDHCPLeaseEvent folds a dhcp_lease observation into the
	// lease-history ledger (Phase 3 of
	// docs/plans/2026-07-13-dhcp-lease-visibility.md). machineID is
	// nil for a MAC that doesn't resolve to a known machine — legal on
	// the promiscuous OOB segment, not an error.
	RecordDHCPLeaseEvent(ctx context.Context, siteSlug, workerID, event, mac, ip, kind, pool string, machineID *uuid.UUID, expiresAt time.Time) error
}

// NetBoxClient is the subset of netbox.Client the handler calls.
type NetBoxClient interface {
	SyncCommissionData(ctx context.Context, sync netbox.CommissionSync) error
	GetDeviceByID(ctx context.Context, id int) (*netbox.Device, error)

	// Console determination sweep write-back (consolesweep.go): the
	// discovered console config is recorded on the device-type, only
	// when its slagworks_kernel_params is still empty.
	GetDeviceTypeByID(ctx context.Context, id int) (*netbox.DeviceType, error)
	SetDeviceTypeKernelParams(ctx context.Context, id int, kernelParams string) error
}

// CastScanner is the console_cast_events lookup the determination
// sweep resolves nonces against. Satisfied by *store.Store. Nil
// disables sweep resolution (local dev / tests without cast tables).
type CastScanner interface {
	ScanCastEventsForNonce(ctx context.Context, machineID uuid.UUID, nonce string, lookback time.Duration) (bool, error)
}

// MonitorWriter is the AWS Secrets Manager surface for the per-pop
// monitor secret. The deploy_complete and bmc_reconcile_report handlers
// upsert a per-host entry, keyed by NetBox device serial, into
// pop/<pop>/bmc-creds-monitor when a new monitor password was written.
type MonitorWriter interface {
	UpsertMonitorEntry(ctx context.Context, pop, serial string, entry awssecrets.BMCMonitorEntry) error
}

// Invalidator is called after commission data lands so the site's
// snapshot picks up the new SystemUUID / storage-script selection on
// the next push. Nil disables that notification (tests).
type Invalidator interface {
	Invalidate(slug string)
}

// ClaimResolver maps a device to the worker-identity slug(s) of the
// config(s) that claim it. Needed because the registry keys on the
// config slug (the PK / worker identity), NOT the NetBox site slug —
// for a location-scoped config the two differ. Nil falls back to no
// invalidation (the 5-min reconcile catches up). Satisfied by
// *snapshot.ClaimResolver.
type ClaimResolver interface {
	ClaimingConfigs(ctx context.Context, d netbox.Device) ([]string, error)
}

// OpPublisher fans op-stream messages to workers. The commission handler
// completes the commission op via CompleteActiveOperation (a DURABLE
// state write → commissioned), so it publishes op_removed carrying that
// state; the identity rebuild is fired separately via the Invalidator
// below (unchanged). The bmc_reconcile_report handler publishes
// op_removed for the bmc_reconcile op it finalizes (no machine state).
// Nil disables the op-stream publish (tests).
type OpPublisher interface {
	PublishOpRemoved(op model.Operation, machineState model.MachineState)
}

// Handler routes commission_report observations through NetBox +
// Postgres. The ack-after-both-writes contract lives in Handle's
// single error return: the hub converts a non-nil error into an
// rpc_response{error:...} and the worker retries; a nil return
// becomes rpc_response{ok:true} and the worker acks its outbox row.
type Handler struct {
	store      Store
	nb         NetBoxClient
	inv        Invalidator
	resolver   ClaimResolver // nil falls back to no invalidation (5-min reconcile catches up)
	monitorAWS MonitorWriter // nil disables monitor-cred upsert (local dev / tests without AWS)
	casts      CastScanner   // nil disables console-sweep resolution (local dev / tests)
	publisher  OpPublisher   // nil disables op-stream op_removed publish (tests)
	log        *slog.Logger

	// sweepMu serializes console-sweep resolutions. Two jobs: (1) it
	// makes the only-if-empty device-type write-back atomic across
	// concurrent same-type sibling commissions (NetBox has no CAS,
	// but every automated write flows through this one process, so an
	// in-process lock fully serializes the automated path); (2) with
	// sweepInFlight it dedups resolver goroutines when a worker
	// replays the same commission report.
	sweepMu       sync.Mutex
	sweepInFlight map[uuid.UUID]bool
}

func New(st Store, nb NetBoxClient, inv Invalidator) *Handler {
	return &Handler{
		store: st,
		nb:    nb,
		inv:   inv,
		log:   slog.Default(),
	}
}

// SetMonitorWriter wires the AWS Secrets Manager monitor-cred writer.
// Called by slagworksd at startup if AWS credentials are available.
// Nil monitorAWS leaves deploy_complete in "log only" mode: the monitor
// upsert is skipped with a warning, so local-dev pods without IRSA stay
// usable. A bmc_reconcile op (whose whole purpose is the monitor cred)
// fails instead.
func (h *Handler) SetMonitorWriter(w MonitorWriter) { h.monitorAWS = w }

// SetClaimResolver wires the device->config-slug resolver so commission
// invalidations key on the right config (worker identity), not the NetBox
// site slug. Called by slagworksd at startup. Nil leaves commission
// relying on the 5-min reconcile to push the post-commission snapshot.
func (h *Handler) SetClaimResolver(r ClaimResolver) { h.resolver = r }

// SetCastScanner wires the console_cast_events scanner used to resolve
// console-determination sweeps. Called by slagworksd at startup. Nil
// leaves sweeps unresolved (the field stays empty; operator sets it).
func (h *Handler) SetCastScanner(cs CastScanner) { h.casts = cs }

// SetPublisher wires the op-event publisher so the commission op's
// completion drops the op on the worker's stream. Nil leaves the
// commission close relying on the worker's next op_resync to drop the op.
func (h *Handler) SetPublisher(p OpPublisher) { h.publisher = p }

func (h *Handler) SetLogger(l *slog.Logger) { h.log = l }

// ErrMachineNotFound is returned when the observation's MachineID
// doesn't match any machine. Retrying won't help — no other actor is
// going to suddenly create a row with that UUID. Wrapped with
// ws.ErrTerminal so the worker drops the outbox row on receipt.
var ErrMachineNotFound = fmt.Errorf("commission: machine not found: %w", ws.ErrTerminal)

// ErrDeviceNotBound is returned when the machine has no
// netbox_device_id. Commission can't proceed without a NetBox Device
// to mirror into; retrying later might help (device may still be
// getting created), so this is a transient-style error.
var ErrDeviceNotBound = errors.New("commission: machine has no netbox_device_id")

// HandleObservation is the hub-facing entry point. Dispatches by event
// type; returns nil for events the handler doesn't care about (the
// hub still acks them so the worker's outbox stays clean).
func (h *Handler) HandleObservation(ctx context.Context, obs model.Observation) error {
	switch obs.Event {
	case "commission_report":
		return h.handleCommissionReport(ctx, obs)
	case "deploy_complete":
		return h.handleDeployComplete(ctx, obs)
	case "bmc_reconcile_report":
		return h.handleBMCReconcileReport(ctx, obs)
	case "dhcp_lease":
		return h.handleDHCPLease(ctx, obs)
	case "phone_home", "inventory_import":
		// Nothing NetBox-touching to do; ack so the worker drops the
		// outbox row. State transitions for these events happen via
		// the op_update path, not through observations.
		return nil
	default:
		h.log.Debug("unknown observation event, acking anyway",
			"event", obs.Event, "machine_id", obs.MachineID)
		return nil
	}
}

// handleDHCPLease decodes a dhcp_lease observation and folds it into
// the lease-history ledger (Phase 3 of docs/plans/2026-07-13-dhcp-
// lease-visibility.md). SiteSlug/WorkerID come from the observation's
// hub-stamped identity fields (never the worker's payload — see
// model.Observation's doc comment); obs.MachineID is uuid.Nil for a
// MAC that doesn't resolve to a known machine, which is legal on the
// promiscuous OOB segment and is passed through as a nil pointer, NOT
// treated as ErrMachineNotFound the way commission_report/
// deploy_complete would.
func (h *Handler) handleDHCPLease(ctx context.Context, obs model.Observation) error {
	var ev model.DHCPLeaseEvent
	if err := json.Unmarshal(obs.Data, &ev); err != nil {
		return fmt.Errorf("decode dhcp_lease event: %w", err)
	}
	expiresAt, err := time.Parse(time.RFC3339, ev.ExpiresAt)
	if err != nil {
		return fmt.Errorf("decode dhcp_lease expires_at %q: %w", ev.ExpiresAt, err)
	}
	var machineID *uuid.UUID
	if obs.MachineID != uuid.Nil {
		id := obs.MachineID
		machineID = &id
	}
	return h.store.RecordDHCPLeaseEvent(ctx, obs.SiteSlug, obs.WorkerID, ev.Event, ev.MAC, ev.IP, ev.Kind, ev.Pool, machineID, expiresAt)
}

// handleDeployComplete persists the monitor credential the agent's BMC
// reconcile phase produced at the end of deploy: it upserts the per-host
// monitor cred into pop/<pop>/bmc-creds-monitor, keyed by the NetBox
// device serial. This is the cred ipmi_exporter consumes for fleet-wide
// BMC scrapes. slagworks keeps no BMC credential of its own (it drives
// BMCs with the bmc-creds-admin entry), so the monitor cred is the only
// thing a deploy reports.
//
// Empty Data, or a report with no Monitor, is the no-rotation case: the
// agent's reconcile wrote no new password, and the stored monitor entry
// stays valid.
func (h *Handler) handleDeployComplete(ctx context.Context, obs model.Observation) error {
	if _, err := h.store.ValidateOperationBinding(
		obs.OperationID, obs.MachineID, model.OpDeploy, nil,
	); err != nil {
		return reportBindingError("validate deploy operation binding", err)
	}
	if len(obs.Data) == 0 {
		return nil
	}
	var creds model.BMCCreds
	if err := json.Unmarshal(obs.Data, &creds); err != nil {
		return fmt.Errorf("decode BMCCreds: %w", err)
	}
	if creds.Monitor == nil {
		h.log.Info("deploy_complete: no new BMC monitor password to persist",
			"machine_id", obs.MachineID,
			"boot_order_persisted", creds.BootOrderPersisted,
			"leftovers_disabled", creds.LeftoversDisabled)
		return nil
	}

	dev, err := h.machineDevice(ctx, obs.MachineID)
	if err != nil {
		return err
	}
	if dev == nil {
		// Device deleted between deploy completion and our handling.
		// Without it there is no pop to pick the monitor secret; ack
		// (the soft-decommission reconciler handles the orphan) and
		// forgo the monitor cred.
		h.log.Warn("deploy_complete for deleted NetBox device; skipping monitor upsert",
			"machine_id", obs.MachineID)
		return nil
	}

	h.log.Info("deploy_complete: persisting BMC monitor credential",
		"machine_id", obs.MachineID,
		"hostname", dev.Name,
		"serial", dev.Serial,
		"boot_order_persisted", creds.BootOrderPersisted,
		"leftovers_disabled", creds.LeftoversDisabled)

	if h.monitorAWS == nil {
		// Local-dev / no-AWS path: there's no place to write the monitor
		// cred. Surface a clear warning so this isn't silent in prod (the
		// slagworksd startup wires monitorAWS when AWS credentials are
		// available; nil here in production means a misconfigured pod).
		h.log.Warn("deploy_complete: no MonitorWriter configured, monitor cred dropped",
			"machine_id", obs.MachineID, "hostname", dev.Name)
		return nil
	}
	if err := h.upsertMonitorCred(ctx, dev, *creds.Monitor); err != nil {
		// "Monitor secret doesn't exist" is an operator-attention,
		// retry-can't-fix condition (the pop wasn't bootstrapped). Map it
		// to TERMINAL so the worker drops the outbox row instead of
		// replaying forever; the monitor cred is forgone until the pop is
		// bootstrapped and the host redeployed (or bmc-reconciled).
		if errors.Is(err, awssecrets.ErrMonitorSecretMissing) {
			h.log.Warn("deploy_complete: monitor secret missing; monitor cred forgone until pop bootstrapped (dropping outbox row, not retrying)",
				"machine_id", obs.MachineID, "hostname", dev.Name, "err", err)
			return fmt.Errorf("upsert monitor cred for %s: %w: %w", dev.Name, err, ws.ErrTerminal)
		}
		return fmt.Errorf("upsert monitor cred for %s: %w", dev.Name, err)
	}
	return nil
}

// machineDevice resolves an observation's machine to its NetBox device.
// Returns (nil, nil) when the machine is bound to a device NetBox no
// longer has; ErrMachineNotFound (terminal) for an unknown machine;
// ErrDeviceNotBound for a machine with no netbox_device_id.
func (h *Handler) machineDevice(ctx context.Context, machineID uuid.UUID) (*netbox.Device, error) {
	m, err := h.store.GetMachine(machineID)
	if err != nil {
		// gorm returns ErrRecordNotFound rather than (nil, nil), so the
		// "no such machine" path lives here, NOT in `m == nil` below.
		// We map it to ErrMachineNotFound (wrapped with ws.ErrTerminal)
		// so the worker drops the outbox row instead of retrying
		// forever for a row that will never exist.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrMachineNotFound
		}
		return nil, fmt.Errorf("lookup machine %s: %w", machineID, err)
	}
	if m == nil {
		// Defensive: an in-memory store could return (nil, nil).
		return nil, ErrMachineNotFound
	}
	if m.NetBoxDeviceID == nil {
		return nil, ErrDeviceNotBound
	}
	dev, err := h.nb.GetDeviceByID(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return nil, fmt.Errorf("fetch netbox device %d: %w", *m.NetBoxDeviceID, err)
	}
	return dev, nil
}

func (h *Handler) handleCommissionReport(ctx context.Context, obs model.Observation) error {
	alreadyCompleted, err := h.store.ValidateOperationBinding(
		obs.OperationID, obs.MachineID, model.OpCommission, obs.Data,
	)
	if err != nil {
		return reportBindingError("validate commission operation binding", err)
	}
	if alreadyCompleted {
		return nil
	}

	m, err := h.store.GetMachine(obs.MachineID)
	if err != nil {
		// gorm returns ErrRecordNotFound rather than (nil, nil), so the
		// "no such machine" path lives here, NOT in `m == nil` below.
		// We map it to ErrMachineNotFound (wrapped with ws.ErrTerminal)
		// so the worker drops the outbox row instead of retrying
		// forever for a row that will never exist.
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrMachineNotFound
		}
		return fmt.Errorf("lookup machine %s: %w", obs.MachineID, err)
	}
	if m == nil {
		// Defensive: an in-memory store could return (nil, nil); the
		// gorm-backed Store returns ErrRecordNotFound and is caught
		// above. Both paths funnel to the same terminal error.
		return ErrMachineNotFound
	}
	if m.NetBoxDeviceID == nil {
		return ErrDeviceNotBound
	}

	// Decompose the inventory into NetBox-shaped struct.
	sync, err := netbox.CommissionToSync(*m.NetBoxDeviceID, obs.Data)
	if err != nil {
		return fmt.Errorf("decode commission inventory: %w", err)
	}
	sync.SlagworksID = m.ID.String()
	sync.MachineState = string(m.State)

	// Resolve the NetBox site ID from the device (needed for cable
	// resolution inside SyncCommissionData). Fetch once rather than
	// trusting stale state on the Machine row.
	dev, err := h.nb.GetDeviceByID(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return fmt.Errorf("fetch netbox device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev == nil {
		// The device was deleted between the worker's send and our
		// handling. Reconciler will soft-decommission on the next
		// tick; acking here would drop a commission report pointed at
		// a now-ghost machine, which is the least-bad outcome.
		h.log.Warn("commission report for deleted NetBox device, acking",
			"machine_id", obs.MachineID, "device_id", *m.NetBoxDeviceID)
		return nil
	}
	if dev.Site != nil {
		sync.SiteID = dev.Site.ID
	}
	if dev.Role != nil {
		sync.DeviceRole = dev.Role.Slug
	}
	if dev.Name != "" {
		sync.Hostname = dev.Name
	}

	// ORDER MATTERS: write NetBox first. If the NetBox write succeeds
	// but the Postgres write fails, the next retry re-runs NetBox
	// (idempotent) and then Postgres — net zero harm. If we did
	// Postgres first and NetBox failed, retries would keep firing
	// NetBox while commissioned_at is already set, which is fine but
	// gives a confusing window where the machine looks commissioned
	// in slagworks but not in NetBox.
	if err := h.nb.SyncCommissionData(ctx, *sync); err != nil {
		return fmt.Errorf("netbox SyncCommissionData: %w", err)
	}
	// Persist the inventory and complete the exact operation atomically.
	// The store revalidates the binding under a row lock, closing the race
	// between the pre-NetBox check above and this database commit.
	outcome, _, err := h.store.CommitCommissionResult(obs.OperationID, m.ID, obs.Data)
	if err != nil {
		return reportBindingError("commit commission result", err)
	}
	// The commission op left the active set with a DURABLE state write
	// (→ commissioned): drop it on the worker's op stream carrying that
	// state so the overlay tracks it, and the Invalidator below fires the
	// identity rebuild so the baseline catches up. outcome is nil when the
	// op already completed (retry) — publish nothing.
	if outcome != nil && h.publisher != nil {
		h.publisher.PublishOpRemoved(outcome.Op, outcome.MachineState)
	}

	// Invalidate so the next snapshot push includes the new
	// SystemUUID + storage-script selection. Best-effort — if
	// invalidator/resolver is nil (tests, or cold start before pipeline
	// is wired), the next 5-min reconcile will catch up. The invalidator
	// keys on the config slug (worker identity), which for a
	// location-scoped config differs from dev.Site.Slug — so resolve the
	// claiming config rather than invalidating the NetBox site slug.
	if h.inv != nil && h.resolver != nil && dev != nil {
		slugs, err := h.resolver.ClaimingConfigs(ctx, *dev)
		if err != nil {
			h.log.Warn("commission invalidate: claim resolve failed",
				"device_id", dev.ID, "err", err)
		}
		for _, slug := range slugs {
			h.inv.Invalidate(slug)
		}
	}

	// Console determination sweep: when the agent probed candidate
	// ttys (device-type had no slagworks_kernel_params), resolve which
	// nonce reached the cast stream and write the discovery back to
	// the device-type. Async off the ack path — the nonce bytes may
	// still be in flight through the SOL pump, and a miss only means
	// the field stays empty. Background context: the resolution
	// outlives this observation's request scope by design, and the
	// retry ladder bounds the goroutine's lifetime (~25s), so it needs
	// no shutdown hook. startSweepResolution dedups replays of the
	// same commission report (at-least-once outbox delivery).
	if h.casts != nil {
		var sweepCarrier struct {
			ConsoleSweep *model.ConsoleSweep `json:"console_sweep"`
		}
		if err := json.Unmarshal(obs.Data, &sweepCarrier); err == nil &&
			sweepCarrier.ConsoleSweep != nil {
			h.startSweepResolution(m.ID, *m.NetBoxDeviceID, sweepCarrier.ConsoleSweep)
		}
	}
	return nil
}

func reportBindingError(action string, err error) error {
	if errors.Is(err, store.ErrOperationBinding) || errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%s: %w: %w", action, err, ws.ErrTerminal)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// startSweepResolution spawns resolveConsoleSweep unless a resolution
// for the same machine is already running (a worker WS replay of the
// commission report must not fan out duplicate scans).
func (h *Handler) startSweepResolution(machineID uuid.UUID, deviceID int, sweep *model.ConsoleSweep) {
	h.sweepMu.Lock()
	if h.sweepInFlight == nil {
		h.sweepInFlight = map[uuid.UUID]bool{}
	}
	if h.sweepInFlight[machineID] {
		h.sweepMu.Unlock()
		return
	}
	h.sweepInFlight[machineID] = true
	h.sweepMu.Unlock()

	go func() {
		defer func() {
			h.sweepMu.Lock()
			delete(h.sweepInFlight, machineID)
			h.sweepMu.Unlock()
		}()
		h.resolveConsoleSweep(context.Background(), machineID, deviceID, sweep)
	}()
}
