// Package mirror owns NetBox-webhook-and-reconcile writes to slagworks
// Postgres identity tables. It enforces the invariants from the SSoT
// spec: serial required and unique, site slug immutable once managed.
//
// On NetBox device deletion the corresponding Machine row is hard-deleted
// once the machine is idle (cascading to machine_interfaces; child rows in
// operations/log_envelopes are cleaned up by the deletion path); a machine
// with an operation in flight is kept, see OnDeviceDeleted. On NetBox
// device re-creation with a serial that matches an existing row, the row
// is rebound (its netbox_device_id is updated); state is preserved.
//
// The webhook handler (internal/netbox/webhook.go) and the reconciler
// (internal/snapshot/reconcile.go) both delegate here so the
// write-to-Postgres logic lives in exactly one place.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
)

// Device is the small subset of a NetBox Device the mirror needs. It's
// declared here rather than imported from internal/netbox to avoid a
// dependency cycle (internal/netbox/webhook.go imports the mirror to
// dispatch events + compare against the sentinel errors below).
type Device struct {
	ID     int
	Name   string
	Serial string
	// SiteSlug is the device's NetBox site slug (e.g. "pop2"). Used only
	// for audit logging now — membership and the invalidation key both
	// key on ConfigSlug, set by the caller after resolving the claim.
	SiteSlug string
	// ConfigSlug is the worker-identity slug of the site_config that
	// claims this device (e.g. "pop2-old-cage"). The caller resolves the
	// claim before handing the Device to the mirror: the reconciler sets
	// it to the cfg it's iterating, the webhook handler resolves it via a
	// ClaimResolver. It is the key the registry/dispatcher expect — NOT
	// SiteSlug, which for a location-scoped config differs from the PK.
	ConfigSlug string
}

var (
	// ErrSerialRequired is returned when a NetBox Device has an empty
	// serial. slagworks treats the Device as not-ready-to-manage until
	// the operator fills the serial in NetBox.
	ErrSerialRequired = errors.New("netbox device has empty serial; skipping")

	// ErrSlugImmutable is returned when a NetBox Site slug rename is
	// observed for a site slagworks is managing. The caller (webhook
	// handler) should log this loudly and surface it to operators.
	ErrSlugImmutable = errors.New("netbox site slug change rejected: slug is managed by slagworks")
)

// Invalidator is the subset of snapshot.Registry the mirror needs.
type Invalidator interface {
	Invalidate(slug string)
}

// OpPublisher is the op-event publish surface (satisfied by *ws.Hub, same
// seam the api server uses). The B7 drift paths fail in-flight ops in the
// DB; without publishing op_removed per op, a worker holds a zombie
// ACTIVE op until reconnect — and under the fail-closed boot rule an
// active op is exactly what authorizes actuation. Nil disables the
// publish (tests): the ops still fail in the DB and the expiry-sweep /
// reconnect resync eventually converge, but a live worker's window stays
// open, so production always wires this.
type OpPublisher interface {
	PublishOpRemoved(op model.Operation, machineState model.MachineState)
}

// Store is the subset of the slagworks store the mirror writes to.
type Store interface {
	CreateMachine(m *model.Machine) error
	GetMachine(id uuid.UUID) (*model.Machine, error)
	GetMachineByNetBoxDeviceID(id int) (*model.Machine, error)
	GetMachineByNetBoxSerial(serial string) (*model.Machine, error)
	UpdateMachine(m *model.Machine) error
	// SetMachineNetBoxDeviceID writes ONLY netbox_device_id. The rebind path
	// uses this instead of full-row UpdateMachine so it can't clobber columns
	// the mirror doesn't own (e.g. state, locked) with a stale in-memory value.
	SetMachineNetBoxDeviceID(id uuid.UUID, netboxDeviceID int) error
	// DeleteMachineIfIdle hard-deletes the machine and its op history
	// unless it has an active op, checking and deleting in one transaction
	// that a concurrent op create cannot slip into. Returns false (nothing
	// deleted) when an active op exists, true when the row is gone.
	DeleteMachineIfIdle(id uuid.UUID) (bool, error)
	// FailInFlightOpsForMachine fails every non-terminal op on the
	// machine (B7: external NetBox drift mid-operation). Returns the
	// post-mutation outcomes (failed row + the durable machine state the
	// fail wrote) so the mirror publishes op_removed per op.
	FailInFlightOpsForMachine(machineID uuid.UUID, reason string) ([]store.OpOutcome, error)
	// AnySiteConfigForNetBoxSite reports whether slagworks manages the
	// given NetBox site. Used only by OnSiteUpdated's slug-immutability
	// guard, which is a genuine netbox_site question (not a claim).
	AnySiteConfigForNetBoxSite(netboxSite string) (bool, error)
}

// Mirror applies NetBox events (from webhook or reconcile) to the
// slagworks store.
type Mirror struct {
	st  Store
	inv Invalidator
	pub OpPublisher // nil disables op-stream publish (tests)
	log *slog.Logger
}

func New(st Store, inv Invalidator) *Mirror {
	return &Mirror{st: st, inv: inv, log: slog.Default()}
}

func (m *Mirror) SetLogger(log *slog.Logger) { m.log = log }

// SetPublisher wires the op-event publisher so B7 drift-fails drop the
// affected ops on the workers' op stream (op_removed per outcome).
// Called by slagworksd at startup, after the hub exists.
func (m *Mirror) SetPublisher(p OpPublisher) { m.pub = p }

// publishRemoved fans op_removed per drift-failed op, carrying the durable
// machine state the fail wrote (consistent with the expiry sweeps). No-op
// when no publisher is wired.
func (m *Mirror) publishRemoved(outcomes []store.OpOutcome) {
	if m.pub == nil {
		return
	}
	for _, o := range outcomes {
		m.pub.PublishOpRemoved(o.Op, o.MachineState)
	}
}

// OnDeviceCreated handles `device.created` (and the reconcile-time
// equivalent). Rebinds an existing machine row matching the serial (just
// updates netbox_device_id; state and other fields are preserved) or
// creates a fresh row. Serial is required.
//
// Membership is NOT re-checked here: by the time a Device reaches the
// mirror, the caller has resolved the claim and stamped d.ConfigSlug (the
// reconciler scopes inline; the webhook handler resolves via a
// ClaimResolver). The mirror trusts that single predicate and keys its
// invalidation on ConfigSlug.
func (m *Mirror) OnDeviceCreated(ctx context.Context, d Device) error {
	if d.Serial == "" {
		m.log.Warn("netbox device missing serial; skipping", "device_id", d.ID, "name", d.Name)
		return ErrSerialRequired
	}
	existing, err := m.st.GetMachineByNetBoxSerial(d.Serial)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("lookup by serial: %w", err)
	}
	if existing != nil {
		// Refuse to silently steal a machine row from an active
		// device. Hard-delete on OnDeviceDeleted means a serial that
		// matches an existing row whose NetBoxDeviceID is set to a
		// DIFFERENT id indicates a duplicate serial in NetBox — bug
		// in the operator's data, not a legitimate rebind. Surface it.
		if existing.NetBoxDeviceID != nil && *existing.NetBoxDeviceID != d.ID {
			return fmt.Errorf("serial %s is already bound to active device %d; refusing to rebind to %d",
				d.Serial, *existing.NetBoxDeviceID, d.ID)
		}
		// Write ONLY netbox_device_id — NOT a full-row UpdateMachine(Save).
		// A full-row save here clobbers columns the mirror doesn't own with the
		// stale values in `existing` (loaded above), racing any concurrent
		// writer of those columns (state transitions, lock).
		if err := m.st.SetMachineNetBoxDeviceID(existing.ID, d.ID); err != nil {
			return fmt.Errorf("rebind machine: %w", err)
		}
		m.log.Info("rebound machine to new netbox device id",
			"uuid", existing.ID, "serial", d.Serial, "device_id", d.ID)
		m.inv.Invalidate(d.ConfigSlug)
		return nil
	}
	nbID := d.ID
	mach := &model.Machine{
		ID:             uuid.New(),
		NetBoxDeviceID: &nbID,
		NetBoxSerial:   d.Serial,
		State:          model.MachineStateNew,
	}
	if err := m.st.CreateMachine(mach); err != nil {
		return fmt.Errorf("create machine: %w", err)
	}
	m.log.Info("enrolled new machine",
		"uuid", mach.ID, "serial", d.Serial, "device_id", d.ID, "site", d.SiteSlug, "config", d.ConfigSlug)
	m.inv.Invalidate(d.ConfigSlug)
	return nil
}

// OnDeviceUpdated handles `device.updated`. Slagworks doesn't mirror
// Device fields into Postgres — the snapshot builder joins NetBox live
// at rebuild time. Just invalidate so the next rebuild picks up the
// new state. Claim membership is the caller's responsibility (it stamps
// d.ConfigSlug); an unclaimed device never reaches here.
func (m *Mirror) OnDeviceUpdated(ctx context.Context, d Device) error {
	m.inv.Invalidate(d.ConfigSlug)
	return nil
}

// OnDeviceDeleted hard-deletes the machine row when its NetBox device is
// deleted and the machine is idle. Cascades to machine_interfaces; the
// store also deletes its operations and log_envelopes. Called with the
// device_id and the claiming config slug because the Device object itself
// is gone. configSlug keys the invalidation; it may be empty when the
// caller couldn't resolve a claim (the row is still deleted by device_id —
// the snapshot just refreshes on the next reconcile tick).
//
// B7: a NetBox delete landing mid-operation is a blow-up-the-world event,
// not a cleanup. Fail the in-flight ops (freezing the machine in `failed`),
// keep the row and its op history for the operator to inspect, and alarm.
// The row is NOT deleted this pass — once the machine is idle, the next
// reconcile sweep confirms the device is still gone and removes it.
func (m *Mirror) OnDeviceDeleted(ctx context.Context, deviceID int, configSlug string) error {
	existing, err := m.st.GetMachineByNetBoxDeviceID(deviceID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		m.log.Info("device deleted with no corresponding machine; ignoring",
			"device_id", deviceID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup machine by device id %d: %w", deviceID, err)
	}
	if failed, err := m.failInFlightOnDelete(existing.ID, deviceID, configSlug); err != nil || failed {
		return err
	}
	// An operator request already past its own checks when the device was
	// deleted can still create an op after failInFlightOnDelete looked.
	// DeleteMachineIfIdle re-checks under the machine row lock and refuses
	// to delete a machine with an active op; that op then gets the same B7
	// treatment instead of vanishing with the row after the operator was
	// handed its ID.
	deleted, err := m.st.DeleteMachineIfIdle(existing.ID)
	if err != nil {
		return fmt.Errorf("delete machine: %w", err)
	}
	if !deleted {
		failed, err := m.failInFlightOnDelete(existing.ID, deviceID, configSlug)
		if err != nil {
			return err
		}
		if !failed {
			// The op finished before it could be failed. The row stays
			// for the next reconcile sweep, but the device is gone from
			// NetBox, so the snapshot still needs rebuilding.
			m.inv.Invalidate(configSlug)
		}
		return nil
	}
	m.log.Info("deleted machine on netbox device deletion",
		"uuid", existing.ID, "device_id", deviceID)
	m.inv.Invalidate(configSlug)
	return nil
}

// failInFlightOnDelete is OnDeviceDeleted's B7 branch: it fails the
// machine's in-flight ops, drops them on the workers' op stream and
// invalidates the snapshot, reporting whether there were any. The caller
// keeps the row whenever this fails an op.
func (m *Mirror) failInFlightOnDelete(machineID uuid.UUID, deviceID int, configSlug string) (bool, error) {
	outcomes, err := m.st.FailInFlightOpsForMachine(machineID,
		"netbox device deleted mid-operation")
	if err != nil {
		return false, fmt.Errorf("fail in-flight ops for machine %s: %w", machineID, err)
	}
	if len(outcomes) == 0 {
		return false, nil
	}
	m.log.Error("netbox device deleted MID-OPERATION; in-flight ops failed, machine row retained for inspection",
		"uuid", machineID, "device_id", deviceID, "ops_failed", len(outcomes))
	// Drop the failed ops on the workers' op stream — without this a
	// worker keeps a zombie active op until reconnect, which under the
	// fail-closed boot rule is what authorizes actuation. The identity
	// rebuild below delivers the pulled-down machine state.
	m.publishRemoved(outcomes)
	m.inv.Invalidate(configSlug)
	return true, nil
}

// OnDeviceExternalDrift handles an externally-driven change to a
// topology field slagworks never writes (site, location, device_type,
// rack — detected by the webhook handler's prechange/postchange diff).
// If the machine has an operation in flight, the op was created and
// validated against data that just changed under it: blow up the world
// (fail the ops, freeze the machine, alarm) rather than let it keep
// running (B7). Drift on an idle machine is fine — the change simply
// flows into the next snapshot.
//
// Fields slagworks writes itself (serial, name, status, custom_fields,
// tags, asset_tag, primary_ip4 — the commission/deploy sync set) are
// exempted at the detection layer, so slagworks' own mid-op writes
// never trip this.
func (m *Mirror) OnDeviceExternalDrift(ctx context.Context, deviceID int, fields []string, configSlug string) error {
	existing, err := m.st.GetMachineByNetBoxDeviceID(deviceID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lookup machine by device id %d: %w", deviceID, err)
	}
	outcomes, err := m.st.FailInFlightOpsForMachine(existing.ID,
		fmt.Sprintf("netbox device changed mid-operation (fields: %s)", strings.Join(fields, ", ")))
	if err != nil {
		return fmt.Errorf("fail in-flight ops for machine %s: %w", existing.ID, err)
	}
	if len(outcomes) > 0 {
		m.log.Error("netbox device changed MID-OPERATION; in-flight ops failed",
			"uuid", existing.ID, "device_id", deviceID,
			"changed_fields", fields, "ops_failed", len(outcomes))
		// Same zombie-op rule as OnDeviceDeleted: publish op_removed per
		// failed op so workers drop them now, not at reconnect.
		m.publishRemoved(outcomes)
		m.inv.Invalidate(configSlug)
	}
	return nil
}

// OnSiteUpdated enforces slug immutability for managed sites. If the
// slug changed and the old NetBox site is managed by any config, return
// ErrSlugImmutable so the caller can alert the operator. Unmanaged sites
// are free to rename. oldSlug/newSlug are NetBox site slugs (from the
// site webhook), so this keys on netbox_site, not the config PK.
func (m *Mirror) OnSiteUpdated(ctx context.Context, oldSlug, newSlug string) error {
	if oldSlug == newSlug {
		return nil
	}
	ok, err := m.st.AnySiteConfigForNetBoxSite(oldSlug)
	if err != nil {
		return err
	}
	if ok {
		m.log.Error("site slug rename rejected",
			"old", oldSlug, "new", newSlug)
		return ErrSlugImmutable
	}
	return nil
}
