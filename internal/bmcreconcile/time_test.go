package bmcreconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	fakeRedfishUser = "ADMIN"
	fakeRedfishPass = "adm!n-pw"
	testVIP         = "10.254.240.1"
)

// fakeRedfishBMC is an in-process Redfish BMC exposing the surface the time
// pass touches (Managers, the manager, its NetworkProtocol and, for an
// iDRAC, its Attributes), with switches for the vendor behaviour seen on
// the fleet (2026-10-28 survey and canaries):
//
//   - strictETag: Supermicro refuses `If-Match: *` (412) and wants the
//     resource's real ETag.
//   - asyncNTPGets: AMI and Supermicro answer a NetworkProtocol PATCH
//     with 202 and apply it later (here: after that many NetworkProtocol
//     GETs).
//   - dropNTPEnable, dropNTPDisable: the async task fails, so the PATCH
//     answers 202 and never applies (Supermicro X12/X13 on 01.03.31).
//   - reportNTPOff: one ASUS model reports ProtocolEnabled=false while it
//     keeps syncing.
//   - taskEnd: Supermicro answers an NTP PATCH 202 with a Redfish Task
//     body, and the Task applies the change only if it ends Completed
//     (here: the Task answers Running for taskRunning GETs, then taskEnd).
//     taskGone: the Task ran to taskEnd at once and its resource 404s.
//   - echoNTP: Supermicro's first NetworkProtocol read after an NTP PATCH
//     echoes the PATCHed values, applied or not (nova13.pop2, 2026-10-30).
//   - ignoreSetWhileNTP: Supermicro silently ignores IPMI Set SEL Time
//     while NTP is enabled.
//
// The BMC clock is the real clock plus skew, reported in offset's zone.
type fakeRedfishBMC struct {
	t   *testing.T
	srv *httptest.Server

	mu                sync.Mutex
	managerID         string
	memberLink        string // overrides the Managers member link when set
	skew              time.Duration
	offset            string
	timezone          string // iDRAC Time.1.Timezone
	ntpEnabled        bool
	ntpServers        []string // as reported, slot-padded with ""
	strictETag        bool
	asyncNTPGets      int
	ignoreSetWhileNTP bool
	ignoreSet         bool // the clock never moves
	failNTPEnable     bool // a PATCH enabling NTP answers 500
	dropNTPEnable     bool // a PATCH enabling NTP answers 202 and never applies
	dropNTPDisable    bool // a PATCH disabling NTP answers 202 and never applies
	hangUpNTPDisable  bool // a PATCH disabling NTP applies, then the connection drops unanswered
	lateNTPDisable    int  // with hangUpNTPDisable: the disable applies only after this many NetworkProtocol GETs
	reportNTPOff      bool // NetworkProtocol GETs report ProtocolEnabled=false
	taskEnd           string
	taskRunning       int
	taskMessages      []string // Messages of an ended Task
	taskGone          bool
	echoNTP           bool
	stale412          int  // answer the next N PATCHes with 412
	noManagers        bool // Managers collection is empty

	gen        map[string]int // per-resource ETag generation
	pending    *ntpPatchBody  // an accepted async NTP PATCH not yet applied
	pendingIn  int            // NetworkProtocol GETs until pending applies
	late       *ntpPatchBody  // an unanswered disable not yet applied (lateNTPDisable)
	lateIn     int            // NetworkProtocol GETs until late applies
	echo       *ntpPatchBody  // what the next NetworkProtocol GET echoes
	tasks      map[string]*fakeTask
	patches    []fakePatch
	setCalls   []bool   // ntpEnabled at each Set SEL Time
	events     []string // Task ends ("<path> <state>") and "step", in order
	requests   int
	otherHosts int // requests carrying a Host other than the server's
}

// fakeTask is a Redfish Task applying an NTP PATCH.
type fakeTask struct {
	running int // GETs left answering Running
	ended   bool
	apply   ntpPatchBody // applied when the Task ends Completed
}

type fakePatch struct {
	Path    string
	IfMatch string
	Body    map[string]any
}

// ntpPatchBody mirrors the NTP PATCH wire shape for the fake's decoding;
// NTPServers is nil when the PATCH did not carry it.
type ntpPatchBody struct {
	NTP struct {
		ProtocolEnabled *bool    `json:"ProtocolEnabled"`
		NTPServers      []string `json:"NTPServers"`
	} `json:"NTP"`
}

func newFakeRedfishBMC(t *testing.T, managerID string) *fakeRedfishBMC {
	t.Helper()
	f := &fakeRedfishBMC{t: t, managerID: managerID, offset: "+00:00", gen: map[string]int{}, tasks: map[string]*fakeTask{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRedfishBMC) addr() string { return f.srv.Listener.Addr().String() }

func (f *fakeRedfishBMC) managerPath() string { return "/redfish/v1/Managers/" + f.managerID }

func (f *fakeRedfishBMC) etag(path string) string {
	return fmt.Sprintf(`"%s-%d"`, path[strings.LastIndex(path, "/")+1:], f.gen[path])
}

func (f *fakeRedfishBMC) clock() string {
	loc := time.UTC
	if f.offset != "+00:00" {
		ot, err := time.Parse("-07:00", f.offset)
		if err != nil {
			f.t.Errorf("fake BMC offset %q: %v", f.offset, err)
		}
		_, sec := ot.Zone()
		loc = time.FixedZone(f.offset, sec)
	}
	return time.Now().Add(f.skew).In(loc).Format("2006-01-02T15:04:05-07:00")
}

func (f *fakeRedfishBMC) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Host != f.addr() {
		f.otherHosts++
	}
	if u, p, ok := r.BasicAuth(); !ok || u != fakeRedfishUser || p != fakeRedfishPass {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	mgr := f.managerPath()
	np := mgr + "/NetworkProtocol"
	attrs := mgr + "/Attributes"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/redfish/v1/Managers":
		members := []map[string]string{}
		if !f.noManagers {
			link := mgr
			if f.memberLink != "" {
				link = f.memberLink
			}
			members = append(members, map[string]string{"@odata.id": link})
		}
		f.json(w, "", map[string]any{"Members": members})
	case r.Method == http.MethodGet && r.URL.Path == mgr:
		f.json(w, f.etag(mgr), map[string]any{
			"Id": f.managerID, "DateTime": f.clock(), "DateTimeLocalOffset": f.offset,
			"NetworkProtocol": map[string]string{"@odata.id": np},
		})
	case r.Method == http.MethodGet && r.URL.Path == np:
		if f.late != nil {
			if f.lateIn--; f.lateIn <= 0 {
				f.applyNTP(*f.late)
				f.late = nil
			}
		}
		if f.pending != nil {
			if f.pendingIn--; f.pendingIn <= 0 {
				f.applyNTP(*f.pending)
				f.pending = nil
			}
		}
		enabled, servers := f.ntpEnabled, f.ntpServers
		if f.echo != nil {
			enabled, servers = patchedNTP(enabled, slices.Clone(servers), *f.echo)
			f.echo = nil
		}
		f.json(w, f.etag(np), map[string]any{
			"NTP": map[string]any{"ProtocolEnabled": enabled && !f.reportNTPOff, "NTPServers": servers},
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/redfish/v1/TaskService/Tasks/"):
		f.getTask(w, r.URL.Path)
	case r.Method == http.MethodGet && r.URL.Path == attrs && strings.HasPrefix(f.managerID, "iDRAC"):
		f.json(w, f.etag(attrs), map[string]any{"Attributes": map[string]any{"Time.1.Timezone": f.timezone}})
	case r.Method == http.MethodPatch && (r.URL.Path == mgr || r.URL.Path == np || r.URL.Path == attrs):
		f.patch(w, r)
	default:
		http.Error(w, "not found: "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

// json writes v with its ETag both as the header and as @odata.etag, the
// two places BMCs expose it.
func (f *fakeRedfishBMC) json(w http.ResponseWriter, etag string, v map[string]any) {
	if etag != "" {
		w.Header().Set("ETag", etag)
		v["@odata.etag"] = etag
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeRedfishBMC) patch(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	ifMatch := r.Header.Get("If-Match")
	f.patches = append(f.patches, fakePatch{Path: r.URL.Path, IfMatch: ifMatch, Body: body})
	if f.stale412 > 0 {
		f.stale412--
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}
	switch {
	case ifMatch == "*" && !f.strictETag:
	case ifMatch == f.etag(r.URL.Path):
	default:
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	}
	mgr := f.managerPath()
	switch r.URL.Path {
	case mgr:
		f.offset = body["DateTimeLocalOffset"].(string)
	case mgr + "/Attributes":
		f.timezone = body["Attributes"].(map[string]any)["Time.1.Timezone"].(string)
	default:
		var p ntpPatchBody
		_ = json.Unmarshal(raw, &p)
		enable := p.NTP.ProtocolEnabled != nil && *p.NTP.ProtocolEnabled
		if f.failNTPEnable && enable {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if f.echoNTP {
			f.echo = &p
		}
		if f.taskEnd != "" {
			f.startTask(w, r.URL.Path, p)
			return
		}
		if (f.dropNTPEnable && enable) || (f.dropNTPDisable && !enable) {
			f.gen[r.URL.Path]++
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if f.asyncNTPGets > 0 {
			f.pending, f.pendingIn = &p, f.asyncNTPGets
			f.gen[r.URL.Path]++
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if f.hangUpNTPDisable && !enable {
			if f.lateNTPDisable > 0 {
				f.late, f.lateIn = &p, f.lateNTPDisable
			} else {
				f.applyNTP(p)
			}
			f.gen[r.URL.Path]++
			f.hangUp(w)
			return
		}
		f.applyNTP(p)
	}
	f.gen[r.URL.Path]++
	w.WriteHeader(http.StatusOK)
}

// hangUp closes the connection without answering: a write the BMC applied
// whose response never arrives, as when its web server restarts, the
// connection drops, or the client gives up first.
func (f *fakeRedfishBMC) hangUp(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		f.t.Errorf("fake BMC hang-up: %v", err)
		return
	}
	_ = conn.Close()
}

// startTask answers an NTP PATCH the way Supermicro does: 202, a
// TaskMonitor Location, and a Running Task body. The Task applies p if it
// ends Completed; a taskGone Task has already ended and 404s.
func (f *fakeRedfishBMC) startTask(w http.ResponseWriter, npPath string, p ntpPatchBody) {
	f.gen[npPath]++
	path := fmt.Sprintf("/redfish/v1/TaskService/Tasks/%d", len(f.tasks)+1)
	t := &fakeTask{running: f.taskRunning, apply: p}
	if f.taskGone {
		t.ended = true
		if f.taskEnd == "Completed" {
			f.applyNTP(p)
		}
	}
	f.tasks[path] = t
	w.Header().Set("Location", "/redfish/v1/TaskMonitor/c2VjcmV0")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"@odata.type": "#Task.v1_5_1.Task", "@odata.id": path, "TaskState": "Running", "PercentComplete": 50,
	})
}

// getTask serves a Task GET. The first GET that finds it ended applies
// its PATCH (if Completed) and records the end in events.
func (f *fakeRedfishBMC) getTask(w http.ResponseWriter, path string) {
	t := f.tasks[path]
	if t == nil || f.taskGone {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body := map[string]any{"@odata.type": "#Task.v1_5_1.Task", "@odata.id": path, "TaskState": "Running", "PercentComplete": 50}
	if t.running > 0 {
		t.running--
		f.json(w, "", body)
		return
	}
	if !t.ended {
		t.ended = true
		if f.taskEnd == "Completed" {
			f.applyNTP(t.apply)
		}
		f.events = append(f.events, path+" "+f.taskEnd)
	}
	var msgs []map[string]string
	for _, m := range f.taskMessages {
		msgs = append(msgs, map[string]string{"Message": m})
	}
	body["TaskState"], body["Messages"] = f.taskEnd, msgs
	f.json(w, "", body)
}

// applyNTP applies an NTP PATCH.
func (f *fakeRedfishBMC) applyNTP(p ntpPatchBody) {
	f.ntpEnabled, f.ntpServers = patchedNTP(f.ntpEnabled, f.ntpServers, p)
}

// patchedNTP is NTP state with p applied, with element-wise array
// semantics: a shorter NTPServers array leaves the trailing slots as they
// were. It may reuse servers' backing array.
func patchedNTP(enabled bool, servers []string, p ntpPatchBody) (bool, []string) {
	if p.NTP.ProtocolEnabled != nil {
		enabled = *p.NTP.ProtocolEnabled
	}
	for i, s := range p.NTP.NTPServers {
		if i < len(servers) {
			servers[i] = s
		} else {
			servers = append(servers, s)
		}
	}
	return enabled, servers
}

// setSELTimeNow stands in for `ipmitool sel time set now`.
func (f *fakeRedfishBMC) setSELTimeNow(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, f.ntpEnabled)
	f.events = append(f.events, "step")
	if f.ignoreSet || (f.ignoreSetWhileNTP && f.ntpEnabled) {
		return nil
	}
	f.skew = 0
	return nil
}

func (f *fakeRedfishBMC) reconciler() *timeReconciler {
	return &timeReconciler{
		rf:            newRedfishClient(f.addr(), fakeRedfishUser, fakeRedfishPass, f.srv.Client()),
		stepClock:     f.setSELTimeNow,
		pollInterval:  5 * time.Millisecond,
		settleTimeout: 2 * time.Second,
	}
}

func (f *fakeRedfishBMC) run(servers ...string) TimeResult {
	f.t.Helper()
	return f.reconciler().run(context.Background(), servers)
}

// state returns the fake's NTP state and offset under its lock.
func (f *fakeRedfishBMC) state() (enabled bool, servers []string, offset, tz string, skew time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ntpEnabled, slices.Clone(f.ntpServers), f.offset, f.timezone, f.skew
}

func (f *fakeRedfishBMC) patchesTo(suffix string) []fakePatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakePatch
	for _, p := range f.patches {
		if strings.HasSuffix(p.Path, suffix) {
			out = append(out, p)
		}
	}
	return out
}

func ntpEnabledIn(p fakePatch) any { return p.Body["NTP"].(map[string]any)["ProtocolEnabled"] }

func ntpServersIn(p fakePatch) []string {
	raw, ok := p.Body["NTP"].(map[string]any)["NTPServers"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(raw))
	for i, v := range raw {
		out[i] = v.(string)
	}
	return out
}

// TestReconcileTime_SupermicroRetiredServerClockYearsOff is the pop2
// old-cage shape: NTP pointed at the retired 10.1.1.99, clock years off, a
// BMC that wants real ETags and ignores Set SEL Time while NTP is on. The
// pass disables NTP, steps the clock while NTP is off, then re-enables NTP
// with the VIP as the only server.
func TestReconcileTime_SupermicroRetiredServerClockYearsOff(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.ignoreSetWhileNTP = true, true
	f.ntpEnabled, f.ntpServers = true, []string{"10.1.1.99", ""}
	f.skew = -2 * 365 * 24 * time.Hour

	res := f.run(testVIP)

	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if !res.ClockStepped || !res.NTPConfigured || res.TimezoneSet {
		t.Errorf("result = %+v, want stepped + NTP configured, timezone untouched", res)
	}
	if res.ClockSkew > -700*24*time.Hour {
		t.Errorf("ClockSkew = %v, want the initial ~-2y skew", res.ClockSkew)
	}
	if !slices.Equal(f.setCalls, []bool{false}) {
		t.Errorf("Set SEL Time calls (NTP enabled at each) = %v, want exactly one with NTP off", f.setCalls)
	}
	ntp := f.patchesTo("/NetworkProtocol")
	if len(ntp) != 2 || ntpEnabledIn(ntp[0]) != false || ntpEnabledIn(ntp[1]) != true {
		t.Fatalf("NetworkProtocol PATCHes = %+v, want disable then enable", ntp)
	}
	if got := ntpServersIn(ntp[1]); !slices.Equal(got, []string{testVIP}) {
		t.Errorf("enable PATCH NTPServers = %v, want [%s]", got, testVIP)
	}
	for _, p := range f.patchesTo("") {
		if p.IfMatch == "*" || p.IfMatch == "" {
			t.Errorf("PATCH %s sent If-Match %q; Supermicro needs the resource's real ETag", p.Path, p.IfMatch)
		}
	}
	enabled, servers, _, _, skew := f.state()
	if !enabled || !slices.Equal(servers, []string{testVIP, ""}) || skew != 0 {
		t.Errorf("BMC after = enabled %v servers %v skew %v", enabled, servers, skew)
	}
}

// TestReconcileTime_DellNTPOffCST6CDT is the iDRAC shape: NTP off, no
// servers, timezone CST6CDT, clock right by luck. The pass sets the
// Time.1.Timezone attribute to UTC and enables NTP with the VIP; no step.
func TestReconcileTime_DellNTPOffCST6CDT(t *testing.T) {
	f := newFakeRedfishBMC(t, "iDRAC.Embedded.1")
	f.timezone, f.offset = "CST6CDT", "-05:00"
	f.ntpServers = []string{"", "", ""}

	res := f.run(testVIP)

	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if !res.TimezoneSet || !res.NTPConfigured || res.ClockStepped {
		t.Errorf("result = %+v, want timezone set + NTP configured, no step", res)
	}
	if len(f.setCalls) != 0 {
		t.Errorf("clock stepped on a correct clock: %v", f.setCalls)
	}
	if len(f.patchesTo(f.managerPath())) != 0 {
		t.Error("iDRAC timezone must go through the Time.1.Timezone attribute, not DateTimeLocalOffset")
	}
	enabled, servers, _, tz, _ := f.state()
	if tz != "UTC" || !enabled || !slices.Equal(servers, []string{testVIP, "", ""}) {
		t.Errorf("BMC after = tz %q enabled %v servers %v", tz, enabled, servers)
	}
}

// TestReconcileTime_AMIOffsetMinusFive is the ASUS shape: NTP already on
// the VIP (from DHCP), clock right, but the manager reports -05:00. The
// pass sets DateTimeLocalOffset to +00:00 and leaves NTP alone.
func TestReconcileTime_AMIOffsetMinusFive(t *testing.T) {
	f := newFakeRedfishBMC(t, "Self")
	f.offset = "-05:00"
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}

	res := f.run(testVIP)

	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if !res.TimezoneSet || res.NTPConfigured || res.ClockStepped {
		t.Errorf("result = %+v, want only the timezone set", res)
	}
	if len(f.patchesTo("/NetworkProtocol")) != 0 {
		t.Error("compliant NTP was rewritten")
	}
	if _, _, off, _, _ := f.state(); off != "+00:00" {
		t.Errorf("offset after = %q, want +00:00", off)
	}
}

// TestReconcileTime_AMIProtocolEnabledFlapIsCompliant: one ASUS model
// reports ProtocolEnabled=false while it keeps syncing from the
// DHCP-supplied server. With the right server listed, the clock within a
// minute and the offset UTC, that is compliant: nothing is written, so a
// re-run does not keep re-enabling it. The same state on a non-AMI BMC is
// NTP genuinely off and is fixed.
func TestReconcileTime_AMIProtocolEnabledFlapIsCompliant(t *testing.T) {
	ami := newFakeRedfishBMC(t, "Self")
	ami.ntpServers = []string{testVIP, ""}
	ami.skew = 5 * time.Second
	if res := ami.run(testVIP); res.Err != nil || res.NTPConfigured || res.ClockStepped || res.TimezoneSet {
		t.Errorf("AMI result = %+v, want compliant with no writes", res)
	}
	if n := len(ami.patchesTo("")); n != 0 {
		t.Errorf("AMI got %d PATCHes, want none", n)
	}

	smc := newFakeRedfishBMC(t, "1")
	smc.ntpServers = []string{testVIP, ""}
	if res := smc.run(testVIP); res.Err != nil || !res.NTPConfigured {
		t.Errorf("Supermicro result = %+v, want NTP enabled", res)
	}
	if enabled, _, _, _, _ := smc.state(); !enabled {
		t.Error("Supermicro NTP still disabled")
	}
}

// TestReconcileTime_AlreadyCompliant: nothing to do means no writes and no
// step, and Redfish is only read.
func TestReconcileTime_AlreadyCompliant(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = 20 * time.Second

	res := f.run(testVIP)

	if res.Err != nil || res.NTPConfigured || res.TimezoneSet || res.ClockStepped {
		t.Errorf("result = %+v, want compliant with no writes", res)
	}
	if res.ClockSkew < 19*time.Second || res.ClockSkew > 21*time.Second {
		t.Errorf("ClockSkew = %v, want ~20s", res.ClockSkew)
	}
	if n := len(f.patchesTo("")); n != 0 || len(f.setCalls) != 0 {
		t.Errorf("writes on a compliant BMC: %d PATCHes, %d steps", n, len(f.setCalls))
	}
}

// TestReconcileTime_ClearsStaleSecondary: "the cage's NTP server as the only
// configured server" — a stale server in a later slot is cleared with "",
// because a shorter PATCH array can leave trailing slots in place.
func TestReconcileTime_ClearsStaleSecondary(t *testing.T) {
	f := newFakeRedfishBMC(t, "iDRAC.Embedded.1")
	f.timezone = "UTC"
	f.ntpEnabled, f.ntpServers = true, []string{"10.254.243.254", "10.1.1.99", ""}

	res := f.run(testVIP)

	if res.Err != nil || !res.NTPConfigured {
		t.Fatalf("result = %+v", res)
	}
	ntp := f.patchesTo("/NetworkProtocol")
	if len(ntp) != 1 || !slices.Equal(ntpServersIn(ntp[0]), []string{testVIP, "", ""}) {
		t.Errorf("NetworkProtocol PATCHes = %+v, want one with [%s \"\" \"\"]", ntp, testVIP)
	}
	if _, servers, _, _, _ := f.state(); !slices.Equal(servers, []string{testVIP, "", ""}) {
		t.Errorf("servers after = %v", servers)
	}
}

// TestReconcileTime_ReenableFailureIsLoud: a step that disabled NTP and
// then failed to re-enable it leaves the BMC worse than before; the error
// must say NTP is disabled.
func TestReconcileTime_ReenableFailureIsLoud(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = time.Hour
	f.failNTPEnable = true

	res := f.run(testVIP)

	if res.Err == nil || !strings.Contains(res.Err.Error(), "NTP is DISABLED") {
		t.Fatalf("Err = %v, want it to say NTP was left disabled", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "re-run `slagctl machine bmc-reconcile`") {
		t.Errorf("Err = %v, want the what-to-do", res.Err)
	}
}

// TestReconcileTime_StepDisableWithNoResponseIsReenabled: the step's NTP
// disable reaches the BMC and applies, but no response comes back (here the
// connection drops; a client timeout or ctx ending mid-PATCH is the same).
// NTP was compliant before, so only the step's re-enable can restore it:
// the pass must treat the disable as possibly applied, re-enable NTP and
// read it back, and not step a clock it cannot know is free of NTP.
func TestReconcileTime_StepDisableWithNoResponseIsReenabled(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.hangUpNTPDisable = true
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = time.Hour

	res := f.runShortSettle(testVIP)

	if enabled, servers, _, _, _ := f.state(); !enabled || !slices.Equal(servers, []string{testVIP, ""}) {
		t.Errorf("BMC after = enabled %v servers %v, want NTP re-enabled on the VIP (Err = %v)", enabled, servers, res.Err)
	}
	ntp := f.patchesTo("/NetworkProtocol")
	if len(ntp) != 2 || ntpEnabledIn(ntp[0]) != false || ntpEnabledIn(ntp[1]) != true {
		t.Errorf("NetworkProtocol PATCHes = %+v, want the unanswered disable, then an enable", ntp)
	}
	if res.ClockStepped || len(f.setCalls) != 0 {
		t.Errorf("stepped the clock (calls %v) after a disable with no response", f.setCalls)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "disable NTP for the clock step: ") {
		t.Fatalf("Err = %v, want the failed disable reported", res.Err)
	}
	if strings.Contains(res.Err.Error(), "DISABLED") {
		t.Errorf("Err = %v, but NTP was re-enabled", res.Err)
	}
}

// TestReconcileTime_StepWaitsForDisableWithNoResponse: the unanswered
// disable is still applying when the pass gets to the re-enable (AMI
// applies NTP PATCHes asynchronously). Re-enabling at once would let the
// disable land after the re-enable was read back, leaving NTP off with no
// alarm; the pass must wait for NTP to read disabled first, as it does
// after an accepted disable.
func TestReconcileTime_StepWaitsForDisableWithNoResponse(t *testing.T) {
	f := newFakeRedfishBMC(t, "Self")
	f.hangUpNTPDisable, f.lateNTPDisable = true, 10
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = time.Hour

	res := f.run(testVIP)

	f.mu.Lock()
	if f.late != nil { // the BMC applies what it accepted, eventually
		f.applyNTP(*f.late)
		f.late = nil
	}
	f.mu.Unlock()
	if enabled, _, _, _, _ := f.state(); !enabled {
		t.Errorf("the disable landed after the re-enable and left NTP off (Err = %v)", res.Err)
	}
}

// TestReconcileTime_AMIStepDisableWithNoResponseReenableThatNeverAppliesIsLoud:
// as above on AMI, where the re-enable is accepted but never applies. The
// unanswered disable may be why NTP reads off, so, as after an accepted
// disable, matching servers do not confirm the re-enable, and the error
// says NTP is left disabled.
func TestReconcileTime_AMIStepDisableWithNoResponseReenableThatNeverAppliesIsLoud(t *testing.T) {
	f := newFakeRedfishBMC(t, "Self")
	f.hangUpNTPDisable, f.dropNTPEnable = true, true
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = time.Hour

	res := f.runShortSettle(testVIP)

	if enabled, _, _, _, _ := f.state(); enabled {
		t.Fatal("fake BMC NTP enabled; the test does not exercise a dropped re-enable")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "NTP is DISABLED") {
		t.Fatalf("Err = %v, want it to say NTP was left disabled", res.Err)
	}
}

// TestReconcileTime_StepThatDoesNotTakeIsReported: the BMC accepted the
// step but its clock did not move; re-reading the clock catches it.
func TestReconcileTime_StepThatDoesNotTakeIsReported(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.ntpEnabled, f.ntpServers = true, []string{testVIP}
	f.skew = 20 * 24 * time.Hour
	f.ignoreSet = true

	res := f.run(testVIP)

	if res.Err == nil || !strings.Contains(res.Err.Error(), "still") {
		t.Fatalf("Err = %v, want the clock still off after the step", res.Err)
	}
	if enabled, _, _, _, _ := f.state(); !enabled {
		t.Error("NTP left disabled after the step")
	}
}

// TestReconcileTime_AMIAsyncDisableIsAwaited: AMI applies an NTP PATCH
// asynchronously (202); the step waits for NTP to read disabled before
// Set SEL Time, which a BMC ignores while NTP is on.
func TestReconcileTime_AMIAsyncDisableIsAwaited(t *testing.T) {
	f := newFakeRedfishBMC(t, "Self")
	f.asyncNTPGets = 3
	f.ignoreSetWhileNTP = true
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = 20 * 24 * time.Hour

	res := f.run(testVIP)

	if res.Err != nil || !res.ClockStepped {
		t.Fatalf("result = %+v, want a successful step", res)
	}
	if !slices.Equal(f.setCalls, []bool{false}) {
		t.Errorf("Set SEL Time ran with NTP enabled = %v, want it after the async disable applied", f.setCalls)
	}
}

// shortSettle bounds the read-back in tests where the NTP change never
// applies, so they wait out settleTimeout quickly.
const shortSettle = 100 * time.Millisecond

func (f *fakeRedfishBMC) runShortSettle(servers ...string) TimeResult {
	f.t.Helper()
	r := f.reconciler()
	r.settleTimeout = shortSettle
	return r.run(context.Background(), servers)
}

// TestReconcileTime_AsyncEnableIsReadBack: Supermicro answers the NTP
// enable with 202 and applies it by the next read; NTP is configured only
// once it reads back enabled on the VIP.
func TestReconcileTime_AsyncEnableIsReadBack(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.asyncNTPGets = 1
	f.ntpServers = []string{testVIP, ""}

	res := f.run(testVIP)

	if res.Err != nil || !res.NTPConfigured {
		t.Fatalf("result = %+v, want NTP configured", res)
	}
	if enabled, servers, _, _, _ := f.state(); !enabled || !slices.Equal(servers, []string{testVIP, ""}) {
		t.Errorf("BMC after = enabled %v servers %v; the enable was not read back", enabled, servers)
	}
}

// TestReconcileTime_AsyncEnableAppliesLate: the first read-backs still
// show the old state (the task is running); the pass keeps polling until
// the new one shows.
func TestReconcileTime_AsyncEnableAppliesLate(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.asyncNTPGets = 3
	f.ntpServers = []string{"10.1.1.99", ""}

	res := f.run(testVIP)

	if res.Err != nil || !res.NTPConfigured {
		t.Fatalf("result = %+v, want NTP configured", res)
	}
	if enabled, servers, _, _, _ := f.state(); !enabled || !slices.Equal(servers, []string{testVIP, ""}) {
		t.Errorf("BMC after = enabled %v servers %v; the pass stopped before the task applied", enabled, servers)
	}
}

// TestReconcileTime_AcceptedEnableThatNeverAppliesIsReported is the nova13
// shape (2026-10-29): Supermicro accepts the NTP enable with 202, the task
// fails, and NTP stays disabled. That is not configured, and the error
// says what the BMC still reports.
func TestReconcileTime_AcceptedEnableThatNeverAppliesIsReported(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.dropNTPEnable = true
	f.ntpServers = []string{testVIP, ""}

	res := f.runShortSettle(testVIP)

	if res.NTPConfigured {
		t.Errorf("NTPConfigured = true for an enable that never applied")
	}
	want := "configure NTP: the BMC accepted the NTP change but still reports NTP disabled with servers [" + testVIP + "]"
	if res.Err == nil || !strings.Contains(res.Err.Error(), want) {
		t.Fatalf("Err = %v, want it to contain %q", res.Err, want)
	}
	if !strings.Contains(res.Err.Error(), "re-run `slagctl machine bmc-reconcile`") {
		t.Errorf("Err = %v, want the what-to-do", res.Err)
	}
}

// TestReconcileTime_StepReenableThatNeverAppliesIsLoud: the step disabled
// NTP, and the BMC accepted the re-enable but never applied it, so NTP is
// left off; the error must say so like a refused re-enable does.
func TestReconcileTime_StepReenableThatNeverAppliesIsLoud(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.ignoreSetWhileNTP = true, true
	f.dropNTPEnable = true
	f.ntpEnabled, f.ntpServers = true, []string{"10.1.1.99", ""}
	f.skew = time.Hour

	res := f.runShortSettle(testVIP)

	if !res.ClockStepped || res.NTPConfigured {
		t.Errorf("result = %+v, want stepped, NTP not configured", res)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "NTP is DISABLED") ||
		!strings.Contains(res.Err.Error(), "still reports NTP disabled with servers [10.1.1.99]") {
		t.Fatalf("Err = %v, want it to say NTP was left disabled, and what the BMC reports", res.Err)
	}
	if enabled, _, _, _, _ := f.state(); enabled {
		t.Error("fake BMC NTP enabled; the test does not exercise a dropped re-enable")
	}
}

// TestReconcileTime_AMIStepReenableThatNeverAppliesIsLoud: on AMI the
// servers were already right, so after the step's own disable a read-back
// that accepted matching servers would pass at once; it must see NTP on.
func TestReconcileTime_AMIStepReenableThatNeverAppliesIsLoud(t *testing.T) {
	f := newFakeRedfishBMC(t, "Self")
	f.dropNTPEnable = true
	f.ntpEnabled, f.ntpServers = true, []string{testVIP, ""}
	f.skew = time.Hour

	res := f.runShortSettle(testVIP)

	if !res.ClockStepped || res.NTPConfigured {
		t.Errorf("result = %+v, want stepped, NTP not configured", res)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "NTP is DISABLED") {
		t.Fatalf("Err = %v, want it to say NTP was left disabled", res.Err)
	}
	if enabled, _, _, _, _ := f.state(); enabled {
		t.Error("fake BMC NTP enabled; the test does not exercise a dropped re-enable")
	}
}

// TestReconcileTime_StepReenableReadsBackOnOtherServers: neither of the
// step's NTP writes applied, so NTP is still on, on the old server. That
// is reported, but not as NTP left disabled.
func TestReconcileTime_StepReenableReadsBackOnOtherServers(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.dropNTPEnable, f.dropNTPDisable = true, true
	f.ntpEnabled, f.ntpServers = true, []string{"10.1.1.99", ""}
	f.skew = time.Hour

	res := f.runShortSettle(testVIP)

	if res.NTPConfigured {
		t.Errorf("NTPConfigured = true for an enable that never applied")
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "still reports NTP enabled with servers [10.1.1.99]") {
		t.Fatalf("Err = %v, want what the BMC reports", res.Err)
	}
	if strings.Contains(res.Err.Error(), "DISABLED") {
		t.Errorf("Err = %v, but NTP reads back enabled", res.Err)
	}
}

// TestReconcileTime_StepReenableOutlivesCtx: ctx ends during the step;
// the re-enable and its read-back still run (detached), and wait for an
// async apply.
func TestReconcileTime_StepReenableOutlivesCtx(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag = true
	f.asyncNTPGets = 2
	f.ntpEnabled, f.ntpServers = true, []string{"10.1.1.99", ""}
	f.skew = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := f.reconciler()
	r.stepClock = func(c context.Context) error {
		cancel()
		return f.setSELTimeNow(c)
	}

	res := r.run(ctx, []string{testVIP})

	if !res.NTPConfigured {
		t.Errorf("result = %+v, want NTP configured by the detached re-enable", res)
	}
	if res.Err != nil && strings.Contains(res.Err.Error(), "NTP") {
		t.Errorf("Err = %v, want no NTP failure", res.Err)
	}
	if enabled, servers, _, _, _ := f.state(); !enabled || !slices.Equal(servers, []string{testVIP, ""}) {
		t.Errorf("BMC after = enabled %v servers %v", enabled, servers)
	}
}

// TestReconcileTime_AMIReadBackAcceptsServers: the ASUS that reports
// ProtocolEnabled=false while syncing reads back with the right servers
// but NTP "off"; on AMI that is configured. The same read-back on a
// Supermicro is NTP genuinely off and is reported.
func TestReconcileTime_AMIReadBackAcceptsServers(t *testing.T) {
	ami := newFakeRedfishBMC(t, "Self")
	ami.asyncNTPGets = 1
	ami.reportNTPOff = true
	ami.ntpServers = []string{"10.1.1.99", ""}
	if res := ami.runShortSettle(testVIP); res.Err != nil || !res.NTPConfigured {
		t.Errorf("AMI result = %+v, want NTP configured", res)
	}
	if _, servers, _, _, _ := ami.state(); !slices.Equal(servers, []string{testVIP, ""}) {
		t.Errorf("AMI servers after = %v; the read-back did not wait for the servers", servers)
	}

	smc := newFakeRedfishBMC(t, "1")
	smc.reportNTPOff = true
	smc.ntpServers = []string{"10.1.1.99", ""}
	res := smc.runShortSettle(testVIP)
	if res.NTPConfigured || res.Err == nil || !strings.Contains(res.Err.Error(), "still reports NTP disabled") {
		t.Errorf("Supermicro result = %+v, want NTP reported disabled", res)
	}
}

// TestReconcileTime_TaskExceptionWithEchoIsReported is nova13.pop2 on
// 0.3.1: the enable's Task ends in Exception, and the first read after the
// PATCH echoes the PATCHed values. The Task's outcome decides, and the
// error names it and what the BMC reports once the echo is past.
func TestReconcileTime_TaskExceptionWithEchoIsReported(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.echoNTP = true, true
	f.taskEnd, f.taskRunning = "Exception", 1
	f.taskMessages = []string{"NTP Update failed."}
	f.ntpServers = []string{testVIP}

	res := f.run(testVIP)

	if res.NTPConfigured {
		t.Error("NTPConfigured = true for an enable whose Task ended in Exception")
	}
	want := `configure NTP: the BMC accepted the NTP change but its task /redfish/v1/TaskService/Tasks/1 ended in Exception ("NTP Update failed."); ` +
		"it still reports NTP disabled with servers [" + testVIP + "]"
	if res.Err == nil || !strings.Contains(res.Err.Error(), want) {
		t.Fatalf("Err = %v, want it to contain %q", res.Err, want)
	}
}

// TestReconcileTime_EchoWithoutTaskIsNotConfirmed: a BMC that answers 202
// without a Task body, echoes the PATCH on the first read and then reverts
// is caught by requiring two consecutive compliant reads.
func TestReconcileTime_EchoWithoutTaskIsNotConfirmed(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.echoNTP, f.dropNTPEnable = true, true, true
	f.ntpServers = []string{testVIP}

	res := f.runShortSettle(testVIP)

	if res.NTPConfigured {
		t.Error("NTPConfigured = true on an echoed read")
	}
	want := "configure NTP: the BMC accepted the NTP change but still reports NTP disabled with servers [" + testVIP + "]"
	if res.Err == nil || !strings.Contains(res.Err.Error(), want) {
		t.Fatalf("Err = %v, want it to contain %q", res.Err, want)
	}
}

// TestReconcileTime_TaskCompletedIsConfirmed: the Task runs, then
// Completes and applies the change; the read-back confirms it.
func TestReconcileTime_TaskCompletedIsConfirmed(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.echoNTP = true, true
	f.taskEnd, f.taskRunning = "Completed", 2
	f.ntpServers = []string{"10.1.1.99", ""}

	res := f.run(testVIP)

	if res.Err != nil || !res.NTPConfigured {
		t.Fatalf("result = %+v, want NTP configured", res)
	}
	if !slices.Equal(f.events, []string{"/redfish/v1/TaskService/Tasks/1 Completed"}) {
		t.Errorf("events = %q, want the Task waited out", f.events)
	}
	if enabled, servers, _, _, _ := f.state(); !enabled || !slices.Equal(servers, []string{testVIP, ""}) {
		t.Errorf("BMC after = enabled %v servers %v", enabled, servers)
	}
}

// TestReconcileTime_GoneTaskFallsBackToReadBack: a Task that 404s has an
// unknown outcome, so the read-back decides: configured when the change
// applied, reported when it did not (past the first read's echo).
func TestReconcileTime_GoneTaskFallsBackToReadBack(t *testing.T) {
	applied := newFakeRedfishBMC(t, "1")
	applied.strictETag = true
	applied.taskEnd, applied.taskGone = "Completed", true
	applied.ntpServers = []string{testVIP}
	if res := applied.run(testVIP); res.Err != nil || !res.NTPConfigured {
		t.Errorf("applied: result = %+v, want NTP configured", res)
	}

	failed := newFakeRedfishBMC(t, "1")
	failed.strictETag, failed.echoNTP = true, true
	failed.taskEnd, failed.taskGone = "Exception", true
	failed.ntpServers = []string{testVIP}
	res := failed.runShortSettle(testVIP)
	if res.NTPConfigured || res.Err == nil || !strings.Contains(res.Err.Error(), "still reports NTP disabled with servers ["+testVIP+"]") {
		t.Errorf("failed: result = %+v, want NTP reported disabled", res)
	}
}

// TestReconcileTime_StepWaitsForDisableTask: the step's disable is a Task;
// the clock is stepped only once it Completed, so NTP is off for Set SEL
// Time and the disable cannot land after the re-enable.
func TestReconcileTime_StepWaitsForDisableTask(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.echoNTP, f.ignoreSetWhileNTP = true, true, true
	f.taskEnd, f.taskRunning = "Completed", 2
	f.ntpEnabled, f.ntpServers = true, []string{testVIP}
	f.skew = time.Hour

	res := f.run(testVIP)

	if res.Err != nil || !res.ClockStepped {
		t.Fatalf("result = %+v, want a successful step", res)
	}
	want := []string{"/redfish/v1/TaskService/Tasks/1 Completed", "step", "/redfish/v1/TaskService/Tasks/2 Completed"}
	if !slices.Equal(f.events, want) {
		t.Errorf("events = %q, want %q", f.events, want)
	}
	if !slices.Equal(f.setCalls, []bool{false}) {
		t.Errorf("Set SEL Time calls (NTP enabled at each) = %v, want one with NTP off", f.setCalls)
	}
}

// TestReconcileTime_TaskStillRunningIsReported: a Task that has not ended
// by settleTimeout is not a confirmed change.
func TestReconcileTime_TaskStillRunningIsReported(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.echoNTP = true, true
	f.taskEnd, f.taskRunning = "Completed", 1<<30
	f.ntpServers = []string{testVIP}

	res := f.runShortSettle(testVIP)

	if res.NTPConfigured {
		t.Error("NTPConfigured = true while the Task was still running")
	}
	for _, want := range []string{
		"its task /redfish/v1/TaskService/Tasks/1 was still running after " + shortSettle.String(),
		"it still reports NTP disabled with servers [" + testVIP + "]",
	} {
		if res.Err == nil || !strings.Contains(res.Err.Error(), want) {
			t.Errorf("Err = %v, want it to contain %q", res.Err, want)
		}
	}
}

// TestReconcileTime_StepWithFailingTasksIsLoud is nova13 with a wrong
// clock: NTP off, every NTP Task ends in Exception. The step still runs
// (NTP is off anyway); the failed disable is reported, and the failed
// re-enable says NTP is left DISABLED.
func TestReconcileTime_StepWithFailingTasksIsLoud(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.strictETag, f.echoNTP, f.ignoreSetWhileNTP = true, true, true
	f.taskEnd = "Exception"
	f.ntpServers = []string{testVIP}
	f.skew = time.Hour

	res := f.runShortSettle(testVIP)

	if !res.ClockStepped || res.NTPConfigured {
		t.Errorf("result = %+v, want stepped, NTP not configured", res)
	}
	for _, want := range []string{
		"disable NTP for the clock step: the BMC accepted it but its task /redfish/v1/TaskService/Tasks/1 ended in Exception; stepped anyway",
		"NTP is DISABLED on the BMC: the BMC accepted the NTP change but its task /redfish/v1/TaskService/Tasks/2 ended in Exception; " +
			"it still reports NTP disabled with servers [" + testVIP + "]",
	} {
		if res.Err == nil || !strings.Contains(res.Err.Error(), want) {
			t.Errorf("Err = %v, want it to contain %q", res.Err, want)
		}
	}
}

// TestTaskPath pins which PATCH response bodies name a Task to wait for.
func TestTaskPath(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"@odata.type":"#Task.v1_5_1.Task","@odata.id":"/redfish/v1/TaskService/Tasks/8","TaskState":"Running"}`, "/redfish/v1/TaskService/Tasks/8"},
		{``, ""},
		{`{"@Message.ExtendedInfo":[{"MessageId":"Base.1.8.Success"}]}`, ""},
		{`{"@odata.type":"#Task.v1_5_1.Task","TaskState":"Running"}`, ""},
		{`{"@odata.type":"#ManagerNetworkProtocol.v1_5_0.ManagerNetworkProtocol","@odata.id":"/redfish/v1/Managers/1/NetworkProtocol"}`, ""},
		// A link off this BMC would send the admin credential elsewhere.
		{`{"@odata.type":"#Task.v1_5_1.Task","@odata.id":"https://198.51.100.7/redfish/v1/TaskService/Tasks/8"}`, ""},
	}
	for _, tc := range cases {
		if got := taskPath([]byte(tc.body)); got != tc.want {
			t.Errorf("taskPath(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// TestReconcileTime_RetriesStaleETagOnce: a 412 (the ETag moved between
// the GET and the PATCH) is retried once with a fresh ETag.
func TestReconcileTime_RetriesStaleETagOnce(t *testing.T) {
	f := newFakeRedfishBMC(t, "Self")
	f.offset = "-05:00"
	f.ntpEnabled, f.ntpServers = true, []string{testVIP}
	f.stale412 = 1

	res := f.run(testVIP)

	if res.Err != nil || !res.TimezoneSet {
		t.Fatalf("result = %+v, want the timezone set on the retry", res)
	}
	if n := len(f.patchesTo(f.managerPath())); n != 2 {
		t.Errorf("manager PATCHes = %d, want 2 (412 then retry)", n)
	}
}

// TestReconcileTime_UnreachableRedfishIsReported: no Redfish means no time
// compliance; the error names the read that failed and nothing is stepped.
func TestReconcileTime_UnreachableRedfishIsReported(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.noManagers = true

	res := f.run(testVIP)

	if res.Err == nil || !strings.Contains(res.Err.Error(), "Managers") {
		t.Fatalf("Err = %v, want the manager discovery failure", res.Err)
	}
	if len(f.setCalls) != 0 || len(f.patchesTo("")) != 0 {
		t.Error("wrote to a BMC whose manager could not be read")
	}
}

// TestReconcileTime_RefusesOffBMCLink: resource links come from the BMC; an
// absolute URL would send the admin credential elsewhere, so it is refused
// before any request follows it.
func TestReconcileTime_RefusesOffBMCLink(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.memberLink = "https://198.51.100.7/redfish/v1/Managers/1"

	res := f.run(testVIP)

	if res.Err == nil || !strings.Contains(res.Err.Error(), "refusing Redfish link") {
		t.Fatalf("Err = %v, want the link refused", res.Err)
	}
	if f.otherHosts != 0 || f.requests != 1 {
		t.Errorf("requests = %d (other hosts %d), want only the Managers read", f.requests, f.otherHosts)
	}
}

// TestReconcileTime_ProductionWiring drives ReconcileTime (the default HTTP
// client: TLS to a self-signed BMC cert, basic auth) against a compliant
// BMC, so no ipmitool step runs.
func TestReconcileTime_ProductionWiring(t *testing.T) {
	f := newFakeRedfishBMC(t, "1")
	f.ntpEnabled, f.ntpServers = true, []string{testVIP}

	res := ReconcileTime(context.Background(), TimeTarget{
		RedfishAddr: f.addr(),
		LAN:         LANTarget{Host: "127.0.0.1", Username: fakeRedfishUser, Password: fakeRedfishPass},
		NTPServers:  []string{testVIP},
	})

	if res.Err != nil || res.NTPConfigured || res.ClockStepped || res.TimezoneSet {
		t.Errorf("result = %+v, want compliant", res)
	}
	if !slices.Equal(res.NTPServers, []string{testVIP}) {
		t.Errorf("NTPServers = %v", res.NTPServers)
	}
}

// TestSetSELTimeNow_CommandShape pins the step's ipmitool invocation: the
// LAN session of the account reconcile, `sel time set now`, the password
// only in IPMI_PASSWORD, and TZ=UTC (ipmitool shifts "now" by the host's
// UTC offset).
func TestSetSELTimeNow_CommandShape(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("sel time set now", "10/28/2026 21:15:00\n")
	opt := lanSession(LANTarget{Host: "10.253.240.57", Username: "ADMIN", Password: "s3cret!pw"})
	opt.IPMITool = fake.path

	if err := setSELTimeNow(context.Background(), opt); err != nil {
		t.Fatalf("setSELTimeNow: %v", err)
	}
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	want := "-I lanplus -H 10.253.240.57 -U ADMIN -C 17 -E sel time set now"
	if calls[0].Args != want || calls[0].Env != "s3cret!pw" || calls[0].TZ != "UTC" {
		t.Errorf("call = %+v, want args %q, IPMI_PASSWORD set, TZ=UTC", calls[0], want)
	}
	if strings.Contains(calls[0].Args, "s3cret!pw") {
		t.Error("password on argv")
	}
}

// TestNTPPatchServers pins the padding rule directly.
func TestNTPPatchServers(t *testing.T) {
	cases := []struct {
		current, want []string
	}{
		{nil, []string{testVIP}},
		{[]string{"10.1.1.99"}, []string{testVIP}},
		{[]string{"10.1.1.99", ""}, []string{testVIP}},
		{[]string{"", "", ""}, []string{testVIP}},
		{[]string{"10.1.1.99", "pool.ntp.org"}, []string{testVIP, ""}},
		{[]string{"", "", "10.1.1.99"}, []string{testVIP, "", ""}},
	}
	for _, tc := range cases {
		if got := ntpPatchServers([]string{testVIP}, tc.current); !slices.Equal(got, tc.want) {
			t.Errorf("ntpPatchServers(%q) = %q, want %q", tc.current, got, tc.want)
		}
	}
}
