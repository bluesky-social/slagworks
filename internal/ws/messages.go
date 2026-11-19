package ws

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// Envelope wraps all WebSocket messages with a type discriminator.
type Envelope struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id,omitempty"`
	Data      json.RawMessage `json:"data"`
}

// ParseEnvelope decodes the outer envelope from raw JSON.
func ParseEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("parse envelope: %w", err)
	}
	return env, nil
}

// MarshalEnvelope creates an Envelope with the given type and data payload.
func MarshalEnvelope(msgType string, requestID string, data any) ([]byte, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{
		Type:      msgType,
		RequestID: requestID,
		Data:      raw,
	})
}

// --- Site Worker -> Global ---

type ObservationMsg struct {
	MachineID   uuid.UUID       `json:"machine_id"`
	OperationID uuid.UUID       `json:"op_id,omitempty"`
	Event       string          `json:"event"`
	Timestamp   string          `json:"timestamp"`
	Data        json.RawMessage `json:"data,omitempty"`
}

type OpUpdateMsg struct {
	OperationID string          `json:"op_id"`
	Status      string          `json:"status,omitempty"`
	Phase       string          `json:"phase,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// LogBatchMsg is a worker→global push of structured log envelopes.
// Site workers buffer envelopes from agents (via /logs/ingest), the
// SOL pump (in-process), and their own slog handler in an in-memory
// ring; the WS forwarder drains the ring and sends batches as this
// message type. Global persists the envelopes to log_envelopes and
// re-emits them on stdout for alloy-logs / Loki.
type LogBatchMsg struct {
	Envelopes []slogkit.Envelope `json:"envelopes"`
}

// CastFrameMsg is one batched chunk of bytes captured from a
// machine's BMC SOL pty. This message is the wire shape between the
// site worker's CastPump and global.
//
// TUs is monotonic-per-cast microseconds, assigned by the site
// worker (prev_max_us + monotonic_since_pump_start_us). Global
// trusts this value verbatim; it does not recompute against its own
// clock.
type CastFrameMsg struct {
	MachineID uuid.UUID `json:"machine_id"`
	TUs       int64     `json:"t_us"`
	// Data is raw bytes — not base64, not JSON-string — as a JSON
	// number-array so non-UTF-8 bytes round-trip correctly.
	Data []byte `json:"data"`
}

// CastBatchMsg bundles multiple CastFrameMsgs in one WS write so
// the relay's flow-control logic only pays the per-write overhead
// once per pump's flush window.
type CastBatchMsg struct {
	Frames []CastFrameMsg `json:"frames"`
}

// CastResumeRequestMsg is the worker→global RPC asking for a
// machine's current resume offset (prev_max_us). Sent on each
// CastPump session start. Global replies with CastResumeResponseMsg
// wrapped in RPCResponseMsg.
type CastResumeRequestMsg struct {
	MachineID uuid.UUID `json:"machine_id"`
}

// CastResumeResponseMsg is global's reply: prev_max_us plus enough
// metadata for the API to render an asciicast header later. The
// site worker only needs PrevMaxUs; the rest is optional context
// for logging.
type CastResumeResponseMsg struct {
	CastID        int64     `json:"cast_id"`
	CastStartedAt time.Time `json:"cast_started_at"`
	Width         int       `json:"width"`
	Height        int       `json:"height"`
	PrevMaxUs     int64     `json:"prev_max_us"`
}

type HeartbeatMsg struct {
	WorkerID string `json:"worker_id"`
}

// --- Global -> Site Worker: power-status RPC ---

// PowerStatusRequestMsg is the global→worker RPC asking the site
// worker to read a machine's BMC power state and reply inline. This
// is the read path the BMC contract anticipated: actuation flows
// through the operation queue, but a status read creates no op row —
// the API handler RPCs the machine's site worker over the existing
// WS connection and returns the answer synchronously to slagctl.
//
// BMCRuntime carries the same JSON shape as op.Params["bmc_runtime"]
// (model.BMCRuntime): the device's pop/<pop>/bmc-creds-admin entry,
// resolved by the global at request time, plus extra_config quirks. It is a RawMessage so the ws package doesn't take a model
// dependency for a blob it only forwards; the producer (api) and
// consumer (worker) both marshal/unmarshal it as model.BMCRuntime.
//
// AMTRuntime (model.AMTRuntime) is set instead for a machine whose OOB
// endpoint is Intel AMT (bmc-creds-admin kind=amt); the worker then reads
// the state over WS-Man rather than IPMI/Redfish. Exactly one of the two
// is populated.
type PowerStatusRequestMsg struct {
	MachineID  uuid.UUID       `json:"machine_id"`
	BMCRuntime json.RawMessage `json:"bmc_runtime"`
	AMTRuntime json.RawMessage `json:"amt_runtime,omitempty"`
}

// PowerStatusResponseMsg is the worker's reply payload, wrapped in
// RPCResponseMsg.Data inside a power_status_response envelope.
// State is one of power.State's values ("on"/"off"/"unknown");
// Transport names what actually answered ("redfish", "ipmi", or the
// generic "bmc" when the provider couldn't be identified).
type PowerStatusResponseMsg struct {
	State     string `json:"state"`
	Transport string `json:"transport"`
	// Hint is an optional operator-facing note about how the read was
	// served — today, that a Supermicro host took the generic IPMI path
	// because SMCIPMITool isn't installed on the worker.
	Hint string `json:"hint,omitempty"`
}

// BootStatusRequestMsg is the global→worker RPC asking the site worker to
// read a machine's BMC boot-device override. Same shape and routing as
// PowerStatusRequestMsg (a read, no op row); see that type for the
// BMCRuntime contract.
type BootStatusRequestMsg struct {
	MachineID  uuid.UUID       `json:"machine_id"`
	BMCRuntime json.RawMessage `json:"bmc_runtime"`
}

// BootStatusResponseMsg is the worker's reply payload for a boot_status RPC.
//
// Supported is false when the machine's controller does not implement the
// boot-override capability (power.BootManager). The other fields are then
// zero and callers
// surface "not supported on this host". When Supported is true: Device is a
// power.BootDevice value ("pxe"/"disk"/"default"/"other"), Valid is whether
// an override is actually armed (false ⇒ no override, Device is inert), Raw
// is the vendor's raw device token, Transport is the controller transport.
type BootStatusResponseMsg struct {
	Supported  bool   `json:"supported"`
	Device     string `json:"device"`
	Persistent bool   `json:"persistent"`
	Valid      bool   `json:"valid"`
	Raw        string `json:"raw"`
	Transport  string `json:"transport"`
	// Hint: see PowerStatusResponseMsg.Hint. Set with Supported=false when
	// a Supermicro host would support boot ops once SMCIPMITool is installed.
	Hint string `json:"hint,omitempty"`
}

// --- Global -> Site Worker: dhcp_leases RPC ---

// DHCPLeasesRequestMsg is the global→worker RPC asking the site worker
// to snapshot its in-memory DHCP lease table. Empty — there is nothing
// to parameterize (the worker has one lease table, shared across every
// bound interface). Kept as a named struct rather than sending no
// payload at all for shape symmetry with every other RPC request type
// (PowerStatusRequestMsg, BootStatusRequestMsg), and so a future field
// addition doesn't require inventing a struct from nothing.
type DHCPLeasesRequestMsg struct{}

// DHCPLeaseInfo is one lease as reported by a site worker's dhcp_leases
// reply, enriched with the worker's own classification and machine
// identity (the dhcp package's Leases() knows only MAC/IP/expiry/pool;
// Kind/MachineID/Hostname come from the worker's cache — see
// internal/worker's runDHCPLeases).
type DHCPLeaseInfo struct {
	MAC       string    `json:"mac"`
	IP        string    `json:"ip"`
	ExpiresAt time.Time `json:"expires_at"`
	// Kind is "static" (the MAC's NetBox/reservation IP matches the
	// leased IP) or "dynamic" (allocated from the pool).
	Kind string `json:"kind"`
	// Pool is the pool subnet (CIDR) containing the leased IP, or
	// "none" if it falls outside every configured pool.
	Pool string `json:"pool"`
	// MachineID is the resolved machine UUID string, "" when the MAC
	// doesn't resolve to a known machine (legal on a promiscuous OOB
	// segment — unregistered BMCs/PDUs still get leases).
	MachineID string `json:"machine_id,omitempty"`
	// Hostname is the best-effort machine hostname for the MAC, ""
	// when unknown.
	Hostname string `json:"hostname,omitempty"`
}

// DHCPLeasesResponseMsg is one worker's reply payload, wrapped in
// RPCResponseMsg.Data inside a dhcp_leases_response envelope.
type DHCPLeasesResponseMsg struct {
	Leases []DHCPLeaseInfo `json:"leases"`
}

// OpStreamProto is the wire-protocol version of the op-event stream
// (Tier 2 Phase 2): operations left SiteSnapshot and flow on the
// op_delta / op_resync / op_removed messages. It is carried on HelloMsg
// AND on every hello reply (SnapshotMsg, NoChangeMsg, ErrorMsg). A peer
// that omits it (JSON zero value 0) or sends a lower value is a
// pre-op-stream binary; both sides reject the connection loudly rather
// than silently dropping op state. Bump on any breaking change to the
// op-stream wire shape.
const OpStreamProto = 1

// HelloMsg is the worker's first message after WS connect. Global uses
// LastSeqNum to decide whether to push a fresh snapshot or reply
// no_change. FQDN is echoed into site_workers.fqdn on the Touch call.
//
// Version is the site worker binary's build SHA (main.version,
// -ldflags-stamped at build time). The hub logs it on every hello and
// warns when it diverges from the global's own build SHA — phase 1 of
// the version-pin rollout (auto-heal lands later, behind a flag).
// Empty string means the worker is older than the version-pin work.
//
// Proto is the op-stream protocol version (OpStreamProto). Global
// refuses a hello whose Proto is absent/lower — see the skew gate in
// hub.go. A pre-op-stream worker sends 0.
// Serial is the worker host's hardware serial, read from SMBIOS
// (dmidecode) at startup and normalized (placeholder/default values
// become ""). Global joins it to the NetBox device.serial for
// hardware-attested worker->machine identity: config (worker_id/FQDN)
// can be copy-pasted, the metal's serial cannot. A non-empty Serial
// that doesn't resolve to exactly one in-scope NetBox device fails the
// hello closed (see hub.go). Empty is REFUSED under serial enforcement
// (production: a real SerialResolver wired) — there is no tolerated
// unverified state; a worker with a bad DMI read or an old binary is
// fixed, not connected around. Only the nil-resolver dev/test mode skips
// serial validation. The P4b outlet-op interlock additionally requires a
// verified serial on the claimant.
type HelloMsg struct {
	WorkerID   string `json:"worker_id"`
	FQDN       string `json:"fqdn"`
	LastSeqNum int64  `json:"last_seq_num"`
	Version    string `json:"version,omitempty"`
	Proto      int    `json:"proto,omitempty"`
	Serial     string `json:"serial,omitempty"`
	// SMCIPMITool is whether the operator-installed Supermicro SMCIPMITool
	// is usable on this worker: "installed", "missing", "missing-required",
	// or "disabled" (smcipmitool.StatusX); "" when the worker has no status
	// func wired (tests). Global holds it per connection for
	// `slagctl worker list`.
	SMCIPMITool string `json:"smcipmitool,omitempty"`
}

// ClaimOpMsg is the worker → global request to claim a pending
// operation. The hub uses the WS-authenticated worker_id of the
// sending connection — never a client-provided value — so the
// caller's identity is established by the connection itself, not
// by trusting a JSON field.
type ClaimOpMsg struct {
	OperationID string `json:"op_id"`
}

// ClaimOpResponse is the global → worker reply. Exactly one of the
// two outcomes is set:
//
//   - Operation non-nil: this worker won the claim.
//   - AlreadyClaimed true: the op is no longer pending (another
//     worker won the race, or the op doesn't exist). This is the
//     routine dual-worker outcome, NOT an error — the wrapping
//     RPCResponseMsg carries no Error so the worker can count it
//     as a lost claim rather than a transport/store failure.
//
// Hard failures (DB error, malformed request) still travel as
// RPCResponseMsg.Error with no ClaimOpResponse payload.
type ClaimOpResponse struct {
	Operation      *model.Operation `json:"operation,omitempty"`
	AlreadyClaimed bool             `json:"already_claimed,omitempty"`
}

// --- Global -> Site Worker ---

// SnapshotMsg carries a full SiteSnapshot. Pushed on content-hash change
// (dispatcher fan-out via the hub) and on hello when the worker's
// last_seq_num lags behind site_configs.last_snapshot_seq.
//
// Version is the global daemon's build SHA and PackageVersion the deb
// package version scripts/build-deb.sh built it as. The site worker
// compares Version against its own and, when SLAGWORKS_AUTO_UPGRADE=1,
// runs the packaged slagworks-upgrade helper with both to install that
// package build. Empty Version disables the comparison (dev builds);
// empty PackageVersion (a global not built by build-deb.sh) leaves the
// worker nothing to install, so it stays on its current version.
type SnapshotMsg struct {
	Snapshot       model.SiteSnapshot `json:"snapshot"`
	Version        string             `json:"version,omitempty"`
	PackageVersion string             `json:"package_version,omitempty"`
	// Proto is the op-stream protocol version (OpStreamProto). The
	// worker refuses any hello reply whose Proto is absent/lower — see
	// the skew gate in wsconn.go.
	Proto int `json:"proto,omitempty"`
}

// NoChangeMsg is the reply to hello when the worker's last_seq_num
// matches site_configs.last_snapshot_seq. Carries Version and
// PackageVersion (see SnapshotMsg) so the site worker learns the
// global's build on every reconnect even when the snapshot itself is
// unchanged.
//
// Proto carries OpStreamProto so a no_change reply can't be the hole a
// snapshot-only skew check leaves: an old worker's cached seq matching
// makes global reply no_change, and without Proto here a new worker
// would get no snapshot to inspect and no op stream, silently. The
// worker refuses a no_change whose Proto is absent/lower.
type NoChangeMsg struct {
	Version        string `json:"version,omitempty"`
	PackageVersion string `json:"package_version,omitempty"`
	Proto          int    `json:"proto,omitempty"`
}

type RPCResponseMsg struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`

	// Terminal indicates that the error is non-transient — the
	// caller (worker) should drop the pending work rather than
	// retrying. Only meaningful when Error != "". Example: the
	// observation references a machine the global has never heard
	// of, so no amount of retrying will make the write succeed.
	Terminal bool `json:"terminal,omitempty"`
}

// --- slagctl logs <-> global ---

// SubscribedMsg acknowledges a LogStreamSubscribeMsg before backfill.
type SubscribedMsg struct{}

// LogStreamSubscribeMsg is the first message slagctl sends to
// /api/v1/logs/stream. Filters narrow the live tail; cursor selects
// the resumption point.
type LogStreamSubscribeMsg struct {
	TraceID     string   `json:"trace_id,omitempty"`
	MachineID   string   `json:"machine_id,omitempty"`
	MachineName string   `json:"machine_name,omitempty"`
	OpID        string   `json:"op_id,omitempty"`
	Sources     []string `json:"sources,omitempty"`
	MinLevel    string   `json:"min_level,omitempty"`

	// SinceTimestamp bounds the stream, backfill and live tail alike,
	// to envelopes whose ts or whose time of receipt by global is at or
	// after this point, so an emitter whose clock runs behind still
	// streams. Empty applies no time bound.
	SinceTimestamp string `json:"since,omitempty"`

	// LastSeenID resumes after a given log_envelopes.id. Used on
	// reconnect: the stream delivers what matches the rest of the
	// subscription past this id.
	LastSeenID int64 `json:"last_seen_id,omitempty"`
}

// LogStreamEnvelopeMsg ships one log envelope to slagctl. The id is
// the log_envelopes.id BIGSERIAL, used by slagctl as last_seen_id on
// reconnect.
type LogStreamEnvelopeMsg struct {
	ID       int64            `json:"id"`
	Envelope slogkit.Envelope `json:"envelope"`
}

// LogStreamMetaMsg is a synthetic annotation slagctl renders inline
// in the human-readable stream. Used for drop notifications.
type LogStreamMetaMsg struct {
	Kind    string `json:"kind"`              // "drop" so far
	Site    string `json:"site,omitempty"`    // host that dropped
	Source  string `json:"source,omitempty"`  // sol|agent|site|global
	Count   int64  `json:"count,omitempty"`   // envelopes lost
	Message string `json:"message,omitempty"` // pre-formatted display text
}

type ErrorMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Proto carries OpStreamProto on the hello-rejection error path so a
	// worker whose hello was refused for a too-low Proto can still learn
	// global's proto (diagnostics) rather than inferring it. Zero on
	// non-hello errors.
	Proto int `json:"proto,omitempty"`
}

// --- Global -> Site Worker: op-event stream (Tier 2 Phase 2) ---
//
// Operations left SiteSnapshot; they flow here as per-op deltas plus a
// full-active-set resync on (re)connect. See
// docs/plans/2026-07-05-ops-out-of-snapshot.md. Every message that
// changed machines.state carries the new MachineState so the worker's
// op-driven overlay tracks the durable value the accompanying identity
// rebuild will deliver; a message that changed only op status/phase (or
// a pending-expiry that never set any state) omits it.

// OpDeltaMsg carries one operation's post-mutation row verbatim
// (model.Operation, typed Params decoded at the consumer as today) plus,
// when the transition changed the machine's state, the new machine_state.
// Sent on every op mutation: create (pending), claim, each phase
// transition. MachineState is empty ("") when the transition changed no
// machine state (phase-only, pending create). After every snapshot the
// hub pushes on a content change it re-sends each active op of the site as
// an op_delta, with the transient state a claimed lifecycle op implies as
// MachineState: the new membership can admit an op whose delta the old one
// filtered out, and the worker drops the versions it already holds.
type OpDeltaMsg struct {
	Operation    model.Operation    `json:"operation"`
	MachineState model.MachineState `json:"machine_state,omitempty"`
}

// OpResyncMsg is the full active-op set for a site (status in
// opstate.ActiveStatuses) plus the machine-state overlays each active
// lifecycle op implies. Sent unconditionally after the snapshot/no_change
// decision on every (re)connect. Bounded by concurrent-op count (dozens),
// not machine count. The worker full-replaces its op set from Ops and
// rebuilds its overlay from MachineStates, then clears the untrusted flag.
//
// MachineStates maps machine-ID string -> the transient state each
// claimed/running lifecycle op implies (opstate.ClaimTransition); pending
// ops imply nothing and contribute no entry. Global has no overlay store,
// so this is derived, not stored.
type OpResyncMsg struct {
	Ops           []model.Operation             `json:"ops"`
	MachineStates map[string]model.MachineState `json:"machine_states,omitempty"`
}

// OpRemovedMsg drops an op that left the active set (terminal), carrying
// its terminal Version so a late lower-version op_delta for the same op
// is discarded (the worker keeps a bounded tombstone opID->version). When
// the terminal transition changed machines.state
// (completion→running/commissioned, FailOperation→failed/new), it carries
// the new MachineState and the worker moves its overlay to that durable
// value; pending-expiry (which set no state) carries none.
type OpRemovedMsg struct {
	OperationID  string             `json:"op_id"`
	MachineID    string             `json:"machine_id"`
	Version      int                `json:"version"`
	MachineState model.MachineState `json:"machine_state,omitempty"`
}

// Message type constants.
const (
	// Worker -> global
	TypeHello        = "hello"
	TypeHeartbeat    = "heartbeat"
	TypeObservation  = "observation"
	TypeOpUpdate     = "op_update"
	TypeLogBatch     = "log_batch"
	TypeClaimOp      = "claim_op"
	TypeCastBatch    = "cast_batch"     // SOL asciinema pipeline
	TypeCastResume   = "cast_resume"    // resume-offset RPC
	TypeCastResumeOK = "cast_resume_ok" // RPC reply (in RPCResponseMsg.Data)

	// Global -> worker
	TypeSnapshot    = "snapshot"
	TypeNoChange    = "no_change"
	TypeOpDispatch  = "op_dispatch"
	TypeRPCResponse = "rpc_response"
	TypeClaimOpResp = "claim_op_response"

	// Global -> worker op-event stream (Tier 2 Phase 2). Operations left
	// SiteSnapshot; they flow as per-op deltas + a full resync on connect.
	TypeOpDelta   = "op_delta"
	TypeOpResync  = "op_resync"
	TypeOpRemoved = "op_removed"
	// Global -> worker power-status RPC (request), and the worker's
	// reply (response). The reverse direction of claim_op: the hub
	// originates the request_id and waits on the reply.
	TypePowerStatus     = "power_status"
	TypePowerStatusResp = "power_status_response"
	TypeBootStatus      = "boot_status"
	TypeBootStatusResp  = "boot_status_response"
	// Global -> worker dhcp-leases RPC (request), and the worker's reply
	// (response). Same reverse-of-claim_op shape as power_status, but the
	// hub fans this one to EVERY connected worker for the site (an HA
	// cage's two workers each hold their own dynamic slice's leases).
	TypeDHCPLeases     = "dhcp_leases"
	TypeDHCPLeasesResp = "dhcp_leases_response"

	// slagctl logs and operation watch (structured log stream)
	TypeSubscribed         = "subscribed"
	TypeLogStreamSubscribe = "log_stream_subscribe"
	TypeLogStreamEnvelope  = "log_stream_envelope"
	TypeLogStreamMeta      = "log_stream_meta"

	// Bidirectional
	TypeError = "error"
)
