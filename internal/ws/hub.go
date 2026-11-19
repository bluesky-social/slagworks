package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opsmetrics"
	"github.com/bluesky-social/slagworks/internal/opstate"
	"github.com/bluesky-social/slagworks/internal/store"
)

// WorkerAuthenticator authenticates a site worker by the plaintext
// token value pulled off the ?token= query string at WS upgrade time.
// Returns ErrSiteWorkerNotFound on invalid/missing token; callers 401
// without disambiguating.
type WorkerAuthenticator interface {
	GetSiteWorkerByTokenHash(tokenHash string) (*model.SiteWorker, error)
	// TouchSiteWorker refreshes last_seen_at and, when non-nil, records the
	// fqdn (non-empty only) and serial. serial is a pointer so the hello
	// path (serial != nil) can persist the worker's verified hardware serial
	// — including "" for an unverified worker — while heartbeats (serial ==
	// nil) leave the stored value untouched.
	TouchSiteWorker(siteSlug, workerID, fqdn string, serial *string) (*model.SiteWorker, error)
	// ListSiteWorkers returns the registered site workers for a slug
	// (connected OR not). The P4b outlet-op interlock reads this at claim
	// time to enforce INV-1 (never power `off` a serving worker's feed):
	// worker-ness is resolved from the PERSISTED registered set, not from
	// live connections — a disconnected registered worker is still a serving
	// worker (and the most dangerous one to cut). *store.Store satisfies it.
	ListSiteWorkers(siteSlug string) ([]model.SiteWorker, error)
}

// Rebuilder is the subset of snapshot.Registry the hub calls to force
// a rebuild when a hello arrives and the hub has no usable cached
// snapshot for the slug. RebuildAndWait enqueues a waiter on the
// slug's rebuild actor and blocks for the result; concurrent hellos
// for the same slug batch onto one rebuild, so the hub needs no
// coalescing of its own.
//
// Kept as an interface so tests can provide a fake and so the ws
// package does not have to import internal/snapshot (which would drag
// in netbox, mirror, and their transitive dependencies).
type Rebuilder interface {
	RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error)
}

// ObservationHandler processes worker-origin observation envelopes.
// A nil-error return from Handle signals the hub to send
// rpc_response{ok:true} to the worker, which in turn acks the
// observation out of its local SQLite outbox. A non-nil return sends
// rpc_response{error:"..."} and the worker leaves the outbox row for
// the next reconnect to replay. Handlers MUST be idempotent — see
// internal/commission for the commission-report path.
type ObservationHandler interface {
	HandleObservation(ctx context.Context, obs model.Observation) error
}

// noopObservationHandler accepts every observation with a nil error.
// Used when the hub is constructed without a handler (unit tests that
// don't exercise the commission path).
type noopObservationHandler struct{}

func (noopObservationHandler) HandleObservation(_ context.Context, _ model.Observation) error {
	return nil
}

// OpUpdateHandler persists worker-emitted operation updates (phase
// transitions, terminal status with result). Fire-and-forget from the
// worker's perspective — there's no rpc_response contract — so errors
// are logged at the hub, not propagated back. MUST be idempotent:
// workers may resend the same op_update after a reconnect.
type OpUpdateHandler interface {
	HandleOpUpdate(ctx context.Context, msg OpUpdateMsg) error
}

type noopOpUpdateHandler struct{}

func (noopOpUpdateHandler) HandleOpUpdate(_ context.Context, _ OpUpdateMsg) error {
	return nil
}

// LogBatchHandler persists structured log envelopes pushed by site
// workers. Fire-and-forget contract: errors are logged but never
// signalled back to the worker. The worker drops on send failure
// (per the structured-logs plan; logging cannot block deploy work).
type LogBatchHandler interface {
	HandleLogBatch(ctx context.Context, workerID string, msg LogBatchMsg) error
}

type noopLogBatchHandler struct{}

func (noopLogBatchHandler) HandleLogBatch(_ context.Context, _ string, _ LogBatchMsg) error {
	return nil
}

// CastBatchHandler persists asciinema cast frames pushed by site
// workers. Fire-and-forget.
type CastBatchHandler interface {
	HandleCastBatch(ctx context.Context, workerID string, msg CastBatchMsg) error
}

type noopCastBatchHandler struct{}

func (noopCastBatchHandler) HandleCastBatch(_ context.Context, _ string, _ CastBatchMsg) error {
	return nil
}

// CastResumeHandler computes the prev_max_us for a machine on
// site-worker pump start. Synchronous (RPC).
type CastResumeHandler interface {
	HandleCastResume(ctx context.Context, workerID string, msg CastResumeRequestMsg) (CastResumeResponseMsg, error)
}

type noopCastResumeHandler struct{}

func (noopCastResumeHandler) HandleCastResume(_ context.Context, _ string, _ CastResumeRequestMsg) (CastResumeResponseMsg, error) {
	return CastResumeResponseMsg{}, nil
}

// OpClaimer atomically claims a pending operation for a worker.
// Returns the claimed op on success, (nil, nil) if the op is no
// longer pending (already claimed, doesn't exist, etc.), or an
// error on a hard failure (DB error). Implementations are
// store-backed; the hub passes the WS-authenticated worker_id —
// never a client-provided value.
type OpClaimer interface {
	ClaimOperation(opID uuid.UUID, workerID string) (*model.Operation, error)
	// GetOperationIfExists reads an op row by ID, returning (nil, nil) when
	// no row matches (never a not-found sentinel — keeps the ws package free
	// of a gorm import). The P4b outlet-op interlock reads the pending op's
	// Params (PDURuntime.Outlet/DownstreamSerial/AllowSelf) and Type BEFORE
	// the claim CAS to decide eligibility. This is a plain read outside any
	// txn — the eligibility gate holds no DB lock (see handleClaimOp). A real
	// store error (nil, err) fails the claim CLOSED: nothing is granted and
	// the worker gets a retryable rpc error. *store.Store satisfies it.
	GetOperationIfExists(opID uuid.UUID) (*model.Operation, error)
}

// noopOpClaimer always returns "already claimed" — used when the
// hub is constructed without a claimer (unit tests that don't
// exercise the claim path).
type noopOpClaimer struct{}

func (noopOpClaimer) ClaimOperation(_ uuid.UUID, _ string) (*model.Operation, error) {
	return nil, nil
}

// GetOperationIfExists on the noop claimer returns (nil, nil): no op found,
// so the interlock treats it as a non-outlet op and lets the claim proceed
// to the (noop) CAS, which returns already-claimed. Tests that exercise the
// interlock wire a real/fake claimer.
func (noopOpClaimer) GetOperationIfExists(_ uuid.UUID) (*model.Operation, error) {
	return nil, nil
}

// Invalidator schedules a debounced snapshot rebuild for a site.
// Post-Tier-2 the hub calls it only when an op transition wrote DURABLE
// machines.state (a completion or a FailOperation via abort/sweep) — the
// identity snapshot must catch up to running/commissioned/failed/new.
// Transient transitions (claim, phase) ride the op stream alone and do
// NOT invalidate. Satisfied by *snapshot.Registry; mirrors api.Invalidator.
type Invalidator interface {
	Invalidate(slug string)
}

// noopInvalidator is the nil-config default (tests that don't
// exercise the claim → snapshot-push path).
type noopInvalidator struct{}

func (noopInvalidator) Invalidate(string) {}

// OpPublisher is the op-event publish surface the hub exposes to the API
// server and the op_update handler (Tier 2 Phase 2). They call it instead
// of invalidating identity for transient op transitions; identity
// invalidation is reserved for durable machines.state writes. The hub
// implements it (PublishOpDelta/PublishOpRemoved).
type OpPublisher interface {
	// PublishOpDelta fans an op's post-mutation row to every connected
	// worker. machineState is the new machine state when the transition
	// wrote one (transient or durable), "" for a phase-only / pending
	// create that changed no state.
	PublishOpDelta(op model.Operation, machineState model.MachineState)
	// PublishOpRemoved fans an op_removed (op left the active set) to
	// every connected worker. machineState is the durable state a
	// terminal transition wrote (running/commissioned/failed/new), ""
	// for pending-expiry (which set no state).
	PublishOpRemoved(op model.Operation, machineState model.MachineState)
}

// ActiveOpsReader is the subset of *store.Store the hub reads for the
// hello resync: the full active-op set (opstate.ActiveStatuses) across
// the fleet. Bounded by concurrent-op count, not machine count.
type ActiveOpsReader interface {
	ListActiveOperations() ([]model.Operation, error)
}

// ErrSerialAmbiguous / ErrSerialUnknown / ErrSerialOutOfScope classify the
// three fail-closed outcomes of joining a reported hardware serial to a
// NetBox device. The hub refuses the hello LOUD on each (mirroring the Proto
// skew-gate); the error text names the reported serial so the reject-path log
// is a grep away.
var (
	// ErrSerialUnknown: the reported serial matches ZERO NetBox devices.
	ErrSerialUnknown = errors.New("reported hardware serial matches no NetBox device")
	// ErrSerialAmbiguous: the reported serial matches MULTIPLE NetBox devices
	// (duplicate/placeholder serials in NetBox) — no authoritative mapping.
	ErrSerialAmbiguous = errors.New("reported hardware serial matches multiple NetBox devices")
	// ErrSerialOutOfScope: the reported serial resolves to exactly one device,
	// but that device is not claimed by this worker's site-config scope.
	ErrSerialOutOfScope = errors.New("reported hardware serial resolves to a device outside this worker's site scope")
)

// WorkerSerialResolver joins a worker-reported hardware serial to the NetBox
// device that this worker's site-config slug is authorized to represent. It
// is the hardware-attested-identity gate: a copy-pasted worker
// config can claim any worker_id, but it cannot forge the metal's serial.
//
// Kept as an injected interface (like Rebuilder / OpClaimer) so the ws package
// does not import internal/netbox or internal/snapshot. The production
// implementation (cmd/slagworksd) looks the serial up via
// netbox.GetDevicesBySerial and confirms scope via the ClaimResolver.
//
// Contract:
//   - exactly one in-scope device      -> (nil): hello accepted.
//   - zero devices                     -> ErrSerialUnknown.
//   - multiple devices                 -> ErrSerialAmbiguous.
//   - one device, wrong site scope     -> ErrSerialOutOfScope.
//
// The hub only calls Resolve for a NON-EMPTY serial; an empty serial is
// refused before the resolver is consulted (serial enforcement — nothing to
// resolve; the emptiness itself is the failure). A non-nil
// error other than the three sentinels above is a transient lookup failure
// (NetBox/DB down); the hub refuses the hello but the operator can retry once
// the dependency recovers — fail-closed, never fail-open.
type WorkerSerialResolver interface {
	Resolve(ctx context.Context, slug, serial string) error
}

// noopSerialResolver accepts every serial. The nil-config default for tests
// and for a global built without a NetBox-backed resolver: serial validation
// is a no-op, so the hello proceeds exactly as it did before serial verification. Production
// wires a real resolver in cmd/slagworksd.
type noopSerialResolver struct{}

func (noopSerialResolver) Resolve(_ context.Context, _, _ string) error { return nil }

// noopActiveOpsReader is the nil-config default: an empty active set, so
// a hello resync sends no ops (tests that don't exercise the op stream).
type noopActiveOpsReader struct{}

func (noopActiveOpsReader) ListActiveOperations() ([]model.Operation, error) { return nil, nil }

// HubConfig carries the collaborators a Hub needs at construction time.
type HubConfig struct {
	Auth      WorkerAuthenticator
	Rebuilder Rebuilder
	// ObsHandler receives worker-origin observation events. Nil becomes
	// a no-op handler that acks everything — useful for tests.
	ObsHandler ObservationHandler
	// OpUpdateHandler receives worker-origin op_update messages. Nil
	// becomes a no-op (messages are logged and dropped).
	OpUpdateHandler OpUpdateHandler
	// LogBatchHandler receives worker-origin log_batch envelopes. Nil
	// becomes a no-op.
	LogBatchHandler LogBatchHandler
	// CastBatchHandler receives worker-origin cast_batch frames
	// (SOL asciinema pipeline). Nil becomes a no-op.
	CastBatchHandler CastBatchHandler
	// CastResumeHandler answers worker-origin cast_resume RPCs.
	// Nil becomes a no-op (returns zero offset).
	CastResumeHandler CastResumeHandler
	// OpClaimer handles worker-origin claim_op RPCs. Nil becomes a
	// claimer that always returns "already claimed" (the safe
	// failing-closed default — workers won't execute ops if no real
	// claimer is wired).
	OpClaimer OpClaimer
	// Invalidator is notified with a site slug after an op transition
	// wrote DURABLE machines.state (completion, abort/sweep fail), so the
	// identity snapshot catches up. Transient transitions do NOT
	// invalidate — they ride the op stream. Nil becomes a no-op (tests).
	Invalidator Invalidator
	// ActiveOps supplies the full active-op set for the hello resync.
	// Nil becomes an empty-set reader (tests): a hello then sends an
	// op_resync with no ops.
	ActiveOps ActiveOpsReader
	// SerialResolver joins a worker-reported hardware serial to its NetBox
	// device and enforces site scope. Nil becomes a no-op resolver
	// that accepts every serial — the behavior before serial verification, used by tests and
	// by a global built without NetBox-backed serial verification. Only
	// consulted for a non-empty reported serial.
	SerialResolver WorkerSerialResolver
	Logger         *slog.Logger

	// RebuildTimeout bounds how long a hello waits on the registry's
	// rebuild actor (RebuildAndWait). It bounds only this caller's
	// wait — the underlying rebuild is shared with other triggers and
	// runs under the registry's own lifecycle. Zero means 30s.
	RebuildTimeout time.Duration

	// ObservationTimeout bounds a single observation handler call.
	// Commission reports hit NetBox which can be slow under load; 60s
	// gives network latency + queued writes room. Zero means 60s.
	ObservationTimeout time.Duration

	// OpUpdateTimeout bounds a single op_update handler call. Just a
	// Postgres round-trip; 10s is generous. Zero means 10s.
	OpUpdateTimeout time.Duration

	// LogBatchTimeout bounds a single log_batch handler call. Bulk
	// insert against pg log_envelopes; 10s is ample. Zero means 10s.
	LogBatchTimeout time.Duration

	// Version is the global daemon's own build SHA. The hub compares
	// it against each connecting worker's Hello.Version and logs a
	// WARN on mismatch — phase 1 of the version-pin rollout. Empty
	// disables comparison (e.g. local dev runs with version="dev").
	Version string

	// PackageVersion is the deb package version scripts/build-deb.sh
	// built the global daemon as ("" for any other build). Sent with
	// Version on snapshot and no_change; a site worker's auto-upgrade
	// installs exactly this package build.
	PackageVersion string

	// Metrics, when non-nil, records worker hellos and the per-worker
	// version-info gauge (the HA both-workers-upgraded drift signal).
	Metrics *opsmetrics.Metrics
}

// Hub is the server-side endpoint for the worker-facing WebSocket
// protocol. It:
//   - authenticates each connection at upgrade time via ?token=
//   - tracks connected workers per site slug (many-per-slug for HA)
//   - accepts Push(slug, snap) from the snapshot dispatcher and fans
//     out to all connected workers for that slug
//   - on hello, replies with either no_change (when the worker's
//     last_seq_num matches the latest cached snapshot) or a fresh
//     snapshot (triggering a rebuild if no cached snapshot is available)
//
// Hub implements snapshot.PushSink.
type Hub struct {
	cfg HubConfig
	log *slog.Logger

	// serialEnforced is true iff a real SerialResolver was wired at
	// construction (production): hello serial validation is mandatory,
	// including refusing an empty serial. False only under the nil-resolver
	// dev/test default. Set once in NewHub, read-only after.
	serialEnforced bool

	mu          sync.RWMutex
	connections map[string]map[*workerConn]struct{} // slug -> set
	snapshots   map[string]model.SiteSnapshot       // slug -> most recent pushed snap

	// rpcPending tracks in-flight global→worker RPCs (power_status)
	// keyed by request_id. The hub originates the request_id; the
	// worker echoes it on its reply envelope; dispatch routes the
	// reply into the waiting channel. Mirror image of the worker's
	// pending map in wsconn.go.
	rpcMu      sync.Mutex
	rpcPending map[string]chan RPCResponseMsg
}

// NewHub constructs a Hub from config. Panics on a nil Auth or
// Rebuilder — those are structural, not runtime recoverable.
func NewHub(cfg HubConfig) *Hub {
	if cfg.Auth == nil {
		panic("ws.NewHub: Auth is required")
	}
	if cfg.Rebuilder == nil {
		panic("ws.NewHub: Rebuilder is required")
	}
	if cfg.RebuildTimeout == 0 {
		cfg.RebuildTimeout = 30 * time.Second
	}
	if cfg.ObservationTimeout == 0 {
		cfg.ObservationTimeout = 60 * time.Second
	}
	if cfg.OpUpdateTimeout == 0 {
		cfg.OpUpdateTimeout = 10 * time.Second
	}
	if cfg.LogBatchTimeout == 0 {
		cfg.LogBatchTimeout = 10 * time.Second
	}
	if cfg.ObsHandler == nil {
		cfg.ObsHandler = noopObservationHandler{}
	}
	if cfg.OpUpdateHandler == nil {
		cfg.OpUpdateHandler = noopOpUpdateHandler{}
	}
	if cfg.LogBatchHandler == nil {
		cfg.LogBatchHandler = noopLogBatchHandler{}
	}
	if cfg.CastBatchHandler == nil {
		cfg.CastBatchHandler = noopCastBatchHandler{}
	}
	if cfg.CastResumeHandler == nil {
		cfg.CastResumeHandler = noopCastResumeHandler{}
	}
	if cfg.OpClaimer == nil {
		cfg.OpClaimer = noopOpClaimer{}
	}
	if cfg.Invalidator == nil {
		cfg.Invalidator = noopInvalidator{}
	}
	if cfg.ActiveOps == nil {
		cfg.ActiveOps = noopActiveOpsReader{}
	}
	// serialEnforced: hardware-serial identity is enforced iff a real
	// resolver was wired (production). A nil resolver (dev/test, same shape
	// as a nil bmcSecrets) skips serial validation entirely — the ONLY
	// unverified mode; there is no tolerated-empty-serial state under a real
	// resolver (no transitional rollout hedge).
	serialEnforced := cfg.SerialResolver != nil
	if cfg.SerialResolver == nil {
		cfg.SerialResolver = noopSerialResolver{}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		cfg:            cfg,
		log:            log,
		serialEnforced: serialEnforced,
		connections:    make(map[string]map[*workerConn]struct{}),
		snapshots:      make(map[string]model.SiteSnapshot),
		rpcPending:     make(map[string]chan RPCResponseMsg),
	}
}

// Push implements snapshot.PushSink. Called by the dispatcher after a
// content-hash change: cache the snapshot and fan out to every
// connected worker for the slug. The snapshot is queued on each worker's
// outbound writer (workerConn.enqueue), so one slow worker cannot stall
// delivery to the rest and each worker receives snapshots in push order.
// Each fan-out snapshot is followed by the site's active ops, re-sent as
// op_deltas scoped to the new membership (see workerConn.send). The cache
// is updated before the fan-out starts, which the opReady handoff in send
// relies on.
func (h *Hub) Push(slug string, snap model.SiteSnapshot) {
	h.mu.Lock()
	h.snapshots[slug] = snap
	conns := make([]*workerConn, 0, len(h.connections[slug]))
	for c := range h.connections[slug] {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, c := range conns {
		c.enqueue("snapshot", func() { c.send(snap) })
	}
}

// connsSnapshot returns every currently-connected worker across all
// slugs. op_removed fans to all of them; op_delta is additionally
// filtered by slug membership (see PublishOpDelta — Operation.Params
// carries per-site credentials).
func (h *Hub) connsSnapshot() []*workerConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var conns []*workerConn
	for _, set := range h.connections {
		for c := range set {
			conns = append(conns, c)
		}
	}
	return conns
}

// machineInSlug reports whether machineID belongs to slug's site, per the
// hub's cached per-slug snapshot (the same NetBox-derived membership the
// identity fan-out uses). No cached snapshot for the slug → false: the
// hub can't prove membership, so it must not send the op (fail closed —
// this is a confidentiality gate, see PublishOpDelta).
func (h *Hub) machineInSlug(slug string, machineID uuid.UUID) bool {
	snap, ok := h.snapshotFor(slug)
	if !ok {
		return false
	}
	for i := range snap.Machines {
		if snap.Machines[i].ID == machineID {
			return true
		}
	}
	return false
}

// PublishOpDelta implements OpPublisher: fan an op's post-mutation row as
// op_delta to the workers whose site owns the machine — NOT fleet-wide.
// Operation.Params carries per-site secrets (BMCRuntime/PDURuntime
// passwords, single-use tailnet auth keys); pre-cutover ops rode the
// per-slug SiteSnapshot, so a worker only ever saw its own site's op rows,
// and this membership filter preserves that confidentiality baseline.
//
// A conn whose slug has no cached snapshot (or whose snapshot doesn't
// contain the machine) is skipped. The skip can drop an op the conn's
// site owns: the mirror commits a machine row and only schedules the
// identity rebuild (debounced, then NetBox round trips, retried on
// failure), so an op can be created on the machine before the snapshot
// holding it is pushed. The op then reaches the worker in the op re-send
// that follows that push (workerConn.send), or in the hello resync of a
// conn that connects after it.
//
// On a write failure the worker's connection is force-closed (B3
// drain-loop pattern) so its reconnect + op_resync self-heals a missed
// pending-op delta rather than silently stranding the op.
func (h *Hub) PublishOpDelta(op model.Operation, machineState model.MachineState) {
	msg := OpDeltaMsg{Operation: op, MachineState: machineState}
	for _, c := range h.connsSnapshot() {
		if !h.machineInSlug(c.slug, op.MachineID) {
			continue
		}
		c.enqueue("op_delta", func() { h.sendOpMessage(c, TypeOpDelta, msg, "op_delta", op.ID.String()) })
	}
}

// PublishOpRemoved implements OpPublisher: fan an op_removed (op left the
// active set) to every connected worker, deliberately UNFILTERED.
// OpRemovedMsg carries no Operation row and no params — just op ID,
// machine ID, version, and a state string — so there is nothing
// confidential to scope. And it MUST stay fleet-wide to close the
// machine-deleted-from-NetBox edge: when the mirror's B7 drift path fails
// ops for a machine that just left NetBox, a membership check would drop
// the removal (the machine is no longer in ANY cached snapshot) and
// strand a zombie active op on the very worker that was driving it. Same
// force-close-on-write-failure contract as PublishOpDelta.
func (h *Hub) PublishOpRemoved(op model.Operation, machineState model.MachineState) {
	msg := OpRemovedMsg{
		OperationID:  op.ID.String(),
		MachineID:    op.MachineID.String(),
		Version:      op.Version,
		MachineState: machineState,
	}
	for _, c := range h.connsSnapshot() {
		c.enqueue("op_removed", func() { h.sendOpMessage(c, TypeOpRemoved, msg, "op_removed", op.ID.String()) })
	}
}

// sendOpMessage writes one op-stream message to a worker under a bounded
// timeout. PublishOpDelta and PublishOpRemoved run it on the conn's
// outbound writer (workerConn.enqueue), so a worker receives op messages
// in publish order. On failure it force-closes the connection: the
// reconnect replays the full active set via op_resync, so a lost op
// message self-heals within one reconnect (plan "Delivery reliability").
//
// The write happens under opStreamMu with an opReady gate: a conn that
// hasn't sent its hello op_resync yet gets NO op messages (the publish is
// dropped — safe, because a publisher that saw opReady=false committed its
// DB mutation before the resync's DB read, so the mutation rides the
// resync). See the invariant note on workerConn.
func (h *Hub) sendOpMessage(c *workerConn, msgType string, payload any, kind, opID string) {
	c.opStreamMu.Lock()
	defer c.opStreamMu.Unlock()
	if !c.opReady {
		return // pre-resync: the mutation is covered by the pending resync
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.writeTyped(ctx, msgType, "", payload); err != nil {
		c.log.Warn("op-stream write failed; forcing reconnect", "kind", kind, "op_id", opID, "err", err)
		_ = c.conn.CloseNow()
	}
}

// ServeHTTP upgrades the incoming request to a WebSocket connection
// for a site worker. The request must include ?token=<plaintext>;
// authentication resolves (site_slug, worker_id) from
// site_workers.token_hash.
//
// On success the call blocks until the worker disconnects or ctx is
// cancelled, so this is intended to be wired under a long-lived
// handler (e.g. echo.WrapHandler) rather than a request-scoped one.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	worker, err := h.cfg.Auth.GetSiteWorkerByTokenHash(store.HashWorkerToken(token))
	if err != nil {
		// Treat any lookup error as unauthenticated; don't leak whether
		// the token was malformed, missing, or a DB error.
		if !errors.Is(err, store.ErrSiteWorkerNotFound) {
			h.log.Error("worker auth lookup failed", "err", err)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := Accept(w, r, &websocket.AcceptOptions{
		// Workers run on an internal trusted network; CompressionMode
		// left at default (no compression) to keep CPU predictable on
		// the global — the snapshots are already sent as JSON with a
		// known upper bound (~300KB).
	})
	if err != nil {
		// Accept already wrote an error to w.
		h.log.Warn("ws accept failed", "slug", worker.SiteSlug, "worker", worker.WorkerID, "err", err)
		return
	}

	ctx := r.Context()
	wc := &workerConn{
		hub:      h,
		conn:     conn,
		slug:     worker.SiteSlug,
		workerID: worker.WorkerID,
		log:      h.log.With("slug", worker.SiteSlug, "worker", worker.WorkerID),
		outbound: make(chan func(), outboundQueueSize),
	}
	// Registration happens inside serve, immediately before the hello
	// reply is sent. See workerConn.serve for the rationale — the
	// alternative ordering (register-after-hello) races Push fan-out.

	wc.log.Info("worker connected")
	defer wc.log.Info("worker disconnected")

	if err := wc.serve(ctx); err != nil && !errors.Is(err, context.Canceled) {
		wc.log.Warn("worker session ended with error", "err", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "server shutdown")
}

func (h *Hub) register(c *workerConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections[c.slug] == nil {
		h.connections[c.slug] = make(map[*workerConn]struct{})
	}
	h.connections[c.slug][c] = struct{}{}
}

func (h *Hub) unregister(c *workerConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.connections[c.slug]; ok {
		delete(set, c)
		if len(set) == 0 {
			delete(h.connections, c.slug)
		}
	}
}

// Close force-terminates every connected worker's WS connection. Used
// during server shutdown: http.Server.Shutdown waits for active
// handlers to return, but a WS handler blocked in ReadMessage won't
// return until the socket errors. Closing the conn makes the read
// error out and ServeHTTP unwind cleanly.
//
// Safe to call from a goroutine concurrent with ServeHTTP; CloseNow
// is idempotent and per-worker goroutines already tolerate a closed
// conn.
func (h *Hub) Close() {
	h.mu.Lock()
	conns := make([]*workerConn, 0)
	for _, set := range h.connections {
		for c := range set {
			conns = append(conns, c)
		}
	}
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.conn.CloseNow()
	}
}

// snapshotFor returns the hub's cached snapshot for the slug, if any.
// Called from hello-handling; a miss triggers a dispatcher rebuild.
func (h *Hub) snapshotFor(slug string) (model.SiteSnapshot, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	snap, ok := h.snapshots[slug]
	return snap, ok
}

// ErrNoWorkerForSite is returned by PowerStatus when the target site
// has no connected worker. Callers (the API handler) translate it to
// a 503 with an operator-actionable message.
var ErrNoWorkerForSite = errors.New("no connected site worker for site")

// ConnectedWorkerSerials returns the verified hardware serials of the workers
// CURRENTLY CONNECTED for a slug (one entry per connection; "" for a connected
// worker that reported no verified serial). The P4b op-creation-time
// eligible-partner check (api PDU-outlet POST) uses this to refuse creating a
// non-AllowSelf self-feed cycle/on op unless a claim-eligible non-self worker
// is connected right then — turning a would-be claim timeout into an immediate
// error. This is advisory routing, NOT the safety gate (the claim-time
// interlock is). Each conn's verifiedSerial is set once before it registers
// and never mutated, so reading it under the hub RLock is race-free.
func (h *Hub) ConnectedWorkerSerials(slug string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.connections[slug]))
	for c := range h.connections[slug] {
		out = append(out, c.verifiedSerial)
	}
	return out
}

// WorkerSMCIPMITool returns the SMCIPMITool status a currently-connected
// worker reported in its hello ("installed", "missing", "missing-required",
// "disabled", or "" when it has no status func wired), and whether the
// worker is connected at all.
func (h *Hub) WorkerSMCIPMITool(siteSlug, workerID string) (status string, connected bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.connections[siteSlug] {
		if c.workerID == workerID {
			return c.smcipmitool, true
		}
	}
	return "", false
}

// SupermicroMachineCount returns how many machines in the hub's cached
// snapshot for slug are Supermicro (0 when no snapshot is cached yet).
func (h *Hub) SupermicroMachineCount(slug string) int {
	snap, ok := h.snapshotFor(slug)
	if !ok {
		return 0
	}
	n := 0
	for i := range snap.Machines {
		if snap.Machines[i].IsSupermicro() {
			n++
		}
	}
	return n
}

// IsWorkerConnected reports whether a worker with the given (siteSlug,
// workerID) currently has a live WS connection. Used by the admin
// site-worker delete path: deleting the row out from under a live
// connection would strand that connection (the row is what authenticated
// it) — refusing is the safety rail.
func (h *Hub) IsWorkerConnected(siteSlug, workerID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.connections[siteSlug] {
		if c.workerID == workerID {
			return true
		}
	}
	return false
}

// PowerStatus RPCs a connected worker for the given site slug to read
// a machine's BMC power state inline. Reverse direction of claim_op:
// the hub mints the request_id, writes a power_status envelope to one
// connected worker (any — both HA workers can reach the OOB net), and
// blocks until the worker's power_status_response arrives or ctx
// expires. bmcRuntime / amtRuntime are the marshalled model.BMCRuntime /
// model.AMTRuntime blocks resolved by the caller (exactly one is non-nil,
// selected by the machine's credential kind); they carry the creds, so
// they must never be logged.
func (h *Hub) PowerStatus(ctx context.Context, siteSlug string, machineID uuid.UUID, bmcRuntime, amtRuntime json.RawMessage) (PowerStatusResponseMsg, error) {
	h.mu.RLock()
	var target *workerConn
	for c := range h.connections[siteSlug] {
		target = c
		break
	}
	h.mu.RUnlock()
	if target == nil {
		return PowerStatusResponseMsg{}, fmt.Errorf("%w: %s", ErrNoWorkerForSite, siteSlug)
	}

	reqID := uuid.NewString()
	ch := make(chan RPCResponseMsg, 1)
	h.rpcMu.Lock()
	h.rpcPending[reqID] = ch
	h.rpcMu.Unlock()
	defer func() {
		h.rpcMu.Lock()
		delete(h.rpcPending, reqID)
		h.rpcMu.Unlock()
	}()

	if err := target.writeTyped(ctx, TypePowerStatus, reqID, PowerStatusRequestMsg{
		MachineID:  machineID,
		BMCRuntime: bmcRuntime,
		AMTRuntime: amtRuntime,
	}); err != nil {
		return PowerStatusResponseMsg{}, fmt.Errorf("power_status write to worker %s: %w", target.workerID, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return PowerStatusResponseMsg{}, fmt.Errorf("site worker %s: %s", target.workerID, resp.Error)
		}
		var out PowerStatusResponseMsg
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return PowerStatusResponseMsg{}, fmt.Errorf("decode power_status response: %w", err)
		}
		return out, nil
	case <-ctx.Done():
		return PowerStatusResponseMsg{}, fmt.Errorf("power_status RPC to worker %s: %w", target.workerID, ctx.Err())
	}
}

// BootStatus RPCs a connected worker for the given site to read a machine's
// BMC boot-device override inline. Identical machinery to PowerStatus (mint
// request_id, write to any connected worker, block on the reply); only the
// envelope type and payload differ. bmcRuntime carries creds and must never
// be logged.
func (h *Hub) BootStatus(ctx context.Context, siteSlug string, machineID uuid.UUID, bmcRuntime json.RawMessage) (BootStatusResponseMsg, error) {
	h.mu.RLock()
	var target *workerConn
	for c := range h.connections[siteSlug] {
		target = c
		break
	}
	h.mu.RUnlock()
	if target == nil {
		return BootStatusResponseMsg{}, fmt.Errorf("%w: %s", ErrNoWorkerForSite, siteSlug)
	}

	reqID := uuid.NewString()
	ch := make(chan RPCResponseMsg, 1)
	h.rpcMu.Lock()
	h.rpcPending[reqID] = ch
	h.rpcMu.Unlock()
	defer func() {
		h.rpcMu.Lock()
		delete(h.rpcPending, reqID)
		h.rpcMu.Unlock()
	}()

	if err := target.writeTyped(ctx, TypeBootStatus, reqID, BootStatusRequestMsg{
		MachineID:  machineID,
		BMCRuntime: bmcRuntime,
	}); err != nil {
		return BootStatusResponseMsg{}, fmt.Errorf("boot_status write to worker %s: %w", target.workerID, err)
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			return BootStatusResponseMsg{}, fmt.Errorf("site worker %s: %s", target.workerID, resp.Error)
		}
		var out BootStatusResponseMsg
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			return BootStatusResponseMsg{}, fmt.Errorf("decode boot_status response: %w", err)
		}
		return out, nil
	case <-ctx.Done():
		return BootStatusResponseMsg{}, fmt.Errorf("boot_status RPC to worker %s: %w", target.workerID, ctx.Err())
	}
}

// WorkerDHCPLeases is one connected worker's answer to a dhcp_leases
// fan-out RPC: its lease snapshot, or Err when the RPC to that worker
// failed (timeout, write error, or the worker replied with an error).
// A per-worker failure never fails the others — see DHCPLeases.
type WorkerDHCPLeases struct {
	WorkerID string
	FQDN     string
	Err      string
	Leases   []DHCPLeaseInfo
}

// DHCPLeases RPCs EVERY connected worker for the given site slug to
// snapshot its in-memory DHCP lease table, concurrently, and collects
// each worker's answer. Unlike PowerStatus/BootStatus (pick any one
// connected worker — both can reach the same BMC), an HA cage's two
// workers each own a disjoint dynamic slice, so only fanning out to
// all of them answers "what leases exist right now" completely.
//
// A per-worker timeout or error populates that worker's Err field and
// does not affect the others (each conn gets its own goroutine, its
// own rpcPending entry, and its own bounded wait on ctx). No connected
// workers for the slug returns nil, nil — an empty HA cage is an
// answerable question ("no workers connected"), not a hub-level error;
// the API layer turns nil into a 200 with an empty workers array.
func (h *Hub) DHCPLeases(ctx context.Context, siteSlug string) []WorkerDHCPLeases {
	h.mu.RLock()
	conns := make([]*workerConn, 0, len(h.connections[siteSlug]))
	for c := range h.connections[siteSlug] {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	if len(conns) == 0 {
		return nil
	}

	results := make([]WorkerDHCPLeases, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c *workerConn) {
			defer wg.Done()
			results[i] = h.dhcpLeasesOne(ctx, c)
		}(i, c)
	}
	wg.Wait()
	return results
}

// dhcpLeasesOne RPCs a single worker for its lease snapshot. Same
// mint-request_id / register-channel / write / block-on-reply-or-ctx
// shape as PowerStatus, scoped to one conn so DHCPLeases can run it
// concurrently per worker.
func (h *Hub) dhcpLeasesOne(ctx context.Context, target *workerConn) WorkerDHCPLeases {
	out := WorkerDHCPLeases{WorkerID: target.workerID, FQDN: target.fqdn}

	reqID := uuid.NewString()
	ch := make(chan RPCResponseMsg, 1)
	h.rpcMu.Lock()
	h.rpcPending[reqID] = ch
	h.rpcMu.Unlock()
	defer func() {
		h.rpcMu.Lock()
		delete(h.rpcPending, reqID)
		h.rpcMu.Unlock()
	}()

	if err := target.writeTyped(ctx, TypeDHCPLeases, reqID, DHCPLeasesRequestMsg{}); err != nil {
		out.Err = fmt.Sprintf("dhcp_leases write to worker %s: %v", target.workerID, err)
		return out
	}

	select {
	case resp := <-ch:
		if resp.Error != "" {
			out.Err = resp.Error
			return out
		}
		var body DHCPLeasesResponseMsg
		if err := json.Unmarshal(resp.Data, &body); err != nil {
			out.Err = fmt.Sprintf("decode dhcp_leases response: %v", err)
			return out
		}
		out.Leases = body.Leases
		return out
	case <-ctx.Done():
		out.Err = fmt.Sprintf("dhcp_leases RPC to worker %s: %v", target.workerID, ctx.Err())
		return out
	}
}

// deliverRPCReply routes a worker's power_status_response into the
// channel PowerStatus is blocked on. Unknown request_ids (RPC timed
// out and was deregistered, or a worker bug) are dropped with a log.
func (h *Hub) deliverRPCReply(reqID string, resp RPCResponseMsg) {
	// Delete on first delivery so a duplicate reply for the same
	// request_id (buggy or malicious worker) hits the unknown-reqID
	// path instead of a full channel. The non-blocking send is belt
	// and suspenders for the same reason: this runs on the worker's
	// read loop, which must never block.
	h.rpcMu.Lock()
	ch, ok := h.rpcPending[reqID]
	if ok {
		delete(h.rpcPending, reqID)
	}
	h.rpcMu.Unlock()
	if !ok {
		h.log.Warn("rpc reply for unknown request_id (caller timed out or duplicate reply?)", "req_id", reqID)
		return
	}
	select {
	case ch <- resp:
	default:
		h.log.Warn("rpc reply dropped: channel full", "req_id", reqID)
	}
}

// outboundQueueSize bounds a worker connection's outbound queue
// (workerConn.enqueue). The queue only grows while the conn's writer is
// stuck on one write, and every write times out after 10s. The largest
// burst one caller publishes is a message per active op (an expiry sweep,
// the mirror's drift fail), and the active set is dozens of ops, so a
// queue this size fills only when the worker has stopped reading.
const outboundQueueSize = 4096

// workerConn is a single authenticated worker connection. The goroutine
// in serve() owns reads; per-connection writes are serialized by
// writeMu (coder/websocket requires one writer at a time). Snapshot
// fan-outs and op-stream messages are not written by their publishers:
// they go through outbound, whose one writer goroutine delivers them in
// the order they were queued. The hello reply, the hello op_resync and RPC
// traffic are written directly by the goroutine that produces them.
type workerConn struct {
	hub      *Hub
	conn     *Conn
	slug     string
	workerID string
	log      *slog.Logger

	// verifiedSerial is the worker's hardware serial as reported in its
	// hello and verified against NetBox. "" means the worker
	// reported no serial (unverified). Set once in serve() before the read
	// loop starts and read-only thereafter, so no lock is needed. The P4b
	// outlet-op interlock reads this to decide claim eligibility. NetBox
	// matched it case-insensitively, so it can differ in case from the
	// device's NetBox serial: compare with model.SameHardwareSerial.
	verifiedSerial string

	// fqdn is the worker's advertised hello.FQDN, set once before the
	// read loop starts (same single-write lifecycle as verifiedSerial)
	// so DHCPLeases can attribute a fan-out result to a human-readable
	// worker name without a store round-trip.
	fqdn string

	// smcipmitool is the worker's hello.SMCIPMITool (whether the
	// operator-installed Supermicro tool is usable there). Same
	// single-write lifecycle as fqdn; read by WorkerSMCIPMITool for
	// `slagctl worker list`.
	smcipmitool string

	writeMu sync.Mutex

	// outbound is the conn's FIFO of snapshot fan-outs and op-stream
	// messages, run one at a time by writeOutbound, which serve starts
	// before the conn registers. Push and PublishOp* add to it through
	// enqueue instead of writing, so the worker receives their messages in
	// the order they were published.
	outbound chan func()

	// opStreamMu + opReady close the register-before-resync race: the
	// conn registers BEFORE the hello reply (so it can't miss snapshot
	// fan-outs), which means a PublishOp* can otherwise write an op
	// message to this conn BEFORE the hello op_resync — and the resync's
	// full-replace on the worker would then clobber the newer delta.
	//
	// sendOpMessage checks opReady under opStreamMu and DROPS the publish
	// while false; handleHello sets opReady=true and performs the resync's
	// DB read + write while still holding opStreamMu. Invariant: any
	// publisher that observed opReady=false committed its DB mutation
	// before the resync's DB read (mutate-then-publish ordering), so the
	// mutation is IN the resync — the drop loses nothing; any publisher
	// that observes opReady=true writes strictly after the resync.
	// Op messages are ordered among themselves by outbound, which runs
	// them one at a time; opStreamMu orders them against the resync, which
	// handleHello writes from the read goroutine.
	//
	// The post-push op re-send (send) follows the same rule: a Push whose
	// fan-out observed opReady=false cached its snapshot before the hello
	// resync reads the slug's membership under opStreamMu, so the hello
	// resync is already scoped by that snapshot.
	opStreamMu sync.Mutex
	opReady    bool
}

func (c *workerConn) serve(ctx context.Context) error {
	// The first message from a newly connected worker MUST be hello;
	// anything else is a protocol violation. Keeping the read
	// single-threaded here is intentional — coder/websocket requires
	// serialized reads.
	env, err := c.conn.ReadMessage(ctx)
	if err != nil {
		return fmt.Errorf("read first message: %w", err)
	}
	if env.Type != TypeHello {
		c.writeError(ctx, "protocol_error", "first message must be hello")
		return fmt.Errorf("first message was %q, expected hello", env.Type)
	}
	var hello HelloMsg
	if err := unmarshalData(env.Data, &hello); err != nil {
		c.writeError(ctx, "bad_request", "malformed hello payload")
		return fmt.Errorf("decode hello: %w", err)
	}
	if hello.WorkerID != c.workerID {
		// Token says one worker_id, hello claims another. Reject —
		// tokens are per-worker and must match; otherwise lease
		// tracking gets confused.
		c.writeError(ctx, "unauthorized", "worker_id mismatch")
		return fmt.Errorf("hello worker_id %q does not match auth %q", hello.WorkerID, c.workerID)
	}

	// Op-stream skew gate (Tier 2 Phase 2): operations left SiteSnapshot,
	// so a pre-op-stream worker (Proto absent/lower) would get identity
	// with no ops and never actuate. Both skew directions are otherwise
	// silent (the op_id-rename precedent), so refuse LOUD: connection-level
	// error naming the fix. The worker enforces the mirror image on the
	// reply. (The reply also carries Proto; a snapshot still carrying
	// Operations is impossible now that the field is gone, so the Proto
	// checks are the primary gate.)
	if hello.Proto < OpStreamProto {
		c.log.Error("worker op-stream protocol too old; refusing connection",
			"worker_proto", hello.Proto, "required_proto", OpStreamProto)
		c.writeErrorProto(ctx, "proto_mismatch",
			fmt.Sprintf("worker op-stream protocol %d < required %d; upgrade the slagworks package on this worker", hello.Proto, OpStreamProto))
		return fmt.Errorf("worker op-stream proto %d < %d", hello.Proto, OpStreamProto)
	}

	// Hardware-attested identity gate. A non-empty reported serial
	// must join to exactly one NetBox device in THIS worker's site scope; any
	// mismatch (zero/multiple/out-of-scope), a lookup failure, AND an EMPTY
	// serial all refuse the hello LOUD, mirroring the Proto skew-gate. There
	// is no tolerated-but-unverified state: when serial enforcement is
	// configured (a real SerialResolver wired — production), a worker that
	// reports no serial (old binary, dmidecode failure, placeholder SMBIOS)
	// is refused until the metal, NetBox, and the worker binary agree. The
	// only carve-out is a nil resolver (dev/test, same shape as a nil
	// bmcSecrets), which skips serial validation entirely.
	if c.hub.serialEnforced {
		if hello.Serial == "" {
			c.log.Error("worker reported no hardware serial; refusing connection (serial enforcement is on)",
				"worker_id", c.workerID, "slug", c.slug)
			c.writeError(ctx, "serial_missing",
				"worker reported no hardware serial; upgrade the slagworks package on this worker (the site worker reads its SMBIOS serial at startup) and ensure the board has a real serial")
			return fmt.Errorf("worker %s reported no hardware serial (slug=%s)", c.workerID, c.slug)
		}
		if err := c.hub.cfg.SerialResolver.Resolve(ctx, c.slug, hello.Serial); err != nil {
			c.log.Error("worker hardware serial failed NetBox join; refusing connection",
				"reported_serial", hello.Serial, "slug", c.slug, "err", err)
			c.writeError(ctx, "serial_mismatch",
				fmt.Sprintf("reported hardware serial %q does not resolve to a device in this worker's site scope: %v; reconcile NetBox with the metal before connecting", hello.Serial, err))
			return fmt.Errorf("worker serial join failed (serial=%q slug=%q): %w", hello.Serial, c.slug, err)
		}
		c.log.Info("worker hardware serial verified", "serial", hello.Serial)
	}
	c.verifiedSerial = hello.Serial
	c.fqdn = hello.FQDN
	c.smcipmitool = hello.SMCIPMITool

	if _, err := c.hub.cfg.Auth.TouchSiteWorker(c.slug, c.workerID, hello.FQDN, &hello.Serial); err != nil {
		c.log.Warn("touch site_worker failed", "err", err)
		// Non-fatal; the worker is already authenticated and we don't
		// want to drop the connection over a last_seen_at bookkeeping
		// hiccup.
	}

	// Phase 1 of version-pin: log every connecting worker's build SHA
	// and warn on mismatch. No enforcement yet — auto-heal lands later
	// behind a flag, then enforcement after that. Empty Version on
	// either side disables comparison so dev builds (version="dev")
	// don't spam warnings against an SHA-stamped global.
	if c.hub.cfg.Version != "" && hello.Version != "" && hello.Version != c.hub.cfg.Version {
		c.log.Warn("worker version mismatch",
			"worker_version", hello.Version,
			"global_version", c.hub.cfg.Version)
	} else {
		c.log.Info("worker hello", "worker_version", hello.Version)
	}
	if m := c.hub.cfg.Metrics; m != nil {
		m.WorkerHellos.WithLabelValues(c.slug).Inc()
		v := hello.Version
		if v == "" {
			v = "unknown"
		}
		m.WorkerVersion.WithLabelValues(c.slug, c.workerID, v).Set(1)
		// Drop the series on disconnect so a worker that upgrades and
		// reconnects doesn't leave its old SHA lingering as a phantom
		// second version.
		defer m.WorkerVersion.DeleteLabelValues(c.slug, c.workerID, v)
	}

	// Register BEFORE the hello reply. The alternative (register after)
	// races Push fan-out: if the dispatcher pushed a new snapshot
	// between snapshotFor() and register(), this worker misses it until
	// the NEXT push. We accept the other side of the tradeoff — a
	// possible stale hello-reply reaching the worker after a newer
	// fan-out snapshot — and rely on Cache.ApplySnapshot's SeqNum
	// monotonicity check to drop it.
	//
	// The outbound writer starts first, because a Push or PublishOp* can
	// queue to the conn as soon as it is registered, and stops only after
	// the conn has unregistered (deferred calls run in reverse order).
	stopWriter := make(chan struct{})
	go c.writeOutbound(stopWriter)
	defer close(stopWriter)
	c.hub.register(c)
	defer c.hub.unregister(c)

	if err := c.handleHello(ctx, hello); err != nil {
		return err
	}

	// Steady-state read loop. For Plan 2 we accept hello only once
	// per connection; subsequent messages are heartbeat / observation /
	// op_update / log_lines / claim_op. A second hello is a protocol
	// violation.
	for {
		env, err := c.conn.ReadMessage(ctx)
		if err != nil {
			return err
		}
		if err := c.dispatch(ctx, env); err != nil {
			return err
		}
	}
}

// handleHello decides snapshot vs no_change. If the hub has a cached
// snapshot whose SeqNum equals the worker's last_seq_num, reply
// no_change. Otherwise send the cached snapshot, or trigger a rebuild
// if there isn't one yet.
func (c *workerConn) handleHello(ctx context.Context, hello HelloMsg) error {
	snap, ok := c.hub.snapshotFor(c.slug)
	if !ok {
		// Hub is cold for this slug (just started, or no push has
		// happened yet). Block on the slug's rebuild actor; concurrent
		// hellos for the same slug batch onto one rebuild there.
		rctx, cancel := context.WithTimeout(ctx, c.hub.cfg.RebuildTimeout)
		defer cancel()
		built, err := c.hub.cfg.Rebuilder.RebuildAndWait(rctx, c.slug)
		if err != nil {
			c.writeError(ctx, "rebuild_failed", err.Error())
			return fmt.Errorf("hello-triggered rebuild: %w", err)
		}
		snap = built
		// Cache the rebuilt snapshot. The dispatcher only Pushes (and so
		// only populates h.snapshots) on a content-hash CHANGE; after a
		// global restart with unchanged content the rebuild returns
		// skipped_no_change and never Pushes, leaving this slug absent
		// from the cache. machineInSlug reads that cache as the op-stream
		// confidentiality gate, so an uncached slug would silently drop
		// every op_delta for this worker until an unrelated Push. Guard
		// against clobbering a newer concurrently-Pushed snapshot.
		c.hub.mu.Lock()
		if cur, ok := c.hub.snapshots[c.slug]; !ok || cur.SeqNum < built.SeqNum {
			c.hub.snapshots[c.slug] = built
		}
		c.hub.mu.Unlock()
	}

	if snap.SeqNum == hello.LastSeqNum && snap.SeqNum != 0 {
		if err := c.writeTyped(ctx, TypeNoChange, "", NoChangeMsg{
			Version: c.hub.cfg.Version, PackageVersion: c.hub.cfg.PackageVersion, Proto: OpStreamProto}); err != nil {
			return err
		}
	} else {
		if err := c.writeTyped(ctx, TypeSnapshot, "", c.hub.snapshotMsg(snap)); err != nil {
			return err
		}
	}

	// Unconditional op_resync after the snapshot/no_change decision: the
	// worker full-replaces its op set + rebuilds machine-state overlays
	// from it and clears its untrusted flag. Bounded by active-op count,
	// so a resync is cheap even when no ops changed (plan: no op-watermark).
	return c.sendOpResync(ctx)
}

// sendOpResync builds and writes the hello op_resync: the site's active
// ops (siteActiveOpsLocked), plus the transient machine-state overlay each
// claimed/running lifecycle op implies (claimedState). Global has no
// overlay store — the map is derived here, from the site-scoped set only.
//
// The whole body — flipping opReady AND the ListActiveOperations DB read
// AND the resync write — runs under opStreamMu (see workerConn). This
// closes the register-before-resync race: a concurrent PublishOp* either
// observed opReady=false (its DB mutation precedes our read here, so the
// mutation is in the resync and dropping the message loses nothing) or
// blocks on opStreamMu until the resync is on the wire and then delivers
// its delta strictly after it. The DB read under a per-conn mutex is fine:
// it blocks only op messages destined for THIS conn, it's a bounded
// active-set query (dozens of rows), and hellos are rare.
func (c *workerConn) sendOpResync(ctx context.Context) error {
	c.opStreamMu.Lock()
	defer c.opStreamMu.Unlock()
	c.opReady = true

	ops, err := c.siteActiveOpsLocked()
	if err != nil {
		// A resync failure is connection-fatal: without the op set the
		// worker stays untrusted and can't actuate. Surface it and drop.
		c.writeError(ctx, "resync_failed", err.Error())
		return fmt.Errorf("op resync: %w", err)
	}
	states := make(map[string]model.MachineState)
	for _, op := range ops {
		if target := claimedState(op); target != "" {
			states[op.MachineID.String()] = target
		}
	}
	return c.writeTyped(ctx, TypeOpResync, "", OpResyncMsg{Ops: ops, MachineStates: states})
}

// claimedState is the transient machine state an active op implies: the
// claim's target (opstate.ClaimTransition) for a claimed/running lifecycle
// op, "" otherwise. Pending ops imply nothing (the machine is still at its
// baseline); a power op's ClaimTransition is ("", nil).
func claimedState(op model.Operation) model.MachineState {
	if op.Status != model.OpClaimed && op.Status != model.OpRunning {
		return ""
	}
	target, _ := opstate.ClaimTransition(op.Type)
	return target
}

// siteActiveOpsLocked returns the active-op set SCOPED TO THIS CONN'S
// SITE. Caller holds opStreamMu.
//
// Confidentiality filter: ListActiveOperations returns the FLEET-WIDE
// set, and Operation.Params carries per-site secrets (BMC/PDU passwords,
// tailnet auth keys). Only ops whose machine is in the slug's own cached
// identity snapshot go on the wire; this preserves the pre-cutover
// baseline where ops rode the per-slug SiteSnapshot.
//
// The membership is read from the hub cache here, under opStreamMu and
// after the DB read, rather than taken from the snapshot the hello reply
// carried: a Push that lands after the hello reply but before the hello
// resync, and whose fan-out skipped its op re-send because the conn was
// not op-ready yet, is then still covered, and so is a Push that lands
// during the DB read. The cache always holds the slug by now: handleHello's
// cold path caches its rebuild, and Push caches before fanning out.
func (c *workerConn) siteActiveOpsLocked() ([]model.Operation, error) {
	fleet, err := c.hub.cfg.ActiveOps.ListActiveOperations()
	if err != nil {
		return nil, fmt.Errorf("list active ops: %w", err)
	}
	snap, ok := c.hub.snapshotFor(c.slug)
	if !ok {
		return nil, fmt.Errorf("no cached snapshot for site %q to scope the op set by", c.slug)
	}
	members := make(map[uuid.UUID]bool, len(snap.Machines))
	for i := range snap.Machines {
		members[snap.Machines[i].ID] = true
	}
	ops := make([]model.Operation, 0, len(fleet))
	for _, op := range fleet {
		if !members[op.MachineID] {
			continue // another site's op: its params must not reach this worker
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// send is the Push fan-out write path. Hub.Push queues it on the conn's
// outbound writer, so it runs in order with the conn's other snapshots
// and op messages. It writes the snapshot, then re-sends the site's
// active ops (resendOpsAfterPush).
//
// A failed snapshot write force-closes the connection, as a failed
// op-stream write does: the re-sent ops are only actionable for a worker
// holding the snapshot, and the reconnect's hello delivers both.
func (c *workerConn) send(snap model.SiteSnapshot) {
	// The write runs under a timeout so a worker that stops reading fails
	// it and is force-closed, rather than holding up its outbound writer.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.writeTyped(ctx, TypeSnapshot, "", c.hub.snapshotMsg(snap)); err != nil {
		c.log.Warn("fan-out snapshot write failed; forcing reconnect", "err", err)
		_ = c.conn.CloseNow()
		return
	}
	c.resendOpsAfterPush()
}

// resendOpsAfterPush re-sends every active op of the site, scoped to the
// slug's newly cached membership, as an op_delta. PublishOpDelta filters
// by the membership cached at publish time, so an op created on a machine
// before the snapshot holding that machine was pushed never reached this
// worker; this delivers it, and likewise a later version published while
// the machine was missing from the cached snapshot.
//
// Each re-sent delta carries the transient state a claimed op implies
// (claimedState), as the hello resync derives it. Of an op's deltas only
// the claim's sets the worker's overlay, and a re-send can stand in for
// it: when that delta was filtered out, or when the re-send reads a
// just-committed claim and is written before the claim's own delta, which
// the worker then drops as a version it already holds.
//
// The re-send reads the op rows when the outbound writer runs it, not when
// Push queued it, so a re-sent row can be newer than an op message queued
// behind it: a create's pending delta queued after the snapshot reaches
// the worker after a re-send that already read the claim. The worker drops
// that older version too, so the publish-order guarantee covers what was
// published, not rows the re-send reads.
//
// op_delta rather than op_resync: the worker applies op_delta
// last-writer-wins by Version and drops a version it already holds, so
// the ops it is up to date on are left alone, including progress it has
// recorded locally without a Version bump (a claim or a failure whose
// op_update global has not processed yet). An op_resync full-replaces the
// op set and would revert that progress to global's row on every push.
//
// A conn that is not op-ready yet skips the re-send: Push cached the
// snapshot before this fan-out began, so the pending hello resync, which
// reads the membership under opStreamMu, is already scoped by it (see
// workerConn). Any failure force-closes the connection, as sendOpMessage
// does, so the reconnect's hello resync covers what this could not.
func (c *workerConn) resendOpsAfterPush() {
	c.opStreamMu.Lock()
	defer c.opStreamMu.Unlock()
	if !c.opReady {
		return
	}
	ops, err := c.siteActiveOpsLocked()
	if err != nil {
		c.log.Warn("post-push op re-send failed; forcing reconnect", "err", err)
		_ = c.conn.CloseNow()
		return
	}
	for _, op := range ops {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := c.writeTyped(ctx, TypeOpDelta, "", OpDeltaMsg{Operation: op, MachineState: claimedState(op)})
		cancel()
		if err != nil {
			c.log.Warn("post-push op re-send failed; forcing reconnect", "op_id", op.ID, "err", err)
			_ = c.conn.CloseNow()
			return
		}
	}
}

// snapshotMsg wraps snap with the global's build identity and op-stream
// protocol version, for both the hello reply and the Push fan-out.
func (h *Hub) snapshotMsg(snap model.SiteSnapshot) SnapshotMsg {
	return SnapshotMsg{
		Snapshot:       snap,
		Version:        h.cfg.Version,
		PackageVersion: h.cfg.PackageVersion,
		Proto:          OpStreamProto,
	}
}

func (c *workerConn) dispatch(ctx context.Context, env Envelope) error {
	switch env.Type {
	case TypeHeartbeat:
		// Heartbeat is fire-and-forget; refreshes last_seen_at. serial=nil
		// leaves the stored serial (set at hello time) untouched.
		if _, err := c.hub.cfg.Auth.TouchSiteWorker(c.slug, c.workerID, "", nil); err != nil {
			c.log.Warn("heartbeat touch failed", "err", err)
		}
		return nil
	case TypeObservation:
		// Run handler in a goroutine so one slow observation (e.g.
		// commission_report hitting a sluggish NetBox) doesn't block
		// the read loop. Each observation carries its own request_id;
		// responses can arrive out of order and the worker tracks the
		// mapping on its side.
		go c.handleObservation(ctx, env)
		return nil
	case TypeOpUpdate:
		// Process synchronously. Spawning a goroutine would break
		// worker-side ordering: a sequence of phase updates followed
		// by a terminal status could race, landing the terminal state
		// before an earlier phase that then stomps on it, or marking
		// an op completed before the phase that preceded it arrives.
		// Handler is fast (one Postgres round-trip) so blocking the
		// read loop briefly is fine. Slow handlers would need a
		// per-connection serial queue, not a goroutine-per-message.
		c.handleOpUpdate(ctx, env)
		return nil
	case TypeLogBatch:
		// Process synchronously to keep persistence ordered: a log
		// batch followed by an op_update reflecting the same phase
		// should land in the order the worker sent them.
		c.handleLogBatch(ctx, env)
		return nil
	case TypeCastBatch:
		// Process synchronously: per-machine ordering of cast frames
		// matters (t_us is monotonic), and a goroutine per batch
		// would let later batches land before earlier ones if the
		// scheduler reorders them.
		c.handleCastBatch(ctx, env)
		return nil
	case TypeCastResume:
		// Synchronous RPC — the worker waits on the rpc_response
		// before starting its pump session.
		c.handleCastResume(ctx, env)
		return nil
	case TypeClaimOp:
		// claim_op is the worker → global RPC for claiming a pending
		// operation. The hub uses the WS-authenticated identity
		// (c.workerID), NEVER any worker_id field on the wire — that
		// field was removed from ClaimOpMsg precisely to prevent a
		// compromised worker from claiming on behalf of another.
		c.handleClaimOp(ctx, env)
		return nil
	case TypePowerStatusResp:
		// Reply to a hub-originated power_status RPC. Route to the
		// waiting Hub.PowerStatus call by request_id.
		var resp RPCResponseMsg
		if err := unmarshalData(env.Data, &resp); err != nil {
			c.log.Warn("malformed power_status_response", "req_id", env.RequestID, "err", err)
			resp = RPCResponseMsg{Error: fmt.Sprintf("malformed power_status_response from worker: %v", err)}
		}
		c.hub.deliverRPCReply(env.RequestID, resp)
		return nil
	case TypeBootStatusResp:
		// Reply to a hub-originated boot_status RPC. Same routing as
		// power_status_response (deliverRPCReply is keyed by request_id).
		var resp RPCResponseMsg
		if err := unmarshalData(env.Data, &resp); err != nil {
			c.log.Warn("malformed boot_status_response", "req_id", env.RequestID, "err", err)
			resp = RPCResponseMsg{Error: fmt.Sprintf("malformed boot_status_response from worker: %v", err)}
		}
		c.hub.deliverRPCReply(env.RequestID, resp)
		return nil
	case TypeDHCPLeasesResp:
		// Reply to a hub-originated dhcp_leases RPC. Same routing as
		// power_status_response.
		var resp RPCResponseMsg
		if err := unmarshalData(env.Data, &resp); err != nil {
			c.log.Warn("malformed dhcp_leases_response", "req_id", env.RequestID, "err", err)
			resp = RPCResponseMsg{Error: fmt.Sprintf("malformed dhcp_leases_response from worker: %v", err)}
		}
		c.hub.deliverRPCReply(env.RequestID, resp)
		return nil
	case TypeHello:
		c.writeError(ctx, "protocol_error", "hello already received")
		return fmt.Errorf("duplicate hello")
	default:
		c.log.Warn("unknown message type", "type", env.Type)
		return nil
	}
}

// handleObservation decodes an observation envelope and dispatches to
// the configured ObservationHandler, then sends an rpc_response
// carrying the handler's outcome. The worker's ackPending map keys
// on request_id, so we must ALWAYS reply with the same request_id
// the worker sent — even on decode error — or the outbox row will
// never ack and the worker will replay forever.
func (c *workerConn) handleObservation(parent context.Context, env Envelope) {
	reqID := env.RequestID
	if reqID == "" {
		// No request_id means the worker doesn't want an ack. Still
		// run the handler so the side effects happen, but don't send
		// a response.
		c.log.Debug("observation without request_id, running handler without ack")
	}

	var msg ObservationMsg
	if err := unmarshalData(env.Data, &msg); err != nil {
		c.log.Warn("malformed observation", "req_id", reqID, "err", err)
		c.replyObservation(reqID, fmt.Errorf("decode observation: %w", err))
		return
	}
	obs := model.Observation{
		MachineID:   msg.MachineID,
		OperationID: msg.OperationID,
		Event:       msg.Event,
		Data:        msg.Data,
		// Stamped from the authenticated connection identity, never
		// from the wire payload (ObservationMsg carries no such
		// fields, so there is nothing for a worker to forge here).
		SiteSlug: c.slug,
		WorkerID: c.workerID,
	}
	// Detach from the connection ctx via WithoutCancel: the worker
	// expects its ack back even if it dropped mid-handle, and the
	// handler's side effects (NetBox writes, Postgres commits) should
	// not abort partway. The per-observation timeout still caps cost.
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent),
		c.hub.cfg.ObservationTimeout,
	)
	defer cancel()
	handlerErr := c.hub.cfg.ObsHandler.HandleObservation(ctx, obs)
	if handlerErr != nil {
		c.log.Warn("observation handler failed",
			"event", obs.Event, "machine_id", obs.MachineID, "err", handlerErr)
	}
	c.replyObservation(reqID, handlerErr)
}

// handleOpUpdate decodes an op_update envelope and routes it to the
// configured OpUpdateHandler. Fire-and-forget — no rpc_response
// contract with the worker. Errors are logged at the hub so they
// surface in operator-visible logs without blocking the worker.
//
// Uses context.WithoutCancel to detach from the connection's ctx:
// once we've read the bytes off the wire, the worker considers the
// update delivered and won't retry. If the connection drops mid-
// handle (and we were still attached), we'd abort the DB write and
// the op would hang. The per-handler timeout caps cost; shutdown
// still gets a bounded wait.
func (c *workerConn) handleOpUpdate(parent context.Context, env Envelope) {
	var msg OpUpdateMsg
	if err := unmarshalData(env.Data, &msg); err != nil {
		c.log.Warn("malformed op_update", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent),
		c.hub.cfg.OpUpdateTimeout,
	)
	defer cancel()
	if err := c.hub.cfg.OpUpdateHandler.HandleOpUpdate(ctx, msg); err != nil {
		c.log.Warn("op_update handler failed",
			"op_id", msg.OperationID, "phase", msg.Phase, "status", msg.Status,
			"err", err)
	}
}

// handleLogBatch decodes a log_batch envelope and routes to the
// configured LogBatchHandler. Fire-and-forget. Detached ctx for the
// same reason as handleOpUpdate.
func (c *workerConn) handleLogBatch(parent context.Context, env Envelope) {
	var msg LogBatchMsg
	if err := unmarshalData(env.Data, &msg); err != nil {
		c.log.Warn("malformed log_batch", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent),
		c.hub.cfg.LogBatchTimeout,
	)
	defer cancel()
	if err := c.hub.cfg.LogBatchHandler.HandleLogBatch(ctx, c.workerID, msg); err != nil {
		c.log.Warn("log_batch handler failed",
			"worker_id", c.workerID,
			"count", len(msg.Envelopes), "err", err)
	}
}

// handleCastBatch decodes a cast_batch envelope and
// routes frames to the configured CastBatchHandler. Same shape as
// handleLogBatch — fire-and-forget, detached ctx, log on error.
func (c *workerConn) handleCastBatch(parent context.Context, env Envelope) {
	var msg CastBatchMsg
	if err := unmarshalData(env.Data, &msg); err != nil {
		c.log.Warn("malformed cast_batch", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent),
		c.hub.cfg.LogBatchTimeout, // same budget; bulk pg insert
	)
	defer cancel()
	if err := c.hub.cfg.CastBatchHandler.HandleCastBatch(ctx, c.workerID, msg); err != nil {
		c.log.Warn("cast_batch handler failed",
			"worker_id", c.workerID,
			"count", len(msg.Frames), "err", err)
	}
}

// handleCastResume answers a cast_resume RPC with a CastResumeResponseMsg
// wrapped in RPCResponseMsg. The worker's pending-RPC map keys on
// request_id; we always reply with the same request_id (even on error)
// so the worker doesn't hang waiting forever.
func (c *workerConn) handleCastResume(parent context.Context, env Envelope) {
	reqID := env.RequestID
	var resp RPCResponseMsg
	defer func() {
		if reqID == "" {
			c.log.Warn("cast_resume without request_id, dropping reply")
			return
		}
		writeCtx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		if err := c.conn.WriteTyped(writeCtx, TypeRPCResponse, reqID, resp); err != nil {
			c.log.Warn("cast_resume reply write failed", "req_id", reqID, "err", err)
		}
	}()

	var msg CastResumeRequestMsg
	if err := unmarshalData(env.Data, &msg); err != nil {
		resp.Error = fmt.Sprintf("decode cast_resume: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent),
		c.hub.cfg.OpUpdateTimeout, // pg round-trip; same budget
	)
	defer cancel()
	out, err := c.hub.cfg.CastResumeHandler.HandleCastResume(ctx, c.workerID, msg)
	if err != nil {
		resp.Error = err.Error()
		return
	}
	data, err := json.Marshal(out)
	if err != nil {
		resp.Error = fmt.Sprintf("marshal CastResumeResponseMsg: %v", err)
		return
	}
	resp.Data = data
}

// handleClaimOp decodes a claim_op envelope and atomically claims
// the operation for the WS-authenticated worker. The reply uses
// TypeClaimOpResp (with the same request_id the worker sent) so
// the worker's pending-RPC map can route the response.
//
// Identity contract: the workerID passed to ClaimOperation is
// c.workerID (set at connection auth time), NOT anything the
// worker can put on the wire. ClaimOpMsg deliberately has no
// worker_id field; even if a malicious client adds one to the
// JSON, json.Unmarshal silently drops it (Go's default).
//
// Response shape:
//   - success: rpc_response{ data: {operation: <op>} }
//   - already-claimed (store returned nil/nil):
//     rpc_response{ data: {already_claimed: true} } — structured,
//     NOT an error, so the worker counts it as a lost race
//     (metrics outcome "lost") instead of a claim failure.
//   - hard error (DB / decode, including the interlock's pre-CAS
//     reads): rpc_response{ error: "..." } — the worker rolls its
//     local claimed mark back to pending and retries.
//
// On success the hub also invalidates the claiming worker's site
// snapshot so every worker in the pop (especially the claim-race
// loser) sees the op leave pending without waiting for an
// unrelated push.
func (c *workerConn) handleClaimOp(parent context.Context, env Envelope) {
	reqID := env.RequestID
	var resp RPCResponseMsg

	defer func() {
		if reqID == "" {
			// Worker didn't ask for a response; nothing to write.
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.writeTyped(ctx, TypeClaimOpResp, reqID, resp); err != nil {
			c.log.Warn("claim_op response write failed", "req_id", reqID, "err", err)
		}
	}()

	var msg ClaimOpMsg
	if err := unmarshalData(env.Data, &msg); err != nil {
		resp.Error = fmt.Sprintf("decode claim_op: %v", err)
		return
	}
	opID, err := uuid.Parse(msg.OperationID)
	if err != nil {
		resp.Error = fmt.Sprintf("invalid op_id %q: %v", msg.OperationID, err)
		return
	}

	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent),
		c.hub.cfg.OpUpdateTimeout, // claim is the same shape: single PG round-trip
	)
	defer cancel()
	_ = ctx // store doesn't take ctx today; reserved for future deadline plumbing

	// P4b self-power interlock: BEFORE the store CAS, decide whether THIS
	// worker may serve THIS op. Only outlet ops (PDURuntime.Outlet==true) are
	// gated; everything else passes through unaffected. A declined claim
	// returns the SAME already-claimed response a lost race produces, so the
	// worker moves on and the op stays pending for a partner (INV-3
	// decline-not-handoff). The gate holds no db lock — it does a plain op
	// read (params) + a registered-serial-set read. A failed read is a hard
	// error, not a decline: nothing is granted (fail-closed), and the worker
	// rolls the op back to pending and retries, where already_claimed would
	// make it drop an op that is still pending.
	eligible, reason, err := c.outletClaimEligible(opID)
	if err != nil {
		c.log.Warn("claim_op interlock read failed; claim not granted",
			"op_id", opID, "worker_id", c.workerID, "err", err)
		resp.Error = fmt.Sprintf("claim failed: %v; nothing was claimed, retry the claim", err)
		return
	}
	if !eligible {
		c.log.Info("outlet-op claim declined by self-power interlock",
			"op_id", opID, "worker_id", c.workerID, "serial", c.verifiedSerial, "reason", reason)
		out, mErr := json.Marshal(ClaimOpResponse{AlreadyClaimed: true})
		if mErr != nil {
			resp.Error = fmt.Sprintf("marshal response: %v", mErr)
			return
		}
		resp.Data = out
		return
	}

	op, err := c.hub.cfg.OpClaimer.ClaimOperation(opID, c.workerID)
	if err != nil {
		c.log.Warn("claim_op store error",
			"op_id", opID, "worker_id", c.workerID, "err", err)
		resp.Error = fmt.Sprintf("claim failed: %v", err)
		return
	}
	if op == nil {
		// Routine race outcome: another worker claimed first (or the
		// op is gone). Structured, not an error — the worker maps
		// this to ClaimLost and stops retrying.
		out, err := json.Marshal(ClaimOpResponse{AlreadyClaimed: true})
		if err != nil {
			resp.Error = fmt.Sprintf("marshal response: %v", err)
			return
		}
		resp.Data = out
		return
	}
	c.log.Info("operation claimed",
		"op_id", op.ID, "type", op.Type, "machine_id", op.MachineID,
		"worker_id", c.workerID)

	// The claim advanced the op to claimed AND wrote a TRANSIENT machine
	// state (commissioning/deploying). Publish an op_delta carrying the
	// claimed row + that transient state so every worker — in particular
	// the claim-race loser — sees the op leave pending and overlays the
	// transient state, without an identity rebuild. Transient states ride
	// the op stream alone; no Invalidate here (plan: claim invalidate
	// disappears). ClaimTransition gives the transient target ("" for a
	// power op, which writes no machine state).
	transient, _ := opstate.ClaimTransition(op.Type)
	c.hub.PublishOpDelta(*op, transient)

	out, err := json.Marshal(ClaimOpResponse{Operation: op})
	if err != nil {
		c.log.Warn("claim_op response marshal failed", "op_id", opID, "err", err)
		resp.Error = fmt.Sprintf("marshal response: %v", err)
		return
	}
	resp.Data = out
}

// outletClaimEligible is the workerConn-scoped wrapper around the P4b
// interlock (outletClaimEligibility). It reads the pending op and, for an
// outlet op, the registered site-worker serial set for this conn's slug, then
// applies the eligibility invariants. Returns (eligible, reason, err); reason
// is a log-only diagnostic when ineligible.
//
// A store failure on either read returns err, and the caller grants nothing
// (a hard-AC-cut primitive must never fail open). It is deliberately not a
// decline: the op read runs before the op is known to be an outlet op, and a
// decline tells the worker another worker owns the op, so a transient store
// error would strand every op type, deploys included. An err is answered as a
// retryable rpc error instead. A genuinely-absent op (nil, nil from
// GetOperationIfExists) is treated as eligible so the claim proceeds to the
// CAS, which reports already-claimed — the interlock never fabricates a
// decline for an op that simply raced away.
func (c *workerConn) outletClaimEligible(opID uuid.UUID) (bool, string, error) {
	op, err := c.hub.cfg.OpClaimer.GetOperationIfExists(opID)
	if err != nil {
		return false, "", fmt.Errorf("read op before claim: %w", err)
	}
	if op == nil {
		// Op is gone; let the CAS report already-claimed.
		return true, "", nil
	}
	// Applicability probe FIRST, leniently: only ops explicitly marked
	// pdu_runtime.outlet are gated by the interlock. A params blob that won't
	// even probe (malformed/legacy JSON) is treated as NOT an outlet op and
	// passes through to the normal executor machinery — the interlock must not
	// broaden its blast radius to strand unrelated deploy/power ops on a
	// params-decode quirk. Only once an op is CONFIRMED to be an outlet op does
	// the strict, fail-closed decode in outletClaimEligibility apply.
	if !isOutletOp(op.Params) {
		return true, "", nil // not an outlet op: outside the interlock
	}
	// Outlet op: INV-1 needs the persisted registered-worker-serial set for
	// the slug (connected OR not). A store error grants nothing (see above).
	registered, err := c.registeredWorkerSerials()
	if err != nil {
		return false, "", fmt.Errorf("resolve registered worker serials for the outlet interlock: %w", err)
	}
	eligible, reason := outletClaimEligibility(op, c.verifiedSerial, registered)
	return eligible, reason, nil
}

// registeredWorkerSerials returns the set of non-empty verified hardware
// serials for the workers registered against this conn's slug (connected or
// not), for the INV-1 serving-worker check. An unverified worker (serial "")
// contributes no entry — it can't be matched as a downstream anyway. Entries
// keep the workers' SMBIOS spelling; outletClaimEligibility matches them
// against the NetBox downstream serial ignoring case.
func (c *workerConn) registeredWorkerSerials() (map[string]bool, error) {
	workers, err := c.hub.cfg.Auth.ListSiteWorkers(c.slug)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(workers))
	for i := range workers {
		if s := workers[i].Serial; s != "" {
			set[s] = true
		}
	}
	return set, nil
}

// replyObservation sends the rpc_response that the worker's ackPending
// map is waiting on. reqID == "" means the worker didn't ask for an
// ack, so we skip the write. Errors that wrap ErrTerminal mark the
// response as terminal so the worker drops the outbox row instead of
// retrying indefinitely.
func (c *workerConn) replyObservation(reqID string, handlerErr error) {
	if reqID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var resp RPCResponseMsg
	if handlerErr != nil {
		resp.Error = handlerErr.Error()
		resp.Terminal = errors.Is(handlerErr, ErrTerminal)
	}
	if err := c.writeTyped(ctx, TypeRPCResponse, reqID, resp); err != nil {
		c.log.Warn("observation rpc_response write failed", "req_id", reqID, "err", err)
	}
}

// ErrTerminal is the sentinel observation handlers wrap to tell the
// hub (and in turn the worker) that retrying won't help. The worker
// sees Terminal=true in the rpc_response and drops the outbox row.
var ErrTerminal = errors.New("terminal")

// enqueue adds write to the conn's outbound queue without blocking.
// writeOutbound runs queued writes one at a time in queue order, which is
// what makes the worker receive them in the order they were queued:
// starting a goroutine per write and serializing the writes with a mutex
// leaves their order to the scheduler.
//
// A full queue means the worker has stopped reading. enqueue then
// force-closes the connection rather than block the publisher (the API, a
// sweep, or a worker's read loop) or drop the message unnoticed: the
// reconnect's hello snapshot and op_resync supersede everything that was
// queued.
func (c *workerConn) enqueue(kind string, write func()) {
	select {
	case c.outbound <- write:
	default:
		c.log.Warn("outbound queue full; forcing reconnect", "kind", kind, "queue_size", cap(c.outbound))
		_ = c.conn.CloseNow()
	}
}

// writeOutbound is the conn's outbound writer: it runs queued writes in
// order until stop is closed. Writes still queued then are discarded; the
// conn is closing, and the worker's next hello replaces them.
func (c *workerConn) writeOutbound(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case write := <-c.outbound:
			write()
		}
	}
}

func (c *workerConn) writeTyped(ctx context.Context, msgType, reqID string, payload any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteTyped(ctx, msgType, reqID, payload)
}

func (c *workerConn) writeError(ctx context.Context, code, message string) {
	if err := c.writeTyped(ctx, TypeError, "", ErrorMsg{Code: code, Message: message}); err != nil {
		c.log.Warn("error write failed", "err", err)
	}
}

// writeErrorProto is writeError carrying the global's OpStreamProto, used
// on the hello-rejection path so a refused worker can log the proto it
// needs to reach.
func (c *workerConn) writeErrorProto(ctx context.Context, code, message string) {
	if err := c.writeTyped(ctx, TypeError, "", ErrorMsg{Code: code, Message: message, Proto: OpStreamProto}); err != nil {
		c.log.Warn("error write failed", "err", err)
	}
}

func unmarshalData(raw []byte, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
