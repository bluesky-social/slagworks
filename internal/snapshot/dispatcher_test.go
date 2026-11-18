package snapshot

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

func dispatcherFixture(t *testing.T) (*Dispatcher, *countingSink, *fakeStore, *fakeNetBox) {
	t.Helper()
	cfg := model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}
	fs := newFakeStore(t, cfg)
	nb := &fakeNetBox{
		site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
	}
	sink := &countingSink{}
	return NewDispatcher(nb, fs, sink), sink, fs, nb
}

func TestDispatcherFirstBuildPushesAndBumpsSeq(t *testing.T) {
	d, sink, _, _ := dispatcherFixture(t)
	snap, err := d.rebuild(context.Background(), "pop3", 1)
	if err != nil {
		t.Fatal(err)
	}
	if sink.Count() != 1 {
		t.Fatalf("expected 1 push, got %d", sink.Count())
	}
	if snap.SeqNum != 1 {
		t.Fatalf("seq should be 1, got %d", snap.SeqNum)
	}
	if sink.Last().SeqNum != 1 {
		t.Fatalf("pushed snapshot seq mismatch: %d", sink.Last().SeqNum)
	}
}

func TestDispatcherSkipsWhenContentUnchanged(t *testing.T) {
	d, sink, _, _ := dispatcherFixture(t)
	if _, err := d.rebuild(context.Background(), "pop3", 1); err != nil {
		t.Fatal(err)
	}
	snap, err := d.rebuild(context.Background(), "pop3", 2) // identical inputs
	if err != nil {
		t.Fatal(err)
	}
	if sink.Count() != 1 {
		t.Fatalf("expected 1 push (2nd should skip), got %d", sink.Count())
	}
	// The skipped rebuild still returns a snapshot for caller inspection,
	// carrying the previously-bumped seq.
	if snap.SeqNum != 1 {
		t.Fatalf("skipped rebuild seq should report last-sent (1), got %d", snap.SeqNum)
	}
}

func TestDispatcherBumpsSeqOnContentChange(t *testing.T) {
	d, sink, fs, nb := dispatcherFixture(t)
	if _, err := d.rebuild(context.Background(), "pop3", 1); err != nil {
		t.Fatal(err)
	}
	// Add a device + matching machine between rebuilds.
	nb.devices = append(nb.devices, netbox.Device{ID: 10, Name: "new-host", Serial: "SN"})
	fs.machines = append(fs.machines, model.Machine{
		ID: uuid.New(), NetBoxDeviceID: new(10), NetBoxSerial: "SN", State: model.MachineStateNew,
	})
	snap, err := d.rebuild(context.Background(), "pop3", 2)
	if err != nil {
		t.Fatal(err)
	}
	if sink.Count() != 2 {
		t.Fatalf("expected 2 pushes, got %d", sink.Count())
	}
	if snap.SeqNum != 2 {
		t.Fatalf("second push seq should be 2, got %d", snap.SeqNum)
	}
}

func TestDispatcherBuildSnapshotIsReadOnly(t *testing.T) {
	d, sink, fs, nb := dispatcherFixture(t)
	// Add a device + matching machine so the build has content to inspect.
	nb.devices = append(nb.devices, netbox.Device{ID: 10, Name: "new-host", Serial: "SN"})
	mID := uuid.New()
	fs.machines = append(fs.machines, model.Machine{
		ID: mID, NetBoxDeviceID: new(10), NetBoxSerial: "SN", State: model.MachineStateNew,
	})

	snap, err := d.BuildSnapshot(context.Background(), "pop3")
	if err != nil {
		t.Fatal(err)
	}
	// Read-only: no push, no seq bump (SeqNum stays zero — not authoritative).
	if sink.Count() != 0 {
		t.Fatalf("BuildSnapshot must not push, got %d pushes", sink.Count())
	}
	if snap.SeqNum != 0 {
		t.Fatalf("BuildSnapshot must not bump seq, got %d", snap.SeqNum)
	}
	// It still produces the real built content the boot would consume.
	if len(snap.Machines) != 1 || snap.Machines[0].ID != mID {
		t.Fatalf("expected the built machine snapshot, got %+v", snap.Machines)
	}

	// A subsequent Rebuild still pushes — BuildSnapshot left no state behind
	// that would make the dispatcher think the content was already sent.
	if _, err := d.rebuild(context.Background(), "pop3", 1); err != nil {
		t.Fatal(err)
	}
	if sink.Count() != 1 {
		t.Fatalf("expected Rebuild to push once after a read-only BuildSnapshot, got %d", sink.Count())
	}
}

// The dispatcher's hostname template reaches Build on both gather paths: the
// read-only BuildSnapshot (preflight, match) and the pushing rebuild.
func TestDispatcherAppliesHostnameTemplate(t *testing.T) {
	d, sink, fs, nb := dispatcherFixture(t)
	d.SetHostnameTemplate(mustParseHostnameTemplate(t, internalZoneTemplate))
	nb.devices = append(nb.devices, netbox.Device{ID: 10, Name: "kworker-1.site1", Serial: "SN"})
	fs.machines = append(fs.machines, model.Machine{
		ID: uuid.New(), NetBoxDeviceID: new(10), NetBoxSerial: "SN", State: model.MachineStateNew,
	})
	const want = "kworker-1.internal.site1.example.net"

	snap, err := d.BuildSnapshot(context.Background(), "pop3")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines) != 1 || snap.Machines[0].FQDN != want {
		t.Fatalf("BuildSnapshot machines = %+v, want one machine with FQDN %q", snap.Machines, want)
	}
	if _, err := d.rebuild(context.Background(), "pop3", 1); err != nil {
		t.Fatal(err)
	}
	if pushed := sink.Last(); len(pushed.Machines) != 1 || pushed.Machines[0].FQDN != want {
		t.Fatalf("pushed machines = %+v, want one machine with FQDN %q", pushed.Machines, want)
	}
}

func TestDispatcherReturnsErrorWhenSiteMissingInNetBox(t *testing.T) {
	cfg := model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}
	fs := newFakeStore(t, cfg)
	nb := &fakeNetBox{sites: map[string]netbox.Site{}} // empty multi-site map == no sites
	sink := &countingSink{}
	d := NewDispatcher(nb, fs, sink)
	if _, err := d.rebuild(context.Background(), "pop3", 1); err == nil {
		t.Fatal("expected error when netbox has no matching site")
	}
	if sink.Count() != 0 {
		t.Fatal("expected no push")
	}
}

func TestDispatcherReturnsErrorWhenSiteConfigMissing(t *testing.T) {
	fs := newFakeStore(t) // no configs
	nb := &fakeNetBox{site: netbox.Site{Slug: "pop3"}}
	d := NewDispatcher(nb, fs, &countingSink{})
	if _, err := d.rebuild(context.Background(), "pop3", 1); err == nil {
		t.Fatal("expected error when site_config missing")
	}
}

func TestDispatcherEmitsMetrics(t *testing.T) {
	d, _, _, _ := dispatcherFixture(t)
	reg := prometheus.NewRegistry()
	d.SetMetrics(NewMetrics(reg))

	// First rebuild: success, seq bumps to 1.
	if _, err := d.rebuild(context.Background(), "pop3", 5); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(d.metrics.RebuildsTotal.WithLabelValues("pop3", "success")); got != 1 {
		t.Fatalf("rebuilds_total{outcome=success}: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(d.metrics.SnapshotSeq.WithLabelValues("pop3")); got != 1 {
		t.Fatalf("snapshot_seq: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(d.metrics.DirtyGen.WithLabelValues("pop3")); got != 5 {
		t.Fatalf("dirty_gen: got %v, want 5", got)
	}

	// Second rebuild with identical inputs → skipped_no_change.
	if _, err := d.rebuild(context.Background(), "pop3", 6); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(d.metrics.RebuildsTotal.WithLabelValues("pop3", "skipped_no_change")); got != 1 {
		t.Fatalf("rebuilds_total{outcome=skipped_no_change}: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(d.metrics.DirtyGen.WithLabelValues("pop3")); got != 6 {
		t.Fatalf("dirty_gen after skip: got %v, want 6", got)
	}
}
