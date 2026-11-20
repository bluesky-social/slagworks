package worker

// End-to-end property test: the real WSConn (connectAndRun, drainLoop,
// drainOnce, handleObservationAck, the reconnect reset) against a real
// ws.Hub over a loopback HTTP server, with the hub's observation handler
// scripted to accept, reject transiently, or reject terminally, connections
// killed from either side, and the worker process crashed and restarted
// on the same SQLite file. Concurrency makes the interleaving vary, so the
// properties are eventual ones checked at quiescence:
//
//   - every observation the worker accepted is eventually acked by global,
//     unless global rejected it terminally;
//   - once quiescent (worker outbox empty), further reconnects deliver
//     nothing again (nothing acked is redelivered forever);
//   - op updates reach global without duplication or reordering, and every
//     update appended while no fault could intervene is delivered.

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// --- fake global collaborators ---

type e2eAuth struct {
	hash, slug, workerID string
	hellos               atomic.Int64 // hub-side hello count (TouchSiteWorker with a serial)
}

func (a *e2eAuth) GetSiteWorkerByTokenHash(h string) (*model.SiteWorker, error) {
	if h != a.hash {
		return nil, errors.New("unknown token")
	}
	return &model.SiteWorker{ID: uuid.New(), SiteSlug: a.slug, WorkerID: a.workerID}, nil
}

func (a *e2eAuth) TouchSiteWorker(slug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	if serial != nil {
		a.hellos.Add(1)
	}
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}, nil
}

func (a *e2eAuth) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

type e2eRebuilder struct{}

func (e2eRebuilder) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: slug}}, nil
}

// e2eGlobal records what the hub's handlers saw and scripts their answers.
type e2eGlobal struct {
	mu sync.Mutex
	// reject[k] > 0: answer the next reject[k] deliveries of key k with a
	// transient error. terminal[k]: answer every delivery with a terminal
	// error.
	reject     map[uuid.UUID]int
	terminal   map[uuid.UUID]bool
	deliveries map[uuid.UUID]int
	okAcked    map[uuid.UUID]bool // handler returned nil at least once
	termAcked  map[uuid.UUID]bool
	updates    []model.OperationUpdate
	sizes      []int
}

func newE2EGlobal() *e2eGlobal {
	return &e2eGlobal{
		reject: map[uuid.UUID]int{}, terminal: map[uuid.UUID]bool{},
		deliveries: map[uuid.UUID]int{}, okAcked: map[uuid.UUID]bool{}, termAcked: map[uuid.UUID]bool{},
	}
}

func (g *e2eGlobal) HandleObservation(_ context.Context, obs model.Observation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deliveries[obs.MachineID]++
	g.sizes = append(g.sizes, len(obs.Data))
	if g.terminal[obs.MachineID] {
		g.termAcked[obs.MachineID] = true
		return fmt.Errorf("machine not found: %w", ws.ErrTerminal)
	}
	if g.reject[obs.MachineID] > 0 {
		g.reject[obs.MachineID]--
		return errors.New("netbox unavailable")
	}
	g.okAcked[obs.MachineID] = true
	return nil
}

func (g *e2eGlobal) HandleOpUpdate(_ context.Context, msg ws.OpUpdateMsg) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.updates = append(g.updates, model.OperationUpdate{OperationID: msg.OperationID, Status: msg.Status, Phase: msg.Phase, Result: msg.Result})
	return nil
}

func (g *e2eGlobal) totalDeliveries() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, v := range g.deliveries {
		n += v
	}
	return n
}

type fataler interface {
	Fatalf(format string, args ...any)
}

type e2eEnv struct {
	t      fataler
	dir    string
	srv    *httptest.Server
	hub    *ws.Hub
	auth   *e2eAuth
	global *e2eGlobal
	token  string

	db     *LocalDB
	cache  *Cache
	w      *WSConn
	cancel context.CancelFunc
	done   chan struct{}

	lastErr atomic.Pointer[string] // last connectAndRun error, for failure reports

	// globalURL is where the worker dials; "" means srv.URL. heartbeat and
	// idle shorten the link's liveness timers at both ends when non-zero.
	globalURL       string
	heartbeat, idle time.Duration
}

func newE2EEnv(t fataler, dir string) *e2eEnv {
	e := newE2EGlobalSide(t, dir, 0, 0)
	e.startWorker()
	return e
}

// newE2EGlobalSide starts the hub and its fake collaborators without a
// worker, so a caller can put something between the two before startWorker.
func newE2EGlobalSide(t fataler, dir string, heartbeat, idle time.Duration) *e2eEnv {
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

		HeartbeatInterval: heartbeat,
		IdleTimeout:       idle,
	})
	srv := httptest.NewServer(hub)
	return &e2eEnv{t: t, dir: dir, srv: srv, hub: hub, auth: auth, global: g, token: token, heartbeat: heartbeat, idle: idle}
}

// runLoop is WSConn.Run without the reconnect backoff sleep (a 1s floor per
// reconnect would make the property too slow to run many cases).
func (e *e2eEnv) runLoop(ctx context.Context, w *WSConn, done chan struct{}) {
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

func (e *e2eEnv) startWorker() {
	db, err := OpenLocalDB(filepath.Join(e.dir, "worker.db"))
	if err != nil {
		e.t.Fatalf("open local db: %v", err)
	}
	db.SetLogger(quietLog)
	c := NewCache(db)
	c.outbox.RejectBackoffBase = 2 * time.Millisecond
	c.outbox.RejectBackoffMax = 20 * time.Millisecond
	w := NewWSConn(WSConnConfig{GlobalURL: cmp.Or(e.globalURL, e.srv.URL), Token: e.token, WorkerID: "worker-a"}, c, db, nil)
	w.SetLogger(quietLog)
	w.heartbeatInterval, w.idleTimeout = e.heartbeat, e.idle
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.db, e.cache, e.w, e.cancel, e.done = db, c, w, cancel, done
	go e.runLoop(ctx, w, done)
}

func (e *e2eEnv) stopWorker() {
	e.cancel()
	<-e.done
	// drainLoop's final ctx.Done pass may still be running, but drainOnce
	// touches only the in-memory queues, and the read loop (the only path
	// that acks rows in SQLite) has returned, so closing the DB is safe.
	o := e.cache.outbox
	o.mu.Lock()
	if o.retryTimer != nil {
		o.retryTimer.Stop()
	}
	o.mu.Unlock()
	closeQuiet(e.db)
}

func (e *e2eEnv) close() {
	e.stopWorker()
	e.hub.Close()
	e.srv.Close()
}

// workerState describes the worker's in-memory outbox for failure reports.
func (e *e2eEnv) workerState() string {
	o := e.cache.outbox
	o.mu.Lock()
	now := time.Now()
	var pend []string
	for _, p := range o.pending {
		pend = append(pend, fmt.Sprintf("row%d(next=%v,attempts=%d)", p.RowID, p.NextAttempt.Sub(now).Round(time.Millisecond), p.Attempts))
	}
	updates := len(o.opUpdates)
	o.mu.Unlock()
	e.w.ackPendingMu.Lock()
	var ap []string
	for id, p := range e.w.ackPending {
		ap = append(ap, fmt.Sprintf("%s->row%d", id[:8], p.RowID))
	}
	e.w.ackPendingMu.Unlock()
	return fmt.Sprintf("connected=%v pending=%v opUpdates=%d wakeTokens=%d ackPending=%v",
		e.w.Connected(), pend, updates, len(e.cache.Wake), ap)
}

// outboxRows counts the worker's persisted outbox rows. The read runs
// beside the worker's own writes, so it is retried instead of failing the
// case on a busy database.
func (e *e2eEnv) outboxRows() int64 {
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

func e2eWaitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

type e2eStats struct {
	cases, appends, updates, kills, hubKills, crashes, rejects, terminals, redeliveries atomic.Int64
}

func TestWSConnOutboxE2E_Prop(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end WS property is slow")
	}
	root := t.TempDir()
	var n atomic.Int64
	st := &e2eStats{}
	rapid.Check(t, func(rt *rapid.T) {
		dir := filepath.Join(root, fmt.Sprintf("e%d", n.Add(1)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rt.Fatalf("mkdir: %v", err)
		}
		e := newE2EEnv(rt, dir)
		defer e.close()
		st.cases.Add(1)
		// Generous: a loaded runner can stall a loopback dial for seconds.
		if !e2eWaitFor(20*time.Second, e.w.Connected) {
			last := "<none>"
			if p := e.lastErr.Load(); p != nil {
				last = *p
			}
			rt.Fatalf("worker never connected within 20s (last connect error: %s)", last)
		}

		var keys []uuid.UUID
		terminal := map[uuid.UUID]bool{}
		var appended []model.OperationUpdate // every update appended
		lostFloor := 0                       // updates[:lostFloor] may be lost to a crash/kill
		// faultHellos is the hub's hello count at the last connection kill
		// (-1: none outstanding). Until a later hello shows the worker has
		// reconnected, an update can be written into the dying socket and
		// lost (op updates are fire-and-forget), so it joins the lossy prefix.
		faultHellos := int64(-1)
		var trace []string

		steps := rapid.IntRange(1, 25).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			switch rapid.IntRange(0, 9).Draw(rt, "op") {
			case 0, 1, 2:
				k := uuid.New()
				keys = append(keys, k)
				e.global.mu.Lock()
				switch rapid.IntRange(0, 5).Draw(rt, "fate") {
				case 0:
					e.global.terminal[k] = true
					terminal[k] = true
					st.terminals.Add(1)
				case 1, 2:
					e.global.reject[k] = rapid.IntRange(1, 3).Draw(rt, "rejects")
					st.rejects.Add(1)
				}
				e.global.mu.Unlock()
				e.cache.appendObservation(model.Observation{MachineID: k, Event: rapid.SampledFrom(obEvents).Draw(rt, "event"), Data: genObsData(rt)})
				st.appends.Add(1)
				trace = append(trace, "append")
			case 3, 4:
				u := genOpUpdate(rt)
				// Sampled before the update is queued: once it is, the drain
				// goroutine may already have written it to the dying socket
				// by the time a new hello is counted.
				mayBeLost := faultHellos >= 0 && e.auth.hellos.Load() <= faultHellos
				e.cache.appendOpUpdate(u)
				appended = append(appended, u)
				if mayBeLost {
					lostFloor = len(appended)
				}
				st.updates.Add(1)
				trace = append(trace, "update")
			case 5:
				e.w.ForceDisconnect()
				lostFloor = len(appended)
				faultHellos = e.auth.hellos.Load()
				st.kills.Add(1)
				trace = append(trace, "worker-side kill")
			case 6:
				e.hub.Close()
				lostFloor = len(appended)
				faultHellos = e.auth.hellos.Load()
				st.hubKills.Add(1)
				trace = append(trace, "hub-side kill")
			case 7:
				e.stopWorker()
				e.startWorker()
				lostFloor = len(appended)
				faultHellos = -1
				st.crashes.Add(1)
				trace = append(trace, "crash+restart")
			default:
				time.Sleep(time.Duration(rapid.IntRange(0, 20).Draw(rt, "sleepMs")) * time.Millisecond)
				trace = append(trace, "sleep")
			}
		}

		// Quiesce: every key acked (or terminally dropped), outbox empty.
		ok := e2eWaitFor(20*time.Second, func() bool {
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
			rt.Fatalf("not quiescent after 20s: outbox rows=%d unacked=%v\nworker state: %s\ntrace=%v", e.outboxRows(), missing, e.workerState(), trace)
		}

		// Op updates: no duplicates, no reordering; the tail appended after
		// the last fault must all arrive.
		// Wait for the tail itself to arrive: updates from before the last
		// fault may also have arrived, so a count alone races the drain.
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
		// got must be a subsequence of appended (order preserved, nothing
		// invented), and must contain appended[lostFloor:] as its suffix.
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
		// Nothing acked is redelivered: one more reconnect and a crash
		// must not put anything back on the wire.
		// Each reconnect is confirmed by a new hello on the hub: Connected
		// alone can still read true from the connection just dropped, and a
		// worker that never reconnected would pass the check vacuously.
		before := e.global.totalDeliveries()
		reconnected := func(hellos int64) func() bool {
			return func() bool { return e.auth.hellos.Load() > hellos && e.w.Connected() }
		}
		hellos := e.auth.hellos.Load()
		e.w.ForceDisconnect()
		if !e2eWaitFor(20*time.Second, reconnected(hellos)) {
			rt.Fatalf("worker did not reconnect within 20s after a forced disconnect\nworker state: %s", e.workerState())
		}
		e.stopWorker()
		hellos = e.auth.hellos.Load()
		e.startWorker()
		if !e2eWaitFor(20*time.Second, reconnected(hellos)) {
			rt.Fatalf("restarted worker did not connect within 20s\nworker state: %s", e.workerState())
		}
		time.Sleep(30 * time.Millisecond)
		if after := e.global.totalDeliveries(); after != before {
			rt.Fatalf("%d deliveries after quiescence (acked rows redelivered)\ntrace=%v", after-before, trace)
		}

		e.global.mu.Lock()
		for _, d := range e.global.deliveries {
			if d > 1 {
				st.redeliveries.Add(int64(d - 1))
			}
		}
		e.global.mu.Unlock()
	})
	t.Logf("e2e coverage: cases=%d appends=%d updates=%d workerKills=%d hubKills=%d crashes=%d rejectScripts=%d terminalScripts=%d redeliveries=%d",
		st.cases.Load(), st.appends.Load(), st.updates.Load(), st.kills.Load(), st.hubKills.Load(), st.crashes.Load(), st.rejects.Load(), st.terminals.Load(), st.redeliveries.Load())
}
