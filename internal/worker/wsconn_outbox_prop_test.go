package worker

// End-to-end property test: the real WSConn (connectAndRun, drainLoop,
// drainOnce, handleOutboxAck, the reconnect reset) against a real ws.Hub
// over a loopback HTTP server, with the hub's observation and op_update
// handlers scripted to accept, reject transiently, or reject terminally,
// connections killed from either side, and the worker process crashed and
// restarted on the same SQLite file. Concurrency makes the interleaving
// vary, so the properties are eventual ones checked at quiescence:
//
//   - every observation and op update the worker accepted is eventually
//     acked by global, unless global rejected it terminally;
//   - once quiescent (worker outbox empty), further reconnects deliver
//     nothing again (nothing acked is redelivered forever);
//   - each op's updates reach global in the order they were appended: a
//     resend (a lost ack, a dropped connection, a crash) repeats the newest
//     update global has seen for the op, never an older one after a newer.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	sizes      []int

	// Op updates are keyed by the n their Result carries ({"n":N}), with
	// the same scripting as observations.
	updReject     map[int]int
	updTerminal   map[int]bool
	updDeliveries map[int]int
	updAcked      map[int]bool // handler returned nil, or a terminal error
	updOrder      map[string][]int
}

func newE2EGlobal() *e2eGlobal {
	return &e2eGlobal{
		reject: map[uuid.UUID]int{}, terminal: map[uuid.UUID]bool{},
		deliveries: map[uuid.UUID]int{}, okAcked: map[uuid.UUID]bool{}, termAcked: map[uuid.UUID]bool{},
		updReject: map[int]int{}, updTerminal: map[int]bool{}, updDeliveries: map[int]int{},
		updAcked: map[int]bool{}, updOrder: map[string][]int{},
	}
}

// e2eUpdateN reads the key an op update's Result carries.
func e2eUpdateN(r json.RawMessage) int {
	var v struct {
		N int `json:"n"`
	}
	_ = json.Unmarshal(r, &v)
	return v.N
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
	n := e2eUpdateN(msg.Result)
	g.updDeliveries[n]++
	g.updOrder[msg.OperationID] = append(g.updOrder[msg.OperationID], n)
	if g.updTerminal[n] {
		g.updAcked[n] = true
		return fmt.Errorf("operation not found: %w", ws.ErrTerminal)
	}
	if g.updReject[n] > 0 {
		g.updReject[n]--
		return errors.New("database unavailable")
	}
	g.updAcked[n] = true
	return nil
}

func (g *e2eGlobal) totalDeliveries() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, v := range g.deliveries {
		n += v
	}
	for _, v := range g.updDeliveries {
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
}

func newE2EEnv(t fataler, dir string) *e2eEnv {
	return newE2EEnvWith(t, dir, nil)
}

// newE2EEnvWith is newE2EEnv with the hub's op_update handler replaced by
// opHandler (nil keeps the scripted e2eGlobal).
func newE2EEnvWith(t fataler, dir string, opHandler ws.OpUpdateHandler) *e2eEnv {
	g := newE2EGlobal()
	token := "tok-" + uuid.NewString()
	sum := sha256.Sum256([]byte(token))
	auth := &e2eAuth{hash: hex.EncodeToString(sum[:]), slug: "site-a", workerID: "worker-a"}
	if opHandler == nil {
		opHandler = g
	}
	hub := ws.NewHub(ws.HubConfig{
		Auth:            auth,
		Rebuilder:       e2eRebuilder{},
		ObsHandler:      g,
		OpUpdateHandler: opHandler,
		Logger:          quietLog,
	})
	srv := httptest.NewServer(hub)
	e := &e2eEnv{t: t, dir: dir, srv: srv, hub: hub, auth: auth, global: g, token: token}
	e.startWorker()
	return e
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
	w := NewWSConn(WSConnConfig{GlobalURL: e.srv.URL, Token: e.token, WorkerID: "worker-a"}, c, db, nil)
	w.SetLogger(quietLog)
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
	for _, p := range o.entries {
		pend = append(pend, fmt.Sprintf("row%d(update=%v,inflight=%v,next=%v,attempts=%d)",
			p.RowID, p.Update != nil, p.inFlight, p.NextAttempt.Sub(now).Round(time.Millisecond), p.Attempts))
	}
	o.mu.Unlock()
	e.w.ackPendingMu.Lock()
	var ap []string
	for id, p := range e.w.ackPending {
		ap = append(ap, fmt.Sprintf("%s->row%d", id[:8], p.RowID))
	}
	e.w.ackPendingMu.Unlock()
	return fmt.Sprintf("connected=%v entries=%v wakeTokens=%d ackPending=%v",
		e.w.Connected(), pend, len(e.cache.Wake), ap)
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
		appended := map[string][]int{} // op -> update keys in append order
		nUpd := 0
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
				nUpd++
				n := nUpd
				u := genOpUpdate(rt)
				u.Result = json.RawMessage(fmt.Sprintf(`{"n":%d}`, n))
				e.global.mu.Lock()
				switch rapid.IntRange(0, 5).Draw(rt, "updFate") {
				case 0:
					e.global.updTerminal[n] = true
					st.terminals.Add(1)
				case 1, 2:
					e.global.updReject[n] = rapid.IntRange(1, 3).Draw(rt, "updRejects")
					st.rejects.Add(1)
				}
				e.global.mu.Unlock()
				e.cache.appendOpUpdate(u)
				appended[u.OperationID] = append(appended[u.OperationID], n)
				st.updates.Add(1)
				trace = append(trace, fmt.Sprintf("update %d op=%s", n, u.OperationID[len(u.OperationID)-1:]))
			case 5:
				e.w.ForceDisconnect()
				st.kills.Add(1)
				trace = append(trace, "worker-side kill")
			case 6:
				e.hub.Close()
				st.hubKills.Add(1)
				trace = append(trace, "hub-side kill")
			case 7:
				e.stopWorker()
				e.startWorker()
				st.crashes.Add(1)
				trace = append(trace, "crash+restart")
			default:
				time.Sleep(time.Duration(rapid.IntRange(0, 20).Draw(rt, "sleepMs")) * time.Millisecond)
				trace = append(trace, "sleep")
			}
		}

		// Quiesce: every observation and op update acked (or terminally
		// dropped), outbox empty.
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
			for n := 1; n <= nUpd; n++ {
				if !e.global.updAcked[n] {
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
			for n := 1; n <= nUpd; n++ {
				if !e.global.updAcked[n] {
					missing = append(missing, fmt.Sprintf("update %d(deliveries=%d,rejectsLeft=%d)", n, e.global.updDeliveries[n], e.global.updReject[n]))
				}
			}
			e.global.mu.Unlock()
			rt.Fatalf("not quiescent after 20s: outbox rows=%d unacked=%v\nworker state: %s\ntrace=%v", e.outboxRows(), missing, e.workerState(), trace)
		}

		// Op updates: per op, global saw them in append order, each one
		// possibly more than once in a row (resends of the newest update it
		// had seen), never an older one after a newer one.
		e.global.mu.Lock()
		for op, want := range appended {
			got := e.global.updOrder[op]
			j := -1
			for _, n := range got {
				switch {
				case j >= 0 && n == want[j]:
					// resend of the newest update global has seen
				case j+1 < len(want) && n == want[j+1]:
					j++
				default:
					e.global.mu.Unlock()
					rt.Fatalf("op %s: global received update %d out of order\ngot=%v\nappended=%v\ntrace=%v", op, n, got, want, trace)
				}
			}
			if j != len(want)-1 {
				e.global.mu.Unlock()
				rt.Fatalf("op %s: global received %v, appended %v: updates missing\ntrace=%v", op, got, want, trace)
			}
		}
		e.global.mu.Unlock()
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
