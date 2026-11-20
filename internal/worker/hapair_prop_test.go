package worker

// Property test for a pair of site workers serving one site: what the HOST
// experiences when two workers answer its PXE boots and its agent's reports.
//
// It extends the executor world (execworld_prop_test.go: real Cache, real
// LocalDB, real Executor per worker, an independent reference hub, a faulty
// transport) with a host model that talks to the workers only through the
// real boot HTTP API (internal/httpboot: GET /boot/ipxe, POST /api/v1/report):
//
//   - every power cycle a worker issues, and an occasional reboot of its own,
//     sends the host to PXE; one reachable worker answers and its boot script
//     decides installer or local disk;
//   - an installer boot runs an agent that reports phases, wipes the disk at
//     partitioning, and ends with deploy_complete / commission_report, with
//     the agent's documented handling of 409 (retry the final report) and 410
//     (halt);
//   - a local boot runs whatever is on the disk; a freshly installed disk
//     phones home until its first 2xx.
//
// Two deployment shapes are generated: both workers active at once, each
// serving the address its own DHCP handed out (the agent and the phone-home
// unit are pinned to the worker that served the boot script), and
// active/standby behind a VIP (reports go to whichever worker holds it).
//
// Properties, checked where they would break:
//
//	rewipe-after-deploy-complete   no worker that holds the awaiting_phone_home
//	                               latch (it accepted deploy_complete, or
//	                               global's row carries the phase and the
//	                               worker is caught up) serves the installer
//	rewipe-latch-not-at-hub        once global has handled a deploy's
//	                               deploy_complete, its row carries the latch
//	                               and a caught-up peer serves local boot
//	installer-after-op-removed     no worker serves the installer for an op
//	                               whose op_removed it has applied
//	installer-for-ended-op         a caught-up worker never serves the
//	                               installer for an op global ended
//	installer-from-long-frozen-op-set  a worker whose op stream has been down
//	                               longer than the trust window serves no
//	                               installer until it has resynced
//	untrusted-installer-boot       a rehydrated, not yet resynced worker never
//	                               serves the installer
//	installer-wrong-binding        an installer boot names the booting
//	                               machine's own op of the right type
//	agent-halted-live-op-absent    a worker answers 410 Gone for an op absent
//	                               from its set only when it has resynced
//	                               since it last lost its connection
//	phone-home-completes-other-op  an accepted phone-home records the
//	                               completion of the op it named
//
// and at quiescence (faults stopped, everything reconnected and delivered):
//
//	deploy-completed-without-phone-home  a deploy is completed on global only
//	                                     if a worker accepted its phone-home
//	latch-not-propagated                 a deploy whose deploy_complete a
//	                                     worker accepted is at
//	                                     awaiting_phone_home on global
//	phone-home-refused-at-quiescence     an installed host's phone-home is
//	                                     accepted by the worker it reaches
//	accepted-commission-report-lost      a commission report a worker answered
//	                                     2xx completes the commission
//	quiescent-boot-divergence            a machine without an active lifecycle
//	                                     op boots from disk on every worker
//	quiescent-peer-would-rewipe          no worker would serve the installer
//	                                     for a deploy whose install finished
//	quiescent-installer-withheld         every worker serves the installer for
//	                                     a claimed lifecycle op still waiting
//	                                     for its agent
//
// The classes in haTolerated describe what a pair cannot guarantee while a
// message is in flight or a worker is cut off from global. They are counted,
// not failed; HA_STRICT=<class,...> makes any of them fatal so that a run
// shrinks to a trace of it, and HA_MODE=both|vip pins the deployment shape.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/model"
)

// haTolerated are the violation classes a pair of workers produces by
// construction: each needs a message still in flight, or a worker cut off
// from global for less than the trust window of its op set.
var haTolerated = map[string]string{
	"rewipe-pair-partitioned":                  "the agent's deploy_complete was accepted by one worker; while that worker or its peer is disconnected from global the latch cannot cross, and the peer served the installer for the same deploy again",
	"rewipe-stream-lag":                        "the peer served the installer again while global's awaiting_phone_home row was still in flight to it (milliseconds in practice)",
	"rewipe-scope-change-in-flight":            "the machine left the site's scope on global during the deploy, so global stopped sending the site its op_deltas (they are filtered by snapshot membership) and the latch among them; the peer, which had not yet applied the snapshot that drops the machine, served the installer again",
	"installer-for-ended-op-disconnected":      "a worker disconnected from global for less than the trust window served the installer for an op global has ended (completed through the peer, aborted, swept): its op set is frozen at the disconnect and trusted for that long",
	"installer-for-ended-op-stream-lag":        "a worker served the installer for an op whose op_removed was still in flight to it",
	"claim-reply-lost-after-boot-phase":        "a claim committed with its reply lost on an op whose row already carried a phase (the host PXE-booted on the pending-op clause): neither the executor's re-claim nor the orphaned-claim sweep applies to a row with a phase, so the executor never drives it and the op waits for the stuck sweep",
	"installer-withheld-claim-delta-overtaken": "the op_delta that carries a claim's transient machine state (commissioning, deploying) was overtaken on its way to a worker by a later delta of the same op and dropped as stale; the worker keeps the machine's previous state and serves local boot for the claimed op until a resync or a snapshot push re-sends it. Global queues one connection's messages in publish order, so this needs the claim and a phase report to publish out of commit order",
	"agent-halted-live-op-stream-lag":          "a worker answered an agent report with 410 Gone for an op whose op_delta was still in flight to it (two workers answering one agent)",
	"agent-halted-live-op-terminal-local":      "a worker answered an agent report with 410 Gone for an op global still holds active: the worker holds it terminal locally and its report to global was lost",
}

var haStrict = map[string]bool{}

func init() {
	for class, why := range haTolerated {
		epTolerated[class] = why
	}
	for _, c := range strings.Split(os.Getenv("HA_STRICT"), ",") {
		if c = strings.TrimSpace(c); c != "" {
			haStrict[c] = true
			delete(epTolerated, c)
		}
	}
}

// haTrustWindow is, in virtual seconds, how long a worker keeps actuating
// boots from the op set it held when its op stream went down.
const haTrustWindow = int64(opStreamTrustWindow / time.Second)

func haSystemUUID(m *epMachine) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, m.id[:]).String()
}

// ---------------------------------------------------------------------------
// Host model
// ---------------------------------------------------------------------------

type haHost struct {
	m     *epMachine
	stage string // os | pxe | agent | halted | off | dead

	// disk
	diskOK bool      // false after a wipe that has not been followed by a finished install
	diskOp uuid.UUID // deploy op whose finished install is on the disk; Nil = the previous system
	phOp   uuid.UUID // phone-home unit on the disk
	phSrv  int       // worker the unit posts to, -1 = the VIP
	phDone bool

	// agent
	agOp    uuid.UUID
	agType  model.OperationType
	agSrv   int // worker the agent posts to, -1 = the VIP
	agStep  int
	agFinal bool

	bootNext bool // BootNext armed: the next boot goes to disk without PXE
}

type haSrv struct {
	gen int
	h   http.Handler
}

type haWorld struct {
	w          *epWorld
	bothActive bool
	hosts      []*haHost
	lanDown    map[int]bool
	srv        map[int]haSrv

	installed map[uuid.UUID]map[int]bool // deploy op -> workers that accepted its deploy_complete
	phoneHome map[uuid.UUID]bool         // deploy op -> a worker answered 2xx to its phone-home
	commDone  map[uuid.UUID]bool         // commission op -> a worker answered 2xx to its report
	// agentFailed: op -> workers that accepted the agent's failure report
	// for it (the executor stops driving such an op).
	agentFailed map[uuid.UUID]map[int]bool
}

var haOpRe = regexp.MustCompile(`slagworks\.op=([0-9a-f-]{36})`)

func (h *haWorld) handler(pw *epWorker) http.Handler {
	if s, ok := h.srv[pw.idx]; ok && s.gen == pw.gen {
		return s.h
	}
	srv := httpboot.NewServer("", httpboot.ServerConfig{
		DefaultImage: "img", HTTPPort: 8080, DeployMode: true, Provider: pw.cache,
	})
	srv.SetLogger(testDiscardLogger())
	hh := srv.Build()
	h.srv[pw.idx] = haSrv{gen: pw.gen, h: hh}
	return hh
}

// reach picks the worker a host request lands on: the pinned one, or (pin<0)
// any worker serving the site. nil when nobody answers.
func (h *haWorld) reach(pin int, label string) *epWorker {
	var cands []*epWorker
	for _, pw := range h.w.wk {
		if !pw.active || h.lanDown[pw.idx] || pw.crashed {
			continue
		}
		if pin >= 0 && pw.idx != pin {
			continue
		}
		cands = append(cands, pw)
	}
	if len(cands) == 0 {
		return nil
	}
	return epChoose(h.w, cands, label)
}

func (h *haWorld) report(pw *epWorker, rep model.ProgressReport) (int, string) {
	body, err := json.Marshal(rep)
	if err != nil {
		panic(err)
	}
	rec := httptest.NewRecorder()
	h.handler(pw).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/report", bytes.NewReader(body)))
	h.w.agentEpoch++
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func (w *epWorld) hostPower(m *epMachine, kind string) {
	if w.ha == nil {
		return
	}
	host := w.ha.hosts[m.idx]
	if kind == "off" {
		host.stage = "off"
		return
	}
	w.ha.reboot(host, "power "+kind)
}

func (h *haWorld) reboot(host *haHost, why string) {
	host.stage = "pxe"
	host.agOp = uuid.Nil
	h.w.logf("host m%d: reboot (%s)", host.m.idx, why)
}

func (h *haWorld) bootDisk(host *haHost) {
	if !host.diskOK {
		host.stage = "dead"
		h.w.stat("host.boot_disk_unbootable")
		h.w.logf("host m%d: boots a disk that holds no system (wiped, install unfinished)", host.m.idx)
		return
	}
	host.stage = "os"
	h.w.stat("host.boot_disk")
}

// inboxMentions reports whether a message that can change pw's view of op is
// still in flight to it.
func haInboxMentions(pw *epWorker, op uuid.UUID) bool {
	for _, msg := range pw.inbox {
		switch msg.kind {
		case epMsgResync:
			return true
		case epMsgDelta, epMsgRemoved:
			if msg.op.ID == op {
				return true
			}
		}
	}
	return false
}

// haSnapshotInFlight reports whether an identity snapshot is still in flight
// to pw.
func haSnapshotInFlight(pw *epWorker) bool {
	for _, msg := range pw.inbox {
		if msg.kind == epMsgSnapshot {
			return true
		}
	}
	return false
}

func (h *haWorld) pxe(host *haHost) {
	w := h.w
	if host.bootNext {
		host.bootNext = false
		w.stat("host.bootnext_to_disk")
		h.bootDisk(host)
		return
	}
	pw := h.reach(-1, "pxeWorker")
	if pw == nil {
		w.stat("host.pxe_unanswered")
		h.bootDisk(host)
		return
	}
	pw.syncStreamClock()
	rec := httptest.NewRecorder()
	h.handler(pw).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boot/ipxe?mac="+host.m.mac, nil))
	w.agentEpoch++
	script := rec.Body.String()
	m := haOpRe.FindStringSubmatch(script)
	if m == nil || !strings.Contains(script, "\nkernel ") {
		w.stat("host.pxe_local")
		if pw.streamDownFor() > haTrustWindow {
			w.stat("host.pxe_local_op_set_frozen_past_trust_window")
		}
		w.logf("host m%d: PXE on w%d -> local boot", host.m.idx, pw.idx)
		h.bootDisk(host)
		return
	}
	opID := uuid.MustParse(m[1])
	typ := model.OpDeploy
	if strings.Contains(script, "slagworks.mode=commission") {
		typ = model.OpCommission
	}
	w.stat("host.installer_boot." + string(typ))
	w.logf("host m%d: PXE on w%d -> %s installer for %s", host.m.idx, pw.idx, typ, w.opName(opID))
	h.checkInstallerBoot(pw, host, opID, typ)
	host.stage, host.agOp, host.agType, host.agStep, host.agFinal = "agent", opID, typ, 0, false
	host.agSrv = -1
	if h.bothActive {
		host.agSrv = pw.idx
	}
}

func (h *haWorld) checkInstallerBoot(pw *epWorker, host *haHost, opID uuid.UUID, typ model.OperationType) {
	w := h.w
	rec, facts, name := w.hub.ops[opID], w.byOp[opID], w.opName(opID)
	if rec == nil || rec.op.MachineID != host.m.id || rec.op.Type != typ {
		w.violate("installer-wrong-binding", "w%d served a %s installer naming %s to m%d", pw.idx, typ, name, host.m.idx)
		return
	}
	if pw.untrusted() {
		w.violate("untrusted-installer-boot", "w%d served the installer for %s while its op layer is rehydrated and not resynced", pw.idx, name)
	}
	if facts.removedSeen[pw.idx] {
		w.violate("installer-after-op-removed", "w%d served the installer for %s after applying its op_removed", pw.idx, name)
	}
	if down := pw.streamDownFor(); down > haTrustWindow {
		w.violate("installer-from-long-frozen-op-set", "w%d served the installer for %s although its op stream has been down for %ds, longer than the trust window, with no resync since", pw.idx, name, down)
	}
	caughtUp := pw.connUp && !haInboxMentions(pw, opID)
	// Global sends an op's deltas only to a site whose cached snapshot holds
	// the machine. Once the machine has left that snapshot the latch cannot
	// reach this worker as an op_delta; the snapshot that removes the machine
	// from its view (and with it any installer boot) is what it is waiting
	// for.
	scopeChangeInFlight := !w.hub.members[host.m.id] && haSnapshotInFlight(pw)
	if acc := h.installed[opID]; len(acc) > 0 && typ == model.OpDeploy {
		switch {
		case acc[pw.idx]:
			w.violate("rewipe-after-deploy-complete", "w%d accepted deploy_complete for %s and then served its installer again", pw.idx, name)
		case caughtUp && scopeChangeInFlight:
			w.violate("rewipe-scope-change-in-flight", "the install of %s finished; m%d has left the site's scope on global, so the latch was not sent to w%d, which has not applied the snapshot yet and served the installer again", name, host.m.idx, pw.idx)
		case caughtUp && rec.op.Phase == "awaiting_phone_home":
			w.violate("rewipe-after-deploy-complete", "w%d is caught up with global's row for %s (phase awaiting_phone_home) and served its installer again", pw.idx, name)
		default:
			// Where is the latch? Still in the outbox of a worker that
			// accepted deploy_complete, in flight, or nowhere any more.
			queued, queuedOnDisconnected := false, false
			for idx := range acc {
				a := w.wk[idx]
				for _, u := range a.queuedUpdates() {
					if u.OperationID == opID.String() && u.Phase == "awaiting_phone_home" {
						queued = true
						queuedOnDisconnected = queuedOnDisconnected || !a.connUp
					}
				}
			}
			if !facts.deployCompleteAtHub {
				// The durable observation carries the latch too, and it is
				// still in an acceptor's outbox.
				queued = true
				for idx := range acc {
					queuedOnDisconnected = queuedOnDisconnected || !w.wk[idx].connUp
				}
			}
			switch {
			case !pw.connUp || queuedOnDisconnected:
				w.violate("rewipe-pair-partitioned", "the install of %s finished (deploy_complete accepted by another worker); a worker of the pair is cut off from global (serving w%d connected=%v, latch stuck on a disconnected acceptor=%v) and w%d served the installer again",
					name, pw.idx, pw.connUp, queuedOnDisconnected, pw.idx)
			case queued || !caughtUp || rec.op.Phase == "awaiting_phone_home":
				w.violate("rewipe-stream-lag", "the install of %s finished; w%d served the installer again with the latch still in flight", name, pw.idx)
			default:
				w.violate("rewipe-latch-not-at-hub", "the install of %s finished (deploy_complete accepted by another worker); the latch is queued nowhere and global's row is at phase %q; caught-up w%d served the installer again",
					name, rec.op.Phase, pw.idx)
			}
		}
	}
	if epTerminal(rec.op.Status) {
		if host.m.state == model.MachineStateRunning {
			w.stat("host.installer_served_to_running_machine")
		}
		switch {
		case !pw.connUp:
			w.violate("installer-for-ended-op-disconnected", "global ended %s (%s, machine now %s); w%d, disconnected from global, served its installer",
				name, rec.op.Status, host.m.state, pw.idx)
		case caughtUp:
			w.violate("installer-for-ended-op", "global ended %s (%s); caught-up w%d served its installer", name, rec.op.Status, pw.idx)
		default:
			w.violate("installer-for-ended-op-stream-lag", "global ended %s (%s); w%d served its installer with the removal still in flight", name, rec.op.Status, pw.idx)
		}
	}
}

var (
	haDeploySteps     = []string{"agent_starting", "partitioning", "rsyncing_os"}
	haCommissionSteps = []string{"agent_starting", "collecting_inventory"}
)

func (h *haWorld) agentStep(host *haHost) {
	w := h.w
	op := host.agOp
	steps := haDeploySteps
	if host.agType == model.OpCommission {
		steps = haCommissionSteps
	}
	rep := model.ProgressReport{OperationID: op.String(), MachineID: host.m.id.String()}
	final := host.agStep >= len(steps)
	switch {
	case w.chance(1, 24, "agentFatal"):
		rep.Failed, rep.Phase, rep.Error = true, "partitioning", "ha: agent fatal"
		if pw := h.reach(host.agSrv, "agentWorker"); pw != nil {
			if code, _ := h.report(pw, rep); code == http.StatusOK {
				if h.agentFailed[op] == nil {
					h.agentFailed[op] = map[int]bool{}
				}
				h.agentFailed[op][pw.idx] = true
			}
		}
		host.stage = "halted"
		w.stat("host.agent_fatal")
		w.logf("host m%d: agent reports failure for %s and stops", host.m.idx, w.opName(op))
		return
	case !final:
		rep.Phase = steps[host.agStep]
	case host.agType == model.OpDeploy:
		rep.Event = "deploy_complete"
	default:
		rep.Event = "commission_report"
		rep.CommissionData = json.RawMessage(fmt.Sprintf(`{"system":{"serial_number":%q,"uuid":%q},"nics":[{"mac":%q}]}`,
			host.m.serial, haSystemUUID(host.m), host.m.mac))
	}
	pw := h.reach(host.agSrv, "agentWorker")
	code, body := 0, ""
	if pw != nil {
		code, body = h.report(pw, rep)
	}
	what := rep.Phase
	if final {
		what = rep.Event
	}
	w.stat(fmt.Sprintf("host.report.%s.%d", map[bool]string{false: "phase", true: "final"}[final], code))
	if pw != nil {
		w.logf("host m%d: agent -> w%d %s for %s: %d %s", host.m.idx, pw.idx, what, w.opName(op), code, body)
	}
	if code == http.StatusGone {
		h.checkHalt(pw, host, body)
		host.stage = "halted"
		return
	}
	if !final {
		if rep.Phase == "partitioning" {
			host.diskOK, host.diskOp, host.phOp = false, uuid.Nil, uuid.Nil
			w.stat("host.wipe")
		}
		host.agStep++
		return
	}
	if code != http.StatusOK {
		return // the final report is retried
	}
	host.agFinal = true
	if host.agType == model.OpDeploy {
		if h.installed[op] == nil {
			h.installed[op] = map[int]bool{}
		}
		h.installed[op][pw.idx] = true
		host.diskOK, host.diskOp, host.phOp, host.phSrv, host.phDone = true, op, op, host.agSrv, false
		host.bootNext = w.chance(1, 2, "bootNextHonored")
		w.stat("host.deploy_complete_accepted")
	} else {
		h.commDone[op] = true
		w.stat("host.commission_report_accepted")
	}
	h.reboot(host, "agent finished")
}

func (h *haWorld) checkHalt(pw *epWorker, host *haHost, body string) {
	w := h.w
	rec := w.hub.ops[host.agOp]
	if rec == nil || epTerminal(rec.op.Status) {
		w.stat("host.agent_halted_op_over")
		return
	}
	for _, u := range pw.updates(host.agOp) {
		if u.Status != "" {
			// The worker ended the op itself and global will hear of it.
			w.stat("host.agent_halted_worker_ended_op")
			return
		}
	}
	class := "agent-halted-live-op-terminal-local"
	if strings.Contains(body, "is not active on this site") {
		class = "agent-halted-live-op-absent"
		if pw.connUp && haInboxMentions(pw, host.agOp) {
			class = "agent-halted-live-op-stream-lag"
		}
	}
	w.violate(class, "w%d answered 410 (%s) to the agent of m%d for %s, which global holds %s at phase %q",
		pw.idx, body, host.m.idx, w.opName(host.agOp), rec.op.Status, rec.op.Phase)
}

func (h *haWorld) phoneHomeStep(host *haHost) bool {
	w := h.w
	if host.stage != "os" || !host.diskOK || host.phOp == uuid.Nil || host.phDone {
		return false
	}
	pw := h.reach(host.phSrv, "phoneHomeWorker")
	if pw == nil {
		w.stat("host.phone_home_unreachable")
		return false
	}
	op := host.phOp
	facts := w.byOp[op]
	obsBefore := pw.obsCount(op)
	code, body := h.report(pw, model.ProgressReport{OperationID: op.String(), MachineID: host.m.id.String(), Event: "phone_home"})
	w.stat(fmt.Sprintf("host.phone_home.%d", code))
	w.logf("host m%d: phone-home -> w%d for %s: %d %s", host.m.idx, pw.idx, w.opName(op), code, body)
	if code != http.StatusOK {
		return false
	}
	host.phDone = true
	h.phoneHome[op] = true
	// The observation the worker queued must name the op the phone-home
	// named: global completes whichever active deploy op it carries.
	if q := pw.queuedObs(); len(q) > 0 {
		last := q[len(q)-1].Obs
		if last.Event == "phone_home" && last.OperationID != op && last.OperationID != uuid.Nil {
			h.phoneHome[last.OperationID] = true
			if of := w.byOp[last.OperationID]; of != nil {
				of.phoneHomeRecorded = true
			}
			w.violate("phone-home-completes-other-op", "the installed system on m%d phoned home for %s; w%d accepted it and queued the completion of %s instead",
				host.m.idx, w.opName(op), pw.idx, w.opName(last.OperationID))
			return true
		}
	}
	switch {
	case pw.obsCount(op) > obsBefore:
		facts.phoneHomeRecorded = true
	case !facts.phoneHomeRecorded:
		facts.phoneHomeAckedUnrecorded = true
	}
	return true
}

func (h *haWorld) hostStep(host *haHost) {
	switch host.stage {
	case "pxe":
		h.pxe(host)
	case "agent":
		h.agentStep(host)
	case "os":
		h.phoneHomeStep(host)
	}
}

// step runs one host-side action of the world.
func (h *haWorld) step(name string, _ *epWorker) {
	w := h.w
	switch name {
	case "hostStep":
		// Prefer a host with something to do.
		var busy []*haHost
		for _, host := range h.hosts {
			if host.stage == "pxe" || host.stage == "agent" || (host.stage == "os" && host.phOp != uuid.Nil && !host.phDone) {
				busy = append(busy, host)
			}
		}
		if len(busy) > 0 {
			h.hostStep(epChoose(w, busy, "host"))
		}
	case "hostReboot":
		host := epChoose(w, h.hosts, "rebootHost")
		if host.stage != "off" && host.stage != "pxe" {
			w.stat("host.spontaneous_reboot")
			h.reboot(host, "spontaneous")
		}
	case "lanFlap":
		pw := w.pickWorker("lanWorker")
		h.lanDown[pw.idx] = !h.lanDown[pw.idx]
		w.logf("w%d: site LAN down=%v", pw.idx, h.lanDown[pw.idx])
	case "unlock":
		m := w.pickMachine("unlockMachine")
		if m.locked {
			m.locked = false
			w.logf("hub: m%d unlocked", m.idx)
			w.hub.push()
		}
	case "haCreate":
		// A lifecycle op on a machine with a BMC, drivable end to end.
		spec := epOpSpec{typ: model.OpDeploy, mach: w.pick(2, "haMachine"), transport: "bmc", serialMode: "match"}
		if w.chance(1, 3, "haCommission") {
			spec.typ = model.OpCommission
		}
		spec.expected = w.mach[spec.mach].serial
		if rec := w.hub.create(spec, spec.params()); rec != nil {
			w.logf("hub: create %s", w.opName(rec.op.ID))
			w.stat("op.created." + string(spec.typ))
		} else {
			w.stat("op.create_rejected")
		}
	case "flapDuringClaim":
		// keepalived moves the VIP away and back while the executor is inside
		// its next claim or BMC call, right after a reconnect whose hello
		// op_resync is still unread.
		pw := w.vipHolder()
		if h.bothActive || pw == nil || pw.inTick > 0 || w.depth > 0 {
			return
		}
		pw.disconnect()
		pw.connect()
		w.forced = append(w.forced, func(pw *epWorker) {
			pw.releaseVIP()
			pw.acquireVIP()
			pw.tick()
		})
		pw.tick()
		w.forced = nil
	}
}

// ---------------------------------------------------------------------------
// Quiescence
// ---------------------------------------------------------------------------

func (h *haWorld) activeLifecycleOp(m *epMachine) *epHubOp {
	for _, id := range h.w.hub.order {
		rec := h.w.hub.ops[id]
		if rec.op.MachineID == m.id && !epTerminal(rec.op.Status) && epLifecycle(rec.op.Type) {
			return rec
		}
	}
	return nil
}

func (h *haWorld) checkQuiescence() {
	w := h.w
	for i := range h.lanDown {
		h.lanDown[i] = false
	}
	// An installed host keeps phoning home; with everything healed the worker
	// it reaches must take it.
	again := false
	for _, host := range h.hosts {
		if host.stage != "os" || host.phOp == uuid.Nil || host.phDone || !host.m.inSite {
			continue // a machine outside the site's scope is nobody's to complete
		}
		rec := w.hub.ops[host.phOp]
		if rec == nil || epTerminal(rec.op.Status) {
			continue
		}
		if h.phoneHomeStep(host) {
			again = true
			continue
		}
		if h.reach(host.phSrv, "quiescentPhoneHome") != nil {
			w.violate("phone-home-refused-at-quiescence", "m%d runs the system %s installed and its phone-home is still refused with everything reconnected and delivered (global's row: %s phase %q)",
				host.m.idx, w.opName(host.phOp), rec.op.Status, rec.op.Phase)
		}
	}
	if again {
		w.settle()
	}

	for _, id := range w.hub.order {
		rec := w.hub.ops[id]
		name := w.opName(id)
		switch {
		case rec.op.Type == model.OpDeploy && rec.op.Status == model.OpCompleted && !h.phoneHome[id]:
			w.violate("deploy-completed-without-phone-home", "%s is completed on global and no worker ever accepted a phone-home for it", name)
		case rec.op.Type == model.OpDeploy && !epTerminal(rec.op.Status) && len(h.installed[id]) > 0 && rec.op.Phase != "awaiting_phone_home":
			w.violate("latch-not-propagated", "a worker accepted deploy_complete for %s; at quiescence global's row is %s at phase %q", name, rec.op.Status, rec.op.Phase)
		case rec.op.Type == model.OpCommission && !epTerminal(rec.op.Status) && h.commDone[id]:
			w.violate("accepted-commission-report-lost", "a worker answered the commission report of %s with 2xx; at quiescence global still holds it %s", name, rec.op.Status)
		}
	}

	// Recomputation oracle for the boot decision: with everything delivered,
	// what each worker would hand a PXE-booting machine follows from global's
	// rows alone.
	for _, m := range w.mach {
		if !m.inSite {
			continue
		}
		active := h.activeLifecycleOp(m)
		for _, pw := range w.wk {
			pw.syncStreamClock()
			got := pw.cache.BootActionForMAC(m.mac)
			action := map[model.BootAction]string{model.BootLocal: "local", model.BootDeploy: "deploy", model.BootCommission: "commission"}[got]
			w.stat("final.boot_action." + action)
			switch {
			case active != nil && active.op.Status == model.OpClaimed && m.state == epTransient(active.op) &&
				active.op.Phase != "awaiting_phone_home" && !w.byOp[active.op.ID].reportLossy:
				// The other direction: a claimed lifecycle op that is still
				// waiting for its agent gets the installer from every worker,
				// or nothing would ever install. (An op whose failure report
				// was lost in transit is held failed by the worker that
				// reported it while global still has it claimed: report-lost.)
				want := model.BootDeploy
				if active.op.Type == model.OpCommission {
					want = model.BootCommission
				}
				switch {
				case got == want:
				case w.byOp[active.op.ID].stateOvertaken[pw.idx]:
					w.violate("installer-withheld-claim-delta-overtaken", "%s is claimed on global and m%d is %s, but w%d applied a later op_delta of the op before the one that carried the claim's machine state and would serve it %s",
						w.opName(active.op.ID), m.idx, m.state, pw.idx, action)
				default:
					w.violate("quiescent-installer-withheld", "%s is claimed on global at phase %q and m%d is %s; at quiescence w%d would serve it %s",
						w.opName(active.op.ID), active.op.Phase, m.idx, m.state, pw.idx, action)
				}
			case active == nil && got != model.BootLocal:
				w.violate("quiescent-boot-divergence", "m%d has no active lifecycle op on global (state %s) and w%d would serve it %s", m.idx, m.state, pw.idx, action)
			case active != nil && active.op.Type == model.OpDeploy && len(h.installed[active.op.ID]) > 0 && got != model.BootLocal:
				w.violate("quiescent-peer-would-rewipe", "the install of %s finished (deploy_complete accepted by a worker); at quiescence w%d would serve m%d %s (global's row: phase %q)",
					w.opName(active.op.ID), pw.idx, m.idx, action, active.op.Phase)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// The property
// ---------------------------------------------------------------------------

var haActions = []epAction{
	{"haCreate", 8, true},
	{"hostStep", 22, true},
	{"hostReboot", 1, true},
	{"lanFlap", 1, true},
	{"unlock", 1, true},
	{"flapDuringClaim", 2, false},
}

func haRun(t *rapid.T, dir string, maxSteps int) {
	defer func() { epStat.add("cases", 1) }()
	w := newEpWorld(t, dir)
	h := &haWorld{
		w: w, lanDown: map[int]bool{}, srv: map[int]haSrv{},
		installed: map[uuid.UUID]map[int]bool{}, phoneHome: map[uuid.UUID]bool{}, commDone: map[uuid.UUID]bool{},
		agentFailed: map[uuid.UUID]map[int]bool{},
	}
	w.ha = h
	h.bothActive = w.chance(1, 2, "bothActive")
	switch os.Getenv("HA_MODE") {
	case "both":
		h.bothActive = true
	case "vip":
		h.bothActive = false
	}
	for _, m := range w.mach {
		h.hosts = append(h.hosts, &haHost{m: m, stage: "os", diskOK: true, phSrv: -1, agSrv: -1})
	}
	w.hub.buildSnapshot() // again, now with the boot-script fields

	var top, intr []string
	enable := func(a epAction) {
		for i := 0; i < a.weight; i++ {
			top = append(top, a.name)
			if a.interrupt {
				intr = append(intr, a.name)
			}
		}
	}
	for _, a := range append(append([]epAction(nil), epActions...), haActions...) {
		switch a.name {
		case "agent":
			continue // the host model stands in for the free-form agent
		case "vipAcquire", "vipRelease", "vipFlap", "flapDuringClaim":
			if h.bothActive {
				continue
			}
		}
		must := a.name == "create" || a.name == "tick" || a.name == "connect" || a.name == "haCreate" || a.name == "hostStep" || a.name == "drain"
		if must || w.chance(3, 4, "swarm") {
			enable(a)
		}
	}
	w.interruptNames = intr

	if h.bothActive {
		w.stat("mode.both_active")
		for _, pw := range w.wk {
			pw.acquireVIP()
		}
	} else {
		w.stat("mode.vip")
		w.wk[0].acquireVIP()
	}
	for _, pw := range w.wk {
		pw.connect()
		pw.deliverAll()
	}
	steps := 1 + w.pick(maxSteps, "steps")
	for i := 0; i < steps; i++ {
		w.step(epChoose(w, top, "action"), nil)
	}
	epStat.add("steps", int64(steps))
	w.settle()
	h.checkQuiescence()
	w.checkQuiescence()
}

func TestProp_HAPairHostView(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(testDiscardLogger())
	t.Cleanup(func() { slog.SetDefault(prev) })
	dir := t.TempDir()
	rapid.Check(t, func(rt *rapid.T) { haRun(rt, dir, 140) })
	t.Logf("coverage (aggregate over all cases, shrinking included):\n%s", epStat.dump())
}
