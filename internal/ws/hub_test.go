package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/store"
)

// fakeAuth implements WorkerAuthenticator for tests. It matches tokens
// by exact plaintext (hashed via store.HashWorkerToken to mirror what
// the hub will pass in).
type fakeAuth struct {
	mu       sync.Mutex
	byHash   map[string]*model.SiteWorker
	touchErr error
	touches  []touchCall
	// siteWorkers is the per-slug registered-worker set the P4b interlock's
	// INV-1 reads via ListSiteWorkers. listErr forces a fail-closed store
	// error. Both default to nil (empty set / no error).
	siteWorkers map[string][]model.SiteWorker
	listErr     error
}

type touchCall struct {
	slug, workerID, fqdn string
	serial               *string
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{byHash: make(map[string]*model.SiteWorker)}
}

func (f *fakeAuth) register(plaintext, slug, workerID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[store.HashWorkerToken(plaintext)] = &model.SiteWorker{
		ID: uuid.New(), SiteSlug: slug, WorkerID: workerID,
	}
}

func (f *fakeAuth) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.byHash[hash]
	if !ok {
		return nil, store.ErrSiteWorkerNotFound
	}
	return w, nil
}

func (f *fakeAuth) TouchSiteWorker(slug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touches = append(f.touches, touchCall{slug, workerID, fqdn, serial})
	if f.touchErr != nil {
		return nil, f.touchErr
	}
	w := &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}
	if serial != nil {
		w.Serial = *serial
	}
	return w, nil
}

func (f *fakeAuth) ListSiteWorkers(slug string) ([]model.SiteWorker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.siteWorkers[slug], nil
}

// setSiteWorkers configures the registered-worker set for a slug (INV-1).
func (f *fakeAuth) setSiteWorkers(slug string, workers ...model.SiteWorker) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.siteWorkers == nil {
		f.siteWorkers = make(map[string][]model.SiteWorker)
	}
	f.siteWorkers[slug] = workers
}

// fakeRebuilder returns a configured snapshot and counts invocations.
type fakeRebuilder struct {
	mu    sync.Mutex
	calls atomic.Int32
	snap  model.SiteSnapshot
	err   error
}

func (f *fakeRebuilder) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return model.SiteSnapshot{}, f.err
	}
	snap := f.snap
	snap.Site.Slug = slug
	return snap, nil
}

func (f *fakeRebuilder) callCount() int32 { return f.calls.Load() }

// fakeSerialResolver records the (slug, serial) it was asked to resolve and
// returns a configurable per-serial verdict.
type fakeSerialResolver struct {
	mu      sync.Mutex
	byERR   map[string]error // serial -> error to return (nil verdict = accept)
	calls   []serialCall
	fallErr error // returned for any serial not in byERR (nil = accept)
}

type serialCall struct{ slug, serial string }

func newFakeSerialResolver() *fakeSerialResolver {
	return &fakeSerialResolver{byERR: make(map[string]error)}
}

func (f *fakeSerialResolver) Resolve(_ context.Context, slug, serial string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, serialCall{slug, serial})
	if err, ok := f.byERR[serial]; ok {
		return err
	}
	return f.fallErr
}

func (f *fakeSerialResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newTestHubWithSerial builds a hub with a serial resolver wired in.
func newTestHubWithSerial(t *testing.T, sr WorkerSerialResolver) (*Hub, *fakeAuth) {
	t.Helper()
	auth := newFakeAuth()
	reb := &fakeRebuilder{}
	h := NewHub(HubConfig{Auth: auth, Rebuilder: reb, SerialResolver: sr, RebuildTimeout: 2 * time.Second})
	return h, auth
}

func newTestHub(t *testing.T) (*Hub, *fakeAuth, *fakeRebuilder) {
	t.Helper()
	auth := newFakeAuth()
	reb := &fakeRebuilder{}
	h := NewHub(HubConfig{Auth: auth, Rebuilder: reb, RebuildTimeout: 2 * time.Second})
	return h, auth, reb
}

func dialHub(t *testing.T, srv *httptest.Server, token string) *Conn {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	if token != "" {
		u.RawQuery = "token=" + token
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, u.String(), nil)
	if err != nil {
		return nil
	}
	return conn
}

func TestHub_UnauthorizedMissingToken(t *testing.T) {
	h, _, _ := newTestHub(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHub_UnauthorizedBadToken(t *testing.T) {
	h, _, _ := newTestHub(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?token=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestHub_HelloColdTriggersRebuild(t *testing.T) {
	h, auth, reb := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	reb.snap = model.SiteSnapshot{SeqNum: 5}

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", FQDN: "worker-1.pop3", LastSeqNum: 0, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeSnapshot {
		t.Fatalf("expected snapshot, got %q", env.Type)
	}
	if reb.callCount() != 1 {
		t.Fatalf("expected 1 rebuild, got %d", reb.callCount())
	}
}

func TestHub_HelloMatchingSeqReturnsNoChange(t *testing.T) {
	h, auth, reb := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")

	// Prime the hub with a cached snapshot at SeqNum=7.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 7, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 7, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeNoChange {
		t.Fatalf("expected no_change, got %q", env.Type)
	}
	if reb.callCount() != 0 {
		t.Fatal("rebuild should not have been called")
	}
}

func TestHub_HelloStaleSeqReturnsSnapshot(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 7, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 4, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeSnapshot {
		t.Fatalf("expected snapshot, got %q", env.Type)
	}
}

func TestHub_HelloWorkerIDMismatchRejected(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Claim a different worker_id than the token is bound to.
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-impostor", LastSeqNum: 0, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeError {
		t.Fatalf("expected error, got %q", env.Type)
	}
}

func TestHub_PushFansOutToAllWorkers(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-a", "pop3", "worker-a")
	auth.register("tok-b", "pop3", "worker-b")

	srv := httptest.NewServer(h)
	defer srv.Close()

	// Prime hub so hello doesn't trigger a rebuild.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	connA := dialHub(t, srv, "tok-a")
	if connA == nil {
		t.Fatal("dial A failed")
	}
	defer connA.CloseNow()
	connB := dialHub(t, srv, "tok-b")
	if connB == nil {
		t.Fatal("dial B failed")
	}
	defer connB.CloseNow()

	// Send hello from both and consume snapshot replies.
	for _, c := range []*Conn{connA, connB} {
		if err := c.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: workerIDFor(c, connA), LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
			t.Fatal(err)
		}
		env, err := c.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != TypeNoChange {
			t.Fatalf("expected no_change, got %q", env.Type)
		}
		// Drain the unconditional op_resync that follows the hello reply.
		if _, err := c.ReadMessage(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Wait briefly for hub.register to have both conns. The hello
	// round-trip above guarantees the register happened (serve runs
	// register before reading the first message), but we give the
	// read-side of the second worker a moment to settle.
	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.connections["pop3"]) == 2
	}, time.Second)

	// Fan-out: push a new snapshot, both workers should receive it.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 2, Site: model.SiteInfo{Slug: "pop3"}})

	for _, c := range []*Conn{connA, connB} {
		env, err := c.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != TypeSnapshot {
			t.Fatalf("expected snapshot, got %q", env.Type)
		}
	}
}

func TestHub_UnregisterOnDisconnect(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.connections["pop3"]) == 1
	}, time.Second)

	_ = conn.CloseNow()

	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.connections["pop3"]) == 0
	}, 2*time.Second)
}

// TestHub_ConcurrentHelloAllServed verifies that N workers hello'ing
// at once for the same cold slug each receive a snapshot through
// RebuildAndWait. Coalescing those concurrent calls onto one
// underlying rebuild is the registry's job, asserted in
// snapshot/registry_test.go (TestRegistryWaitersBatch) — the hub just
// delegates and must not deadlock or drop a reply under concurrency.
func TestHub_ConcurrentHelloAllServed(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-a", "pop3", "worker-a")
	auth.register("tok-b", "pop3", "worker-b")
	auth.register("tok-c", "pop3", "worker-c")

	// Swap in a gated rebuilder that holds all callers until we close
	// the gate, ensuring the hellos genuinely overlap.
	inner := &fakeRebuilder{snap: model.SiteSnapshot{SeqNum: 1}}
	gate := make(chan struct{})
	h.cfg.Rebuilder = &gatedRebuilder{inner: inner, gate: gate}

	srv := httptest.NewServer(h)
	defer srv.Close()

	tokens := []string{"tok-a", "tok-b", "tok-c"}
	workers := []string{"worker-a", "worker-b", "worker-c"}
	conns := make([]*Conn, len(tokens))
	for i, tok := range tokens {
		c := dialHub(t, srv, tok)
		if c == nil {
			t.Fatalf("dial %s failed", workers[i])
		}
		conns[i] = c
		defer c.CloseNow()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for i, c := range conns {
		if err := c.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: workers[i], LastSeqNum: 0, Proto: OpStreamProto}); err != nil {
			t.Fatal(err)
		}
	}

	// Give hub goroutines time to block on the gate together.
	time.Sleep(100 * time.Millisecond)
	close(gate)

	for _, c := range conns {
		env, err := c.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != TypeSnapshot {
			t.Fatalf("expected snapshot, got %q", env.Type)
		}
	}

	if got := inner.callCount(); got != int32(len(conns)) {
		t.Fatalf("expected %d RebuildAndWait delegations, got %d", len(conns), got)
	}
}

// gatedRebuilder blocks until the gate is closed, then delegates.
type gatedRebuilder struct {
	inner *fakeRebuilder
	gate  chan struct{}
}

func (g *gatedRebuilder) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	select {
	case <-g.gate:
	case <-ctx.Done():
		return model.SiteSnapshot{}, ctx.Err()
	}
	return g.inner.RebuildAndWait(ctx, slug)
}

func TestHub_RebuildErrorReturnedAsError(t *testing.T) {
	h, auth, reb := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	reb.err = errors.New("netbox down")

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeError {
		t.Fatalf("expected error, got %q", env.Type)
	}
}

func TestHub_CloseTerminatesActiveConnections(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	// Drain the unconditional op_resync so the post-Close read below
	// blocks on a genuinely empty stream and unblocks with an error.
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.connections["pop3"]) == 1
	}, time.Second)

	h.Close()

	// Read should unblock with an error now that the server-side
	// conn is closed.
	readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
	defer readCancel()
	if _, err := conn.ReadMessage(readCtx); err == nil {
		t.Fatal("expected read error after hub.Close()")
	}
}

// fakeObsHandler captures every observation and returns a configurable
// error. Used to verify the hub's ack contract.
type fakeObsHandler struct {
	mu       sync.Mutex
	received []model.Observation
	err      error
}

func (f *fakeObsHandler) HandleObservation(_ context.Context, obs model.Observation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, obs)
	return f.err
}

func (f *fakeObsHandler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

func TestHub_ObservationDispatchedToHandler_AcksOnSuccess(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	reb := &fakeRebuilder{}
	obsHandler := &fakeObsHandler{}

	h := NewHub(HubConfig{Auth: auth, Rebuilder: reb, ObsHandler: obsHandler})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Hello + drain reply + op_resync.
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}

	// Send an observation with a request_id; expect rpc_response ok.
	reqID := "req-commission-1"
	machineID := uuid.New()
	if err := conn.WriteTyped(ctx, TypeObservation, reqID, ObservationMsg{
		MachineID: machineID,
		Event:     "commission_report",
		Data:      []byte(`{"system":{}}`),
	}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeRPCResponse || env.RequestID != reqID {
		t.Fatalf("got %+v, want rpc_response with reqID %q", env, reqID)
	}
	var resp RPCResponseMsg
	if err := unmarshalData(env.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("expected ok, got error %q", resp.Error)
	}

	if obsHandler.count() != 1 {
		t.Fatalf("handler got %d obs, want 1", obsHandler.count())
	}
}

func TestHub_ObservationHandlerError_RepliesWithError(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	obsHandler := &fakeObsHandler{err: errors.New("netbox down")}

	h := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, ObsHandler: obsHandler})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}

	reqID := "req-commission-fail"
	if err := conn.WriteTyped(ctx, TypeObservation, reqID, ObservationMsg{
		MachineID: uuid.New(),
		Event:     "commission_report",
	}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeRPCResponse || env.RequestID != reqID {
		t.Fatalf("got %+v", env)
	}
	var resp RPCResponseMsg
	_ = unmarshalData(env.Data, &resp)
	if resp.Error == "" {
		t.Fatal("expected error in rpc_response, got empty")
	}
}

func waitUntil(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// workerIDFor returns "worker-a" for connA, "worker-b" otherwise. The
// fan-out test reuses the same loop body for two different workers.
func workerIDFor(c, a *Conn) string {
	if c == a {
		return "worker-a"
	}
	return "worker-b"
}

// --- OpUpdate + LogBatch dispatch tests ---

type fakeOpUpdateHandler struct {
	mu       sync.Mutex
	received []OpUpdateMsg
	err      error
}

func (f *fakeOpUpdateHandler) HandleOpUpdate(_ context.Context, msg OpUpdateMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, msg)
	return f.err
}

func (f *fakeOpUpdateHandler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

type fakeLogBatchHandler struct {
	mu       sync.Mutex
	received []LogBatchMsg
	err      error
}

func (f *fakeLogBatchHandler) HandleLogBatch(_ context.Context, _ string, msg LogBatchMsg) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, msg)
	return f.err
}

func (f *fakeLogBatchHandler) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.received)
}

// fakeOpClaimer captures every claim request and returns either a
// configured operation or a configured error. Used to verify the
// hub's claim contract.
type fakeOpClaimer struct {
	mu       sync.Mutex
	received []claimCall
	op       *model.Operation // returned on success; nil signals "not claimable"
	err      error            // returned as a hard failure (DB error, etc.)
	// pendingOp is what GetOperationIfExists returns — the pending op the P4b
	// interlock reads BEFORE the CAS. nil => (nil, nil) "op is gone". getErr
	// forces a fail-closed store error on the pre-CAS read.
	pendingOp *model.Operation
	getErr    error
}

type claimCall struct {
	OperationID uuid.UUID
	WorkerID    string // identity the hub asserts; never client-provided
}

func (f *fakeOpClaimer) ClaimOperation(opID uuid.UUID, workerID string) (*model.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, claimCall{OperationID: opID, WorkerID: workerID})
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

// setGetErr changes the pre-CAS read's injected error while the hub is
// serving, so a test can fail the read and then let the store recover.
func (f *fakeOpClaimer) setGetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *fakeOpClaimer) GetOperationIfExists(_ uuid.UUID) (*model.Operation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.pendingOp, nil
}

func (f *fakeOpClaimer) calls() []claimCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]claimCall, len(f.received))
	copy(out, f.received)
	return out
}

// fakeInvalidator records every Invalidate call. Used to verify the
// hub schedules a snapshot rebuild after a successful claim.
type fakeInvalidator struct {
	mu    sync.Mutex
	slugs []string
}

func (f *fakeInvalidator) Invalidate(slug string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slugs = append(f.slugs, slug)
}

func (f *fakeInvalidator) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.slugs))
	copy(out, f.slugs)
	return out
}

// TestHub_ClaimOp_HappyPath verifies the WS-channel claim flow:
// worker sends claim_op with op id (no client-provided worker_id —
// that field is gone from the wire format), hub calls the
// authenticated identity's ClaimOperation, sends back an
// rpc_response carrying ClaimOpResponse{Operation: <claimed op>}.
func TestHub_ClaimOp_HappyPath(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	opID := uuid.New()
	machineID := uuid.New()
	claimer := &fakeOpClaimer{
		op: &model.Operation{
			ID:        opID,
			MachineID: machineID,
			Type:      model.OpDeploy,
			Status:    model.OpClaimed,
			ClaimedBy: "worker-1",
		},
	}
	h := NewHub(HubConfig{
		Auth:      auth,
		Rebuilder: &fakeRebuilder{},
		OpClaimer: claimer,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	reqID := "claim-req-1"
	if err := conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{
		OperationID: opID.String(),
	}); err != nil {
		t.Fatal(err)
	}

	// A successful claim also publishes an op_delta (fanned concurrently),
	// so read until the claim_op_response arrives — order is not guaranteed.
	var env Envelope
	for {
		e, err := conn.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if e.Type == TypeOpDelta {
			continue
		}
		env = e
		break
	}
	if env.Type != TypeClaimOpResp {
		t.Fatalf("type: got %q, want %q", env.Type, TypeClaimOpResp)
	}
	if env.RequestID != reqID {
		t.Fatalf("request_id: got %q, want %q", env.RequestID, reqID)
	}
	var resp RPCResponseMsg
	if err := unmarshalData(env.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Fatalf("expected ok, got error %q", resp.Error)
	}
	var claimResp ClaimOpResponse
	if err := unmarshalData(resp.Data, &claimResp); err != nil {
		t.Fatalf("decode ClaimOpResponse: %v", err)
	}
	if claimResp.Operation == nil {
		t.Fatal("Operation: expected non-nil")
	}
	if claimResp.Operation.ID != opID {
		t.Errorf("Operation.ID: got %s, want %s", claimResp.Operation.ID, opID)
	}
	if claimResp.Operation.ClaimedBy != "worker-1" {
		t.Errorf("Operation.ClaimedBy: got %q, want worker-1", claimResp.Operation.ClaimedBy)
	}

	// The hub MUST have called ClaimOperation with the WS-authenticated
	// worker identity ("worker-1"), not with anything client-provided.
	calls := claimer.calls()
	if len(calls) != 1 {
		t.Fatalf("ClaimOperation called %d times, want 1", len(calls))
	}
	if calls[0].WorkerID != "worker-1" {
		t.Errorf("WorkerID: got %q, want %q (must come from authenticated identity)",
			calls[0].WorkerID, "worker-1")
	}
	if calls[0].OperationID != opID {
		t.Errorf("OperationID: got %s, want %s", calls[0].OperationID, opID)
	}
}

// TestHub_ClaimOp_AlreadyClaimed verifies the conflict path: when
// store.ClaimOperation returns (nil, nil) — meaning the op is no
// longer pending or has been claimed by someone else — the hub
// replies with a structured ClaimOpResponse{AlreadyClaimed: true}
// and NO error. Losing a claim race is the routine dual-worker
// outcome, not a failure; the worker maps it to (nil, nil) so the
// executor counts it as ClaimLost instead of ClaimErr.
func TestHub_ClaimOp_AlreadyClaimed(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	claimer := &fakeOpClaimer{
		op: nil, // store signals "no rows updated; already claimed"
	}
	inv := &fakeInvalidator{}
	h := NewHub(HubConfig{
		Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer,
		Invalidator: inv,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reqID := "claim-conflict"
	if err := conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{
		OperationID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeClaimOpResp || env.RequestID != reqID {
		t.Fatalf("envelope: got %+v", env)
	}
	var resp RPCResponseMsg
	if err := unmarshalData(env.Data, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "" {
		t.Errorf("already-claimed must NOT be an rpc error, got %q", resp.Error)
	}
	var claimResp ClaimOpResponse
	if err := unmarshalData(resp.Data, &claimResp); err != nil {
		t.Fatalf("decode ClaimOpResponse: %v", err)
	}
	if !claimResp.AlreadyClaimed {
		t.Error("AlreadyClaimed: got false, want true")
	}
	if claimResp.Operation != nil {
		t.Errorf("Operation: got %+v, want nil on lost claim", claimResp.Operation)
	}
	// A lost claim changed nothing — no snapshot rebuild needed.
	if got := inv.calls(); len(got) != 0 {
		t.Errorf("Invalidate called %d times on lost claim, want 0", len(got))
	}
}

// TestHub_ClaimOp_SuccessPublishesOpDelta verifies the Tier-2 behavior:
// a successful claim publishes an op_delta (the claimed row + its
// transient machine state) to connected workers, INSTEAD of invalidating
// the identity snapshot. This is what makes the claim-race loser see the
// op leave pending without a full snapshot rebuild — transient claim
// state rides the op stream, not identity. (Pre-Tier-2 this fired an
// Invalidate; claim no longer touches identity.)
func TestHub_ClaimOp_SuccessPublishesOpDelta(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	opID := uuid.New()
	machineID := uuid.New()
	claimer := &fakeOpClaimer{
		op: &model.Operation{
			ID: opID, MachineID: machineID, Type: model.OpDeploy,
			Status: model.OpClaimed, ClaimedBy: "worker-1", Version: 1,
		},
	}
	inv := &fakeInvalidator{}
	h := NewHub(HubConfig{
		Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer,
		Invalidator: inv,
	})
	// The delta's confidentiality filter requires the machine to be a
	// member of the conn's slug snapshot.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reqID := "claim-delta"
	if err := conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{
		OperationID: opID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	// The claim publishes an op_delta (fanned to all conns) AND replies
	// with the claim_op_response. Both arrive; order is not guaranteed, so
	// read until we see the op_delta carrying the claimed deploy row +
	// deploying transient state.
	sawDelta := false
	for range 2 {
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type == TypeOpDelta {
			var d OpDeltaMsg
			if err := unmarshalData(env.Data, &d); err != nil {
				t.Fatalf("decode op_delta: %v", err)
			}
			if d.Operation.ID != opID {
				t.Errorf("op_delta op ID: got %s, want %s", d.Operation.ID, opID)
			}
			if d.MachineState != model.MachineStateDeploying {
				t.Errorf("op_delta machine_state: got %q, want deploying (claim transient)", d.MachineState)
			}
			sawDelta = true
		}
	}
	if !sawDelta {
		t.Fatal("claim did not publish an op_delta")
	}
	// Claim must NOT invalidate identity — transient state rides the stream.
	if got := inv.calls(); len(got) != 0 {
		t.Errorf("claim must not invalidate identity, got %d calls", len(got))
	}
}

// TestHub_ClaimOp_DBError surfaces a hard ClaimOperation error to
// the worker via rpc_response.error. The worker treats this the
// same as already-claimed (drop the local intent) — the operator
// can re-issue if it was a transient DB issue.
func TestHub_ClaimOp_DBError(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	claimer := &fakeOpClaimer{err: errors.New("postgres exploded")}
	h := NewHub(HubConfig{
		Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reqID := "claim-dberr"
	if err := conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{
		OperationID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeClaimOpResp || env.RequestID != reqID {
		t.Fatalf("envelope: got %+v", env)
	}
	var resp RPCResponseMsg
	_ = unmarshalData(env.Data, &resp)
	if resp.Error == "" {
		t.Error("expected error in rpc_response when claim returns DB error")
	}
}

// TestHub_ClaimOp_BadOperationID surfaces a malformed op-id field
// as a decode error in the rpc_response, without ever touching the
// store. This is the wire-format-violation path.
func TestHub_ClaimOp_BadOperationID(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	claimer := &fakeOpClaimer{}
	h := NewHub(HubConfig{
		Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	reqID := "claim-baduuid"
	if err := conn.WriteTyped(ctx, TypeClaimOp, reqID, ClaimOpMsg{
		OperationID: "not-a-uuid",
	}); err != nil {
		t.Fatal(err)
	}
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeClaimOpResp || env.RequestID != reqID {
		t.Fatalf("envelope: got %+v", env)
	}
	var resp RPCResponseMsg
	_ = unmarshalData(env.Data, &resp)
	if resp.Error == "" {
		t.Error("expected error in rpc_response for malformed UUID")
	}
	if len(claimer.calls()) != 0 {
		t.Errorf("ClaimOperation must NOT be called for malformed op-id; got %d calls",
			len(claimer.calls()))
	}
}

// TestHub_ClaimOp_IgnoresClientWorkerID is the security-critical
// test: even if the worker connection is authenticated as
// "worker-A" and a malicious/buggy client tries to inject a
// different worker_id in the JSON payload, the hub uses the
// connection's authenticated identity, never the client field.
//
// Wire-format-wise this is a non-issue because the WorkerID field
// is removed from ClaimOpMsg. We assert it via the marshal-and-send
// path; if a future maintainer re-adds the field, this test fails.
func TestHub_ClaimOp_IgnoresClientWorkerID(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-A")
	opID := uuid.New()
	claimer := &fakeOpClaimer{
		op: &model.Operation{ID: opID, Status: model.OpClaimed, ClaimedBy: "worker-A"},
	}
	h := NewHub(HubConfig{
		Auth: auth, Rebuilder: &fakeRebuilder{}, OpClaimer: claimer,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-A")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Hand-build the envelope to inject an extra worker_id field
	// that the server-side ClaimOpMsg type doesn't have.
	// json.Unmarshal with default settings ignores unknown fields,
	// so the hub MUST source workerID from c.workerID, not the body.
	rawData := json.RawMessage(`{"op_id":"` + opID.String() + `","worker_id":"worker-IMPOSTOR"}`)
	if err := conn.WriteMessage(ctx, Envelope{
		Type:      TypeClaimOp,
		RequestID: "claim-impostor",
		Data:      rawData,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	calls := claimer.calls()
	if len(calls) != 1 {
		t.Fatalf("ClaimOperation called %d times, want 1", len(calls))
	}
	if calls[0].WorkerID != "worker-A" {
		t.Errorf("WorkerID: got %q, want worker-A — hub MUST ignore client-provided worker_id",
			calls[0].WorkerID)
	}
}

// helloAndDrain is a small helper used by op_update/log_lines tests
// that only care about the post-hello state.
func helloAndDrain(t *testing.T, conn *Conn, workerID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: workerID, LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	// Post-Tier-2 the hub sends TWO messages on hello: the snapshot/
	// no_change reply, then an unconditional op_resync. Drain both so a
	// subsequent read on this conn sees the message the test cares about.
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHub_OpUpdateDispatchedToHandler(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	opHandler := &fakeOpUpdateHandler{}
	h := NewHub(HubConfig{
		Auth:            auth,
		Rebuilder:       &fakeRebuilder{},
		OpUpdateHandler: opHandler,
	})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	opID := uuid.NewString()
	if err := conn.WriteTyped(ctx, TypeOpUpdate, "", OpUpdateMsg{
		OperationID: opID,
		Phase:       "partitioning",
	}); err != nil {
		t.Fatal(err)
	}

	// Fire-and-forget: no response expected. Poll the handler.
	waitUntil(t, func() bool { return opHandler.count() == 1 }, 2*time.Second)
	opHandler.mu.Lock()
	defer opHandler.mu.Unlock()
	got := opHandler.received[0]
	if got.OperationID != opID || got.Phase != "partitioning" {
		t.Fatalf("received: %+v", got)
	}
}

func TestHub_LogBatchDispatchedToHandler(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	logHandler := &fakeLogBatchHandler{}
	h := NewHub(HubConfig{
		Auth:            auth,
		Rebuilder:       &fakeRebuilder{},
		LogBatchHandler: logHandler,
	})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeLogBatch, "", LogBatchMsg{
		Envelopes: []slogkit.Envelope{
			{Source: slogkit.SourceAgent, Level: slogkit.LevelInfo, Msg: "starting"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool { return logHandler.count() == 1 }, 2*time.Second)
	logHandler.mu.Lock()
	defer logHandler.mu.Unlock()
	got := logHandler.received[0]
	if len(got.Envelopes) != 1 || got.Envelopes[0].Msg != "starting" {
		t.Fatalf("received: %+v", got)
	}
}

func TestHub_OpUpdateHandlerErrorLoggedButNotPropagated(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	opHandler := &fakeOpUpdateHandler{err: errors.New("db unreachable")}
	h := NewHub(HubConfig{
		Auth:            auth,
		Rebuilder:       &fakeRebuilder{},
		OpUpdateHandler: opHandler,
	})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeOpUpdate, "", OpUpdateMsg{
		OperationID: uuid.NewString(),
		Phase:       "partitioning",
	}); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, func() bool { return opHandler.count() == 1 }, 2*time.Second)
	// Critically: connection must stay open. Send a second message to
	// verify the read loop wasn't torn down.
	if err := conn.WriteTyped(ctx, TypeOpUpdate, "", OpUpdateMsg{
		OperationID: uuid.NewString(),
		Phase:       "rsyncing_os",
	}); err != nil {
		t.Fatalf("second write failed, connection probably dead: %v", err)
	}
	waitUntil(t, func() bool { return opHandler.count() == 2 }, 2*time.Second)
}

// captureHandler is a slog.Handler that records every emitted record
// for inspection. Used by version-pin tests.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

func TestHub_HelloVersionMismatchLogsWarning(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	cap := &captureHandler{}
	h := NewHub(HubConfig{
		Auth:           auth,
		Rebuilder:      &fakeRebuilder{},
		Logger:         slog.New(cap),
		Version:        "global-sha",
		RebuildTimeout: 2 * time.Second,
	})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")
	// helloAndDrain sends Version="" — that should NOT warn (empty
	// disables comparison). Now reconnect and send a real mismatch.
	conn.CloseNow()

	conn2 := dialHub(t, srv, "tok-1")
	if conn2 == nil {
		t.Fatal("dial 2 failed")
	}
	defer conn2.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn2.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 1, Version: "worker-sha", Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn2.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}

	// Eventual consistency: hub records the warn after reading hello.
	waitUntil(t, func() bool {
		for _, r := range cap.snapshot() {
			if r.Level == slog.LevelWarn && r.Message == "worker version mismatch" {
				return true
			}
		}
		return false
	}, 2*time.Second)

	// And: when the first connection sent Version="", no mismatch
	// warning should have fired for that hello.
	mismatches := 0
	for _, r := range cap.snapshot() {
		if r.Message == "worker version mismatch" {
			mismatches++
		}
	}
	if mismatches != 1 {
		t.Fatalf("expected exactly 1 mismatch warn, got %d", mismatches)
	}
}

// TestHub_MessagesCarryGlobalBuild: every message that tells a worker
// global's build SHA (the snapshot and no_change hello replies, and the
// pushed snapshot) also carries global's package version, which the
// worker hands to its auto-upgrade helper as the exact package to
// install.
func TestHub_MessagesCarryGlobalBuild(t *testing.T) {
	const sha, pkg = "global-sha", "0.3.0+git.4.gabc1234"
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	h := NewHub(HubConfig{
		Auth:           auth,
		Rebuilder:      &fakeRebuilder{},
		Version:        sha,
		PackageVersion: pkg,
		RebuildTimeout: 2 * time.Second,
	})
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	read := func(conn *Conn, wantType string, msg any) {
		t.Helper()
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if env.Type != wantType {
			t.Fatalf("got %q, want %q", env.Type, wantType)
		}
		if err := json.Unmarshal(env.Data, msg); err != nil {
			t.Fatalf("decode %s: %v", wantType, err)
		}
	}
	check := func(what, gotSHA, gotPkg string) {
		t.Helper()
		if gotSHA != sha || gotPkg != pkg {
			t.Fatalf("%s carried version=%q package_version=%q, want %q and %q",
				what, gotSHA, gotPkg, sha, pkg)
		}
	}

	// Stale seq: the hello reply is a snapshot.
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	var snap SnapshotMsg
	read(conn, TypeSnapshot, &snap)
	check("hello snapshot reply", snap.Version, snap.PackageVersion)
	var resync OpResyncMsg
	read(conn, TypeOpResync, &resync)

	// Fan-out push.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 2, Site: model.SiteInfo{Slug: "pop3"}})
	var pushed SnapshotMsg
	read(conn, TypeSnapshot, &pushed)
	check("pushed snapshot", pushed.Version, pushed.PackageVersion)
	conn.CloseNow()

	// Current seq: the hello reply is no_change.
	conn2 := dialHub(t, srv, "tok-1")
	if conn2 == nil {
		t.Fatal("dial 2 failed")
	}
	defer conn2.CloseNow()
	if err := conn2.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 2, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	var noChange NoChangeMsg
	read(conn2, TypeNoChange, &noChange)
	check("hello no_change reply", noChange.Version, noChange.PackageVersion)
}

// TestHub_PowerStatus_RoundTrip exercises the global→worker RPC the
// power-status read path rides on: the hub writes a power_status
// envelope with a fresh request_id to a connected worker for the
// site; the "worker" (this test's WS client) replies with a
// power_status_response carrying the same request_id; the blocked
// PowerStatus call decodes and returns the payload.
func TestHub_PowerStatus_RoundTrip(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop1", "worker-1")
	h.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	machineID := uuid.New()
	rt := json.RawMessage(`{"username":"slagworks","password":"pw","extra_config":{}}`)

	// Fake site worker: read the request, verify shape, reply.
	workerErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			workerErr <- err
			return
		}
		if env.Type != TypePowerStatus {
			workerErr <- fmt.Errorf("type: got %q, want %q", env.Type, TypePowerStatus)
			return
		}
		if env.RequestID == "" {
			workerErr <- fmt.Errorf("request_id must be set on power_status")
			return
		}
		var req PowerStatusRequestMsg
		if err := unmarshalData(env.Data, &req); err != nil {
			workerErr <- err
			return
		}
		if req.MachineID != machineID {
			workerErr <- fmt.Errorf("machine_id: got %s, want %s", req.MachineID, machineID)
			return
		}
		if string(req.BMCRuntime) != string(rt) {
			workerErr <- fmt.Errorf("bmc_runtime: got %s, want %s", req.BMCRuntime, rt)
			return
		}
		out, _ := json.Marshal(PowerStatusResponseMsg{State: "on", Transport: "redfish"})
		workerErr <- conn.WriteTyped(ctx, TypePowerStatusResp, env.RequestID, RPCResponseMsg{Data: out})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := h.PowerStatus(ctx, "pop1", machineID, rt, nil)
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if resp.State != "on" || resp.Transport != "redfish" {
		t.Errorf("got %+v, want {on redfish}", resp)
	}
	if err := <-workerErr; err != nil {
		t.Fatalf("fake worker: %v", err)
	}
	// The pending entry must be cleaned up after the call returns.
	h.rpcMu.Lock()
	n := len(h.rpcPending)
	h.rpcMu.Unlock()
	if n != 0 {
		t.Errorf("rpcPending leaked %d entries", n)
	}
}

// TestHub_PowerStatus_WorkerError covers the worker replying with
// rpc_response{error}: the error string must reach the caller.
func TestHub_PowerStatus_WorkerError(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop1", "worker-1")
	h.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"}})

	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			return
		}
		_ = conn.WriteTyped(ctx, TypePowerStatusResp, env.RequestID,
			RPCResponseMsg{Error: "bmc connect: auth failed"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := h.PowerStatus(ctx, "pop1", uuid.New(), nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("error %q must carry the worker's reason", err)
	}
}

// TestHub_PowerStatus_NoWorker pins the no-connected-worker outcome:
// ErrNoWorkerForSite, immediately (not after the ctx timeout). The
// API maps this onto a 503 with operator guidance.
func TestHub_PowerStatus_NoWorker(t *testing.T) {
	h, _, _ := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := h.PowerStatus(ctx, "pop9", uuid.New(), nil, nil)
	if !errors.Is(err, ErrNoWorkerForSite) {
		t.Fatalf("expected ErrNoWorkerForSite, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Error("no-worker case must fail fast, not wait for ctx")
	}
}

// TestHub_PowerStatus_Timeout covers a worker that accepts the
// request but never replies: the caller's ctx bounds the wait and the
// pending entry is cleaned up.
func TestHub_PowerStatus_Timeout(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop1", "worker-1")
	h.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"}})

	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")
	// Deliberately never read/reply on the conn.

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := h.PowerStatus(ctx, "pop1", uuid.New(), nil, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded in chain, got %v", err)
	}
	h.rpcMu.Lock()
	n := len(h.rpcPending)
	h.rpcMu.Unlock()
	if n != 0 {
		t.Errorf("rpcPending leaked %d entries after timeout", n)
	}
}

// --- Tier 2 op-stream tests ---

// fakeActiveOpsReader serves a fixed active-op set for the hello resync.
type fakeActiveOpsReader struct{ ops []model.Operation }

func (f fakeActiveOpsReader) ListActiveOperations() ([]model.Operation, error) {
	return f.ops, nil
}

// TestHub_SkewGate_RefusesOldHello pins the loud skew gate: a hello with an
// absent/lower Proto (a pre-op-stream worker) is refused at the connection
// level with an error naming the fix, NOT silently served.
func TestHub_SkewGate_RefusesOldHello(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Old worker: no Proto field (JSON zero -> 0).
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 0}); err != nil {
		t.Fatal(err)
	}
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeError {
		t.Fatalf("expected error reply for old-proto hello, got %q", env.Type)
	}
	var em ErrorMsg
	if err := unmarshalData(env.Data, &em); err != nil {
		t.Fatal(err)
	}
	if em.Code != "proto_mismatch" {
		t.Errorf("error code = %q, want proto_mismatch", em.Code)
	}
	if !strings.Contains(em.Message, "upgrade the slagworks package") {
		t.Errorf("error message must name the fix, got %q", em.Message)
	}
	if em.Proto != OpStreamProto {
		t.Errorf("error must carry global's proto %d, got %d", OpStreamProto, em.Proto)
	}
}

// TestHub_HelloSendsOpResync pins that the hub sends an unconditional
// op_resync after the hello reply, carrying the active-op set + the derived
// transient overlay for claimed/running lifecycle ops.
func TestHub_HelloSendsOpResync(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	machineID := uuid.New()
	foreignMachine := uuid.New()
	claimedOp := model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy,
		Status: model.OpClaimed, Version: 2,
	}
	// A foreign site's op in the fleet-wide store set: its params carry
	// that site's credentials and must NOT reach this worker's resync.
	foreignOp := model.Operation{
		ID: uuid.New(), MachineID: foreignMachine, Type: model.OpDeploy,
		Status: model.OpClaimed, Version: 9,
	}
	h := NewHub(HubConfig{
		Auth:      auth,
		Rebuilder: &fakeRebuilder{},
		ActiveOps: fakeActiveOpsReader{ops: []model.Operation{claimedOp, foreignOp}},
	})
	// pop3's snapshot contains only machineID — foreignMachine belongs to
	// another site.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	// First reply: no_change (seq matches).
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}
	// Second: op_resync.
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeOpResync {
		t.Fatalf("expected op_resync after hello, got %q", env.Type)
	}
	var rs OpResyncMsg
	if err := unmarshalData(env.Data, &rs); err != nil {
		t.Fatal(err)
	}
	// Confidentiality filter: only pop3's op — the foreign site's op (and
	// its credential-bearing params) must be absent.
	if len(rs.Ops) != 1 || rs.Ops[0].ID != claimedOp.ID {
		t.Fatalf("resync ops = %+v, want ONLY pop3's claimed op (foreign op filtered)", rs.Ops)
	}
	if rs.MachineStates[machineID.String()] != model.MachineStateDeploying {
		t.Errorf("resync overlay for claimed deploy = %q, want deploying", rs.MachineStates[machineID.String()])
	}
	if _, ok := rs.MachineStates[foreignMachine.String()]; ok {
		t.Error("resync overlay must not leak the foreign machine's state")
	}
}

// TestHub_MissedDelta_ForceClosesOnWriteFailure is the missed-delta
// self-heal: an op-stream write to a dead connection force-closes it (so
// the reconnect replays via op_resync) rather than silently stranding the
// op. We close the client conn, then publish; the hub's fan-out write fails
// and the server-side conn is force-closed — observable as the worker's
// serve loop unwinding and the connection unregistering.
func TestHub_MissedDelta_ForceClosesOnWriteFailure(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	machineID := uuid.New()
	// The machine must be in pop3's snapshot so the delta's membership
	// filter lets the write through (and hit the dead conn).
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	helloAndDrain(t, conn, "worker-1")
	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.connections["pop3"]) == 1
	}, time.Second)

	// Kill the client side so the hub's next write fails.
	_ = conn.CloseNow()

	// Publish an op_delta; the hub's write fails and force-closes the conn,
	// which unregisters it. Assert the connection set drains.
	h.PublishOpDelta(model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending, Version: 1}, "")

	waitUntil(t, func() bool {
		h.mu.RLock()
		defer h.mu.RUnlock()
		return len(h.connections["pop3"]) == 0
	}, 3*time.Second)
}

// TestSendOpMessage_DroppedBeforeResync pins the register-before-resync
// gate's drop half: an op-stream message targeting a conn whose op_resync
// hasn't been sent (opReady=false) is dropped without touching the
// connection. Safe by the mutate-then-publish invariant — a publisher that
// observed opReady=false committed its DB mutation before the resync's DB
// read, so the mutation rides the resync. The nil conn proves no write was
// attempted (a write would panic).
func TestSendOpMessage_DroppedBeforeResync(t *testing.T) {
	h, _, _ := newTestHub(t)
	c := &workerConn{hub: h, conn: nil, slug: "pop3", workerID: "w1", log: h.log}
	// opReady is false (zero value): must return without writing.
	h.sendOpMessage(c, TypeOpDelta, OpDeltaMsg{}, "op_delta", "x")
}

// blockingActiveOps blocks ListActiveOperations until released, signalling
// entry — lets a test wedge a publish INSIDE the resync's critical section.
type blockingActiveOps struct {
	ops     []model.Operation
	entered chan struct{}
	release chan struct{}
}

func (b *blockingActiveOps) ListActiveOperations() ([]model.Operation, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.ops, nil
}

// TestHub_PublishDuringResyncOrdersAfter pins the gate's ordering half: a
// PublishOp* that fires while the hello op_resync is mid-flight (opReady
// already true, resync's DB read in progress) blocks on the per-conn
// opStreamMu and is delivered STRICTLY AFTER the resync — never before,
// where the resync's full-replace would clobber it on the worker.
func TestHub_PublishDuringResyncOrdersAfter(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	machineID := uuid.New()
	resyncOp := model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy,
		Status: model.OpPending, Version: 1,
	}
	bl := &blockingActiveOps{
		ops:     []model.Operation{resyncOp},
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	h := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}, ActiveOps: bl})
	// machineID must be a member of pop3's snapshot so both the resync
	// filter and the delta filter pass.
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	// Wait until the resync is inside its critical section (opReady=true,
	// DB read in flight), then publish a newer delta for the same op. The
	// publish must block on opStreamMu and land AFTER the resync.
	<-bl.entered
	newer := resyncOp
	newer.Status = model.OpClaimed
	newer.Version = 2
	published := make(chan struct{})
	go func() {
		h.PublishOpDelta(newer, model.MachineStateDeploying)
		close(published)
	}()
	// Give the publish goroutine a moment to reach the mutex, then let the
	// resync proceed.
	time.Sleep(50 * time.Millisecond)
	close(bl.release)

	// Wire order: no_change, then op_resync, THEN the delta.
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeNoChange {
		t.Fatalf("first: got %q, want no_change", env.Type)
	}
	env, err = conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeOpResync {
		t.Fatalf("second: got %q, want op_resync (a mid-resync publish must not jump ahead)", env.Type)
	}
	env, err = conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeOpDelta {
		t.Fatalf("third: got %q, want the deferred op_delta", env.Type)
	}
	var d OpDeltaMsg
	if err := unmarshalData(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Operation.Version != 2 {
		t.Fatalf("delta version = %d, want 2 (the newer post-resync state)", d.Operation.Version)
	}
	<-published
}

// TestHub_OpDeltaIsSlugScoped pins the confidentiality fix: an op_delta
// (whose Operation.Params carries per-site credentials) is written ONLY to
// workers whose slug snapshot contains the op's machine. Two conns on two
// slugs; the pop3 machine's delta reaches the pop3 conn and never the pop1
// conn — preserving the pre-cutover baseline where ops rode the per-slug
// snapshot.
func TestHub_OpDeltaIsSlugScoped(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-3", "pop3", "worker-3")
	auth.register("tok-1", "pop1", "worker-1")
	pop3Machine := uuid.New()
	pop1Machine := uuid.New()
	hub := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}})
	hub.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: pop3Machine}}})
	hub.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"},
		Machines: []model.MachineSnapshot{{ID: pop1Machine}}})
	srv := httptest.NewServer(hub)
	defer srv.Close()

	conn3 := dialHub(t, srv, "tok-3")
	if conn3 == nil {
		t.Fatal("dial pop3 failed")
	}
	defer conn3.CloseNow()
	helloAndDrain(t, conn3, "worker-3")
	conn1 := dialHub(t, srv, "tok-1")
	if conn1 == nil {
		t.Fatal("dial pop1 failed")
	}
	defer conn1.CloseNow()
	helloAndDrain(t, conn1, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Delta for the pop3 machine, params carrying a (fake) secret.
	op := model.Operation{
		ID: uuid.New(), MachineID: pop3Machine, Type: model.OpDeploy,
		Status: model.OpPending, Version: 1,
		Params: json.RawMessage(`{"bmc_runtime":{"password":"hunter2"}}`),
	}
	hub.PublishOpDelta(op, "")

	// pop3 conn receives it.
	env, err := conn3.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeOpDelta {
		t.Fatalf("pop3 conn: got %q, want op_delta", env.Type)
	}

	// pop1 conn must receive NOTHING for it. Prove it by publishing a
	// pop1-machine delta next: the first (and only) message pop1 sees is
	// that one, not the pop3 secret-bearing delta.
	marker := model.Operation{
		ID: uuid.New(), MachineID: pop1Machine, Type: model.OpDeploy,
		Status: model.OpPending, Version: 1,
	}
	hub.PublishOpDelta(marker, "")
	env, err = conn1.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeOpDelta {
		t.Fatalf("pop1 conn: got %q, want op_delta", env.Type)
	}
	var d OpDeltaMsg
	if err := unmarshalData(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Operation.ID != marker.ID {
		t.Fatalf("pop1 conn received op %s, want only its own marker %s — cross-site delta leaked", d.Operation.ID, marker.ID)
	}
}

// TestHub_OpRemovedReachesNonMemberWorker pins the deliberate asymmetry:
// op_removed fans fleet-wide, UNFILTERED — it carries no params (no
// secrets), and it must reach a worker whose snapshot no longer contains
// the machine (the machine-deleted-from-NetBox drift edge: a membership
// check would drop the removal and strand a zombie active op on the
// worker that was driving it).
func TestHub_OpRemovedReachesNonMemberWorker(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop3", "worker-1")
	hub := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{}})
	// pop3's snapshot contains NO machines (the machine was deleted from
	// NetBox and the identity rebuild already dropped it).
	hub.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-1")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gone := model.Operation{
		ID: uuid.New(), MachineID: uuid.New(), Type: model.OpDeploy,
		Status: model.OpFailed, Version: 3,
	}
	hub.PublishOpRemoved(gone, model.MachineStateFailed)

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeOpRemoved {
		t.Fatalf("got %q, want op_removed despite the machine being absent from the snapshot", env.Type)
	}
	var rm OpRemovedMsg
	if err := unmarshalData(env.Data, &rm); err != nil {
		t.Fatal(err)
	}
	if rm.OperationID != gone.ID.String() || rm.MachineState != model.MachineStateFailed {
		t.Fatalf("op_removed = %+v, want op %s with failed state", rm, gone.ID)
	}
}

// TestHub_ColdHelloCachesSnapshotForOpDeltaGate pins the cold-hello
// caching fix: the dispatcher only Pushes (and so populates h.snapshots)
// on a content-hash change, so after a global restart with unchanged
// content the hello-path rebuild is the ONLY source of the slug's
// snapshot. If handleHello doesn't cache it, machineInSlug (the op-stream
// confidentiality gate) sees no membership and silently drops every
// op_delta for the worker until an unrelated Push.
func TestHub_ColdHelloCachesSnapshotForOpDeltaGate(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-3", "pop3", "worker-3")
	machineID := uuid.New()
	// Cold hub: no Push. The rebuild actor supplies the snapshot.
	reb := &fakeRebuilder{snap: model.SiteSnapshot{
		SeqNum: 7, Site: model.SiteInfo{Slug: "pop3"},
		Machines: []model.MachineSnapshot{{ID: machineID}},
	}}
	hub := NewHub(HubConfig{Auth: auth, Rebuilder: reb})
	srv := httptest.NewServer(hub)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-3")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-3")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	op := model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy,
		Status: model.OpPending, Version: 1,
	}
	hub.PublishOpDelta(op, "")

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("op_delta never arrived (cold-hello snapshot not cached for the membership gate): %v", err)
	}
	if env.Type != TypeOpDelta {
		t.Fatalf("got %q, want op_delta", env.Type)
	}
}

// TestHub_WorkerSMCIPMITool: the SMCIPMITool status a worker reports in its
// hello is readable per connected worker, and the Supermicro count comes
// from the hub's cached snapshot — together they drive the
// `slagctl worker list` install hint.
func TestHub_WorkerSMCIPMITool(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 7, Site: model.SiteInfo{Slug: "pop3"}, Machines: []model.MachineSnapshot{
		{ID: uuid.New(), Manufacturer: "supermicro"},
		{ID: uuid.New(), Manufacturer: "supermicro"},
		{ID: uuid.New(), Manufacturer: "dell"},
	}})

	srv := httptest.NewServer(h)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 7, Proto: OpStreamProto, SMCIPMITool: "missing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ReadMessage(ctx); err != nil {
		t.Fatal(err)
	}

	if status, ok := h.WorkerSMCIPMITool("pop3", "worker-1"); !ok || status != "missing" {
		t.Errorf("WorkerSMCIPMITool(connected) = %q, %v; want missing, true", status, ok)
	}
	if status, ok := h.WorkerSMCIPMITool("pop3", "worker-2"); ok || status != "" {
		t.Errorf("WorkerSMCIPMITool(unknown worker) = %q, %v; want \"\", false", status, ok)
	}
	if got := h.SupermicroMachineCount("pop3"); got != 2 {
		t.Errorf("SupermicroMachineCount(pop3) = %d, want 2", got)
	}
	if got := h.SupermicroMachineCount("nope"); got != 0 {
		t.Errorf("SupermicroMachineCount(no snapshot) = %d, want 0", got)
	}
}
