package snapshot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// reconcileFakeStore implements ReconcileStore.
type reconcileFakeStore struct {
	mu       sync.Mutex
	configs  []model.SiteConfig
	machines []model.Machine
}

func (f *reconcileFakeStore) ListSiteConfigs() ([]model.SiteConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]model.SiteConfig, len(f.configs))
	copy(out, f.configs)
	return out, nil
}

func (f *reconcileFakeStore) ListMachines(state *model.MachineState) ([]model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if state == nil {
		out := make([]model.Machine, len(f.machines))
		copy(out, f.machines)
		return out, nil
	}
	var out []model.Machine
	for _, m := range f.machines {
		if m.State == *state {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *reconcileFakeStore) ListMachinesByDeviceIDs(deviceIDs []int) ([]model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[int]bool, len(deviceIDs))
	for _, id := range deviceIDs {
		want[id] = true
	}
	var out []model.Machine
	for _, m := range f.machines {
		if m.NetBoxDeviceID != nil && want[*m.NetBoxDeviceID] {
			out = append(out, m)
		}
	}
	return out, nil
}

type recordingMirror struct {
	mu      sync.Mutex
	creates []mirror.Device
	updates []mirror.Device
	deletes []deleteRec
}

type deleteRec struct {
	DeviceID int
	Slug     string
}

func (r *recordingMirror) OnDeviceCreated(ctx context.Context, d mirror.Device) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates = append(r.creates, d)
	return nil
}

func (r *recordingMirror) OnDeviceUpdated(ctx context.Context, d mirror.Device) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, d)
	return nil
}

func (r *recordingMirror) OnDeviceDeleted(ctx context.Context, id int, slug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, deleteRec{DeviceID: id, Slug: slug})
	return nil
}

func (r *recordingMirror) deleteCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deletes)
}

type countingInvalidator struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingInvalidator) Invalidate(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[slug]++
}

func (c *countingInvalidator) countFor(slug string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[slug]
}

func TestReconcileInvalidatesEverySite(t *testing.T) {
	st := &reconcileFakeStore{configs: []model.SiteConfig{
		{Slug: "pop3", NetBoxSite: "pop3"}, {Slug: "pop4", NetBoxSite: "pop4"},
	}}
	nb := &fakeNetBox{sites: map[string]netbox.Site{
		"pop3": {Slug: "pop3", Name: "pop3"},
		"pop4": {Slug: "pop4", Name: "pop4"},
	}}
	inv := &countingInvalidator{}
	mir := &recordingMirror{}
	r := NewReconciler(nb, st, inv, mir).WithInterval(20 * time.Millisecond)
	ctx := t.Context()
	go r.Run(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if inv.countFor("pop3") >= 2 && inv.countFor("pop4") >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("each site should invalidate at least twice, got pop3=%d pop4=%d",
		inv.countFor("pop3"), inv.countFor("pop4"))
}

func TestReconcileBackfillsUntrackedDevicesAsCreate(t *testing.T) {
	// Devices that NetBox knows about but we don't (create webhook lost,
	// or site became managed after devices already existed) must be
	// enrolled through OnDeviceCreated. Devices we already track get
	// OnDeviceUpdated as a no-op touch.
	trackedID := 10
	untrackedID := 20
	st := &reconcileFakeStore{
		configs: []model.SiteConfig{{Slug: "pop3", NetBoxSite: "pop3"}},
		machines: []model.Machine{{
			NetBoxDeviceID: &trackedID, NetBoxSerial: "SN-TRACKED", State: model.MachineStateNew,
		}},
	}
	nb := &fakeNetBox{
		sites: map[string]netbox.Site{"pop3": {Slug: "pop3"}},
		devices: []netbox.Device{
			{ID: trackedID, Name: "tracked", Serial: "SN-TRACKED", Site: &netbox.NestedRef{Slug: "pop3"}},
			{ID: untrackedID, Name: "untracked", Serial: "SN-UNTRACKED", Site: &netbox.NestedRef{Slug: "pop3"}},
		},
	}
	mir := &recordingMirror{}
	r := NewReconciler(nb, st, &countingInvalidator{}, mir)
	r.ManualTick(context.Background())

	if len(mir.creates) != 1 || mir.creates[0].ID != untrackedID {
		t.Fatalf("expected 1 OnDeviceCreated for the untracked device, got %#v", mir.creates)
	}
	if len(mir.updates) != 1 || mir.updates[0].ID != trackedID {
		t.Fatalf("expected 1 OnDeviceUpdated for the tracked device, got %#v", mir.updates)
	}
}

func TestReconcileMarksSlugStaleWhenMissingFromNetBox(t *testing.T) {
	st := &reconcileFakeStore{configs: []model.SiteConfig{
		{Slug: "pop3", NetBoxSite: "pop3"}, {Slug: "stale-site", NetBoxSite: "stale-site"},
	}}
	nb := &fakeNetBox{sites: map[string]netbox.Site{"pop3": {Slug: "pop3"}}}
	inv := &countingInvalidator{}
	r := NewReconciler(nb, st, inv, &recordingMirror{}).WithInterval(20 * time.Millisecond)
	ctx := t.Context()
	go r.Run(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if r.IsStale("stale-site") && !r.IsStale("pop3") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected stale-site stale, pop3 not stale; got stale=%v pop3=%v",
		r.IsStale("stale-site"), r.IsStale("pop3"))
}

func TestReconcileSoftDecommissionsMissingDevice(t *testing.T) {
	nbID := 42
	st := &reconcileFakeStore{
		configs:  []model.SiteConfig{{Slug: "pop3", NetBoxSite: "pop3"}},
		machines: []model.Machine{{NetBoxDeviceID: &nbID, NetBoxSerial: "SN-GONE", State: model.MachineStateRunning}},
	}
	// Site exists, but Device 42 doesn't — GetDeviceByID returns nil.
	nb := &fakeNetBox{sites: map[string]netbox.Site{"pop3": {Slug: "pop3"}}}
	// fakeNetBox.GetDeviceByID returns nil for unknown ids by default.
	inv := &countingInvalidator{}
	mir := &recordingMirror{}
	r := NewReconciler(nb, st, inv, mir)
	r.ManualTick(context.Background())

	if mir.deleteCount() != 1 {
		t.Fatalf("expected 1 soft-decommission, got %d", mir.deleteCount())
	}
}

func TestReconcileSkipsDeviceThatMovedToAnotherSite(t *testing.T) {
	// Machine at pop3 but NetBox says device 42 is at pop4 now. The
	// reconciler for pop3 must NOT decommission — pop4's reconcile
	// pass owns it.
	nbID := 42
	st := &reconcileFakeStore{
		configs: []model.SiteConfig{{Slug: "pop3", NetBoxSite: "pop3"}},
		machines: []model.Machine{{
			NetBoxDeviceID: &nbID, NetBoxSerial: "SN-MOVED", State: model.MachineStateRunning,
		}},
	}
	nb := &fakeNetBox{
		sites: map[string]netbox.Site{"pop3": {Slug: "pop3"}, "pop4": {Slug: "pop4"}},
		// Device 42 lives at pop4 and is NOT returned by ListDevicesForSite("pop3").
		devices: []netbox.Device{{ID: nbID, Name: "host", Serial: "SN-MOVED", Site: &netbox.NestedRef{Slug: "pop4"}}},
	}
	inv := &countingInvalidator{}
	mir := &recordingMirror{}
	r := NewReconciler(nb, st, inv, mir)
	r.ManualTick(context.Background())

	if mir.deleteCount() != 0 {
		t.Fatalf("should not decommission device that moved; got %d deletes", mir.deleteCount())
	}
}

func TestReconcileFirstDoneImmediatelyWithNoConfigs(t *testing.T) {
	st := &reconcileFakeStore{} // no configs
	nb := &fakeNetBox{}
	r := NewReconciler(nb, st, &countingInvalidator{}, &recordingMirror{})
	r.ManualTick(context.Background())
	if !r.FirstReconcileComplete() {
		t.Fatal("empty-config startup should flip FirstReconcileComplete on first tick")
	}
}

func TestReconcileFirstDoneOnlyAfterANonStaleSiteProcessed(t *testing.T) {
	st := &reconcileFakeStore{configs: []model.SiteConfig{{Slug: "stale-only", NetBoxSite: "stale-only"}}}
	nb := &fakeNetBox{sites: map[string]netbox.Site{}} // no sites at all
	r := NewReconciler(nb, st, &countingInvalidator{}, &recordingMirror{})
	r.ManualTick(context.Background())
	if r.FirstReconcileComplete() {
		t.Fatal("should stay not-ready when every site is stale")
	}
}

func TestReconcileEmitsStaleGauge(t *testing.T) {
	st := &reconcileFakeStore{configs: []model.SiteConfig{
		{Slug: "pop3", NetBoxSite: "pop3"}, {Slug: "missing-site", NetBoxSite: "missing-site"},
	}}
	nb := &fakeNetBox{sites: map[string]netbox.Site{"pop3": {Slug: "pop3"}}}
	r := NewReconciler(nb, st, &countingInvalidator{}, &recordingMirror{})
	reg := prometheus.NewRegistry()
	r.SetMetrics(NewMetrics(reg))

	r.ManualTick(context.Background())

	if got := testutil.ToFloat64(r.metrics.SiteStale.WithLabelValues("missing-site")); got != 1 {
		t.Fatalf("missing-site stale gauge: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(r.metrics.SiteStale.WithLabelValues("pop3")); got != 0 {
		t.Fatalf("pop3 stale gauge: got %v, want 0", got)
	}

	// Add the missing site to NetBox; next tick clears the gauge.
	nb.sites["missing-site"] = netbox.Site{Slug: "missing-site"}
	r.ManualTick(context.Background())
	if got := testutil.ToFloat64(r.metrics.SiteStale.WithLabelValues("missing-site")); got != 0 {
		t.Fatalf("after recovery: got %v, want 0", got)
	}
}

// TestReconcileDualCageDoesNotDoubleMirrorOrCrossDelete covers invariant 1:
// two location-scoped configs over one NetBox site, disjoint cages. Each
// config must mirror only its own cage's device (no double-mirror), and
// the phase-2 sweep must not decommission a machine that lives in the
// other cage.
func TestReconcileDualCageDoesNotDoubleMirrorOrCrossDelete(t *testing.T) {
	devA, devB := 100, 200
	st := &reconcileFakeStore{
		configs: []model.SiteConfig{
			{Slug: "pop2-cage-a", NetBoxSite: "pop2", NetBoxLocation: "cage-a"},
			{Slug: "pop2-cage-b", NetBoxSite: "pop2", NetBoxLocation: "cage-b"},
		},
		machines: []model.Machine{
			{NetBoxDeviceID: &devA, NetBoxSerial: "SN-A", State: model.MachineStateRunning},
			{NetBoxDeviceID: &devB, NetBoxSerial: "SN-B", State: model.MachineStateRunning},
		},
	}
	nb := &fakeNetBox{
		sites:     map[string]netbox.Site{"pop2": {Slug: "pop2"}},
		locations: testLocations(),
		devices: []netbox.Device{
			{ID: devA, Name: "a", Serial: "SN-A", Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 10}},
			{ID: devB, Name: "b", Serial: "SN-B", Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 20}},
		},
	}
	mir := &recordingMirror{}
	r := NewReconciler(nb, st, &countingInvalidator{}, mir)
	r.ManualTick(context.Background())

	// No cross-mirror: each device updated exactly once (by its own cage),
	// never twice.
	updatesFor := map[int]int{}
	configFor := map[int]string{}
	for _, u := range mir.updates {
		updatesFor[u.ID]++
		configFor[u.ID] = u.ConfigSlug
	}
	if updatesFor[devA] != 1 || updatesFor[devB] != 1 {
		t.Fatalf("each cage device should be mirrored exactly once; got A=%d B=%d",
			updatesFor[devA], updatesFor[devB])
	}
	// The reconciler must stamp each device with its CLAIMING config slug
	// (the worker-identity PK), which the mirror uses as the invalidation
	// key. cage-a (loc 10) -> pop2-cage-a; cage-b (loc 20) -> pop2-cage-b.
	if configFor[devA] != "pop2-cage-a" {
		t.Fatalf("devA ConfigSlug: got %q want pop2-cage-a", configFor[devA])
	}
	if configFor[devB] != "pop2-cage-b" {
		t.Fatalf("devB ConfigSlug: got %q want pop2-cage-b", configFor[devB])
	}
	// No cross-delete: both devices exist in NetBox, so the sweep deletes
	// nothing even though each config's seen-set covers only its own cage.
	if mir.deleteCount() != 0 {
		t.Fatalf("dual-cage sweep must not decommission live devices; got %d deletes", mir.deleteCount())
	}
}
