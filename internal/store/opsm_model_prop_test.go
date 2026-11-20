package store

// Reference model for the operation/machine state machine.
//
// This file is the oracle half of the op/machine property harness. It is a
// pure-Go restatement of docs/dev/state-machine.md plus the doc comments on
// the store methods, written without calling internal/opstate or any store
// helper: every rule below is spelled out as a literal table or branch so
// the model and the production code cannot agree by sharing a function.
// Strings, not model.* constants, on purpose.

import (
	"bytes"
	"encoding/json"
	"math/big"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
)

type refMachine struct {
	id           uuid.UUID
	serial       string
	deviceID     int // 0 = unbound
	state        string
	locked       bool
	pxe          bool
	comm         string // canonical commission JSON; "" = never commissioned
	commissioned bool   // commissioned_at set
	desiredOS    string
}

type refEvent struct {
	Msg       string
	Status    string
	ClaimedBy string
	Err       string
}

type refOp struct {
	id        uuid.UUID
	machine   uuid.UUID
	typ       string
	status    string
	phase     string
	version   int
	claimedBy string
	claimed   bool   // claimed_at set
	completed bool   // completed_at set
	result    string // canonical JSON; "" = NULL
	params    string // canonical JSON; "" = NULL

	// Sweep clocks. "old" means the timestamp was backdated past the
	// sweep horizon and has not been rewritten since.
	createdOld, claimedOld, phaseOld bool
	// Generation counters: bumped whenever the corresponding timestamp
	// column is expected to change. The harness checks the real column
	// moved exactly when the generation did.
	createdGen, claimedGen, phaseGen int

	events []refEvent
}

type refModel struct {
	machines map[uuid.UUID]*refMachine
	ops      map[uuid.UUID]*refOp
	cov      map[string]int
}

func newRefModel() *refModel {
	return &refModel{
		machines: map[uuid.UUID]*refMachine{},
		ops:      map[uuid.UUID]*refOp{},
		cov:      map[string]int{},
	}
}

func (r *refModel) clone() *refModel {
	c := newRefModel()
	for id, m := range r.machines {
		cp := *m
		c.machines[id] = &cp
	}
	for id, o := range r.ops {
		cp := *o
		cp.events = append([]refEvent(nil), o.events...)
		c.ops[id] = &cp
	}
	// Coverage is intentionally not carried into clones: clones are used
	// for what-if replays (linearizability orders), not for counting.
	return c
}

func (r *refModel) hit(tag string) { r.cov[tag]++ }

// --- observable results ----------------------------------------------------

// opsmOpView is the part of a returned operation row the harness compares.
type opsmOpView struct {
	ID        uuid.UUID
	Status    string
	Phase     string
	ClaimedBy string
	Version   int
}

// opsmObs is the normalized observable result of one store call. Both the model
// and the real-store adapter produce one; they must be equal.
type opsmObs struct {
	Err     string       // "", "error", "binding", "notfound"
	Nil     bool         // the call returned a nil op / nil outcome
	Ops     []opsmOpView // returned op rows (multi-row results sorted by ID)
	MStates []string     // outcome machine states, aligned with Ops
	Changed bool         // PhaseTransition.Changed
	Prev    string       // PhaseTransition.PrevPhase
	Already bool         // alreadyCompleted
	Deleted bool         // DeleteMachineIfIdle reported the machine gone
	N       int64        // row counts
	MState  string       // machine state of a returned machine row
	MLocked bool         // lock of a returned machine row
}

func (o *refOp) view() opsmOpView {
	return opsmOpView{ID: o.id, Status: o.status, Phase: o.phase, ClaimedBy: o.claimedBy, Version: o.version}
}

// --- literal contract tables (docs/dev/state-machine.md) --------------------

// Claim allow-lists ("claimTransition allowed sources").
var refClaimSources = map[string][]string{
	"commission": {"new", "commissioned", "failed", "running"},
	"deploy":     {"commissioned", "failed", "running"},
}

// Claim target transient states.
var refClaimTarget = map[string]string{
	"commission": "commissioning",
	"deploy":     "deploying",
}

// FailOperation rule 2: op types whose failure never touches machine state.
var refLeavesStateOnFail = map[string]bool{
	"power_on": true, "power_off": true, "power_cycle": true,
	"set_boot_device": true, "bmc_reconcile": true,
}

var refAllOpTypes = []string{
	"deploy", "commission", "power_on", "power_off", "power_cycle",
	"set_boot_device", "bmc_reconcile",
}

func refActive(status string) bool {
	return status == "pending" || status == "claimed" || status == "running"
}

func refTerminal(status string) bool { return status == "completed" || status == "failed" }

// enter applies a lifecycle state write with the documented auto-lock
// policy: -> running locks, -> failed unlocks, anything else leaves the lock.
func (m *refMachine) enter(state string) {
	m.state = state
	switch state {
	case "running":
		m.locked = true
	case "failed":
		m.locked = false
	}
}

func (r *refModel) activeOpOn(machine uuid.UUID) *refOp {
	for _, o := range r.ops {
		if o.machine == machine && refActive(o.status) {
			return o
		}
	}
	return nil
}

func (r *refModel) sortedOps() []*refOp {
	out := make([]*refOp, 0, len(r.ops))
	for _, o := range r.ops {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].id[:], out[j].id[:]) < 0 })
	return out
}

// --- machines ---------------------------------------------------------------

func (r *refModel) createMachine(id uuid.UUID, serial string, deviceID int, state string, pxe, locked bool) opsmObs {
	if serial == "" {
		return opsmObs{Err: "error"}
	}
	if _, dup := r.machines[id]; dup {
		return opsmObs{Err: "error"}
	}
	for _, m := range r.machines {
		if m.serial == serial || (deviceID != 0 && m.deviceID == deviceID) {
			r.hit("machine.create.conflict")
			return opsmObs{Err: "error"}
		}
	}
	if state == "" {
		state = "new"
	}
	r.machines[id] = &refMachine{id: id, serial: serial, deviceID: deviceID, state: state, pxe: pxe, locked: locked}
	r.hit("machine.create")
	return opsmObs{}
}

// deleteIfIdle is Store.DeleteMachineIfIdle: hard delete of the row, its
// operations and its envelopes, refused (nothing changes) while the machine
// has an active operation. A machine that is already gone counts as deleted.
func (r *refModel) deleteIfIdle(id uuid.UUID) opsmObs {
	if _, ok := r.machines[id]; !ok {
		r.hit("machine.delete.absent")
		return opsmObs{Deleted: true}
	}
	if r.activeOpOn(id) != nil {
		r.hit("machine.delete.refused_active_op")
		return opsmObs{}
	}
	for opID, o := range r.ops {
		if o.machine == id {
			delete(r.ops, opID)
			r.hit("machine.delete.op")
		}
	}
	delete(r.machines, id)
	r.hit("machine.delete")
	return opsmObs{Deleted: true}
}

func (r *refModel) setLocked(id uuid.UUID, v bool) opsmObs {
	m, ok := r.machines[id]
	if !ok {
		return opsmObs{Err: "notfound"}
	}
	m.locked = v
	r.hit("machine.lock")
	return opsmObs{}
}

func (r *refModel) setPXE(id uuid.UUID, v bool) opsmObs {
	m, ok := r.machines[id]
	if !ok {
		return opsmObs{Err: "notfound"}
	}
	if v && m.locked {
		// A locked machine cannot be PXE-enabled; disabling is not gated.
		r.hit("machine.pxe.enable_while_locked")
		return opsmObs{Err: "error"}
	}
	m.pxe = v
	r.hit("machine.pxe")
	return opsmObs{}
}

func (r *refModel) rebind(id uuid.UUID, deviceID int) opsmObs {
	m, ok := r.machines[id]
	if !ok {
		return opsmObs{}
	}
	for _, other := range r.machines {
		if other.id != id && other.deviceID == deviceID {
			r.hit("machine.rebind.conflict")
			return opsmObs{Err: "error"}
		}
	}
	m.deviceID = deviceID
	r.hit("machine.rebind")
	return opsmObs{}
}

// patch models the API's PATCH /machines/:id: only desired_os changes, and
// an unknown machine is not found (never created).
func (r *refModel) patch(id uuid.UUID, desiredOS string) opsmObs {
	m, ok := r.machines[id]
	if !ok {
		return opsmObs{Err: "notfound"}
	}
	m.desiredOS = desiredOS
	r.hit("machine.patch")
	return opsmObs{}
}

func (r *refModel) storeCommission(id uuid.UUID, data string) opsmObs {
	if m, ok := r.machines[id]; ok {
		m.comm = opsmCanonJSON(data)
		m.commissioned = true
		r.hit("machine.store_commission")
	}
	return opsmObs{}
}

// --- op creation --------------------------------------------------------------

func (r *refModel) newOp(id, machine uuid.UUID, typ, params string) *refOp {
	o := &refOp{id: id, machine: machine, typ: typ, status: "pending", params: opsmCanonJSON(params)}
	r.ops[id] = o
	return o
}

// createOpRaw is Store.CreateOperation: a bare INSERT guarded only by the
// machine FK and the one-active-op-per-machine unique index.
func (r *refModel) createOpRaw(id, machine uuid.UUID, typ, params string) opsmObs {
	if _, ok := r.machines[machine]; !ok {
		r.hit("op.create_raw.fk")
		return opsmObs{Err: "error"}
	}
	if r.activeOpOn(machine) != nil {
		r.hit("op.create_raw.active_conflict")
		return opsmObs{Err: "error"}
	}
	if _, dup := r.ops[id]; dup {
		// The caller supplied an ID a (terminal) row already has.
		r.hit("op.create_raw.id_conflict")
		return opsmObs{Err: "error"}
	}
	o := r.newOp(id, machine, typ, params)
	r.hit("op.create_raw." + typ)
	return opsmObs{Ops: []opsmOpView{o.view()}}
}

// createMachineOp is Store.CreateMachineOp. newID is the ID the real store
// minted (the model has no way to predict a random UUID); it is used only
// when the model itself predicts success.
func (r *refModel) createMachineOp(machine uuid.UUID, typ, params string, allowed []string, newID uuid.UUID) opsmObs {
	m, ok := r.machines[machine]
	if !ok {
		return opsmObs{Err: "error"}
	}
	if m.locked {
		r.hit("op.create.locked")
		return opsmObs{Err: "error"}
	}
	if !m.pxe && (typ == "commission" || typ == "deploy") {
		r.hit("op.create.not_pxe")
		return opsmObs{Err: "error"}
	}
	if len(allowed) > 0 && !slices.Contains(allowed, m.state) {
		r.hit("op.create.bad_state")
		return opsmObs{Err: "error"}
	}
	if r.activeOpOn(machine) != nil {
		r.hit("op.create.active_conflict")
		return opsmObs{Err: "error"}
	}
	o := r.newOp(newID, machine, typ, params)
	r.hit("op.create." + typ)
	return opsmObs{Ops: []opsmOpView{o.view()}}
}

// createPowerOp is Store.CreatePowerOp: no machine-state or lock guard, but
// the one-active-op index still applies to every op type
// (TestCreatePowerOp_RejectedWhenLifecycleOpActive pins that).
func (r *refModel) createPowerOp(machine uuid.UUID, typ, params string, newID uuid.UUID) opsmObs {
	if _, ok := r.machines[machine]; !ok {
		return opsmObs{Err: "error"}
	}
	if r.activeOpOn(machine) != nil {
		r.hit("op.create_power.active_conflict")
		return opsmObs{Err: "error"}
	}
	o := r.newOp(newID, machine, typ, params)
	r.hit("op.create_power." + typ)
	return opsmObs{Ops: []opsmOpView{o.view()}}
}

// --- claim --------------------------------------------------------------------

func (r *refModel) claim(opID uuid.UUID, worker string) opsmObs {
	o, ok := r.ops[opID]
	if !ok {
		r.hit("claim.absent")
		return opsmObs{Nil: true}
	}
	// Idempotent re-claim: same worker, still claimed, no phase recorded.
	if o.status == "claimed" && o.claimedBy == worker && o.phase == "" {
		o.version++
		o.claimedOld = false
		o.claimedGen++
		r.hit("claim.reclaim")
		return opsmObs{Ops: []opsmOpView{o.view()}}
	}
	if o.status != "pending" {
		if refTerminal(o.status) {
			r.hit("claim.terminal")
		} else {
			r.hit("claim.lost")
		}
		return opsmObs{Nil: true}
	}
	target, lifecycle := refClaimTarget[o.typ]
	if lifecycle {
		m := r.machines[o.machine]
		if !slices.Contains(refClaimSources[o.typ], m.state) {
			r.hit("claim.refused.state")
			return opsmObs{Err: "error"}
		}
		if m.locked {
			r.hit("claim.refused.locked")
			return opsmObs{Err: "error"}
		}
		m.enter(target)
	}
	o.status = "claimed"
	o.claimedBy = worker
	o.claimed = true
	o.claimedOld = false
	o.claimedGen++
	o.version++
	o.events = append(o.events, refEvent{Msg: "operation claimed", ClaimedBy: worker})
	r.hit("claim.ok." + o.typ)
	return opsmObs{Ops: []opsmOpView{o.view()}}
}

// --- phase --------------------------------------------------------------------

func (r *refModel) setPhase(opID uuid.UUID, phase string) opsmObs {
	o, ok := r.ops[opID]
	if !ok {
		r.hit("phase.absent")
		return opsmObs{Err: "notfound"}
	}
	if o.phase == phase {
		r.hit("phase.same")
		return opsmObs{Changed: false}
	}
	if o.phase == "awaiting_phone_home" && phase != "failed" {
		// The phone-home latch: only the agent's failure leaves it.
		r.hit("phase.held_awaiting_phone_home")
		return opsmObs{Changed: false}
	}
	if refTerminal(o.status) {
		// A terminal row is never written again.
		r.hit("phase.terminal")
		return opsmObs{Changed: false}
	}
	prev := o.phase
	o.phase = phase
	o.phaseOld = false
	o.phaseGen++
	o.version++
	r.hit("phase.changed")
	if phase == "awaiting_phone_home" {
		r.hit("phase.to_awaiting_phone_home")
	}
	if prev == "awaiting_phone_home" {
		r.hit("phase.failed_from_awaiting_phone_home")
	}
	return opsmObs{Changed: true, Prev: prev, Ops: []opsmOpView{o.view()}}
}

// --- complete -----------------------------------------------------------------

func (r *refModel) terminalEvent(o *refOp, status, errText string) {
	msg := "operation completed"
	if status == "failed" {
		msg = "operation failed"
	}
	o.events = append(o.events, refEvent{Msg: msg, Status: status, Err: errText})
}

func (r *refModel) completeWithState(opID uuid.UUID, target, result string) opsmObs {
	o, ok := r.ops[opID]
	if !ok || refTerminal(o.status) {
		r.hit("complete.noop")
		return opsmObs{Nil: true}
	}
	if o.status == "pending" {
		r.hit("complete.pending")
	}
	o.status = "completed"
	o.completed = true
	o.result = opsmCanonJSON(result)
	o.version++
	r.terminalEvent(o, "completed", "")
	if target != "" {
		r.machines[o.machine].enter(target)
	}
	r.hit("complete.ok." + o.typ)
	return opsmObs{Ops: []opsmOpView{o.view()}, MStates: []string{target}}
}

// completePlain is Store.CompleteOperation: status only, no machine state,
// and (as written) no terminal envelope.
func (r *refModel) completePlain(opID uuid.UUID, result string) opsmObs {
	o, ok := r.ops[opID]
	if !ok || refTerminal(o.status) {
		return opsmObs{}
	}
	o.status = "completed"
	o.completed = true
	o.result = opsmCanonJSON(result)
	o.version++
	r.hit("complete_plain.ok")
	return opsmObs{}
}

func (r *refModel) completeActive(machine uuid.UUID, typ, target, result string) opsmObs {
	o := r.activeOpOn(machine)
	if o == nil || o.typ != typ {
		r.hit("complete_active.noop")
		return opsmObs{Nil: true}
	}
	o.status = "completed"
	o.completed = true
	o.result = opsmCanonJSON(result)
	o.version++
	r.terminalEvent(o, "completed", "")
	r.machines[machine].enter(target)
	r.hit("complete_active.ok")
	return opsmObs{Ops: []opsmOpView{o.view()}, MStates: []string{target}}
}

// --- fail ---------------------------------------------------------------------

// failOne applies the four FailOperation rules to an active op and returns
// the durable machine state it wrote ("" for none).
func (r *refModel) failOne(o *refOp, result string) string {
	wasPending := o.status == "pending"
	o.status = "failed"
	o.completed = true
	o.result = opsmCanonJSON(result)
	o.version++
	r.terminalEvent(o, "failed", opsmFailErrText(result))
	if wasPending {
		r.hit("fail.rule1_pending")
		return ""
	}
	if refLeavesStateOnFail[o.typ] {
		r.hit("fail.rule2_power")
		return ""
	}
	m := r.machines[o.machine]
	if o.typ == "commission" && m.comm == "" {
		r.hit("fail.rule3_initial_commission")
		m.state = "new" // lock untouched
		return "new"
	}
	r.hit("fail.rule4_failed")
	if m.locked {
		r.hit("fail.rule4_unlocks")
	}
	m.enter("failed")
	return "failed"
}

func (r *refModel) fail(opID uuid.UUID, result string) opsmObs {
	o, ok := r.ops[opID]
	if !ok || refTerminal(o.status) {
		r.hit("fail.noop")
		return opsmObs{Nil: true}
	}
	st := r.failOne(o, result)
	return opsmObs{Ops: []opsmOpView{o.view()}, MStates: []string{st}}
}

func (r *refModel) failMany(match func(*refOp) bool, result string) opsmObs {
	out := opsmObs{}
	for _, o := range r.sortedOps() {
		if !refActive(o.status) || !match(o) {
			continue
		}
		st := r.failOne(o, result)
		out.Ops = append(out.Ops, o.view())
		out.MStates = append(out.MStates, st)
	}
	return out
}

func (r *refModel) abort(machine uuid.UUID) opsmObs {
	m, ok := r.machines[machine]
	if !ok {
		return opsmObs{Err: "error"}
	}
	out := r.failMany(func(o *refOp) bool { return o.machine == machine }, `{"error":"aborted by operator"}`)
	if len(out.Ops) > 0 {
		r.hit("abort.failed_op")
	} else {
		r.hit("abort.noop")
	}
	out.MState, out.MLocked = m.state, m.locked
	return out
}

func (r *refModel) failInFlight(machine uuid.UUID, reason string) opsmObs {
	b, _ := json.Marshal(map[string]string{"error": reason})
	out := r.failMany(func(o *refOp) bool { return o.machine == machine }, string(b))
	if len(out.Ops) > 0 {
		r.hit("fail_in_flight.failed_op")
	}
	return out
}

// --- commission result --------------------------------------------------------

// validateBinding restates Store.ValidateOperationBinding.
func (r *refModel) validateBinding(opID, machine uuid.UUID, wantType, data string) opsmObs {
	if opID == uuid.Nil || machine == uuid.Nil {
		return opsmObs{Err: "binding"}
	}
	o, ok := r.ops[opID]
	if !ok {
		return opsmObs{Err: "notfound"}
	}
	return r.bindingOf(o, machine, wantType, data)
}

func (r *refModel) bindingOf(o *refOp, machine uuid.UUID, wantType, data string) opsmObs {
	if o.machine != machine {
		r.hit("binding.wrong_machine")
		return opsmObs{Err: "binding"}
	}
	if o.typ != wantType {
		r.hit("binding.wrong_type")
		return opsmObs{Err: "binding"}
	}
	switch o.status {
	case "pending", "claimed", "running":
		return opsmObs{}
	case "completed":
		if wantType == "deploy" {
			return opsmObs{Already: true}
		}
		if wantType == "commission" && o.result != "" && r.replayEqual(o.result, data) {
			r.hit("binding.replay_same")
			return opsmObs{Already: true}
		}
		r.hit("binding.replay_different")
		return opsmObs{Err: "binding"}
	default:
		r.hit("binding.terminal_failed")
		return opsmObs{Err: "binding"}
	}
}

// replayEqual: is a replayed report "semantically identical" to the stored
// result? stored is canonical (as Postgres normalizes it).
func (r *refModel) replayEqual(stored, replay string) bool {
	return opsmCanonJSON(stored) == opsmCanonJSON(replay)
}

func (r *refModel) commit(opID, machine uuid.UUID, data string) opsmObs {
	o, ok := r.ops[opID]
	if !ok {
		r.hit("commit.absent")
		return opsmObs{Err: "notfound", Nil: true}
	}
	b := r.bindingOf(o, machine, "commission", data)
	if b.Err != "" {
		b.Nil = true
		return b
	}
	if b.Already {
		b.Nil = true
		r.hit("commit.already")
		return b
	}
	m := r.machines[machine]
	if o.status == "pending" {
		r.hit("commit.pending")
	}
	m.comm = opsmCanonJSON(data)
	m.commissioned = true
	o.status = "completed"
	o.completed = true
	o.result = opsmCanonJSON(data)
	o.version++
	r.terminalEvent(o, "completed", "")
	m.state = "commissioned" // lock untouched
	r.hit("commit.ok")
	return opsmObs{Ops: []opsmOpView{o.view()}, MStates: []string{"commissioned"}}
}

// --- sweeps -------------------------------------------------------------------

// opsmSweepWindow says which timestamps a sweep's cutoff reaches.
type opsmSweepWindow int

const (
	opsmSweepAll  opsmSweepWindow = iota // cutoff in the future: every non-null timestamp is past it
	opsmSweepOld                         // only backdated timestamps are past it
	opsmSweepNone                        // nothing is past it
)

func (w opsmSweepWindow) reaches(old bool) bool {
	switch w {
	case opsmSweepAll:
		return true
	case opsmSweepOld:
		return old
	default:
		return false
	}
}

func (r *refModel) expirePending(w opsmSweepWindow) opsmObs {
	out := r.failMany(func(o *refOp) bool {
		return o.status == "pending" && w.reaches(o.createdOld)
	}, `{"error":"pending op timed out"}`)
	r.hit(opsmSweepTag("sweep.pending", out))
	return out
}

func (r *refModel) expireStuck(w opsmSweepWindow) opsmObs {
	spared := 0
	out := r.failMany(func(o *refOp) bool {
		if o.status != "claimed" && o.status != "running" {
			return false
		}
		if !o.claimed || !w.reaches(o.claimedOld) {
			return false
		}
		if o.phase == "awaiting_phone_home" {
			spared++
			return false
		}
		return true
	}, `{"error":"operation timed out"}`)
	if spared > 0 {
		r.hit("sweep.stuck.spared_awaiting_phone_home")
	}
	r.hit(opsmSweepTag("sweep.stuck", out))
	return out
}

func (r *refModel) expireOrphans(w opsmSweepWindow) opsmObs {
	out := r.failMany(func(o *refOp) bool {
		return o.status == "claimed" && o.phase == "" && o.claimed && w.reaches(o.claimedOld)
	}, `{"error":"claiming worker disconnected before execution started"}`)
	r.hit(opsmSweepTag("sweep.orphan", out))
	return out
}

func (r *refModel) expirePhoneHome(w opsmSweepWindow) opsmObs {
	out := r.failMany(func(o *refOp) bool {
		return (o.status == "claimed" || o.status == "running") &&
			o.phase == "awaiting_phone_home" && w.reaches(o.phaseOld)
	}, `{"error":"phone-home timed out"}`)
	r.hit(opsmSweepTag("sweep.phone_home", out))
	return out
}

func opsmSweepTag(prefix string, out opsmObs) string {
	if len(out.Ops) > 0 {
		return prefix + ".hit"
	}
	return prefix + ".miss"
}

// backdate marks one of an op's sweep clocks as past the sweep horizon.
// field: "created", "claimed", "phase".
func (r *refModel) backdate(opID uuid.UUID, field string) {
	o, ok := r.ops[opID]
	if !ok {
		return
	}
	switch field {
	case "created":
		o.createdOld = true
		o.createdGen++
	case "claimed":
		if o.claimed {
			o.claimedOld = true
			o.claimedGen++
		}
	case "phase":
		o.phaseOld = true
		o.phaseGen++
	}
	r.hit("backdate." + field)
}

// --- credentials --------------------------------------------------------------

// refSecretPaths is the documented credential list ("Credentials in
// operations"): bmc_runtime.password, each pdu_runtime.outlets[].password,
// amt_runtime.password, monitor_runtime.password, tailscale_runtime.auth_key.
var refSecretPaths = [][]string{
	{"bmc_runtime", "password"},
	{"pdu_runtime", "outlets", "[]", "password"},
	{"amt_runtime", "password"},
	{"monitor_runtime", "password"},
	{"tailscale_runtime", "auth_key"},
}

func (r *refModel) scrub() opsmObs {
	var n int64
	for _, o := range r.ops {
		if !refTerminal(o.status) || o.params == "" {
			continue
		}
		scrubbed, changed := opsmScrubParams(o.params)
		if changed {
			o.params = scrubbed
			n++
		}
	}
	if n > 0 {
		r.hit("scrub.rewrote")
	} else {
		r.hit("scrub.noop")
	}
	return opsmObs{N: n}
}

// opsmScrubParams removes every documented credential from a params document.
// Only object documents are touched.
func opsmScrubParams(canon string) (string, bool) {
	var doc any
	dec := json.NewDecoder(strings.NewReader(canon))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return canon, false
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return canon, false
	}
	changed := false
	for _, p := range refSecretPaths {
		if opsmRemovePath(root, p) {
			changed = true
		}
	}
	if !changed {
		return canon, false
	}
	b, _ := json.Marshal(root)
	return opsmCanonJSON(string(b)), true
}

func opsmRemovePath(v any, path []string) bool {
	if len(path) == 0 {
		return false
	}
	if path[0] == "[]" {
		arr, ok := v.([]any)
		if !ok {
			return false
		}
		changed := false
		for _, e := range arr {
			if opsmRemovePath(e, path[1:]) {
				changed = true
			}
		}
		return changed
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if len(path) == 1 {
		if _, has := obj[path[0]]; has {
			delete(obj, path[0])
			return true
		}
		return false
	}
	child, has := obj[path[0]]
	if !has {
		return false
	}
	return opsmRemovePath(child, path[1:])
}

// opsmHasCredential reports whether a params document still holds any
// documented credential key.
func opsmHasCredential(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	_, changed := opsmScrubParams(opsmCanonJSON(raw))
	return changed
}

// --- JSON helpers ---------------------------------------------------------------

// opsmCanonJSON renders a JSON document in a canonical spelling: sorted keys, no
// insignificant whitespace, numbers as exact rationals. "" (SQL NULL) stays
// "". It makes a document compare equal to its jsonb round trip.
func opsmCanonJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "INVALID:" + raw
	}
	var b strings.Builder
	opsmWriteCanon(&b, v)
	return b.String()
}

func opsmWriteCanon(b *strings.Builder, v any) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			opsmWriteCanon(b, t[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			opsmWriteCanon(b, e)
		}
		b.WriteByte(']')
	case json.Number:
		if rat, ok := new(big.Rat).SetString(string(t)); ok {
			b.WriteString(rat.RatString())
		} else {
			b.WriteString(string(t))
		}
	default:
		eb, _ := json.Marshal(t)
		b.Write(eb)
	}
}

// opsmFailErrText is the failure cause a terminal envelope carries: the string
// under "error" when the result is a JSON object that has one.
func opsmFailErrText(result string) string {
	if strings.TrimSpace(result) == "" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &obj); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(obj["error"], &s); err != nil {
		return ""
	}
	return s
}
