package worker

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// helloLog is a global's worker-auth store that accepts one worker
// (token "tok-1") and records when each of its hellos reaches the hub.
type helloLog struct {
	mu sync.Mutex
	at []time.Time
}

func (h *helloLog) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if hash != store.HashWorkerToken("tok-1") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "pop1", WorkerID: "worker-1"}, nil
}

// TouchSiteWorker is called with a non-nil serial on each hello and with
// nil on heartbeats; only hellos are recorded.
func (h *helloLog) TouchSiteWorker(slug, workerID, _ string, serial *string) (*model.SiteWorker, error) {
	if serial != nil {
		h.mu.Lock()
		h.at = append(h.at, time.Now())
		h.mu.Unlock()
	}
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (h *helloLog) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (h *helloLog) hellos() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Time(nil), h.at...)
}

// A global restart (Hub.Close) or a restart of a proxy in front of
// global drops every worker's socket at the same moment. Each worker
// had completed a hello, so each resets its backoff to 1s; if the wait
// were exactly the backoff, the whole fleet would redial at +1s and keep
// retrying in lockstep (1, 2, 4 ... 30s) for as long as global stayed
// down. The wait carries a per-worker random draw instead. With the draw
// pinned near the top of its range, the first redial after the drop must
// come at least 1s plus nearly 20% later. The bound is a floor on a
// timer, which cannot fire early, so the check is deterministic.
func TestWSConn_ReconnectWaitAfterDropIsJittered(t *testing.T) {
	auth := &helloLog{}
	hub := ws.NewHub(ws.HubConfig{Auth: auth, Rebuilder: e2eRebuilder{}, Logger: quietLog})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	db, err := OpenLocalDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetLogger(quietLog)
	t.Cleanup(func() { closeQuiet(db) })
	w := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, NewCache(db), db, nil)
	w.SetLogger(quietLog)
	const draw = 0.99
	w.reconnectRand = func() float64 { return draw }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); _ = w.Run(ctx) }()

	if !e2eWaitFor(5*time.Second, func() bool { return len(auth.hellos()) == 1 }) {
		t.Fatalf("worker sent %d hellos, want 1 before the drop", len(auth.hellos()))
	}
	dropped := time.Now()
	hub.Close()
	if !e2eWaitFor(5*time.Second, func() bool { return len(auth.hellos()) == 2 }) {
		t.Fatalf("worker sent %d hellos within 5s of the drop, want 2", len(auth.hellos()))
	}

	gap := auth.hellos()[1].Sub(dropped)
	floor := time.Second + time.Duration(draw*0.2*float64(time.Second))
	if gap < floor {
		t.Fatalf("worker redialed %v after the drop with jitter draw %.2f, want at least %v: the reconnect wait ignores the draw, so workers dropped together redial at the same instant",
			gap, draw, floor)
	}
}
