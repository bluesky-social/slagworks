package worker

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// These tests run a real WSConn, Cache and LocalDB against a real ws.Hub
// over a WebSocket, for a global whose snapshot sequence for the site
// goes backwards: the site config was deleted and recreated (the new row
// counts from 0 again), global's database was restored from a backup, or
// the worker was pointed at a different global.

// seqGlobal is a minimal global serving one fixed snapshot for site pop1.
type seqGlobal struct{ snap model.SiteSnapshot }

func (g seqGlobal) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if hash != store.HashWorkerToken("tok-1") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "pop1", WorkerID: "worker-1"}, nil
}

func (g seqGlobal) TouchSiteWorker(slug, workerID, _ string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (g seqGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (g seqGlobal) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return g.snap, nil
}

// pop1Snap is pop1's snapshot at seq holding one machine.
func pop1Snap(seq int64, id uuid.UUID, hostname string) model.SiteSnapshot {
	return model.SiteSnapshot{SeqNum: seq, Site: model.SiteInfo{Slug: "pop1"},
		Machines: []model.MachineSnapshot{{ID: id, Hostname: hostname}}}
}

// startGlobal serves a hub for g. stop closes it.
func startGlobal(g seqGlobal) (hub *ws.Hub, url string, stop func()) {
	hub = ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g})
	srv := httptest.NewServer(hub)
	return hub, srv.URL, func() { hub.Close(); srv.Close() }
}

// startWorker runs a worker process's WS client against the global at url
// with its data directory at dbPath, and waits for its first sync. stop
// cancels it and waits for it to return.
func startWorker(t *testing.T, url, dbPath string) (cache *Cache, stop func()) {
	t.Helper()
	localDB, err := OpenLocalDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cache = NewCache(localDB)
	wsc := NewWSConn(WSConnConfig{GlobalURL: url, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = wsc.Run(ctx) }()
	stop = func() { cancel(); <-done }
	sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
	defer scancel()
	if err := wsc.WaitForSync(sctx); err != nil {
		stop()
		t.Fatalf("worker never synced: %v", err)
	}
	return cache, stop
}

// A worker that restarts with a data directory holding a higher seq than
// its global's must adopt the snapshot its global sends as the hello reply
// rather than silently keep the stale one.
func TestWSConn_AdoptsGlobalSnapshotWhenSeqRegresses(t *testing.T) {
	oldMachine := uuid.New()
	newMachine := uuid.New()
	dbPath := filepath.Join(t.TempDir(), "worker.db")

	// Life before: the worker synced snapshot seq 57 from its global.
	_, url, stopGlobal := startGlobal(seqGlobal{snap: pop1Snap(57, oldMachine, "old-host")})
	cache, stopWorker := startWorker(t, url, dbPath)
	if cache.LastSeqNum() != 57 || cache.MachineByID(oldMachine) == nil {
		t.Fatalf("setup: worker did not apply snapshot 57")
	}
	stopWorker()
	stopGlobal()

	// The global now numbers this slug's snapshots from the start, with
	// different content. The worker process restarts with the same data
	// directory.
	_, url, stopGlobal = startGlobal(seqGlobal{snap: pop1Snap(3, newMachine, "new-host")})
	defer stopGlobal()
	cache, stopWorker = startWorker(t, url, dbPath)
	defer stopWorker()

	// WaitForSync returned: the hub sent its snapshot (seq 3) as the hello
	// reply and the worker reports itself connected and synced.
	if cache.MachineByID(newMachine) == nil {
		t.Errorf("worker is connected and reports synced, but does not know machine %s from the global's current snapshot (seq 3)", newMachine)
	}
	if cache.MachineByID(oldMachine) != nil {
		t.Errorf("worker still serves machine %s, which is not in the global's snapshot", oldMachine)
	}
	if got := cache.LastSeqNum(); got != 3 {
		t.Errorf("worker holds snapshot seq %d after a full snapshot reply of seq 3 from its global", got)
	}
}

// A worker that stays connected while its global's sequence for the site
// goes backwards (the site config is deleted and recreated while global
// keeps running, so the dispatcher pushes seq 1 to a worker holding 57)
// must also end up on the global's snapshot, rather than drop that push
// and every later one until the counter passes 57.
func TestWSConn_AdoptsPushedSnapshotWhenSeqRegressesWhileConnected(t *testing.T) {
	oldMachine := uuid.New()
	newMachine := uuid.New()

	hub, url, stopGlobal := startGlobal(seqGlobal{snap: pop1Snap(57, oldMachine, "old-host")})
	defer stopGlobal()
	cache, stopWorker := startWorker(t, url, filepath.Join(t.TempDir(), "worker.db"))
	defer stopWorker()
	if cache.LastSeqNum() != 57 {
		t.Fatalf("setup: worker holds seq %d, want 57", cache.LastSeqNum())
	}

	hub.Push("pop1", pop1Snap(1, newMachine, "new-host"))

	// The worker's reconnect backoff is one second.
	deadline := time.Now().Add(5 * time.Second)
	for cache.LastSeqNum() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := cache.LastSeqNum(); got != 1 {
		t.Fatalf("worker holds snapshot seq %d five seconds after its global pushed seq 1", got)
	}
	if cache.MachineByID(newMachine) == nil {
		t.Errorf("worker does not know machine %s from the global's current snapshot", newMachine)
	}
	if cache.MachineByID(oldMachine) != nil {
		t.Errorf("worker still serves machine %s, which is not in the global's snapshot", oldMachine)
	}
}
