package worker

import (
	"context"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// silentRelay forwards TCP between the worker and global. blackhole makes
// every connection open at that moment keep reading and acknowledging both
// ends while delivering nothing and never closing, as a load balancer or
// NAT that lost the flow's state can; connections opened afterwards are
// forwarded normally.
type silentRelay struct {
	ln    net.Listener
	mu    sync.Mutex
	dead  []*atomic.Bool
	conns []net.Conn
}

func newSilentRelay(t *testing.T, target string) *silentRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &silentRelay{ln: ln}
	go func() {
		for {
			down, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", target)
			if err != nil {
				_ = down.Close()
				continue
			}
			dead := &atomic.Bool{}
			r.mu.Lock()
			r.dead = append(r.dead, dead)
			r.conns = append(r.conns, down, up)
			r.mu.Unlock()
			pipe := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					if n > 0 && !dead.Load() {
						if _, werr := dst.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						if !dead.Load() {
							_ = dst.Close()
						}
						return
					}
				}
			}
			go pipe(up, down)
			go pipe(down, up)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.conns {
			_ = c.Close()
		}
	})
	return r
}

func (r *silentRelay) url() string { return "http://" + r.ln.Addr().String() }

func (r *silentRelay) blackhole() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.dead {
		d.Store(true)
	}
}

// TestWSConn_ReconnectsWhenLinkGoesSilent: when the path to global stops
// delivering without a FIN or RST reaching the worker, the worker must
// notice and reconnect. Until it does, every op_update it writes is lost
// (it leaves the in-memory outbox once it is written into the socket) and
// every claim waits out its RPC timeout. TCP gives no signal here: the
// worker's own writes are acknowledged by whatever sits in the path, so
// only the absence of anything from global can tell it. An idle link that
// is still alive must not be cut.
func TestWSConn_ReconnectsWhenLinkGoesSilent(t *testing.T) {
	const (
		interval = 50 * time.Millisecond
		idle     = 500 * time.Millisecond
	)
	g := &uplinkGlobal{changed: make(chan struct{}, 1)}
	hub := ws.NewHub(ws.HubConfig{
		Auth: g, Rebuilder: g, ObsHandler: g, OpUpdateHandler: g,
		HeartbeatInterval: interval, IdleTimeout: idle,
	})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)
	relay := newSilentRelay(t, strings.TrimPrefix(srv.URL, "http://"))

	localDB := openTestDB(t)
	w := NewWSConn(WSConnConfig{GlobalURL: relay.url(), Token: "tok-1", WorkerID: "worker-1"}, NewCache(localDB), localDB, nil)
	w.heartbeatInterval = interval
	w.idleTimeout = idle
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	hellos := func() int { n, _, _ := g.counts(); return n }
	waitFor := func(d time.Duration, cond func() bool) bool {
		for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return cond()
	}
	if !waitFor(10*time.Second, w.Connected) {
		t.Fatal("worker never connected")
	}

	// An idle site on a live link: four idle windows pass on the first
	// connection.
	time.Sleep(4 * idle)
	if n := hellos(); n != 1 || !w.Connected() {
		t.Fatalf("worker left a live idle link: %d hellos, connected=%v; want 1 hello and still connected", n, w.Connected())
	}

	// The path goes silent. Run's first reconnect waits a 1s backoff.
	relay.blackhole()
	silentAt := time.Now()
	if !waitFor(10*time.Second, func() bool { return hellos() > 1 && w.Connected() }) {
		t.Fatalf("worker still on the silent connection %s after it stopped delivering (idle timeout %s): connected=%v, hellos=%d",
			time.Since(silentAt).Round(time.Millisecond), idle, w.Connected(), hellos())
	}
}

// imageSiteGlobal is uplinkGlobal for a site with a boot image, so the
// worker syncs images when it applies the hello reply's snapshot.
type imageSiteGlobal struct{ *uplinkGlobal }

func (imageSiteGlobal) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{
		SeqNum: 1,
		Site:   model.SiteInfo{Slug: "site-a"},
		Images: []model.Image{{Name: "noble-pxe", URL: "http://images.example/noble-pxe", SHA256: "abc"}},
	}, nil
}

// TestWSConn_HeartbeatsWhileApplyingTheHelloSnapshot: applying the hello
// reply's snapshot downloads any boot image the site gained before the
// worker reaches its read loop, which can take minutes. Global drops a
// worker it hears nothing from once it has replied to the hello, so the
// worker has to heartbeat through the download; otherwise a healthy worker
// is dropped, and whatever it sends once the download ends goes into a
// connection global has already closed.
func TestWSConn_HeartbeatsWhileApplyingTheHelloSnapshot(t *testing.T) {
	const (
		interval = 50 * time.Millisecond
		idle     = 500 * time.Millisecond
	)
	g := imageSiteGlobal{&uplinkGlobal{changed: make(chan struct{}, 1)}}
	hub := ws.NewHub(ws.HubConfig{
		Auth: g, Rebuilder: g, ObsHandler: g, OpUpdateHandler: g,
		HeartbeatInterval: interval, IdleTimeout: idle,
	})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	localDB := openTestDB(t)
	cache := NewCache(localDB)
	cache.ImagesDir = t.TempDir()
	var syncs atomic.Int64
	cache.imageSyncFn = func(string, map[string]*model.Image) error {
		syncs.Add(1)
		time.Sleep(4 * idle) // the download outlasts global's idle timeout
		return nil
	}
	w := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	w.heartbeatInterval = interval
	w.idleTimeout = idle
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
	defer scancel()
	if err := w.WaitForSync(sctx); err != nil {
		t.Fatalf("worker never synced: %v", err)
	}
	if n := syncs.Load(); n != 1 {
		t.Fatalf("image sync ran %d times before the worker synced; want 1", n)
	}
	// A dropped connection shows up as a second hello after Run's 1s
	// backoff.
	time.Sleep(2*time.Second + 2*idle)
	if n, _, _ := g.counts(); n != 1 || !w.Connected() || !hub.IsWorkerConnected("site-a", "worker-1") {
		t.Fatalf("global dropped a worker that was applying its hello snapshot: %d hellos, worker connected=%v, global connected=%v; want 1 hello and connected",
			n, w.Connected(), hub.IsWorkerConnected("site-a", "worker-1"))
	}
}
