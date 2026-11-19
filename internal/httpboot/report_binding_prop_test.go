package httpboot_test

// Stateful property-based coverage for the agent-facing identity binding of
// the HTTP boot server, driven end to end over HTTP against the real site
// cache (worker.Cache): op-stream events from global interleaved with agent
// reports, phone-homes and boot-script fetches that name the right, a
// stale, another machine's, or an unknown operation.
//
// Reference model (independent of worker.Cache): each machine has at most
// one active lifecycle op. A report is accepted iff it names a machine in
// the snapshot and that machine's own active, non-terminal op of the type
// the event requires. A phone_home is accepted only once that deploy op's
// deploy_complete was accepted, plus the documented retry on a machine that
// is already running. Contract sources: opstate_cache.go
// validateReportBinding ("Hardware discovered by the agent is never allowed
// to select a different machine", "The first phone-home ... must pass the
// exact active deploy-op checks"), handler.go boundBootOperation, and
// docs/dev/state-machine.md.
//
// Invariants checked after every step:
//
//	I1  accept/reject of a report equals the model's decision.
//	I2  a rejected request queues nothing for global and changes no boot
//	    decision.
//	I3  everything an accepted report queues names exactly the reported
//	    machine and operation.
//	I4  an installer boot script names the requesting machine's own active
//	    op, of the right type, and is never served once that op is awaiting
//	    phone-home, locally failed, locally completed or gone.
//	I5  a deploy op is completed locally only by a phone_home that names it,
//	    and a phone-home that names only a MAC is refused.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

var bindQuietOnce sync.Once

type bindOp struct {
	id        uuid.UUID
	machine   int
	typ       model.OperationType
	claimed   bool
	version   int
	awaiting  bool // deploy_complete accepted
	failed    bool // locally failed by an accepted failure report
	completed bool // locally completed by an accepted phone-home
	removed   bool // global removed it (terminal)
}

func (o *bindOp) activeForReports() bool { return !o.removed && !o.failed && !o.completed }

type bindMachine struct {
	id      uuid.UUID
	mac     string
	sysUUID string
	base    model.MachineState // durable baseline in the snapshot
	running bool               // local phone-home overlay
	op      *bindOp            // current op (may be terminal locally), nil if none
}

type bindSim struct {
	t        *rapid.T
	cache    *worker.Cache
	h        http.Handler
	machines []*bindMachine
	ops      []*bindOp // every op ever created, for stale references
	seq      int64
	stats    map[string]int
}

func newBindSim(t *rapid.T, stats map[string]int) *bindSim {
	s := &bindSim{t: t, cache: worker.NewCache(nil), stats: stats}
	s.machines = []*bindMachine{
		{id: uuid.MustParse("11111111-1111-4111-8111-111111111111"), mac: "52:54:00:aa:bb:01", sysUUID: "4c4c4544-0042-3510-8051-b4c04f4a4433", base: model.MachineStateCommissioned},
		{id: uuid.MustParse("22222222-2222-4222-8222-222222222222"), mac: "52:54:00:aa:bb:02", sysUUID: "03000200-0400-0500-0006-000700080009", base: model.MachineStateCommissioned},
	}
	bindQuietOnce.Do(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })
	srv := httpboot.NewServer(":0", httpboot.ServerConfig{
		DefaultImage: "noble-pxe", HTTPPort: 8080, DeployMode: true, Provider: s.cache,
	})
	srv.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.h = srv.Build()
	s.pushSnapshot()
	// An op_resync marks the op layer synchronized, as the first WS sync does.
	s.cache.ApplyOpResync(nil, nil)
	return s
}

func (s *bindSim) pushSnapshot() {
	s.seq++
	snap := &model.SiteSnapshot{SeqNum: s.seq}
	for _, m := range s.machines {
		snap.Machines = append(snap.Machines, model.MachineSnapshot{
			ID: m.id, State: m.base, PXEEligible: true, SystemUUID: m.sysUUID,
			KernelParams: "console=ttyS1,115200n8", NetBoxSerial: "SER-" + m.mac[len(m.mac)-2:],
			Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: m.mac}},
		})
	}
	s.cache.ApplySnapshot(snap)
}

func (s *bindSim) do(method, target string, body []byte) (int, string) {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// drain returns what the cache queued for global since the last drain.
func (s *bindSim) drain() ([]model.Observation, []model.OperationUpdate) {
	pend, upd := s.cache.DrainOutbox()
	obs := make([]model.Observation, 0, len(pend))
	for _, p := range pend {
		obs = append(obs, p.Obs)
	}
	return obs, upd
}

// bootClass fetches the boot script and classifies it.
func (s *bindSim) bootClass(mac string) (class, machineID, opID string) {
	code, body := s.do("GET", "/boot/ipxe?mac="+mac, nil)
	if code != 200 {
		return fmt.Sprintf("http%d", code), "", ""
	}
	var kernel string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "kernel ") {
			kernel = line
		}
	}
	if kernel == "" {
		if strings.Contains(body, "sanboot") {
			return "local", "", ""
		}
		return "refused", "", ""
	}
	class = "deploy"
	for _, tok := range strings.Fields(kernel) {
		switch {
		case tok == "slagworks.mode=commission":
			class = "commission"
		case strings.HasPrefix(tok, "slagworks.machine="):
			machineID = strings.TrimPrefix(tok, "slagworks.machine=")
		case strings.HasPrefix(tok, "slagworks.op="):
			opID = strings.TrimPrefix(tok, "slagworks.op=")
		}
	}
	return class, machineID, opID
}

func (s *bindSim) bootDecisions() string {
	var out []string
	for _, m := range s.machines {
		c, mid, oid := s.bootClass(m.mac)
		out = append(out, c+"/"+mid+"/"+oid)
	}
	// Boot fetches report ipxe_loaded; discard that so it is not attributed
	// to the step under test.
	s.drain()
	return strings.Join(out, " ")
}

// checkBoot is I4 for one machine.
func (s *bindSim) checkBoot(m *bindMachine) {
	class, mid, oid := s.bootClass(m.mac)
	obs, upd := s.drain()
	op := m.op
	servable := op != nil && op.activeForReports() && !op.awaiting && !m.running
	if class == "deploy" || class == "commission" {
		s.stats["installer_served"]++
		if !servable {
			s.t.Fatalf("installer (%s) served to %s whose op is %+v (running=%v)", class, m.mac, op, m.running)
		}
		if mid != m.id.String() || oid != op.id.String() {
			s.t.Fatalf("installer for %s names machine %s op %s, want machine %s op %s", m.mac, mid, oid, m.id, op.id)
		}
		if (class == "deploy") != (op.typ == model.OpDeploy) {
			s.t.Fatalf("installer class %s for a %s op", class, op.typ)
		}
	} else {
		s.stats["boot_"+class]++
		// Liveness: a claimed op that has not finished installing must get
		// its installer, or the machine is stranded.
		if servable && op.claimed {
			s.t.Fatalf("no installer (%s) for %s although its op %+v is claimed and mid-flight", class, m.mac, op)
		}
	}
	for _, u := range upd {
		if op == nil || u.OperationID != op.id.String() {
			s.t.Fatalf("boot fetch for %s queued an op update for %s", m.mac, u.OperationID)
		}
	}
	for _, o := range obs {
		s.t.Fatalf("boot fetch for %s queued observation %+v", m.mac, o)
	}
}

func (s *bindSim) commissionData(m *bindMachine) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"system": map[string]string{"serial_number": "SER-" + m.mac[len(m.mac)-2:], "uuid": m.sysUUID},
		"nics":   []map[string]string{{"mac": m.mac, "driver": "ixgbe"}},
	})
	return b
}

func (s *bindSim) step() {
	t := s.t
	switch rapid.SampledFrom([]string{
		"new_op", "new_op", "claim", "claim", "remove", "report", "report", "report", "report", "report", "boot", "boot", "snapshot",
	}).Draw(t, "action") {
	case "new_op":
		m := s.machines[rapid.IntRange(0, len(s.machines)-1).Draw(t, "m")]
		if m.op != nil && !m.op.removed {
			return // global allows one active op per machine
		}
		typ := rapid.SampledFrom([]model.OperationType{model.OpDeploy, model.OpDeploy, model.OpCommission}).Draw(t, "type")
		if m.base == model.MachineStateRunning {
			return // a running machine is locked; not exercised here
		}
		op := &bindOp{id: uuid.New(), machine: bindIndexOf(s.machines, m), typ: typ}
		// Deterministic IDs so shrinking and replay are stable.
		op.id = uuid.MustParse(fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", len(s.ops)+1))
		s.ops = append(s.ops, op)
		m.op, m.running = op, false
		s.cache.ApplyOpDelta(model.Operation{ID: op.id, MachineID: m.id, Type: typ, Status: model.OpPending, Version: 0,
			TraceID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")}, "")
		s.stats["new_op"]++
	case "claim":
		m := s.machines[rapid.IntRange(0, len(s.machines)-1).Draw(t, "m")]
		op := m.op
		// A claim delta may land after the agent already failed the op: the
		// host can boot on the pending-op clause while the claim is delayed.
		// It must not revive the op.
		//
		// A claim delta that lands after the installed OS phoned home is
		// kept out of the history: it moves the machine's local state from
		// running back to deploying (an op_resync in the same position
		// keeps it running), so a retried phone-home is processed in full
		// and queues a second completion instead of being acknowledged. The
		// boot decision stays local either way.
		if op == nil || op.removed || op.claimed || op.completed {
			return
		}
		if op.failed {
			s.stats["claim_after_local_failure"]++
		}
		op.claimed, op.version = true, op.version+1
		transient := model.MachineStateDeploying
		if op.typ == model.OpCommission {
			transient = model.MachineStateCommissioning
		}
		s.cache.ApplyOpDelta(model.Operation{ID: op.id, MachineID: m.id, Type: op.typ, Status: model.OpClaimed, Version: op.version,
			TraceID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")}, transient)
		s.stats["claim"]++
	case "remove":
		// Global finishes the op the way the local outcome implies (or
		// times it out) and the identity snapshot catches up.
		m := s.machines[rapid.IntRange(0, len(s.machines)-1).Draw(t, "m")]
		op := m.op
		if op == nil || op.removed {
			return
		}
		durable := model.MachineStateFailed
		if op.completed {
			durable = model.MachineStateRunning
		}
		op.removed, op.version = true, op.version+1
		m.base = durable
		s.cache.ApplyOpRemoved(op.id.String(), m.id.String(), op.version, durable)
		s.pushSnapshot()
		if durable != model.MachineStateRunning {
			m.running = false
		}
		s.stats["remove"]++
	case "snapshot":
		s.pushSnapshot()
	case "boot":
		s.checkBoot(s.machines[rapid.IntRange(0, len(s.machines)-1).Draw(t, "m")])
	case "report":
		s.report()
	}
}

func bindIndexOf(ms []*bindMachine, m *bindMachine) int {
	for i := range ms {
		if ms[i] == m {
			return i
		}
	}
	return -1
}

func (s *bindSim) report() {
	t := s.t
	// Machine reference: usually a real one, sometimes unknown.
	mi := rapid.IntRange(0, 3*len(s.machines)).Draw(t, "report_machine")
	machineID := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	var m *bindMachine
	if mi < 3*len(s.machines) {
		m = s.machines[mi%len(s.machines)]
		machineID = m.id
	}
	// Op reference: this machine's current op, any op ever created
	// (stale or another machine's), or an unknown one.
	var op *bindOp
	opID := uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
	switch k := rapid.IntRange(0, 9).Draw(t, "report_op_kind"); {
	case k <= 5 && m != nil && m.op != nil:
		op = m.op
	case k <= 8 && len(s.ops) > 0:
		op = s.ops[rapid.IntRange(0, len(s.ops)-1).Draw(t, "report_op")]
	}
	if op != nil {
		opID = op.id
	}
	kind := rapid.SampledFrom([]string{
		"phase", "phase", "failed", "deploy_complete", "deploy_complete", "phone_home", "phone_home", "commission_report",
	}).Draw(t, "report_kind")
	rep := model.ProgressReport{OperationID: opID.String(), MachineID: machineID.String()}
	wantType := model.OperationType("")
	switch kind {
	case "phase":
		rep.Phase = rapid.SampledFrom([]string{"agent_starting", "partitioning", "installing_bootloader", "ipxe_loaded"}).Draw(t, "phase")
	case "failed":
		rep.Failed, rep.Phase, rep.Error = true, "partitioning", "boom"
	case "deploy_complete":
		rep.Event, wantType = "deploy_complete", model.OpDeploy
	case "phone_home":
		rep.Event, wantType = "phone_home", model.OpDeploy
	case "commission_report":
		rep.Event, wantType = "commission_report", model.OpCommission
		if m != nil {
			rep.CommissionData = s.commissionData(m)
		} else {
			rep.CommissionData = json.RawMessage(`{"system":{"uuid":"x"},"nics":[]}`)
		}
	}

	// Model decision (I1).
	accept, ackOnly := false, false
	switch {
	case m == nil:
	case kind == "phone_home" && m.running:
		accept, ackOnly = true, true
	case op == nil || !op.activeForReports() || s.machines[op.machine] != m:
	case wantType != "" && op.typ != wantType:
	case kind == "phone_home" && !op.awaiting:
		// The installed OS cannot have booted a disk the agent has not
		// finished writing.
	default:
		accept = true
	}

	before := s.bootDecisions()
	body, _ := json.Marshal(rep)
	code, respBody := s.do("POST", "/api/v1/report", body)
	obs, upd := s.drain()
	got := code >= 200 && code < 300
	if got != accept {
		t.Fatalf("report %s (machine=%v op=%+v) -> HTTP %d %q, model says accept=%v", kind, mi, op, code, strings.TrimSpace(respBody), accept)
	}
	if !accept || ackOnly {
		s.stats["report_rejected_or_ack"]++
		if op != nil && (op.removed || s.machines[op.machine] != m) {
			s.stats["report_stale_or_foreign_op"]++
		}
		// I2
		if len(obs) != 0 || len(upd) != 0 {
			t.Fatalf("report %s was %d but queued observations=%+v updates=%+v", kind, code, obs, upd)
		}
		if after := s.bootDecisions(); after != before {
			t.Fatalf("report %s was %d but boot decisions changed:\n before %s\n after  %s", kind, code, before, after)
		}
		return
	}
	s.stats["report_accepted_"+kind]++
	// I3
	for _, u := range upd {
		if u.OperationID != op.id.String() {
			t.Fatalf("accepted report for op %s queued an update for op %s", op.id, u.OperationID)
		}
		// I5
		if u.Status == string(model.OpCompleted) && kind != "phone_home" {
			t.Fatalf("report %s completed op %s", kind, op.id)
		}
	}
	for _, o := range obs {
		if o.MachineID != m.id || (o.OperationID != uuid.Nil && o.OperationID != op.id) {
			t.Fatalf("accepted report for machine %s op %s queued observation %+v", m.id, op.id, o)
		}
	}
	statuses := map[string]bool{}
	for _, u := range upd {
		statuses[u.Status] = true
	}
	switch kind {
	case "phase":
		// A late phase report is accepted but never takes the op off
		// awaiting phone-home.
		if op.awaiting {
			s.stats["phase_report_while_awaiting"]++
		}
	case "failed":
		if !statuses["failed"] {
			t.Fatalf("accepted failure report queued no failed update: %+v", upd)
		}
		op.failed = true
	case "deploy_complete":
		op.awaiting = true
	case "phone_home":
		if !statuses["completed"] {
			t.Fatalf("accepted phone_home queued no completed update: %+v", upd)
		}
		op.completed, m.running = true, true
	}
}

func reportBindingStats(t *testing.T, stats map[string]int) {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, stats[k])
	}
	t.Logf("coverage:%s", b.String())
}

// macOnlyPhoneHome is I5's second half: POST /api/v1/phone-home?mac= names
// no operation, so at every point of the history it is refused, queues
// nothing and changes no boot decision.
func (s *bindSim) macOnlyPhoneHome() {
	m := s.machines[rapid.IntRange(0, len(s.machines)-1).Draw(s.t, "mac_only_m")]
	before := s.bootDecisions()
	code, _ := s.do("POST", "/api/v1/phone-home?mac="+m.mac, nil)
	obs, upd := s.drain()
	s.stats["mac_only_phone_home"]++
	if m.op != nil && m.op.activeForReports() && m.op.awaiting {
		s.stats["mac_only_phone_home_while_awaiting"]++
	}
	if code >= 200 && code < 300 {
		s.t.Fatalf("POST /api/v1/phone-home?mac=%s answered %d, want a refusal (op %+v)", m.mac, code, m.op)
	}
	if len(obs) != 0 || len(upd) != 0 {
		s.t.Fatalf("POST /api/v1/phone-home?mac=%s (HTTP %d) queued observations=%+v updates=%+v", m.mac, code, obs, upd)
	}
	if after := s.bootDecisions(); after != before {
		s.t.Fatalf("POST /api/v1/phone-home?mac=%s changed boot decisions:\n before %s\n after  %s (op %+v)", m.mac, before, after, m.op)
	}
}

// TestProp_ReportBinding_History runs a random history of op-stream events,
// agent reports, MAC-only phone-homes and boot fetches, checking I1-I5 after
// every step and I4 for every machine at the end.
func TestProp_ReportBinding_History(t *testing.T) {
	stats := map[string]int{}
	rapid.Check(t, func(t *rapid.T) {
		s := newBindSim(t, stats)
		n := rapid.IntRange(1, 80).Draw(t, "steps")
		for i := 0; i < n; i++ {
			if rapid.IntRange(0, 9).Draw(t, "mac_only") == 0 {
				s.macOnlyPhoneHome()
				continue
			}
			s.step()
		}
		for _, m := range s.machines {
			s.checkBoot(m)
		}
	})
	reportBindingStats(t, stats)
}

// TestProp_MACNotation_SameMachine: every notation the endpoints accept for
// one MAC must resolve to the same machine as the canonical lower-case form
// the snapshot stores, on every MAC-keyed endpoint.
func TestProp_MACNotation_SameMachine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		stats := map[string]int{}
		s := newBindSim(t, stats)
		m := s.machines[0]
		// Put the machine mid-deploy so the canonical answer is an installer.
		op := &bindOp{id: uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-000000000001"), typ: model.OpDeploy, claimed: true, version: 1}
		m.op = op
		s.cache.ApplyOpDelta(model.Operation{ID: op.id, MachineID: m.id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1}, model.MachineStateDeploying)

		canonClass, canonMachine, canonOp := s.bootClass(m.mac)
		if canonClass != "deploy" {
			t.Fatalf("setup: canonical MAC got %s", canonClass)
		}
		// Mixed-case renderings of the same six octets.
		b := []byte(m.mac)
		for i := range b {
			if b[i] >= 'a' && b[i] <= 'f' && rapid.Bool().Draw(t, "upper") {
				b[i] -= 'a' - 'A'
			}
		}
		variant := string(b)
		class, mid, oid := s.bootClass(variant)
		if strings.HasPrefix(class, "http4") {
			return // rejecting a notation outright is consistent
		}
		if class != canonClass || mid != canonMachine || oid != canonOp {
			t.Fatalf("GET /boot/ipxe?mac=%s -> %s/%s/%s, but the same MAC written %s -> %s/%s/%s",
				variant, class, mid, oid, m.mac, canonClass, canonMachine, canonOp)
		}
	})
}
