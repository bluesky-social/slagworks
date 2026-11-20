package worker

// The site-worker link carried through a fault-injecting proxy: the real
// WSConn (connectAndRun, drainLoop, drainOnce, handleObservationAck, the
// reconnect reset) against a real ws.Hub, with internal/proptest/faultproxy
// between them standing in for the ALB and the identity-aware proxy that
// front global in production. Faults are interleaved with outbox traffic:
// connection drops with FIN and with RST, half-closed connections,
// blackholed paths that end in an RST, proxy restarts, 429/502/503 on the
// upgrade, a connection lifetime limit, the byte stream split into writes of
// a few bytes, hub-side kills and worker crashes. Properties, checked at
// quiescence:
//
//   - every observation is eventually acked by global, unless global
//     rejected it terminally, and nothing acked is redelivered afterwards;
//   - op updates reach global in order and without duplicates, and every
//     update appended while no fault could intervene is delivered;
//   - every upgrade the proxy saw carried the worker token in the
//     Authorization header and nowhere in the request line.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/proptest/faultproxy"
	"github.com/bluesky-social/slagworks/internal/ws"
)

type pxEnv struct {
	t      fataler
	dir    string
	srv    *httptest.Server
	hub    *ws.Hub
	auth   *e2eAuth
	global *e2eGlobal
	token  string
	px     *faultproxy.Proxy

	db     *LocalDB
	cache  *Cache
	w      *WSConn
	cancel context.CancelFunc
	done   chan struct{}

	lastErr atomic.Pointer[string]
}

func newPxEnv(t fataler, dir string) *pxEnv {
	g := newE2EGlobal()
	token := "tok-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	auth := &e2eAuth{hash: hex.EncodeToString(sum[:]), slug: "site-a", workerID: "worker-a"}
	hub := ws.NewHub(ws.HubConfig{
		Auth:            auth,
		Rebuilder:       e2eRebuilder{},
		ObsHandler:      g,
		OpUpdateHandler: g,
		Logger:          quietLog,
	})
	srv := httptest.NewServer(hub)
	px, err := faultproxy.New(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	e := &pxEnv{t: t, dir: dir, srv: srv, hub: hub, auth: auth, global: g, token: token, px: px}
	e.startWorker()
	return e
}

func (e *pxEnv) runLoop(ctx context.Context, w *WSConn, done chan struct{}) {
	defer close(done)
	for ctx.Err() == nil {
		if _, err := w.connectAndRun(ctx); err != nil {
			msg := err.Error()
			e.lastErr.Store(&msg)
		}
		w.connected.Store(false)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (e *pxEnv) startWorker() {
	db, err := OpenLocalDB(filepath.Join(e.dir, "worker.db"))
	if err != nil {
		e.t.Fatalf("open local db: %v", err)
	}
	db.SetLogger(quietLog)
	c := NewCache(db)
	c.outbox.RejectBackoffBase = 2 * time.Millisecond
	c.outbox.RejectBackoffMax = 20 * time.Millisecond
	w := NewWSConn(WSConnConfig{GlobalURL: e.px.URL(), Token: e.token, WorkerID: "worker-a"}, c, db, nil)
	w.SetLogger(quietLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.db, e.cache, e.w, e.cancel, e.done = db, c, w, cancel, done
	go e.runLoop(ctx, w, done)
}

func (e *pxEnv) stopWorker() {
	e.cancel()
	<-e.done
	o := e.cache.outbox
	o.mu.Lock()
	if o.retryTimer != nil {
		o.retryTimer.Stop()
	}
	o.mu.Unlock()
	closeQuiet(e.db)
}

func (e *pxEnv) close() {
	e.stopWorker()
	e.px.Close()
	e.hub.Close()
	e.srv.Close()
}

func (e *pxEnv) outboxRows() int64 {
	var n int64
	var err error
	for range 5 {
		if err = e.db.db.Raw("SELECT count(*) FROM outbox").Scan(&n).Error; err == nil {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("count outbox: %v", err)
	return n
}

func (e *pxEnv) lastConnErr() string {
	if p := e.lastErr.Load(); p != nil {
		return *p
	}
	return "<none>"
}

// genBigObsData occasionally produces a multi-MiB observation so frames far
// larger than one TCP segment cross the splitting proxy.
func genBigObsData(t *rapid.T) json.RawMessage {
	if rapid.IntRange(0, 15).Draw(t, "bigObs") != 0 {
		return genObsData(t)
	}
	n := rapid.IntRange(1<<20, 6<<20).Draw(t, "bigLen")
	b, _ := json.Marshal(map[string]string{"blob": strings.Repeat("q", n)})
	return b
}

type pxStats struct {
	cases, appends, bigAppends, updates, drops, resets, halfCloses, blackholes,
	restarts, refusals, lifetimes, chunked, hubKills, crashes, redeliveries, upgrades atomic.Int64
}

func TestWSConnThroughFaultProxy_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end WS proxy property is slow")
	}
	root := t.TempDir()
	var n atomic.Int64
	st := &pxStats{}
	rapid.Check(t, func(rt *rapid.T) {
		dir := filepath.Join(root, fmt.Sprintf("p%d", n.Add(1)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rt.Fatalf("mkdir: %v", err)
		}
		e := newPxEnv(rt, dir)
		defer e.close()
		e.px.SetSeed(rapid.Uint64().Draw(rt, "proxySeed"))
		st.cases.Add(1)
		if !e2eWaitFor(20*time.Second, e.w.Connected) {
			rt.Fatalf("worker never connected within 20s (last connect error: %s)", e.lastConnErr())
		}

		var keys []uuid.UUID
		terminal := map[uuid.UUID]bool{}
		var appended []model.OperationUpdate
		lostFloor := 0
		faultHellos := int64(-1)
		lifetimeOn := false
		var trace []string
		fault := func(name string) {
			lostFloor = len(appended)
			faultHellos = e.auth.hellos.Load()
			trace = append(trace, name)
		}

		steps := rapid.IntRange(1, 30).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			switch rapid.IntRange(0, 16).Draw(rt, "op") {
			case 0, 1, 2:
				k := uuid.New()
				keys = append(keys, k)
				e.global.mu.Lock()
				switch rapid.IntRange(0, 5).Draw(rt, "fate") {
				case 0:
					e.global.terminal[k] = true
					terminal[k] = true
				case 1, 2:
					e.global.reject[k] = rapid.IntRange(1, 3).Draw(rt, "rejects")
				}
				e.global.mu.Unlock()
				data := genBigObsData(rt)
				if len(data) > 1<<20 {
					st.bigAppends.Add(1)
				}
				e.cache.appendObservation(model.Observation{MachineID: k, Event: rapid.SampledFrom(obEvents).Draw(rt, "event"), Data: data})
				st.appends.Add(1)
				trace = append(trace, "append")
			case 3, 4:
				u := genOpUpdate(rt)
				mayBeLost := lifetimeOn || (faultHellos >= 0 && e.auth.hellos.Load() <= faultHellos)
				e.cache.appendOpUpdate(u)
				appended = append(appended, u)
				if mayBeLost {
					lostFloor = len(appended)
				}
				st.updates.Add(1)
				trace = append(trace, "update")
			case 5:
				e.px.DropAll()
				st.drops.Add(1)
				fault("proxy drop (FIN)")
			case 6:
				e.px.ResetAll()
				st.resets.Add(1)
				fault("proxy reset (RST)")
			case 7:
				e.px.HalfCloseDownAll()
				st.halfCloses.Add(1)
				fault("proxy half-close toward worker")
			case 8:
				e.px.BlackholeAll()
				fault("proxy blackhole")
				time.Sleep(time.Duration(rapid.IntRange(0, 40).Draw(rt, "blackholeMs")) * time.Millisecond)
				e.px.ResetAll()
				st.blackholes.Add(1)
				trace = append(trace, "...then RST")
			case 9:
				down := time.Duration(rapid.IntRange(0, 60).Draw(rt, "restartDownMs")) * time.Millisecond
				fault(fmt.Sprintf("proxy restart (down %v)", down))
				if err := e.px.Restart(down); err != nil {
					rt.Fatalf("proxy restart: %v", err)
				}
				st.restarts.Add(1)
			case 10:
				k := rapid.IntRange(1, 4).Draw(rt, "refusals")
				codes := make([]int, k)
				for j := range codes {
					codes[j] = rapid.SampledFrom([]int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable}).Draw(rt, "refuseCode")
				}
				e.px.RefuseNext(codes...)
				st.refusals.Add(1)
				trace = append(trace, fmt.Sprintf("refuse next %v", codes))
			case 11:
				if lifetimeOn {
					e.px.SetMaxLifetime(0)
					lifetimeOn = false
					fault("lifetime limit off")
				} else {
					e.px.SetMaxLifetime(time.Duration(rapid.IntRange(30, 300).Draw(rt, "lifetimeMs")) * time.Millisecond)
					lifetimeOn = true
					st.lifetimes.Add(1)
					fault("lifetime limit on")
				}
			case 12:
				if rapid.Bool().Draw(rt, "chunkOn") {
					e.px.SetChunking(rapid.IntRange(1, 4096).Draw(rt, "chunkMax"), 0)
					st.chunked.Add(1)
					trace = append(trace, "chunking on")
				} else {
					e.px.SetChunking(0, 0)
					trace = append(trace, "chunking off")
				}
			case 13:
				e.hub.Close()
				st.hubKills.Add(1)
				fault("hub-side kill")
			case 14:
				e.stopWorker()
				e.startWorker()
				lostFloor = len(appended)
				faultHellos = -1
				st.crashes.Add(1)
				trace = append(trace, "crash+restart")
			default:
				time.Sleep(time.Duration(rapid.IntRange(0, 25).Draw(rt, "sleepMs")) * time.Millisecond)
				trace = append(trace, "sleep")
			}
		}

		// Clear every standing fault before quiescing. A half-closed or
		// blackholed pair may still be open; reset it so nothing waits on a
		// path that will never deliver.
		if lifetimeOn {
			e.px.SetMaxLifetime(0)
			lostFloor = len(appended)
		}
		e.px.SetChunking(0, 0)

		ok := e2eWaitFor(30*time.Second, func() bool {
			if e.outboxRows() != 0 {
				return false
			}
			e.global.mu.Lock()
			defer e.global.mu.Unlock()
			for _, k := range keys {
				if terminal[k] {
					if !e.global.termAcked[k] {
						return false
					}
				} else if !e.global.okAcked[k] {
					return false
				}
			}
			return true
		})
		if !ok {
			e.global.mu.Lock()
			var missing []string
			for _, k := range keys {
				if !e.global.okAcked[k] && !e.global.termAcked[k] {
					missing = append(missing, fmt.Sprintf("%s(deliveries=%d,rejectsLeft=%d)", k.String()[:8], e.global.deliveries[k], e.global.reject[k]))
				}
			}
			e.global.mu.Unlock()
			rt.Fatalf("not quiescent after 30s: outbox rows=%d unacked=%v connected=%v proxy=%+v last connect error=%s\ntrace=%v",
				e.outboxRows(), missing, e.w.Connected(), e.px.Stats(), e.lastConnErr(), trace)
		}

		_ = e2eWaitFor(5*time.Second, func() bool {
			e.global.mu.Lock()
			defer e.global.mu.Unlock()
			tail := appended[lostFloor:]
			n := len(e.global.updates)
			return n >= len(tail) && sameUpdates(e.global.updates[n-len(tail):], tail)
		})
		e.global.mu.Lock()
		got := append([]model.OperationUpdate(nil), e.global.updates...)
		e.global.mu.Unlock()
		j := 0
		for _, u := range got {
			for j < len(appended) && !sameUpdates([]model.OperationUpdate{u}, []model.OperationUpdate{appended[j]}) {
				j++
			}
			if j == len(appended) {
				rt.Fatalf("hub received update %+v out of order, duplicated, or never appended\ngot=%+v\nappended=%+v\ntrace=%v", u, got, appended, trace)
			}
			j++
		}
		tail := appended[lostFloor:]
		if len(got) < len(tail) || !sameUpdates(got[len(got)-len(tail):], tail) {
			rt.Fatalf("updates appended after the last fault were not all delivered\ngot=%+v\ntail=%+v\ntrace=%v", got, tail, trace)
		}

		before := e.global.totalDeliveries()
		hellos := e.auth.hellos.Load()
		e.px.ResetAll()
		if !e2eWaitFor(20*time.Second, func() bool { return e.auth.hellos.Load() > hellos && e.w.Connected() }) {
			rt.Fatalf("worker did not reconnect within 20s after a proxy reset (last connect error: %s)", e.lastConnErr())
		}
		time.Sleep(30 * time.Millisecond)
		if after := e.global.totalDeliveries(); after != before {
			rt.Fatalf("%d deliveries after quiescence (acked rows redelivered)\ntrace=%v", after-before, trace)
		}

		// Every upgrade carried the token in the header and never in the URL.
		for _, r := range e.px.Requests() {
			st.upgrades.Add(1)
			if r.RequestURI != "/api/v1/ws/site" {
				rt.Fatalf("worker upgrade request line %q, want exactly /api/v1/ws/site with no query", r.RequestURI)
			}
			if strings.Contains(r.RequestURI, e.token) {
				rt.Fatalf("worker token in the upgrade URL %q", r.RequestURI)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer "+e.token {
				rt.Fatalf("upgrade Authorization = %q, want the worker bearer token", got)
			}
		}

		e.global.mu.Lock()
		for _, d := range e.global.deliveries {
			if d > 1 {
				st.redeliveries.Add(int64(d - 1))
			}
		}
		e.global.mu.Unlock()
	})
	t.Logf("proxy coverage: cases=%d appends=%d big=%d updates=%d drops=%d resets=%d halfCloses=%d blackholes=%d restarts=%d refusals=%d lifetimes=%d chunked=%d hubKills=%d crashes=%d redeliveries=%d upgrades=%d",
		st.cases.Load(), st.appends.Load(), st.bigAppends.Load(), st.updates.Load(), st.drops.Load(), st.resets.Load(), st.halfCloses.Load(),
		st.blackholes.Load(), st.restarts.Load(), st.refusals.Load(), st.lifetimes.Load(), st.chunked.Load(), st.hubKills.Load(),
		st.crashes.Load(), st.redeliveries.Load(), st.upgrades.Load())
}

func openFDs() int {
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

// TestWSConnReconnectsDoNotLeak drops the worker's connection 1000 times
// through the proxy (FIN, RST, half-close toward the worker, a short
// blackhole ending in an RST, an upgrade refusal) and checks that goroutines
// and file descriptors return to where they were and that the proxy holds
// only the live connection afterwards (a half-closed worker connection is
// closed by both ends, not left open on global).
func TestWSConnReconnectsDoNotLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("1000 reconnects")
	}
	rounds := 1000
	if s := os.Getenv("SLAGWORKS_PX_ROUNDS"); s != "" {
		_, _ = fmt.Sscan(s, &rounds)
	}
	e := newPxEnv(t, t.TempDir())
	defer e.close()
	if !e2eWaitFor(20*time.Second, e.w.Connected) {
		t.Fatalf("never connected: %s", e.lastConnErr())
	}
	settle := func() (int, int) {
		time.Sleep(300 * time.Millisecond)
		return runtime.NumGoroutine(), openFDs()
	}
	g0, f0 := settle()
	for i := 0; i < rounds; i++ {
		hellos := e.auth.hellos.Load()
		switch i % 5 {
		case 0:
			e.px.DropAll()
		case 1:
			e.px.ResetAll()
		case 2:
			e.px.HalfCloseDownAll()
		case 3:
			e.px.BlackholeAll()
			time.Sleep(2 * time.Millisecond)
			e.px.ResetAll()
		case 4:
			e.px.RefuseNext(http.StatusServiceUnavailable)
			e.px.ResetAll()
		}
		if !e2eWaitFor(20*time.Second, func() bool { return e.auth.hellos.Load() > hellos && e.w.Connected() }) {
			t.Fatalf("round %d: worker did not reconnect (last error %s, proxy %+v)", i, e.lastConnErr(), e.px.Stats())
		}
	}
	e2eWaitFor(5*time.Second, func() bool { return e.px.Open() <= 1 })
	g1, f1 := settle()
	t.Logf("after %d reconnects: goroutines %d -> %d, fds %d -> %d, proxy %+v", rounds, g0, g1, f0, f1, e.px.Stats())
	if g1 > g0+25 {
		t.Errorf("goroutines grew from %d to %d across %d reconnects", g0, g1, rounds)
	}
	if f0 >= 0 && f1 > f0+25 {
		t.Errorf("open file descriptors grew from %d to %d across %d reconnects", f0, f1, rounds)
	}
	if open := e.px.Open(); open != 1 {
		t.Errorf("proxy holds %d open connections after the churn, want 1 (the live one)", open)
	}
}
