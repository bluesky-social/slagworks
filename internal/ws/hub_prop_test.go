package ws

// Model-based schedule test for the worker-facing Hub.
//
// A generated history of worker connects / disconnects, NetBox content
// changes, dispatcher rebuilds (Push on content change), op creation, claim
// RPCs (single and racing), phase and terminal publications, hub restarts
// and hello-window races is replayed against a real Hub served over a real
// WebSocket (httptest). Every connection records the exact message sequence
// it received. An independent reference model (propWorld: plain maps, no
// production helpers) says what each connection was allowed to receive and
// what it must hold once the system is quiescent.
//
// Properties (category prefixes appear in failure output):
//
//	protocol:*   the first message after hello is snapshot|no_change|error;
//	             op_resync precedes every op_delta/op_removed; exactly one
//	             op_resync per connection; every hello reply carries Proto.
//	leak:*       an op row (params carry per-site credentials) reaches only
//	             connections whose slug snapshot contained the op's machine
//	             when it was published, re-sent after a push, or resynced;
//	             a claim never hands a foreign site's op row to a worker.
//	lost-op      after quiescence every live connection holds every active
//	             op of its site at the store's current version (no lost
//	             wake-up).
//	zombie-op    after quiescence no connection still holds a terminal op.
//	snapshot:*   after quiescence every live connection holds the latest
//	             snapshot of its slug.
//	claim:*      a claim is granted iff the op was pending and its machine
//	             is in the claimant's site; exactly one of two racing
//	             eligible claims wins; the granted row names the
//	             WS-authenticated worker.
//	order:*      per-connection op-stream ordering: published versions of
//	             one op do not go backwards on the wire, and no op_delta
//	             follows the op's op_removed. The op re-send that follows a
//	             pushed snapshot reads the current row, so it may run ahead
//	             of a publish still queued behind it; that one message per
//	             op and push is exempt.
//	liveness:*   a connection the model expects to stay up stays up and
//	             drains (no deadlock, no hub-side force close).
//
// The consumer is a small replay of the DOCUMENTED worker semantics
// (messages.go: op_resync full-replaces, op_delta is last-writer-wins by
// Version, op_removed tombstones; architecture.md: the first snapshot on a
// connection is adopted whatever its SeqNum, later ones only when newer),
// deliberately not internal/worker.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
)

// ---------- fixed small pools ----------

type propWorker struct{ slug, id, token string }

var propWorkers = []propWorker{
	{"a", "a1", "tok-a1"}, // HA pair for slug a
	{"a", "a2", "tok-a2"},
	{"b", "b1", "tok-b1"},
}

var propSlugs = []string{"a", "b"}

const propMachineCount = 4

func propMachineID(i int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1))
}

func propMachineName(id uuid.UUID) string {
	for i := 0; i < propMachineCount; i++ {
		if propMachineID(i) == id {
			return fmt.Sprintf("M%d", i)
		}
	}
	return id.String()
}

type propSet map[uuid.UUID]bool

func (s propSet) clone() propSet {
	out := make(propSet, len(s))
	for k := range s {
		out[k] = true
	}
	return out
}

func (s propSet) equal(o propSet) bool {
	if len(s) != len(o) {
		return false
	}
	for k := range s {
		if !o[k] {
			return false
		}
	}
	return true
}

func (s propSet) String() string {
	names := make([]string, 0, len(s))
	for k := range s {
		names = append(names, propMachineName(k))
	}
	sort.Strings(names)
	return "{" + strings.Join(names, ",") + "}"
}

type propSnap struct {
	seq      int64
	machines propSet
}

func (s propSnap) toModel(slug string) model.SiteSnapshot {
	ids := make([]uuid.UUID, 0, len(s.machines))
	for id := range s.machines {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	out := model.SiteSnapshot{SeqNum: s.seq, Site: model.SiteInfo{Slug: slug}}
	for _, id := range ids {
		out.Machines = append(out.Machines, model.MachineSnapshot{ID: id})
	}
	return out
}

// ---------- coverage counters (across all cases of one test run) ----------

type propStats struct {
	cases, steps                                 atomic.Int64
	connects, coldHellos, noChange, helloSnap    atomic.Int64
	disconnects, restarts                        atomic.Int64
	contentChanges, rebuilds, pushes             atomic.Int64
	opsCreated, phases, terminals                atomic.Int64
	claims, claimsGranted, claimRaces, raceWins  atomic.Int64
	foreignClaims                                atomic.Int64
	raceTouch, raceRebuild, raceListPre, raceLst atomic.Int64
	deltasRecv, resyncOpsRecv, removedRecv       atomic.Int64
	snapshotsRecv                                atomic.Int64
	reqOpsChecked, settles                       atomic.Int64
	maxConns, maxActiveOps                       atomic.Int64
	violations                                   sync.Map // category -> *atomic.Int64
}

func (s *propStats) bump(m *sync.Map, cat string) {
	v, _ := m.LoadOrStore(cat, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func propMax(a *atomic.Int64, v int64) {
	for {
		cur := a.Load()
		if v <= cur || a.CompareAndSwap(cur, v) {
			return
		}
	}
}

func (s *propStats) report(t *testing.T) {
	t.Helper()
	dump := func(m *sync.Map) string {
		var parts []string
		m.Range(func(k, v any) bool {
			parts = append(parts, fmt.Sprintf("%s=%d", k, v.(*atomic.Int64).Load()))
			return true
		})
		sort.Strings(parts)
		return strings.Join(parts, " ")
	}
	t.Logf("coverage: cases=%d steps=%d connects=%d (cold=%d no_change=%d snapshot=%d) disconnects=%d restarts=%d",
		s.cases.Load(), s.steps.Load(), s.connects.Load(), s.coldHellos.Load(), s.noChange.Load(), s.helloSnap.Load(),
		s.disconnects.Load(), s.restarts.Load())
	t.Logf("coverage: content_changes=%d rebuilds=%d pushes=%d ops_created=%d phases=%d terminals=%d",
		s.contentChanges.Load(), s.rebuilds.Load(), s.pushes.Load(), s.opsCreated.Load(), s.phases.Load(), s.terminals.Load())
	t.Logf("coverage: claims=%d granted=%d foreign_claims=%d races=%d race_single_winner=%d",
		s.claims.Load(), s.claimsGranted.Load(), s.foreignClaims.Load(), s.claimRaces.Load(), s.raceWins.Load())
	t.Logf("coverage: hello_race touch=%d rebuild=%d list_pre=%d list_post=%d",
		s.raceTouch.Load(), s.raceRebuild.Load(), s.raceListPre.Load(), s.raceLst.Load())
	t.Logf("coverage: recv snapshots=%d deltas=%d resync_ops=%d removed=%d; required_ops_checked=%d settles=%d max_conns=%d max_active_ops=%d",
		s.snapshotsRecv.Load(), s.deltasRecv.Load(), s.resyncOpsRecv.Load(), s.removedRecv.Load(),
		s.reqOpsChecked.Load(), s.settles.Load(), s.maxConns.Load(), s.maxActiveOps.Load())
	t.Logf("violations: %s", dump(&s.violations))
}

// ---------- test client: records exactly what the hub sent ----------

type propRecv struct {
	typ   string
	reqID string
	data  json.RawMessage
}

type propClient struct {
	worker   propWorker
	helloSeq int64
	conn     *Conn
	cancel   context.CancelFunc
	id       int

	mu     sync.Mutex
	log    []propRecv
	closed bool
	notify chan struct{}

	// Model bookkeeping, written under propWorld.mu.
	helloTick int // tick at which the hub read its cache for the hello
	// Ticks bracketing the op set reads made while this conn was in its
	// hello: the resync read its ops and then the slug's membership inside
	// [scopeLo, scopeHi]. (A re-send for another conn can read in the same
	// stretch, so the model keeps the bracket rather than one tick.)
	scopeLo, scopeHi int
	helloSnap        *propSnap // snapshot the hub based the hello on (cache or cold build)
	raced            bool      // a hello-window action ran; relax exact reply checks
	expectReply      string    // "snapshot" | "no_change" (when !raced)
	clientClosed     bool      // the test closed it deliberately
	checkedUpTo      int       // log index already scanned for delivery-time checks
}

func (c *propClient) run(ctx context.Context) {
	for {
		env, err := c.conn.ReadMessage(ctx)
		c.mu.Lock()
		if err != nil {
			c.closed = true
			c.mu.Unlock()
			select {
			case c.notify <- struct{}{}:
			default:
			}
			return
		}
		c.log = append(c.log, propRecv{typ: env.Type, reqID: env.RequestID, data: env.Data})
		c.mu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
}

func (c *propClient) snapshotLog() ([]propRecv, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]propRecv(nil), c.log...), c.closed
}

// waitFor polls pred over the received log until it holds, the connection
// closes, or the deadline passes. Returns (pred held, closed).
func (c *propClient) waitFor(d time.Duration, pred func([]propRecv) bool) (bool, bool) {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		lg, closed := c.snapshotLog()
		if pred(lg) {
			return true, closed
		}
		if closed {
			return false, true
		}
		select {
		case <-c.notify:
		case <-deadline.C:
			lg, closed = c.snapshotLog()
			return pred(lg), closed
		}
	}
}

func propHasType(typ string) func([]propRecv) bool {
	return func(lg []propRecv) bool {
		for _, r := range lg {
			if r.typ == typ {
				return true
			}
		}
		return false
	}
}

// ---------- the reference world ----------

type propPublish struct {
	tick int
	elig map[string]bool // slugs whose hub-cached snapshot held the machine at publish
}

type propCacheEvent struct {
	tick int
	snap *propSnap // nil = cold
}

type propWorld struct {
	t     *rapid.T
	stats *propStats

	mu   sync.Mutex
	tick int

	hub *Hub
	srv *httptest.Server

	// "NetBox": the machine set each slug's next rebuild would produce.
	content map[string]propSet
	// "site_configs": last hashed content + last_snapshot_seq. Survives a
	// hub restart.
	pushedSeq     map[string]int64
	pushedContent map[string]propSet
	// Mirror of the hub's in-memory per-slug snapshot cache (nil = cold),
	// with its history for classification.
	hubCache  map[string]*propSnap
	cacheHist map[string][]propCacheEvent

	// "operations" table.
	ops     map[uuid.UUID]*model.Operation
	opOrder []uuid.UUID
	nextOp  int
	// publish records keyed by "opID/version".
	publishes map[string]propPublish
	lastPub   map[uuid.UUID]int // op -> tick of its latest mutation's publish
	// claim grants, by op.
	grants map[uuid.UUID][]string

	clients    []*propClient
	nextClient int
	connecting *propClient
	sentinels  map[string]bool

	// One-shot hello-window hooks, consumed by the fakes.
	hookTouch, hookRebuild, hookListPre, hookListPost func()

	violations []string
}

func propDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newPropWorld(t *rapid.T, stats *propStats) *propWorld {
	w := &propWorld{
		t:             t,
		stats:         stats,
		content:       map[string]propSet{"a": {}, "b": {}},
		pushedSeq:     map[string]int64{},
		pushedContent: map[string]propSet{},
		hubCache:      map[string]*propSnap{},
		cacheHist:     map[string][]propCacheEvent{},
		ops:           map[uuid.UUID]*model.Operation{},
		publishes:     map[string]propPublish{},
		lastPub:       map[uuid.UUID]int{},
		grants:        map[uuid.UUID][]string{},
		sentinels:     map[string]bool{},
	}
	w.startHub()
	return w
}

func (w *propWorld) startHub() {
	w.hub = NewHub(HubConfig{
		Auth:           propAuth{w},
		Rebuilder:      propRebuilder{w},
		OpClaimer:      propStore{w},
		ActiveOps:      propStore{w},
		RebuildTimeout: 5 * time.Second,
		Logger:         propDiscardLogger(),
	})
	w.srv = httptest.NewServer(w.hub)
}

// tracef records one readable history line; rapid prints it only for the
// final (shrunk) failing case.
func (w *propWorld) tracef(format string, args ...any) {
	w.t.Logf("step: "+format, args...)
}

func (w *propWorld) violate(cat, format string, args ...any) {
	v := cat + " " + fmt.Sprintf(format, args...)
	for _, seen := range w.violations {
		if seen == v {
			return
		}
	}
	w.violations = append(w.violations, v)
}

// checkRegistry waits for the hub's connection registry to match the set
// of connections the test holds open: a closed connection must be
// unregistered, a live one must be listed.
func (w *propWorld) checkRegistry() {
	want := func() map[string]int {
		m := map[string]int{}
		for _, c := range w.liveClients() {
			if _, closed := c.snapshotLog(); !closed {
				m[c.worker.slug]++
			}
		}
		return m
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok := true
		exp := want()
		for _, slug := range propSlugs {
			if len(w.hub.ConnectedWorkerSerials(slug)) != exp[slug] {
				ok = false
			}
		}
		for _, wk := range propWorkers {
			live := false
			for _, c := range w.liveClients() {
				if _, closed := c.snapshotLog(); !closed && c.worker.id == wk.id {
					live = true
				}
			}
			if w.hub.IsWorkerConnected(wk.slug, wk.id) != live {
				ok = false
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			w.mu.Lock()
			for _, slug := range propSlugs {
				if got := len(w.hub.ConnectedWorkerSerials(slug)); got != exp[slug] {
					w.violate("registry:count", "slug %s: hub lists %d connections, %d are open", slug, got, exp[slug])
				}
			}
			for _, wk := range propWorkers {
				live := false
				for _, c := range w.liveClients() {
					if _, closed := c.snapshotLog(); !closed && c.worker.id == wk.id {
						live = true
					}
				}
				if got := w.hub.IsWorkerConnected(wk.slug, wk.id); got != live {
					w.violate("registry:identity", "worker %s of slug %s: hub says connected=%v, the test holds an open connection=%v", wk.id, wk.slug, got, live)
				}
			}
			w.mu.Unlock()
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (w *propWorld) setCacheLocked(slug string, s *propSnap) {
	w.tick++
	w.hubCache[slug] = s
	w.cacheHist[slug] = append(w.cacheHist[slug], propCacheEvent{tick: w.tick, snap: s})
}

// cacheAtLocked returns the hub cache mirror for slug as of tick.
func (w *propWorld) cacheAtLocked(slug string, tick int) *propSnap {
	var cur *propSnap
	for _, ev := range w.cacheHist[slug] {
		if ev.tick > tick {
			break
		}
		cur = ev.snap
	}
	return cur
}

// inCacheBetweenLocked reports whether any snapshot the hub had cached for
// slug at some tick in [lo, hi] held machine m.
func (w *propWorld) inCacheBetweenLocked(slug string, m uuid.UUID, lo, hi int) bool {
	if s := w.cacheAtLocked(slug, lo); s != nil && s.machines[m] {
		return true
	}
	for _, ev := range w.cacheHist[slug] {
		if ev.tick > lo && ev.tick <= hi && ev.snap != nil && ev.snap.machines[m] {
			return true
		}
	}
	return false
}

func (w *propWorld) inCacheLocked(slug string, m uuid.UUID) bool {
	s := w.hubCache[slug]
	return s != nil && s.machines[m]
}

// --- the dispatcher model: per-slug rebuild, Push only on content change ---

func (w *propWorld) rebuild(slug string) propSnap {
	w.mu.Lock()
	cur := w.content[slug].clone()
	last, hashed := w.pushedContent[slug]
	if hashed && last.equal(cur) {
		s := propSnap{seq: w.pushedSeq[slug], machines: cur}
		w.mu.Unlock()
		return s
	}
	w.pushedSeq[slug]++
	w.pushedContent[slug] = cur
	s := propSnap{seq: w.pushedSeq[slug], machines: cur}
	w.setCacheLocked(slug, &s)
	hub := w.hub
	w.mu.Unlock()
	w.stats.pushes.Add(1)
	hub.Push(slug, s.toModel(slug))
	return s
}

// --- op mutations: always mutate-then-publish, like production ---

func (w *propWorld) recordPublishLocked(op model.Operation) {
	w.tick++
	elig := map[string]bool{}
	for _, slug := range propSlugs {
		if w.inCacheLocked(slug, op.MachineID) {
			elig[slug] = true
		}
	}
	w.publishes[fmt.Sprintf("%s/%d", op.ID, op.Version)] = propPublish{tick: w.tick, elig: elig}
	w.lastPub[op.ID] = w.tick
}

func (w *propWorld) activeOpForLocked(m uuid.UUID) *model.Operation {
	for _, id := range w.opOrder {
		op := w.ops[id]
		if op.MachineID == m && propActive(op.Status) {
			return op
		}
	}
	return nil
}

func propActive(s model.OperationStatus) bool {
	return s == model.OpPending || s == model.OpClaimed || s == model.OpRunning
}

func (w *propWorld) createOp(m uuid.UUID, typ model.OperationType) bool {
	w.mu.Lock()
	if w.activeOpForLocked(m) != nil { // idx_one_active_op_per_machine
		w.mu.Unlock()
		return false
	}
	w.nextOp++
	op := &model.Operation{
		ID:        uuid.MustParse(fmt.Sprintf("10000000-0000-4000-8000-%012d", w.nextOp)),
		MachineID: m,
		Type:      typ,
		Status:    model.OpPending,
		Params:    json.RawMessage(fmt.Sprintf(`{"bmc_runtime":{"password":"secret-for-%s"}}`, propMachineName(m))),
	}
	w.ops[op.ID] = op
	w.opOrder = append(w.opOrder, op.ID)
	cp := *op
	w.recordPublishLocked(cp)
	hub := w.hub
	w.mu.Unlock()
	w.stats.opsCreated.Add(1)
	hub.PublishOpDelta(cp, "")
	return true
}

func (w *propWorld) phase(id uuid.UUID) bool {
	w.mu.Lock()
	op := w.ops[id]
	if op == nil || (op.Status != model.OpClaimed && op.Status != model.OpRunning) {
		w.mu.Unlock()
		return false
	}
	op.Version++
	op.Status = model.OpRunning
	op.Phase = fmt.Sprintf("phase-%d", op.Version)
	cp := *op
	w.recordPublishLocked(cp)
	hub := w.hub
	w.mu.Unlock()
	w.stats.phases.Add(1)
	hub.PublishOpDelta(cp, "")
	return true
}

func (w *propWorld) terminal(id uuid.UUID, failed bool) bool {
	w.mu.Lock()
	op := w.ops[id]
	if op == nil || !propActive(op.Status) {
		w.mu.Unlock()
		return false
	}
	op.Version++
	op.Status = model.OpCompleted
	state := model.MachineStateRunning
	if failed {
		op.Status = model.OpFailed
		state = model.MachineStateFailed
	}
	cp := *op
	w.tick++
	w.lastPub[op.ID] = w.tick
	hub := w.hub
	w.mu.Unlock()
	w.stats.terminals.Add(1)
	hub.PublishOpRemoved(cp, state)
	return true
}

// ---------- fakes handed to the Hub ----------

type propAuth struct{ w *propWorld }

func (a propAuth) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	for _, wk := range propWorkers {
		if store.HashWorkerToken(wk.token) == hash {
			return &model.SiteWorker{SiteSlug: wk.slug, WorkerID: wk.id}, nil
		}
	}
	return nil, store.ErrSiteWorkerNotFound
}

// TouchSiteWorker runs before the conn registers and before the hub reads
// its snapshot cache: the pre-register hello-window hook lives here.
func (a propAuth) TouchSiteWorker(slug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	w := a.w
	if serial == nil {
		return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil // heartbeat
	}
	w.mu.Lock()
	hook := w.hookTouch
	w.hookTouch = nil
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	w.mu.Lock()
	if c := w.connecting; c != nil && c.worker.id == workerID {
		c.helloTick = w.tick
		if s := w.hubCache[slug]; s != nil {
			cp := *s
			c.helloSnap = &cp
			if c.helloSeq == s.seq && s.seq != 0 {
				c.expectReply = TypeNoChange
			} else {
				c.expectReply = TypeSnapshot
			}
		}
	}
	w.mu.Unlock()
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (a propAuth) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

type propRebuilder struct{ w *propWorld }

// RebuildAndWait is the cold-hello path: the conn is registered, the hub
// has no cached snapshot for the slug. The rebuild result is captured
// first; a hello-window action then runs before the hub goroutine resumes
// (a legal interleaving: nothing is held between RebuildAndWait returning
// and the hub's next step).
func (r propRebuilder) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	w := r.w
	w.stats.coldHellos.Add(1)
	built := w.rebuild(slug)

	w.mu.Lock()
	// The hub caches the built snapshot unless a newer one is already there.
	if cur := w.hubCache[slug]; cur == nil || cur.seq < built.seq {
		cp := built
		w.setCacheLocked(slug, &cp)
	}
	if c := w.connecting; c != nil && c.worker.slug == slug {
		cp := built
		c.helloSnap = &cp
		c.helloTick = w.tick
		if c.helloSeq == built.seq && built.seq != 0 {
			c.expectReply = TypeNoChange
		} else {
			c.expectReply = TypeSnapshot
		}
	}
	hook := w.hookRebuild
	w.hookRebuild = nil
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	return built.toModel(slug), nil
}

type propStore struct{ w *propWorld }

// ListActiveOperations is the op set read of the hello resync and of the
// re-send after a push (the hub holds the conn's opStreamMu). During a
// hello, the pre hook's mutation is in the read; the post hook's mutation
// commits after it.
func (s propStore) ListActiveOperations() ([]model.Operation, error) {
	w := s.w
	w.mu.Lock()
	pre := w.hookListPre
	w.hookListPre = nil
	w.mu.Unlock()
	if pre != nil {
		pre()
	}
	w.mu.Lock()
	var out []model.Operation
	for _, id := range w.opOrder {
		if op := w.ops[id]; propActive(op.Status) {
			out = append(out, *op)
		}
	}
	w.tick++
	c := w.connecting
	if c != nil && c.scopeLo == 0 {
		c.scopeLo = w.tick
	}
	post := w.hookListPost
	w.hookListPost = nil
	w.mu.Unlock()
	if post != nil {
		post()
	}
	// The hub scopes the resync by the membership it reads once this call
	// returns, so a push made by the post hook is already in it.
	if c != nil {
		w.mu.Lock()
		c.scopeHi = w.tick
		w.mu.Unlock()
	}
	return out, nil
}

func (s propStore) GetOperationIfExists(id uuid.UUID) (*model.Operation, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if op := w.ops[id]; op != nil {
		cp := *op
		return &cp, nil
	}
	return nil, nil
}

// ClaimOperation is the model CAS: pending -> claimed, version bump.
func (s propStore) ClaimOperation(id uuid.UUID, workerID string) (*model.Operation, error) {
	w := s.w
	w.mu.Lock()
	defer w.mu.Unlock()
	op := w.ops[id]
	if op == nil || op.Status != model.OpPending {
		return nil, nil
	}
	op.Status = model.OpClaimed
	op.ClaimedBy = workerID
	op.Version++
	cp := *op
	w.grants[id] = append(w.grants[id], workerID)
	w.recordPublishLocked(cp) // the hub publishes the claimed row right after
	return &cp, nil
}

// ---------- steps ----------

func (w *propWorld) liveClients() []*propClient {
	var out []*propClient
	for _, c := range w.clients {
		if !c.clientClosed {
			out = append(out, c)
		}
	}
	return out
}

// propDial opens a worker WebSocket to the test server.
func propDial(srv *httptest.Server, token string) (*Conn, error) {
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return Dial(ctx, u.String(), &websocket.DialOptions{HTTPHeader: h})
}

func (w *propWorld) connect(wk propWorker, helloSeq int64, raced bool) *propClient {
	conn, err := propDial(w.srv, wk.token)
	if err != nil {
		w.t.Fatalf("harness: dial: %v", err)
	}
	conn.SetReadLimit(16 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.nextClient++
	c := &propClient{worker: wk, helloSeq: helloSeq, conn: conn, cancel: cancel,
		id: w.nextClient, notify: make(chan struct{}, 1), raced: raced}
	w.clients = append(w.clients, c)
	w.connecting = c
	w.mu.Unlock()
	go c.run(ctx)

	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = conn.WriteTyped(wctx, TypeHello, "", HelloMsg{WorkerID: wk.id, LastSeqNum: helloSeq, Proto: OpStreamProto})
	wcancel()
	if err != nil {
		w.t.Fatalf("harness: send hello: %v", err)
	}
	ok, closed := c.waitFor(10*time.Second, propHasType(TypeOpResync))
	w.mu.Lock()
	w.connecting = nil
	if !ok {
		w.violate("liveness:hello", "conn#%d %s: no op_resync within 10s (closed=%v)", c.id, wk.id, closed)
	}
	w.mu.Unlock()
	w.stats.connects.Add(1)
	propMax(&w.stats.maxConns, int64(len(w.liveClients())))
	return c
}

func (w *propWorld) disconnect(c *propClient) {
	c.clientClosed = true
	c.cancel()
	_ = c.conn.CloseNow()
	w.stats.disconnects.Add(1)
}

func (w *propWorld) restartHub() {
	for _, c := range w.liveClients() {
		c.clientClosed = true
		c.cancel()
		_ = c.conn.CloseNow()
	}
	w.hub.Close()
	w.srv.Close()
	w.mu.Lock()
	for _, slug := range propSlugs {
		if w.hubCache[slug] != nil {
			w.setCacheLocked(slug, nil)
		}
	}
	w.mu.Unlock()
	w.startHub()
	w.stats.restarts.Add(1)
}

type propClaimResult struct {
	ok      bool // a response arrived
	granted bool
	op      *model.Operation
	errStr  string
}

func (w *propWorld) sendClaim(c *propClient, opID uuid.UUID, reqID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{OperationID: opID.String()})
}

func (w *propWorld) awaitClaim(c *propClient, reqID string) propClaimResult {
	var res propClaimResult
	ok, _ := c.waitFor(10*time.Second, func(lg []propRecv) bool {
		for _, r := range lg {
			if r.typ == TypeClaimOpResp && r.reqID == reqID {
				var rpc RPCResponseMsg
				_ = json.Unmarshal(r.data, &rpc)
				res.ok = true
				res.errStr = rpc.Error
				var cr ClaimOpResponse
				_ = json.Unmarshal(rpc.Data, &cr)
				res.granted = cr.Operation != nil
				res.op = cr.Operation
				return true
			}
		}
		return false
	})
	if !ok {
		res.ok = false
	}
	return res
}

func (w *propWorld) checkClaimResult(c *propClient, opID uuid.UUID, res propClaimResult, machineInScope bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !res.ok {
		w.violate("liveness:claim", "conn#%d %s: no claim_op_response for %s", c.id, c.worker.id, opID)
		return
	}
	if res.errStr != "" {
		w.violate("claim:error", "conn#%d %s: claim of %s returned error %q", c.id, c.worker.id, opID, res.errStr)
	}
	if res.granted {
		w.stats.claimsGranted.Add(1)
		if res.op.ClaimedBy != c.worker.id {
			w.violate("claim:identity", "conn#%d %s: granted row says claimed_by=%q", c.id, c.worker.id, res.op.ClaimedBy)
		}
		if !machineInScope {
			w.violate("leak:claim-foreign", "conn#%d %s (slug %s) was handed op %s for machine %s, which is not in slug %s's snapshot; params=%s",
				c.id, c.worker.id, c.worker.slug, opID, propMachineName(res.op.MachineID), c.worker.slug, res.op.Params)
		}
	}
}

func (w *propWorld) claim(c *propClient, opID uuid.UUID) {
	w.mu.Lock()
	op := w.ops[opID]
	wasPending := op != nil && op.Status == model.OpPending
	inScope := op != nil && w.inCacheLocked(c.worker.slug, op.MachineID)
	before := len(w.grants[opID])
	w.nextClient++
	reqID := fmt.Sprintf("claim-%d", w.nextClient)
	w.mu.Unlock()
	w.stats.claims.Add(1)
	if op != nil && !inScope {
		w.stats.foreignClaims.Add(1)
	}
	w.sendClaim(c, opID, reqID)
	res := w.awaitClaim(c, reqID)
	w.checkClaimResult(c, opID, res, inScope)
	w.mu.Lock()
	defer w.mu.Unlock()
	want := wasPending && inScope
	if res.ok && res.granted != want {
		w.violate("claim:grant", "conn#%d %s: claim of %s granted=%v but op pending=%v, machine in the claimant's site=%v", c.id, c.worker.id, opID, res.granted, wasPending, inScope)
	}
	if got := len(w.grants[opID]) - before; (want && got != 1) || (!want && got != 0) {
		w.violate("claim:cas", "op %s: %d store grants from one claim (pending=%v, machine in the claimant's site=%v)", opID, got, wasPending, inScope)
	}
}

func (w *propWorld) claimRace(c1, c2 *propClient, opID uuid.UUID) {
	w.mu.Lock()
	op := w.ops[opID]
	wasPending := op != nil && op.Status == model.OpPending
	in1 := op != nil && w.inCacheLocked(c1.worker.slug, op.MachineID)
	in2 := op != nil && w.inCacheLocked(c2.worker.slug, op.MachineID)
	w.nextClient += 2
	r1, r2 := fmt.Sprintf("race-%d", w.nextClient-1), fmt.Sprintf("race-%d", w.nextClient)
	w.mu.Unlock()
	w.stats.claimRaces.Add(1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.sendClaim(c1, opID, r1) }()
	go func() { defer wg.Done(); w.sendClaim(c2, opID, r2) }()
	wg.Wait()
	res1 := w.awaitClaim(c1, r1)
	res2 := w.awaitClaim(c2, r2)
	w.checkClaimResult(c1, opID, res1, in1)
	w.checkClaimResult(c2, opID, res2, in2)
	wins := 0
	if res1.granted {
		wins++
	}
	if res2.granted {
		wins++
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	want := 0
	if wasPending && (in1 || in2) {
		want = 1
	}
	if res1.ok && res2.ok && wins != want {
		w.violate("claim:race", "op %s: %d winners from two racing claims (pending=%v, in site: %v and %v)", opID, wins, wasPending, in1, in2)
	}
	if wins == 1 {
		w.stats.raceWins.Add(1)
	}
}

// settle publishes a fleet-wide sentinel op_removed and waits for every
// live connection to receive it. Snapshots and op messages for one conn
// leave through one ordered queue, so once the sentinel has arrived
// everything published before it has been written too.
func (w *propWorld) settle() {
	w.stats.settles.Add(1)
	w.mu.Lock()
	w.nextOp++
	sid := uuid.MustParse(fmt.Sprintf("5e000000-0000-4000-8000-%012d", w.nextOp))
	w.sentinels[sid.String()] = true
	hub := w.hub
	w.mu.Unlock()
	hub.PublishOpRemoved(model.Operation{ID: sid}, "")
	for _, c := range w.liveClients() {
		ok, closed := c.waitFor(15*time.Second, func(lg []propRecv) bool {
			for _, r := range lg {
				if r.typ == TypeOpRemoved && strings.Contains(string(r.data), sid.String()) {
					return true
				}
			}
			return false
		})
		if !ok {
			w.mu.Lock()
			w.violate("liveness:settle", "conn#%d %s: sentinel never arrived (closed by hub=%v)", c.id, c.worker.id, closed)
			w.mu.Unlock()
		}
	}
	w.checkRegistry()
}

// ---------- the consumer replay + oracle ----------

type propConsumer struct {
	snapSeq   int64
	machines  propSet // nil until a snapshot was applied
	ops       map[uuid.UUID]model.Operation
	tomb      map[uuid.UUID]int
	resynced  bool
	lastDelta map[uuid.UUID]int
	// resendSeen is non-nil from a snapshot message until the next
	// op_removed: the stretch in which the hub's op re-send after a push
	// can appear. It holds the ops that already had a delta in it.
	resendSeen map[uuid.UUID]bool
}

// check replays each connection's log through the documented consumer
// semantics, applies the delivery-time rules to messages not yet scanned,
// and (final=true) the quiescence rules.
func (w *propWorld) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.clients {
		lg, closed := c.snapshotLog()
		st := propConsumer{snapSeq: c.helloSeq, ops: map[uuid.UUID]model.Operation{}, tomb: map[uuid.UUID]int{}, lastDelta: map[uuid.UUID]int{}}
		who := fmt.Sprintf("conn#%d %s(slug %s)", c.id, c.worker.id, c.worker.slug)
		resyncs := 0
		for i, r := range lg {
			fresh := i >= c.checkedUpTo
			if i == 0 {
				if r.typ != TypeSnapshot && r.typ != TypeNoChange && r.typ != TypeError {
					if fresh {
						w.violate("protocol:first-message", "%s: first message after hello was %q", who, r.typ)
					}
				}
				if fresh && !c.raced && c.expectReply != "" && r.typ != c.expectReply {
					w.violate("protocol:hello-reply", "%s: hello(last_seq=%d) against hub seq %d got %q, want %q",
						who, c.helloSeq, c.helloSnap.seq, r.typ, c.expectReply)
				}
				if fresh {
					switch r.typ {
					case TypeNoChange:
						w.stats.noChange.Add(1)
					case TypeSnapshot:
						w.stats.helloSnap.Add(1)
					}
				}
			}
			switch r.typ {
			case TypeSnapshot:
				var m SnapshotMsg
				if err := json.Unmarshal(r.data, &m); err != nil {
					w.violate("protocol:decode", "%s: snapshot: %v", who, err)
					continue
				}
				if fresh {
					w.stats.snapshotsRecv.Add(1)
					if m.Proto < OpStreamProto {
						w.violate("protocol:proto", "%s: snapshot carries proto %d", who, m.Proto)
					}
				}
				st.resendSeen = map[uuid.UUID]bool{}
				// architecture.md: the first snapshot on a connection is
				// adopted whatever its SeqNum; after that the cache rejects
				// one whose SeqNum is equal to or less than the current.
				if i == 0 || st.machines == nil && m.Snapshot.SeqNum >= st.snapSeq || m.Snapshot.SeqNum > st.snapSeq {
					st.snapSeq = m.Snapshot.SeqNum
					st.machines = propSet{}
					for _, mm := range m.Snapshot.Machines {
						st.machines[mm.ID] = true
					}
				}
			case TypeNoChange:
				var m NoChangeMsg
				_ = json.Unmarshal(r.data, &m)
				if fresh && m.Proto < OpStreamProto {
					w.violate("protocol:proto", "%s: no_change carries proto %d", who, m.Proto)
				}
			case TypeOpResync:
				var m OpResyncMsg
				if err := json.Unmarshal(r.data, &m); err != nil {
					w.violate("protocol:decode", "%s: op_resync: %v", who, err)
					continue
				}
				resyncs++
				if fresh {
					w.stats.resyncOpsRecv.Add(int64(len(m.Ops)))
					if i == 0 {
						w.violate("protocol:resync-before-reply", "%s: op_resync before the hello reply", who)
					}
					if resyncs > 1 {
						w.violate("protocol:double-resync", "%s: %d op_resync messages on one connection", who, resyncs)
					}
					for _, op := range m.Ops {
						if !w.inCacheBetweenLocked(c.worker.slug, op.MachineID, c.scopeLo, c.scopeHi) {
							w.violate("leak:resync", "%s: op_resync carried op %s for machine %s, not in the slug's snapshot when the resync was scoped; params=%s",
								who, op.ID, propMachineName(op.MachineID), op.Params)
						}
					}
				}
				next := map[uuid.UUID]model.Operation{}
				for _, op := range m.Ops {
					if tv, ok := st.tomb[op.ID]; ok && op.Version <= tv {
						continue
					}
					next[op.ID] = op
				}
				st.ops = next
				st.resynced = true
			case TypeOpDelta:
				var m OpDeltaMsg
				if err := json.Unmarshal(r.data, &m); err != nil {
					w.violate("protocol:decode", "%s: op_delta: %v", who, err)
					continue
				}
				op := m.Operation
				if fresh {
					w.stats.deltasRecv.Add(1)
					if !st.resynced {
						w.violate("protocol:op-before-resync", "%s: op_delta for %s before op_resync", who, op.ID)
					}
					pub, ok := w.publishes[fmt.Sprintf("%s/%d", op.ID, op.Version)]
					switch {
					case !ok:
						w.violate("protocol:phantom-delta", "%s: op_delta %s v%d was never published", who, op.ID, op.Version)
					case !pub.elig[c.worker.slug] && !w.inCacheBetweenLocked(c.worker.slug, op.MachineID, pub.tick, w.tick):
						// Not eligible when published, and no later snapshot of
						// the slug held the machine, so no re-send explains it.
						w.violate("leak:delta", "%s: op_delta %s v%d for machine %s, never in the slug's snapshot since that version was published; params=%s",
							who, op.ID, op.Version, propMachineName(op.MachineID), op.Params)
					}
					if tv, dead := st.tomb[op.ID]; dead {
						w.violate("order:delta-after-removed", "%s: op_delta %s v%d arrived after op_removed v%d", who, op.ID, op.Version, tv)
					}
				}
				// The first delta for an op after a snapshot may be the hub's
				// re-send of the current row rather than a publish, and a
				// publish queued behind it may then carry an older version.
				// Every other delta is a publish and must not go backwards.
				if st.resendSeen != nil && !st.resendSeen[op.ID] {
					st.resendSeen[op.ID] = true
				} else {
					if last, seen := st.lastDelta[op.ID]; fresh && seen && op.Version < last {
						w.violate("order:delta-regress", "%s: op_delta %s v%d arrived after v%d", who, op.ID, op.Version, last)
					}
					if op.Version > st.lastDelta[op.ID] {
						st.lastDelta[op.ID] = op.Version
					}
				}
				if tv, ok := st.tomb[op.ID]; ok && op.Version <= tv {
					continue
				}
				if ex, ok := st.ops[op.ID]; ok && op.Version <= ex.Version {
					continue
				}
				st.ops[op.ID] = op
			case TypeOpRemoved:
				var m OpRemovedMsg
				if err := json.Unmarshal(r.data, &m); err != nil {
					w.violate("protocol:decode", "%s: op_removed: %v", who, err)
					continue
				}
				st.resendSeen = nil
				if w.sentinels[m.OperationID] {
					continue
				}
				if fresh {
					w.stats.removedRecv.Add(1)
					if !st.resynced {
						w.violate("protocol:op-before-resync", "%s: op_removed for %s before op_resync", who, m.OperationID)
					}
				}
				id, _ := uuid.Parse(m.OperationID)
				if cur, ok := st.tomb[id]; !ok || m.Version > cur {
					st.tomb[id] = m.Version
				}
				delete(st.ops, id)
			case TypeClaimOpResp, TypeError:
			default:
				if fresh {
					w.violate("protocol:unexpected-type", "%s: unexpected message type %q", who, r.typ)
				}
			}
		}
		c.checkedUpTo = len(lg)

		if c.clientClosed {
			continue
		}
		if closed {
			w.violate("liveness:closed", "%s: connection closed by the hub; log tail=%s", who, propTail(lg))
			continue
		}
		if !st.resynced {
			continue // already reported by connect
		}

		// --- quiescence rules ---
		cache := w.hubCache[c.worker.slug]
		if cache != nil {
			if st.snapSeq != cache.seq {
				w.violate("snapshot:stale", "%s: holds snapshot seq %d, latest for the slug is %d (hello last_seq=%d)", who, st.snapSeq, cache.seq, c.helloSeq)
			} else if st.machines != nil && !st.machines.equal(cache.machines) {
				w.violate("snapshot:content", "%s: seq %d machines %s, hub has %s", who, st.snapSeq, st.machines, cache.machines)
			}
		}
		for _, id := range w.opOrder {
			op := w.ops[id]
			held, has := st.ops[id]
			if !propActive(op.Status) {
				if has {
					w.violate("zombie-op", "%s: still holds terminal op %s (v%d, store status %s v%d)", who, id, held.Version, op.Status, op.Version)
				}
				continue
			}
			if cache == nil || !cache.machines[op.MachineID] {
				continue // not this site's op (stale retention is not checked here)
			}
			w.stats.reqOpsChecked.Add(1)
			if has && held.Version == op.Version {
				continue
			}
			// Missing or stale. Say whether the slug's snapshot held the
			// machine at the op's last publish and at the hello resync.
			lastPub := w.lastPub[id]
			detail := "missing"
			if has {
				detail = fmt.Sprintf("held at stale v%d", held.Version)
			}
			atPub := w.cacheAtLocked(c.worker.slug, lastPub)
			atResync := w.cacheAtLocked(c.worker.slug, c.scopeHi)
			w.violate("lost-op", "%s: active op %s (machine %s, %s v%d) %s; last publish at tick %d (machine in the slug's snapshot then: %v), hello resync scoped at tick %d (in the snapshot then: %v)",
				who, id, propMachineName(op.MachineID), op.Status, op.Version, detail,
				lastPub, atPub != nil && atPub.machines[op.MachineID],
				c.scopeHi, atResync != nil && atResync.machines[op.MachineID])
		}
	}
	active := 0
	for _, op := range w.ops {
		if propActive(op.Status) {
			active++
		}
	}
	propMax(&w.stats.maxActiveOps, int64(active))
}

func propTail(lg []propRecv) string {
	var parts []string
	start := 0
	if len(lg) > 6 {
		start = len(lg) - 6
	}
	for _, r := range lg[start:] {
		d := string(r.data)
		if len(d) > 160 {
			d = d[:160] + "..."
		}
		parts = append(parts, r.typ+":"+d)
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

func (w *propWorld) teardown() {
	for _, c := range w.clients {
		c.cancel()
		_ = c.conn.CloseNow()
	}
	w.hub.Close()
	w.srv.Close()
}

// report fails the case on any violation.
func (w *propWorld) report() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, v := range w.violations {
		w.stats.bump(&w.stats.violations, v[:strings.IndexByte(v, ' ')])
	}
	if len(w.violations) > 0 {
		w.t.Fatalf("%d violation(s):\n  %s", len(w.violations), strings.Join(w.violations, "\n  "))
	}
}

// ---------- generator: an op-enum history over the small pools ----------

type propKind int

const (
	pkConnect propKind = iota
	pkDisconnect
	pkContent
	pkRebuild
	pkCreateOp
	pkClaim
	pkClaimRace
	pkPhase
	pkTerminal
	pkConnectRace
	pkRestart
	pkMove
	pkSettle
)

const pkKinds = int(pkSettle) + 1

var propKindNames = [...]string{"connect", "disconnect", "content", "rebuild", "create_op", "claim", "claim_race",
	"phase", "terminal", "connect_race", "restart", "move", "settle"}

var propOpTypes = []model.OperationType{model.OpDeploy, model.OpCommission, model.OpPowerCycle, model.OpPowerOff}

// raceAction is something that happens inside a hello window.
func (w *propWorld) drawRaceAction(t *rapid.T, slug string) (string, func()) {
	m := propMachineID(rapid.IntRange(0, propMachineCount-1).Draw(t, "raceMachine"))
	typ := rapid.SampledFrom(propOpTypes).Draw(t, "raceOpType")
	switch rapid.IntRange(0, 5).Draw(t, "raceAction") {
	case 0:
		return "create_op(" + propMachineName(m) + ")", func() { w.createOp(m, typ) }
	case 1:
		return "add+rebuild+create_op(" + propMachineName(m) + ")", func() {
			w.mu.Lock()
			w.content[slug][m] = true
			w.mu.Unlock()
			w.rebuild(slug)
			w.createOp(m, typ)
		}
	case 2:
		return "remove+rebuild(" + propMachineName(m) + ")", func() {
			w.mu.Lock()
			delete(w.content[slug], m)
			w.mu.Unlock()
			w.rebuild(slug)
		}
	case 3:
		return "terminal(any)", func() {
			w.mu.Lock()
			var id uuid.UUID
			for _, oid := range w.opOrder {
				if propActive(w.ops[oid].Status) {
					id = oid
					break
				}
			}
			w.mu.Unlock()
			if id != uuid.Nil {
				w.terminal(id, false)
			}
		}
	case 4:
		return "phase(any)", func() {
			w.mu.Lock()
			var id uuid.UUID
			for _, oid := range w.opOrder {
				if s := w.ops[oid].Status; s == model.OpClaimed || s == model.OpRunning {
					id = oid
					break
				}
			}
			w.mu.Unlock()
			if id != uuid.Nil {
				w.phase(id)
			}
		}
	default:
		return "rebuild", func() { w.rebuild(slug) }
	}
}

func propRunCase(t *rapid.T, stats *propStats) {
	stats.cases.Add(1)
	w := newPropWorld(t, stats)
	defer w.teardown()

	// Swarm: each case enables a random subset of step kinds with random
	// weights, so some cases are connect-heavy, some op-heavy, some churn.
	weights := make([]int, pkKinds)
	var enabled []propKind
	for k := propKind(0); int(k) < pkKinds; k++ {
		if rapid.IntRange(0, 9).Draw(t, "enable_"+propKindNames[k]) < 7 {
			weights[k] = rapid.IntRange(1, 4).Draw(t, "weight_"+propKindNames[k])
		}
	}
	// The core kinds are always available so cases are never empty.
	for _, k := range []propKind{pkConnect, pkCreateOp, pkContent, pkRebuild} {
		if weights[k] == 0 {
			weights[k] = 1
		}
	}
	for k := propKind(0); int(k) < pkKinds; k++ {
		for i := 0; i < weights[k]; i++ {
			enabled = append(enabled, k)
		}
	}
	allowFutureSeq := rapid.IntRange(0, 9).Draw(t, "allowFutureSeq") == 0

	// Initial NetBox content: a small, overlapping-capable assignment.
	for i := 0; i < propMachineCount; i++ {
		switch rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("initSlug_M%d", i)) {
		case 0:
			w.content["a"][propMachineID(i)] = true
		case 1:
			w.content["b"][propMachineID(i)] = true
		case 2: // in neither
		case 3:
			w.content["a"][propMachineID(i)] = true
		}
	}
	if rapid.Bool().Draw(t, "prebuilt") {
		for _, slug := range propSlugs {
			w.rebuild(slug)
		}
	}

	drawOp := func(label string, pred func(*model.Operation) bool) (uuid.UUID, bool) {
		w.mu.Lock()
		var cands []uuid.UUID
		for _, id := range w.opOrder {
			if pred == nil || pred(w.ops[id]) {
				cands = append(cands, id)
			}
		}
		w.mu.Unlock()
		if len(cands) == 0 {
			return uuid.Nil, false
		}
		return cands[rapid.IntRange(0, len(cands)-1).Draw(t, label)], true
	}
	drawHelloSeq := func(slug string) int64 {
		w.mu.Lock()
		cur := w.pushedSeq[slug]
		w.mu.Unlock()
		n := 2
		if allowFutureSeq {
			n = 3
		}
		switch rapid.IntRange(0, n).Draw(t, "helloSeq") {
		case 0:
			return 0
		case 1:
			return cur
		case 2:
			if cur > 1 {
				return cur - 1
			}
			return 0
		default:
			return cur + 5
		}
	}

	n := rapid.IntRange(1, 40).Draw(t, "steps")
	for i := 0; i < n; i++ {
		stats.steps.Add(1)
		kind := enabled[rapid.IntRange(0, len(enabled)-1).Draw(t, "kind")]
		live := w.liveClients()
		switch kind {
		case pkConnect:
			wk := propWorkers[rapid.IntRange(0, len(propWorkers)-1).Draw(t, "worker")]
			seq := drawHelloSeq(wk.slug)
			w.tracef("connect %s hello.last_seq=%d", wk.id, seq)
			w.connect(wk, seq, false)
		case pkDisconnect:
			if len(live) > 0 {
				c := live[rapid.IntRange(0, len(live)-1).Draw(t, "conn")]
				w.tracef("disconnect conn#%d %s", c.id, c.worker.id)
				w.disconnect(c)
			}
		case pkContent:
			slug := rapid.SampledFrom(propSlugs).Draw(t, "slug")
			m := propMachineID(rapid.IntRange(0, propMachineCount-1).Draw(t, "machine"))
			w.mu.Lock()
			if w.content[slug][m] {
				delete(w.content[slug], m)
			} else {
				w.content[slug][m] = true
			}
			w.tracef("netbox content of slug %s is now %s (no rebuild yet)", slug, w.content[slug])
			w.mu.Unlock()
			stats.contentChanges.Add(1)
		case pkRebuild:
			slug := rapid.SampledFrom(propSlugs).Draw(t, "slug")
			s := w.rebuild(slug)
			w.tracef("rebuild slug %s -> seq %d machines %s", slug, s.seq, s.machines)
			stats.rebuilds.Add(1)
		case pkCreateOp:
			m := propMachineID(rapid.IntRange(0, propMachineCount-1).Draw(t, "machine"))
			typ := rapid.SampledFrom(propOpTypes).Draw(t, "opType")
			created := w.createOp(m, typ)
			w.tracef("create %s op for %s (created=%v) and publish op_delta", typ, propMachineName(m), created)
		case pkClaim:
			if len(live) == 0 {
				continue
			}
			c := live[rapid.IntRange(0, len(live)-1).Draw(t, "conn")]
			// Mostly claim something claimable; sometimes anything at all
			// (already claimed, terminal, another site's).
			var pred func(*model.Operation) bool
			if rapid.IntRange(0, 3).Draw(t, "claimAny") != 0 {
				pred = func(o *model.Operation) bool { return o.Status == model.OpPending }
			}
			id, ok := drawOp("op", pred)
			if !ok {
				id, ok = drawOp("op", nil)
			}
			if !ok || rapid.IntRange(0, 9).Draw(t, "unknownOp") == 0 {
				id = uuid.MustParse("99999999-0000-4000-8000-000000000001")
			}
			w.tracef("claim op %s from conn#%d %s", id, c.id, c.worker.id)
			w.claim(c, id)
		case pkClaimRace:
			if len(live) < 2 {
				continue
			}
			i1 := rapid.IntRange(0, len(live)-1).Draw(t, "conn1")
			i2 := rapid.IntRange(0, len(live)-2).Draw(t, "conn2")
			if i2 >= i1 {
				i2++
			}
			id, ok := drawOp("op", nil)
			if !ok {
				continue
			}
			w.tracef("racing claims of op %s from conn#%d %s and conn#%d %s", id, live[i1].id, live[i1].worker.id, live[i2].id, live[i2].worker.id)
			w.claimRace(live[i1], live[i2], id)
		case pkPhase:
			if id, ok := drawOp("op", func(o *model.Operation) bool { return o.Status == model.OpClaimed || o.Status == model.OpRunning }); ok {
				w.tracef("phase update of op %s, publish op_delta", id)
				w.phase(id)
			}
		case pkTerminal:
			if id, ok := drawOp("op", func(o *model.Operation) bool { return propActive(o.Status) }); ok {
				failed := rapid.Bool().Draw(t, "failed")
				w.tracef("terminal (failed=%v) op %s, publish op_removed", failed, id)
				w.terminal(id, failed)
			}
		case pkConnectRace:
			wk := propWorkers[rapid.IntRange(0, len(propWorkers)-1).Draw(t, "worker")]
			desc, action := w.drawRaceAction(t, wk.slug)
			point := rapid.IntRange(0, 3).Draw(t, "racePoint")
			seq := drawHelloSeq(wk.slug)
			w.mu.Lock()
			cold := w.hubCache[wk.slug] == nil
			switch point {
			case 0:
				w.hookTouch = action
				stats.raceTouch.Add(1)
			case 1:
				if cold {
					w.hookRebuild = action
					stats.raceRebuild.Add(1)
				} else {
					w.hookListPre = action
					stats.raceListPre.Add(1)
				}
			case 2:
				w.hookListPre = action
				stats.raceListPre.Add(1)
			default:
				w.hookListPost = action
				stats.raceLst.Add(1)
			}
			w.mu.Unlock()
			w.tracef("connect %s hello.last_seq=%d with %s inside the hello window (point=%d: 0 before register, 1 during cold rebuild else before resync read, 2 before resync read, 3 after resync read; hub cold=%v)",
				wk.id, seq, desc, point, cold)
			w.connect(wk, seq, true)
			w.mu.Lock()
			w.hookTouch, w.hookRebuild, w.hookListPre, w.hookListPost = nil, nil, nil, nil
			w.mu.Unlock()
		case pkRestart:
			w.tracef("restart the hub (all workers drop, snapshot cache is lost)")
			w.restartHub()
		case pkMove:
			m := propMachineID(rapid.IntRange(0, propMachineCount-1).Draw(t, "machine"))
			from := rapid.SampledFrom(propSlugs).Draw(t, "from")
			to := "a"
			if from == "a" {
				to = "b"
			}
			w.mu.Lock()
			delete(w.content[from], m)
			w.content[to][m] = true
			w.mu.Unlock()
			addFirst := rapid.Bool().Draw(t, "addFirst")
			w.tracef("move %s from slug %s to slug %s (add first=%v), rebuilding both", propMachineName(m), from, to, addFirst)
			if addFirst {
				w.rebuild(to)
				w.rebuild(from)
			} else {
				w.rebuild(from)
				w.rebuild(to)
			}
			stats.contentChanges.Add(1)
		case pkSettle:
			w.tracef("settle and check")
			w.settle()
			w.check()
		}
	}
	w.settle()
	w.check()
	w.report()
}

func TestProp_HubSchedule(t *testing.T) {
	stats := &propStats{}
	defer stats.report(t)
	rapid.Check(t, func(rt *rapid.T) { propRunCase(rt, stats) })
}
