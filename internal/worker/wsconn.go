package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/logring"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
	"github.com/bluesky-social/slagworks/internal/sdnotify"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// WSConn is the site worker's WebSocket connection to the global API.
// It speaks the Plan 2 protocol: authenticate via ?token=, send a hello
// carrying the last-applied snapshot SeqNum, accept either a fresh
// snapshot or a no_change ack, then enter the steady-state outbox
// drain + heartbeat + log forward loop.
type WSConn struct {
	config    WSConnConfig
	cache     *Cache
	localDB   *LocalDB
	log       *slog.Logger
	connected atomic.Bool

	connMu sync.RWMutex
	conn   *ws.Conn // current active connection (nil when disconnected)

	syncedOnce sync.Once
	syncedCh   chan struct{} // closed after first successful snapshot apply

	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage

	// ackPending maps request_id (sent on an observation envelope) to
	// the pending outbox entry it originated from. When the global
	// sends rpc_response{ok:true} we ack the SQLite row via
	// cache.AckObservation; on a non-terminal error we use the stored
	// PendingOutbox to requeue just this entry with exponential
	// backoff (cache.RequeueRejected), instead of calling
	// ReloadOutboxFromDisk and risking duplicate sends of other
	// still-in-flight observations.
	ackPendingMu sync.Mutex
	ackPending   map[string]PendingOutbox

	// drainMu serializes drainOnce invocations. Without it, the old
	// connection's ctx.Done-triggered final drainOnce could run
	// concurrently with the new connection's first drainOnce, racing
	// on the outbox drain and the log ring drain.
	drainMu sync.Mutex

	// logRing buffers structured log envelopes from the site
	// worker's slog handler, the in-process SOL pump, and the
	// /logs/ingest HTTP endpoint (agent POSTs). drainLoop pumps
	// batches over the WS to global. See docs/dev/architecture.md
	// for the logging design.
	logRing *logring.Ring

	// castFrameCh is the buffered channel CastPumps push frames into.
	// castDrainLoop pumps batches over the WS to global as
	// TypeCastBatch. Bounded so a stuck WS connection can't grow the
	// queue unbounded; dropped-on-full with a warning is the right
	// behavior for SOL bytes (they're observability data, not
	// load-bearing state).
	castFrameCh chan CastFrame

	// upgrading guards single-flight execution of the auto-upgrade
	// helper so concurrent version-mismatch ticks (heartbeat reconnects,
	// snapshot + no_change racing) don't fire it twice. On success the
	// helper restarts us and this process state vanishes. On FAILURE we
	// reset the latch after upgradeRetryCooldown so a later tick can
	// retry: most upgrade failures are transient (apt lock, download
	// blip, sudo -n hiccup), and a permanently-stuck latch silently
	// strands the worker on a stale binary — exactly the wedge
	// that bit the OOB DHCP cutover.
	upgrading atomic.Bool

	// lastGlobalBuild is the build the global most recently reported
	// (hello reply or snapshot push). The global reports its build on
	// nothing else, so after a failed upgrade no further check would
	// arrive until a reconnect or a NetBox-driven snapshot change;
	// heartbeatLoop retries against this value instead (retryUpgrade).
	// On 2026-10-29 every worker's first attempt failed and the fleet
	// sat on the old build for 40 minutes, until a redeploy forced
	// reconnects.
	lastGlobalBuild atomic.Pointer[globalBuild]

	// upgradeCooldown overrides upgradeRetryCooldown when non-zero.
	// Tests set it near-zero to exercise the failure-reset path without
	// sleeping for minutes.
	upgradeCooldown time.Duration

	// upgradeExec runs the upgrade helper for the given target build and
	// returns its combined output. nil means the default sudo exec.
	// Tests inject a stub so the failure path is deterministic and
	// never touches a real /usr/bin/slagworks-upgrade.
	upgradeExec func(target globalBuild) ([]byte, error)

	// powerFactory builds a power.Controller for hub-originated
	// power_status RPCs (the BMC read path). Same factory the
	// executor uses for actuation ops; nil means the worker rejects
	// power_status requests (unit tests / configurations without a
	// BMC path).
	powerFactory PowerControllerFactory

	// dhcpLeaseSource returns the site's current unexpired DHCP
	// leases (dhcpHandler.Leases) for hub-originated dhcp_leases RPCs.
	// nil (unset, or Load() returning nil) means no live DHCP handler —
	// the worker answers with an error rather than a bogus empty
	// snapshot. atomic.Pointer, not a plain field: unlike powerFactory
	// (set once before Run starts), this is rewired on every
	// startSiteServices call — including standby VIP transitions —
	// while the WS read loop is already live answering RPCs
	// concurrently on other goroutines (roast finding, 2026-07-13).
	dhcpLeaseSource atomic.Pointer[func() []dhcp.LeaseInfo]
}

// WSConnConfig carries the site worker identity + global endpoint.
// Token is the plaintext bearer minted by RegisterSiteWorker on the
// global; WorkerID and FQDN are the worker's own advertised identity
// in the hello payload. Version is the build SHA stamped into
// main.version at link time, reported on hello so the global can
// detect site-vs-global drift.
type WSConnConfig struct {
	GlobalURL string
	Token     string
	WorkerID  string
	FQDN      string
	Version   string
	// Serial is the worker host's hardware serial (SMBIOS, read via
	// ReadSelfSerial at startup, placeholders normalized to ""). Reported
	// in HelloMsg.Serial so global can join it to the NetBox device for
	// hardware-attested identity. "" means unverified — refused at hello
	// under serial enforcement (production); accepted only by a dev/test
	// global with no serial resolver wired.
	Serial string
	// SMCIPMIToolStatus reports whether the operator-installed Supermicro
	// SMCIPMITool is usable on this worker (an smcipmitool.StatusX value).
	// Called on each hello — global keeps that value until the worker
	// reconnects — and on each status read for the install hint. nil
	// reports "" (unknown) and suppresses the hints.
	SMCIPMIToolStatus func() string
}

func NewWSConn(cfg WSConnConfig, cache *Cache, localDB *LocalDB, logRing *logring.Ring) *WSConn {
	return &WSConn{
		config:     cfg,
		cache:      cache,
		localDB:    localDB,
		log:        slog.Default(),
		pending:    make(map[string]chan json.RawMessage),
		ackPending: make(map[string]PendingOutbox),
		syncedCh:   make(chan struct{}),
		logRing:    logRing,
		// 1024 frames @ ≤4KB each = 4MB buffer ceiling. Per-pump
		// flush rate is well under 100 frames/s, so 10 active machines
		// take ~1s to fill this if the WS is fully wedged.
		castFrameCh: make(chan CastFrame, 1024),
	}
}

// PushCastFrame implements CastSink. Site-worker CastPumps push
// captured frames here; castDrainLoop drains them onto the WS to
// global. Drops on full (logged) so a wedged WS can't blow site
// memory.
//
// Defensive copy of f.Data: today's cast pump already copies bytes
// before constructing the frame, but the sink contract is "you
// hand me bytes, I own them" — without the copy a future caller
// (or refactor) that reuses its read buffer would race with the
// drain loop's marshal. The copy is cheap relative to the WS
// write that follows.
func (w *WSConn) PushCastFrame(f CastFrame) {
	f.Data = append([]byte(nil), f.Data...)
	select {
	case w.castFrameCh <- f:
	default:
		w.log.Warn("castFrameCh full, dropping frame",
			"machine_id", f.MachineID, "bytes", len(f.Data))
	}
}

func (w *WSConn) SetLogger(l *slog.Logger) { w.log = l }

// SetPowerControllerFactory wires the BMC controller factory used to
// answer hub-originated power_status RPCs. Must be called before Run
// for the worker to serve status reads; without it every power_status
// request is answered with an error.
func (w *WSConn) SetPowerControllerFactory(f PowerControllerFactory) { w.powerFactory = f }

// SetDHCPLeaseSource wires the site's DHCP handler's Leases method as
// the source for hub-originated dhcp_leases RPCs. Safe to call
// concurrently with an in-flight RPC (atomic swap) — needed because
// standby VIP transitions rewire this while the WS connection, and its
// read loop, are already live. Must be called at least once before Run
// for the worker to serve lease reads; before that (or if f is nil)
// every dhcp_leases request is answered with an error.
func (w *WSConn) SetDHCPLeaseSource(f func() []dhcp.LeaseInfo) { w.dhcpLeaseSource.Store(&f) }

// Connected reports whether the WebSocket is connected and has completed
// its first handshake.
func (w *WSConn) Connected() bool { return w.connected.Load() }

// ForceDisconnect closes the active connection to trigger a reconnect.
// Used by tests to simulate network disruption.
func (w *WSConn) ForceDisconnect() {
	w.connMu.RLock()
	c := w.conn
	w.connMu.RUnlock()
	if c != nil {
		_ = c.CloseNow()
	}
}

// ClaimOp atomically claims a pending operation by sending a
// claim_op RPC over the WebSocket and waiting for the matching
// rpc_response. Implements the executor's Claimer contract:
//
//   - (op, nil): this worker won the claim.
//   - (nil, nil): the hub replied with the structured
//     ClaimOpResponse{AlreadyClaimed: true} — another worker won
//     the race or the op is no longer pending. The executor counts
//     this as a lost claim, not an error.
//   - (nil, err): transport failure (WS down, RPC timeout) or a
//     hub-side hard error (rpc_response{error:"..."}, e.g. DB
//     failure).
//
// The hub uses the WS-authenticated worker_id of this connection,
// so this method does NOT pass a worker_id parameter — the
// connection's identity IS the claim's identity.
func (w *WSConn) ClaimOp(ctx context.Context, opID uuid.UUID) (*model.Operation, error) {
	if !w.Connected() {
		return nil, fmt.Errorf("ws not connected")
	}
	w.connMu.RLock()
	conn := w.conn
	w.connMu.RUnlock()
	if conn == nil {
		return nil, fmt.Errorf("ws not connected")
	}

	reqID := uuid.NewString()
	ch := w.registerPending(reqID)
	defer w.removePending(reqID)

	if err := conn.WriteTyped(ctx, ws.TypeClaimOp, reqID, ws.ClaimOpMsg{
		OperationID: opID.String(),
	}); err != nil {
		return nil, fmt.Errorf("claim_op write: %w", err)
	}

	raw, err := w.waitResponse(ctx, ch)
	if err != nil {
		return nil, fmt.Errorf("claim_op response: %w", err)
	}
	return decodeClaimOpResponse(raw)
}

// decodeClaimOpResponse maps the hub's rpc_response payload onto the
// Claimer contract: (op, nil) on a won claim, (nil, nil) on the
// structured already-claimed rejection, (nil, err) on hub-side hard
// errors or malformed payloads. Split from ClaimOp so the mapping is
// unit-testable without a live WS connection.
func decodeClaimOpResponse(raw json.RawMessage) (*model.Operation, error) {
	var rpcResp ws.RPCResponseMsg
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode rpc_response: %w", err)
	}
	if rpcResp.Error != "" {
		return nil, fmt.Errorf("hub rejected claim: %s", rpcResp.Error)
	}

	var claimResp ws.ClaimOpResponse
	if err := json.Unmarshal(rpcResp.Data, &claimResp); err != nil {
		return nil, fmt.Errorf("decode claim_op response: %w", err)
	}
	if claimResp.AlreadyClaimed {
		// Lost the race — routine under dual-worker. (nil, nil) per
		// the Claimer contract so the executor counts ClaimLost.
		return nil, nil
	}
	if claimResp.Operation == nil {
		// Defensive — the hub should have populated exactly one of
		// Error, AlreadyClaimed, or Operation. An empty success is a
		// contract violation.
		return nil, fmt.Errorf("hub returned empty claim_op response")
	}
	return claimResp.Operation, nil
}

// WaitForSync blocks until the first snapshot apply completes or ctx
// is cancelled.
func (w *WSConn) WaitForSync(ctx context.Context) error {
	select {
	case <-w.syncedCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run connects to the global API and enters the message loop. It
// reconnects with exponential backoff on disconnect. Blocks until ctx
// is cancelled.
func (w *WSConn) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		wasConnected, err := w.connectAndRun(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		w.connected.Store(false)
		if wasConnected {
			backoff = time.Second
		}
		w.log.Warn("WebSocket disconnected, reconnecting", "err", err, "backoff", backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (w *WSConn) connectAndRun(ctx context.Context) (connected bool, err error) {
	wsURL := w.buildURL()
	conn, err := ws.Dial(ctx, wsURL, nil)
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}

	// Scope heartbeat + drain to THIS connection's lifetime. Without
	// this, each reconnect leaks the previous connection's goroutines:
	// they stay blocked on cache.Wake holding the (now closed) conn
	// pointer. When the next RequeueOutbox call wakes them, they'd
	// write-fail → requeue → re-wake in a tight loop, 100% CPU, and
	// steal items from the live connection's drain.
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	defer func() {
		w.connMu.Lock()
		w.conn = nil
		w.connMu.Unlock()
		_ = conn.CloseNow()
	}()

	w.connMu.Lock()
	w.conn = conn
	w.connMu.Unlock()

	// Site worker reads can be up to ws.MaxMessageBytes (full snapshot
	// with commission data for all machines at the site).
	conn.SetReadLimit(ws.MaxMessageBytes)

	// Send hello first. The hub uses last_seq_num to decide whether to
	// reply with a fresh snapshot or a no_change ack.
	hello := ws.HelloMsg{
		WorkerID:    w.config.WorkerID,
		FQDN:        w.config.FQDN,
		LastSeqNum:  w.cache.HelloSeqNum(),
		Version:     w.config.Version,
		Proto:       ws.OpStreamProto,
		Serial:      w.config.Serial,
		SMCIPMITool: w.smcToolStatus(),
	}
	if err := conn.WriteTyped(ctx, ws.TypeHello, "", hello); err != nil {
		return false, fmt.Errorf("send hello: %w", err)
	}

	// Read the hello reply — must be snapshot or no_change, and it MUST
	// carry an op-stream Proto >= ours. A reply with an absent/lower Proto
	// is an old global that dropped our unknown hello.Proto field and
	// speaks no op stream: operations left the snapshot, so continuing
	// would silently strand every op. Refuse LOUD (connection-level error),
	// including on no_change — the hole a snapshot-only check leaves.
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		return false, fmt.Errorf("read hello reply: %w", err)
	}
	switch env.Type {
	case ws.TypeSnapshot:
		if err := w.checkReplyProto(ws.TypeSnapshot, env.Data); err != nil {
			return false, err
		}
		if err := w.applySnapshot(env.Data); err != nil {
			return false, err
		}
	case ws.TypeNoChange:
		if err := w.checkReplyProto(ws.TypeNoChange, env.Data); err != nil {
			return false, err
		}
		// Local cache is already up to date with the server's view.
		// Still read the global's Version so auto-upgrade fires on
		// reconnects between snapshot bumps.
		w.applyNoChange(env.Data)
	case ws.TypeError:
		return false, fmt.Errorf("hello rejected: %s", string(env.Data))
	default:
		return false, fmt.Errorf("expected snapshot or no_change, got %q", env.Type)
	}

	w.connected.Store(true)
	w.syncedOnce.Do(func() { close(w.syncedCh) })
	_ = sdnotify.Send(sdnotify.Ready)
	w.log.Info("WebSocket connected",
		"seq", w.cache.LastSeqNum(),
		"machines", w.cache.MachineCount(),
		"images", w.cache.ImageCount(),
	)

	// Reconnect reset. Held under drainMu so any in-flight drainOnce
	// from the previous connection's final ctx.Done pass completes
	// before we clear ackPending / reload from disk. Without this
	// guard:
	//   - the old drainOnce could deliver an obs and write into our
	//     newly cleared ackPending; the response is bound for the
	//     dead conn, so the entry leaks.
	//   - the old drainOnce could RequeueOutbox AFTER we reload,
	//     duplicating items that reload already put back.
	w.drainMu.Lock()
	w.ackPendingMu.Lock()
	w.ackPending = make(map[string]PendingOutbox)
	w.ackPendingMu.Unlock()
	// Pull unacked observations persisted in SQLite back into the
	// in-memory queue. Critical for recovery from disconnect during
	// the ack window: the observation was sent + SQLite row still
	// exists, but it's no longer in the in-memory outbox because
	// DrainOutbox removed it for the previous send. Without reload,
	// we'd wait for worker restart to retry.
	w.cache.ReloadOutboxFromDisk()
	w.drainMu.Unlock()

	// Start heartbeat and outbox drain goroutines scoped to the
	// per-connection context so they exit on disconnect.
	go w.heartbeatLoop(connCtx, conn)
	go w.drainLoop(connCtx, conn)

	// Enter read loop.
	for {
		env, err := conn.ReadMessage(ctx)
		if err != nil {
			return true, fmt.Errorf("read: %w", err)
		}

		switch env.Type {
		case ws.TypeSnapshot:
			if err := w.applySnapshot(env.Data); err != nil {
				return true, err
			}
		case ws.TypeNoChange:
			// No-op. Hub may send these during reconciliation as an
			// explicit ack; we don't currently generate such a path
			// server-side but tolerate it.
		case ws.TypeOpDelta:
			w.applyOpDelta(env.Data)
		case ws.TypeOpResync:
			w.applyOpResync(env.Data)
		case ws.TypeOpRemoved:
			w.applyOpRemoved(env.Data)
		case ws.TypeClaimOpResp, ws.TypeRPCResponse:
			// Observation acks claim request_ids from the ackPending
			// map first; if the reqID belongs to an outbox observation,
			// we ack the row and consume the response. Otherwise it's
			// an active RPC (ClaimOp) waiting on pending{}.
			if !w.handleObservationAck(env.RequestID, env.Data) {
				w.deliverRPCResponse(env.RequestID, env.Data)
			}
		case ws.TypePowerStatus:
			// Hub-originated RPC: read the machine's BMC power state
			// and reply inline. Run in a goroutine — a slow BMC
			// (seconds) must not stall snapshot applies and op
			// dispatch on this read loop. The reply write is
			// serialized by the websocket library.
			go w.handlePowerStatus(connCtx, conn, env)
		case ws.TypeBootStatus:
			// Hub-originated RPC: read the machine's BMC boot-device
			// override and reply inline. Same goroutine rationale as
			// power_status.
			go w.handleBootStatus(connCtx, conn, env)
		case ws.TypeDHCPLeases:
			// Hub-originated RPC: snapshot the site's DHCP lease table
			// and reply inline. Goroutine for symmetry with the BMC RPCs
			// above, though this read is fast (in-memory map walk, no
			// I/O).
			go w.handleDHCPLeases(connCtx, conn, env)
		case ws.TypeError:
			w.log.Warn("error from global", "data", string(env.Data))
		default:
			w.log.Warn("unknown message from global", "type", env.Type)
		}
	}
}

// buildURL constructs the WS upgrade URL. Scheme is converted
// http→ws, https→wss; token is the single query parameter.
func (w *WSConn) buildURL() string {
	base := w.config.GlobalURL
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	u, _ := url.Parse(base + "/api/v1/ws/site")
	q := u.Query()
	q.Set("token", w.config.Token)
	u.RawQuery = q.Encode()
	return u.String()
}

// checkReplyProto enforces the op-stream skew gate on a hello reply: the
// reply MUST carry Proto >= ws.OpStreamProto. An absent/lower Proto is an
// old global that dropped our hello.Proto field and speaks no op stream;
// since operations left the snapshot, continuing would silently strand
// every op. Also belt-and-suspenders: reject a snapshot that STILL carries
// an "operations" key (a stale global that hasn't removed the field).
// Returns a connection-level error on mismatch so the operator sees it.
func (w *WSConn) checkReplyProto(replyType string, data json.RawMessage) error {
	// Peek Proto + a legacy operations key off the raw reply without a
	// full typed decode, so the check works regardless of the reply shape.
	var probe struct {
		Proto      int             `json:"proto"`
		Snapshot   json.RawMessage `json:"snapshot"`
		Operations json.RawMessage `json:"operations"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("decode %s reply for proto check: %w", replyType, err)
	}
	if probe.Proto < ws.OpStreamProto {
		return fmt.Errorf("global op-stream protocol %d < required %d on %s reply; "+
			"the global daemon is older than this worker — this should not happen with GitOps deploys, "+
			"check the image SHA global's deployment pins", probe.Proto, ws.OpStreamProto, replyType)
	}
	// Belt-and-suspenders: a snapshot that still carries operations is a
	// pre-Tier-2 global that somehow passed the Proto check (shouldn't
	// happen — the field is gone). Reject loudly.
	if len(probe.Snapshot) > 0 {
		var snapProbe struct {
			Operations json.RawMessage `json:"operations"`
		}
		if err := json.Unmarshal(probe.Snapshot, &snapProbe); err == nil && len(snapProbe.Operations) > 0 {
			return fmt.Errorf("global sent a snapshot still carrying operations on %s reply; "+
				"operations left the snapshot in Tier 2 — the global daemon is stale", replyType)
		}
	}
	return nil
}

// applyOpDelta decodes an op_delta and applies it to the cache's op layer.
func (w *WSConn) applyOpDelta(data json.RawMessage) {
	var msg ws.OpDeltaMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		w.log.Warn("decode op_delta", "err", err)
		return
	}
	w.cache.ApplyOpDelta(msg.Operation, msg.MachineState)
}

// applyOpResync decodes an op_resync (full active-op set + overlay) and
// full-replaces the cache's op layer, clearing the untrusted flag.
func (w *WSConn) applyOpResync(data json.RawMessage) {
	var msg ws.OpResyncMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		w.log.Warn("decode op_resync", "err", err)
		return
	}
	w.cache.ApplyOpResync(msg.Ops, msg.MachineStates)
}

// applyOpRemoved decodes an op_removed and drops the op (tombstoned),
// moving the overlay to the carried durable state if present.
func (w *WSConn) applyOpRemoved(data json.RawMessage) {
	var msg ws.OpRemovedMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		w.log.Warn("decode op_removed", "err", err)
		return
	}
	w.cache.ApplyOpRemoved(msg.OperationID, msg.MachineID, msg.Version, msg.MachineState)
}

// applySnapshot decodes a snapshot message, applies it to the cache,
// and persists the new SeqNum via LocalDB. Also triggers an
// auto-upgrade check if the global advertises a different build SHA.
func (w *WSConn) applySnapshot(data json.RawMessage) error {
	var msg ws.SnapshotMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("decode snapshot: %w", err)
	}
	w.cache.ApplySnapshot(&msg.Snapshot)
	if w.localDB != nil {
		w.localDB.SetSyncCursor(msg.Snapshot.SeqNum)
	}
	w.checkVersion(globalBuild{SHA: msg.Version, PackageVersion: msg.PackageVersion})
	return nil
}

// applyNoChange handles the no_change reply. The snapshot is already
// up to date; we just want to read the global's reported build so
// auto-upgrade can fire even when the snapshot doesn't change.
func (w *WSConn) applyNoChange(data json.RawMessage) {
	var msg ws.NoChangeMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		// no_change carries only the build and proto fields; a
		// malformed payload here is non-fatal — we just don't get a
		// version check.
		w.log.Warn("decode no_change", "err", err)
		return
	}
	w.checkVersion(globalBuild{SHA: msg.Version, PackageVersion: msg.PackageVersion})
}

// globalBuild is the global daemon's build as it reports it on snapshot
// and no_change: its git SHA, and the deb package version
// scripts/build-deb.sh built that SHA as ("" when global was not built
// by build-deb.sh, so there is no package to install).
type globalBuild struct {
	SHA            string
	PackageVersion string
}

// checkVersion compares the global's reported build SHA against ours
// and, when SLAGWORKS_AUTO_UPGRADE=1, kicks off the upgrade helper to
// install the global's package version. An empty SHA on either side
// disables comparison (dev builds).
//
// The upgrade is single-flight via w.upgrading; concurrent snapshot/
// no_change callers from heartbeat reconnects don't fire it twice.
func (w *WSConn) checkVersion(remote globalBuild) {
	if remote.SHA == "" || w.config.Version == "" {
		return
	}
	w.lastGlobalBuild.Store(&remote)
	if remote.SHA == w.config.Version {
		return
	}
	w.log.Warn("version mismatch with global",
		"local_version", w.config.Version,
		"remote_version", remote.SHA,
		"remote_package_version", remote.PackageVersion)

	if os.Getenv("SLAGWORKS_AUTO_UPGRADE") != "1" {
		w.log.Info("auto-upgrade not enabled (SLAGWORKS_AUTO_UPGRADE != 1); staying on current version")
		return
	}
	if remote.PackageVersion == "" {
		w.log.Warn("cannot auto-upgrade to an unpackaged build: global reports no package version "+
			"(it was not built by scripts/build-deb.sh); staying on current version until global runs a packaged build",
			"remote_version", remote.SHA)
		return
	}
	if !w.upgrading.CompareAndSwap(false, true) {
		// Already in flight from an earlier reconnect. The helper
		// either restarts us (this state goes away) or it failed and
		// re-firing wouldn't help.
		return
	}
	go w.runUpgrade(remote)
}

// retryUpgrade re-runs checkVersion against the global's last reported
// build when auto-upgrade is on and no attempt (or its cooldown) is in
// flight. heartbeatLoop calls it every tick, so a failed upgrade retries
// once the cooldown releases the latch, always targeting the newest
// build the global reported, and stops as soon as the versions match.
// Checking the latch first keeps the mismatch warning to one per retry
// rather than one per heartbeat. A build with no package version has
// nothing to retry: checkVersion logged why when the global reported
// it, and a packaged global announces itself on its next snapshot or
// reconnect.
func (w *WSConn) retryUpgrade() {
	if os.Getenv("SLAGWORKS_AUTO_UPGRADE") != "1" || w.upgrading.Load() {
		return
	}
	if b := w.lastGlobalBuild.Load(); b != nil && b.PackageVersion != "" {
		w.checkVersion(*b)
	}
}

// upgradeRetryCooldown is how long runUpgrade keeps the single-flight
// latch held after a FAILED upgrade before clearing it, so the next
// heartbeat's retryUpgrade can retry without hot-looping the helper on
// a persistent failure.
const upgradeRetryCooldown = 2 * time.Minute

// upgradeCommand is the production helper invocation:
// `sudo -n /usr/bin/slagworks-upgrade <git-sha> <package-version>`.
func upgradeCommand(target globalBuild) *exec.Cmd {
	return exec.Command("sudo", "-n", "/usr/bin/slagworks-upgrade", target.SHA, target.PackageVersion)
}

func defaultUpgradeExec(target globalBuild) ([]byte, error) {
	return upgradeCommand(target).CombinedOutput()
}

// runUpgrade execs the upgrade helper for target. On success the helper
// restarts slagworksd-site, which terminates this process — runUpgrade
// returns only if the helper failed OR exited zero without restarting
// us. BOTH of those are non-progress outcomes: w.config.Version is
// immutable for this process, so the version mismatch that triggered
// the upgrade is still live, and an un-throttled latch release would
// let the next mismatch tick re-exec the helper immediately. So every
// return path holds the latch through the cooldown before clearing it,
// then releases so a future tick can retry.
func (w *WSConn) runUpgrade(target globalBuild) {
	w.log.Info("starting auto-upgrade",
		"target_sha", target.SHA,
		"target_package_version", target.PackageVersion)
	cooldown := upgradeRetryCooldown
	if w.upgradeCooldown > 0 {
		cooldown = w.upgradeCooldown
	}
	// Whatever happens, don't strand the worker on a stale
	// binary. Hold the latch through the cooldown (suppressing
	// duplicate concurrent fires) then release so a later tick retries.
	defer func() {
		time.Sleep(cooldown)
		w.upgrading.Store(false)
	}()

	execFn := w.upgradeExec
	if execFn == nil {
		execFn = defaultUpgradeExec
	}
	out, err := execFn(target)
	if err != nil {
		w.log.Error("auto-upgrade failed; will retry after cooldown",
			"err", err,
			"output", strings.TrimSpace(string(out)),
			"cooldown", cooldown)
		return
	}
	// Helper exited zero but DIDN'T restart us. Since the mismatch is
	// still live (see above), treat this like a failure for throttling
	// purposes — the deferred cooldown+release handles the retry.
	w.log.Warn("auto-upgrade helper exited without restart; will retry after cooldown",
		"output", strings.TrimSpace(string(out)),
		"cooldown", cooldown)
}

// drainLoop drains the cache outbox (observations + op updates) and
// the in-memory log envelope ring, sending them over the WebSocket.
// It wakes immediately when new data is written via cache.Wake; the
// log ring runs its own drain ticker because envelopes don't go
// through the cache outbox path.
func (w *WSConn) drainLoop(ctx context.Context, conn *ws.Conn) {
	if w.logRing != nil {
		go w.logRingDrainLoop(ctx, conn)
	}
	go w.castDrainLoop(ctx, conn)
	for {
		select {
		case <-ctx.Done():
			w.drainOnce(ctx, conn)
			return
		case <-w.cache.Wake:
			w.drainOnce(ctx, conn)
		}
	}
}

// logRingDrainLoop is a per-connection goroutine that pumps log
// envelopes from w.logRing to global as TypeLogBatch messages.
// Drains in batches of up to logRingBatchMax envelopes per WS write;
// blocks on Ring.Wait between batches. On send failure the batch is
// dropped (the ring is the operator-facing buffer; we don't double-
// buffer).
func (w *WSConn) logRingDrainLoop(ctx context.Context, conn *ws.Conn) {
	const batchMax = 256
	for {
		if !w.logRing.Wait(ctx) {
			// SIGTERM-flush: drain whatever's left so a routine
			// restart doesn't unnecessarily lose buffered envelopes.
			batch := w.logRing.Drain(batchMax)
			if len(batch) > 0 {
				_ = w.sendLogBatch(ctx, conn, batch)
			}
			return
		}
		batch := w.logRing.Drain(batchMax)
		if len(batch) == 0 {
			continue
		}
		if err := w.sendLogBatch(ctx, conn, batch); err != nil {
			// A failed write means the connection is bad. Force-close it
			// so connectAndRun's read loop errors and the reconnect path
			// restarts ALL drain goroutines — just returning here would
			// leave a half-alive connection with no log drain for its
			// remaining lifetime (silent observability loss).
			w.log.Warn("log_batch send failed; forcing reconnect",
				"count", len(batch), "err", err)
			_ = conn.CloseNow()
			return
		}
	}
}

func (w *WSConn) drainOnce(ctx context.Context, conn *ws.Conn) {
	// Serialize drain invocations. Reconnects can briefly overlap
	// two drainLoops (old ctx.Done path + new first wake); running
	// both drainOnce bodies concurrently would double-send items.
	w.drainMu.Lock()
	defer w.drainMu.Unlock()

	obs, updates := w.cache.DrainOutbox()

	// DrainOutbox removes items from the in-memory queue before we
	// try to send them. On send failure we requeue the remainder so
	// the next wake retries — otherwise a transiently-failed
	// op_update or observation would vanish for this session.
	//
	// Each observation is sent with a fresh request_id paired with
	// its SQLite row ID via ackPending. When the global's
	// rpc_response arrives (handled in connectAndRun's read loop),
	// we ack the row — closing the commission outbox contract: the
	// worker keeps re-sending until global confirms both NetBox and
	// Postgres writes succeeded.
	for i, p := range obs {
		reqID := uuid.New().String()
		frame, err := observationFrame(reqID, p.Obs)
		if err == nil && len(frame) > ws.MaxMessageBytes {
			err = fmt.Errorf("encoded message is %d bytes, over the %d-byte limit global reads",
				len(frame), ws.MaxMessageBytes)
		}
		if err != nil {
			// No retry can deliver this row. Global closes the
			// connection on an oversized message without answering
			// it, and the reconnect's ReloadOutboxFromDisk puts the
			// row back first, so sending or requeueing it would cut
			// every later observation and op_update off from global
			// for good. Drop it, as a terminal rejection does.
			w.log.Error("dropping outbox observation that cannot be sent to global",
				"event", p.Obs.Event, "machine_id", p.Obs.MachineID, "op_id", p.Obs.OperationID,
				"row_id", p.RowID, "err", err)
			w.cache.AckObservation(p.RowID)
			continue
		}
		// Track BEFORE sending: otherwise the global can process
		// the observation and write rpc_response back faster than
		// we return from WriteFrame, and the read loop drops the
		// ack because ackPending doesn't know about reqID yet.
		// Untrack on send failure so we don't leak the entry.
		w.trackAck(reqID, p)
		if err := conn.WriteFrame(ctx, frame); err != nil {
			w.untrackAck(reqID)
			w.log.Warn("send observation failed, requeueing remainder", "err", err, "unsent", len(obs)-i)
			w.cache.RequeueOutbox(obs[i:], updates)
			return
		}
	}

	for i, u := range updates {
		if err := w.sendOpUpdate(ctx, conn, u); err != nil {
			w.log.Warn("send op_update failed, requeueing remainder", "err", err, "unsent", len(updates)-i)
			w.cache.RequeueOutbox(nil, updates[i:])
			return
		}
	}
}

func (w *WSConn) heartbeatLoop(ctx context.Context, conn *ws.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msg := ws.HeartbeatMsg{WorkerID: w.config.WorkerID}
			if err := conn.WriteTyped(ctx, ws.TypeHeartbeat, "", msg); err != nil {
				w.log.Warn("heartbeat send failed", "err", err)
				return
			}
			_ = sdnotify.Send(sdnotify.Watchdog)
			w.retryUpgrade()
		}
	}
}

// --- Outbound message sending ---

func (w *WSConn) sendLogBatch(ctx context.Context, conn *ws.Conn, envelopes []slogkit.Envelope) error {
	msg := ws.LogBatchMsg{Envelopes: envelopes}
	return conn.WriteTyped(ctx, ws.TypeLogBatch, "", msg)
}

// castDrainLoop drains w.castFrameCh and ships frames to global as
// TypeCastBatch. Each batch is whatever's currently in the channel
// (up to castDrainBatchMax) per drain cycle. The drain wakes on
// every channel receive, so a single trickle frame is sent
// immediately; bursts are coalesced into one WS write.
func (w *WSConn) castDrainLoop(ctx context.Context, conn *ws.Conn) {
	const castDrainBatchMax = 64
	batch := make([]ws.CastFrameMsg, 0, castDrainBatchMax)
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-w.castFrameCh:
			if !ok {
				return
			}
			batch = append(batch, ws.CastFrameMsg{
				MachineID: f.MachineID,
				TUs:       f.TUs,
				Data:      f.Data,
			})
		}
		// Drain anything else queued without blocking, up to the cap.
	gather:
		for len(batch) < castDrainBatchMax {
			select {
			case f, ok := <-w.castFrameCh:
				if !ok {
					break gather
				}
				batch = append(batch, ws.CastFrameMsg{
					MachineID: f.MachineID,
					TUs:       f.TUs,
					Data:      f.Data,
				})
			default:
				break gather
			}
		}
		if err := conn.WriteTyped(ctx, ws.TypeCastBatch, "", ws.CastBatchMsg{Frames: batch}); err != nil {
			// Same rationale as logRingDrainLoop: a dead write path with
			// a live connection would silently drop SOL frames forever.
			// Force the reconnect instead.
			w.log.Warn("cast_batch send failed; forcing reconnect",
				"count", len(batch), "err", err)
			_ = conn.CloseNow()
			return
		}
		batch = batch[:0]
	}
}

// powerStatusTimeout bounds a single hub-originated BMC status read.
// ASUS/AMI BMCs answer Redfish reads in single-digit seconds when
// healthy; the global API caps the whole round trip at ~15s, so the
// worker gives the BMC slightly less to leave room for the reply to
// reach slagctl with a real error instead of a generic timeout.
const powerStatusTimeout = 12 * time.Second

// handlePowerStatus answers a hub-originated power_status RPC: build
// a BMC controller for the machine using the request's bmc_runtime
// creds, read the power state, reply with state + transport. Always
// replies (success or error) with the hub's request_id — the hub's
// rpcPending entry would otherwise dangle until its ctx timeout.
//
// Runs on its own goroutine (BMC reads are seconds-slow); the reply
// write is safe concurrent with the read loop because ws.Conn writes
// are serialized by coder/websocket.
func (w *WSConn) handlePowerStatus(ctx context.Context, conn *ws.Conn, env ws.Envelope) {
	resp := w.runPowerStatus(ctx, env.Data)
	if env.RequestID == "" {
		w.log.Warn("power_status without request_id, dropping reply")
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.WriteTyped(writeCtx, ws.TypePowerStatusResp, env.RequestID, resp); err != nil {
		w.log.Warn("power_status reply write failed", "req_id", env.RequestID, "err", err)
	}
}

// runPowerStatus is the side-effecting body of handlePowerStatus,
// split out so unit tests can exercise the decode/factory/read flow
// without a live WS connection. Returns the RPCResponseMsg to send.
func (w *WSConn) runPowerStatus(ctx context.Context, data json.RawMessage) ws.RPCResponseMsg {
	var req ws.PowerStatusRequestMsg
	if err := json.Unmarshal(data, &req); err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("decode power_status request: %v", err)}
	}
	log := w.log.With("machine_id", req.MachineID)

	if w.powerFactory == nil {
		return ws.RPCResponseMsg{Error: "site worker has no power controller factory configured"}
	}
	m := w.cache.MachineByID(req.MachineID)
	if m == nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("machine %s not in site snapshot", req.MachineID)}
	}
	var amtRT *model.AMTRuntime
	if len(req.AMTRuntime) > 0 {
		amtRT = &model.AMTRuntime{}
		if err := json.Unmarshal(req.AMTRuntime, amtRT); err != nil {
			return ws.RPCResponseMsg{Error: fmt.Sprintf("decode amt_runtime: %v", err)}
		}
	}
	// Same transport backstop as the executor: an AMT endpoint is never
	// read as IPMI/Redfish, and the BMC path needs a BMC IP.
	switch {
	case amtRT == nil && m.AMT:
		return ws.RPCResponseMsg{Error: fmt.Sprintf("machine %s's OOB endpoint is Intel AMT but the request carries no amt_runtime; refusing to read it as IPMI/Redfish", req.MachineID)}
	case amtRT == nil && m.BMCIP == "":
		return ws.RPCResponseMsg{Error: fmt.Sprintf("machine %s has no BMC IP in the snapshot", req.MachineID)}
	}

	var rt model.BMCRuntime
	if len(req.BMCRuntime) > 0 {
		if err := json.Unmarshal(req.BMCRuntime, &rt); err != nil {
			return ws.RPCResponseMsg{Error: fmt.Sprintf("decode bmc_runtime: %v", err)}
		}
	}

	oob := "bmc"
	if amtRT != nil {
		oob = "amt"
	}
	ctx, cancel := context.WithTimeout(ctx, powerStatusTimeout)
	defer cancel()
	ctrl, err := w.powerFactory(ctx, m, rt, nil, amtRT)
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("%s connect: %v", oob, err)}
	}
	// Close on a fresh context: if the read timed out, ctx is already
	// dead and a graceful BMC logout would be skipped — leaking one of
	// the ~5 session slots these AMI BMCs hold (and don't free on
	// abrupt disconnect).
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = ctrl.Close(closeCtx)
	}()

	// No chassis-serial gate here: this is a pure read — it cannot
	// damage the wrong box (the spec's "Read-only ops MAY skip the
	// serial check"), and skipping it halves the BMC round-trips on
	// a latency-sensitive interactive path.
	state, err := ctrl.PowerStatus(ctx)
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("%s power status: %v", oob, err)}
	}
	log.Info("power status read", "state", state, "transport", ctrl.Transport())

	out, err := json.Marshal(ws.PowerStatusResponseMsg{
		State:     string(state),
		Transport: ctrl.Transport(),
		Hint:      w.smcToolHint(m, ctrl),
	})
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("marshal power_status response: %v", err)}
	}
	return ws.RPCResponseMsg{Data: out}
}

// handleBootStatus answers a hub-originated boot_status RPC. Mirrors
// handlePowerStatus: build a controller from the request's bmc_runtime,
// read the boot override, reply inline with the hub's request_id.
func (w *WSConn) handleBootStatus(ctx context.Context, conn *ws.Conn, env ws.Envelope) {
	resp := w.runBootStatus(ctx, env.Data)
	if env.RequestID == "" {
		w.log.Warn("boot_status without request_id, dropping reply")
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.WriteTyped(writeCtx, ws.TypeBootStatusResp, env.RequestID, resp); err != nil {
		w.log.Warn("boot_status reply write failed", "req_id", env.RequestID, "err", err)
	}
}

// runBootStatus is the side-effecting body of handleBootStatus, split out
// for unit testing. Reuses the power-status decode/factory flow, then
// type-asserts the controller to power.BootManager. A controller that
// isn't a BootManager (bmclib BMCController, PDU) replies
// Supported:false rather than an error — that's an expected, operator-
// facing "this host doesn't support boot ops yet", not a failure.
func (w *WSConn) runBootStatus(ctx context.Context, data json.RawMessage) ws.RPCResponseMsg {
	var req ws.BootStatusRequestMsg
	if err := json.Unmarshal(data, &req); err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("decode boot_status request: %v", err)}
	}
	log := w.log.With("machine_id", req.MachineID)

	if w.powerFactory == nil {
		return ws.RPCResponseMsg{Error: "site worker has no power controller factory configured"}
	}
	m := w.cache.MachineByID(req.MachineID)
	if m == nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("machine %s not in site snapshot", req.MachineID)}
	}
	if m.AMT {
		// Boot-override read is a BootManager capability AMT doesn't
		// implement (it only arms a one-time PXE on lifecycle/power ops).
		// Answer without connecting: never read an AMT endpoint as IPMI.
		out, err := json.Marshal(ws.BootStatusResponseMsg{Supported: false, Transport: "amt"})
		if err != nil {
			return ws.RPCResponseMsg{Error: fmt.Sprintf("marshal boot_status response: %v", err)}
		}
		return ws.RPCResponseMsg{Data: out}
	}
	if m.BMCIP == "" {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("machine %s has no BMC IP in the snapshot", req.MachineID)}
	}

	var rt model.BMCRuntime
	if len(req.BMCRuntime) > 0 {
		if err := json.Unmarshal(req.BMCRuntime, &rt); err != nil {
			return ws.RPCResponseMsg{Error: fmt.Sprintf("decode bmc_runtime: %v", err)}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, powerStatusTimeout)
	defer cancel()
	ctrl, err := w.powerFactory(ctx, m, rt, nil, nil)
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("bmc connect: %v", err)}
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = ctrl.Close(closeCtx)
	}()

	// Boot ops are an optional capability. A controller that isn't a
	// BootManager (bmclib BMCController, PDU) gets Supported:false so the
	// CLI prints "not supported on this host" instead of a confusing error.
	bm, ok := ctrl.(power.BootManager)
	if !ok {
		out, err := json.Marshal(ws.BootStatusResponseMsg{Supported: false, Hint: w.smcToolHint(m, ctrl)})
		if err != nil {
			return ws.RPCResponseMsg{Error: fmt.Sprintf("marshal boot_status response: %v", err)}
		}
		return ws.RPCResponseMsg{Data: out}
	}

	// No chassis-serial gate: a pure read can't damage the wrong box, and
	// skipping it halves the BMC round-trips on this interactive path.
	bo, err := bm.BootStatus(ctx)
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("bmc boot status: %v", err)}
	}
	log.Info("boot status read", "device", bo.Device, "valid", bo.Valid, "persistent", bo.Persistent, "transport", ctrl.Transport())

	out, err := json.Marshal(ws.BootStatusResponseMsg{
		Supported:  true,
		Device:     string(bo.Device),
		Persistent: bo.Persistent,
		Valid:      bo.Valid,
		Raw:        bo.Raw,
		Transport:  ctrl.Transport(),
	})
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("marshal boot_status response: %v", err)}
	}
	return ws.RPCResponseMsg{Data: out}
}

// smcToolStatus returns this worker's SMCIPMITool status, "" when unwired.
func (w *WSConn) smcToolStatus() string {
	if w.config.SMCIPMIToolStatus == nil {
		return ""
	}
	return w.config.SMCIPMIToolStatus()
}

// smcToolHint returns the operator-facing nudge to install SMCIPMITool
// when m is a Supermicro host that ctrl — the controller that just served
// the read — served over the generic path because the tool is missing.
// Keyed on the controller actually built, so the hint can't contradict the
// transport printed next to it. "" otherwise, including when the operator
// disabled the tool (SLAGWORKS_SMCIPMITOOL=off).
func (w *WSConn) smcToolHint(m *model.MachineSnapshot, ctrl power.Controller) string {
	if !m.IsSupermicro() {
		return ""
	}
	if _, ok := ctrl.(*power.SMCController); ok {
		return ""
	}
	if w.smcToolStatus() != smcipmitool.StatusMissing {
		return ""
	}
	name := w.config.FQDN
	if name == "" {
		name = w.config.WorkerID
	}
	return smcipmitool.MissingHint(name)
}

// handleDHCPLeases answers a hub-originated dhcp_leases RPC: snapshot
// the site's DHCP lease table, classify + enrich each lease, reply
// with the hub's request_id. Always replies (success or error) — the
// hub's rpcPending entry would otherwise dangle until its ctx timeout.
func (w *WSConn) handleDHCPLeases(ctx context.Context, conn *ws.Conn, env ws.Envelope) {
	resp := w.runDHCPLeases()
	if env.RequestID == "" {
		w.log.Warn("dhcp_leases without request_id, dropping reply")
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := conn.WriteTyped(writeCtx, ws.TypeDHCPLeasesResp, env.RequestID, resp); err != nil {
		w.log.Warn("dhcp_leases reply write failed", "req_id", env.RequestID, "err", err)
	}
}

// runDHCPLeases is the side-effecting body of handleDHCPLeases, split
// out so unit tests can exercise the source/classify/enrich flow
// without a live WS connection. Returns the RPCResponseMsg to send.
func (w *WSConn) runDHCPLeases() ws.RPCResponseMsg {
	sourcePtr := w.dhcpLeaseSource.Load()
	if sourcePtr == nil || *sourcePtr == nil {
		return ws.RPCResponseMsg{Error: "site worker has no DHCP handler configured"}
	}

	leases := (*sourcePtr)()
	if leases == nil {
		// The source is wired but reports no live handler right now (a
		// standby worker mid-VIP-release — see cmd/slagworksd/site.go's
		// SetDHCPLeaseSource wiring). dhcp.Handler.Leases always returns
		// a non-nil (possibly empty) slice, so nil here is unambiguously
		// "no handler," not "handler with zero leases" — same error as
		// the source-unset case, not a bogus empty success.
		return ws.RPCResponseMsg{Error: "site worker has no DHCP handler configured"}
	}
	out := make([]ws.DHCPLeaseInfo, 0, len(leases))
	for _, l := range leases {
		info := ws.DHCPLeaseInfo{
			MAC:       l.MAC,
			IP:        l.IP,
			ExpiresAt: l.Expiry,
			Kind:      "dynamic",
			Pool:      l.Pool,
		}
		// Static iff the MAC's reservation IP matches the actually-leased
		// IP — a reservation-holding MAC could in principle be holding a
		// dynamic lease (reservation added after the lease), so classify
		// by the leased address, not by "does a reservation exist".
		if reserved := w.cache.ReservedIPForMAC(l.MAC); reserved != nil && reserved.String() == l.IP {
			info.Kind = "static"
		}
		if m := w.cache.MachineByMAC(l.MAC); m != nil {
			info.MachineID = m.ID.String()
			info.Hostname = m.Hostname
		}
		out = append(out, info)
	}

	data, err := json.Marshal(ws.DHCPLeasesResponseMsg{Leases: out})
	if err != nil {
		return ws.RPCResponseMsg{Error: fmt.Sprintf("marshal dhcp_leases response: %v", err)}
	}
	return ws.RPCResponseMsg{Data: data}
}

// CastResumeOffset is the worker→global RPC implementing
// CastResumeSource for the production pump. Sends a TypeCastResume
// request, awaits the matching rpc_response, returns the prev_max_us
// global computed.
func (w *WSConn) CastResumeOffset(ctx context.Context, machineID uuid.UUID) (int64, error) {
	if !w.Connected() {
		return 0, fmt.Errorf("ws not connected")
	}
	w.connMu.RLock()
	conn := w.conn
	w.connMu.RUnlock()
	if conn == nil {
		return 0, fmt.Errorf("ws not connected")
	}
	reqID := uuid.NewString()
	ch := w.registerPending(reqID)
	defer w.removePending(reqID)

	if err := conn.WriteTyped(ctx, ws.TypeCastResume, reqID, ws.CastResumeRequestMsg{
		MachineID: machineID,
	}); err != nil {
		return 0, fmt.Errorf("cast_resume write: %w", err)
	}
	raw, err := w.waitResponse(ctx, ch)
	if err != nil {
		return 0, fmt.Errorf("cast_resume response: %w", err)
	}
	var rpcResp ws.RPCResponseMsg
	if err := json.Unmarshal(raw, &rpcResp); err != nil {
		return 0, fmt.Errorf("decode rpc_response: %w", err)
	}
	if rpcResp.Error != "" {
		return 0, fmt.Errorf("cast_resume rpc error: %s", rpcResp.Error)
	}
	var resp ws.CastResumeResponseMsg
	if err := json.Unmarshal(rpcResp.Data, &resp); err != nil {
		return 0, fmt.Errorf("decode CastResumeResponseMsg: %w", err)
	}
	return resp.PrevMaxUs, nil
}

// ResumeOffsetUs satisfies CastResumeSource.
func (w *WSConn) ResumeOffsetUs(ctx context.Context, machineID uuid.UUID) (int64, error) {
	return w.CastResumeOffset(ctx, machineID)
}

// observationFrame encodes an outbox observation as the message drainOnce
// sends, so its size can be checked against ws.MaxMessageBytes first.
func observationFrame(reqID string, obs model.Observation) ([]byte, error) {
	msg := ws.ObservationMsg{
		MachineID:   obs.MachineID,
		OperationID: obs.OperationID,
		Event:       obs.Event,
		Timestamp:   obs.Timestamp.Format(time.RFC3339),
		Data:        obs.Data,
	}
	return ws.EncodeTyped(ws.TypeObservation, reqID, msg)
}

// trackAck records that a request_id corresponds to a particular
// outbox entry, so the later rpc_response can ack the right row or
// requeue just this entry on transient error. rowID == 0 (failed
// persist path) is silently dropped: there's nothing to ack.
func (w *WSConn) trackAck(reqID string, entry PendingOutbox) {
	if entry.RowID == 0 {
		return
	}
	w.ackPendingMu.Lock()
	w.ackPending[reqID] = entry
	w.ackPendingMu.Unlock()
}

// untrackAck removes a pending ack entry. Called when a send fails
// after trackAck registered the reqID — otherwise the entry leaks.
func (w *WSConn) untrackAck(reqID string) {
	w.ackPendingMu.Lock()
	delete(w.ackPending, reqID)
	w.ackPendingMu.Unlock()
}

// handleObservationAck is called when an rpc_response arrives whose
// request_id matches one we tracked for a sent observation. On
// success, acks the SQLite row. On terminal error, acks so the row
// stops cycling. On transient error, requeues just this entry in
// memory — NOT via ReloadOutboxFromDisk, which would pull back any
// other still-in-flight observations and cause duplicate sends.
func (w *WSConn) handleObservationAck(reqID string, data json.RawMessage) bool {
	w.ackPendingMu.Lock()
	entry, ok := w.ackPending[reqID]
	if ok {
		delete(w.ackPending, reqID)
	}
	w.ackPendingMu.Unlock()
	if !ok {
		return false
	}

	var resp ws.RPCResponseMsg
	if err := json.Unmarshal(data, &resp); err != nil {
		w.log.Warn("malformed rpc_response for observation ack", "req_id", reqID, "err", err)
		return true
	}
	if resp.Error != "" {
		if resp.Terminal {
			w.log.Warn("observation rejected terminally, dropping outbox row",
				"req_id", reqID, "row_id", entry.RowID, "err", resp.Error)
			w.cache.AckObservation(entry.RowID)
			return true
		}
		// Non-terminal rejection: requeue with per-row exponential
		// backoff instead of an immediate re-send. Log WARN
		// on the first rejection of a row and (rate-limited) while
		// pinned at the backoff cap; everything in between is Debug so
		// a persistent transient failure doesn't flood the journal.
		attempts, delay, warn := w.cache.RequeueRejected(entry)
		logFn := w.log.Debug
		if warn {
			logFn = w.log.Warn
		}
		logFn("observation rejected by global, requeueing with backoff",
			"req_id", reqID, "row_id", entry.RowID, "err", resp.Error,
			"attempts", attempts, "retry_in", delay.Truncate(time.Millisecond))
		return true
	}
	w.cache.AckObservation(entry.RowID)
	return true
}

func (w *WSConn) sendOpUpdate(ctx context.Context, conn *ws.Conn, update model.OperationUpdate) error {
	msg := ws.OpUpdateMsg{
		OperationID: update.OperationID,
		Status:      update.Status,
		Phase:       update.Phase,
		Result:      update.Result,
	}
	return conn.WriteTyped(ctx, ws.TypeOpUpdate, "", msg)
}

// --- Pending RPC plumbing ---

func (w *WSConn) registerPending(reqID string) chan json.RawMessage {
	ch := make(chan json.RawMessage, 1)
	w.pendingMu.Lock()
	w.pending[reqID] = ch
	w.pendingMu.Unlock()
	return ch
}

func (w *WSConn) removePending(reqID string) {
	w.pendingMu.Lock()
	delete(w.pending, reqID)
	w.pendingMu.Unlock()
}

func (w *WSConn) deliverRPCResponse(reqID string, data json.RawMessage) {
	w.pendingMu.Lock()
	ch, ok := w.pending[reqID]
	w.pendingMu.Unlock()
	if ok {
		ch <- data
	}
}

func (w *WSConn) waitResponse(ctx context.Context, ch chan json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case data := <-ch:
		return data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("RPC timeout")
	}
}
