package skewtest

// Upgrade and restart scenarios, end to end.
//
// One case is one small fleet: a Postgres database, a global, a site worker
// with its worker.db, and two or three machines. The global and the worker
// start on the "from" build. A generated history then mixes
//
//   - the deploy lifecycle, driven the way the outside world drives it:
//     the operator creates an op, the worker's executor claims and actuates
//     it, the machine fetches its iPXE script, the agent posts phase reports
//     and deploy_complete, the installed OS posts phone_home;
//   - faults: a dropped connection, a worker or global restart (clean or
//     kill -9), a stale or duplicate report, an abort, a reboot at any time;
//   - the upgrade itself: the global moves to the "to" build (its Migrate
//     runs against the populated database) and the worker moves to the "to"
//     build (it opens the worker.db the other build wrote), in either order
//     and at any point of the history.
//
// The oracle is a model kept by the test from what the outside world saw
// (which reports got a 2xx, which script a boot was served), never from the
// daemons' own state:
//
//	migrate    Migrate on the populated database loses or changes no row.
//	boot       A machine is served the installer only for an active
//	           lifecycle op the agent has not reported complete.
//	actuate    The executor power-cycles a host only for such an op.
//	complete   A deploy whose phone-home got a 2xx ends completed, with its
//	           machine running and locked.
//	honest     A deploy that ends completed had a phone-home 2xx.
//	settle     Once both sides run the "to" build and are connected, the
//	           outbox drains, every op the timeouts cover ends terminal, and
//	           the worker's view of every machine matches global's.
//	health     No process exits, hangs, or refuses to start on the state the
//	           other build left.
//
// TestSkew_Property_RollingRestart runs it with from == to (this tree): a release
// that changes no protocol. TestSkew_Property_UpgradeFromOldRelease runs it from the
// old release (see harness_test.go) to this tree.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"pgregory.net/rapid"
)

// ---------------------------------------------------------------- tool views

type opView struct {
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Phase     string `json:"phase"`
	ClaimedBy string `json:"claimed_by"`
	Result    string `json:"result"`
	Params    string `json:"params"`
	Version   int    `json:"version"`
}

type machineView struct {
	ID     string `json:"id"`
	Serial string `json:"serial"`
	State  string `json:"state"`
	Locked bool   `json:"locked"`
}

type gDump struct {
	Ops           []opView      `json:"ops"`
	Machines      []machineView `json:"machines"`
	Seq           int64         `json:"seq"`
	InstanceID    string        `json:"instance_id"`
	InstanceIDErr string        `json:"instance_id_err"`
	Workers       []string      `json:"workers"`
}

func (d gDump) op(id string) *opView {
	for i := range d.Ops {
		if d.Ops[i].ID == id {
			return &d.Ops[i]
		}
	}
	return nil
}

func (d gDump) machine(id string) *machineView {
	for i := range d.Machines {
		if d.Machines[i].ID == id {
			return &d.Machines[i]
		}
	}
	return nil
}

type wStatus struct {
	Connected bool  `json:"connected"`
	Outbox    int   `json:"outbox"`
	Seq       int64 `json:"seq"`
	Ops       map[string]struct {
		Status string `json:"status"`
		Phase  string `json:"phase"`
	} `json:"ops"`
	States     map[string]string         `json:"states"`
	Actuations map[string]map[string]int `json:"actuations"`
}

func terminal(status string) bool { return status == "completed" || status == "failed" }

// ---------------------------------------------------------------- model

// opRec is what the outside world knows about one operation.
type opRec struct {
	id        string
	typ       string
	lifecycle bool
	// What the worker answered 2xx to, and which worker process (generation)
	// answered.
	reports     int
	completeAck bool
	completeBy  string
	phoneAck    bool
	phoneAckBy  string
	failAck     bool
	failAckGen  int
	aborted     bool
	// actuated: the executor power-cycled the machine for this op before
	// its agent reported anything, which is how a deploy normally starts.
	// selfBooted: the agent started reporting with no such power cycle
	// seen, so the machine reached the installer on its own (an operator's
	// manual reboot, or the firmware's own PXE attempt) while the worker
	// could not claim the op. The executor may still claim such an op later
	// and power-cycle the host; the worker's awaiting_phone_home latch, not
	// the executor, is what protects the installed disk then.
	actuated   bool
	selfBooted bool
	// unlockedAfter: the operator unlocked the machine again after this op
	// was created (to create the next one).
	unlockedAfter bool
	createdBy     string
	createdAtGen  int
}

func (o *opRec) settled() bool { return o.phoneAck || o.failAck || o.aborted }

type mach struct {
	id, mac, serial string
	ops             []*opRec // lifecycle ops, oldest first
	powerOps        []*opRec
}

func (m *mach) cur() *opRec {
	if len(m.ops) == 0 {
		return nil
	}
	return m.ops[len(m.ops)-1]
}

type stats struct {
	cases, steps                                    int
	creates, createRefused, claims                  int
	reports2xx, reports409, reports410, reportsDown int
	completeAcks, phoneAcks, failAcks, aborts       int
	boots                                           map[string]int
	wsDrops, wRestarts, wKills, gRestarts, gKills   int
	upgradeGFirst, upgradeWFirst                    int
	midDeployUpgrades, awaitingAtUpgrade            int
	actuations, leases, stale, powerOps             int
	finalCompleted, finalFailed                     int
	refusedWindows                                  int
	outboxAtWorkerUpgrade                           int
	fromPhoneHomeLost                               int
	fromLatchLost                                   int
	lateClaimActuations                             int
	gDowns, wDowns                                  int
	lateClaimSetups                                 int
}

// ---------------------------------------------------------------- world

type world struct {
	t       *testing.T
	f       fataler
	st      *stats
	dbName  string
	dsn     string
	gAddr   string
	dbPath  string
	slug    string
	worker  string
	token   string
	binFrom string
	binTo   string
	verFrom string
	verTo   string

	g     *proc
	gVer  string // "from" or "to"; meaningful while g != nil or between restarts
	w     *proc
	wVer  string
	wHTTP string
	wGen  int
	gDown bool
	wDown bool

	machines []*mach
	trace    []string
	sql      *gorm.DB

	fromLatchInMemory bool
	// refuses is true when the two builds refuse each other's worker
	// connections.
	refuses bool
	// logScanned is how much of the current worker's log checkActuations
	// has read.
	logScanned int
	// noWait makes createOp return without waiting for the claim.
	noWait     bool
	healthOnly bool
}

func (w *world) note(format string, args ...any) {
	w.trace = append(w.trace, fmt.Sprintf(format, args...))
}

func (w *world) fail(format string, args ...any) {
	w.f.Fatalf("%s\n\ntrace:\n  %s\n\nglobal stderr tail:\n%s\n\nworker stderr tail:\n%s",
		fmt.Sprintf(format, args...), strings.Join(w.trace, "\n  "), w.stderrTail(w.g), w.stderrTail(w.w))
}

func (w *world) stderrTail(p *proc) string {
	if p == nil {
		return "(not running)"
	}
	return p.stderr.tail(9000)
}

func (w *world) bin(ver string) string {
	if ver == "to" {
		return w.binTo
	}
	return w.binFrom
}

func (w *world) tag(ver string) string {
	if ver == "to" {
		return w.verTo
	}
	return w.verFrom
}

// must fails the case on a tool error: a refusal to start, a crash, a hang.
func (w *world) must(what string, err error) {
	if err != nil {
		w.fail("health: %s: %v", what, err)
	}
}

func (w *world) macs() map[string]string {
	out := map[string]string{}
	for _, m := range w.machines {
		out[m.id] = m.mac
	}
	return out
}

func (w *world) startGlobal(ver string) {
	var p *proc
	var err error
	// The address was free when the case picked it and has to stay the
	// same across restarts (it is the worker's configured global URL), but
	// the kernel can still be releasing it, or a parallel case can hold it
	// for a moment.
	for attempt := 0; attempt < 40; attempt++ {
		p = startProc(w.f, w.bin(ver), "global-"+w.tag(ver))
		err = p.call("g.start", map[string]any{
			"dsn": w.dsn, "addr": w.gAddr, "slug": w.slug, "version": w.tag(ver), "macs": w.macs(),
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			break
		}
		p.kill()
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil && strings.Contains(err.Error(), "address already in use") {
		// Something outside this test took the port while global was
		// down. That says nothing about slagworks: drop the case.
		p.kill()
		if sk, ok := w.f.(interface{ Skipf(string, ...any) }); ok {
			sk.Skipf("harness: the global's port %s was taken by another process: %v", w.gAddr, err)
		}
	}
	if err != nil {
		w.g = p
		w.fail("health: global (%s build) did not start on this database: %v", w.tag(ver), err)
	}
	w.g, w.gVer, w.gDown = p, ver, false
}

func (w *world) stopGlobal(kill bool) {
	if w.g == nil {
		return
	}
	if kill {
		w.g.kill()
	} else {
		w.must("g.stop", w.g.call("g.stop", struct{}{}, nil))
		w.g.quit()
	}
	w.g, w.gDown = nil, true
}

func (w *world) startWorker(ver string) {
	var p *proc
	var err error
	// The worker's HTTP port can be any port; if another process on the
	// host takes the one picked before the tool binds it, pick again.
	for attempt := 0; attempt < 20; attempt++ {
		p = startProc(w.f, w.bin(ver), "worker-"+w.tag(ver))
		w.wHTTP = freePort(w.f)
		err = p.call("w.start", map[string]any{
			"db": w.dbPath, "url": "http://" + w.gAddr, "token": w.token, "worker_id": w.worker,
			"serial": "SKEWWORKER1", "http_addr": w.wHTTP, "version": w.tag(ver),
		}, nil)
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			break
		}
		p.kill()
	}
	if err != nil {
		w.w = p
		w.fail("health: worker (%s build) did not start on this worker.db: %v", w.tag(ver), err)
	}
	w.w, w.wVer, w.wDown = p, ver, false
	w.wGen++
	w.logScanned = 0
}

func (w *world) stopWorker(kill bool) {
	if w.w == nil {
		return
	}
	if kill {
		w.w.kill()
	} else {
		w.must("w.stop", w.w.call("w.stop", struct{}{}, nil))
		w.w.quit()
	}
	w.w, w.wDown = nil, true
}

func (w *world) dump() gDump {
	var d gDump
	w.must("g.dump", w.g.call("g.dump", struct{}{}, &d))
	return d
}

func (w *world) status() wStatus {
	var ids, mids []string
	for _, m := range w.machines {
		mids = append(mids, m.id)
		for _, o := range m.ops {
			ids = append(ids, o.id)
		}
		for _, o := range m.powerOps {
			ids = append(ids, o.id)
		}
	}
	var s wStatus
	w.must("w.status", w.w.call("w.status", map[string]any{"ops": ids, "machines": mids}, &s))
	return s
}

// linked reports whether the two sides run builds that can talk and both
// are up. Whether they are connected right now is another matter.
func (w *world) linked() bool {
	return w.g != nil && w.w != nil && (w.gVer == w.wVer || w.binFrom == w.binTo || !w.refuses)
}

// ---------------------------------------------------------------- steps

func (w *world) createOp(m *mach, typ string) {
	if w.g == nil {
		return
	}
	var out struct {
		OpID string `json:"op_id"`
	}
	// An operator unlocks a running machine to redeploy it; nothing else
	// needs the lock off.
	unlock := typ == "deploy"
	if unlock {
		if o := m.cur(); o != nil {
			o.unlockedAfter = true
		}
	}
	err := w.g.call("g.create_op", map[string]any{"machine_id": m.id, "type": typ, "unlock": unlock}, &out)
	if errors.Is(err, errToolDied) {
		w.must("g.create_op", err)
	}
	if err != nil {
		w.st.createRefused++
		w.note("create %s on %s refused: %v", typ, m.serial, err)
		return
	}
	w.st.creates++
	rec := &opRec{id: out.OpID, typ: typ, lifecycle: typ == "deploy" || typ == "commission", createdBy: w.gVer, createdAtGen: w.wGen}
	if rec.lifecycle {
		m.ops = append(m.ops, rec)
	} else {
		m.powerOps = append(m.powerOps, rec)
		w.st.powerOps++
	}
	w.note("create %s %s on %s (global %s)", typ, short(out.OpID), m.serial, w.tag(w.gVer))
	// Give a connected executor the moment it needs to claim and actuate.
	if w.linked() && !w.noWait {
		claimed := eventually(3*time.Second, func() bool {
			op := w.dump().op(out.OpID)
			return op != nil && op.Status != "pending"
		})
		if claimed {
			w.st.claims++
		}
	}
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// report posts one agent or installed-OS report to the worker's HTTP
// surface and folds the answer into the model.
func (w *world) report(m *mach, o *opRec, phase, event string, failed bool) int {
	if w.w == nil {
		w.st.reportsDown++
		return 0
	}
	// Count the executor's writes before this report can change what the
	// model allows: a power cycle that preceded a deploy_complete must not
	// be judged against it.
	w.checkActuations()
	if o == m.cur() && o.reports == 0 && !o.actuated {
		o.selfBooted = true
	}
	body := map[string]any{"machine_id": m.id, "op_id": o.id}
	if phase != "" {
		body["phase"] = phase
	}
	if event != "" {
		body["event"] = event
	}
	if failed {
		body["failed"] = true
		body["error"] = "skewtest: injected agent failure"
	}
	code, text := post("http://"+w.wHTTP+"/api/v1/report", body)
	w.note("report %s op=%s phase=%q event=%q failed=%v -> %d %s (worker %s)", m.serial, short(o.id), phase, event, failed, code, text, w.tag(w.wVer))
	switch {
	case code == 0:
		w.st.reportsDown++
	case code/100 == 2:
		w.st.reports2xx++
		o.reports++
		switch {
		case failed:
			if !o.failAck {
				w.st.failAcks++
			}
			o.failAck, o.failAckGen = true, w.wGen
		case event == "deploy_complete":
			if !o.completeAck {
				w.st.completeAcks++
				o.completeBy = w.wVer
			}
			o.completeAck = true
		case event == "phone_home":
			// The tree under test documents when a phone-home may be
			// accepted: only for a deploy the agent reported complete, or as
			// a retry of one already accepted.
			if w.wVer == "to" && !w.healthOnly && !o.completeAck && !o.phoneAck {
				w.fail("honest: worker (%s build) answered %d to a phone_home for op %s whose agent never got a 2xx for deploy_complete", w.tag(w.wVer), code, o.id)
			}
			if !o.phoneAck {
				w.st.phoneAcks++
				o.phoneAckBy = w.wVer
			}
			o.phoneAck = true
		}
	case code == 409:
		w.st.reports409++
	case code == 410:
		w.st.reports410++
	}
	return code
}

func classify(script string) string {
	// The banner each script echoes is the same in every release so far. A
	// script that loads a kernel under any other banner still counts as an
	// installer boot: the question is what the machine would run.
	switch {
	case strings.Contains(script, "slagworks: deploy boot"):
		return "deploy"
	case strings.Contains(script, "slagworks: commission boot"), strings.Contains(script, "slagworks.mode=commission"):
		return "commission"
	case strings.Contains(script, "\nkernel "):
		return "deploy"
	case strings.Contains(script, "boot-local"):
		return "local"
	default:
		return "other"
	}
}

// boot fetches the machine's iPXE script, as its firmware does on every
// network boot, and checks what it was served.
func (w *world) boot(m *mach) string {
	if w.w == nil {
		return ""
	}
	code, script := get("http://" + w.wHTTP + "/boot/ipxe?mac=" + m.mac)
	if code != 200 {
		w.note("boot %s -> HTTP %d", m.serial, code)
		return ""
	}
	class := classify(script)
	w.st.boots[class]++
	w.note("boot %s -> %s (worker %s)", m.serial, class, w.tag(w.wVer))
	if class == "other" {
		w.note("  script: %q", script)
	}
	if class != "deploy" && class != "commission" {
		return class
	}
	o := m.cur()
	switch {
	case o == nil:
		w.fail("boot: %s was served the %s installer but no lifecycle op was ever created for it", m.serial, class)
	case o.typ != class:
		w.fail("boot: %s was served the %s installer but its current op %s is a %s", m.serial, class, o.id, o.typ)
	case w.healthOnly:
		// The boot decision is the "to" build's business.
	case (o.completeAck || o.phoneAck) && w.fromLatchInMemory && (o.completeBy == "from" || o.phoneAckBy == "from"):
		// See scenarioConfig.fromLatchInMemory.
		w.st.fromLatchLost++
	case o.completeAck || o.phoneAck:
		w.fail("boot: %s was served the %s installer although the agent's deploy_complete for its current op %s was accepted (complete=%v by the %s worker, phone_home=%v): the installed system would be wiped again",
			m.serial, class, o.id, o.completeAck, w.tag(o.completeBy), o.phoneAck)
	case o.failAck && o.failAckGen == w.wGen:
		w.fail("boot: %s was served the %s installer although this worker process accepted the agent's failure report for op %s", m.serial, class, o.id)
	}
	return class
}

// checkActuations compares the fake BMC's write counters with what the
// model allows.
func (w *world) checkActuations() {
	if w.w == nil {
		return
	}
	// The executor logs each lifecycle power write with the op it is for
	// ("PXE boot set, cycling power" or "powering on", the same line in
	// every release so far). Reading that, not the fake BMC's per-machine
	// counter, is what tells an aborted op's late power cycle from the
	// next op's first.
	log := w.w.stderr.String()
	fresh := log[w.logScanned:]
	if cut := strings.LastIndexByte(fresh, '\n'); cut >= 0 {
		fresh = fresh[:cut+1]
	} else {
		fresh = ""
	}
	w.logScanned += len(fresh)
	for _, line := range strings.Split(fresh, "\n") {
		if !strings.Contains(line, `"msg":"PXE boot set, `) {
			continue
		}
		var entry struct {
			OpID      string `json:"op_id"`
			MachineID string `json:"machine_id"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry.OpID == "" {
			w.fail("harness: cannot read the executor's power-write log line: %v: %s", err, line)
		}
		var m *mach
		var o *opRec
		for _, cand := range w.machines {
			for _, rec := range cand.ops {
				if rec.id == entry.OpID {
					m, o = cand, rec
				}
			}
		}
		w.st.actuations++
		if o == nil {
			w.fail("actuate: the executor power-cycled machine %s for op %s, which the harness never created as a lifecycle op", entry.MachineID, entry.OpID)
		}
		w.note("executor actuated %s for op %s", m.serial, short(o.id))
		switch {
		case w.healthOnly:
		case (o.completeAck || o.phoneAck) && o.selfBooted:
			w.st.lateClaimActuations++
		case o.completeAck || o.phoneAck:
			w.fail("actuate: the executor power-cycled %s into PXE for op %s after the agent reported that op complete (complete=%v phone_home=%v), and the host had not booted on its own before the claim",
				m.serial, o.id, o.completeAck, o.phoneAck)
		case o.actuated && o.reports > 0:
			w.fail("actuate: the executor power-cycled %s into PXE a second time for op %s, after its agent had started reporting", m.serial, o.id)
		case o.reports == 0:
			o.actuated = true
		}
		// A power-cycled machine PXE-boots next: see what it is served.
		w.boot(m)
	}
}

var agentPhases = []string{
	"safety check", "resolving storage layout", "partitioning", "rsyncing_os",
	"installing bootloader", "applying network config", "BMC reconcile", "finalizing",
}

// advance performs the next natural event of m's deploy.
func (w *world) advance(rt *rapid.T, m *mach) {
	o := m.cur()
	switch {
	case o == nil || o.settled():
		w.createOp(m, "deploy")
	case o.reports == 0:
		// The machine reaches the installer because the executor
		// power-cycled it. Wait for that when the worker can claim; when it
		// cannot, the machine boots on its own (report marks it).
		if !o.actuated && w.linked() {
			eventually(2*time.Second, func() bool {
				w.checkActuations()
				return o.actuated
			})
		}
		if w.boot(m) == "" && w.w == nil {
			return
		}
		w.report(m, o, rapid.SampledFrom(agentPhases).Draw(rt, "phase"), "", false)
	case !o.completeAck:
		if rapid.Bool().Draw(rt, "finish") {
			w.report(m, o, "", "deploy_complete", false)
		} else {
			w.report(m, o, rapid.SampledFrom(agentPhases).Draw(rt, "phase"), "", false)
		}
	default:
		// The agent reboots into the installed system, which phones home.
		w.boot(m)
		w.report(m, o, "", "phone_home", false)
	}
}

func (w *world) upgradeGlobal() {
	// Count what the upgrade interrupts.
	for _, m := range w.machines {
		if o := m.cur(); o != nil && !o.settled() {
			w.st.midDeployUpgrades++
			if o.completeAck {
				w.st.awaitingAtUpgrade++
			}
		}
	}
	w.stopGlobal(false)
	// A statement the stopped global had on the wire still runs to
	// completion on the server; read the rows only once nothing else is
	// connected to this database.
	quiet := func() bool {
		var n int
		if err := w.sql.Raw(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`).Scan(&n).Error; err != nil {
			w.fail("harness: pg_stat_activity: %v", err)
		}
		return n == 0
	}
	if !eventually(10*time.Second, quiet) {
		w.fail("harness: sessions of the stopped global are still open on the database after 10s")
	}
	before := w.tableRows()
	// Migrate alone first, with no worker able to reach the hub, so the
	// comparison sees only what the migrations did.
	p := startProc(w.f, w.binTo, "migrate-"+w.verTo)
	var info struct {
		Before int64 `json:"schema_before"`
	}
	if err := p.call("g.start", map[string]any{"dsn": w.dsn, "addr": "", "slug": w.slug, "version": w.verTo, "macs": w.macs()}, &info); err != nil {
		w.g = p
		w.fail("migrate: the %s build's Migrate failed on the database the %s build populated: %v", w.verTo, w.verFrom, err)
	}
	var d gDump
	if err := p.call("g.dump", struct{}{}, &d); err != nil {
		w.g = p
		w.fail("migrate: the %s build cannot read the migrated database: %v", w.verTo, err)
	}
	if d.InstanceID == "" && !w.healthOnly {
		w.g = p
		w.fail("migrate: no instance ID after the %s build's Migrate: %s", w.verTo, d.InstanceIDErr)
	}
	_ = p.call("g.stop", struct{}{}, nil)
	p.quit()
	after := w.tableRows()
	if diff := lostRows(before, after); diff != "" {
		w.fail("migrate: rows written by the %s build did not survive the %s build's Migrate (schema %d before):\n%s", w.verFrom, w.verTo, info.Before, diff)
	}
	w.note("global upgraded %s -> %s (schema was %d)", w.verFrom, w.verTo, info.Before)
	w.startGlobal("to")
}

func (w *world) upgradeWorker() {
	w.stopWorker(false)
	if n := outboxRows(w.f, w.dbPath); n > 0 {
		w.st.outboxAtWorkerUpgrade += n
	}
	w.note("worker upgraded %s -> %s", w.verFrom, w.verTo)
	w.startWorker("to")
}

// ---------------------------------------------------------------- database rows

// tableRows reads every row of every table as JSON text, keyed by table.
func (w *world) tableRows() map[string][]map[string]json.RawMessage {
	var tables []string
	if err := w.sql.Raw(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name <> 'goose_db_version' ORDER BY 1`).Scan(&tables).Error; err != nil {
		w.fail("harness: list tables: %v", err)
	}
	out := map[string][]map[string]json.RawMessage{}
	for _, tbl := range tables {
		var blob string
		if err := w.sql.Raw(`SELECT COALESCE(jsonb_agg(to_jsonb(t))::text, '[]') FROM ` + tbl + ` t`).Scan(&blob).Error; err != nil {
			w.fail("harness: read %s: %v", tbl, err)
		}
		var rows []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(blob), &rows); err != nil {
			w.fail("harness: decode %s: %v", tbl, err)
		}
		out[tbl] = rows
	}
	return out
}

// lostRows reports every table or row of before that after no longer holds
// with the same value in every column before had. Added tables, rows and
// columns are fine.
func lostRows(before, after map[string][]map[string]json.RawMessage) string {
	var out []string
	var tables []string
	for t := range before {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	for _, t := range tables {
		arows, ok := after[t]
		if !ok {
			if len(before[t]) > 0 {
				out = append(out, fmt.Sprintf("table %s (%d rows) is gone", t, len(before[t])))
			}
			continue
		}
		used := make([]bool, len(arows))
		for _, br := range before[t] {
			found := false
			for i, ar := range arows {
				if used[i] {
					continue
				}
				same := true
				for col, v := range br {
					if av, ok := ar[col]; !ok || string(av) != string(v) {
						same = false
						break
					}
				}
				if same {
					used[i], found = true, true
					break
				}
			}
			if !found {
				b, _ := json.Marshal(br)
				out = append(out, fmt.Sprintf("%s: row changed or lost: %s", t, b))
			}
		}
	}
	return strings.Join(out, "\n")
}

// seedHistory inserts terminal operations in the statuses and phases a
// long-lived deployment's operations table holds: free-form agent phases
// from several releases, failures with and without a phase, results of each
// shape.
func (w *world) seedHistory() {
	rows := []struct{ typ, status, phase, result string }{
		{"deploy", "completed", "awaiting_phone_home", `null`},
		{"deploy", "completed", "completed", `null`},
		{"deploy", "failed", "safety check", `{"error":"refusing to wipe","phase":"safety check"}`},
		{"deploy", "failed", "storage: mdadm", `{"error":"exit status 1"}`},
		{"deploy", "failed", "installing_bootloader", `{"error":"grub-install failed"}`},
		{"deploy", "failed", "", `{"error":"operation timed out"}`},
		{"commission", "completed", "console_probe", `{"cpus":[],"disks":[],"hostname":"h","nics":[],"system":{}}`},
		{"commission", "failed", "dhcp_ack", `{"error":"aborted by operator"}`},
		{"power_cycle", "completed", "", `null`},
		{"power_off", "failed", "", `{"error":"BMC unreachable"}`},
		{"bmc_reconcile", "completed", "bmc_reconcile", `{"bmc_reconcile":{"monitor_rotated":false}}`},
		{"set_boot_device", "completed", "", `null`},
	}
	for i, r := range rows {
		m := w.machines[i%len(w.machines)]
		if err := w.sql.Exec(`INSERT INTO operations
			(id, machine_id, trace_id, type, status, phase, phase_entered_at, claimed_by, claimed_at, created_at, completed_at, params, result, version)
			VALUES (gen_random_uuid(), ?, gen_random_uuid(), ?, ?, ?, now() - interval '40 days', ?, now() - interval '40 days',
			        now() - interval '40 days', now() - interval '39 days', '{"destructive":true}'::jsonb, ?::jsonb, 4)`,
			m.id, r.typ, r.status, r.phase, w.worker, r.result).Error; err != nil {
			w.fail("harness: seed history row %d: %v", i, err)
		}
	}
}

// ---------------------------------------------------------------- the scenario

type scenarioConfig struct {
	binFrom, binTo string
	verFrom, verTo string
	refuses        bool
	// fromPhoneHomeUnbound exempts, from the completion check, a deploy
	// whose phone-home was accepted by the "from" build. A pre-0.6 worker
	// queued its phone_home observation without the op ID, and global
	// deliberately does not bind such a row to whatever deploy the machine
	// has open (TestPhoneHomeObservationWithoutOpID in internal/commission
	// pins that), so a completion that had not reached global when the
	// upgrade began ends at the phone-home timeout. That is the old
	// release's behavior, not this tree's; the property counts these.
	fromPhoneHomeUnbound bool
	// fromLatchInMemory exempts, from the boot check, a deploy whose
	// deploy_complete was accepted by the "from" build. A pre-0.6 worker
	// kept the awaiting_phone_home latch in memory until global echoed the
	// phase back, so one stopped before that echo restarts without it
	// (TestSkew_Property_RollingRestart checks that this tree's worker does not).
	fromLatchInMemory bool
	// healthOnly keeps the checks that do not depend on how the "to" build
	// handles a deploy: every process starts on the state the other build
	// left, Migrate loses no row, nothing crashes or hangs, and the two
	// sides connect once both run the "to" build. A rollback to an older
	// release runs with it: the older release's own lifecycle bugs are not
	// this tree's to fix.
	healthOnly bool
}

func runScenario(t *testing.T, rt *rapid.T, cfg scenarioConfig, st *stats, dir string) {
	st.cases++
	w := &world{
		t: t, f: rt, st: st, slug: "site-a", worker: "worker-1",
		binFrom: cfg.binFrom, binTo: cfg.binTo, verFrom: cfg.verFrom, verTo: cfg.verTo,
		fromLatchInMemory: cfg.fromLatchInMemory, refuses: cfg.refuses, healthOnly: cfg.healthOnly,
	}
	w.dbName, w.dsn = freshDatabase(t, rt, "")
	defer dropDatabase(w.dbName)
	sqlDB, err := gorm.Open(postgres.Open(w.dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		rt.Fatalf("harness: open case database: %v", err)
	}
	w.sql = sqlDB
	defer func() {
		if s, err := sqlDB.DB(); err == nil {
			_ = s.Close()
		}
	}()
	w.gAddr = ports.get(rt, true)
	defer ports.release(w.gAddr)
	caseDir := filepath.Join(dir, fmt.Sprintf("case-%d", st.cases))
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		rt.Fatalf("harness: %v", err)
	}
	defer func() { _ = os.RemoveAll(caseDir) }()
	w.dbPath = filepath.Join(caseDir, "worker.db")
	defer func() {
		if w.w != nil {
			w.w.kill()
		}
		if w.g != nil {
			w.g.kill()
		}
	}()

	// The fleet, on the "from" build.
	w.startGlobal("from")
	nMachines := rapid.IntRange(2, 3).Draw(rt, "machines")
	type seedMachine struct {
		Serial string `json:"serial"`
		MAC    string `json:"mac"`
		State  string `json:"state"`
		Locked bool   `json:"locked"`
	}
	var seeds []seedMachine
	for i := 0; i < nMachines; i++ {
		state := rapid.SampledFrom([]string{"commissioned", "commissioned", "running", "failed"}).Draw(rt, "state")
		seeds = append(seeds, seedMachine{
			Serial: fmt.Sprintf("SKEW%04d", i), MAC: fmt.Sprintf("02:00:00:00:00:%02x", i+1),
			State: state, Locked: state == "running",
		})
	}
	var seeded struct {
		Tokens   map[string]string `json:"tokens"`
		Machines []string          `json:"machines"`
	}
	w.must("g.seed", w.g.call("g.seed", map[string]any{"workers": []string{w.worker}, "machines": seeds}, &seeded))
	w.token = seeded.Tokens[w.worker]
	for i, id := range seeded.Machines {
		w.machines = append(w.machines, &mach{id: id, mac: seeds[i].MAC, serial: seeds[i].Serial})
	}
	w.must("g.seed_extra", w.g.call("g.seed_extra", struct{}{}, nil))
	w.seedHistory()
	w.startWorker("from")
	if !eventually(20*time.Second, func() bool { return w.status().Connected }) {
		w.fail("harness: the %s worker never connected to the %s global", w.verFrom, w.verFrom)
	}

	const (
		kAdvance = iota
		kBoot
		kStale
		kFail
		kAbort
		kWSDrop
		kWorkerRestart
		kGlobalRestart
		kLease
		kPower
		kUpgradeGlobal
		kUpgradeWorker
		kDrive
		kGlobalToggle
		kWorkerToggle
		kLateClaim
	)
	weights := []int{kAdvance, kAdvance, kAdvance, kAdvance, kAdvance, kAdvance,
		kDrive, kDrive, kDrive, kDrive,
		kBoot, kBoot, kStale, kStale, kFail, kAbort, kWSDrop, kWSDrop, kWorkerRestart, kWorkerRestart, kGlobalRestart, kLease, kLease, kPower,
		kGlobalToggle, kGlobalToggle, kWorkerToggle, kLateClaim, kLateClaim, kLateClaim,
		kUpgradeGlobal, kUpgradeWorker}
	steps := rapid.IntRange(6, 30).Draw(rt, "steps")
	firstUpgrade := ""
	for i := 0; i < steps; i++ {
		st.steps++
		m := w.machines[rapid.IntRange(0, len(w.machines)-1).Draw(rt, "machine")]
		switch rapid.SampledFrom(weights).Draw(rt, "kind") {
		case kAdvance:
			w.advance(rt, m)
		case kDrive:
			// Several lifecycle events in a row on one machine, so faults
			// land late in a deploy as often as early.
			for n := rapid.IntRange(2, 5).Draw(rt, "drive"); n > 0; n-- {
				w.advance(rt, m)
			}
		case kBoot:
			w.boot(m)
		case kStale:
			// A report nobody should be waiting for: any kind, against any
			// op this machine ever had.
			if len(m.ops) == 0 {
				continue
			}
			o := m.ops[rapid.IntRange(0, len(m.ops)-1).Draw(rt, "op")]
			st.stale++
			switch rapid.IntRange(0, 2).Draw(rt, "what") {
			case 0:
				w.report(m, o, rapid.SampledFrom(agentPhases).Draw(rt, "phase"), "", false)
			case 1:
				// deploy_complete is only what an agent that finished would
				// send; replay it for an op whose agent did.
				if o.completeAck {
					w.report(m, o, "", "deploy_complete", false)
				}
			case 2:
				w.report(m, o, "", "phone_home", false)
			}
		case kFail:
			if o := m.cur(); o != nil && !o.completeAck && !o.settled() && o.reports > 0 {
				w.report(m, o, "partitioning", "", true)
			}
		case kAbort:
			// An operator aborts only what has not phoned home; the model's
			// completion check exempts aborted ops.
			if o := m.cur(); w.g != nil && o != nil && !o.phoneAck && !o.settled() {
				var out struct {
					Failed []string `json:"failed"`
				}
				w.must("g.abort", w.g.call("g.abort", map[string]any{"machine_id": m.id}, &out))
				for _, id := range out.Failed {
					for _, rec := range append(append([]*opRec{}, m.ops...), m.powerOps...) {
						if rec.id == id {
							rec.aborted = true
						}
					}
				}
				st.aborts++
				w.note("abort %s -> failed %v", m.serial, out.Failed)
			}
		case kWSDrop:
			if w.w != nil {
				w.must("w.disconnect", w.w.call("w.disconnect", struct{}{}, nil))
				st.wsDrops++
				w.note("ws drop")
			}
		case kWorkerRestart:
			kill := rapid.Bool().Draw(rt, "kill")
			ver := w.wVer
			w.stopWorker(kill)
			w.startWorker(ver)
			if kill {
				st.wKills++
			} else {
				st.wRestarts++
			}
			w.note("worker restart (kill=%v, %s)", kill, w.tag(ver))
		case kGlobalRestart:
			kill := rapid.Bool().Draw(rt, "kill")
			ver := w.gVer
			w.stopGlobal(kill)
			w.startGlobal(ver)
			if kill {
				st.gKills++
			} else {
				st.gRestarts++
			}
			w.note("global restart (kill=%v, %s)", kill, w.tag(ver))
		case kLateClaim:
			// A deploy the worker holds but cannot claim (global's claim
			// path is failing, as it does while its database is
			// unreachable), on a machine that reaches the installer anyway
			// because someone rebooted it. The install runs to some point;
			// then claims work again, and the executor claims the op and
			// power-cycles a host that may already be installed.
			if w.g == nil || w.w == nil || !w.linked() {
				continue
			}
			// Any machine with no deploy in flight will do.
			var target *mach
			for _, cand := range append([]*mach{m}, w.machines...) {
				if o := cand.cur(); o == nil || o.settled() {
					target = cand
					break
				}
			}
			if target == nil {
				continue
			}
			w.must("g.fail_claims", w.g.call("g.fail_claims", map[string]any{"on": true}, nil))
			before := len(target.ops)
			w.noWait = true
			w.createOp(target, "deploy")
			w.noWait = false
			if len(target.ops) > before {
				o := target.cur()
				// Wait until the worker holds the op.
				eventually(2*time.Second, func() bool {
					_, ok := w.status().Ops[o.id]
					return ok
				})
				st.lateClaimSetups++
				w.boot(target)
				w.report(target, o, rapid.SampledFrom(agentPhases).Draw(rt, "phase"), "", false)
				if rapid.IntRange(0, 3).Draw(rt, "installed") > 0 {
					w.report(target, o, "", "deploy_complete", false)
					if rapid.Bool().Draw(rt, "phoned") {
						w.boot(target)
						w.report(target, o, "", "phone_home", false)
					}
				}
			}
			if w.g != nil {
				w.must("g.fail_claims", w.g.call("g.fail_claims", map[string]any{"on": false}, nil))
				// The executor retries the claim on its next tick; if it
				// wins, it power-cycles the host, and checkActuations
				// looks at what the host is then served.
				time.Sleep(200 * time.Millisecond)
				w.checkActuations()
			}
		case kGlobalToggle:
			// Global goes away and stays away (or comes back). Half the
			// time an op is created just before, so the worker is left
			// holding an op it may not have claimed yet.
			if w.g != nil {
				if rapid.Bool().Draw(rt, "strand") {
					w.noWait = true
					w.createOp(m, "deploy")
					w.noWait = false
				}
				w.stopGlobal(rapid.Bool().Draw(rt, "kill"))
				st.gDowns++
				w.note("global down (%s)", w.tag(w.gVer))
			} else {
				w.startGlobal(w.gVer)
				w.note("global up (%s)", w.tag(w.gVer))
			}
		case kWorkerToggle:
			if w.w != nil {
				w.stopWorker(rapid.Bool().Draw(rt, "kill"))
				st.wDowns++
				w.note("worker down (%s)", w.tag(w.wVer))
			} else {
				w.startWorker(w.wVer)
				w.note("worker up (%s)", w.tag(w.wVer))
			}
		case kLease:
			if w.w != nil {
				w.must("w.dhcp_lease", w.w.call("w.dhcp_lease", map[string]any{
					"machine_id": m.id, "event": rapid.SampledFrom([]string{"allocate", "renew", "delete"}).Draw(rt, "lease"),
					"mac": m.mac, "ip": fmt.Sprintf("192.0.2.%d", 20+rapid.IntRange(0, 2).Draw(rt, "ip")),
				}, nil))
				st.leases++
			}
		case kPower:
			w.createOp(m, "power_cycle")
		case kUpgradeGlobal:
			if w.gVer == "from" {
				if firstUpgrade == "" {
					firstUpgrade = "global"
					st.upgradeGFirst++
				}
				w.upgradeGlobal()
			}
		case kUpgradeWorker:
			if w.wVer == "from" {
				if firstUpgrade == "" {
					firstUpgrade = "worker"
					st.upgradeWFirst++
				}
				w.upgradeWorker()
			}
		}
		if cfg.refuses && w.g != nil && w.w != nil && w.gVer != w.wVer {
			st.refusedWindows++
		}
		w.checkActuations()
	}

	// Finish the upgrade and let the fleet settle on the "to" build.
	if w.gVer == "from" {
		if firstUpgrade == "" {
			st.upgradeGFirst++
		}
		w.upgradeGlobal()
	}
	if w.g == nil {
		w.startGlobal("to")
	}
	if w.wVer == "from" {
		w.upgradeWorker()
	}
	if w.w == nil {
		w.startWorker("to")
	}
	w.note("-- settle --")
	if !eventually(60*time.Second, func() bool { return w.status().Connected }) {
		w.fail("settle: with both sides on the %s build, the worker did not connect within 60s", w.verTo)
	}
	if cfg.healthOnly {
		// One more exchange in each direction, to show the pair works.
		d := w.dump()
		_ = w.status()
		if len(d.Machines) != len(w.machines) {
			w.fail("health: global lists %d machines after the scenario, the fleet has %d", len(d.Machines), len(w.machines))
		}
		return
	}
	if !eventually(45*time.Second, func() bool { return outboxRows(w.f, w.dbPath) == 0 }) {
		w.fail("settle: the worker's outbox still holds %d unacknowledged observations 45s after it connected: global is neither accepting nor terminally rejecting them", outboxRows(w.f, w.dbPath))
	}
	w.checkActuations()

	// complete: every deploy whose phone-home was accepted is completed.
	// Give the last observation's handler a moment to commit.
	var d gDump
	pending := func() string {
		d = w.dump()
		for _, m := range w.machines {
			for _, o := range m.ops {
				if o.typ != "deploy" || !o.phoneAck || o.failAck || o.aborted {
					continue
				}
				g := d.op(o.id)
				if g == nil {
					return fmt.Sprintf("op %s is missing from the operations table", o.id)
				}
				if cfg.fromPhoneHomeUnbound && o.phoneAckBy == "from" && g.Status != "completed" {
					continue
				}
				if g.Status != "completed" {
					return fmt.Sprintf("deploy %s on %s: the installed OS's phone_home got a 2xx from the %s worker, but global has it %s (phase %q, result %s)",
						o.id, m.serial, w.tag(o.phoneAckBy), g.Status, g.Phase, g.Result)
				}
			}
		}
		return ""
	}
	if !eventually(10*time.Second, func() bool { return pending() == "" }) {
		w.fail("complete: %s", pending())
	}

	// Everything the timeouts cover now expires at once, as if the longest
	// timeout had passed with the fleet idle.
	// The sweep runs on every pass, as global's one-minute tick does: a
	// claim that lands while one pass reads its candidates is spared by
	// that pass and caught by the next.
	problem := func() string {
		var swept map[string][]string
		w.must("g.sweep", w.g.call("g.sweep", map[string]any{"pending_ms": 0, "stuck_ms": 0, "orphan_ms": 0, "phone_home_ms": 0}, &swept))
		for sweep, ids := range swept {
			if len(ids) > 0 {
				w.note("final sweep %s failed %v", sweep, ids)
			}
		}
		d = w.dump()
		s := w.status()
		for _, m := range w.machines {
			for _, o := range append(append([]*opRec{}, m.ops...), m.powerOps...) {
				g := d.op(o.id)
				if g == nil {
					return fmt.Sprintf("op %s is missing from the operations table", o.id)
				}
				if !terminal(g.Status) {
					return fmt.Sprintf("op %s (%s on %s) is still %s after every timeout sweep", o.id, o.typ, m.serial, g.Status)
				}
				if ws, ok := s.Ops[o.id]; ok && !terminal(ws.Status) {
					return fmt.Sprintf("the worker still holds op %s (%s on %s) as %s/%q although global has it %s", o.id, o.typ, m.serial, ws.Status, ws.Phase, g.Status)
				}
			}
			gm := d.machine(m.id)
			if gm == nil {
				return fmt.Sprintf("machine %s is missing from the machines table", m.serial)
			}
			if s.States[m.id] != gm.State {
				return fmt.Sprintf("machine %s: the worker holds state %q, global has %q", m.serial, s.States[m.id], gm.State)
			}
		}
		if n := outboxRows(w.f, w.dbPath); n != 0 {
			return fmt.Sprintf("the worker's outbox holds %d unacknowledged observations", n)
		}
		return ""
	}
	if !eventually(20*time.Second, func() bool { return problem() == "" }) {
		w.fail("settle: %s", problem())
	}

	// honest + complete, on the final rows.
	for _, m := range w.machines {
		for i, o := range m.ops {
			g := d.op(o.id)
			if o.typ != "deploy" {
				continue
			}
			if o.phoneAck && g.Status != "completed" && !o.failAck && !o.aborted {
				st.fromPhoneHomeLost++
			}
			if g.Status == "completed" {
				st.finalCompleted++
				if !o.phoneAck {
					w.fail("honest: deploy %s on %s is completed but no phone_home for it ever got a 2xx (reports=%d complete=%v)", o.id, m.serial, o.reports, o.completeAck)
				}
				if i == len(m.ops)-1 {
					gm := d.machine(m.id)
					if gm.State != "running" || (!gm.Locked && !o.unlockedAfter) {
						w.fail("complete: deploy %s is completed but machine %s is %s (locked=%v), want running and locked", o.id, m.serial, gm.State, gm.Locked)
					}
				}
			} else {
				st.finalFailed++
			}
		}
	}
	if os.Getenv("SKEW_TRACE") != "" {
		t.Logf("trace (case %d):\n  %s\nworker stderr:\n%s\nglobal stderr:\n%s", st.cases, strings.Join(w.trace, "\n  "), w.w.stderr.tail(20000), w.g.stderr.tail(8000))
	}
	// The secret every op was created with must be gone from no place the
	// harness can check here but the worker's surface: the boot script.
	for _, m := range w.machines {
		if _, script := get("http://" + w.wHTTP + "/boot/ipxe?mac=" + m.mac); strings.Contains(script, "skew-secret") {
			w.fail("credential: the boot script served to %s carries the op's BMC password", m.serial)
		}
	}
}

// skewCheck is rapid.Check with a fraction of the configured checks (four
// cases at rapid's default of 100): a case here starts four or more
// processes and runs real migrations.
func skewCheck(t *testing.T, div int, prop func(*rapid.T)) {
	t.Helper()
	checksOnce.Do(func() {
		if f := flag.Lookup("rapid.checks"); f != nil {
			if n, err := strconv.Atoi(f.Value.String()); err == nil && n >= div {
				if err := flag.Set("rapid.checks", strconv.Itoa(n/div)); err != nil {
					t.Fatalf("harness: set rapid.checks: %v", err)
				}
			}
		}
	})
	rapid.Check(t, prop)
}

// checksOnce guards the one-time scaling of -rapid.checks: the properties
// in this package all divide by the same factor, and parallel shards read
// the flag concurrently.
var checksOnce sync.Once

func logStats(t *testing.T, st *stats) {
	t.Logf("coverage: cases=%d steps=%d creates=%d (refused %d) claims=%d power_ops=%d", st.cases, st.steps, st.creates, st.createRefused, st.claims, st.powerOps)
	t.Logf("coverage: reports 2xx=%d 409=%d 410=%d worker-down=%d stale-or-duplicate=%d", st.reports2xx, st.reports409, st.reports410, st.reportsDown, st.stale)
	t.Logf("coverage: deploy_complete acks=%d phone_home acks=%d failure acks=%d aborts=%d leases=%d", st.completeAcks, st.phoneAcks, st.failAcks, st.aborts, st.leases)
	t.Logf("coverage: boots=%v executor power writes=%d", st.boots, st.actuations)
	t.Logf("coverage: ws drops=%d worker restarts=%d kills=%d outages=%d global restarts=%d kills=%d outages=%d", st.wsDrops, st.wRestarts, st.wKills, st.wDowns, st.gRestarts, st.gKills, st.gDowns)
	t.Logf("coverage: global upgraded first=%d worker first=%d; lifecycle ops in flight at a global upgrade=%d (awaiting phone-home: %d); steps with a refused peer=%d; outbox rows carried across worker upgrades=%d",
		st.upgradeGFirst, st.upgradeWFirst, st.midDeployUpgrades, st.awaitingAtUpgrade, st.refusedWindows, st.outboxAtWorkerUpgrade)
	t.Logf("coverage: deploys completed=%d failed=%d (of the failed, %d had a phone-home accepted by the from-build worker); installer boots after a from-build deploy_complete=%d; late-claim setups=%d, power cycles of an installed host by a late claim=%d", st.finalCompleted, st.finalFailed, st.fromPhoneHomeLost, st.fromLatchLost, st.lateClaimSetups, st.lateClaimActuations)
}

// shards runs prop as SKEW_SHARDS parallel rapid checks (default 1), each
// with its own statistics and seed, against the one Postgres container.
// With -rapid.seed every shard would replay the same cases, so a seeded run
// uses one.
func shards(t *testing.T, cfg scenarioConfig) {
	n := 1
	if v := os.Getenv("SKEW_SHARDS"); v != "" {
		if k, err := strconv.Atoi(v); err == nil && k > 0 {
			n = k
		}
	}
	// Bring the container up before the shards race for it.
	name, _ := freshDatabase(t, t, "")
	dropDatabase(name)
	run := func(t *testing.T) {
		st := &stats{boots: map[string]int{}}
		dir := t.TempDir()
		defer logStats(t, st)
		skewCheck(t, 25, func(rt *rapid.T) { runScenario(t, rt, cfg, st, dir) })
	}
	if n == 1 {
		run(t)
		return
	}
	for i := 0; i < n; i++ {
		t.Run(fmt.Sprintf("shard%d", i), func(t *testing.T) {
			t.Parallel()
			run(t)
		})
	}
}

// TestSkew_Property_RollingRestart is the scenario with both builds from this tree:
// every restart, crash and reconnection an upgrade causes, with no protocol
// or schema change in the way.
func TestSkew_Property_RollingRestart(t *testing.T) {
	shards(t, scenarioConfig{binFrom: toolNew, binTo: toolNew, verFrom: "aaaaaaa", verTo: "bbbbbbb"})
}

// TestSkew_Property_UpgradeFromOldRelease is the scenario from the old release to
// this tree. The two builds refuse each other's worker connections (the
// v0.6.0 breaking change), so every case has a window in which the worker
// serves boots and reports on its own. A deploy the old worker carried only
// in memory is exempt from the completion and boot checks (see
// scenarioConfig); everything this tree's builds accepted is not.
func TestSkew_Property_UpgradeFromOldRelease(t *testing.T) {
	needOld(t)
	shards(t, scenarioConfig{binFrom: toolOld, binTo: toolNew, verFrom: oldRef, verTo: "main", refuses: true,
		fromPhoneHomeUnbound: true, fromLatchInMemory: true})
}

// TestSkew_Property_RollbackToOldRelease is the scenario backwards: the fleet starts
// on this tree and both sides go back to the old release, global onto a
// database this tree migrated and the worker onto a worker.db this tree
// wrote. docs/user/installation.md says rolling global back rolls the
// workers back; this checks the old release can start on what it finds.
func TestSkew_Property_RollbackToOldRelease(t *testing.T) {
	needOld(t)
	shards(t, scenarioConfig{binFrom: toolNew, binTo: toolOld, verFrom: "main", verTo: oldRef, refuses: true, healthOnly: true})
}
