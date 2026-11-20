package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// dropAfterHelloGlobal is a global that accepts every hello and then drops
// the connection: it answers the hello from its snapshot but cannot read the
// active operations for the op_resync that follows (a statement timeout or a
// lock on global's operations table), so the hub sends resync_failed and
// closes. It counts the hellos it accepts.
type dropAfterHelloGlobal struct{ hellos atomic.Int64 }

func (g *dropAfterHelloGlobal) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if hash != store.HashWorkerToken("tok-1") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "pop1", WorkerID: "worker-1"}, nil
}

// TouchSiteWorker with a serial is the hub recording an accepted hello;
// heartbeats pass nil.
func (g *dropAfterHelloGlobal) TouchSiteWorker(slug, workerID, _ string, serial *string) (*model.SiteWorker, error) {
	if serial != nil {
		g.hellos.Add(1)
	}
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (g *dropAfterHelloGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (g *dropAfterHelloGlobal) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: slug}}, nil
}

func (g *dropAfterHelloGlobal) ListActiveOperations() ([]model.Operation, error) {
	return nil, errors.New("canceling statement due to statement timeout")
}

// Run waits 1s after a failed attempt and doubles the wait up to 30s, so a
// global in trouble is not hammered by its workers. A connection that global
// accepts and drops again at once is not a recovery and must not put the
// worker back on the 1s step: otherwise every worker re-authenticates, has
// its serial resolved against NetBox and its site_workers row written, and
// is sent a hello reply once a second for as long as global's trouble lasts.
func TestWSConn_RunBacksOffWhenGlobalDropsEveryAcceptedHello(t *testing.T) {
	g := &dropAfterHelloGlobal{}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g, ActiveOps: g, Logger: quiet})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	defer hub.Close()

	localDB := openTestDB(t)
	localDB.SetLogger(quiet)
	w := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, NewCache(localDB), localDB, nil)
	w.SetLogger(quiet)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	const window = 5 * time.Second
	time.Sleep(window)
	cancel()
	<-done

	// On the ladder the attempts start at 0, 1, 3 and 7s: three in the
	// window. Going back to 1s after every accepted hello makes it five.
	got := g.hellos.Load()
	if got < 2 {
		t.Fatalf("setup: global accepted %d hellos in %v, want the worker to have connected and reconnected", got, window)
	}
	if got > 3 {
		t.Errorf("global accepted %d hellos in %v while it dropped every connection right after its hello reply; the reconnect backoff allows 3 (at 0, 1 and 3s)", got, window)
	}
}
