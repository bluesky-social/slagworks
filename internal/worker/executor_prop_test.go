package worker

// Property tests for the site worker's actuation engine. See
// execworld_prop_test.go for the world the properties run in.
//
// Safety properties (checked inline, at the moment they would break):
//
//	untrusted-claim        a rehydrated-not-resynced worker never claims
//	claim-after-cancel     an executor whose context is cancelled (lost the
//	                       VIP, shutting down) starts no new claim
//	self-feed-claim        the worker never claims an outlet op that cuts
//	                       its own feed (unless allow_self cycle/on)
//	actuate-without-claim  no controller / reconciler call without a won
//	                       claim reply in the same executeOp
//	double-drive           an op is won-and-run at most once, fleet-wide,
//	                       across re-claims, restarts and VIP flaps
//	plan                   for a won claim the controller calls are exactly
//	                       the documented plan for the op type given the
//	                       observed call outcomes (transport refusals build
//	                       no controller, a serial mismatch issues no
//	                       mutating call, a failed step stops the plan)
//	report                 a won claim always ends in exactly one terminal
//	                       op_update, or (lifecycle) the two boot-prefix
//	                       phases, or (bmc_reconcile) a durable report
//	controller-leak        every controller built is closed exactly once
//
// Liveness at quiescence (faults stopped, everything reconnected and
// delivered, executors ticked to a fixpoint):
//
//	pending-not-driven     no claimable pending op is left pending
//	claimed-never-driven   no op sits claimed by a live VIP holder that is
//	                       not going to drive it
//	report-missing         no op that was driven is still open on the hub
//	                       for want of a report the worker produced
//	phone-home-recorded-   no deploy whose phone-home a worker recorded is
//	not-completed          still claimed on the hub
//
// A few violation classes describe behavior the executor does not currently
// guarantee. They are listed in epTolerated (execworld_prop_test.go) and are
// counted instead of failed.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

var epCaseSeq atomic.Int64

// ---------------------------------------------------------------------------
// Generators
// ---------------------------------------------------------------------------

var (
	epOpTypes = []model.OperationType{
		model.OpPowerCycle, model.OpPowerOn, model.OpPowerOff, model.OpDeploy,
		model.OpCommission, model.OpSetBootDevice, model.OpBMCReconcile,
	}
	epTransports  = []string{"bmc", "bmc", "bmc", "bmc", "bmc", "pdu", "pdu", "amt", "amt", "both", "nullpdu", "nullamt", "garbage"}
	epSerialModes = []string{"match", "match", "fold", "empty", "mismatch"}
)

// genSpec draws an op. Three in four are constructed to be drivable (a
// transport the machine supports, well-formed params), so that most claims
// reach real actuation; the rest are drawn from the whole space, where most
// combinations are refused before a controller exists. Either way the
// serial expectation, boot device and reconcile inputs still vary.
func (w *epWorld) genSpec() epOpSpec {
	s := epOpSpec{
		typ:        epChoose(w, epOpTypes, "opType"),
		mach:       w.pick(len(w.mach), "opMachine"),
		transport:  epChoose(w, epTransports, "transport"),
		serialMode: epChoose(w, epSerialModes, "serialMode"),
	}
	if !w.chance(1, 4, "anySpec") {
		if s.typ == model.OpBMCReconcile || s.typ == model.OpSetBootDevice {
			// IPMI-LAN account reconcile and the boot-override write need a
			// real BMC: machines 0 and 1.
			s.mach = w.pick(2, "bmcMachine")
		}
		switch m := w.mach[s.mach]; {
		case s.typ == model.OpBMCReconcile || s.typ == model.OpSetBootDevice:
			s.transport = "bmc"
		case m.amt:
			s.transport = epChoose(w, []string{"amt", "amt", "pdu"}, "amtTransport")
		case m.bmcIP == "":
			s.transport = "pdu"
		default:
			s.transport = epChoose(w, []string{"bmc", "bmc", "bmc", "pdu"}, "bmcTransport")
		}
	}
	m := w.mach[s.mach]
	switch s.serialMode {
	case "match":
		s.expected = m.serial
	case "fold":
		s.expected = strings.ToUpper(m.serial)
	case "mismatch":
		s.expected = "ZZ-OTHER-BOX"
	}
	switch {
	case s.powerAction():
		s.bootDevice = epChoose(w, []string{"", "", "", "", "pxe", "pxe", "disk", "floppy"}, "bootDevice")
		if (s.transport == "pdu" || s.transport == "both") && w.chance(1, 2, "outlet") {
			s.outlet = true
			s.downstream = epChoose(w, []string{"wk-0", "WK-1", "OTHER-LOAD", ""}, "downstream")
			s.allowSelf = w.chance(1, 2, "allowSelf")
		}
	case s.typ == model.OpSetBootDevice:
		s.device = epChoose(w, []string{"pxe", "disk", "default", "floppy"}, "device")
		s.persistent = w.chance(1, 2, "persistent")
	case s.typ == model.OpBMCReconcile:
		s.adminCred = !w.chance(1, 8, "noAdminCred")
		s.monitor = epChoose(w, []string{"none", "none", "none", "stored", "stored", "empty", "rotate+stored", "rotate"}, "monitor")
	}
	return s
}

// params renders the op's params blob the way global stamps it.
func (s epOpSpec) params() json.RawMessage {
	if s.transport == "garbage" {
		return json.RawMessage(`{"bmc_runtime": 7}`)
	}
	p := map[string]any{}
	bmc := map[string]any{"expected_chassis_serial": s.expected, "extra_config": map[string]any{}}
	if s.typ != model.OpBMCReconcile || s.adminCred {
		bmc["username"] = "ADMIN"
		bmc["password"] = "adm1n!pw"
	}
	p["bmc_runtime"] = bmc
	pdu := map[string]any{"outlets": []any{map[string]any{"pdu_host": "10.9.9.9", "outlet": 3}}}
	if s.outlet {
		pdu["outlet"] = true
		pdu["downstream_serial"] = s.downstream
		pdu["allow_self"] = s.allowSelf
	}
	amt := map[string]any{"host": "10.8.8.8", "username": "admin", "password": "amt!pw"}
	switch s.transport {
	case "pdu":
		p["pdu_runtime"] = pdu
	case "amt":
		p["amt_runtime"] = amt
	case "both":
		p["pdu_runtime"] = pdu
		p["amt_runtime"] = amt
	case "nullpdu":
		p["pdu_runtime"] = nil
	case "nullamt":
		p["amt_runtime"] = nil
	}
	if s.bootDevice != "" {
		p["boot_device"] = s.bootDevice
	}
	if s.typ == model.OpSetBootDevice {
		p["device"] = s.device
		p["persistent"] = s.persistent
	}
	switch s.monitor {
	case "stored":
		p["monitor_runtime"] = map[string]any{"password": "St0red!pw"}
	case "empty":
		p["monitor_runtime"] = map[string]any{"password": ""}
	case "rotate+stored":
		p["monitor_runtime"] = map[string]any{"password": "St0red!pw"}
		p["rotate_monitor"] = true
	case "rotate":
		p["rotate_monitor"] = true
	}
	b, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return b
}

func newEpWorld(t *rapid.T, dir string) *epWorld {
	w := &epWorld{t: t, byOp: map[uuid.UUID]*epOpFacts{}, dir: dir, persist: true}
	caseNo := epCaseSeq.Add(1)
	states := []model.MachineState{
		model.MachineStateCommissioned, model.MachineStateNew, model.MachineStateFailed, model.MachineStateRunning,
	}
	w.mach = []*epMachine{
		{idx: 0, bmcIP: "10.0.0.40", serial: "SN-A0", bootOK: true, powerOn: true, state: model.MachineStateCommissioned, inSite: true},
		{idx: 1, bmcIP: "10.0.0.41:8000", serial: "Sn-B1", state: epChoose(w, states, "m1state"),
			powerOn: w.chance(1, 2, "m1power"), inSite: true},
		{idx: 2, bmcIP: "10.0.0.42", serial: "SN-C2", amt: true, powerOn: true, state: model.MachineStateCommissioned, inSite: true},
		{idx: 3, bmcIP: "", serial: "SN-D3", powerOn: true, state: model.MachineStateCommissioned,
			inSite: w.chance(3, 4, "m3inSite")},
	}
	for _, m := range w.mach {
		m.id = uuid.New()
		m.mac = fmt.Sprintf("02:00:00:00:%02x:%02x", caseNo%251, m.idx)
		m.hasCommission = m.state != model.MachineStateNew
	}
	w.hub = &epHub{w: w, ops: map[uuid.UUID]*epHubOp{}}
	for i, serial := range []string{"WK-0", "wk-1"} {
		pw := &epWorker{
			w: w, idx: i, id: fmt.Sprintf("worker-%d", i), serial: serial,
			dbPath:  filepath.Join(dir, fmt.Sprintf("c%d-w%d.db", caseNo, i)),
			updSent: map[uuid.UUID][]model.OperationUpdate{},
			obsSent: map[uuid.UUID]int{},
			downAt:  -1,
		}
		pw.openCache()
		w.wk = append(w.wk, pw)
	}
	t.Cleanup(func() {
		for _, pw := range w.wk {
			pw.closeDB()
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(pw.dbPath + suffix)
			}
		}
	})
	w.hub.buildSnapshot()
	return w
}

// ---------------------------------------------------------------------------
// Actions
// ---------------------------------------------------------------------------

type epAction struct {
	name      string
	weight    int
	interrupt bool // may run while an executor is inside a claim / BMC call
}

var epActions = []epAction{
	{"create", 6, true},
	{"tick", 18, false},
	{"deliver", 6, true},
	{"deliverAll", 3, true},
	{"drain", 5, true},
	{"drainFault", 2, true},
	{"disconnect", 2, true},
	{"connect", 4, true},
	{"restart", 1, true},
	{"vipAcquire", 2, true},
	{"vipRelease", 1, true},
	{"vipFlap", 1, true},
	{"abort", 2, true},
	{"advanceSweep", 2, true},
	{"membership", 1, true},
	{"agent", 6, true},
	{"late", 2, true},
	{"nestedTick", 3, true},
}

var epDrainFaults = []string{"lostwrite", "hubdown", "obsreject", "acklost"}

func (w *epWorld) pickWorker(label string) *epWorker {
	return w.wk[w.pick(len(w.wk), label)]
}

func (w *epWorld) pickMachine(label string) *epMachine {
	return w.mach[w.pick(len(w.mach), label)]
}

// step runs one action. cur is the worker whose executor is on the stack
// when the action runs as an interrupt, nil at top level.
func (w *epWorld) step(name string, cur *epWorker) {
	w.stat("action." + name)
	switch name {
	case "create":
		spec := w.genSpec()
		if rec := w.hub.create(spec, spec.params()); rec != nil {
			w.logf("hub: create %s expected=%q boot=%q outlet=%v/%q/%v monitor=%s", w.opName(rec.op.ID), spec.expected,
				spec.bootDevice, spec.outlet, spec.downstream, spec.allowSelf, spec.monitor)
			w.stat("op.created." + string(spec.typ))
			w.stat("op.transport." + spec.transport)
		} else {
			w.stat("op.create_rejected")
		}
	case "tick":
		// Mostly the VIP holder: a tick of a worker without an executor is
		// a no-op.
		pw := w.pickWorker("tickWorker")
		if holder := w.vipHolder(); holder != nil && !pw.active {
			pw = holder
		}
		// The stream normally lands within milliseconds and wakes the
		// executor; the outbox drain is wake-driven too. Model that as the
		// common case and leave lag to the explicit deliver/drain actions.
		if w.chance(3, 4, "tickAfterDelivery") {
			pw.deliverAll()
		}
		pw.tick()
		if w.chance(1, 2, "drainAfterTick") {
			fault := ""
			if w.chance(1, 6, "drainAfterTickFault") {
				fault = epChoose(w, epDrainFaults, "drainFault")
			}
			pw.drain(fault)
		}
	case "nestedTick":
		// Another executor pass while one is in flight: the other worker
		// (both hold the VIP during a keepalived flap), or a second
		// executor of this same worker after it lost and regained the VIP.
		pw := w.pickWorker("nestedWorker")
		if cur == nil || pw.inTick == 0 {
			pw.tick()
		} else if pw == cur && w.depth == 1 && (w.ha == nil || !w.ha.bothActive) {
			pw.releaseVIP()
			pw.acquireVIP()
			pw.tick()
		}
	case "deliver":
		pw := w.pickWorker("deliverWorker")
		if n := len(pw.inbox); n > 0 {
			pw.deliver(w.pick(min(n, 4), "deliverIdx"))
		}
	case "deliverAll":
		w.pickWorker("deliverWorker").deliverAll()
	case "drain":
		w.pickWorker("drainWorker").drain("")
	case "drainFault":
		w.pickWorker("drainWorker").drain(epChoose(w, epDrainFaults, "drainFault"))
	case "disconnect":
		w.pickWorker("connWorker").disconnect()
	case "connect":
		w.pickWorker("connWorker").connect()
	case "restart":
		pw := w.pickWorker("restartWorker")
		if pw.inTick == 0 {
			pw.restart("clean")
		}
	case "vipAcquire":
		w.pickWorker("vipWorker").acquireVIP()
	case "vipRelease":
		w.pickWorker("vipWorker").releaseVIP()
	case "vipFlap":
		// keepalived moves the VIP: the holder is cancelled, the peer starts.
		for _, pw := range w.wk {
			if pw.active {
				pw.releaseVIP()
			} else {
				pw.acquireVIP()
			}
		}
	case "abort":
		// As an interrupt, aim at the machine whose op is being driven half
		// the time (the operator aborting what they just started), and let
		// the op_removed reach the workers right away half the time.
		m := w.pickMachine("abortMachine")
		if cur != nil && cur.cur != nil && w.chance(1, 2, "abortCurrent") {
			m = w.mach[w.byOp[cur.cur.op].spec.mach]
		}
		w.hub.abort(m)
		if w.chance(1, 2, "abortDelivered") {
			for _, pw := range w.wk {
				pw.deliverAll()
			}
		}
	case "advanceSweep":
		w.now += epChoose(w, []int64{1, 30, 61, 121, 1801}, "advance")
		w.hub.sweep()
	case "membership":
		m := w.pickMachine("memberMachine")
		m.inSite = !m.inSite
		w.logf("hub: m%d inSite=%v, snapshot pushed", m.idx, m.inSite)
		w.hub.push()
	case "agent":
		w.agent()
	case "late":
		w.runDeferred()
	default:
		if w.ha != nil {
			w.ha.step(name, cur)
		}
	}
}

func (w *epWorld) runDeferred() {
	d := w.deferred
	w.deferred = nil
	for _, f := range d {
		f()
	}
}

var (
	epInterruptNames []string
	epTopNames       []string
)

func init() {
	for _, a := range epActions {
		for i := 0; i < a.weight; i++ {
			epTopNames = append(epTopNames, a.name)
			if a.interrupt {
				epInterruptNames = append(epInterruptNames, a.name)
			}
		}
	}
}

// interrupt lets other events of the world happen while pw's executor is
// blocked inside a claim RPC or a controller call.
func (w *epWorld) interrupt(pw *epWorker, where string) {
	if w.quiet || w.depth >= 2 {
		return
	}
	if len(w.forced) > 0 && w.depth == 0 {
		f := w.forced[0]
		w.forced = w.forced[1:]
		w.depth++
		defer func() { w.depth-- }()
		w.stat("interrupt.forced@" + where)
		f(pw)
		return
	}
	k := w.pick(16, "interrupt@"+where)
	if k < 13 {
		return
	}
	w.depth++
	defer func() { w.depth-- }()
	for i := 0; i < k-12; i++ {
		name := epChoose(w, w.interruptNames, "interruptAction")
		w.stat("interrupt." + name)
		if name == "vipRelease" || name == "vipFlap" {
			w.stat("interrupt.cancel_midrun")
		}
		w.step(name, pw)
	}
}

// vipHolder returns a worker serving the site's boot/report HTTP, or nil.
func (w *epWorld) vipHolder() *epWorker {
	for _, pw := range w.wk {
		if pw.active {
			return pw
		}
	}
	return nil
}

// agent emulates the on-target agent / installed OS talking to the VIP
// holder's /api/v1/report for the machine's driven lifecycle op.
func (w *epWorld) agent() {
	pw := w.vipHolder()
	if pw == nil {
		return
	}
	// Any machine that was actually booted for a lifecycle op still open on
	// the hub has an agent (or a freshly installed OS) running on it.
	var cands []*epHubOp
	for _, id := range w.hub.order {
		r := w.hub.ops[id]
		if epLifecycle(r.op.Type) && r.op.Status == model.OpClaimed && w.byOp[id].actuations > 0 {
			cands = append(cands, r)
		}
	}
	if len(cands) == 0 {
		return
	}
	rec := epChoose(w, cands, "agentOp")
	m := w.hub.machineOf(rec.op)
	w.agentEpoch++
	rep := &model.ProgressReport{OperationID: rec.op.ID.String(), MachineID: m.id.String()}
	var kinds []string
	if rec.op.Type == model.OpDeploy {
		// The installed OS exists only once the hub has seen deploy_complete.
		// A deploy rarely gets this far in a random history, so the later
		// stages are weighted up.
		kinds = []string{"phase:agent_starting", "phase:partitioning", "phase:rsyncing_os", "deploy_complete", "deploy_complete", "fail"}
		if rec.op.Phase == "awaiting_phone_home" {
			kinds = []string{"phase:rsyncing_os", "deploy_complete", "phone_home", "phone_home", "phone_home", "fail"}
		}
	} else {
		kinds = []string{"phase:agent_starting", "phase:collecting_inventory", "commission_report", "fail"}
	}
	kind := epChoose(w, kinds, "agentKind")
	switch {
	case strings.HasPrefix(kind, "phase:"):
		rep.Phase = strings.TrimPrefix(kind, "phase:")
	case kind == "fail":
		rep.Failed, rep.Phase, rep.Error = true, "partitioning", "ep: agent fatal"
	case kind == "commission_report":
		rep.Event = kind
		rep.CommissionData = json.RawMessage(fmt.Sprintf(`{"system":{"serial_number":%q},"nics":[{"mac":%q}]}`, m.serial, m.mac))
	default:
		rep.Event = kind
	}
	obsBefore := pw.obsCount(rec.op.ID)
	err := pw.cache.HandleProgressReport(rep)
	w.stat("agent." + kind)
	if kind == "phone_home" && err == nil {
		// The installed OS got its 2xx and will not phone home again: from
		// here on the completion must reach the hub. It does when the worker
		// queued the durable phone_home observation, now or on an earlier
		// answer (a retry of a recorded phone-home is acknowledged without
		// queueing anything, by design).
		facts := w.byOp[rec.op.ID]
		switch {
		case pw.obsCount(rec.op.ID) > obsBefore:
			facts.phoneHomeRecorded = true
			w.stat("agent.phone_home_recorded")
		case !facts.phoneHomeRecorded:
			facts.phoneHomeAckedUnrecorded = true
			w.stat("agent.phone_home_acked_unrecorded")
		}
	}
	w.logf("agent m%d -> w%d %s for %s: err=%v", m.idx, pw.idx, kind, w.opName(rec.op.ID), err)
}

// ---------------------------------------------------------------------------
// Per-attempt oracle
// ---------------------------------------------------------------------------

func epStatuses(us []model.OperationUpdate) (terminal []string, phases []string) {
	for _, u := range us {
		if u.Status != "" {
			terminal = append(terminal, u.Status)
		}
		if u.Phase != "" && u.Status == "" {
			phases = append(phases, u.Phase)
		}
	}
	return terminal, phases
}

func epResultError(us []model.OperationUpdate) string {
	for _, u := range us {
		if u.Status == string(model.OpFailed) {
			var r struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(u.Result, &r)
			return r.Error
		}
	}
	return ""
}

// checkAttempt validates one finished claim attempt against the documented
// behaviour, using only the generated op spec, the machine facts and the
// outcomes the fakes handed back.
func (w *epWorld) checkAttempt(pw *epWorker, a *epAttempt) {
	facts := w.byOp[a.op]
	spec := facts.spec
	m := w.mach[spec.mach]
	name := w.opName(a.op)
	all := pw.updates(a.op)
	if a.updEnd > len(all) || a.updBefore > a.updEnd {
		// A restart of this worker (crash of a nested pass) wiped the
		// report log under the attempt; nothing to compare.
		return
	}
	newUpd := all[a.updBefore:a.updEnd]
	terminal, phases := epStatuses(newUpd)
	newObs := a.obsEnd - a.obsBefore
	agentTouched := a.agentEpoch != w.agentEpoch
	bad := func(format string, args ...any) {
		w.violate("plan", "w%d attempt on %s (%s): %s\n  calls=%s\n  updates=%+v newObs=%d factory=%d/%s %s",
			pw.idx, name, a.outcome, fmt.Sprintf(format, args...), epCallsString(a.calls), newUpd, newObs,
			a.factoryCalls, a.factoryFault, a.factoryArgs)
	}

	// Other attempts on this op: a second Executor instance of the same
	// worker (VIP lost and regained) can be inside the same op at the same
	// time, which is its own violation class.
	execs, workers, otherWon := map[int]bool{}, map[int]bool{}, false
	for _, b := range w.attemptLog {
		if b.op != a.op || b.outcome != "won" {
			continue
		}
		execs[b.exec], workers[b.worker] = true, true
		if b != a && b.worker == a.worker {
			otherWon = true
		}
	}
	twoExecutors := len(workers) == 1 && (len(execs) > 1 || facts.overlap)

	if a.outcome != "won" {
		if a.factoryCalls != 0 || len(a.calls) != 0 || a.reconcileCalls != 0 {
			bad("controller activity without a won claim")
		}
		if !agentTouched && !otherWon && (len(terminal) != 0 || newObs != 0) {
			bad("a report was queued for an op this worker did not win")
		}
		return
	}
	w.stat("attempt.won." + string(spec.typ))
	if facts.wonAttempts > 1 {
		class := "double-drive"
		if twoExecutors {
			class = "double-drive-two-executors"
		}
		w.violate(class, "%s was won and driven %d times (hub grants=%d) by %d executor instance(s) on %d worker(s)",
			name, facts.wonAttempts, facts.grants, len(execs), len(workers))
	}

	issuedPower := false
	for _, c := range a.calls {
		if c.issued && (c.name == "PowerOn" || c.name == "PowerOff" || c.name == "PowerCycle" || c.name == "SetBootOverride") {
			issuedPower = true
		}
	}
	if issuedPower {
		facts.actuations++
		w.stat("attempt.actuated")
		if facts.actuations > 1 {
			class := "double-actuation"
			if twoExecutors {
				class = "double-drive-two-executors"
			}
			w.violate(class, "%s issued a power action in %d separate attempts", name, facts.actuations)
		}
	}
	for _, c := range a.ctrls {
		if c.closed != 1 {
			w.violate("controller-leak", "controller for %s closed %d times, want exactly 1", name, c.closed)
		}
	}

	expectFailed := func(why string, wantInError ...string) {
		if !agentTouched && (len(terminal) != 1 || terminal[0] != string(model.OpFailed)) {
			bad("want exactly one failed op_update (%s), got terminal=%v", why, terminal)
			return
		}
		msg := epResultError(newUpd)
		for _, s := range wantInError {
			if !strings.Contains(strings.ToLower(msg), strings.ToLower(s)) {
				bad("failure reason %q does not mention %q (%s)", msg, s, why)
			}
		}
		w.stat("attempt.failed." + why)
	}
	noCalls := func(why string) bool {
		if a.factoryCalls != 0 || len(a.calls) != 0 || a.reconcileCalls != 0 {
			bad("%s: no controller may be built or called", why)
			return false
		}
		return true
	}

	if otherWon {
		// Two executors of this worker were inside the op together; their
		// calls and reports interleave. The double-drive class above reports
		// it; the single-attempt plan does not apply.
		w.stat("attempt.overlapping_executors")
		return
	}
	if a.removedMid {
		// The hub failed the op (abort, drift, sweep) and this worker
		// applied the op_removed while driving it. Whether the executor
		// then stops or finishes is the act-after-removed class, flagged at
		// the call site; the plan and report shape are not pinned here.
		w.stat("attempt.removed_midrun")
		return
	}

	// --- refusals that must happen before any controller exists ---------
	if !a.inView && !a.disturbed {
		noCalls("machine not in view")
		expectFailed("not-in-view", "machine not in cache")
		return
	}
	if a.disturbed && a.factoryCalls == 0 && epResultError(newUpd) == "machine not in cache" {
		// A snapshot applied while the claim RPC was in flight removed the
		// machine: failing the op without touching anything is the only
		// legal outcome.
		noCalls("machine left the view mid-claim")
		expectFailed("left-view")
		return
	}
	reconcile := spec.typ == model.OpBMCReconcile
	switch {
	case spec.transport == "garbage" || spec.transport == "nullpdu" || spec.transport == "nullamt":
		noCalls("undecodable params")
		expectFailed("bad-params")
		return
	case reconcile && spec.transport != "bmc":
		noCalls("bmc_reconcile with a pdu/amt runtime")
		expectFailed("reconcile-transport", "IPMI LAN only")
		return
	case reconcile && !spec.adminCred:
		noCalls("bmc_reconcile without admin credential")
		expectFailed("reconcile-nocred", "bmc_runtime")
		return
	case reconcile && (spec.monitor == "empty" || spec.monitor == "rotate+stored"):
		noCalls("bmc_reconcile with unusable monitor runtime")
		expectFailed("reconcile-monitor", "monitor password")
		return
	case spec.transport == "both":
		noCalls("both pdu_runtime and amt_runtime")
		expectFailed("both-transports", "pdu_runtime", "amt_runtime")
		return
	case reconcile && m.bmcIP == "":
		noCalls("bmc_reconcile without BMC IP")
		expectFailed("reconcile-nobmc", "BMC IP")
		return
	case spec.transport == "bmc" && m.amt:
		noCalls("AMT machine on the BMC path")
		expectFailed("amt-as-bmc", "AMT")
		return
	case spec.transport == "bmc" && m.bmcIP == "":
		noCalls("no BMC IP and no envelope")
		expectFailed("no-bmc", "no BMC")
		return
	}
	if a.disturbed && a.factoryCalls == 0 {
		// The view changed under the attempt; "machine not in cache" is the
		// only legal reason to have built nothing.
		noCalls("machine left the view mid-attempt")
		expectFailed("left-view", "machine not in cache")
		return
	}

	// --- controller construction ---------------------------------------
	if a.factoryCalls != 1 {
		bad("want exactly one factory call, got %d", a.factoryCalls)
		return
	}
	wantArgs := fmt.Sprintf("pdu=%v amt=%v expected=%q", spec.transport == "pdu", spec.transport == "amt", spec.expected)
	if a.factoryArgs != wantArgs {
		bad("factory routed with %s, op spec says %s", a.factoryArgs, wantArgs)
	}
	if a.factoryFault != "ok" {
		if len(a.calls) != 0 || a.reconcileCalls != 0 {
			bad("controller construction failed (%s) but calls followed", a.factoryFault)
		}
		expectFailed("factory-" + a.factoryFault)
		return
	}

	// --- chassis-serial gate (BMC path only) ---------------------------
	calls := a.calls
	next := func() *epCall {
		if len(calls) == 0 {
			return nil
		}
		c := &calls[0]
		calls = calls[1:]
		return c
	}
	stop := func(why string, wantInError ...string) {
		for _, c := range calls {
			bad("%s, yet %s(%s) followed", why, c.name, c.arg)
			return
		}
		if a.reconcileCalls != 0 {
			bad("%s, yet the BMC reconcile ran", why)
		}
		expectFailed(why, wantInError...)
	}
	if spec.transport == "bmc" && spec.expected != "" {
		c := next()
		if c == nil || c.name != "ChassisSerial" {
			bad("the chassis-serial gate must be the first controller call on the BMC path")
			return
		}
		if c.err != nil {
			stop("serial-read-error", "serial check")
			return
		}
		got := strings.TrimSpace(a.serialGot)
		if got != "" && !strings.EqualFold(got, spec.expected) {
			w.stat("attempt.serial_mismatch")
			stop("serial-mismatch", "serial mismatch", spec.expected, got)
			return
		}
		if got == "" {
			w.stat("attempt.serial_blank")
		}
	} else {
		for _, c := range calls {
			if c.name == "ChassisSerial" && spec.transport != "bmc" {
				bad("the chassis-serial gate ran on the %s path", spec.transport)
			}
		}
		if spec.transport == "bmc" {
			w.stat("attempt.serial_skipped_empty")
		}
	}

	// --- per-type plan --------------------------------------------------
	want := func(name, arg string) *epCall {
		c := next()
		if c == nil || c.name != name || (arg != "" && c.arg != arg) {
			got := "nothing"
			if c != nil {
				got = c.name + "(" + c.arg + ")"
			}
			bad("want next call %s(%s), got %s", name, arg, got)
			return nil
		}
		return c
	}
	done := func() bool {
		if len(calls) != 0 {
			bad("unexpected extra call %s(%s)", calls[0].name, calls[0].arg)
			return false
		}
		return true
	}
	switch {
	case spec.powerAction():
		switch spec.bootDevice {
		case "pxe", "disk":
			c := want("SetBootDevice", spec.bootDevice)
			if c == nil {
				return
			}
			if c.err != nil {
				stop("setboot-failed")
				return
			}
		case "floppy":
			stop("bad-boot-device", "boot_device")
			return
		}
		act := map[model.OperationType]string{
			model.OpPowerOn: "PowerOn", model.OpPowerOff: "PowerOff", model.OpPowerCycle: "PowerCycle",
		}[spec.typ]
		c := want(act, "")
		if c == nil {
			return
		}
		if c.err != nil {
			stop("power-failed")
			return
		}
		if done() && !agentTouched && (len(terminal) != 1 || terminal[0] != string(model.OpCompleted)) {
			bad("power op succeeded: want exactly one completed op_update, got %v", terminal)
		}
		w.stat("attempt.completed.power")
	case spec.typ == model.OpSetBootDevice:
		if !m.bootOK || spec.transport != "bmc" {
			stop("bootmanager-unsupported", "not supported")
			return
		}
		if spec.device == "floppy" {
			stop("bad-device", "invalid boot device")
			return
		}
		c := want("SetBootOverride", fmt.Sprintf("%s/%v", spec.device, spec.persistent))
		if c == nil {
			return
		}
		if c.err != nil {
			stop("override-failed")
			return
		}
		if done() && (len(terminal) != 1 || terminal[0] != string(model.OpCompleted)) {
			bad("set_boot_device succeeded: want exactly one completed op_update, got %v", terminal)
		}
		w.stat("attempt.completed.setboot")
	case spec.lifecycle():
		// A host can run the agent on the pending-op boot clause before the
		// claim lands, and the agent can fail the op while the executor is
		// still driving it. The executor re-reads the op before each write
		// to the host and stops, without a report of its own, once this
		// worker has accepted the agent's failure.
		agentFailed := w.ha != nil && w.ha.agentFailed[a.op][pw.idx]
		stoppedForAgent := func(before string) bool {
			if !agentFailed || len(calls) != 0 {
				return false
			}
			w.stat("attempt.stopped_agent_failed@" + before)
			return true
		}
		if stoppedForAgent("SetBootDevice") {
			return
		}
		c := want("SetBootDevice", "pxe")
		if c == nil {
			return
		}
		if c.err != nil {
			stop("pxe-arm-failed")
			return
		}
		st := want("PowerStatus", "")
		if st == nil {
			return
		}
		act := "PowerCycle"
		if st.err == nil && st.arg == "off" {
			act = "PowerOn"
		}
		if stoppedForAgent(act) {
			return
		}
		c = want(act, "")
		if c == nil {
			return
		}
		if c.err != nil {
			stop("lifecycle-power-failed")
			return
		}
		if !done() {
			return
		}
		// A host can install on the pending-op boot clause before the claim
		// lands; the executor then drives an op already at
		// awaiting_phone_home, whose phase it must not move.
		latched := w.ha != nil && len(w.ha.installed[a.op]) > 0
		if !agentTouched && !latched {
			if len(terminal) != 0 {
				bad("lifecycle op booted: the executor must not report a terminal status, got %v", terminal)
			}
			if len(phases) != 2 || phases[0] != "power_cycle" || phases[1] != "pxe_booting" {
				bad("lifecycle op booted: want phases [power_cycle pxe_booting], got %v", phases)
			}
		}
		w.stat("attempt.booted.lifecycle")
	case reconcile:
		if !done() {
			return
		}
		if len(phases) == 0 || phases[0] != "bmc_reconcile" {
			bad("bmc_reconcile must record its phase before touching the BMC, got phases %v", phases)
		}
		if a.reconcileCalls != 1 {
			bad("want exactly one reconcile run, got %d", a.reconcileCalls)
			return
		}
		if strings.HasPrefix(a.reconcileRes, "nil") {
			if a.timeCalls != 0 {
				bad("reconcile wrote nothing: the time pass must not run")
			}
			if newObs != 0 {
				bad("reconcile wrote nothing: no report observation may be queued")
			}
			expectFailed("reconcile-" + a.reconcileRes)
			return
		}
		if newObs != 1 {
			bad("reconcile wrote to the BMC: want exactly one bmc_reconcile_report observation, got %d", newObs)
		}
		if len(terminal) != 0 {
			bad("reconcile reports through the observation only, got op_update status %v", terminal)
		}
		if a.timeCalls != 1 {
			bad("want one time pass after a reconcile that wrote, got %d", a.timeCalls)
		}
		for _, u := range newUpd {
			if strings.Contains(string(u.Result), "Fresh!pw1") || strings.Contains(string(u.Result), "St0red!pw") {
				bad("a monitor password rode an op_update")
			}
		}
		w.stat("attempt.reported.reconcile")
	}
}

func epCallsString(calls []epCall) string {
	var parts []string
	for _, c := range calls {
		s := c.name
		if c.arg != "" {
			s += "(" + strings.TrimSpace(c.arg) + ")"
		}
		if !c.issued {
			s += "[not-issued]"
		}
		if c.err != nil {
			s += "!err"
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// ---------------------------------------------------------------------------
// Quiescence
// ---------------------------------------------------------------------------

func (w *epWorld) fingerprint() string {
	var b strings.Builder
	for _, id := range w.hub.order {
		r := w.hub.ops[id]
		fmt.Fprintf(&b, "%s:%s:%d:%s|", id.String()[:4], r.op.Status, r.op.Version, r.op.Phase)
	}
	for _, pw := range w.wk {
		fmt.Fprintf(&b, "w%d:%d:%d:%d|", pw.idx, len(pw.inbox), len(pw.queuedUpdates()), len(pw.queuedObs()))
	}
	fmt.Fprintf(&b, "a%d", len(w.attemptLog))
	return b.String()
}

// settle stops all faults and lets the system converge: every worker
// reconnects, streams and outboxes are flushed, executors tick, until
// nothing changes.
func (w *epWorld) settle() {
	w.quiet = true
	if w.vipHolder() == nil {
		w.wk[0].acquireVIP()
	}
	w.logf("--- settle ---")
	for round := 0; round < 20; round++ {
		before := w.fingerprint()
		for _, pw := range w.wk {
			pw.connect()
			pw.deliverAll()
		}
		for _, pw := range w.wk {
			pw.tick()
		}
		w.runDeferred()
		for _, pw := range w.wk {
			pw.drain("")
		}
		for _, pw := range w.wk {
			pw.deliverAll()
		}
		if w.fingerprint() == before {
			return
		}
	}
	w.t.Fatalf("system did not converge in 20 settle rounds\n%s", strings.Join(w.trace, "\n"))
}

func (w *epWorld) checkQuiescence() {
	for _, id := range w.hub.order {
		rec := w.hub.ops[id]
		facts := w.byOp[id]
		spec := facts.spec
		m := w.mach[spec.mach]
		name := w.opName(id)
		w.stat("final." + string(rec.op.Status))
		var claimant *epWorker
		for _, pw := range w.wk {
			if pw.id == rec.op.ClaimedBy {
				claimant = pw
			}
		}
		crashed := false
		ran := false
		durableReport := false
		for _, a := range w.attemptLog {
			if a.op == id && a.outcome == "won" {
				ran = true
				crashed = crashed || a.crashed
				// A reconcile that wrote to the BMC reports through an
				// observation persisted in LocalDB: it survives restarts
				// and dropped connections.
				if !a.crashed && a.reconcileCalls == 1 && !strings.HasPrefix(a.reconcileRes, "nil") {
					durableReport = true
				}
			}
		}
		switch rec.op.Status {
		case model.OpPending:
			if !m.inSite {
				w.stat("final.pending.out_of_site")
				continue
			}
			if spec.lifecycle() {
				if _, ok := epClaimSources(spec.typ)[m.state]; !ok || m.locked {
					w.stat("final.pending.precondition")
					continue
				}
			}
			drivable := false
			for _, pw := range w.wk {
				if pw.active && (!spec.outlet || w.hub.outletEligible(spec, pw)) {
					drivable = true
				}
			}
			if !drivable {
				w.stat("final.pending.interlock")
				continue
			}
			w.violate("pending-not-driven", "%s is still pending at quiescence although a VIP holder could claim it", name)
		case model.OpClaimed:
			switch {
			case facts.phoneHomeRecorded:
				w.violate("phone-home-recorded-not-completed", "%s: a worker recorded the installed OS's phone-home (durable observation), yet at quiescence the hub still holds the deploy claimed (phase %q)",
					name, rec.op.Phase)
			case facts.phoneHomeAckedUnrecorded:
				w.violate("phone-home-acked-unrecorded", "%s: a worker answered the installed OS's phone-home with success without recording it; the hub still holds the deploy claimed (phase %q), the host will not phone home again and the phone-home sweep will fail it",
					name, rec.op.Phase)
			case !ran && !m.inSite:
				// Claim committed, reply lost, and the machine left the
				// site before the retry: nobody here may drive it.
				w.stat("final.claimed.out_of_site")
			case !ran && rec.op.Phase != "" && claimant != nil && claimant.active && claimant.gen == facts.grantGen:
				// The host PXE-booted on the pending-op clause and its boot
				// recorded a phase before the claim committed; the claim's
				// reply was then lost. The re-claim (opstate.CanReclaim) and
				// the orphaned-claim sweep both require an empty phase.
				w.violate("claim-reply-lost-after-boot-phase", "%s is claimed by w%d with its claim reply lost; its row already carried phase %q (the host booted on the pending-op clause), so the executor can never resume the claim and the orphaned-claim sweep never fails it",
					name, claimant.idx, rec.op.Phase)
			case !ran && claimant != nil && claimant.active && claimant.gen == facts.grantGen:
				w.violate("stall-claimed-not-driven", "%s is claimed by w%d (which holds the VIP and is connected and resynced) but w%d never drove it and never will: nothing re-claims a claimed row; it waits for the orphaned-claim sweep",
					name, claimant.idx, claimant.idx)
			case !ran:
				// Claimed by a worker process that then died or lost the
				// VIP for good: the documented orphaned-claim case.
				w.stat("final.claimed.claimant_gone")
			case durableReport:
				w.violate("report-missing", "%s: the worker queued a durable bmc_reconcile_report, yet at quiescence the hub still holds the op claimed (phase %q)", name, rec.op.Phase)
			case rec.op.Phase == "" || !epLifecycle(spec.typ):
				if facts.reportLossy || crashed {
					w.violate("report-lost", "%s was driven but its report never reached the hub (dropped write / restart); the op stays claimed until a sweep fails it", name)
				} else {
					w.violate("report-missing", "%s was driven to the end and nothing was lost in transit, yet the hub still holds it claimed (phase %q)", name, rec.op.Phase)
				}
			default:
				w.stat("final.claimed.awaiting_agent")
			}
		case model.OpCompleted:
			if !epLifecycle(spec.typ) && spec.typ != model.OpBMCReconcile && !ran {
				w.violate("completed-without-run", "%s is completed on the hub but no worker ever won and ran it", name)
			}
		case model.OpFailed:
			w.stat("final.failed_by." + facts.hubFailedBy)
		case model.OpRunning:
			// the reference hub never stores running: claimed covers it
		}
	}
	epStat.max("max.ops_per_case", int64(len(w.hub.order)))
	epStat.max("max.attempts_per_case", int64(len(w.attemptLog)))
	epStat.max("max.virtual_seconds", w.now)
}

// ---------------------------------------------------------------------------
// The property
// ---------------------------------------------------------------------------

func epRun(t *rapid.T, dir string, maxSteps int) {
	defer func() { epStat.add("cases", 1) }()
	w := newEpWorld(t, dir)
	// Swarm: each case enables a random subset of the action alphabet
	// (three in four actions on average), for top-level steps and for
	// interrupts alike.
	var enabled []string
	seen := map[string]bool{}
	for _, a := range epActions {
		if a.name == "create" || a.name == "tick" || a.name == "connect" || w.chance(3, 4, "swarm") {
			seen[a.name] = true
		}
	}
	for _, n := range epTopNames {
		if seen[n] {
			enabled = append(enabled, n)
		}
	}
	for _, n := range epInterruptNames {
		if seen[n] {
			w.interruptNames = append(w.interruptNames, n)
		}
	}
	// Start state: usually one VIP holder, connected and resynced.
	if !w.chance(1, 8, "coldStart") {
		w.wk[0].acquireVIP()
		for _, pw := range w.wk {
			pw.connect()
			pw.deliverAll()
		}
	}
	steps := 1 + w.pick(maxSteps, "steps")
	for i := 0; i < steps; i++ {
		w.step(epChoose(w, enabled, "action"), nil)
	}
	epStat.add("steps", int64(steps))
	w.settle()
	w.checkQuiescence()
}

func TestProp_ExecutorActuation(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(testDiscardLogger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	dir := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) { epRun(rt, dir, 60) })
	t.Logf("coverage (aggregate over all cases, shrinking included):\n%s", epStat.dump())
}
