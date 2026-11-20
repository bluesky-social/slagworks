package worker

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// These tests run a real WSConn, Cache and LocalDB against a real ws.Hub
// (seqGlobal, see wsconn_seq_regression_test.go) while the worker downloads
// a boot image. Registering or replacing an image changes every site's
// snapshot, and every connected worker then downloads the image (1.2 GB in
// production, with SHA-256 and extraction); a new worker, or one that was
// down at publish, does the same on its first snapshot. For the length of
// that download the worker has to stay a working member of the control
// plane.

// imgSyncWorld is a hub and one worker whose image download blocks until
// release is closed.
type imgSyncWorld struct {
	hub     *ws.Hub
	cache   *Cache
	w       *WSConn
	dbPath  string
	started chan struct{} // closed when the first image download begins
	release chan struct{} // close to let downloads finish
}

func newImgSyncWorld(t *testing.T, first model.SiteSnapshot) *imgSyncWorld {
	t.Helper()
	world := &imgSyncWorld{started: make(chan struct{}), release: make(chan struct{})}
	g := seqGlobal{snap: first}
	world.hub = ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g, Logger: quietLog})
	srv := httptest.NewServer(world.hub)

	world.dbPath = filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(world.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db.SetLogger(quietLog)
	world.cache = NewCache(db)
	world.cache.ImagesDir = t.TempDir()
	begun := false
	world.cache.imageSyncFn = func(string, map[string]*model.Image) error {
		if !begun {
			begun = true
			close(world.started)
		}
		<-world.release
		return nil
	}
	world.w = NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, world.cache, db, nil)
	world.w.SetLogger(quietLog)
	world.w.SetDHCPLeaseSource(func() []dhcp.LeaseInfo { return []dhcp.LeaseInfo{} })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = world.w.Run(ctx) }()
	t.Cleanup(func() {
		close(world.release)
		cancel()
		<-done
		world.hub.Close()
		srv.Close()
		closeQuiet(db)
	})
	return world
}

func imgSyncSnapshot(seq int64, machine uuid.UUID, withImage bool) model.SiteSnapshot {
	s := model.SiteSnapshot{
		SeqNum:   seq,
		Site:     model.SiteInfo{Name: "pop1", Slug: "pop1"},
		Machines: []model.MachineSnapshot{{ID: machine, Hostname: "host-1", State: model.MachineStateRunning}},
	}
	if withImage {
		s.Images = []model.Image{{Name: "noble-new", URL: "https://images.example.net/noble-new.tar.gz", SHA256: "abc"}}
	}
	return s
}

// within polls cond for up to d. eventually (wsconn_uplink_test.go) waits
// two minutes, too long for a check that fails on a stalled read loop.
func within(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// The hub lists a worker as connected and routes RPCs and op messages to it
// while it downloads an image, so the worker has to answer and apply them.
func TestImageDownloadDoesNotStallTheControlPlane(t *testing.T) {
	machine := uuid.New()
	world := newImgSyncWorld(t, imgSyncSnapshot(1, machine, false))
	if !within(5*time.Second, world.w.Connected) {
		t.Fatal("worker did not connect")
	}

	// A new image is registered: the dispatcher pushes the next snapshot.
	world.hub.Push("pop1", imgSyncSnapshot(2, machine, true))
	select {
	case <-world.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the pushed snapshot's image download never started")
	}

	// An operator reads state through the hub (`slagctl dhcp leases`; power
	// and boot status take the same path). The hub has a connected worker,
	// so it routes to it instead of answering 503.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := world.hub.DHCPLeases(ctx, "pop1")
	if len(res) != 1 {
		t.Fatalf("hub routed the RPC to %d workers, want 1", len(res))
	}
	if res[0].Err != "" {
		t.Errorf("RPC to a worker the hub lists as connected got no answer while the worker downloads an image: %s", res[0].Err)
	}

	// An operator powers a machine on: the op_delta must reach the
	// executor now, not when the download ends.
	op := model.Operation{ID: uuid.New(), MachineID: machine, Type: model.OpPowerOn, Status: model.OpPending}
	world.hub.PublishOpDelta(op, "")
	if !within(2*time.Second, func() bool { return len(world.cache.PendingPowerOps()) == 1 }) {
		t.Errorf("a pending power-on published 2s ago has not reached the executor: the worker's read loop is inside the image download")
	}

	// What the worker serves is what it has persisted: a restart during the
	// download must not go back to the previous snapshot.
	if got := world.cache.LastSeqNum(); got != 2 {
		t.Fatalf("worker serves snapshot seq %d, want 2", got)
	}
	db2, err := OpenLocalDB(world.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	persisted := db2.LoadSnapshotMeta()
	closeQuiet(db2)
	if persisted == nil || persisted.SeqNum != 2 {
		got := int64(0)
		if persisted != nil {
			got = persisted.SeqNum
		}
		t.Errorf("worker serves snapshot seq 2 but LocalDB holds seq %d: a restart during the download serves the older snapshot", got)
	}
}

// The same download on the first snapshot of a connection must not keep the
// worker off global: claims, RPC answers and READY=1 all wait for the hello
// to complete, and systemd kills a worker that is not ready in 90s.
func TestImageDownloadDoesNotDelayConnecting(t *testing.T) {
	world := newImgSyncWorld(t, imgSyncSnapshot(1, uuid.New(), true))
	select {
	case <-world.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first snapshot's image download never started")
	}
	if !within(2*time.Second, world.w.Connected) {
		t.Errorf("worker is not connected 2s into its first snapshot's image download")
	}
}
