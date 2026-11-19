//go:build integration

package qemutest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// testVersion is the build SHA the test hub + in-process site workers
// report on hello. The qemutest package can't reach `main.version`
// (it's in package main, not importable here), and `go test` doesn't
// pass -ldflags by default, so we get the SHA from the env. The
// integration workflow exports SLAGWORKS_TEST_VERSION=${{ github.sha }}
// so the version-pin code path is exercised against a realistic value.
// Local runs without the env var get "dev", which is also what
// production code gets from main.version when it isn't ldflag-stamped.
func testVersion() string {
	if v := os.Getenv("SLAGWORKS_TEST_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// FakeHub is the test-side replacement for slagworksd global. It runs
// a real ws.Hub under an httptest.Server, backed by fake Auth +
// Rebuilder + message handlers. The wire format is shared with
// production; only server-side state is test-specific.
//
// All mutable state is guarded by mu. WS goroutines and the test
// goroutine both touch observations/logBatches, so no code
// path reads or writes those without holding the lock.
type FakeHub struct {
	URL    string
	hub    *ws.Hub
	server *httptest.Server

	// testName captures env.t.Name() at construction time so timing
	// records emitted from WaitForObservation / WaitForObservationAfter
	// can attribute to the right test even if called from a goroutine.
	testName string

	mu sync.Mutex

	// Auth state: plaintext token -> SiteWorker. Populated by
	// RegisterWorker before the site worker dials.
	workersByHash map[string]*model.SiteWorker

	// Per-slug cached snapshot. Unlike the real hub which rebuilds on
	// hello when cold, we just return whatever the test last pushed
	// and synthesize an empty snapshot if nothing has been pushed
	// yet. Tests are expected to Push before connecting SiteWorker.
	snapshots map[string]model.SiteSnapshot

	// Per-slug active-op set (Tier 2 Phase 2). Operations left the
	// snapshot; SnapshotBuilder.Build registers them here. The real
	// ws.Hub embedded below serves this set on the hello op_resync (via
	// the fakeActiveOps reader) and Push fans them as op_deltas.
	activeOps map[string][]model.Operation

	// opUpdates records worker-emitted op_update messages keyed by op ID,
	// so tests can assert a terminal update arrived at the hub (rather
	// than accepting a local cache completion).
	opUpdates       map[string][]ws.OpUpdateMsg
	opUpdateWaiters []*opUpdateWaiter

	// opVersion tracks a monotonic per-op-ID version so a test that
	// re-registers the SAME op ID with a later state (e.g. pending →
	// claimed+awaiting_phone_home across two Push calls) gets a strictly
	// higher Version on each op_delta — otherwise the worker's
	// last-writer-wins guard would drop the second delta. Real global bumps
	// operations.version in every mutation's tx; this fake mimics that.
	opVersion map[string]int

	// Recording buffers. waiters is a per-slug+event list of waiters
	// that get signaled on matching arrivals.
	observations map[string][]recordedObservation
	logBatches   map[string][]ws.LogBatchMsg
	waiters      []*observationWaiter
}

type recordedObservation struct {
	Slug     string
	WorkerID string
	At       time.Time
	Obs      model.Observation
}

type observationWaiter struct {
	slug      string
	event     string
	machineID uuid.UUID // Nil means any
	done      chan recordedObservation
}

// opUpdateWaiter waits for a specific op_update (op ID + status) to be
// recorded at the hub.
type opUpdateWaiter struct {
	opID   string
	status string
	done   chan ws.OpUpdateMsg
}

// NewFakeHub starts a ws.Hub under an httptest.Server. Cleanup is
// registered via env.t.
func newFakeHub(env *Env) *FakeHub {
	fh := &FakeHub{
		testName:      env.t.Name(),
		workersByHash: make(map[string]*model.SiteWorker),
		snapshots:     make(map[string]model.SiteSnapshot),
		activeOps:     make(map[string][]model.Operation),
		opVersion:     make(map[string]int),
		observations:  make(map[string][]recordedObservation),
		logBatches:    make(map[string][]ws.LogBatchMsg),
		opUpdates:     make(map[string][]ws.OpUpdateMsg),
	}

	hub := ws.NewHub(ws.HubConfig{
		Auth:              fakeAuth{fh: fh},
		Rebuilder:         fakeRebuilder{fh: fh},
		ObsHandler:        fakeObsHandler{fh: fh},
		OpUpdateHandler:   fakeOpUpdateHandler{fh: fh},
		LogBatchHandler:   fakeLogBatchHandler{fh: fh},
		CastBatchHandler:  fakeCastBatchHandler{fh: fh},
		CastResumeHandler: fakeCastResumeHandler{fh: fh},
		// fakeActiveOps serves the fleet-wide active-op set (the union
		// across slugs — ws.ActiveOpsReader.ListActiveOperations has no
		// slug parameter, matching production's whole-fleet read). The
		// real hub embedded here then applies the production
		// confidentiality filter: the hello op_resync is scoped to ops
		// whose machine is in the conn's slug snapshot, so a multi-slug
		// test's workers each see only their own site's ops.
		ActiveOps:          fakeActiveOps{fh: fh},
		OpClaimer:          fakeOpClaimer{fh: fh},
		Logger:             nil,
		RebuildTimeout:     5 * time.Second,
		ObservationTimeout: 10 * time.Second,
		OpUpdateTimeout:    5 * time.Second,
		LogBatchTimeout:    5 * time.Second,
		Version:            testVersion(),
	})
	fh.hub = hub

	// wsconn dials <base>/api/v1/ws/site; mount the hub at that path
	// so the worker URL builder reaches the right endpoint.
	mux := http.NewServeMux()
	mux.Handle("/api/v1/ws/site", hub)
	srv := httptest.NewServer(mux)
	fh.URL = srv.URL // http://127.0.0.1:port; wsconn rewrites http->ws internally
	fh.server = srv

	env.t.Cleanup(func() {
		hub.Close()
		srv.Close()
	})
	return fh
}

// RegisterWorker creates an auth entry for a new site worker and
// returns a plaintext token. The site worker dials with
// `Authorization: Bearer <plaintext>` and FakeHub matches via hashed
// lookup (same as production).
func (fh *FakeHub) RegisterWorker(slug, workerID, fqdn string) string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("qemutest: crypto/rand: %v", err))
	}
	plaintext := hex.EncodeToString(b[:])
	hashed := store.HashWorkerToken(plaintext)

	fh.mu.Lock()
	fh.workersByHash[hashed] = &model.SiteWorker{
		ID:        uuid.New(),
		SiteSlug:  slug,
		WorkerID:  workerID,
		FQDN:      fqdn,
		TokenHash: hashed,
	}
	fh.mu.Unlock()
	return plaintext
}

// Push updates the cached snapshot for a slug and fans out to all
// connected workers. Test controls every state transition by pushing
// a new snapshot — the hub never synthesizes them.
//
// Tier 2: the op set (registered via SnapshotBuilder.Build →
// SetActiveOps) is fanned as op_deltas here too, carrying the transient
// machine-state overlay each claimed/running lifecycle op implies — the
// same shape a real claim/phase op_delta carries. This lets a test that
// Pushes a "deploying + claimed awaiting_phone_home" snapshot+op reach the
// worker's op layer, replacing the old ops-in-snapshot path.
//
// hub.Push runs FIRST so the hub's cached snapshot (which
// PublishOpDelta's slug-membership confidentiality filter checks) already
// contains the ops' machines when the deltas fan out.
func (fh *FakeHub) Push(slug string, snap model.SiteSnapshot) {
	fh.mu.Lock()
	fh.snapshots[slug] = snap
	ops := append([]model.Operation(nil), fh.activeOps[slug]...)
	fh.mu.Unlock()
	fh.hub.Push(slug, snap)
	for _, op := range ops {
		st := opDeltaState(op)
		fh.hub.PublishOpDelta(op, st)
	}
}

// SetActiveOps records the slug's active-op set (called by
// SnapshotBuilder.Build). The hub serves it on the hello op_resync and
// Push fans it as op_deltas. Each op gets a strictly-increasing Version
// stamped per op ID so re-registering the same op with a later state
// crosses the worker's last-writer-wins guard (see opVersion).
func (fh *FakeHub) SetActiveOps(slug string, ops []model.Operation) {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	stamped := make([]model.Operation, len(ops))
	for i, op := range ops {
		fh.opVersion[op.ID.String()]++
		op.Version = fh.opVersion[op.ID.String()]
		stamped[i] = op
	}
	fh.activeOps[slug] = stamped
}

// opDeltaState returns the machine-state overlay an op_delta should carry
// for a claimed/running lifecycle op (mirrors the hub's resync derivation
// via opstate.ClaimTransition). Pending ops carry no state.
func opDeltaState(op model.Operation) model.MachineState {
	if op.Status != model.OpClaimed && op.Status != model.OpRunning {
		return ""
	}
	target, _ := opstate.ClaimTransition(op.Type)
	return target
}

// WaitForObservation blocks until an observation matching (slug,
// event, machineID) arrives or ctx fires. Returns the full recorded
// observation. machineID=uuid.Nil matches any. Equivalent to
// WaitForObservationAfter(ctx, slug, event, machineID, 0) — i.e.
// considers ALL recorded observations including old ones.
func (fh *FakeHub) WaitForObservation(ctx context.Context, slug, event string, machineID uuid.UUID) (recordedObservation, error) {
	obs, _, err := fh.WaitForObservationAfter(ctx, slug, event, machineID, 0)
	return obs, err
}

// WaitForObservationAfter waits for an observation with index >=
// afterIdx in fh.observations[slug]. Returns the observation and
// its index+1 (usable as the next afterIdx). Callers that want to
// wait for a sequence of same-event observations should thread the
// returned index forward: passing 0 then N ensures the second wait
// blocks for something NEWER than the first.
func (fh *FakeHub) WaitForObservationAfter(
	ctx context.Context,
	slug, event string,
	machineID uuid.UUID,
	afterIdx int,
) (obsResult recordedObservation, nextIdx int, retErr error) {
	// Budget the wait itself (fast-wedge-detection): callers
	// reach this AFTER the relevant serial marker, so the observation
	// only has to traverse agent->site->WS — seconds, not the outer
	// test context's tens of minutes. Without this every observation
	// wait silently ran unbudgeted against the outer ctx, defeating
	// the budget table's whole purpose. commission_report is the one
	// long pole: the recommission test's SECOND wait spans a full
	// re-PXE cycle, so it gets the commission budget.
	budget := Budgets.HubObservation
	switch event {
	case "commission_report":
		// May span a full PXE boot + commission (the recommission
		// test's waits have no preceding serial gate).
		budget = Budgets.CommissionComplete
	case "phone_home":
		// Spans the post-install reboot: firmware -> boot-local ->
		// deployed-OS boot -> phone-home POST. Observed 10-27s at
		// -parallel 32.
		budget = Budgets.PhoneHomeObs
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	defer func() {
		outcome := "ok"
		if retErr != nil {
			if ctx.Err() != nil {
				outcome = "cancelled"
			} else {
				outcome = "err"
			}
		}
		emitTiming(fh.testName, "obs", event, time.Since(start), outcome)
	}()

	fh.mu.Lock()
	// Check already-recorded at or after afterIdx.
	list := fh.observations[slug]
	for i := afterIdx; i < len(list); i++ {
		o := list[i]
		if o.Obs.Event == event && (machineID == uuid.Nil || o.Obs.MachineID == machineID) {
			fh.mu.Unlock()
			return o, i + 1, nil
		}
	}
	w := &observationWaiter{
		slug:      slug,
		event:     event,
		machineID: machineID,
		done:      make(chan recordedObservation, 1),
	}
	fh.waiters = append(fh.waiters, w)
	fh.mu.Unlock()

	select {
	case o := <-w.done:
		// Re-scan under lock to figure out the matched index so the
		// caller can thread it forward. The signaled observation was
		// appended at the tail; its index is len-1 at signal time,
		// but since we drop the lock in between, we walk forward
		// from afterIdx to find the FIRST match again. In practice
		// this is also the one we signaled, since waiters are
		// single-shot.
		fh.mu.Lock()
		defer fh.mu.Unlock()
		list := fh.observations[slug]
		for i := afterIdx; i < len(list); i++ {
			c := list[i]
			if c.At.Equal(o.At) && c.Obs.Event == o.Obs.Event && c.Obs.MachineID == o.Obs.MachineID {
				return o, i + 1, nil
			}
		}
		// Shouldn't happen — the signal implies the record was
		// appended — but if it does, return len as the best cursor.
		return o, len(list), nil
	case <-ctx.Done():
		fh.mu.Lock()
		for i, ww := range fh.waiters {
			if ww == w {
				// Nil the old tail before shrinking so the
				// departing waiter's channel is eligible for GC.
				copy(fh.waiters[i:], fh.waiters[i+1:])
				fh.waiters[len(fh.waiters)-1] = nil
				fh.waiters = fh.waiters[:len(fh.waiters)-1]
				break
			}
		}
		fh.mu.Unlock()
		return recordedObservation{}, afterIdx, ctx.Err()
	}
}

// Observations returns a snapshot of everything recorded for a slug.
func (fh *FakeHub) Observations(slug string) []recordedObservation {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	out := make([]recordedObservation, len(fh.observations[slug]))
	copy(out, fh.observations[slug])
	return out
}

// LogBatches returns log_batch envelopes recorded for a slug.
func (fh *FakeHub) LogBatches(slug string) []ws.LogBatchMsg {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	out := make([]ws.LogBatchMsg, len(fh.logBatches[slug]))
	copy(out, fh.logBatches[slug])
	return out
}

// PowerStatus is a thin wrapper that lets a test drive the REAL
// ws.Hub.PowerStatus RPC against a connected site worker: the hub mints
// a request_id, RPCs any connected worker for the slug, and blocks for
// the worker's power_status_response. bmcRuntime is the marshalled
// api.BMCRuntime envelope (carries the creds; never logged).
func (fh *FakeHub) PowerStatus(ctx context.Context, siteSlug string, machineID uuid.UUID, bmcRuntime json.RawMessage) (ws.PowerStatusResponseMsg, error) {
	return fh.hub.PowerStatus(ctx, siteSlug, machineID, bmcRuntime, nil)
}

// --- hub collaborator implementations ---

type fakeAuth struct{ fh *FakeHub }

func (a fakeAuth) GetSiteWorkerByTokenHash(tokenHash string) (*model.SiteWorker, error) {
	a.fh.mu.Lock()
	defer a.fh.mu.Unlock()
	w, ok := a.fh.workersByHash[tokenHash]
	if !ok {
		return nil, store.ErrSiteWorkerNotFound
	}
	// Return a copy so callers outside the lock can't race with
	// TouchSiteWorker mutating FQDN on the same pointer.
	wc := *w
	return &wc, nil
}

func (a fakeAuth) TouchSiteWorker(slug, workerID, fqdn string, serial *string) (*model.SiteWorker, error) {
	// No-op — we don't track last_seen in the test env. Return a
	// plausible row so the hub's heartbeat path is satisfied.
	a.fh.mu.Lock()
	defer a.fh.mu.Unlock()
	for _, w := range a.fh.workersByHash {
		if w.SiteSlug == slug && w.WorkerID == workerID {
			if fqdn != "" {
				w.FQDN = fqdn
			}
			if serial != nil {
				w.Serial = *serial
			}
			wc := *w
			return &wc, nil
		}
	}
	return nil, errors.New("qemutest: unknown worker for touch")
}

// ListSiteWorkers returns the registered workers for a slug (the P4b
// interlock's INV-1 source). The qemu env doesn't exercise the outlet
// interlock, but the ws.WorkerAuthenticator interface requires it.
func (a fakeAuth) ListSiteWorkers(slug string) ([]model.SiteWorker, error) {
	a.fh.mu.Lock()
	defer a.fh.mu.Unlock()
	var out []model.SiteWorker
	for _, w := range a.fh.workersByHash {
		if w.SiteSlug == slug {
			out = append(out, *w)
		}
	}
	return out, nil
}

// fakeActiveOps serves the hub's hello op_resync from the per-slug
// active-op sets registered via SetActiveOps. Returns the union across all
// slugs (the production reader has no slug parameter); the one-slug-per-
// test common case is unaffected and workers filter by view membership.
//
// Terminal ops are filtered out to match the production reader's contract
// (ListActiveOperations reads WHERE status is active). A test may register
// a completed op (e.g. to fan its terminal op_delta via Push), but a resync
// must never carry it: applyOpResync's phoneHomed pruning checks set
// MEMBERSHIP, not terminality, so a terminal op in the resync would pin a
// stale `running` overlay on reconnect.
type fakeActiveOps struct{ fh *FakeHub }

func (a fakeActiveOps) ListActiveOperations() ([]model.Operation, error) {
	a.fh.mu.Lock()
	defer a.fh.mu.Unlock()
	var out []model.Operation
	for _, ops := range a.fh.activeOps {
		for _, op := range ops {
			if op.Status == model.OpCompleted || op.Status == model.OpFailed {
				continue
			}
			out = append(out, op)
		}
	}
	return out, nil
}

type fakeRebuilder struct{ fh *FakeHub }

func (r fakeRebuilder) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	r.fh.mu.Lock()
	defer r.fh.mu.Unlock()
	snap, ok := r.fh.snapshots[slug]
	if !ok {
		// Return an empty snapshot rather than erroring: the test
		// author controls whether a snapshot exists at connect time.
		return model.SiteSnapshot{Site: model.SiteInfo{Slug: slug}}, nil
	}
	return snap, nil
}

// slugMatches reports whether the waiter's slug is among the slugs
// the observation is being recorded under. Keeps a wrong-slug wait
// from being satisfied by another site's observation (it would
// otherwise pass or hang depending on arrival order).
func slugMatches(slugs map[string]struct{}, slug string) bool {
	_, ok := slugs[slug]
	return ok
}

type fakeObsHandler struct{ fh *FakeHub }

func (h fakeObsHandler) HandleObservation(ctx context.Context, obs model.Observation) error {
	// ws.ObservationHandler's interface only passes the Observation,
	// not the slug. To keep slug-scoped lookups working for the
	// one-slug-per-test common case, we record the observation
	// under EVERY registered worker's slug. A test asserting against
	// its own slug sees the record; a multi-slug test would see a
	// spurious duplicate — tests that need strict slug attribution
	// should use a per-slug hub instead.
	h.fh.mu.Lock()
	rec := recordedObservation{
		WorkerID: "",
		At:       time.Now(),
		Obs:      obs,
	}
	slugs := make(map[string]struct{})
	for _, w := range h.fh.workersByHash {
		slugs[w.SiteSlug] = struct{}{}
	}
	for slug := range slugs {
		r := rec
		r.Slug = slug
		h.fh.observations[slug] = append(h.fh.observations[slug], r)
	}
	// Satisfy any matching waiter.
	remaining := h.fh.waiters[:0]
	for _, w := range h.fh.waiters {
		if (w.machineID == uuid.Nil || w.machineID == obs.MachineID) && w.event == obs.Event && slugMatches(slugs, w.slug) {
			r := rec
			r.Slug = w.slug
			select {
			case w.done <- r:
				continue
			default:
			}
		}
		remaining = append(remaining, w)
	}
	// Nil out any tail slots we're shrinking away so the signaled
	// waiters are eligible for GC.
	for i := len(remaining); i < len(h.fh.waiters); i++ {
		h.fh.waiters[i] = nil
	}
	h.fh.waiters = remaining
	h.fh.mu.Unlock()

	return nil
}

// fakeOpUpdateHandler records worker-emitted op_update messages and, on a
// terminal update, advances the stored active op so a reconnect resync
// cannot resurrect it (matching the production store contract where a
// terminal mutation moves the row out of the active set).
type fakeOpUpdateHandler struct{ fh *FakeHub }

func (h fakeOpUpdateHandler) HandleOpUpdate(ctx context.Context, msg ws.OpUpdateMsg) error {
	h.fh.recordOpUpdate(msg)
	return nil
}

// recordOpUpdate appends an op_update message and satisfies any matching
// waiter. On a terminal status it also marks the stored active op
// terminal (completed/failed) so the fakeActiveOps resync reader filters
// it out.
func (fh *FakeHub) recordOpUpdate(msg ws.OpUpdateMsg) {
	fh.mu.Lock()
	defer fh.mu.Unlock()

	// Terminal status advances the stored active op in every slug that
	// carries it, so a later connect's op_resync cannot resurrect it.
	if msg.Status == string(model.OpCompleted) || msg.Status == string(model.OpFailed) {
		for slug := range fh.activeOps {
			ops := fh.activeOps[slug]
			for i := range ops {
				if ops[i].ID.String() == msg.OperationID {
					ops[i].Status = model.OperationStatus(msg.Status)
					ops[i].Result = msg.Result
					if msg.Phase != "" {
						ops[i].Phase = msg.Phase
					}
				}
			}
		}
	}

	fh.opUpdates[msg.OperationID] = append(fh.opUpdates[msg.OperationID], msg)

	// Satisfy any matching waiter.
	remain := fh.opUpdateWaiters[:0]
	for _, w := range fh.opUpdateWaiters {
		if w.opID == msg.OperationID && w.status == msg.Status {
			select {
			case w.done <- msg:
				continue
			default:
			}
		}
		remain = append(remain, w)
	}
	for i := len(remain); i < len(fh.opUpdateWaiters); i++ {
		fh.opUpdateWaiters[i] = nil
	}
	fh.opUpdateWaiters = remain
}

// fakeOpClaimer atomically claims a pending op for a worker — the ws.Hub
// OpClaimer the executor's ClaimOp RPC lands on. Pending -> claimed
// (with the worker's identity + monotonic version bump); anything not
// pending (already claimed/running/completed/failed) returns (nil, nil)
// so the worker treats it as a lost race.
type fakeOpClaimer struct{ fh *FakeHub }

func (c fakeOpClaimer) ClaimOperation(opID uuid.UUID, workerID string) (*model.Operation, error) {
	c.fh.mu.Lock()
	defer c.fh.mu.Unlock()
	for _, ops := range c.fh.activeOps {
		for i := range ops {
			if ops[i].ID != opID {
				continue
			}
			// Match the production at-least-once claim contract: if the
			// authenticated worker's claim committed but its response was lost
			// before any phase was recorded, hand the same claim back on retry.
			if opstate.CanReclaim(&ops[i], workerID) {
				cp := ops[i]
				return &cp, nil
			}
			if ops[i].Status != model.OpPending {
				return nil, nil
			}
			c.fh.opVersion[opID.String()]++
			now := time.Now()
			ops[i].Status = model.OpClaimed
			ops[i].ClaimedBy = workerID
			ops[i].ClaimedAt = &now
			ops[i].Version = c.fh.opVersion[opID.String()]
			cp := ops[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (c fakeOpClaimer) GetOperationIfExists(opID uuid.UUID) (*model.Operation, error) {
	c.fh.mu.Lock()
	defer c.fh.mu.Unlock()
	for _, ops := range c.fh.activeOps {
		for i := range ops {
			if ops[i].ID == opID {
				cp := ops[i]
				return &cp, nil
			}
		}
	}
	return nil, nil
}

// WaitForOpUpdate blocks until the hub records an op_update for opID
// with the given status, or the budget expires.
func (fh *FakeHub) WaitForOpUpdate(ctx context.Context, opID, status string) (ws.OpUpdateMsg, error) {
	ctx, cancel := context.WithTimeout(ctx, Budgets.BMCOpUpdate)
	defer cancel()

	fh.mu.Lock()
	for _, m := range fh.opUpdates[opID] {
		if m.Status == status {
			fh.mu.Unlock()
			return m, nil
		}
	}
	w := &opUpdateWaiter{opID: opID, status: status, done: make(chan ws.OpUpdateMsg, 1)}
	fh.opUpdateWaiters = append(fh.opUpdateWaiters, w)
	fh.mu.Unlock()

	select {
	case m := <-w.done:
		return m, nil
	case <-ctx.Done():
		fh.mu.Lock()
		for i, candidate := range fh.opUpdateWaiters {
			if candidate == w {
				copy(fh.opUpdateWaiters[i:], fh.opUpdateWaiters[i+1:])
				fh.opUpdateWaiters[len(fh.opUpdateWaiters)-1] = nil
				fh.opUpdateWaiters = fh.opUpdateWaiters[:len(fh.opUpdateWaiters)-1]
				break
			}
		}
		fh.mu.Unlock()
		return ws.OpUpdateMsg{}, ctx.Err()
	}
}

type fakeLogBatchHandler struct{ fh *FakeHub }

func (h fakeLogBatchHandler) HandleLogBatch(_ context.Context, _ string, msg ws.LogBatchMsg) error {
	h.fh.mu.Lock()
	defer h.fh.mu.Unlock()
	slugs := make(map[string]struct{})
	for _, w := range h.fh.workersByHash {
		slugs[w.SiteSlug] = struct{}{}
	}
	for slug := range slugs {
		h.fh.logBatches[slug] = append(h.fh.logBatches[slug], msg)
	}
	return nil
}

// fakeCastBatchHandler acks cast frames without recording. No qemu
// test asserts on cast content today; the accessor/override plumbing
// this once carried was speculative scaffolding — re-add it
// WITH the tests that read it.
type fakeCastBatchHandler struct{ fh *FakeHub }

func (h fakeCastBatchHandler) HandleCastBatch(ctx context.Context, workerID string, msg ws.CastBatchMsg) error {
	return nil
}

// fakeCastResumeHandler always answers zero offset (brand-new cast).
type fakeCastResumeHandler struct{ fh *FakeHub }

func (h fakeCastResumeHandler) HandleCastResume(ctx context.Context, workerID string, msg ws.CastResumeRequestMsg) (ws.CastResumeResponseMsg, error) {
	return ws.CastResumeResponseMsg{}, nil
}
