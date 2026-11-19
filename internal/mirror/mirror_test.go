package mirror

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
)

// fakeStore implements the Store interface. Minimal in-memory impl
// that mirrors the real store's invariants (unique serial, lookup
// by device id / serial).
type fakeStore struct {
	mu       sync.Mutex
	machines map[uuid.UUID]*model.Machine
	bySerial map[string]uuid.UUID
	byDevice map[int]uuid.UUID
	configs  map[string]*model.SiteConfig

	// inFlightOps simulates machines with active operations: the
	// per-machine count is what FailInFlightOpsForMachine "fails"
	// (returns then zeroes). failedOps records the reasons.
	inFlightOps map[uuid.UUID]int64
	failedOps   map[uuid.UUID]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		machines:    map[uuid.UUID]*model.Machine{},
		bySerial:    map[string]uuid.UUID{},
		byDevice:    map[int]uuid.UUID{},
		configs:     map[string]*model.SiteConfig{},
		inFlightOps: map[uuid.UUID]int64{},
		failedOps:   map[uuid.UUID]string{},
	}
}

func (f *fakeStore) FailInFlightOpsForMachine(machineID uuid.UUID, reason string) ([]store.OpOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.inFlightOps[machineID]
	if n == 0 {
		return nil, nil
	}
	f.failedOps[machineID] = reason
	f.inFlightOps[machineID] = 0
	// Mirror the real store's shape: each fail returns the failed
	// post-mutation row + the durable failed state (a claimed/running
	// lifecycle op pulls the machine to failed).
	outs := make([]store.OpOutcome, 0, n)
	for range n {
		outs = append(outs, store.OpOutcome{
			Op: model.Operation{
				ID: uuid.New(), MachineID: machineID,
				Type: model.OpDeploy, Status: model.OpFailed, Version: 1,
			},
			MachineState: model.MachineStateFailed,
		})
	}
	return outs, nil
}

func (f *fakeStore) CreateMachine(m *model.Machine) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.NetBoxSerial == "" {
		return errors.New("NetBoxSerial required")
	}
	if _, exists := f.bySerial[m.NetBoxSerial]; exists {
		return errors.New("duplicate serial")
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.State == "" {
		m.State = model.MachineStateNew
	}
	f.machines[m.ID] = m
	f.bySerial[m.NetBoxSerial] = m.ID
	if m.NetBoxDeviceID != nil {
		f.byDevice[*m.NetBoxDeviceID] = m.ID
	}
	return nil
}

func (f *fakeStore) GetMachine(id uuid.UUID) (*model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return nil, errors.New("not found")
	}
	cc := *m
	return &cc, nil
}

func (f *fakeStore) GetMachineByNetBoxDeviceID(id int) (*model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, ok := f.byDevice[id]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cc := *f.machines[uid]
	return &cc, nil
}

func (f *fakeStore) GetMachineByNetBoxSerial(serial string) (*model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	uid, ok := f.bySerial[serial]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	cc := *f.machines[uid]
	return &cc, nil
}

// SetMachineNetBoxDeviceID updates ONLY netbox_device_id, leaving every other
// column untouched — mirroring the real store's column-scoped Update.
// This is what makes the rebind path safe against clobbering columns the
// mirror doesn't own.
func (f *fakeStore) SetMachineNetBoxDeviceID(id uuid.UUID, netboxDeviceID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.machines[id]
	if !ok {
		return errors.New("not found")
	}
	if existing.NetBoxDeviceID != nil {
		delete(f.byDevice, *existing.NetBoxDeviceID)
	}
	nb := netboxDeviceID
	existing.NetBoxDeviceID = &nb // mutate in place: all other fields preserved
	f.byDevice[netboxDeviceID] = id
	return nil
}

// DeleteMachineIfIdle mirrors the real store: a machine with in-flight ops
// is left alone (false), an absent machine counts as deleted (true).
func (f *fakeStore) DeleteMachineIfIdle(id uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return true, nil
	}
	if f.inFlightOps[id] > 0 {
		return false, nil
	}
	delete(f.machines, id)
	delete(f.bySerial, m.NetBoxSerial)
	if m.NetBoxDeviceID != nil {
		delete(f.byDevice, *m.NetBoxDeviceID)
	}
	return true, nil
}

// AnySiteConfigForNetBoxSite reports whether any config mirrors the given
// NetBox site. addSite stores a config whose netbox_site equals its slug
// (the pop1/pop3 shape), so this walks the values matching on NetBoxSite.
func (f *fakeStore) AnySiteConfigForNetBoxSite(netboxSite string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.configs {
		if c.NetBoxSite == netboxSite {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) addSite(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs[slug] = &model.SiteConfig{
		Slug: slug, NetBoxSite: slug, EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}
}

// fakeInvalidator records Invalidate calls.
type fakeInvalidator struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeInvalidator) Invalidate(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[slug]++
}

func (f *fakeInvalidator) count(slug string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[slug]
}

// Test helpers.

func withSite(t *testing.T) (*fakeStore, *fakeInvalidator, *Mirror) {
	t.Helper()
	st := newFakeStore()
	st.addSite("pop3")
	inv := &fakeInvalidator{}
	return st, inv, New(st, inv)
}

// mkDevice builds a Device as the caller (reconciler/webhook) would hand
// it to the mirror: claim already resolved, so ConfigSlug is stamped. The
// helper uses the site slug as the config slug too (the pop1/pop3 shape);
// tests that need them to differ build the Device literal directly.
func mkDevice(id int, name, serial string) Device {
	return Device{ID: id, Name: name, Serial: serial, SiteSlug: "pop3", ConfigSlug: "pop3"}
}

func TestOnDeviceCreatedEnrollsWithSerial(t *testing.T) {
	st, inv, m := withSite(t)
	err := m.OnDeviceCreated(context.Background(), mkDevice(1, "host-a", "SN-A"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMachineByNetBoxDeviceID(1)
	if err != nil {
		t.Fatal(err)
	}
	if got.NetBoxSerial != "SN-A" || got.State != model.MachineStateNew {
		t.Fatalf("got %+v", got)
	}
	if inv.count("pop3") != 1 {
		t.Fatal("expected pop3 invalidation")
	}
}

func TestOnDeviceCreatedRejectsEmptySerial(t *testing.T) {
	st, inv, m := withSite(t)
	err := m.OnDeviceCreated(context.Background(), mkDevice(1, "host-a", ""))
	if !errors.Is(err, ErrSerialRequired) {
		t.Fatalf("expected ErrSerialRequired, got %v", err)
	}
	if _, err := st.GetMachineByNetBoxDeviceID(1); err == nil {
		t.Fatal("machine should not have been created")
	}
	if inv.count("pop3") != 0 {
		t.Fatal("no invalidation expected")
	}
}

// TestOnDeviceCreatedInvalidatesConfigSlugNotSite is the regression for
// the location-scoping invalidation bug: the mirror must invalidate the
// config slug (worker identity), NOT the NetBox site slug. For a
// location-scoped config the two differ (e.g. pop2-old-cage vs pop2);
// invalidating "pop2" would spin a phantom site-state and never refresh
// the real worker's snapshot.
func TestOnDeviceCreatedInvalidatesConfigSlugNotSite(t *testing.T) {
	st := newFakeStore()
	inv := &fakeInvalidator{}
	m := New(st, inv)
	// Device claimed by config "pop2-old-cage", whose NetBox site is "pop2".
	d := Device{ID: 1, Name: "host-a", Serial: "SN-A", SiteSlug: "pop2", ConfigSlug: "pop2-old-cage"}
	if err := m.OnDeviceCreated(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachineByNetBoxDeviceID(1); err != nil {
		t.Fatalf("device should have enrolled: %v", err)
	}
	if inv.count("pop2-old-cage") != 1 {
		t.Fatalf("expected invalidation on config slug pop2-old-cage, got %d", inv.count("pop2-old-cage"))
	}
	if inv.count("pop2") != 0 {
		t.Fatalf("must NOT invalidate the NetBox site slug pop2, got %d", inv.count("pop2"))
	}
}

// TestOnDeviceCreatedRebindsBySerial covers the "old NetBox device was
// deleted, operator recreates with a new device id but same serial"
// case. The mirror should rebind the existing Machine row to the new
// netbox_device_id without changing its state.
func TestOnDeviceCreatedRebindsBySerial(t *testing.T) {
	st, inv, m := withSite(t)

	// Pre-create a machine with serial SN-R, currently unbound (the
	// old netbox_device_id has been deleted).
	oldID := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: oldID, NetBoxSerial: "SN-R", State: model.MachineStateRunning,
	}); err != nil {
		t.Fatal(err)
	}

	// Operator recreates the Device with a new NetBox id but same serial.
	if err := m.OnDeviceCreated(context.Background(), mkDevice(99, "host-r", "SN-R")); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMachineByNetBoxDeviceID(99)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != oldID {
		t.Fatalf("rebind must preserve UUID; got %s want %s", got.ID, oldID)
	}
	// State is preserved across rebind — unlike the old soft-decommission
	// flow which reset to `new`.
	if got.State != model.MachineStateRunning {
		t.Fatalf("state %s, want preserved running", got.State)
	}
	if inv.count("pop3") != 1 {
		t.Fatal("expected pop3 invalidation")
	}
}

// TestOnDeviceCreatedRebindPreservesNonOwnedColumns: the reconciler
// (webhook-loss safety net) re-feeds EVERY already-bound device through
// OnDeviceCreated on each tick, so the rebind path must write ONLY
// netbox_device_id. A full-row gorm Save of the loaded `existing`
// struct would race and revert concurrent writes to columns the mirror
// doesn't own (it once reverted a freshly written BMC password this way).
func TestOnDeviceCreatedRebindPreservesNonOwnedColumns(t *testing.T) {
	st, _, m := withSite(t)

	id := uuid.New()
	boundDeviceID := 7
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxSerial: "SN-KEEP", State: model.MachineStateRunning,
		NetBoxDeviceID: &boundDeviceID, Locked: true, DesiredOS: "ubuntu-24.04",
	}); err != nil {
		t.Fatal(err)
	}

	// Reconciler re-feeds the SAME already-bound device (device_id unchanged).
	if err := m.OnDeviceCreated(context.Background(), mkDevice(7, "host-keep", "SN-KEEP")); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetMachineByNetBoxDeviceID(7)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.MachineStateRunning || !got.Locked || got.DesiredOS != "ubuntu-24.04" {
		t.Errorf("rebind touched non-owned columns: state=%s locked=%v desired_os=%q",
			got.State, got.Locked, got.DesiredOS)
	}
}

// TestOnDeviceUpdatedEnrollsUntrackedDevice: a device.updated for a claimed
// device that has a serial but no machine row must enroll it, the rule the
// reconciler already applies to untracked devices. Two ordinary operator
// actions produce that event: filling in the serial of a device whose
// device.created was skipped with ErrSerialRequired, and moving a serialled
// device into a claimed site or location. The webhook handler routes both to
// OnDeviceUpdated, which used to only invalidate, so the device stayed out of
// the machine list and the site snapshot until the next reconcile tick.
func TestOnDeviceUpdatedEnrollsUntrackedDevice(t *testing.T) {
	ctx := context.Background()

	t.Run("serial filled in after a skipped create", func(t *testing.T) {
		st, inv, m := withSite(t)
		if err := m.OnDeviceCreated(ctx, mkDevice(5, "host-5", "")); !errors.Is(err, ErrSerialRequired) {
			t.Fatalf("setup: want ErrSerialRequired, got %v", err)
		}
		if err := m.OnDeviceUpdated(ctx, mkDevice(5, "host-5", "SN-5")); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetMachineByNetBoxDeviceID(5)
		if err != nil {
			t.Fatalf("device 5 now has a serial and is claimed, but device.updated did not enroll it: %v", err)
		}
		if got.NetBoxSerial != "SN-5" || got.State != model.MachineStateNew {
			t.Fatalf("enrolled row = %+v; want serial SN-5 in state new", got)
		}
		if inv.count("pop3") != 1 {
			t.Fatalf("pop3 invalidations = %d; want 1", inv.count("pop3"))
		}
	})

	t.Run("serialled device moved into a claimed scope", func(t *testing.T) {
		st, _, m := withSite(t)
		// The mirror has never seen device 6: it lived in an unclaimed
		// location until this update moved it into pop3's scope.
		if err := m.OnDeviceUpdated(ctx, mkDevice(6, "host-6", "SN-6")); err != nil {
			t.Fatal(err)
		}
		if _, err := st.GetMachineByNetBoxDeviceID(6); err != nil {
			t.Fatalf("device 6 moved into a claimed scope with a serial, but device.updated did not enroll it: %v", err)
		}
	})
}

// TestOnDeviceUpdatedWithNothingToEnrollOnlyInvalidates pins the paths the
// enrollment check leaves alone: an update to a tracked device keeps its row
// as is, and an update to a device that still has no serial creates nothing.
func TestOnDeviceUpdatedWithNothingToEnrollOnlyInvalidates(t *testing.T) {
	ctx := context.Background()

	t.Run("tracked device", func(t *testing.T) {
		st, inv, m := withSite(t)
		id := uuid.New()
		nbID := 8
		if err := st.CreateMachine(&model.Machine{
			ID: id, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-8", State: model.MachineStateRunning, Locked: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := m.OnDeviceUpdated(ctx, mkDevice(8, "host-8-renamed", "SN-8")); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetMachineByNetBoxDeviceID(8)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != id || got.State != model.MachineStateRunning || !got.Locked {
			t.Fatalf("update touched the tracked row: %+v", got)
		}
		if len(st.machines) != 1 {
			t.Fatalf("machines = %d; want the one tracked row", len(st.machines))
		}
		if inv.count("pop3") != 1 {
			t.Fatalf("pop3 invalidations = %d; want 1", inv.count("pop3"))
		}
	})

	t.Run("still no serial", func(t *testing.T) {
		st, inv, m := withSite(t)
		if err := m.OnDeviceUpdated(ctx, mkDevice(9, "host-9", "")); err != nil {
			t.Fatalf("an update to an unserialled device is not an error, got %v", err)
		}
		if _, err := st.GetMachineByNetBoxDeviceID(9); err == nil {
			t.Fatal("a device without a serial must not be enrolled")
		}
		if inv.count("pop3") != 1 {
			t.Fatalf("pop3 invalidations = %d; want 1", inv.count("pop3"))
		}
	})
}

// TestOnDeviceDeletedHardDeletes covers the new hard-delete semantics:
// when a NetBox device is deleted, the Machine row is removed from the
// store outright.
func TestOnDeviceDeletedHardDeletes(t *testing.T) {
	st, inv, m := withSite(t)
	id := uuid.New()
	nbID := 42
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-D", State: model.MachineStateRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.OnDeviceDeleted(context.Background(), 42, "pop3"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachine(id); err == nil {
		t.Fatal("machine should have been hard-deleted")
	}
	if inv.count("pop3") != 1 {
		t.Fatal("expected pop3 invalidation")
	}
}

func TestOnDeviceDeletedUnknownIgnored(t *testing.T) {
	st, inv, m := withSite(t)
	// No machine exists for device 99.
	if err := m.OnDeviceDeleted(context.Background(), 99, "pop3"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := st.GetMachineByNetBoxDeviceID(99); err == nil {
		t.Fatal("should still not exist")
	}
	if inv.count("pop3") != 0 {
		t.Fatal("no invalidation expected")
	}
}

func TestOnSiteUpdatedRejectsManagedSlugRename(t *testing.T) {
	_, _, m := withSite(t)
	err := m.OnSiteUpdated(context.Background(), "pop3", "pop3-renamed")
	if !errors.Is(err, ErrSlugImmutable) {
		t.Fatalf("expected ErrSlugImmutable, got %v", err)
	}
}

func TestOnSiteUpdatedAllowsUnmanagedSlugRename(t *testing.T) {
	_, _, m := withSite(t) // managed: pop3
	err := m.OnSiteUpdated(context.Background(), "other-site", "other-renamed")
	if err != nil {
		t.Fatalf("unmanaged slug rename should be allowed, got %v", err)
	}
}

func TestOnSiteUpdatedNoChangeNoOp(t *testing.T) {
	_, _, m := withSite(t)
	err := m.OnSiteUpdated(context.Background(), "pop3", "pop3")
	if err != nil {
		t.Fatal(err)
	}
}

// --- B7: NetBox drift mid-operation ---

// TestOnDeviceDeletedMidOpFreezesInsteadOfDeleting pins the B7
// contract for deletes: a NetBox device deletion landing while the
// machine has in-flight ops fails the ops and RETAINS the row (the
// evidence), rather than hard-deleting mid-operation. The idle-machine
// path (TestOnDeviceDeletedHardDeletes) still hard-deletes.
// fakePublisher records op_removed publishes (the B7 drift paths must
// drop failed ops on the workers' op stream, not just the DB).
type fakePublisher struct {
	mu      sync.Mutex
	removed []model.Operation
	states  []model.MachineState
}

func (p *fakePublisher) PublishOpRemoved(op model.Operation, st model.MachineState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, op)
	p.states = append(p.states, st)
}

func (p *fakePublisher) removedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.removed)
}

func TestOnDeviceDeletedMidOpFreezesInsteadOfDeleting(t *testing.T) {
	st, inv, m := withSite(t)
	pub := &fakePublisher{}
	m.SetPublisher(pub)
	id := uuid.New()
	nbID := 43
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-MIDOP", State: model.MachineStateDeploying,
	}); err != nil {
		t.Fatal(err)
	}
	st.inFlightOps[id] = 1

	if err := m.OnDeviceDeleted(context.Background(), 43, "pop3"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachine(id); err != nil {
		t.Fatal("machine row must be RETAINED when ops were in flight (operator evidence)")
	}
	if reason := st.failedOps[id]; reason == "" {
		t.Fatal("in-flight ops were not failed")
	}
	if inv.count("pop3") != 1 {
		t.Fatal("expected pop3 invalidation")
	}
	// The failed op must ALSO leave via the op stream — a DB-only fail
	// leaves a zombie active op on the worker until reconnect.
	if pub.removedCount() != 1 {
		t.Fatalf("expected 1 op_removed publish, got %d", pub.removedCount())
	}
	if pub.states[0] != model.MachineStateFailed {
		t.Fatalf("op_removed machine_state = %q, want failed", pub.states[0])
	}
}

// refusingStore stands for a machine whose op appeared after the mirror's
// in-flight check (so DeleteMachineIfIdle refuses) and then finished before
// the mirror could fail it.
type refusingStore struct{ *fakeStore }

func (refusingStore) DeleteMachineIfIdle(uuid.UUID) (bool, error) { return false, nil }

// TestOnDeviceDeletedRefusedDeleteStillInvalidates: when the delete is
// refused but nothing is left to fail, the row stays for the next sweep
// and no op_removed goes out, but the device is gone from NetBox, so the
// snapshot is still invalidated.
func TestOnDeviceDeletedRefusedDeleteStillInvalidates(t *testing.T) {
	st, inv, _ := withSite(t)
	m := New(refusingStore{st}, inv)
	pub := &fakePublisher{}
	m.SetPublisher(pub)
	id := uuid.New()
	nbID := 44
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxDeviceID: &nbID, NetBoxSerial: "SN-REFUSED", State: model.MachineStateCommissioned,
	}); err != nil {
		t.Fatal(err)
	}

	if err := m.OnDeviceDeleted(context.Background(), 44, "pop3"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMachine(id); err != nil {
		t.Fatalf("machine row must be retained when the delete is refused: %v", err)
	}
	if pub.removedCount() != 0 {
		t.Fatalf("expected no op_removed publishes, got %d", pub.removedCount())
	}
	if inv.count("pop3") != 1 {
		t.Fatalf("expected one pop3 invalidation, got %d", inv.count("pop3"))
	}
}

// TestOnDeviceExternalDrift: topology drift on a machine with ops in
// flight fails the ops; drift on an idle machine is a no-op.
func TestOnDeviceExternalDrift(t *testing.T) {
	st, inv, m := withSite(t)
	pub := &fakePublisher{}
	m.SetPublisher(pub)
	busy, idle := uuid.New(), uuid.New()
	busyID, idleID := 50, 51
	for _, mk := range []struct {
		id uuid.UUID
		nb int
		s  string
	}{{busy, busyID, "SN-BUSY"}, {idle, idleID, "SN-IDLE"}} {
		nb := mk.nb
		if err := st.CreateMachine(&model.Machine{
			ID: mk.id, NetBoxDeviceID: &nb, NetBoxSerial: mk.s, State: model.MachineStateRunning,
		}); err != nil {
			t.Fatal(err)
		}
	}
	st.inFlightOps[busy] = 2

	if err := m.OnDeviceExternalDrift(context.Background(), busyID, []string{"location"}, "pop3"); err != nil {
		t.Fatal(err)
	}
	if st.failedOps[busy] == "" {
		t.Fatal("busy machine's in-flight ops were not failed on drift")
	}
	if inv.count("pop3") != 1 {
		t.Fatal("expected invalidation after failing ops")
	}
	// Both failed ops published as op_removed carrying the durable state.
	if pub.removedCount() != 2 {
		t.Fatalf("expected 2 op_removed publishes, got %d", pub.removedCount())
	}

	if err := m.OnDeviceExternalDrift(context.Background(), idleID, []string{"location"}, "pop3"); err != nil {
		t.Fatal(err)
	}
	if st.failedOps[idle] != "" {
		t.Fatal("idle machine must be untouched by drift")
	}
	if pub.removedCount() != 2 {
		t.Fatal("idle-machine drift must publish nothing")
	}
	// Unknown device: also a clean no-op.
	if err := m.OnDeviceExternalDrift(context.Background(), 999, []string{"site"}, "pop3"); err != nil {
		t.Fatal(err)
	}
}
