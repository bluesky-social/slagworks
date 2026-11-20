package worker

// Property-test world for the site worker's actuation engine (Executor).
//
// The harness drives the REAL Cache and the REAL Executor.processOps of two
// HA site workers against:
//
//   - epHub: an independent, in-memory reference model of the global side
//     (operations table, claim CAS + idempotent re-claim, op_update and
//     observation handling, abort, expiry sweeps, op-event stream fan-out),
//     written from docs/dev/state-machine.md, not from the store code;
//   - fake power controllers / reconcilers that record every call and can
//     fail, take (virtual) time, or yield to "interrupts": other events of
//     the world that happen WHILE the executor is inside a claim RPC or a
//     BMC call (stream deliveries, aborts, sweeps, VIP loss, a crash);
//   - a transport model that mirrors what WSConn does with the Cache's
//     public outbox API (drain, requeue, ack, reload on reconnect).
//
// Everything is single-threaded and deterministic given the rapid draw
// stream, except for the map-iteration order of Cache.PendingPowerOps /
// PendingLifecycleOps (production code), which can reorder the ops of one
// tick. Properties are order-independent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// ---------------------------------------------------------------------------
// Tolerated violation classes
// ---------------------------------------------------------------------------

// epTolerated lists, by violation class, behavior the properties would
// otherwise reject and the executor does not currently guarantee. These are
// counted, not failed; every other class fails the case.
var epTolerated = map[string]string{
	"stall-claimed-not-driven":   "claim committed, reply lost, hub's claimed row lands after the local rollback: the op is never re-claimed (it dies in the orphaned-claim sweep)",
	"report-lost":                "a terminal or phase op_update is in memory and unacknowledged: it is lost on a restart or a dropped write, and the op stays claimed until a sweep fails it",
	"act-after-removed":          "the executor keeps actuating an op the worker already saw removed (abort or sweep mid-run)",
	"stale-list-claim":           "the executor claims an op whose machine left the view since the pass built its pending list, then fails it as not in cache",
	"orphan-sweep-midrun":        "the orphaned-claim sweep fails an op the worker is still actuating: no phase is recorded before or during actuation",
	"double-drive-two-executors": "two Executor instances of one worker (VIP lost and regained while the first is still inside a claim or a BMC call) both win the same op: the local claimed mark is erased by a hello op_resync or by the other executor's rollback, and the hub re-grants the claim to the same worker ID",
}

// ---------------------------------------------------------------------------
// Coverage counters (aggregated over all cases of one test run)
// ---------------------------------------------------------------------------

type epStats struct {
	mu sync.Mutex
	n  map[string]int64
}

var epStat = &epStats{n: map[string]int64{}}

func (s *epStats) add(key string, d int64) {
	s.mu.Lock()
	s.n[key] += d
	s.mu.Unlock()
}

func (s *epStats) max(key string, v int64) {
	s.mu.Lock()
	if v > s.n[key] {
		s.n[key] = v
	}
	s.mu.Unlock()
}

func (s *epStats) dump() string {
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
// World
// ---------------------------------------------------------------------------

// Virtual-time constants, from docs/dev/state-machine.md "Timeouts" and
// internal/api (orphaned-claim grace).
const (
	epClaimRPCTimeout  = 30
	epOrphanGrace      = 120
	epStuckTimeout     = 1800
	epPhoneHomeTimeout = 1800
	epPendingTimeout   = 86400
)

type epMachine struct {
	idx    int
	id     uuid.UUID
	bmcIP  string
	amt    bool
	bootOK bool   // controller implements power.BootManager
	serial string // NetBox serial == the BMC's true chassis serial
	mac    string

	// physical state (fake hardware)
	powerOn bool

	// hub durable state
	state         model.MachineState
	locked        bool
	inSite        bool
	hasCommission bool
}

// reportedSerial is what the fake BMC returns from ChassisSerial: the true
// serial in a different case with surrounding whitespace, as real FRU reads
// come back.
func (m *epMachine) reportedSerial() string {
	return "  " + strings.ToLower(m.serial) + "\n"
}

type epOpSpec struct {
	typ        model.OperationType
	mach       int
	transport  string // bmc | pdu | amt | both | nullpdu | nullamt | garbage
	serialMode string // match | fold | empty | mismatch
	bootDevice string // power ops: "" | pxe | disk | floppy
	device     string // set_boot_device
	persistent bool
	outlet     bool
	downstream string
	allowSelf  bool
	adminCred  bool   // bmc_reconcile: bmc_runtime carries username+password
	monitor    string // bmc_reconcile: none | stored | empty | rotate+stored
	expected   string // expected_chassis_serial stamped into bmc_runtime
}

func (s epOpSpec) lifecycle() bool {
	return s.typ == model.OpDeploy || s.typ == model.OpCommission
}

func (s epOpSpec) powerAction() bool {
	return s.typ == model.OpPowerOn || s.typ == model.OpPowerOff || s.typ == model.OpPowerCycle
}

type epCall struct {
	name     string
	arg      string
	issued   bool // entered with a live context (the command went out)
	err      error
	afterRem bool // worker had already applied op_removed for this op
}

type epAttempt struct {
	worker  int
	exec    int
	op      uuid.UUID
	outcome string // won | lost | err
	inView  bool   // machine was in the worker's view when the claim was sent

	factoryCalls   int
	factoryFault   string
	factoryArgs    string
	ctrls          []*epCtrl
	calls          []epCall
	reconcileCalls int
	reconcileRes   string
	timeCalls      int
	serialGot      string
	serialErr      bool

	agentEpoch int
	ended      bool
	updEnd     int
	obsEnd     int
	updBefore  int // op_updates for this op seen before the attempt
	obsBefore  int
	disturbed  bool // a snapshot was applied to this worker mid-attempt
	removedMid bool // op_removed for this very op was applied mid-attempt
	viewEpoch  int  // worker's view epoch when the claim was sent
	removedAt0 bool // op_removed already applied when the claim was sent
	crashed    bool
	checked    bool
}

type epOpFacts struct {
	id   uuid.UUID
	spec epOpSpec

	grants      int          // hub claim grants, re-claims included
	grantGen    int          // process generation of the claimant at its last grant
	wonAttempts int          // attempts that received a won reply
	actuations  int          // won attempts that issued >=1 power action / boot override
	removedSeen map[int]bool // worker idx -> op_removed applied
	everAwait   bool         // hub phase reached awaiting_phone_home
	// phoneHomeRecorded: a worker answered a phone_home report for this
	// deploy op with success and queued the durable observation.
	// phoneHomeAckedUnrecorded: one answered with success and queued nothing,
	// with no earlier recorded phone-home to justify that.
	phoneHomeRecorded, phoneHomeAckedUnrecorded bool
	reportLossy                                 bool         // a report for this op was dropped by an injected lossy fault
	hubFailedBy                                 string       // abort | orphan | stuck | phonehome | pending | worker | observation
	running                                     map[int]bool // worker idx -> currently inside a won attempt
}

type epWorld struct {
	t       *rapid.T
	quiet   bool
	depth   int
	now     int64
	mach    []*epMachine
	hub     *epHub
	wk      []*epWorker
	trace   []string
	byOp    map[uuid.UUID]*epOpFacts
	dir     string
	execSeq int
	persist bool

	interruptNames []string // swarm-enabled interrupt actions for this case

	attemptLog []*epAttempt
	deferred   []func() // hub-side events that land later (a late claim commit)
	agentEpoch int      // bumped by every agent/phone-home action
}

// pick draws an approximately uniform int in [0,n). rapid's own integer and
// SampledFrom generators are deliberately biased toward small values, which
// starves every fault mode that is not listed first; composing the choice
// from fair coin flips keeps the mix even and still shrinks toward 0.
func (w *epWorld) pick(n int, label string) int {
	if n <= 1 {
		return 0
	}
	nbits := bits.Len(uint(n-1)) + 3
	v := 0
	for i := 0; i < nbits; i++ {
		if rapid.Bool().Draw(w.t, label) {
			v |= 1 << i
		}
	}
	return v % n
}

func (w *epWorld) chance(num, den int, label string) bool {
	return w.pick(den, label) < num
}

func epChoose[T any](w *epWorld, xs []T, label string) T {
	return xs[w.pick(len(xs), label)]
}

func (w *epWorld) logf(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf("[t=%d] ", w.now)+fmt.Sprintf(format, args...))
}

func (w *epWorld) stat(key string) { epStat.add(key, 1) }

// violate reports a property violation. The classes in epTolerated are
// counted; any other fails the case.
func (w *epWorld) violate(class, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if _, ok := epTolerated[class]; ok {
		w.stat("violation." + class)
		w.logf("TOLERATED %s: %s", class, msg)
		return
	}
	w.t.Fatalf("PROPERTY VIOLATION [%s]: %s\n--- trace ---\n%s", class, msg, strings.Join(w.trace, "\n"))
}

func (w *epWorld) opName(id uuid.UUID) string {
	f := w.byOp[id]
	if f == nil {
		return id.String()[:8]
	}
	return fmt.Sprintf("%s(%s m%d %s)", id.String()[:4], f.spec.typ, f.spec.mach, f.spec.transport)
}

// ---------------------------------------------------------------------------
// Reference hub
// ---------------------------------------------------------------------------

const (
	epMsgSnapshot = iota
	epMsgDelta
	epMsgResync
	epMsgRemoved
)

type epMsg struct {
	kind   int
	op     model.Operation
	state  model.MachineState
	ops    []model.Operation
	states map[string]model.MachineState
	snap   *model.SiteSnapshot
}

type epHubOp struct {
	op        model.Operation
	createdAt int64
	claimedAt int64
	phaseAt   int64
}

type epHub struct {
	w       *epWorld
	ops     map[uuid.UUID]*epHubOp
	order   []uuid.UUID
	seq     int64
	members map[uuid.UUID]bool // membership of the slug's cached snapshot
	snap    *model.SiteSnapshot
}

func epTerminal(s model.OperationStatus) bool {
	return s == model.OpCompleted || s == model.OpFailed
}

func epLifecycle(t model.OperationType) bool {
	return t == model.OpDeploy || t == model.OpCommission
}

// transientFor is the machine state a claimed/running lifecycle op implies.
func epTransient(op model.Operation) model.MachineState {
	if op.Status != model.OpClaimed && op.Status != model.OpRunning {
		return ""
	}
	switch op.Type {
	case model.OpDeploy:
		return model.MachineStateDeploying
	case model.OpCommission:
		return model.MachineStateCommissioning
	default:
		return ""
	}
}

func (h *epHub) machineOf(op model.Operation) *epMachine {
	for _, m := range h.w.mach {
		if m.id == op.MachineID {
			return m
		}
	}
	return nil
}

func (h *epHub) setState(m *epMachine, target model.MachineState) {
	m.state = target
	switch target {
	case model.MachineStateRunning:
		m.locked = true
	case model.MachineStateFailed:
		m.locked = false
	default:
		// every other state leaves the lock as it is
	}
}

// buildSnapshot renders the site's identity snapshot from the machine pool.
func (h *epHub) buildSnapshot() *model.SiteSnapshot {
	h.seq++
	snap := &model.SiteSnapshot{SeqNum: h.seq}
	snap.OOB = model.OOBConfig{Subnet: "10.0.0.0/22", Gateway: "10.0.0.1", NTP: []string{"10.0.0.1"}}
	members := map[uuid.UUID]bool{}
	for _, m := range h.w.mach {
		if !m.inSite {
			continue
		}
		members[m.id] = true
		snap.Machines = append(snap.Machines, model.MachineSnapshot{
			ID:           m.id,
			NetBoxSerial: m.serial,
			Hostname:     fmt.Sprintf("m%d.pop", m.idx),
			BMCIP:        m.bmcIP,
			AMT:          m.amt,
			State:        m.state,
			Locked:       m.locked,
			PXEEligible:  true,
			Interfaces:   []model.SnapshotInterface{{Name: "eth0", MAC: m.mac, Type: "physical"}},
		})
	}
	h.members = members
	h.snap = snap
	return snap
}

// push rebuilds the snapshot and fans it out, followed (per op-ready conn)
// by a re-send of every active op of the site as an op_delta.
func (h *epHub) push() {
	snap := h.buildSnapshot()
	for _, pw := range h.w.wk {
		if !pw.connUp {
			continue
		}
		cp := *snap
		pw.inbox = append(pw.inbox, epMsg{kind: epMsgSnapshot, snap: &cp})
		for _, id := range h.order {
			rec := h.ops[id]
			if epTerminal(rec.op.Status) || !h.members[rec.op.MachineID] {
				continue
			}
			pw.inbox = append(pw.inbox, epMsg{kind: epMsgDelta, op: rec.op, state: epTransient(rec.op)})
		}
	}
}

func (h *epHub) publishDelta(op model.Operation, st model.MachineState) {
	if !h.members[op.MachineID] {
		return
	}
	for _, pw := range h.w.wk {
		if pw.connUp {
			pw.inbox = append(pw.inbox, epMsg{kind: epMsgDelta, op: op, state: st})
		}
	}
}

func (h *epHub) publishRemoved(op model.Operation, st model.MachineState) {
	for _, pw := range h.w.wk {
		if pw.connUp {
			pw.inbox = append(pw.inbox, epMsg{kind: epMsgRemoved, op: op, state: st})
		}
	}
}

// resyncFor is the hello op_resync: the site's active ops plus the transient
// state each claimed lifecycle op implies.
func (h *epHub) resyncFor() epMsg {
	msg := epMsg{kind: epMsgResync, states: map[string]model.MachineState{}}
	for _, id := range h.order {
		rec := h.ops[id]
		if epTerminal(rec.op.Status) || !h.members[rec.op.MachineID] {
			continue
		}
		msg.ops = append(msg.ops, rec.op)
		if st := epTransient(rec.op); st != "" {
			msg.states[rec.op.MachineID.String()] = st
		}
	}
	return msg
}

// create inserts a pending op, enforcing the creation gates: at most one
// active op per machine of ANY type (the operations table's
// idx_one_active_op_per_machine, pinned by
// store.TestCreatePowerOp_RejectedWhenLifecycleOpActive; the "power ops are
// always issuable alongside a lifecycle op" comments are stale), and no
// lifecycle op on a locked machine or from a disallowed state.
func (h *epHub) create(spec epOpSpec, params json.RawMessage) *epHubOp {
	m := h.w.mach[spec.mach]
	for _, id := range h.order {
		rec := h.ops[id]
		if !epTerminal(rec.op.Status) && rec.op.MachineID == m.id {
			return nil
		}
	}
	if spec.lifecycle() {
		if m.locked {
			return nil
		}
		if _, ok := epClaimSources(spec.typ)[m.state]; !ok {
			return nil
		}
	}
	rec := &epHubOp{createdAt: h.w.now}
	rec.op = model.Operation{
		ID:        uuid.New(),
		MachineID: m.id,
		TraceID:   uuid.New(),
		Type:      spec.typ,
		Status:    model.OpPending,
		Params:    params,
	}
	h.ops[rec.op.ID] = rec
	h.order = append(h.order, rec.op.ID)
	h.w.byOp[rec.op.ID] = &epOpFacts{id: rec.op.ID, spec: spec, removedSeen: map[int]bool{}, running: map[int]bool{}}
	h.publishDelta(rec.op, "")
	return rec
}

func epClaimSources(t model.OperationType) map[model.MachineState]bool {
	switch t {
	case model.OpCommission:
		return map[model.MachineState]bool{
			model.MachineStateNew: true, model.MachineStateCommissioned: true,
			model.MachineStateFailed: true, model.MachineStateRunning: true,
		}
	case model.OpDeploy:
		return map[model.MachineState]bool{
			model.MachineStateCommissioned: true, model.MachineStateFailed: true,
			model.MachineStateRunning: true,
		}
	default:
		// only lifecycle ops have claim preconditions on the machine state
		return nil
	}
}

// outletEligible is the claim-time self-power interlock for outlet ops.
func (h *epHub) outletEligible(spec epOpSpec, claimant *epWorker) bool {
	if spec.downstream == "" || claimant.serial == "" {
		return false
	}
	registered := false
	for _, pw := range h.w.wk {
		if pw.serial != "" && strings.EqualFold(pw.serial, spec.downstream) {
			registered = true
		}
	}
	if spec.typ == model.OpPowerOff && registered {
		return false
	}
	if strings.EqualFold(spec.downstream, claimant.serial) {
		return spec.allowSelf && (spec.typ == model.OpPowerCycle || spec.typ == model.OpPowerOn)
	}
	return true
}

// claim is the claim_op RPC: (op,nil) won, (nil,nil) lost/declined,
// (nil,err) refused by a claim precondition. gen is the process generation
// of the worker that SENT the request (a late commit can land after that
// process is gone).
func (h *epHub) claim(pw *epWorker, id uuid.UUID, gen int) (*model.Operation, error) {
	rec := h.ops[id]
	if rec == nil {
		return nil, nil
	}
	facts := h.w.byOp[id]
	if facts.spec.outlet && (facts.spec.transport == "pdu") && !h.outletEligible(facts.spec, pw) {
		h.w.stat("hub.claim.interlock_declined")
		return nil, nil
	}
	op := &rec.op
	if op.Status == model.OpClaimed && op.ClaimedBy == pw.id && op.Phase == "" {
		op.Version++
		rec.claimedAt = h.w.now
		facts.grants++
		facts.grantGen = gen
		h.w.stat("hub.claim.reclaim")
		h.publishDelta(*op, epTransient(*op))
		cp := *op
		return &cp, nil
	}
	if op.Status != model.OpPending {
		return nil, nil
	}
	m := h.machineOf(*op)
	if epLifecycle(op.Type) {
		if _, ok := epClaimSources(op.Type)[m.state]; !ok {
			return nil, fmt.Errorf("machine in state %q, cannot claim %s op", m.state, op.Type)
		}
		if m.locked {
			return nil, fmt.Errorf("machine %s is locked; cannot claim %s op", m.id, op.Type)
		}
	}
	op.Status = model.OpClaimed
	op.ClaimedBy = pw.id
	op.Version++
	rec.claimedAt = h.w.now
	facts.grants++
	facts.grantGen = gen
	if st := epTransient(*op); st != "" {
		m.state = st
	}
	h.publishDelta(*op, epTransient(*op))
	cp := *op
	return &cp, nil
}

// fail applies the FailOperation rules and publishes op_removed.
func (h *epHub) fail(rec *epHubOp, by string) {
	if epTerminal(rec.op.Status) {
		return
	}
	wasPending := rec.op.Status == model.OpPending
	rec.op.Status = model.OpFailed
	rec.op.Version++
	facts := h.w.byOp[rec.op.ID]
	facts.hubFailedBy = by
	var target model.MachineState
	if !wasPending && epLifecycle(rec.op.Type) {
		m := h.machineOf(rec.op)
		target = model.MachineStateFailed
		if rec.op.Type == model.OpCommission && !m.hasCommission {
			target = model.MachineStateNew
		}
		h.setState(m, target)
	}
	h.publishRemoved(rec.op, target)
	if target != "" {
		h.push()
	}
}

func (h *epHub) complete(rec *epHubOp, target model.MachineState) {
	if epTerminal(rec.op.Status) {
		return
	}
	rec.op.Status = model.OpCompleted
	rec.op.Version++
	if target != "" {
		h.setState(h.machineOf(rec.op), target)
	}
	h.publishRemoved(rec.op, target)
	if target != "" {
		h.push()
	}
}

// applyOpUpdate is the op_update handler: phase first, then status.
func (h *epHub) applyOpUpdate(u model.OperationUpdate) {
	id, err := uuid.Parse(u.OperationID)
	if err != nil {
		return
	}
	rec := h.ops[id]
	if rec == nil {
		return
	}
	facts := h.w.byOp[id]
	if u.Phase != "" && rec.op.Phase != u.Phase && !epTerminal(rec.op.Status) {
		// A phase write leaves a terminal row as it is; on an active one it
		// bumps the version and publishes the post-mutation row.
		rec.op.Phase = u.Phase
		rec.phaseAt = h.w.now
		rec.op.Version++
		if u.Phase == "awaiting_phone_home" {
			facts.everAwait = true
		}
		h.publishDelta(rec.op, "")
	}
	switch u.Status {
	case string(model.OpCompleted):
		if epTerminal(rec.op.Status) {
			return
		}
		var target model.MachineState
		switch rec.op.Type {
		case model.OpDeploy:
			target = model.MachineStateRunning
			if !facts.everAwait && !facts.reportLossy {
				h.w.violate("deploy-completed-early", "deploy op %s completed by op_update at phase %q (status %s) without ever reaching awaiting_phone_home",
					h.w.opName(id), rec.op.Phase, rec.op.Status)
			}
		case model.OpCommission:
			target = model.MachineStateCommissioned
		default:
			// a power-plane op's completion moves no machine state
		}
		h.complete(rec, target)
	case string(model.OpFailed):
		h.fail(rec, "worker")
	}
}

// applyObservation is the observation handler. Returns (err, terminal).
func (h *epHub) applyObservation(obs model.Observation) (error, bool) {
	switch obs.Event {
	case "commission_report", "deploy_complete", "bmc_reconcile_report":
	case "phone_home":
		if obs.OperationID == uuid.Nil {
			return nil, false // no deploy op behind it: acked, no transition
		}
	default:
		return nil, false
	}
	rec := h.ops[obs.OperationID]
	want := map[string]model.OperationType{
		"commission_report":    model.OpCommission,
		"deploy_complete":      model.OpDeploy,
		"phone_home":           model.OpDeploy,
		"bmc_reconcile_report": model.OpBMCReconcile,
	}[obs.Event]
	if rec == nil || rec.op.MachineID != obs.MachineID || rec.op.Type != want {
		return errors.New("operation binding"), true
	}
	switch obs.Event {
	case "deploy_complete":
		if rec.op.Status == model.OpFailed {
			return errors.New("operation is terminal"), true
		}
		return nil, false
	case "phone_home":
		// The durable carrier of a deploy's completion: whichever of it and
		// the completed op_update arrives first completes the op.
		if rec.op.Status == model.OpFailed {
			return errors.New("operation is terminal"), true
		}
		if rec.op.Status == model.OpCompleted {
			return nil, false
		}
		h.w.stat("hub.deploy_completed_by_phone_home_observation")
		h.complete(rec, model.MachineStateRunning)
		return nil, false
	case "commission_report":
		if rec.op.Status == model.OpCompleted {
			return nil, false
		}
		if rec.op.Status == model.OpFailed {
			return errors.New("operation is terminal"), true
		}
		h.machineOf(rec.op).hasCommission = true
		h.complete(rec, model.MachineStateCommissioned)
		return nil, false
	default: // bmc_reconcile_report
		if epTerminal(rec.op.Status) {
			return errors.New("operation is terminal"), true
		}
		var rep model.BMCReconcileReport
		if err := json.Unmarshal(obs.Data, &rep); err != nil {
			return err, true
		}
		failed := rep.Error != "" ||
			(rep.Monitor == nil) ||
			(rep.MonitorRotated && rep.Monitor.Password == "")
		if failed {
			h.fail(rec, "observation")
		} else {
			h.complete(rec, "")
		}
		return nil, false
	}
}

func (h *epHub) abort(m *epMachine) {
	for _, id := range h.order {
		rec := h.ops[id]
		if rec.op.MachineID == m.id && !epTerminal(rec.op.Status) {
			h.w.logf("hub: abort %s", h.w.opName(id))
			h.fail(rec, "abort")
		}
	}
}

// sweep runs one expiry tick in the documented order: pending, stuck,
// orphaned claims, phone-home.
func (h *epHub) sweep() {
	now := h.w.now
	for _, id := range h.order {
		rec := h.ops[id]
		if rec.op.Status == model.OpPending && rec.createdAt < now-epPendingTimeout {
			h.fail(rec, "pending")
		}
	}
	for _, id := range h.order {
		rec := h.ops[id]
		if rec.op.Status == model.OpClaimed && rec.claimedAt < now-epStuckTimeout && rec.op.Phase != "awaiting_phone_home" {
			h.w.logf("hub: stuck sweep fails %s", h.w.opName(id))
			h.fail(rec, "stuck")
		}
	}
	for _, id := range h.order {
		rec := h.ops[id]
		if rec.op.Status == model.OpClaimed && rec.op.Phase == "" && rec.claimedAt < now-epOrphanGrace {
			facts := h.w.byOp[id]
			for widx, on := range facts.running {
				if on {
					h.w.violate("orphan-sweep-midrun", "orphaned-claim sweep failed %s at claimed+%ds while worker %d is still actuating it (no phase recorded yet)",
						h.w.opName(id), now-rec.claimedAt, widx)
				}
			}
			h.w.logf("hub: orphan sweep fails %s", h.w.opName(id))
			h.w.stat("hub.sweep.orphan")
			h.fail(rec, "orphan")
		}
	}
	for _, id := range h.order {
		rec := h.ops[id]
		if rec.op.Status == model.OpClaimed && rec.op.Phase == "awaiting_phone_home" && rec.phaseAt < now-epPhoneHomeTimeout {
			h.w.logf("hub: phone-home sweep fails %s", h.w.opName(id))
			h.fail(rec, "phonehome")
		}
	}
}

// ---------------------------------------------------------------------------
// Worker (real Cache + real Executor, fake transport and hardware)
// ---------------------------------------------------------------------------

type epCrash struct{ worker int }

type epWorker struct {
	w      *epWorld
	idx    int
	id     string
	serial string
	dbPath string
	db     *LocalDB
	cache  *Cache

	gen    int  // process generation, bumped by every restart
	active bool // holds the VIP: an executor exists
	exec   *Executor
	execID int
	ctx    context.Context
	cancel context.CancelFunc

	connUp    bool
	connGen   int // bumped on every connect and disconnect
	viewEpoch int // bumped whenever a snapshot is applied to the cache
	inbox     []epMsg

	cur     *epAttempt
	inTick  int
	crashed bool
	// viewAtPass is the set of machines in the worker's view when the
	// current processOps pass started (when it built its pending lists).
	viewAtPass map[uuid.UUID]bool

	// transport-side report log, for the report oracles
	updSent map[uuid.UUID][]model.OperationUpdate
	obsSent map[uuid.UUID]int
}

func (pw *epWorker) openCache() {
	if pw.w.persist {
		db, err := OpenLocalDB(pw.dbPath)
		if err != nil {
			pw.w.t.Fatalf("open localdb: %v", err)
		}
		pw.db = db
	}
	pw.cache = NewCacheWithSerial(pw.db, pw.serial)
	// Rejection backoff runs on the wall clock; make it instantaneous so
	// the transport model never has to sleep.
	pw.cache.outbox.RejectBackoffBase = time.Nanosecond
	pw.cache.outbox.RejectBackoffMax = time.Nanosecond
}

func (pw *epWorker) closeDB() {
	if pw.db == nil {
		return
	}
	if sqlDB, err := pw.db.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	pw.db = nil
}

func (pw *epWorker) startExecutor() {
	pw.w.execSeq++
	pw.execID = pw.w.execSeq
	pw.ctx, pw.cancel = context.WithCancel(context.Background())
	pw.exec = NewExecutor(ExecutorConfig{
		Cache:        pw.cache,
		Factory:      pw.factory,
		BMCReconcile: pw.bmcReconcile,
		BMCTime:      pw.bmcTime,
		Claimer:      pw,
		WorkerID:     pw.id,
	})
	pw.exec.SetLogger(testDiscardLogger())
}

func (pw *epWorker) acquireVIP() {
	if pw.active {
		return
	}
	pw.active = true
	pw.startExecutor()
	pw.w.logf("w%d: VIP acquired (executor #%d)", pw.idx, pw.execID)
}

func (pw *epWorker) releaseVIP() {
	if !pw.active {
		return
	}
	pw.active = false
	pw.cancel()
	pw.w.logf("w%d: VIP released (executor #%d cancelled)", pw.idx, pw.execID)
}

// untrusted peeks the op layer's rehydrated-not-resynced flag.
func (pw *epWorker) untrusted() bool {
	pw.cache.opState.mu.RLock()
	defer pw.cache.opState.mu.RUnlock()
	return pw.cache.opState.untrusted
}

// queuedUpdates / queuedObs peek the outbox without draining it.
func (pw *epWorker) queuedUpdates() []model.OperationUpdate {
	pw.cache.outbox.mu.Lock()
	defer pw.cache.outbox.mu.Unlock()
	return append([]model.OperationUpdate(nil), pw.cache.outbox.opUpdates...)
}

func (pw *epWorker) queuedObs() []PendingOutbox {
	pw.cache.outbox.mu.Lock()
	defer pw.cache.outbox.mu.Unlock()
	return append([]PendingOutbox(nil), pw.cache.outbox.pending...)
}

// updates returns every op_update this worker process has produced for id,
// sent or still queued, in order.
func (pw *epWorker) updates(id uuid.UUID) []model.OperationUpdate {
	out := append([]model.OperationUpdate(nil), pw.updSent[id]...)
	for _, u := range pw.queuedUpdates() {
		if u.OperationID == id.String() {
			out = append(out, u)
		}
	}
	return out
}

func (pw *epWorker) obsCount(id uuid.UUID) int {
	n := pw.obsSent[id]
	for _, p := range pw.queuedObs() {
		if p.Obs.OperationID == id {
			n++
		}
	}
	return n
}

// connect is the WS (re)connect: hello, snapshot-or-no_change applied
// before the connection counts as up, outbox reload, then the op_resync
// queued on the stream.
func (pw *epWorker) connect() {
	if pw.connUp {
		return
	}
	h := pw.w.hub
	if h.snap == nil {
		h.buildSnapshot()
	}
	if pw.cache.HelloSeqNum() != h.seq {
		cp := *h.snap
		pw.cache.ApplySnapshot(&cp)
	}
	pw.connUp = true
	pw.connGen++
	pw.inbox = nil
	pw.viewEpoch++ // the hello snapshot may have changed the view
	pw.cache.ReloadOutboxFromDisk()
	pw.inbox = append(pw.inbox, h.resyncFor())
	pw.w.logf("w%d: connected (resync queued)", pw.idx)
	pw.w.stat("conn.connect")
}

func (pw *epWorker) disconnect() {
	if !pw.connUp {
		return
	}
	pw.connUp = false
	pw.connGen++
	pw.inbox = nil
	pw.w.logf("w%d: disconnected", pw.idx)
	pw.w.stat("conn.disconnect")
}

// deliver applies inbox message i to the cache. A resync is a barrier: the
// hub writes it under the per-connection op-stream lock before any later
// publish, so nothing queued after it may overtake it.
func (pw *epWorker) deliver(i int) {
	if i < 0 || i >= len(pw.inbox) {
		return
	}
	for j := 0; j < i; j++ {
		if pw.inbox[j].kind == epMsgResync {
			i = j
			break
		}
	}
	msg := pw.inbox[i]
	pw.inbox = append(pw.inbox[:i:i], pw.inbox[i+1:]...)
	w := pw.w
	switch msg.kind {
	case epMsgSnapshot:
		w.stat("stream.snapshot")
		w.logf("w%d <- snapshot seq=%d (%d machines)", pw.idx, msg.snap.SeqNum, len(msg.snap.Machines))
		pw.cache.ApplySnapshot(msg.snap)
		pw.viewEpoch++
	case epMsgDelta:
		w.stat("stream.delta")
		w.logf("w%d <- op_delta %s status=%s phase=%q v%d", pw.idx, w.opName(msg.op.ID), msg.op.Status, msg.op.Phase, msg.op.Version)
		pw.cache.ApplyOpDelta(msg.op, msg.state)
	case epMsgResync:
		w.stat("stream.resync")
		w.logf("w%d <- op_resync (%d ops)", pw.idx, len(msg.ops))
		pw.cache.ApplyOpResync(append([]model.Operation(nil), msg.ops...), msg.states)
	case epMsgRemoved:
		w.stat("stream.removed")
		w.logf("w%d <- op_removed %s v%d state=%q", pw.idx, w.opName(msg.op.ID), msg.op.Version, msg.state)
		pw.cache.ApplyOpRemoved(msg.op.ID.String(), msg.op.MachineID.String(), msg.op.Version, msg.state)
		w.byOp[msg.op.ID].removedSeen[pw.idx] = true
	}
}

func (pw *epWorker) deliverAll() {
	for len(pw.inbox) > 0 {
		pw.deliver(0)
	}
}

// drain mirrors WSConn.drainOnce over the Cache's public outbox API.
//
// fault:
//
//	""           everything is delivered and acked
//	"lostwrite"  the batch is written into a dying connection: nothing
//	             reaches the hub, nothing is acked, the connection drops
//	             (observations stay on disk; op_updates are simply gone)
//	"hubdown"    the hub's handlers error: op_updates are logged and
//	             dropped (fire-and-forget), observations are rejected
//	             non-terminally
//	"obsreject"  observations are rejected non-terminally; op_updates land
//	"acklost"    the hub processes the first observation, its ack never
//	             arrives and the connection drops
func (pw *epWorker) drain(fault string) {
	w := pw.w
	obs, updates := pw.cache.DrainOutbox()
	if !pw.connUp {
		pw.cache.RequeueOutbox(obs, updates)
		return
	}
	if fault != "" && (len(obs) > 0 || len(updates) > 0) {
		w.stat("drain.fault." + fault)
	}
	for i, p := range obs {
		pw.obsSent[p.Obs.OperationID]++
		switch fault {
		case "lostwrite":
			w.stat("drain.obs_lost_in_flight")
			w.logf("w%d -> observation %s op=%s LOST IN FLIGHT", pw.idx, p.Obs.Event, w.opName(p.Obs.OperationID))
			continue
		case "obsreject", "hubdown":
			w.stat("drain.obs_rejected")
			w.logf("w%d -> observation %s op=%s rejected (transient)", pw.idx, p.Obs.Event, w.opName(p.Obs.OperationID))
			pw.cache.RequeueRejected(p)
			continue
		case "acklost":
			w.stat("drain.obs_acklost")
			w.logf("w%d -> observation %s op=%s processed, ack lost", pw.idx, p.Obs.Event, w.opName(p.Obs.OperationID))
			_, _ = w.hub.applyObservation(p.Obs)
			pw.cache.RequeueOutbox(obs[i+1:], updates)
			pw.disconnect()
			return
		}
		w.logf("w%d -> observation %s op=%s", pw.idx, p.Obs.Event, w.opName(p.Obs.OperationID))
		w.stat("drain.obs")
		if err, terminal := w.hub.applyObservation(p.Obs); err != nil && !terminal {
			pw.cache.RequeueRejected(p)
		} else {
			pw.cache.AckObservation(p.RowID)
		}
	}
	for _, u := range updates {
		id, _ := uuid.Parse(u.OperationID)
		pw.updSent[id] = append(pw.updSent[id], u)
		if fault == "lostwrite" || fault == "hubdown" {
			w.stat("drain.update_dropped")
			w.logf("w%d -> op_update %s status=%q phase=%q DROPPED (%s)", pw.idx, w.opName(id), u.Status, u.Phase, fault)
			if f := w.byOp[id]; f != nil {
				f.reportLossy = true
			}
			continue
		}
		w.logf("w%d -> op_update %s status=%q phase=%q", pw.idx, w.opName(id), u.Status, u.Phase)
		w.stat("drain.update")
		w.hub.applyOpUpdate(u)
	}
	if fault == "lostwrite" {
		pw.disconnect()
	}
}

// restart replaces the worker process: in-memory state is gone, LocalDB
// survives. The op layer rehydrates untrusted; the connection is down.
func (pw *epWorker) restart(why string) {
	w := pw.w
	w.logf("w%d: RESTART (%s)", pw.idx, why)
	w.stat("worker.restart." + why)
	for _, u := range pw.queuedUpdates() {
		if id, err := uuid.Parse(u.OperationID); err == nil {
			if f := w.byOp[id]; f != nil {
				f.reportLossy = true
				w.stat("worker.restart.updates_lost")
			}
		}
	}
	if pw.cancel != nil {
		pw.cancel()
	}
	pw.closeDB()
	pw.gen++
	pw.connUp = false
	pw.connGen++
	pw.inbox = nil
	pw.cur = nil
	pw.updSent = map[uuid.UUID][]model.OperationUpdate{}
	pw.obsSent = map[uuid.UUID]int{}
	pw.openCache()
	if pw.active {
		pw.startExecutor()
	}
}

// endAttempt closes an attempt's report window: executeOp has returned for
// it (the executor moved on to the next op, or the pass ended).
func (pw *epWorker) endAttempt(a *epAttempt) {
	if a.ended {
		return
	}
	a.ended = true
	a.updEnd = len(pw.updates(a.op))
	a.obsEnd = pw.obsCount(a.op)
	// Anything applied to this worker's cache while the attempt was on the
	// stack counts, including what a nested pass of another executor let in.
	a.disturbed = a.disturbed || pw.viewEpoch != a.viewEpoch
	if f := pw.w.byOp[a.op]; f != nil {
		a.removedMid = f.removedSeen[pw.idx] && !a.removedAt0
		f.running[pw.idx] = false
	}
}

// tick runs one Executor.processOps pass and validates every claim attempt
// it made. A fake call may "crash" the worker by panicking with epCrash;
// the process is then replaced, exactly as a kill -9 mid-op would.
func (pw *epWorker) tick() {
	if !pw.active || pw.exec == nil {
		return
	}
	w := pw.w
	w.stat("tick")
	first := len(w.attemptLog)
	saved, savedView := pw.cur, pw.viewAtPass
	pw.viewAtPass = map[uuid.UUID]bool{}
	for _, m := range w.mach {
		if pw.cache.MachineByID(m.id) != nil {
			pw.viewAtPass[m.id] = true
		}
	}
	exec, ctx, exec0 := pw.exec, pw.ctx, pw.execID
	crashed := false
	pw.inTick++
	func() {
		defer func() {
			if r := recover(); r != nil {
				if c, ok := r.(epCrash); ok && c.worker == pw.idx {
					crashed = true
					return
				}
				panic(r)
			}
		}()
		exec.processOps(ctx)
	}()
	pw.inTick--
	mine := []*epAttempt{}
	for _, a := range w.attemptLog[first:] {
		if a.worker == pw.idx && a.exec == exec0 && !a.checked {
			mine = append(mine, a)
		}
	}
	for _, a := range mine {
		if crashed {
			if f := w.byOp[a.op]; f != nil {
				f.running[pw.idx] = false
			}
			continue
		}
		pw.endAttempt(a)
	}
	pw.cur, pw.viewAtPass = saved, savedView
	if crashed {
		for _, a := range mine {
			a.crashed = true
			a.checked = true
		}
		if pw.inTick > 0 {
			// A nested pass of this same process (a second executor after
			// a VIP flap): the whole process is gone, unwind the outer
			// pass too.
			panic(epCrash{worker: pw.idx})
		}
		pw.crashed = false
		pw.restart("crash")
		return
	}
	for _, a := range mine {
		w.checkAttempt(pw, a)
		a.checked = true
	}
}

// ---------------------------------------------------------------------------
// Fake claimer
// ---------------------------------------------------------------------------

var epClaimModes = []string{"ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "lostreq", "late-commit", "lostreply-drop", "cancel", "cancel-committed", "crash", "crash-committed"}

func (pw *epWorker) ClaimOp(ctx context.Context, id uuid.UUID) (*model.Operation, error) {
	w := pw.w
	facts := w.byOp[id]
	if prev := pw.cur; prev != nil && prev.exec == pw.execID {
		pw.endAttempt(prev)
	}
	a := &epAttempt{worker: pw.idx, exec: pw.execID, op: id, outcome: "err", agentEpoch: w.agentEpoch}
	w.attemptLog = append(w.attemptLog, a)
	pw.cur = a
	if facts == nil {
		w.t.Fatalf("executor claimed an op the hub never created: %s", id)
	}
	m := w.mach[facts.spec.mach]
	a.inView = pw.cache.MachineByID(m.id) != nil
	a.viewEpoch = pw.viewEpoch
	a.removedAt0 = facts.removedSeen[pw.idx]
	a.updBefore = len(pw.updates(id))
	a.obsBefore = pw.obsCount(id)

	if pw.untrusted() {
		w.violate("untrusted-claim", "w%d claimed %s while its op layer is rehydrated-but-not-resynced", pw.idx, w.opName(id))
	}
	if ctx.Err() != nil {
		w.violate("claim-after-cancel", "w%d executor #%d sent a claim for %s after its context was cancelled (standby / shutting down)", pw.idx, pw.execID, w.opName(id))
	}
	if !a.inView {
		if pw.viewAtPass[m.id] {
			w.violate("stale-list-claim", "w%d claimed %s whose machine m%d left its view after this pass built its pending list", pw.idx, w.opName(id), m.idx)
		} else {
			w.violate("out-of-view-claim", "w%d claimed %s whose machine m%d is not in its view and was not when the pass started", pw.idx, w.opName(id), m.idx)
		}
	}
	s := facts.spec
	selfServed := s.allowSelf && (s.typ == model.OpPowerCycle || s.typ == model.OpPowerOn)
	if s.outlet && pw.serial != "" && strings.EqualFold(s.downstream, pw.serial) && !selfServed {
		w.violate("self-feed-claim", "w%d (serial %q) claimed outlet op %s that cuts its own feed (downstream %q, allow_self=%v)",
			pw.idx, pw.serial, w.opName(id), s.downstream, s.allowSelf)
	}

	if !pw.connUp {
		w.stat("claim.notconnected")
		w.logf("w%d: claim %s -> ws not connected", pw.idx, w.opName(id))
		return nil, errors.New("ws not connected")
	}
	mode := "ok"
	if !w.quiet {
		mode = epChoose(w, epClaimModes, "claimMode")
		w.stat("claim.mode." + mode)
	} else {
		w.stat("settle.claim")
	}
	switch mode {
	case "lostreq":
		// The request never reached the hub.
		w.logf("w%d: claim %s -> request lost, RPC timeout", pw.idx, w.opName(id))
		w.now += epClaimRPCTimeout
		w.interrupt(pw, "claimwait")
		return nil, errors.New("claim_op response: RPC timeout")
	case "late-commit":
		// The hub is slow: the worker's 30s RPC deadline fires first, the
		// claim commits afterwards (its reply goes to a request nobody is
		// waiting on any more).
		w.logf("w%d: claim %s -> RPC timeout; the hub will commit it late", pw.idx, w.opName(id))
		sentBy := pw.gen
		w.deferred = append(w.deferred, func() {
			op, _ := w.hub.claim(pw, id, sentBy)
			w.logf("hub: late commit of w%d's timed-out claim on %s (granted=%v)", pw.idx, w.opName(id), op != nil)
			if op != nil {
				w.stat("claim.committed_reply_lost")
			}
		})
		w.now += epClaimRPCTimeout
		return nil, errors.New("claim_op response: RPC timeout")
	case "lostreply-drop":
		// The connection died after the hub read the request: the claim
		// commits, neither the reply nor the claimed row reaches the worker.
		pw.disconnect()
		op, _ := w.hub.claim(pw, id, pw.gen)
		w.logf("w%d: claim %s -> connection lost, hub committed=%v, RPC timeout", pw.idx, w.opName(id), op != nil)
		if op != nil {
			w.stat("claim.committed_reply_lost")
		}
		w.now += epClaimRPCTimeout
		w.interrupt(pw, "claimwait")
		return nil, errors.New("claim_op response: RPC timeout")
	case "cancel", "cancel-committed":
		if mode == "cancel-committed" {
			_, _ = w.hub.claim(pw, id, pw.gen)
		}
		w.logf("w%d: claim %s -> context cancelled while waiting (VIP lost)", pw.idx, w.opName(id))
		pw.releaseVIP()
		return nil, fmt.Errorf("claim_op response: %w", context.Canceled)
	case "crash", "crash-committed":
		if mode == "crash-committed" {
			_, _ = w.hub.claim(pw, id, pw.gen)
		}
		w.logf("w%d: CRASH inside claim of %s", pw.idx, w.opName(id))
		pw.crashed = true
		panic(epCrash{worker: pw.idx})
	}
	// The reply travels on the same ordered connection as the op stream and
	// is handled by the same read loop: everything the hub wrote before it
	// read this claim is applied before the executor sees the reply. (The
	// hello op_resync in particular is always ahead of any claim reply; a
	// reply overtaking it would let a stale full-replace undo a won claim.)
	conn := pw.connGen
	for prior := len(pw.inbox); prior > 0 && len(pw.inbox) > 0; prior-- {
		pw.deliver(0)
	}
	op, err := w.hub.claim(pw, id, pw.gen)
	// The hub queues the claimed row on the connection's outbound writer and
	// writes the reply directly, so the row may reach the worker before or
	// after the reply.
	if w.quiet || w.chance(1, 2, "claimDeltaFirst") {
		pw.deliverAll()
	}
	w.interrupt(pw, "claim")
	if pw.connGen != conn || pw.crashed {
		// The connection the claim went out on is gone: its reply with it.
		w.logf("w%d: claim %s -> connection replaced while waiting (hub committed=%v), RPC timeout", pw.idx, w.opName(id), op != nil)
		w.stat("claim.reply_lost_conn_replaced")
		if op != nil {
			w.stat("claim.committed_reply_lost")
		}
		w.now += epClaimRPCTimeout
		return nil, errors.New("claim_op response: RPC timeout")
	}
	switch {
	case err != nil:
		w.logf("w%d: claim %s -> hub error: %v", pw.idx, w.opName(id), err)
		w.stat("claim.huberror")
		return nil, fmt.Errorf("hub rejected claim: %w", err)
	case op == nil:
		a.outcome = "lost"
		w.logf("w%d: claim %s -> lost", pw.idx, w.opName(id))
		w.stat("claim.lost")
		return nil, nil
	}
	a.outcome = "won"
	facts.wonAttempts++
	facts.running[pw.idx] = true
	w.logf("w%d: claim %s -> WON (grant #%d)", pw.idx, w.opName(id), facts.grants)
	w.stat("claim.won")
	return op, nil
}

// ---------------------------------------------------------------------------
// Fake hardware
// ---------------------------------------------------------------------------

var epCallDurations = []int64{0, 0, 1, 5, 29}

func (pw *epWorker) attemptForCall(what string) *epAttempt {
	a := pw.cur
	if a == nil || a.outcome != "won" {
		pw.w.violate("actuate-without-claim", "w%d reached %s with no won claim in progress (attempt=%+v)", pw.idx, what, a)
	}
	return a
}

func (pw *epWorker) factory(ctx context.Context, m *model.MachineSnapshot, rt model.BMCRuntime, pduRT *model.PDURuntime, amtRT *model.AMTRuntime) (power.Controller, error) {
	w := pw.w
	a := pw.attemptForCall("the controller factory")
	if a == nil {
		return nil, errors.New("ep: no attempt")
	}
	a.factoryCalls++
	a.factoryArgs = fmt.Sprintf("pdu=%v amt=%v expected=%q", pduRT != nil, amtRT != nil, rt.ExpectedChassisSerial)
	var pm *epMachine
	for _, cand := range w.mach {
		if cand.id == m.ID {
			pm = cand
		}
	}
	if pm == nil || pm.id != w.mach[w.byOp[a.op].spec.mach].id {
		w.t.Fatalf("factory called for machine %s, op %s targets m%d", m.ID, w.opName(a.op), w.byOp[a.op].spec.mach)
	}
	fault := "ok"
	if !w.quiet {
		fault = epChoose(w, []string{"ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "err", "nil"}, "factory")
	}
	a.factoryFault = fault
	if !w.quiet {
		w.stat("factory." + fault)
	}
	switch fault {
	case "err":
		return nil, errors.New("ep: injected connect failure")
	case "nil":
		return nil, nil
	}
	kind := "bmc"
	switch {
	case pduRT != nil:
		kind = "pdu"
	case amtRT != nil:
		kind = "amt"
	}
	c := &epCtrl{pw: pw, a: a, m: pm, kind: kind}
	a.ctrls = append(a.ctrls, c)
	if pm.bootOK && kind == "bmc" {
		return &epBootCtrl{epCtrl: c}, nil
	}
	return c, nil
}

type epCtrl struct {
	pw     *epWorker
	a      *epAttempt
	m      *epMachine
	kind   string
	closed int
}

type epBootCtrl struct{ *epCtrl }

// call is the common prelude of every controller method: attribution, the
// crashed/cancelled short-circuits, virtual duration, an interrupt window
// and an injected failure. It returns the call record (already appended)
// so the method can apply its physical effect when the call succeeded.
func (c *epCtrl) call(ctx context.Context, name, arg string, mutating bool) *epCall {
	pw, w := c.pw, c.pw.w
	if pw.crashed {
		return &epCall{name: name, err: errors.New("ep: process is gone")}
	}
	if pw.cur != c.a {
		w.violate("controller-reuse", "w%d called %s on a controller built for another attempt", pw.idx, name)
	}
	rec := epCall{name: name, arg: arg}
	facts := w.byOp[c.a.op]
	if ctx.Err() != nil {
		rec.err = ctx.Err()
		c.a.calls = append(c.a.calls, rec)
		w.stat("ctrl.cancelled")
		return &c.a.calls[len(c.a.calls)-1]
	}
	rec.issued = true
	rec.afterRem = facts.removedSeen[pw.idx]
	if mutating && rec.afterRem {
		w.violate("act-after-removed", "w%d issued %s(%s) for %s after it had applied op_removed for that op (hub failed it by %s)",
			pw.idx, name, arg, w.opName(c.a.op), facts.hubFailedBy)
	}
	if !w.quiet {
		w.now += epChoose(w, epCallDurations, "dur")
		if w.chance(1, 40, "crash?") {
			w.logf("w%d: CRASH inside %s(%s) of %s", pw.idx, name, arg, w.opName(c.a.op))
			c.a.calls = append(c.a.calls, rec)
			pw.crashed = true
			panic(epCrash{worker: pw.idx})
		}
		w.interrupt(pw, name)
		if w.chance(1, 8, "fail?") {
			rec.err = fmt.Errorf("ep: injected %s failure", name)
		}
	}
	if rec.err == nil && ctx.Err() != nil {
		// Cancelled while the call was in flight: the command went out,
		// the caller sees the context error.
		rec.err = ctx.Err()
	}
	if w.quiet {
		w.stat("settle.ctrl." + name)
	} else {
		w.stat("ctrl." + name)
		if rec.err != nil {
			w.stat("ctrl.failed." + name)
		}
	}
	w.logf("w%d: %s(%s) on m%d for %s -> err=%v", pw.idx, name, arg, c.m.idx, w.opName(c.a.op), rec.err)
	c.a.calls = append(c.a.calls, rec)
	return &c.a.calls[len(c.a.calls)-1]
}

func (c *epCtrl) PowerOn(ctx context.Context) error {
	r := c.call(ctx, "PowerOn", "", true)
	if r.issued {
		c.m.powerOn = true
	}
	return r.err
}

func (c *epCtrl) PowerOff(ctx context.Context) error {
	r := c.call(ctx, "PowerOff", "", true)
	if r.issued {
		c.m.powerOn = false
	}
	return r.err
}

func (c *epCtrl) PowerCycle(ctx context.Context) error {
	r := c.call(ctx, "PowerCycle", "", true)
	if r.issued {
		c.m.powerOn = true
	}
	return r.err
}

func (c *epCtrl) PowerStatus(ctx context.Context) (power.State, error) {
	r := c.call(ctx, "PowerStatus", "", false)
	if r.err != nil {
		return power.StateUnknown, r.err
	}
	if c.m.powerOn {
		r.arg = "on"
		return power.StateOn, nil
	}
	r.arg = "off"
	return power.StateOff, nil
}

func (c *epCtrl) Transport() string { return c.kind }

func (c *epCtrl) SetBootDevice(ctx context.Context, dev power.BootDevice) error {
	return c.call(ctx, "SetBootDevice", string(dev), true).err
}

func (c *epCtrl) ChassisSerial(ctx context.Context) (string, error) {
	r := c.call(ctx, "ChassisSerial", "", false)
	if r.err != nil {
		c.a.serialErr = true
		return "", r.err
	}
	got := c.m.reportedSerial()
	if !c.pw.w.quiet && c.pw.w.chance(1, 8, "blankSerial?") {
		got = " \t"
	}
	c.a.serialGot = got
	r.arg = got
	return got, nil
}

func (c *epCtrl) Close(context.Context) error {
	c.closed++
	return nil
}

func (c *epBootCtrl) BootStatus(context.Context) (power.BootOverride, error) {
	return power.BootOverride{}, nil
}

func (c *epBootCtrl) SetBootOverride(ctx context.Context, dev power.BootDevice, persistent bool) error {
	return c.call(ctx, "SetBootOverride", fmt.Sprintf("%s/%v", dev, persistent), true).err
}

var epReconcileOutcomes = []string{"rotated", "rotated", "verified", "reasserted", "nil-err", "nil-nil", "partial"}

func (pw *epWorker) bmcReconcile(ctx context.Context, t bmcreconcile.LANTarget, monitorPassword string) (*bmcreconcile.Result, error) {
	w := pw.w
	a := pw.attemptForCall("the BMC reconciler")
	if a == nil {
		return nil, errors.New("ep: no attempt")
	}
	a.reconcileCalls++
	facts := w.byOp[a.op]
	m := w.mach[facts.spec.mach]
	if facts.removedSeen[pw.idx] {
		w.violate("act-after-removed", "w%d ran the BMC account reconcile for %s after it had applied op_removed for that op", pw.idx, w.opName(a.op))
	}
	if want := strings.Split(m.bmcIP, ":")[0]; t.Host != want {
		w.t.Fatalf("bmc reconcile dialed %q, machine BMC is %q", t.Host, want)
	}
	if ctx.Err() != nil {
		a.reconcileRes = "nil-err"
		return nil, ctx.Err()
	}
	out := "rotated"
	if !w.quiet {
		out = epChoose(w, epReconcileOutcomes, "reconcile")
		w.now += epChoose(w, []int64{0, 5, 60, 240}, "reconcileDur")
		w.interrupt(pw, "reconcile")
	}
	if monitorPassword != "" && out == "rotated" {
		out = "reasserted"
	}
	a.reconcileRes = out
	w.stat("reconcile." + out)
	w.logf("w%d: bmc reconcile for %s -> %s", pw.idx, w.opName(a.op), out)
	switch out {
	case "rotated":
		return &bmcreconcile.Result{CreatedOrUpdated: []bmcreconcile.ProvisionedUser{{Slot: 4, Name: bmcreconcile.MonitorUser, Password: "Fresh!pw1"}}}, nil
	case "reasserted":
		return &bmcreconcile.Result{CreatedOrUpdated: []bmcreconcile.ProvisionedUser{{Slot: 4, Name: bmcreconcile.MonitorUser}}}, nil
	case "verified":
		return &bmcreconcile.Result{PasswordVerified: []bmcreconcile.ProvisionedUser{{Slot: 4, Name: bmcreconcile.MonitorUser}}}, nil
	case "partial":
		return &bmcreconcile.Result{CreatedOrUpdated: []bmcreconcile.ProvisionedUser{{Slot: 4, Name: bmcreconcile.MonitorUser, Password: "Fresh!pw1"}}}, errors.New("ep: disable leftover failed")
	case "nil-err":
		return nil, errors.New("ep: list users failed")
	default:
		return nil, nil
	}
}

func (pw *epWorker) bmcTime(_ context.Context, _ bmcreconcile.TimeTarget) bmcreconcile.TimeResult {
	if a := pw.attemptForCall("the BMC time pass"); a != nil {
		a.timeCalls++
	}
	return bmcreconcile.TimeResult{NTPServers: []string{"10.0.0.1"}}
}
