package worker

// Model-based property harness for the site worker's op cache (cache.go,
// opstate_cache.go, snapshotview.go): the place where the boot decision is
// made.
//
// Shape:
//
//   - pcWorld holds the GLOBAL TRUTH (machines with durable/transient state,
//     operations with versions, the identity snapshot sequence) and mutates it
//     the way global does: op create, claim, re-claim, phase write, complete,
//     fail, commission commit, PXE-eligibility PATCH, membership change, identity
//     rebuild. Every mutation publishes the op-stream message global would
//     publish (op_delta / op_removed, slug-scoped the way the hub scopes them,
//     plus the re-send of active ops that follows every pushed snapshot).
//   - A delivery channel carries those messages to the Cache under test and can
//     delay, reorder (within a window), duplicate, and drop-then-resync them.
//   - Local events hit the Cache through its public surface: the executor's
//     claim step (MarkOpClaimed, the rollback, the lifecycle phases), DHCP ACK
//     phases, the boot-script fetch, agent progress reports (phase,
//     deploy_complete, failed, commission_report, phone_home),
//     outbox drain, worker restart with rehydration from LocalDB, and
//     reconnect.
//
// Oracles, checked after every step for every MAC:
//
//  1. Decision table. BootActionForMAC against an independently written
//     table from docs/dev/state-machine.md, as a function of what the Cache's
//     own getters report (effective state, PXE eligibility, active ops).
//  2. Safety against the truth and the delivery history, not against the
//     Cache's bookkeeping: BootDeploy needs a deploy op the worker was told
//     about, that it has not been told is dead, and whose install it has not
//     already seen finish; BootCommission likewise; nothing actuates between
//     a restart and the first op_resync; the executor is never handed an op
//     the worker was told is dead.
//  3. Convergence. Once the channel quiesces (and again after a final
//     identity snapshot + op_resync) the Cache's op set and effective machine
//     states equal the truth.
//
// Every violation carries an id. The few behaviors the Cache is known not to
// guarantee are listed in pcTolerated: those are counted, not failed, and
// each is classified from the history that produced it, so a violation the
// classification does not explain still fails.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ---------------------------------------------------------------------------
// Tolerated violations
// ---------------------------------------------------------------------------

// pcTolerated lists, by violation id, behavior the properties would
// otherwise reject and the Cache does not currently guarantee.
var pcTolerated = map[string]string{
	// Adversarial tier only; the hub's per-connection writer keeps op
	// messages in publish order, so production does not deliver this.
	"reorder-overlay-divergence": "the machine-state overlay is not versioned: a claim op_delta that arrives after a " +
		"higher-version op_delta of the same op is dropped together with the state it carries, and a late op_removed " +
		"of an earlier op overwrites a newer op's overlay; both heal at the next op_resync",
	// Adversarial tier only, for the same reason.
	"reorder-stale-op-bound": "a new op's op_delta that overtakes the previous op's op_removed leaves two active " +
		"lifecycle ops held for one machine; the boot decision and the op the boot script binds to are each picked " +
		"by map order, so an actuation one op justifies can be bound to the other, stale one",
	"rewipe-latch-lost-on-resync-omission": "an op_resync that omits a latched deploy op (the hub scopes it by its " +
		"cached snapshot, and the machine was briefly absent from it) drops the latch and the phone-home pin; when " +
		"the op comes back with global's pre-latch phase BootDeploy re-arms",
	"failed-op-forgotten-on-restart": "the agent's failure report is kept in memory only (the failed mark and the " +
		"failed op_update); a worker restart before global hears of it rehydrates the op as active and it actuates again",
	"running-overlay-after-pending-op-failed": "a phone-home sets a running overlay that clears when the identity " +
		"baseline becomes running; when global fails the deploy op while it is still pending, op_removed carries no " +
		"machine state and the overlay stays until the next op_resync",
}

// ---------------------------------------------------------------------------
// Coverage counters (aggregated across cases, printed by the test)
// ---------------------------------------------------------------------------

type pcStats struct {
	mu sync.Mutex
	n  map[string]int
}

var pcStat = &pcStats{n: map[string]int{}}

// reset clears the counters, so each test's coverage log covers that test.
func (s *pcStats) reset() {
	s.mu.Lock()
	s.n = map[string]int{}
	s.mu.Unlock()
}

func (s *pcStats) add(k string) {
	s.mu.Lock()
	s.n[k]++
	s.mu.Unlock()
}

func (s *pcStats) max(k string, v int) {
	s.mu.Lock()
	if v > s.n[k] {
		s.n[k] = v
	}
	s.mu.Unlock()
}

func (s *pcStats) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.n))
	for k := range s.n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%-44s %d\n", k, s.n[k])
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Truth
// ---------------------------------------------------------------------------

const (
	pcSelfSerial = "SELF-SERIAL"
	pcUnknownMAC = "52:54:00:ff:ff:ff"
	pcDupMAC     = "52:54:00:dd:dd:dd"
)

type pcMachine struct {
	idx      int
	id       uuid.UUID
	mac      string
	ipmiMAC  string
	state    model.MachineState
	pxe      bool
	inSite   bool
	commData bool
}

type pcOp struct {
	idx     int
	id      uuid.UUID
	mi      int
	typ     model.OperationType
	status  model.OperationStatus
	phase   string
	version int
	params  json.RawMessage
	byUs    bool // claimed by the worker under test (re-claim eligibility)
	// failedPending: global failed the op while it was still pending, so its
	// op_removed carried no machine state.
	failedPending bool
	selfOut       bool // an outlet op the worker-side self-power interlock must hide
}

func (o *pcOp) active() bool {
	return o.status == model.OpPending || o.status == model.OpClaimed || o.status == model.OpRunning
}

func pcLifecycle(t model.OperationType) bool {
	return t == model.OpDeploy || t == model.OpCommission
}

// pcTransient is the transient machine state a claim of this op type writes
// (docs/dev/state-machine.md, "claimTransition allowed sources").
func pcTransient(t model.OperationType) model.MachineState {
	switch t {
	case model.OpDeploy:
		return model.MachineStateDeploying
	case model.OpCommission:
		return model.MachineStateCommissioning
	default:
		return ""
	}
}

func pcClaimAllowed(t model.OperationType, s model.MachineState) bool {
	switch t {
	case model.OpDeploy:
		return s == model.MachineStateCommissioned || s == model.MachineStateFailed || s == model.MachineStateRunning
	case model.OpCommission:
		return s == model.MachineStateNew || s == model.MachineStateCommissioned ||
			s == model.MachineStateFailed || s == model.MachineStateRunning
	default:
		return true
	}
}

func pcMachineID(i int) uuid.UUID {
	return uuid.UUID{0xaa, 0xaa, 0xaa, 0xaa, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, 0, byte(i + 1)}
}

func pcOpID(i int) uuid.UUID {
	return uuid.UUID{0xbb, 0xbb, 0xbb, 0xbb, 0, 0, 0x40, 0, 0x80, 0, 0, 0, 0, 0, byte(i >> 8), byte(i + 1)}
}

// ---------------------------------------------------------------------------
// Channel
// ---------------------------------------------------------------------------

type pcMsgKind int

const (
	pcSnap pcMsgKind = iota
	pcDelta
	pcResync
	pcRemoved
)

func (k pcMsgKind) String() string {
	return [...]string{"snapshot", "op_delta", "op_resync", "op_removed"}[k]
}

type pcMsg struct {
	kind   pcMsgKind
	snap   *model.SiteSnapshot
	op     model.Operation
	st     model.MachineState
	ops    []model.Operation
	states map[string]model.MachineState
	opID   uuid.UUID
	mID    uuid.UUID
	ver    int
	// dead lists the ops that were terminal when a resync was built: once the
	// resync is delivered the worker has been told they are gone.
	dead []uuid.UUID
	// at is the truth clock when the message was built: a later-built message
	// carries newer truth.
	at int
	// snapSeq is the newest snapshot SeqNum that existed when an op message
	// was published; hello marks the snapshot written as the hello reply.
	snapSeq int64
	hello   bool
}

func (m pcMsg) String() string {
	switch m.kind {
	case pcSnap:
		parts := make([]string, 0, len(m.snap.Machines))
		for _, ms := range m.snap.Machines {
			parts = append(parts, fmt.Sprintf("%s=%s/pxe=%v", pcShort(ms.ID), ms.State, ms.PXEEligible))
		}
		return fmt.Sprintf("snapshot{seq=%d %s}", m.snap.SeqNum, strings.Join(parts, " "))
	case pcDelta:
		return fmt.Sprintf("op_delta{%s m=%s %s %s phase=%q v=%d state=%q}",
			pcShort(m.op.ID), pcShort(m.op.MachineID), m.op.Type, m.op.Status, m.op.Phase, m.op.Version, m.st)
	case pcResync:
		parts := make([]string, 0, len(m.ops))
		for _, o := range m.ops {
			parts = append(parts, fmt.Sprintf("%s:%s/%s/%q/v%d", pcShort(o.ID), o.Type, o.Status, o.Phase, o.Version))
		}
		st := make([]string, 0, len(m.states))
		for k, v := range m.states {
			id, _ := uuid.Parse(k)
			st = append(st, fmt.Sprintf("%s=%s", pcShort(id), v))
		}
		sort.Strings(st)
		return fmt.Sprintf("op_resync{ops=[%s] states=[%s]}", strings.Join(parts, " "), strings.Join(st, " "))
	default:
		return fmt.Sprintf("op_removed{%s m=%s v=%d state=%q}", pcShort(m.opID), pcShort(m.mID), m.ver, m.st)
	}
}

func pcShort(id uuid.UUID) string {
	if id[0] == 0xaa {
		return fmt.Sprintf("M%d", int(id[15])-1)
	}
	if id[0] == 0xbb {
		return fmt.Sprintf("op%d", int(id[14])<<8|int(id[15])-1)
	}
	return id.String()[:8]
}

// ---------------------------------------------------------------------------
// World
// ---------------------------------------------------------------------------

type pcConfig struct {
	// tier 0: one ordered lossless connection.
	// tier 1: realistic faults. Op messages keep their publish order, a
	//         pushed snapshot can carry content older than its place in the
	//         stream, connections drop (then hello + op_resync), the worker
	//         restarts.
	// tier 2: adversarial, beyond what the hub delivers. Any message may
	//         overtake any other within the reorder window (never across an
	//         op_resync), and messages are duplicated.
	tier       int
	useDB      bool // LocalDB-backed Cache: restarts are possible
	dupMAC     bool // two machines share a physical MAC
	reorderWin int
	// swarm switches: families of events enabled for this case
	swPatch, swMember, swOther, swLatePhase, swFailReport, swPower bool
	swAnyCreate                                                    bool // skip the API's create-time state gate
}

type pcWorld struct {
	t   *rapid.T
	cfg pcConfig

	machines  []*pcMachine
	ops       []*pcOp
	seq       int64
	hubSnap   *model.SiteSnapshot // the hub's cached per-slug snapshot (nil = cold)
	hubKey    string
	hubSnapAt int
	clock     int

	connected bool
	g2w       []pcMsg
	w2gUpd    []model.OperationUpdate
	w2gObs    []PendingOutbox

	dbPath string
	db     *LocalDB
	c      *Cache

	// delivery history / worker knowledge
	awaitingResync bool // restarted, no op_resync delivered yet
	told           map[uuid.UUID]bool
	knownDead      map[uuid.UUID]bool
	installed      map[uuid.UUID]bool // deploy_complete accepted, or phone-home recorded, for this op
	failReported   map[uuid.UUID]bool
	// driven: the executor won the claim for this op; the value is the op's
	// version right after that claim.
	driven map[uuid.UUID]int
	// why a latched op might have lost its latch (classifies a re-wipe)
	phaseAfterInstall   map[uuid.UUID]string // a later non-boot-prefix phase report was accepted
	restartAfterInstall map[uuid.UUID]bool   // the worker restarted after the install finished
	execAfterInstall    map[uuid.UUID]bool   // the executor drove the (still pending) op after the install finished
	failBeforeInstall   map[uuid.UUID]bool   // the agent had reported the op failed before a later run installed it
	restartAfterFail    map[uuid.UUID]bool   // the worker restarted after the agent reported the op failed
	pendingAtInstall    map[uuid.UUID]bool   // the Cache still held the op as pending when its install finished
	// a pending-status op_delta/op_resync row for the op was applied after its install finished
	pendingEchoAfterInstall map[uuid.UUID]bool
	// an op_resync that omitted the (still active) op was applied after its
	// install finished: the hub scopes a resync by its cached snapshot, so a
	// machine briefly missing from that snapshot takes its ops with it
	omittedAfterInstall map[uuid.UUID]bool
	// why an effective machine state might not converge (classifies it)
	phoneHomed map[uuid.UUID]bool // the Cache accepted a phone-home for this deploy op
	// Recomputation oracle for the effective machine state: the newest state
	// the worker has been told. expBase is the baseline of the snapshot the
	// Cache must currently hold; expOv is the newest op-stream state told for
	// a machine that no newer snapshot has superseded.
	expSeq      int64
	expFromDisk bool
	expBase     map[int]model.MachineState
	expBaseAt   int // truth clock of the applied snapshot
	expOv       map[int]pcToldState
	expSkip     map[int]bool // a local phone-home moved the machine; the stream no longer defines it
	// expVer is last-writer-wins stated independently: for every op the
	// worker has been told about and not been told is gone, the highest row
	// version delivered since the last op_resync.
	expVer map[uuid.UUID]int
	clean  bool // no reorder/dup/drop/restart/scoped-out delta so far
	// disordered: a message was delivered out of order or twice since the
	// last op_resync.
	disordered bool
	lossless   bool // messages may have been reordered or duplicated, but none was lost

	trace []string
}

// tick advances and returns the truth clock.
func (w *pcWorld) tick() int {
	w.clock++
	return w.clock
}

func (w *pcWorld) logf(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf(format, args...))
}

func (w *pcWorld) violate(id, format string, args ...any) {
	pcStat.add("violation/" + id)
	if _, ok := pcTolerated[id]; ok {
		return
	}
	msg := fmt.Sprintf(format, args...)
	w.t.Fatalf("[%s] %s\n--- trace (%d steps) ---\n%s", id, msg, len(w.trace), strings.Join(w.trace, "\n"))
}

func (w *pcWorld) opByID(id uuid.UUID) *pcOp {
	for _, o := range w.ops {
		if o.id == id {
			return o
		}
	}
	return nil
}

func (w *pcWorld) activeOpOn(mi int) *pcOp {
	for _, o := range w.ops {
		if o.mi == mi && o.active() {
			return o
		}
	}
	return nil
}

func (o *pcOp) row() model.Operation {
	return model.Operation{
		ID: o.id, MachineID: pcMachineID(o.mi), Type: o.typ, Status: o.status,
		Phase: o.phase, Version: o.version, Params: o.params,
	}
}

func (w *pcWorld) hubMember(mi int) bool {
	if w.hubSnap == nil {
		return false
	}
	id := w.machines[mi].id
	for i := range w.hubSnap.Machines {
		if w.hubSnap.Machines[i].ID == id {
			return true
		}
	}
	return false
}

// --- publishers (what the hub puts on the wire for one worker connection) ---

func (w *pcWorld) pubDelta(o *pcOp, st model.MachineState) {
	if !w.connected {
		return // no connection: the next hello's op_resync covers it
	}
	if !w.hubMember(o.mi) {
		// Slug-scoped: the hub only sends an op_delta to workers whose cached
		// snapshot contains the machine.
		pcStat.add("pub/delta_scoped_out")
		w.clean, w.lossless = false, false
		return
	}
	w.g2w = append(w.g2w, pcMsg{kind: pcDelta, op: o.row(), st: st, at: w.tick(), snapSeq: w.seq})
}

func (w *pcWorld) pubRemoved(o *pcOp, st model.MachineState) {
	if !w.connected {
		return
	}
	w.g2w = append(w.g2w, pcMsg{kind: pcRemoved, opID: o.id, mID: pcMachineID(o.mi), ver: o.version, st: st, at: w.tick(), snapSeq: w.seq})
}

func (w *pcWorld) buildSnapshot() (*model.SiteSnapshot, string) {
	snap := &model.SiteSnapshot{}
	var key strings.Builder
	for _, m := range w.machines {
		if !m.inSite {
			continue
		}
		ifaces := []model.SnapshotInterface{
			{MAC: m.mac, Type: "physical"},
			{MAC: m.ipmiMAC, Type: "ipmi"},
		}
		if w.cfg.dupMAC && (m.idx == 0 || m.idx == 1) {
			ifaces = append(ifaces, model.SnapshotInterface{MAC: pcDupMAC, Type: "physical"})
		}
		snap.Machines = append(snap.Machines, model.MachineSnapshot{
			ID: m.id, State: m.state, PXEEligible: m.pxe,
			NetBoxSerial: fmt.Sprintf("SER-%d", m.idx), Interfaces: ifaces,
		})
		fmt.Fprintf(&key, "%d:%s:%v;", m.idx, m.state, m.pxe)
	}
	return snap, key.String()
}

// rebuild is the identity rebuild: it produces (and pushes) a new snapshot
// only when the content changed, like the dispatcher's content-hash gate.
func (w *pcWorld) rebuild() bool {
	snap, key := w.buildSnapshot()
	if w.hubSnap != nil && key == w.hubKey {
		return false
	}
	w.seq++
	snap.SeqNum = w.seq
	w.hubSnap, w.hubKey, w.hubSnapAt = snap, key, w.tick()
	if w.connected {
		w.g2w = append(w.g2w, pcMsg{kind: pcSnap, snap: snap, at: w.hubSnapAt})
		// The hub follows every pushed snapshot with the site's active ops,
		// re-sent as op_deltas scoped to the new membership, each carrying
		// the transient state a claimed op implies. This is what delivers an
		// op whose earlier deltas were scoped out.
		for _, o := range w.ops {
			if !o.active() || !w.hubMember(o.mi) {
				continue
			}
			var st model.MachineState
			if o.status == model.OpClaimed || o.status == model.OpRunning {
				st = pcTransient(o.typ)
			}
			pcStat.add("pub/delta_resent_after_push")
			w.g2w = append(w.g2w, pcMsg{kind: pcDelta, op: o.row(), st: st, at: w.tick(), snapSeq: w.seq})
		}
	}
	pcStat.add("truth/rebuild")
	return true
}

func (w *pcWorld) buildResync() pcMsg {
	m := pcMsg{kind: pcResync, states: map[string]model.MachineState{}, at: w.tick()}
	for _, o := range w.ops {
		if !o.active() {
			m.dead = append(m.dead, o.id)
			continue
		}
		if !w.hubMember(o.mi) {
			// The resync is scoped by the hub's cached snapshot. An active op
			// whose machine is not in it is omitted; the re-send after the
			// push that adds the machine brings it back (see rebuild).
			pcStat.add("pub/resync_scoped_out")
			w.clean, w.lossless = false, false
			continue
		}
		m.ops = append(m.ops, o.row())
		if o.status == model.OpClaimed || o.status == model.OpRunning {
			if tr := pcTransient(o.typ); tr != "" {
				m.states[pcMachineID(o.mi).String()] = tr
			}
		}
	}
	return m
}

// --- global mutations ---

func (w *pcWorld) gCreate(mi int, typ model.OperationType, selfOutlet, allowSelf bool) *pcOp {
	o := &pcOp{idx: len(w.ops), id: pcOpID(len(w.ops)), mi: mi, typ: typ, status: model.OpPending}
	if selfOutlet {
		p := map[string]any{"pdu_runtime": map[string]any{
			"outlets": []any{}, "outlet": true, "downstream_serial": pcSelfSerial, "allow_self": allowSelf,
		}}
		o.params, _ = json.Marshal(p)
		// The interlock serves a self-feed only for an explicit AllowSelf
		// cycle/on; everything else that feeds this worker is hidden.
		served := allowSelf && (typ == model.OpPowerCycle || typ == model.OpPowerOn)
		o.selfOut = !served
	}
	w.ops = append(w.ops, o)
	w.logf("G create %s %s on M%d selfOutlet=%v allowSelf=%v", pcShort(o.id), typ, mi, selfOutlet, allowSelf)
	pcStat.add("truth/create/" + string(typ))
	w.pubDelta(o, "")
	return o
}

type pcToldState struct {
	at int
	st model.MachineState
}

type pcClaimResult int

const (
	pcClaimWon pcClaimResult = iota
	pcClaimLost
	pcClaimErr
)

// gClaim is ClaimOperation: pending -> claimed, transient machine state for a
// lifecycle op, idempotent re-claim for the same worker before any phase.
func (w *pcWorld) gClaim(o *pcOp, us bool) pcClaimResult {
	m := w.machines[o.mi]
	if o.status == model.OpClaimed && o.byUs && us && o.phase == "" {
		o.version++
		w.logf("G re-claim %s v%d", pcShort(o.id), o.version)
		pcStat.add("truth/reclaim")
		w.pubDelta(o, pcTransient(o.typ))
		return pcClaimWon
	}
	if o.status != model.OpPending {
		return pcClaimLost
	}
	if pcLifecycle(o.typ) && !pcClaimAllowed(o.typ, m.state) {
		pcStat.add("truth/claim_precondition_error")
		return pcClaimErr
	}
	o.status = model.OpClaimed
	o.byUs = us
	o.version++
	if tr := pcTransient(o.typ); tr != "" {
		m.state = tr
	}
	w.logf("G claim %s (us=%v) v%d -> M%d %s", pcShort(o.id), us, o.version, o.mi, m.state)
	pcStat.add("truth/claim")
	w.pubDelta(o, pcTransient(o.typ))
	return pcClaimWon
}

// gPhase is SetOperationPhase as called from the op_update handler. A
// terminal op is left as it is: no version bump, nothing published.
func (w *pcWorld) gPhase(o *pcOp, phase string) {
	if o.phase == phase {
		return
	}
	if !o.active() {
		pcStat.add("truth/phase_after_terminal_ignored")
		return
	}
	o.phase = phase
	o.version++
	w.logf("G phase %s %q v%d (status %s)", pcShort(o.id), phase, o.version, o.status)
	pcStat.add("truth/phase")
	w.pubDelta(o, "")
}

func (w *pcWorld) gComplete(o *pcOp, target model.MachineState) {
	if !o.active() {
		return
	}
	o.status = model.OpCompleted
	o.version++
	if target != "" {
		w.machines[o.mi].state = target
		if target == model.MachineStateCommissioned {
			w.machines[o.mi].commData = true
		}
	}
	w.logf("G complete %s v%d -> M%d %q", pcShort(o.id), o.version, o.mi, target)
	pcStat.add("truth/complete/" + string(o.typ))
	w.pubRemoved(o, target)
}

// gFail is FailOperation with the four documented rules.
func (w *pcWorld) gFail(o *pcOp, why string) {
	if !o.active() {
		return
	}
	wasPending := o.status == model.OpPending
	o.failedPending = wasPending
	o.status = model.OpFailed
	o.version++
	var target model.MachineState
	switch {
	case wasPending:
	case !pcLifecycle(o.typ):
	case o.typ == model.OpCommission && !w.machines[o.mi].commData:
		target = model.MachineStateNew
	default:
		target = model.MachineStateFailed
	}
	if target != "" {
		w.machines[o.mi].state = target
	}
	w.logf("G fail %s (%s) v%d -> M%d %q", pcShort(o.id), why, o.version, o.mi, target)
	pcStat.add("truth/fail/" + string(o.typ))
	w.pubRemoved(o, target)
}

func pcCompletionState(t model.OperationType) model.MachineState {
	switch t {
	case model.OpDeploy:
		return model.MachineStateRunning
	case model.OpCommission:
		return model.MachineStateCommissioned
	default:
		return ""
	}
}

// gHandleUpdate is the hub's op_update handler: phase first, then status.
func (w *pcWorld) gHandleUpdate(u model.OperationUpdate) {
	id, err := uuid.Parse(u.OperationID)
	if err != nil {
		return
	}
	o := w.opByID(id)
	if o == nil {
		return
	}
	if u.Phase != "" {
		w.gPhase(o, u.Phase)
	}
	switch u.Status {
	case string(model.OpCompleted):
		w.gComplete(o, pcCompletionState(o.typ))
	case string(model.OpFailed):
		w.gFail(o, "worker op_update")
	}
}

func (w *pcWorld) gHandleObs(p PendingOutbox) {
	switch p.Obs.Event {
	case "commission_report":
		o := w.opByID(p.Obs.OperationID)
		if o != nil && o.active() && o.typ == model.OpCommission && pcMachineID(o.mi) == p.Obs.MachineID {
			w.gComplete(o, model.MachineStateCommissioned)
		}
	case "bmc_reconcile_report":
		o := w.opByID(p.Obs.OperationID)
		if o != nil && o.active() && o.typ == model.OpBMCReconcile {
			w.gComplete(o, "")
		}
	case "phone_home":
		// The durable carrier of a deploy's completion: it names the deploy
		// op, and whichever of it and the completed op_update arrives first
		// completes the op.
		o := w.opByID(p.Obs.OperationID)
		if o != nil && o.active() && o.typ == model.OpDeploy && pcMachineID(o.mi) == p.Obs.MachineID {
			w.gComplete(o, model.MachineStateRunning)
		}
	}
	w.c.AckObservation(p.RowID)
}

// --- connection lifecycle ---

func (w *pcWorld) disconnect(why string) {
	if !w.connected {
		return
	}
	if len(w.g2w) > 0 || len(w.w2gUpd) > 0 {
		w.clean, w.lossless = false, false
	}
	pcStat.add("chan/disconnect")
	w.noteLostUpdates(w.w2gUpd)
	if !w.cfg.useDB && len(w.w2gObs) > 0 {
		// Observations are durable: a real worker reloads the unacked ones
		// from its SQLite outbox on reconnect. The in-memory Cache has no
		// disk to reload from, so hand them back here.
		w.c.RequeueOutbox(w.w2gObs, nil)
	}
	w.connected = false
	w.g2w, w.w2gUpd, w.w2gObs = nil, nil, nil
	w.logf("X disconnect (%s)", why)
}

// connect is the hello: snapshot or no_change, then an unconditional
// op_resync. The resync is a barrier for op messages (the hub gates them on
// opReady and writes them strictly after it).
func (w *pcWorld) connect() {
	if w.connected {
		return
	}
	w.connected = true
	if w.hubSnap == nil {
		snap, key := w.buildSnapshot()
		w.seq++
		snap.SeqNum = w.seq
		w.hubSnap, w.hubKey, w.hubSnapAt = snap, key, w.tick()
	}
	hello := w.c.HelloSeqNum()
	if w.hubSnap.SeqNum != hello || hello == 0 {
		w.g2w = append(w.g2w, pcMsg{kind: pcSnap, snap: w.hubSnap, at: w.hubSnapAt, hello: true})
	}
	w.g2w = append(w.g2w, w.buildResync())
	w.c.ReloadOutboxFromDisk()
	pcStat.add("chan/connect")
	w.logf("X connect hello_seq=%d hub_seq=%d", hello, w.hubSnap.SeqNum)
}

// noteLostUpdates records op updates that will never reach global: the outbox
// keeps them in memory only, so a send into a dying connection or a worker
// restart loses them. (A deploy's completion also travels as the durable
// phone_home observation, so losing its op_update loses nothing.)
func (w *pcWorld) noteLostUpdates(upd []model.OperationUpdate) {
	for _, u := range upd {
		pcStat.add("chan/lost_op_update")
		id, err := uuid.Parse(u.OperationID)
		if err != nil {
			continue
		}
		o := w.opByID(id)
		if o == nil {
			continue
		}
		w.logf("X op_update lost: %s status=%q phase=%q", pcShort(id), u.Status, u.Phase)
		if u.Status == string(model.OpCompleted) && o.active() {
			pcStat.add("chan/lost_completion")
		}
	}
}

func (w *pcWorld) restart() {
	w.disconnect("restart")
	// Whatever is still queued in the outbox dies with the process.
	_, queued := w.c.DrainOutbox()
	w.noteLostUpdates(queued)
	pcCloseDB(w.db)
	db, err := OpenLocalDB(w.dbPath)
	if err != nil {
		w.t.Fatalf("reopen local db: %v", err)
	}
	w.db = db
	w.c = NewCacheWithSerial(db, pcSelfSerial)
	w.awaitingResync = true
	w.expFromDisk = w.expSeq != 0
	w.clean, w.lossless = false, false
	for id := range w.installed {
		w.restartAfterInstall[id] = true
	}
	for id := range w.failReported {
		w.restartAfterFail[id] = true
	}
	pcStat.add("worker/restart")
	w.logf("W restart (rehydrate from LocalDB)")
}

func pcCloseDB(db *LocalDB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// deliver applies g2w[idx] to the Cache. dup leaves the message queued.
func (w *pcWorld) deliver(idx int, dup bool) {
	m := w.g2w[idx]
	if !dup {
		w.g2w = append(w.g2w[:idx:idx], w.g2w[idx+1:]...)
	}
	if idx != 0 || dup {
		w.clean = false
		w.disordered = true
	}
	w.logf("D deliver[%d dup=%v] %s", idx, dup, m)
	pcStat.add("deliver/" + m.kind.String())
	switch m.kind {
	case pcSnap:
		cp := *m.snap
		cp.Machines = append([]model.MachineSnapshot(nil), m.snap.Machines...)
		w.c.ApplySnapshot(&cp)
		// A snapshot applies when it is newer than the view (or equal to a
		// view that only came from disk).
		if m.snap.SeqNum > w.expSeq || (m.snap.SeqNum == w.expSeq && w.expFromDisk) {
			w.expSeq, w.expFromDisk = m.snap.SeqNum, false
			w.expBaseAt = m.at
			w.expBase = map[int]model.MachineState{}
			for _, ms := range m.snap.Machines {
				mi := int(ms.ID[15]) - 1
				w.expBase[mi] = ms.State
				if ov, ok := w.expOv[mi]; ok && m.at >= ov.at {
					delete(w.expOv, mi) // the baseline is newer than the overlay
				}
			}
		}
	case pcDelta:
		w.c.ApplyOpDelta(m.op, m.st)
		w.told[m.op.ID] = true
		if !w.knownDead[m.op.ID] {
			if v, ok := w.expVer[m.op.ID]; !ok || m.op.Version > v {
				w.expVer[m.op.ID] = m.op.Version
			}
		}
		if m.op.Status == model.OpPending && w.installed[m.op.ID] {
			w.pendingEchoAfterInstall[m.op.ID] = true
		}
		if m.st != "" {
			w.tellState(int(m.op.MachineID[15])-1, m.at, m.st)
		}
	case pcResync:
		states := make(map[string]model.MachineState, len(m.states))
		for k, v := range m.states {
			states[k] = v
		}
		w.c.ApplyOpResync(append([]model.Operation(nil), m.ops...), states)
		w.disordered = false // a resync rebuilds the op set and every overlay
		for _, o := range m.ops {
			w.told[o.ID] = true
			if o.Status == model.OpPending && w.installed[o.ID] {
				w.pendingEchoAfterInstall[o.ID] = true
			}
		}
		for _, id := range m.dead {
			w.knownDead[id] = true
		}
		w.awaitingResync = false
		carried := map[uuid.UUID]bool{}
		for _, o := range m.ops {
			carried[o.ID] = true
		}
		for id := range w.installed {
			if !carried[id] && !w.knownDead[id] {
				w.omittedAfterInstall[id] = true
			}
		}
		// A resync replaces the op set: rows it carries (unless the worker
		// already saw them removed), nothing else.
		w.expVer = map[uuid.UUID]int{}
		for _, o := range m.ops {
			if !w.knownDead[o.ID] {
				w.expVer[o.ID] = o.Version
			}
		}
		// A resync rebuilds every overlay from scratch.
		w.expOv = map[int]pcToldState{}
		for k, st := range m.states {
			if id, err := uuid.Parse(k); err == nil {
				w.tellState(int(id[15])-1, m.at, st)
			}
		}
	case pcRemoved:
		w.c.ApplyOpRemoved(m.opID.String(), m.mID.String(), m.ver, m.st)
		w.knownDead[m.opID] = true
		delete(w.expVer, m.opID)
		if m.st != "" {
			w.tellState(int(m.mID[15])-1, m.at, m.st)
		}
	}
}

// tellState records an op-stream machine state; the newest one told wins.
func (w *pcWorld) tellState(mi, at int, st model.MachineState) {
	if ov, ok := w.expOv[mi]; ok && ov.at > at {
		return
	}
	if _, inView := w.expBase[mi]; inView && at < w.expBaseAt {
		return // the applied baseline is already newer than this message
	}
	w.expOv[mi] = pcToldState{at, st}
}

// checkEffectiveState: every machine's effective state must be the newest
// state the worker has been told (overlay if no newer baseline, else the
// baseline of the applied snapshot).
func (w *pcWorld) checkEffectiveState(where string) {
	if !w.connected {
		// Messages may have died with the connection; the worker is stale by
		// construction until the next hello's op_resync.
		return
	}
	for _, m := range w.machines {
		got := w.c.MachineState(m.id)
		base, inView := w.expBase[m.idx]
		if !inView {
			if got != "" {
				w.violate("view-membership", "%s: M%d is not in the newest applied snapshot but MachineState=%q", where, m.idx, got)
			}
			continue
		}
		if w.expSkip[m.idx] || w.stateInFlight(m.idx) {
			// A state-bearing op message for this machine is still on the
			// wire. The Cache cannot order it against the snapshots it has
			// (no timestamps), so the window before it lands is a designed
			// transient, not a divergence.
			continue
		}
		want := base
		if ov, ok := w.expOv[m.idx]; ok {
			want = ov.st
		}
		if got == string(want) {
			continue
		}
		id := "effective-state-not-newest-told"
		if w.cfg.tier == 2 && w.disordered {
			id = "reorder-overlay-divergence"
		}
		w.violate(id, "%s: M%d effective state %q, newest told %q (baseline %q, overlay %+v)", where, m.idx, got, want, base, w.expOv[m.idx])
	}
}

// stateInFlight reports whether an undelivered op message carries a machine
// state for mi (or is a resync, which rebuilds every overlay).
func (w *pcWorld) stateInFlight(mi int) bool {
	id := w.machines[mi].id
	for _, m := range w.g2w {
		switch m.kind {
		case pcResync:
			return true
		case pcDelta:
			if m.st != "" && m.op.MachineID == id {
				return true
			}
		case pcRemoved:
			if m.st != "" && m.mID == id {
				return true
			}
		case pcSnap:
		}
	}
	return false
}

// deliverable returns the indexes the channel may deliver next.
//
// Tier 0 is strictly in order. Tier 1 allows the one reorder a connection
// can show: an op message overtakes a pushed snapshot that was still in the
// build pipeline when the op message was published. A rebuild reads machine
// state and pushes seconds later, so a snapshot's content is older than its
// place in the stream. Rebuilds for a slug are serialized, so only the NEWEST
// snapshot at publish time can be overtaken; the hello reply is never
// overtaken. Op messages keep their publish order (the hub writes them from
// one queue per connection).
//
// Nothing crosses an op_resync (hub opReady gate). Tier 2 lets any message
// overtake any other within the window, still never across an op_resync.
func (w *pcWorld) deliverable() []int {
	if len(w.g2w) == 0 {
		return nil
	}
	out := []int{0}
	if w.cfg.tier == 0 {
		return out
	}
	for i := 1; i < len(w.g2w) && i <= w.cfg.reorderWin; i++ {
		m := w.g2w[i]
		if m.kind == pcResync {
			continue
		}
		ok := true
		for j := 0; j < i && ok; j++ {
			prev := w.g2w[j]
			switch {
			case prev.kind == pcResync:
				ok = false
			case w.cfg.tier == 2:
			case m.kind == pcSnap:
				ok = false // a snapshot does not overtake earlier writes
			case prev.kind == pcSnap:
				ok = !prev.hello && prev.snap.SeqNum >= m.snapSeq
			default:
				ok = false // op messages keep their publish order
			}
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// --- worker-local events ---

// drainOutbox moves the Cache's outbox onto the wire toward global.
func (w *pcWorld) drainOutbox() {
	obs, upd := w.c.DrainOutbox()
	w.w2gObs = append(w.w2gObs, obs...)
	w.w2gUpd = append(w.w2gUpd, upd...)
	if len(obs)+len(upd) > 0 {
		w.logf("W drain outbox: %d obs, %d op updates", len(obs), len(upd))
	}
}

func (w *pcWorld) processUpdates(n int) {
	for n > 0 && len(w.w2gUpd) > 0 {
		u := w.w2gUpd[0]
		w.w2gUpd = w.w2gUpd[1:]
		w.logf("G recv op_update %s status=%q phase=%q", u.OperationID[:8], u.Status, u.Phase)
		w.gHandleUpdate(u)
		n--
	}
}

func (w *pcWorld) processObs(idx int) {
	p := w.w2gObs[idx]
	w.w2gObs = append(w.w2gObs[:idx:idx], w.w2gObs[idx+1:]...)
	w.logf("G recv observation %s op=%s", p.Obs.Event, pcShort(p.Obs.OperationID))
	w.gHandleObs(p)
}

func (w *pcWorld) report(o *pcOp, mi int, r model.ProgressReport) error {
	r.OperationID = o.id.String()
	r.MachineID = pcMachineID(mi).String()
	err := w.c.HandleProgressReport(&r)
	w.logf("W report op=%s m=M%d phase=%q event=%q failed=%v -> err=%v", pcShort(o.id), mi, r.Phase, r.Event, r.Failed, err)
	return err
}

// workerActive returns the active lifecycle ops the Cache currently holds for
// a machine, found by probing GetOperation for every op that ever existed.
func (w *pcWorld) workerActive(mid uuid.UUID) []*model.Operation {
	var out []*model.Operation
	for _, o := range w.ops {
		g := w.c.GetOperation(o.id.String())
		if g == nil || g.MachineID != mid {
			continue
		}
		if g.Status == model.OpCompleted || g.Status == model.OpFailed {
			continue
		}
		if !pcLifecycle(g.Type) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// ---------------------------------------------------------------------------
// Oracle 1: the documented decision table, over the Cache's own getters
// ---------------------------------------------------------------------------

// pcTable is docs/dev/state-machine.md "Boot-action decision table" plus the
// fail-closed and untrusted rules from "Op transport". op is the active
// lifecycle op for the machine (nil = none).
func pcTable(state model.MachineState, pxe bool, op *model.Operation, trusted bool) model.BootAction {
	if !pxe {
		return model.BootLocal
	}
	isDeploy := op != nil && op.Type == model.OpDeploy
	isCommission := op != nil && op.Type == model.OpCommission
	if isDeploy && op.Phase == "awaiting_phone_home" {
		// The install is done: BootLocal in every machine state.
		return model.BootLocal
	}
	want := model.BootLocal
	switch state {
	case model.MachineStateCommissioning:
		if isCommission {
			want = model.BootCommission
		}
	case model.MachineStateDeploying:
		if isDeploy {
			want = model.BootDeploy
		}
	case model.MachineStateNew:
		if isCommission {
			want = model.BootCommission
		}
	case model.MachineStateFailed:
		if isCommission {
			want = model.BootCommission
		}
		if isDeploy {
			want = model.BootDeploy
		}
	case model.MachineStateCommissioned:
		if isDeploy {
			want = model.BootDeploy
		}
	case model.MachineStateRunning:
	}
	if !trusted {
		// A rehydrated op set may force BootLocal but never actuate.
		return model.BootLocal
	}
	return want
}

func pcActionName(a model.BootAction) string {
	switch a {
	case model.BootDeploy:
		return "BootDeploy"
	case model.BootCommission:
		return "BootCommission"
	default:
		return "BootLocal"
	}
}

type pcMAC struct {
	mac string
	mi  int // machine index the MAC physically belongs to; -1 = must be unknown
}

func (w *pcWorld) macs() []pcMAC {
	out := []pcMAC{{pcUnknownMAC, -1}}
	for _, m := range w.machines {
		out = append(out, pcMAC{m.mac, m.idx}, pcMAC{m.ipmiMAC, -1})
	}
	if w.cfg.dupMAC {
		// The shared MAC is modeled on M0 and M1. While both are in the
		// applied view it identifies neither; with only one of them in the
		// view it is that machine's MAC.
		in0 := w.c.MachineByID(w.machines[0].id) != nil
		in1 := w.c.MachineByID(w.machines[1].id) != nil
		switch {
		case in0 && !in1:
			out = append(out, pcMAC{pcDupMAC, 0})
		case in1 && !in0:
			out = append(out, pcMAC{pcDupMAC, 1})
		default:
			out = append(out, pcMAC{pcDupMAC, -1})
		}
	}
	return out
}

func (w *pcWorld) checkAll(where string) {
	trusted := !w.awaitingResync
	for _, pm := range w.macs() {
		act := w.c.BootActionForMAC(pm.mac)
		pcStat.add("boot/" + pcActionName(act))
		view := w.c.MachineByMAC(pm.mac)

		if pm.mi < 0 {
			// ipmi-only MACs, MACs modeled on two machines, and unregistered
			// MACs never identify a machine.
			if view != nil {
				w.violate("mac-identity", "%s: MAC %s resolved to machine %s, want unknown", where, pm.mac, pcShort(view.ID))
			}
			if act != model.BootLocal {
				w.violate("unknown-mac-actuates", "%s: unknown MAC %s got %s", where, pm.mac, pcActionName(act))
			}
			continue
		}
		if view == nil {
			if _, inView := w.expBase[pm.mi]; inView {
				w.violate("mac-identity", "%s: MAC %s belongs to M%d, which the applied snapshot holds, but it resolved to no machine", where, pm.mac, pm.mi)
			}
			if act != model.BootLocal {
				w.violate("unknown-mac-actuates", "%s: MAC %s not in view but got %s", where, pm.mac, pcActionName(act))
			}
			continue
		}
		if view.ID != w.machines[pm.mi].id {
			w.violate("mac-identity", "%s: MAC %s resolved to %s, want M%d", where, pm.mac, pcShort(view.ID), pm.mi)
		}

		// Oracle 1: table over the getters.
		cands := w.workerActive(view.ID)
		allowed := map[model.BootAction]bool{}
		if len(cands) == 0 {
			allowed[pcTable(view.State, view.PXEEligible, nil, trusted)] = true
		}
		for _, op := range cands {
			allowed[pcTable(view.State, view.PXEEligible, op, trusted)] = true
		}
		if len(cands) > 1 {
			pcStat.add("worker/dual_active_lifecycle")
		}
		if !allowed[act] {
			w.violate("table", "%s: MAC %s (M%d) state=%s pxe=%v trusted=%v ops=%s: got %s, table allows %v",
				where, pm.mac, pm.mi, view.State, view.PXEEligible, trusted, pcOpsString(cands), pcActionName(act), pcAllowedString(allowed))
		}
		if st := model.MachineState(w.c.MachineState(view.ID)); st != view.State {
			w.violate("getter-disagree", "%s: MachineState=%s but MachineByMAC.State=%s", where, st, view.State)
		}

		// Oracle 2: safety against truth + delivery history.
		if act != model.BootLocal && !trusted {
			w.violate("untrusted-actuates", "%s: M%d got %s between restart and first op_resync", where, pm.mi, pcActionName(act))
		}
		switch act {
		case model.BootDeploy:
			w.checkActuation(where, pm, model.OpDeploy, w.c.DeployOpForMAC(pm.mac))
		case model.BootCommission:
			w.checkActuation(where, pm, model.OpCommission, w.c.CommissionOpForMAC(pm.mac))
		case model.BootLocal:
		}
	}
	w.checkPending(where, trusted)
	w.checkNoResurrection(where)
	w.checkRowVersions(where)
	w.checkEffectiveState(where)
}

func pcOpsString(ops []*model.Operation) string {
	parts := make([]string, 0, len(ops))
	for _, o := range ops {
		parts = append(parts, fmt.Sprintf("%s:%s/%s/%q/v%d", pcShort(o.ID), o.Type, o.Status, o.Phase, o.Version))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func pcAllowedString(m map[model.BootAction]bool) []string {
	var out []string
	for a := range m {
		out = append(out, pcActionName(a))
	}
	sort.Strings(out)
	return out
}

// checkActuation: a non-local boot action must be backed by an op of the right
// type on that machine that (a) the worker was told about, (b) the worker has
// not been told is terminal, and (c) for deploy, whose install the worker has
// not already seen finish. bound is the op the boot script would bind to.
func (w *pcWorld) checkActuation(where string, pm pcMAC, typ model.OperationType, bound *model.Operation) {
	if bound == nil {
		w.violate("actuate-unbound", "%s: M%d got an actuating boot action for %s but the binding getter returned nil", where, pm.mi, typ)
		return
	}
	var live, installed, dead, failed []string
	boundLive := false
	for _, o := range w.ops {
		if o.mi != pm.mi || o.typ != typ || !w.told[o.id] {
			continue
		}
		switch {
		case w.knownDead[o.id]:
			dead = append(dead, pcShort(o.id))
		case typ == model.OpDeploy && w.installed[o.id]:
			installed = append(installed, pcShort(o.id))
		case w.failReported[o.id]:
			failed = append(failed, pcShort(o.id))
		default:
			live = append(live, pcShort(o.id))
			boundLive = boundLive || o.id == bound.ID
		}
	}
	bo := w.opByID(bound.ID)
	if bo == nil || bo.mi != pm.mi || bo.typ != typ {
		w.violate("actuate-wrong-binding", "%s: M%d %s bound to %s which is not a %s op for this machine", where, pm.mi, typ, pcShort(bound.ID), typ)
		return
	}
	if boundLive {
		// The op the boot script binds to is itself live: it justifies the
		// actuation.
		return
	}
	if len(live) > 0 && w.cfg.tier == 2 && w.disordered {
		w.violate("reorder-stale-op-bound", "%s: M%d %s bound to %s, which is not live, while %v is (cache: %s)",
			where, pm.mi, typ, pcShort(bound.ID), live, pcOpsString(w.workerActive(pcMachineID(pm.mi))))
		return
	}
	switch {
	case len(installed) > 0 && bo != nil && w.installed[bo.id]:
		// The post-install PXE of this op got the installer again. Classify by
		// what the history did to the latch.
		id := "rewipe-after-install"
		state := model.MachineState(w.c.MachineState(pcMachineID(pm.mi)))
		// With two active deploy ops held for the machine (a stale one whose
		// op_removed has not landed yet) BootActionForMAC and DeployOpForMAC
		// each pick by map order, so the op that decided the action need not
		// be the bound one. Classify over every installed op still held.
		var failFirst, failForgotten, execAfter, pendingEcho, restarted, omitted bool
		latePhase := ""
		held := 0
		for _, g := range w.workerActive(pcMachineID(pm.mi)) {
			if g.Type != model.OpDeploy || !w.installed[g.ID] {
				continue
			}
			held++
			failFirst = failFirst || w.failBeforeInstall[g.ID]
			failForgotten = failForgotten || w.restartAfterFail[g.ID]
			execAfter = execAfter || w.execAfterInstall[g.ID]
			pendingEcho = pendingEcho || w.pendingEchoAfterInstall[g.ID]
			restarted = restarted || w.restartAfterInstall[g.ID]
			omitted = omitted || w.omittedAfterInstall[g.ID]
			if w.phaseAfterInstall[g.ID] != "" {
				latePhase = w.phaseAfterInstall[g.ID]
			}
		}
		if held > 1 {
			pcStat.add("worker/dual_active_installed_deploy_ops")
		}
		switch {
		case failForgotten:
			// A late agent failure report took the op off the latch (the
			// one phase allowed to), and a restart then forgot the failure.
			id = "failed-op-forgotten-on-restart"
		case omitted:
			// An op_resync dropped the latched row; the op came back at
			// global's pre-latch phase.
			id = "rewipe-latch-lost-on-resync-omission"
		case state != model.MachineStateDeploying:
			id = "rewipe-latch-outside-deploying"
			if bound.Phase == "awaiting_phone_home" {
				pcStat.add("rewipe-outside-deploying/latch_phase_still_set")
			}
			if w.pendingAtInstall[bo.id] {
				pcStat.add("rewipe-outside-deploying/installed_while_pending")
			} else {
				pcStat.add("rewipe-outside-deploying/claimed_but_no_deploying_overlay")
			}
		case failFirst:
			id = "rewipe-latch-refused-after-failure"
		case execAfter:
			id = "rewipe-latch-phase-clobber"
			pcStat.add("rewipe-phase-clobber/executor_power_cycle")
		case latePhase != "":
			id = "rewipe-latch-phase-clobber"
			pcStat.add("rewipe-phase-clobber/late_agent_report")
		case pendingEcho:
			id = "rewipe-latch-pending-echo"
		case restarted:
			id = "rewipe-latch-lost-on-restart"
		}
		w.violate(id, "%s: M%d (effective state %s) got BootDeploy bound to %s (cache: %s) after its install already finished (deploy_complete/phone-home accepted); later phase report=%q restarted=%v",
			where, pm.mi, state, pcShort(bound.ID), pcOpsString(w.workerActive(pcMachineID(pm.mi))), latePhase, restarted)
	case bo != nil && w.knownDead[bo.id]:
		w.violate("dead-op-actuates", "%s: M%d got an actuating action bound to %s, which the worker was told is terminal; dead=%v",
			where, pm.mi, pcShort(bound.ID), dead)
	case bo != nil && w.failReported[bo.id]:
		id := "failed-op-actuates"
		if w.restartAfterFail[bo.id] {
			id = "failed-op-forgotten-on-restart"
		}
		w.violate(id, "%s: M%d got an actuating action bound to %s after the agent reported it failed", where, pm.mi, pcShort(bound.ID))
	default:
		w.violate("actuate-unjustified", "%s: M%d got an actuating %s action bound to %s with no live op (installed=%v dead=%v failed=%v)",
			where, pm.mi, typ, pcShort(bound.ID), installed, dead, failed)
	}
}

// checkPending: PendingPowerOps / PendingLifecycleOps bucketing against an
// independent rule, plus the executor-facing safety properties.
func (w *pcWorld) checkPending(where string, trusted bool) {
	lc := w.c.PendingLifecycleOps()
	pw := w.c.PendingPowerOps()
	got := map[uuid.UUID]string{}
	for _, o := range lc {
		if _, dup := got[o.ID]; dup {
			w.violate("pending-dup", "%s: %s returned twice", where, pcShort(o.ID))
		}
		got[o.ID] = "lifecycle"
	}
	for _, o := range pw {
		if _, dup := got[o.ID]; dup {
			w.violate("pending-dup", "%s: %s returned by both buckets or twice", where, pcShort(o.ID))
		}
		got[o.ID] = "power"
	}
	if !trusted && len(got) > 0 {
		w.violate("untrusted-actuates", "%s: executor handed %d pending ops between restart and first op_resync", where, len(got))
	}
	for id, bucket := range got {
		pcStat.add("pending/" + bucket)
		o := w.opByID(id)
		if o == nil {
			w.violate("pending-unknown", "%s: executor handed unknown op %s", where, id)
			continue
		}
		if w.knownDead[id] {
			w.violate("dead-op-actuates", "%s: executor handed %s (%s), which the worker was told is terminal", where, pcShort(id), o.typ)
		}
		// Until the claim's own op_delta lands, an op_resync built before the
		// claim can legitimately have reset the row to pending; the executor is
		// still busy driving the op then. Once the Cache holds a row at least
		// as new as the claim, offering the op again would drive it twice.
		if claimV, ok := w.driven[id]; ok && w.expVer[id] >= claimV {
			w.violate("driven-op-offered-again", "%s: executor handed %s (%s) again after it won the claim and drove it", where, pcShort(id), o.typ)
		}
		if !w.told[id] {
			w.violate("pending-untold", "%s: executor handed %s which no delivered message carried", where, pcShort(id))
		}
	}
	// Completeness + bucket: every pending op the Cache holds for an in-view
	// machine is offered in exactly the documented bucket.
	for _, o := range w.ops {
		g := w.c.GetOperation(o.id.String())
		want := ""
		if g != nil && g.Status == model.OpPending && trusted && w.c.MachineByID(g.MachineID) != nil {
			switch g.Type {
			case model.OpDeploy, model.OpCommission:
				want = "lifecycle"
			case model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle, model.OpSetBootDevice, model.OpBMCReconcile:
				want = "power"
				if o.selfOut {
					want = ""
					pcStat.add("pending/self_outlet_hidden")
				}
			}
		}
		if got[o.id] != want {
			w.violate("pending-bucket", "%s: %s (%s, cache=%v) offered as %q, want %q", where, pcShort(o.id), o.typ, g != nil, got[o.id], want)
		}
	}
}

// checkRowVersions: the Cache holds exactly the ops it was told about and
// not told are gone, each at the highest version delivered.
func (w *pcWorld) checkRowVersions(where string) {
	for _, o := range w.ops {
		want, told := w.expVer[o.id]
		g := w.c.GetOperation(o.id.String())
		switch {
		case told && g == nil:
			w.violate("told-op-missing", "%s: the worker was told about %s (v%d) and not that it is gone, but the Cache does not hold it", where, pcShort(o.id), want)
		case told && g.Version != want:
			w.violate("row-not-newest-version", "%s: %s is held at v%d, newest delivered row is v%d", where, pcShort(o.id), g.Version, want)
		case !told && g != nil && !w.knownDead[o.id]:
			w.violate("untold-op-held", "%s: the Cache holds %s (v%d) but no delivered message carries it", where, pcShort(o.id), g.Version)
		}
	}
}

// checkNoResurrection: once the worker was told an op is terminal, the Cache
// must never hold it as active again.
func (w *pcWorld) checkNoResurrection(where string) {
	for _, o := range w.ops {
		if !w.knownDead[o.id] {
			continue
		}
		g := w.c.GetOperation(o.id.String())
		if g == nil {
			continue
		}
		if g.Status == model.OpCompleted || g.Status == model.OpFailed {
			w.violate("zombie-terminal-op", "%s: %s is held (status %s, v%d) after the worker was told it is terminal", where, pcShort(o.id), g.Status, g.Version)
			continue
		}
		w.violate("resurrected-op", "%s: %s is ACTIVE again (status %s, v%d) after the worker was told it is terminal", where, pcShort(o.id), g.Status, g.Version)
	}
}

// ---------------------------------------------------------------------------
// Oracle 3: convergence
// ---------------------------------------------------------------------------

func pcClass(s model.OperationStatus) string {
	switch s {
	case model.OpPending:
		return "pending"
	case model.OpClaimed, model.OpRunning:
		return "active"
	default:
		return "terminal"
	}
}

// phoneHomeThenFailedPending reports whether the Cache accepted a phone-home
// for a deploy op of machine mi that global failed while it was still
// pending.
func (w *pcWorld) phoneHomeThenFailedPending(mi int) bool {
	for _, o := range w.ops {
		if o.mi == mi && o.failedPending && w.phoneHomed[o.id] {
			return true
		}
	}
	return false
}

func (w *pcWorld) checkConverged(where string, afterResync bool) {
	pcStat.add("converge/checked/" + where)
	// pre tags a divergence that needed reordered or duplicated delivery.
	pre := ""
	if where == "reordered-stream" {
		pre = "reorder-"
	}
	for _, m := range w.machines {
		got := w.c.MachineState(m.id)
		if !m.inSite {
			if got != "" {
				w.violate("converge-view", "%s: M%d left the site but the Cache still knows it (state %q)", where, m.idx, got)
			}
			continue
		}
		if got != string(m.state) {
			id := "converge-state"
			switch {
			case got == string(model.MachineStateRunning) && w.phoneHomeThenFailedPending(m.idx):
				id = "running-overlay-after-pending-op-failed"
			case pre != "" && w.cfg.tier == 2 && w.disordered:
				// Arbitrary reorder: the unversioned overlay took an older
				// message's state (see checkEffectiveState).
				id = "reorder-overlay-divergence"
			default:
				id = pre + id
			}
			w.violate(id, "%s: M%d effective state %q, truth %q", where, m.idx, got, m.state)
		}
	}
	for _, o := range w.ops {
		g := w.c.GetOperation(o.id.String())
		if !o.active() {
			if g != nil && pcClass(g.Status) != "terminal" {
				w.violate(pre+"converge-ops", "%s: Cache holds %s as %s but truth has it %s", where, pcShort(o.id), g.Status, o.status)
			}
			continue
		}
		if !w.hubMember(o.mi) {
			// The machine is outside the hub's snapshot for this site, so the
			// stream no longer carries this op. A copy stored earlier stays
			// (invisible to the executor: its machine is not in the view)
			// until an op_resync, which omits it, replaces the set.
			if afterResync && g != nil {
				w.violate("converge-ops", "%s: Cache still holds out-of-site op %s after an op_resync that omitted it", where, pcShort(o.id))
			}
			continue
		}
		if g == nil {
			w.violate(pre+"converge-ops", "%s: truth has %s active (%s) but the Cache does not hold it", where, pcShort(o.id), o.status)
			continue
		}
		if g.Version != o.version {
			w.violate(pre+"converge-op-version", "%s: %s version %d, truth %d", where, pcShort(o.id), g.Version, o.version)
		}
		if g.Status == model.OpFailed && w.failReported[o.id] {
			// Designed divergence: a failure the agent reported is held
			// against global's still-active row until op_removed. Global
			// learns of it from the op_update, or fails the op in a sweep.
			pcStat.add("converge/local_failure_held")
			continue
		}
		if pcClass(g.Status) != pcClass(o.status) {
			w.violate(pre+"converge-op-status", "%s: %s status %s, truth %s", where, pcShort(o.id), g.Status, o.status)
		}
		if g.Phase != o.phase {
			// The one designed divergence: a locally latched terminal wait
			// that global has not recorded.
			latched := g.Phase == "awaiting_phone_home" && w.installed[o.id]
			if !latched {
				w.violate(pre+"converge-op-phase", "%s: %s phase %q, truth %q", where, pcShort(o.id), g.Phase, o.phase)
			}
		}
	}
}

// settle drains everything in order: outbox -> global -> stream -> Cache.
func (w *pcWorld) settle(where string) {
	for i := 0; i < 64; i++ {
		w.drainOutbox()
		moved := len(w.w2gUpd) > 0 || len(w.w2gObs) > 0 || len(w.g2w) > 0
		w.processUpdates(len(w.w2gUpd))
		for len(w.w2gObs) > 0 {
			w.processObs(0)
		}
		for len(w.g2w) > 0 {
			w.deliver(0, false)
			w.checkAll(where)
		}
		if !moved {
			return
		}
	}
	w.t.Fatalf("settle did not quiesce after 64 rounds")
}

// ---------------------------------------------------------------------------
// Steps
// ---------------------------------------------------------------------------

var pcAgentPhases = []string{
	"agent_starting", "partitioning", "rsyncing_os", "installing_bootloader", "finalizing", "bmc_reconcile",
	"collecting_inventory", "reporting",
}

var pcOpTypes = []model.OperationType{
	model.OpDeploy, model.OpDeploy, model.OpDeploy, model.OpCommission, model.OpCommission,
	model.OpPowerCycle, model.OpPowerOn, model.OpPowerOff, model.OpSetBootDevice, model.OpBMCReconcile,
}

var pcStates = []model.MachineState{
	model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning,
}

func (w *pcWorld) commissionData(mi int) json.RawMessage {
	m := w.machines[mi]
	b, _ := json.Marshal(map[string]any{
		"system": map[string]any{"serial_number": fmt.Sprintf("SER-%d", m.idx)},
		"nics":   []any{map[string]any{"mac": m.mac, "driver": "ixgbe"}},
	})
	return b
}

func (w *pcWorld) pickOp(label string, pred func(*pcOp) bool) *pcOp {
	var idxs []int
	for i, o := range w.ops {
		if pred == nil || pred(o) {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) == 0 {
		return nil
	}
	return w.ops[rapid.SampledFrom(idxs).Draw(w.t, label)]
}

func (w *pcWorld) step() {
	t := w.t
	type choice struct {
		name   string
		weight int
		run    func()
	}
	var cs []choice
	add := func(name string, weight int, ok bool, run func()) {
		if ok && weight > 0 {
			cs = append(cs, choice{name, weight, run})
		}
	}
	nm := len(w.machines)
	activeLife := func(o *pcOp) bool { return o.active() && pcLifecycle(o.typ) }
	// held: lifecycle ops the Cache currently holds as active (reports for
	// these are accepted). Agent steps mostly target them, and sometimes any
	// truth-active op so the rejection paths stay exercised.
	heldSet := map[uuid.UUID]bool{}
	anyHeld, anyHeldDeploy, anyHeldInstalled := false, false, false
	for _, o := range w.ops {
		g := w.c.GetOperation(o.id.String())
		if g == nil || !pcLifecycle(g.Type) || g.Status == model.OpCompleted || g.Status == model.OpFailed {
			continue
		}
		heldSet[o.id] = true
		anyHeld = true
		if g.Type == model.OpDeploy {
			anyHeldDeploy = true
			if w.installed[o.id] {
				anyHeldInstalled = true
			}
		}
	}
	agentOp := func(pred func(*pcOp) bool) *pcOp {
		if anyHeld && rapid.IntRange(0, 9).Draw(t, "anyOp") != 0 {
			if o := w.pickOp("op", func(o *pcOp) bool { return heldSet[o.id] && (pred == nil || pred(o)) }); o != nil {
				return o
			}
		}
		return w.pickOp("op", activeLife)
	}
	isDeploy := func(o *pcOp) bool { return o.typ == model.OpDeploy }
	isCommission := func(o *pcOp) bool { return o.typ == model.OpCommission }
	weigh := func(hot bool, hi, lo int) int {
		if hot {
			return hi
		}
		return lo
	}
	anyActiveLife := false
	anyActive := false
	for _, o := range w.ops {
		if o.active() {
			anyActive = true
			if pcLifecycle(o.typ) {
				anyActiveLife = true
			}
		}
	}

	// ---- global ----
	var freeMachines []int // idx_one_active_op_per_machine
	for i := range w.machines {
		if w.activeOpOn(i) == nil {
			freeMachines = append(freeMachines, i)
		}
	}
	add("g.create", 6, len(w.ops) < 16 && len(freeMachines) > 0, func() {
		mi := rapid.SampledFrom(freeMachines).Draw(t, "machine")
		typ := rapid.SampledFrom(pcOpTypes).Draw(t, "type")
		if !w.cfg.swPower && !pcLifecycle(typ) {
			typ = model.OpDeploy
		}
		if !w.cfg.swAnyCreate && !pcClaimAllowed(typ, w.machines[mi].state) {
			pcStat.add("step/noop")
			return // the API refuses the op for this machine state
		}
		selfOutlet, allowSelf := false, false
		if !pcLifecycle(typ) && typ != model.OpBMCReconcile && typ != model.OpSetBootDevice {
			selfOutlet = rapid.IntRange(0, 3).Draw(t, "selfOutlet") == 0
			allowSelf = rapid.Bool().Draw(t, "allowSelf")
		}
		w.gCreate(mi, typ, selfOutlet, allowSelf)
	})
	add("g.fail", 2, anyActive, func() {
		if o := w.pickOp("op", (*pcOp).active); o != nil {
			w.gFail(o, "abort/sweep")
		}
	})
	add("g.claim_other", 1, w.cfg.swOther && anyActive, func() {
		if o := w.pickOp("op", func(o *pcOp) bool { return o.status == model.OpPending }); o != nil {
			w.gClaim(o, false)
		}
	})
	add("g.patch", 1, w.cfg.swPatch, func() {
		// PXE eligibility is the only patched field the boot decision reads.
		// machines.state is not writable through the API (a PATCH carrying
		// it is refused), so only op transitions move it.
		m := w.machines[rapid.IntRange(0, nm-1).Draw(t, "machine")]
		m.pxe = !m.pxe
		w.logf("G patch M%d pxe=%v", m.idx, m.pxe)
		pcStat.add("truth/patch_pxe")
	})
	add("g.member", 1, w.cfg.swMember, func() {
		m := w.machines[rapid.IntRange(0, nm-1).Draw(t, "machine")]
		m.inSite = !m.inSite
		w.logf("G membership M%d inSite=%v", m.idx, m.inSite)
		pcStat.add("truth/membership")
	})
	add("g.rebuild", 6, true, func() {
		if !w.rebuild() {
			pcStat.add("step/noop")
		} else {
			w.logf("G rebuild -> seq %d", w.seq)
		}
	})

	// ---- channel ----
	add("ch.deliver", 22, w.connected && len(w.g2w) > 0, func() {
		idxs := w.deliverable()
		idx := idxs[0]
		if len(idxs) > 1 && rapid.IntRange(0, 3).Draw(t, "reorder") == 0 {
			idx = rapid.SampledFrom(idxs[1:]).Draw(t, "idx")
			pcStat.add("chan/reordered")
		}
		dup := false
		if w.cfg.tier == 2 && w.g2w[idx].kind != pcResync {
			dup = rapid.IntRange(0, 5).Draw(t, "dup") == 0
			if dup {
				pcStat.add("chan/duplicated")
			}
		}
		w.deliver(idx, dup)
	})
	add("ch.process_updates", 14, w.connected && len(w.w2gUpd) > 0, func() {
		w.processUpdates(rapid.IntRange(1, 3).Draw(t, "n"))
	})
	add("ch.process_obs", 6, w.connected && len(w.w2gObs) > 0, func() {
		w.processObs(rapid.IntRange(0, len(w.w2gObs)-1).Draw(t, "idx"))
	})
	add("ch.disconnect", 2, w.connected && w.cfg.tier > 0, func() { w.disconnect("drop") })
	add("ch.connect", 6, !w.connected, func() { w.connect() })

	// ---- worker ----
	add("w.drain", 12, w.connected, func() { w.drainOutbox() })
	add("w.restart", 2, w.cfg.useDB && w.cfg.tier > 0, func() { w.restart() })
	hasPending := len(w.c.PendingPowerOps())+len(w.c.PendingLifecycleOps()) > 0
	add("w.exec", weigh(hasPending, 16, 1), true, func() { w.stepExecutor() })
	add("w.dhcp", 5, true, func() {
		pm := rapid.SampledFrom(w.macs()).Draw(t, "mac")
		isIPXE := rapid.Bool().Draw(t, "ipxe")
		// dhcpCfg.OnACK (cmd/slagworksd/site.go).
		m := w.c.MachineByMAC(pm.mac)
		if m == nil {
			return
		}
		op := w.c.ActiveBootingOpForMachine(m.ID)
		if op == nil {
			return
		}
		phase := "dhcp_ack"
		if isIPXE {
			phase = "ipxe_dhcp"
		}
		w.c.UpdateOpPhase(op.ID.String(), phase)
		w.logf("W dhcp ack %s -> UpdateOpPhase(%s, %s)", pm.mac, pcShort(op.ID), phase)
		pcStat.add("worker/dhcp_phase")
	})
	add("w.bootscript", 8, true, func() {
		// httpboot.Handler.ServeHTTP: decide, then report ipxe_loaded on the
		// op the script binds to.
		pm := rapid.SampledFrom(w.macs()).Draw(t, "mac")
		act := w.c.BootActionForMAC(pm.mac)
		var bootOp *model.Operation
		switch act {
		case model.BootDeploy:
			bootOp = w.c.DeployOpForMAC(pm.mac)
		case model.BootCommission:
			bootOp = w.c.CommissionOpForMAC(pm.mac)
		case model.BootLocal:
		}
		w.logf("W boot script %s -> %s", pm.mac, pcActionName(act))
		pcStat.add("worker/bootscript/" + pcActionName(act))
		if bootOp != nil {
			_ = w.c.HandleProgressReport(&model.ProgressReport{
				OperationID: bootOp.ID.String(), MachineID: bootOp.MachineID.String(), Phase: "ipxe_loaded",
			})
		}
	})
	add("w.agent_phase", weigh(anyHeld, 12, 2), anyActiveLife, func() {
		o := agentOp(nil)
		if o == nil {
			return
		}
		if w.installed[o.id] && !w.cfg.swLatePhase {
			pcStat.add("step/noop")
			return
		}
		phase := rapid.SampledFrom(pcAgentPhases).Draw(t, "phase")
		if err := w.report(o, o.mi, model.ProgressReport{Phase: phase}); err == nil {
			pcStat.add("worker/agent_phase_ok")
			if w.installed[o.id] {
				pcStat.add("worker/agent_phase_after_install")
				w.phaseAfterInstall[o.id] = phase
			}
		} else {
			pcStat.add("worker/agent_phase_rejected")
		}
	})
	add("w.deploy_complete", weigh(anyHeldDeploy, 10, 2), anyActiveLife, func() {
		o := agentOp(isDeploy)
		if o == nil {
			return
		}
		if err := w.report(o, o.mi, model.ProgressReport{Event: "deploy_complete"}); err == nil {
			if o.typ != model.OpDeploy {
				w.violate("report-binding", "deploy_complete accepted for %s op %s", o.typ, pcShort(o.id))
			}
			if w.failReported[o.id] && !w.installed[o.id] {
				w.failBeforeInstall[o.id] = true
				pcStat.add("worker/deploy_complete_after_failure_report")
			}
			if g := w.c.GetOperation(o.id.String()); g != nil && g.Status == model.OpPending && !w.installed[o.id] {
				w.pendingAtInstall[o.id] = true
				pcStat.add("worker/deploy_complete_while_pending")
			}
			w.installed[o.id] = true
			pcStat.add("worker/deploy_complete_ok")
		} else {
			pcStat.add("worker/deploy_complete_rejected")
		}
	})
	add("w.phone_home", weigh(anyHeldInstalled, 12, 1), anyActiveLife, func() {
		o := agentOp(func(o *pcOp) bool { return w.installed[o.id] })
		if o == nil {
			return
		}
		// A real phone-home only comes from an installed OS.
		if !w.installed[o.id] {
			pcStat.add("step/noop")
			return
		}
		if err := w.report(o, o.mi, model.ProgressReport{Event: "phone_home"}); err == nil {
			w.phoneHomed[o.id] = true
			w.expSkip[o.mi] = true
			pcStat.add("worker/phone_home_ok")
		} else {
			pcStat.add("worker/phone_home_rejected")
		}
	})
	add("w.agent_fail", 3, anyActiveLife && w.cfg.swFailReport, func() {
		o := agentOp(nil)
		if o == nil {
			return
		}
		// The real agent reboots right after deploy_complete; a report after
		// it only exists as a late or duplicated request (swLatePhase).
		if w.installed[o.id] && !w.cfg.swLatePhase {
			pcStat.add("step/noop")
			return
		}
		phase := rapid.SampledFrom(pcAgentPhases).Draw(t, "phase")
		if err := w.report(o, o.mi, model.ProgressReport{Failed: true, Phase: phase, Error: "boom"}); err == nil {
			w.failReported[o.id] = true
			pcStat.add("worker/agent_fail_ok")
			if w.installed[o.id] {
				w.phaseAfterInstall[o.id] = "failed@" + phase
			}
		}
	})
	add("w.commission_report", 6, anyActiveLife, func() {
		o := agentOp(isCommission)
		if o == nil {
			return
		}
		err := w.report(o, o.mi, model.ProgressReport{Event: "commission_report", CommissionData: w.commissionData(o.mi)})
		if err == nil {
			if o.typ != model.OpCommission {
				w.violate("report-binding", "commission_report accepted for %s op %s", o.typ, pcShort(o.id))
			}
			pcStat.add("worker/commission_report_ok")
		}
	})
	add("w.report_mismatch", 2, len(w.ops) > 0, func() {
		// A report whose machine does not own the op, or for a terminal /
		// non-lifecycle op, must be rejected and change nothing.
		o := w.pickOp("op", nil)
		mi := rapid.IntRange(0, nm-1).Draw(t, "machine")
		g := w.c.GetOperation(o.id.String())
		mustReject := mi != o.mi || g == nil || !pcLifecycle(o.typ) || g.Status == model.OpCompleted || g.Status == model.OpFailed
		if !mustReject {
			pcStat.add("step/noop")
			return // a well-bound report is w.agent_phase's job
		}
		err := w.report(o, mi, model.ProgressReport{Phase: "partitioning"})
		pcStat.add("worker/report_mismatch_rejected")
		if err == nil {
			w.violate("report-binding", "report for %s with machine M%d accepted (op machine M%d, cache=%v)", pcShort(o.id), mi, o.mi, g != nil)
		}
		after := w.c.GetOperation(o.id.String())
		if (g == nil) != (after == nil) || (g != nil && (g.Phase != after.Phase || g.Status != after.Status)) {
			w.violate("report-binding", "rejected report mutated %s", pcShort(o.id))
		}
	})
	total := 0
	for _, c := range cs {
		total += c.weight
	}
	pick := rapid.IntRange(0, total-1).Draw(t, "step")
	for _, c := range cs {
		if pick < c.weight {
			pcStat.add("step/" + c.name)
			c.run()
			w.checkAll(c.name)
			return
		}
		pick -= c.weight
	}
}

// stepExecutor is Executor.executeOp's cache-facing behavior for one op.
func (w *pcWorld) stepExecutor() {
	t := w.t
	pend := append(w.c.PendingPowerOps(), w.c.PendingLifecycleOps()...)
	if len(pend) == 0 {
		pcStat.add("step/noop")
		return
	}
	sort.Slice(pend, func(i, j int) bool { return pend[i].ID.String() < pend[j].ID.String() })
	op := pend[rapid.IntRange(0, len(pend)-1).Draw(t, "pending")]
	o := w.opByID(op.ID)
	id := op.ID.String()
	w.c.MarkOpClaimed(id)
	outcome := rapid.SampledFrom([]string{"ok", "ok", "ok", "ok", "transport_error", "reply_lost"}).Draw(t, "claimOutcome")
	if !w.connected {
		outcome = "transport_error"
	} else if w.cfg.tier == 0 {
		outcome = "ok" // a lossless connection answers every RPC
	}
	w.logf("W executor picks %s (%s): claim %s", pcShort(op.ID), op.Type, outcome)
	pcStat.add("worker/exec/" + outcome)
	if outcome == "transport_error" {
		// The request never reached global: the connection is gone.
		w.disconnect("claim_op send failed")
		w.c.MarkOpStatus(id, model.OpPending) // rollback
		return
	}
	// The hub answers the hello (snapshot, op_resync) from its read loop
	// before it reads the claim, and the worker's single read loop applies
	// messages in wire order, so an op_resync still in flight (and everything
	// ahead of it) lands before the claim reply reaches the executor.
	// The same is assumed for every op message published before the claim
	// was processed: it was queued for the connection a database round trip
	// ahead of the reply. Tier 2 (adversarial) only keeps the op_resync
	// barrier.
	// Snapshots are pushed independently and keep their place.
	ahead := len(w.g2w)
	lastResync := -1
	for i, m := range w.g2w {
		if m.kind == pcResync {
			lastResync = i
		}
	}
	res := w.gClaim(o, true)
	if outcome == "reply_lost" {
		// The claim may have committed, but the connection died before the
		// reply arrived.
		w.disconnect("claim_op reply lost")
		w.c.MarkOpStatus(id, model.OpPending)
		return
	}
	through := lastResync
	if w.cfg.tier < 2 {
		// Everything up to the last op message ahead of the claim lands, in
		// order. Snapshots between them land too (an op message only overtakes
		// the newest snapshot; delivering that one in place is one valid
		// schedule); snapshots behind the last op message stay in the push
		// pipeline.
		for i := ahead - 1; i > through; i-- {
			if w.g2w[i].kind != pcSnap {
				through = i
				break
			}
		}
	}
	for i := 0; i <= through; i++ {
		w.deliver(0, false)
	}
	switch res {
	case pcClaimLost:
		pcStat.add("worker/exec/claim_lost")
		return // keep the local claimed mark
	case pcClaimErr:
		w.c.MarkOpStatus(id, model.OpPending)
		return
	case pcClaimWon:
	}
	// The claim is ours and committed: from here on the executor must never
	// be handed this op again (it would power-cycle the host a second time).
	w.driven[op.ID] = o.version
	actuated := rapid.IntRange(0, 5).Draw(t, "actuationOK") != 0
	switch {
	case pcLifecycle(op.Type):
		if w.c.MachineByID(op.MachineID) == nil || !actuated {
			w.c.RecordOpUpdate(id, "failed", json.RawMessage(`{"error":"x"}`))
			return
		}
		if w.installed[op.ID] {
			pcStat.add("worker/exec/drove_installed_op")
			w.execAfterInstall[op.ID] = true
		}
		w.c.PrepareDeployLogs(op.MachineID.String(), id)
		w.c.UpdateOpPhase(id, "power_cycle")
		w.c.UpdateOpPhase(id, "pxe_booting")
		w.c.MarkOpStatus(id, model.OpRunning)
	case op.Type == model.OpBMCReconcile:
		if !actuated {
			w.c.RecordOpUpdate(id, "failed", json.RawMessage(`{"error":"x"}`))
			return
		}
		_ = w.c.recordBMCReconcileReport(op.MachineID, op.ID, model.BMCReconcileReport{})
	default:
		if actuated {
			w.c.RecordOpUpdate(id, "completed", nil)
		} else {
			w.c.RecordOpUpdate(id, "failed", json.RawMessage(`{"error":"x"}`))
		}
	}
}

// ---------------------------------------------------------------------------
// Case driver
// ---------------------------------------------------------------------------

func pcDrawConfig(t *rapid.T, forceTier int, useDB bool) pcConfig {
	cfg := pcConfig{tier: forceTier, useDB: useDB}
	if forceTier < 0 {
		cfg.tier = rapid.SampledFrom([]int{0, 1, 1, 2}).Draw(t, "tier")
	}
	cfg.dupMAC = rapid.IntRange(0, 4).Draw(t, "dupMAC") == 0
	cfg.reorderWin = rapid.IntRange(1, 4).Draw(t, "reorderWin")
	cfg.swPatch = rapid.Bool().Draw(t, "swPatch")
	cfg.swMember = rapid.IntRange(0, 3).Draw(t, "swMember") == 0
	cfg.swOther = rapid.Bool().Draw(t, "swOther")
	cfg.swLatePhase = rapid.IntRange(0, 3).Draw(t, "swLatePhase") == 0
	cfg.swFailReport = rapid.Bool().Draw(t, "swFailReport")
	cfg.swPower = rapid.Bool().Draw(t, "swPower")
	cfg.swAnyCreate = rapid.IntRange(0, 3).Draw(t, "swAnyCreate") == 0
	return cfg
}

func pcNewWorld(t *rapid.T, cfg pcConfig, dir string) *pcWorld {
	w := &pcWorld{
		t: t, cfg: cfg, clean: true, lossless: true,
		expBase: map[int]model.MachineState{}, expOv: map[int]pcToldState{}, expSkip: map[int]bool{},
		expVer: map[uuid.UUID]int{},
		told:   map[uuid.UUID]bool{}, knownDead: map[uuid.UUID]bool{},
		installed: map[uuid.UUID]bool{}, failReported: map[uuid.UUID]bool{}, driven: map[uuid.UUID]int{},
		phaseAfterInstall: map[uuid.UUID]string{}, restartAfterInstall: map[uuid.UUID]bool{},
		execAfterInstall: map[uuid.UUID]bool{}, failBeforeInstall: map[uuid.UUID]bool{}, restartAfterFail: map[uuid.UUID]bool{},
		pendingAtInstall: map[uuid.UUID]bool{}, pendingEchoAfterInstall: map[uuid.UUID]bool{}, omittedAfterInstall: map[uuid.UUID]bool{},
		phoneHomed: map[uuid.UUID]bool{},
	}
	nm := rapid.IntRange(2, 3).Draw(t, "machines")
	for i := 0; i < nm; i++ {
		w.machines = append(w.machines, &pcMachine{
			idx: i, id: pcMachineID(i),
			mac:     fmt.Sprintf("52:54:00:00:00:%02x", i+1),
			ipmiMAC: fmt.Sprintf("52:54:00:aa:00:%02x", i+1),
			state:   rapid.SampledFrom(pcStates).Draw(t, fmt.Sprintf("state%d", i)),
			pxe:     rapid.IntRange(0, 5).Draw(t, fmt.Sprintf("pxe%d", i)) != 0,
			inSite:  true,
		})
		if w.machines[i].state != model.MachineStateNew {
			w.machines[i].commData = true
		}
	}
	if cfg.useDB {
		w.dbPath = filepath.Join(dir, "worker.db")
		db, err := OpenLocalDB(w.dbPath)
		if err != nil {
			t.Fatalf("open local db: %v", err)
		}
		w.db = db
		w.c = NewCacheWithSerial(db, pcSelfSerial)
	} else {
		w.c = NewCacheWithSerial(nil, pcSelfSerial)
	}
	return w
}

func pcRunCase(t *rapid.T, forceTier int, useDB bool, scratch func() string) {
	cfg := pcDrawConfig(t, forceTier, useDB)
	dir := ""
	if cfg.useDB {
		dir = scratch()
		defer os.RemoveAll(dir)
	}
	w := pcNewWorld(t, cfg, dir)
	defer func() { pcCloseDB(w.db) }()
	pcStat.add(fmt.Sprintf("case/tier%d", cfg.tier))
	if cfg.useDB {
		pcStat.add("case/localdb")
	}

	w.connect()
	w.checkAll("init")
	n := rapid.IntRange(1, 160).Draw(t, "steps")
	for i := 0; i < n; i++ {
		w.step()
	}
	pcStat.max("max/ops_per_case", len(w.ops))
	pcStat.max("max/trace_len", len(w.trace))

	// Quiesce: everything in flight lands, in order.
	w.logf("--- quiesce ---")
	if !w.connected {
		w.connect()
	}
	w.settle("settle")
	w.rebuild()
	w.settle("settle+identity")
	if w.clean {
		// Nothing was reordered, duplicated or dropped: the stream alone must
		// have kept the Cache equal to the truth.
		pcStat.add("case/clean_history")
		w.checkConverged("stream-only", false)
	} else if w.lossless {
		// Reordered and/or duplicated, but nothing lost: last-writer-wins by
		// version plus tombstones are documented to converge regardless.
		pcStat.add("case/lossless_reordered_history")
		w.checkConverged("reordered-stream", false)
	}
	// Final op_resync + identity snapshot.
	w.disconnect("final resync")
	w.connect()
	w.settle("final")
	w.checkConverged("after-resync", true)
}

// pcQuiet starts a property test: it clears the coverage counters and
// silences the Cache's slog output (fail-closed warnings, duplicate MAC
// warnings) for the duration of the test.
func pcQuiet(t *testing.T) {
	pcStat.reset()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func pcScratch(t *testing.T) func() string {
	pcQuiet(t)
	base := t.TempDir()
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		n++
		d := filepath.Join(base, fmt.Sprintf("case-%d", n))
		mu.Unlock()
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		return d
	}
}

// TestOpCacheProp_Model is the broad swarm: all tiers, in-memory Cache.
func TestOpCacheProp_Model(t *testing.T) {
	scratch := pcScratch(t)
	rapid.Check(t, func(rt *rapid.T) { pcRunCase(rt, -1, false, scratch) })
	t.Logf("coverage:\n%s", pcStat.dump())
}

// TestOpCacheProp_Restart backs the Cache with a real LocalDB so worker
// restarts (rehydrate, untrusted until op_resync) are part of the history.
func TestOpCacheProp_Restart(t *testing.T) {
	scratch := pcScratch(t)
	rapid.Check(t, func(rt *rapid.T) { pcRunCase(rt, 1, true, scratch) })
	t.Logf("coverage:\n%s", pcStat.dump())
}

// TestOpCacheProp_Reliable is the realistic baseline: one ordered, lossless
// connection. Any divergence here needs no channel fault at all.
func TestOpCacheProp_Reliable(t *testing.T) {
	scratch := pcScratch(t)
	rapid.Check(t, func(rt *rapid.T) { pcRunCase(rt, 0, false, scratch) })
	t.Logf("coverage:\n%s", pcStat.dump())
}

// TestOpCacheProp_TombstoneBound probes the bounded tombstone set against a
// plain FIFO model: an op_removed tombstones (opID -> version); a later
// op_delta for that op is dropped iff a tombstone at >= its version is still
// held; the set keeps the newest tombstoneCap ops by FIRST removal (a repeat
// removal keeps its place and the higher version). Bulk removals push the
// set across its capacity so eviction, and the resurrection it permits, is
// part of every long case.
func TestOpCacheProp_TombstoneBound(t *testing.T) {
	pcQuiet(t)
	rapid.Check(t, func(rt *rapid.T) {
		c := NewCache(nil)
		mid := pcMachineID(0)
		// Model: the tombstoned ops in order of first removal (oldest first)
		// and the highest version each was removed at.
		var fifo []uuid.UUID
		vers := map[uuid.UUID]int{}
		remove := func(id uuid.UUID, ver int) {
			c.ApplyOpRemoved(id.String(), mid.String(), ver, "")
			if cur, ok := vers[id]; ok {
				if ver > cur {
					vers[id] = ver
				}
				return
			}
			if len(fifo) >= tombstoneCap {
				delete(vers, fifo[0])
				fifo = fifo[1:]
			}
			fifo = append(fifo, id)
			vers[id] = ver
		}
		next := 0
		fresh := func() uuid.UUID {
			next++
			return uuid.UUID{0xcc, 0xcc, 0, 0, 0, 0, 0x40, 0, 0x80, 0, 0, 0, byte(next >> 24), byte(next >> 16), byte(next >> 8), byte(next)}
		}
		var seen []uuid.UUID
		steps := rapid.IntRange(1, 40).Draw(rt, "steps")
		for s := 0; s < steps; s++ {
			switch rapid.SampledFrom([]string{"bulk", "remove", "remove", "delta", "delta", "delta"}).Draw(rt, "op") {
			case "bulk":
				// Many removals at once, sized around the capacity.
				n := rapid.SampledFrom([]int{1, 7, tombstoneCap - 2, tombstoneCap - 1, tombstoneCap, tombstoneCap + 1}).Draw(rt, "n")
				for i := 0; i < n; i++ {
					id := fresh()
					remove(id, rapid.IntRange(0, 3).Draw(rt, "ver"))
					if i < 3 || i >= n-3 {
						seen = append(seen, id)
					}
				}
			case "remove":
				id := fresh()
				if len(seen) > 0 && rapid.Bool().Draw(rt, "again") {
					id = rapid.SampledFrom(seen).Draw(rt, "id")
				}
				remove(id, rapid.IntRange(0, 5).Draw(rt, "ver"))
				seen = append(seen, id)
			case "delta":
				if len(seen) == 0 {
					continue
				}
				id := rapid.SampledFrom(seen).Draw(rt, "id")
				ver := rapid.IntRange(0, 6).Draw(rt, "ver")
				c.ApplyOpDelta(model.Operation{ID: id, MachineID: mid, Type: model.OpDeploy, Status: model.OpClaimed, Version: ver}, "")
				tombVer, tombstoned := vers[id]
				wantHeld := !tombstoned || ver > tombVer
				got := c.GetOperation(id.String()) != nil
				if got != wantHeld {
					rt.Fatalf("late op_delta v%d for a removed op (tombstoned=%v at v%d, %d tombstones): held=%v, want %v",
						ver, tombstoned, tombVer, len(fifo), got, wantHeld)
				}
				switch {
				case got && !tombstoned:
					pcStat.add("tombstone/resurrected_after_eviction")
				case got:
					pcStat.add("tombstone/accepted_above_tombstone")
				default:
					pcStat.add("tombstone/dropped")
				}
				if got {
					// Clear it again so the next probe starts from "removed".
					remove(id, ver)
				}
			}
		}
		pcStat.max("tombstone/max_model_size", len(fifo))
	})
	t.Logf("coverage:\n%s", pcStat.dump())
}
