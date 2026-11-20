package snapshot

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/worker"
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

// imagesFailStore is a store whose image read fails: a Postgres error raised
// after every NetBox read has succeeded.
type imagesFailStore struct{ *fakeStore }

func (imagesFailStore) ListImages() ([]model.Image, error) {
	return nil, errors.New("connection refused")
}

// A failed rebuild is counted under the outcome naming the system it failed
// on. Build runs after the Postgres reads but fails only on NetBox data (no
// slagworks-production prefix, conflicting OOB reservations, a bad OOB prefix
// or range set), so its error belongs under error_netbox; a Postgres read
// failing in the same stretch still belongs under error_postgres.
func TestDispatcherCountsFailedRebuildUnderFailingSystem(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(nb *fakeNetBox, fs *fakeStore) StoreReader
		wantErr string
		want    string
	}{
		{
			name: "no slagworks-production prefix",
			setup: func(nb *fakeNetBox, fs *fakeStore) StoreReader {
				nb.prefixes = nil
				return fs
			},
			wantErr: "no slagworks-production prefix",
			want:    "error_netbox",
		},
		{
			name: "two slagworks-oob prefixes",
			setup: func(nb *fakeNetBox, fs *fakeStore) StoreReader {
				nb.oobPrefixes = []netbox.Prefix{{ID: 1, Prefix: "10.1.0.0/22"}, {ID: 2, Prefix: "10.2.0.0/22"}}
				return fs
			},
			wantErr: "expected exactly one slagworks-oob prefix",
			want:    "error_netbox",
		},
		{
			name: "image read fails",
			setup: func(nb *fakeNetBox, fs *fakeStore) StoreReader {
				return imagesFailStore{fs}
			},
			wantErr: "connection refused",
			want:    "error_postgres",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, fs, nb := dispatcherFixture(t)
			d := NewDispatcher(nb, tc.setup(nb, fs), &countingSink{})
			d.SetMetrics(NewMetrics(prometheus.NewRegistry()))

			_, err := d.rebuild(context.Background(), "pop3", 1)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("rebuild error = %v, want one containing %q", err, tc.wantErr)
			}
			for _, outcome := range []string{"success", "skipped_no_change", "error_netbox", "error_postgres"} {
				want := 0.0
				if outcome == tc.want {
					want = 1
				}
				if got := testutil.ToFloat64(d.metrics.RebuildsTotal.WithLabelValues("pop3", outcome)); got != want {
					t.Errorf("rebuilds_total{outcome=%s} = %v, want %v", outcome, got, want)
				}
			}
		})
	}
}

// TestDispatcherSeqNumSurvivesSiteConfigRecreate drives rebuilds into a real
// worker.Cache across `slagctl site-config delete` and `create` of the same
// slug. The worker keeps its token and its persisted SeqNum through that, and
// Cache.ApplySnapshot drops any snapshot whose SeqNum is not above the one it
// holds, so a SeqNum that restarts with the re-created row freezes the worker
// on pre-delete content. The fake store's BumpSiteSnapshotMeta models the
// Postgres sequence; store TestBumpSiteSnapshotMetaSurvivesRecreate checks the
// real one.
func TestDispatcherSeqNumSurvivesSiteConfigRecreate(t *testing.T) {
	ctx := context.Background()
	d, sink, fs, nb := dispatcherFixture(t)
	m1 := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	m2 := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	nb.devices = []netbox.Device{{ID: 1, Name: "host-1", Serial: "SN-1"}}
	fs.machines = []model.Machine{{ID: m1, NetBoxDeviceID: new(1), NetBoxSerial: "SN-1", State: model.MachineStateNew}}

	localDB, err := worker.OpenLocalDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	cache := worker.NewCache(localDB)
	// Stands in for the hub: every pushed snapshot reaches the worker.
	rebuildAndDeliver := func() {
		t.Helper()
		before := sink.Count()
		if _, err := d.rebuild(ctx, "pop3", 0); err != nil {
			t.Fatalf("rebuild: %v", err)
		}
		if sink.Count() == before {
			t.Fatal("rebuild pushed nothing; each step needs a content change")
		}
		snap := sink.Last()
		cache.ApplySnapshot(&snap)
	}

	rebuildAndDeliver()
	fs.machines[0].State = model.MachineStateCommissioned
	rebuildAndDeliver()
	fs.machines[0].State = model.MachineStateRunning
	rebuildAndDeliver()
	held := cache.LastSeqNum()
	if held != sink.Last().SeqNum {
		t.Fatalf("setup: worker holds SeqNum %d, global pushed %d", held, sink.Last().SeqNum)
	}

	// Delete and re-create: CreateSiteConfig inserts a fresh row with
	// last_snapshot_seq 0 and last_snapshot_hash "".
	fresh := *fs.configs["pop3"]
	fresh.LastSnapshotSeq, fresh.LastSnapshotHash = 0, ""
	delete(fs.configs, "pop3")
	fs.configs["pop3"] = &fresh

	// A machine enrolled after the re-create must reach the worker.
	nb.devices = append(nb.devices, netbox.Device{ID: 2, Name: "host-2", Serial: "SN-2"})
	fs.machines = append(fs.machines, model.Machine{ID: m2, NetBoxDeviceID: new(2), NetBoxSerial: "SN-2", State: model.MachineStateNew})
	rebuildAndDeliver()

	if got := sink.Last().SeqNum; got <= held {
		t.Errorf("after the re-create global pushed SeqNum %d; the worker already holds %d for this slug", got, held)
	}
	if cache.MachineByID(m2) == nil {
		t.Errorf("worker dropped the post-re-create snapshot: it holds SeqNum %d, global pushed %d",
			cache.LastSeqNum(), sink.Last().SeqNum)
	}
}
