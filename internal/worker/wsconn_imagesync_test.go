package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// stalledBucket serves image tarballs that send headers and a first chunk, then
// stop until release is called: a download that is slow or stalled. Register
// releaseAndDrain with t.Cleanup AFTER starting the worker so it runs before
// the worker's shutdown.
func stalledBucket(t *testing.T) (srv *httptest.Server, release func()) {
	t.Helper()
	ch := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<30))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 4096))
		w.(http.Flusher).Flush()
		select {
		case <-ch:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	return srv, func() { once.Do(func() { close(ch) }) }
}

// stalledImageGlobal is the hub's view of one site with one worker: it
// authenticates token "tok-1" and answers every hello with snap.
type stalledImageGlobal struct {
	mu   sync.Mutex
	snap model.SiteSnapshot
}

func (g *stalledImageGlobal) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if hash != store.HashWorkerToken("tok-1") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "site-a", WorkerID: "worker-1"}, nil
}
func (g *stalledImageGlobal) TouchSiteWorker(slug, workerID, _ string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}
func (g *stalledImageGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }
func (g *stalledImageGlobal) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snap, nil
}

// releaseAndDrain ends the stalled download and waits for the cache's image
// syncer to exit, so it is not still writing into the test's images dir when
// the dir is removed.
func releaseAndDrain(t *testing.T, cache *Cache, release func()) {
	t.Cleanup(func() {
		release()
		waitImageSyncIdle(t, cache)
	})
}

// startStalledImageWorker runs a real hub and a real WSConn against it, with the
// production image sync writing into a temp images dir.
func startStalledImageWorker(t *testing.T, g *stalledImageGlobal) (*ws.Hub, *WSConn, *Cache) {
	t.Helper()
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)
	localDB := openTestDB(t)
	cache := NewCache(localDB)
	cache.ImagesDir = t.TempDir()
	wsc := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = wsc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return hub, wsc, cache
}

// The first snapshot on a connection pins an image the worker lacks (a new
// worker, or the pin moved while the worker restarted). The download must not
// delay the first sync: that is when the worker sends READY=1, and the
// packaged Type=notify unit is killed if READY does not arrive within
// systemd's start timeout (90s by default).
func TestWSConn_FirstSyncDoesNotWaitForImageDownload(t *testing.T) {
	bucket, release := stalledBucket(t)
	g := &stalledImageGlobal{snap: model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "site-a"}, Images: []model.Image{{
		Name: "noble-pxe", Version: "24.04", SHA256: strings.Repeat("aa", 32), URL: bucket.URL + "/noble-pxe/new.tar.gz",
	}}}}
	_, wsc, cache := startStalledImageWorker(t, g)
	releaseAndDrain(t, cache, release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := wsc.WaitForSync(ctx); err != nil {
		t.Fatalf("worker did not finish its first sync within 10s while the image download was in flight: %v", err)
	}
}

// A connected worker gets a snapshot pinning a new image (slagctl image
// follow-latest pushes one to every site), followed by an op_delta. The op
// must reach the cache while the image downloads: the same read loop carries
// op_removed (aborts), claim replies and the hub's status RPCs.
func TestWSConn_ImageDownloadDoesNotStallOpStream(t *testing.T) {
	machineID := uuid.New()
	bucket, release := stalledBucket(t)
	g := &stalledImageGlobal{snap: model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "site-a"}, Machines: []model.MachineSnapshot{{ID: machineID}}}}
	hub, wsc, cache := startStalledImageWorker(t, g)
	releaseAndDrain(t, cache, release)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := wsc.WaitForSync(ctx); err != nil {
		t.Fatalf("first sync (no images): %v", err)
	}
	// The hub registers the connection's site after the hello reply; keep
	// publishing a probe op until one lands so the op stream is known live.
	probe := model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpCommission, Status: model.OpPending, Version: 1}
	deadline := time.Now().Add(10 * time.Second)
	for cache.GetOperation(probe.ID.String()) == nil {
		if time.Now().After(deadline) {
			t.Fatal("op stream never became live before the image push")
		}
		hub.PublishOpDelta(probe, model.MachineStateCommissioning)
		time.Sleep(20 * time.Millisecond)
	}
	hub.Push("site-a", model.SiteSnapshot{SeqNum: 2, Site: model.SiteInfo{Slug: "site-a"}, Machines: []model.MachineSnapshot{{ID: machineID}},
		Images: []model.Image{{Name: "noble-pxe", Version: "24.04", SHA256: strings.Repeat("bb", 32), URL: bucket.URL + "/noble-pxe/next.tar.gz"}}})
	op := model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending, Version: 1}
	hub.PublishOpDelta(op, model.MachineStateDeploying)
	for deadline = time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cache.GetOperation(op.ID.String()) != nil {
			return
		}
	}
	t.Fatalf("op_delta for a pending deploy did not reach the worker's cache within 10s: the WebSocket read loop is blocked in the image download started by the snapshot ahead of it")
}
