package api

// Model-based stateful differential test of the operator API.
//
// The production router (NewServer: real auth middleware, real handlers, real
// Postgres store) is driven in-process with generated request histories over a
// small machine pool, alongside an independent in-memory reference model
// written from docs/dev/api.md and docs/dev/state-machine.md. After every step
// the full machine and operation state is read back through the API and
// compared with the model.
//
// Machines are created by the NetBox mirror, never over REST, so the pool is
// seeded through the store. The site worker and hub are not running: the
// history interleaves "environment" steps that perform what the hub would
// (claim, phase, complete, fail) and what the sweep ticker would (the four
// expiry sweeps, after backdating a timestamp), through the same store
// methods the hub and ticker call.
//
// This file runs the server WITHOUT NetBox (the configuration every unit test
// in this package uses): hostname resolution answers 503, BMC transport
// resolution yields an empty runtime, preflight is a pass. The NetBox-backed
// paths are covered in netbox_prop_test.go.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ---------------------------------------------------------------- model

type pmMachine struct {
	id        uuid.UUID
	serial    string
	state     model.MachineState
	locked    bool
	pxe       bool
	desiredOS string
	hasCD     bool // commission data stored
}

type pmOp struct {
	id        uuid.UUID
	mi        int
	typ       model.OperationType
	status    model.OperationStatus
	phase     string
	claimedBy string
	// Sweep clocks: true once the corresponding timestamp has been backdated
	// past its timeout.
	oldCreated, oldClaimed, oldPhase bool
	// rank orders ops by created_at: creation order, except that an op
	// whose created_at was backdated becomes the oldest.
	rank int
}

func (o *pmOp) active() bool {
	return o.status == model.OpPending || o.status == model.OpClaimed || o.status == model.OpRunning
}

type pmImage struct {
	id                      uuid.UUID
	name, version, sha, url string
	arch                    string
}

type pmScript struct {
	id       uuid.UUID
	name     string
	versions []pmScriptVersion // oldest first; last is head
}

type pmScriptVersion struct {
	priority int
	content  string
}

type pmSiteConfig struct {
	slug, netboxSite, location string
	poolStart, poolEnd         string
	promiscuous                bool
}

type pmWorker struct {
	id                   uuid.UUID
	slug, workerID, fqdn string
}

type pmKey struct {
	id      uuid.UUID
	key     string
	comment string
}

type pModel struct {
	machines []*pmMachine
	ops      []*pmOp // creation order
	hiRank   int
	// unlockedByFail counts failures that cleared a lock (coverage).
	unlockedByFail int
	keys           []*pmKey
	images         map[string]*pmImage
	scripts        map[string]map[string]*pmScript // kind ("storage"/"network") -> name
	sites          map[string]*pmSiteConfig
	workers        []*pmWorker
}

// newestFirst returns the ops in created_at-descending order, the order
// GET /operations documents.
func (m *pModel) newestFirst() []*pmOp {
	out := append([]*pmOp(nil), m.ops...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].rank > out[j].rank })
	return out
}

func (m *pModel) activeOp(mi int) *pmOp {
	for _, o := range m.ops {
		if o.mi == mi && o.active() {
			return o
		}
	}
	return nil
}

// fail applies docs/dev/state-machine.md "FailOperation branching" to one op.
// It reports whether the fail wrote a durable machine state.
func (m *pModel) fail(o *pmOp) (durable bool) {
	if !o.active() {
		return false
	}
	wasPending := o.status == model.OpPending
	o.status = model.OpFailed
	if wasPending { // rule 1
		return false
	}
	switch o.typ { // rule 2
	case model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle, model.OpSetBootDevice, model.OpBMCReconcile:
		return false
	case model.OpDeploy, model.OpCommission:
	}
	mach := m.machines[o.mi]
	if o.typ == model.OpCommission && !mach.hasCD { // rule 3
		mach.state = model.MachineStateNew
		return true
	}
	mach.state = model.MachineStateFailed // rule 4, auto-unlock
	if mach.locked {
		m.unlockedByFail++
	}
	mach.locked = false
	return true
}

func pmLifecycleSources(t model.OperationType) []model.MachineState {
	switch t {
	case model.OpDeploy:
		return []model.MachineState{model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning}
	case model.OpCommission:
		return []model.MachineState{model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning}
	default: // not a lifecycle operation
		return nil
	}
}

func pmStateIn(s model.MachineState, set []model.MachineState) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- run

type pmRun struct {
	t     *rapid.T
	env   *propEnv
	m     *pModel
	stats *propStats
	step  int
	trace []string
}

func (r *pmRun) logf(format string, args ...any) {
	r.trace = append(r.trace, fmt.Sprintf("%02d ", r.step)+fmt.Sprintf(format, args...))
}

func (r *pmRun) failf(format string, args ...any) {
	r.t.Helper()
	r.t.Fatalf("%s\n--- trace ---\n%s", fmt.Sprintf(format, args...), strings.Join(r.trace, "\n"))
}

// call performs a request, records it, and enforces the response rules that
// hold for every operator response in this configuration.
func (r *pmRun) call(what string, req propReq) propResp {
	r.t.Helper()
	resp := r.env.do(req)
	body := req.body
	if len(body) > 160 {
		body = body[:160] + "..."
	}
	r.logf("%s %s %s -> %s", req.method, req.path, body, resp)
	r.stats.add(fmt.Sprintf("%s -> %d", what, resp.code))
	if resp.code != http.StatusNoContent && len(resp.body) > 0 && !json.Valid(resp.body) {
		r.failf("%s: response body is not JSON: %s", what, resp)
	}
	return resp
}

func (r *pmRun) expect(what string, resp propResp, want ...int) {
	r.t.Helper()
	for _, w := range want {
		if resp.code == w {
			return
		}
	}
	r.failf("%s: status %d, model expects %v\n  response: %s", what, resp.code, want, resp)
}

// ---------------------------------------------------------------- machine refs

type pmRefKind int

const (
	pmRefOK      pmRefKind = iota // resolves to pool machine idx
	pmRefMissing                  // a well-formed UUID naming no machine
	pmRefBad                      // not a UUID and no '.': 400
	pmRefDotted                   // a hostname; 503 without NetBox
)

type pmRef struct {
	kind pmRefKind
	idx  int
	seg  string // the path segment sent
}

const pmMissingUUID = "00000000-0000-4000-8000-00000000dead"

func (r *pmRun) drawRef(label string) pmRef { return r.drawRefPrefer(label, nil) }

// drawRefPrefer draws a machine identifier. When prefer is given, most draws
// name a pool machine the model says satisfies it (state-aware generation:
// without it nearly every lifecycle request is refused and histories never
// get past the first op). The rest sample the whole identifier space, so
// every handler's refusal, resolve and not-found paths stay exercised.
func (r *pmRun) drawRefPrefer(label string, prefer func(mi int) bool) pmRef {
	if prefer != nil && rapid.IntRange(0, 9).Draw(r.t, label+"Smart") < 6 {
		var ok []int
		for i := range r.m.machines {
			if prefer(i) {
				ok = append(ok, i)
			}
		}
		if len(ok) > 0 {
			idx := rapid.SampledFrom(ok).Draw(r.t, label+"Eligible")
			return pmRef{pmRefOK, idx, r.m.machines[idx].id.String()}
		}
	}
	kind := rapid.SampledFrom([]string{
		"ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok", "ok",
		"upper", "nodash", "urn", "missing", "nil", "bad", "dotted",
	}).Draw(r.t, label+"Kind")
	idx := rapid.IntRange(0, len(r.m.machines)-1).Draw(r.t, label+"Idx")
	id := r.m.machines[idx].id.String()
	switch kind {
	case "ok":
		return pmRef{pmRefOK, idx, id}
	case "upper":
		return pmRef{pmRefOK, idx, strings.ToUpper(id)}
	case "nodash":
		return pmRef{pmRefOK, idx, strings.ReplaceAll(id, "-", "")}
	case "urn":
		return pmRef{pmRefOK, idx, "urn:uuid:" + id}
	case "missing":
		return pmRef{pmRefMissing, -1, pmMissingUUID}
	case "nil":
		return pmRef{pmRefMissing, -1, uuid.Nil.String()}
	case "bad":
		return pmRef{pmRefBad, -1, rapid.SampledFrom([]string{"not-a-uuid", "12345", "SN-0", "deadbeef"}).Draw(r.t, label+"Bad")}
	default:
		return pmRef{pmRefDotted, -1, rapid.SampledFrom([]string{"host-1.pop-a", "a.b", "..", "."}).Draw(r.t, label+"Dotted")}
	}
}

// resolveCode is the status resolveMachineParam gives before the handler
// looks the machine up (0 when it resolves to a UUID).
func (ref pmRef) resolveCode() int {
	switch ref.kind {
	case pmRefBad:
		return http.StatusBadRequest
	case pmRefDotted:
		return http.StatusServiceUnavailable
	default:
		return 0
	}
}

// ---------------------------------------------------------------- bodies

// pmBody is a generated request body with the fields the model needs.
type pmBody struct {
	raw   string
	ctype string
	shape string
	// parseable is false when the server cannot bind the body: a JSON
	// syntax error, a field of the wrong JSON type, a non-JSON content type.
	parseable bool
	// f holds the field values the body carries (the intended ones, when
	// the body is not parseable).
	f map[string]any
}

func (b pmBody) str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (b pmBody) boolean(m map[string]any, k string) bool {
	v, _ := m[k].(bool)
	return v
}

// drawBody builds a body from typed fields. fields maps each JSON key to its
// candidate values; wrongType maps keys to a value of the wrong JSON type.
func (r *pmRun) drawBody(label string, fields map[string][]any, wrongType map[string]any) pmBody {
	shape := rapid.SampledFrom([]string{
		"valid", "valid", "valid", "valid", "valid", "valid", "valid", "valid",
		"valid", "valid", "valid", "valid", "valid", "valid", "valid", "valid",
		"none", "none", "null", "syntax", "type", "ctype", "noctype", "array", "trailing",
	}).Draw(r.t, label+"Shape")

	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	set := map[string]any{}
	for _, k := range keys {
		if rapid.Bool().Draw(r.t, label+"Has_"+k) {
			set[k] = rapid.SampledFrom(fields[k]).Draw(r.t, label+"_"+k)
		}
	}
	// An unknown key is ignored by every handler that binds a struct.
	doc := map[string]any{}
	for k, v := range set {
		doc[k] = v
	}
	if rapid.Bool().Draw(r.t, label+"Unknown") {
		doc["pxe_enable"] = true
		doc["image"] = "noble-pxe"
	}
	enc := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			panic(err)
		}
		return string(raw)
	}

	switch shape {
	case "none":
		return pmBody{shape: shape, raw: "", parseable: true, f: map[string]any{}}
	case "null":
		return pmBody{shape: shape, raw: "null", parseable: true, f: map[string]any{}}
	case "syntax":
		raw := enc(doc)
		return pmBody{shape: shape, raw: raw[:len(raw)-1], parseable: false, f: set}
	case "trailing":
		return pmBody{shape: shape, raw: enc(doc) + "}", parseable: false, f: set}
	case "ctype":
		return pmBody{shape: shape, raw: enc(doc), ctype: "text/plain", parseable: false, f: set}
	case "noctype":
		return pmBody{shape: shape, raw: enc(doc), ctype: "-", parseable: false, f: set}
	case "array":
		return pmBody{shape: shape, raw: "[" + enc(doc) + "]", parseable: false, f: set}
	case "type":
		wkeys := make([]string, 0, len(wrongType))
		for k := range wrongType {
			wkeys = append(wkeys, k)
		}
		sort.Strings(wkeys)
		if len(wkeys) > 0 {
			k := rapid.SampledFrom(wkeys).Draw(r.t, label+"WrongKey")
			doc[k] = wrongType[k]
			return pmBody{shape: shape, raw: enc(doc), parseable: false, f: set}
		}
	}
	return pmBody{shape: "valid", raw: enc(doc), parseable: true, f: set}
}

// requireField fills in a required field most of the time when a valid body
// happens to omit it, so that complete requests are not rare.
func (r *pmRun) requireField(b pmBody, key string, value any) pmBody {
	if b.shape != "valid" || rapid.IntRange(0, 9).Draw(r.t, "fill_"+key) >= 7 {
		return b
	}
	if _, ok := b.f[key]; ok {
		return b
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(b.raw), &doc); err != nil {
		panic(err)
	}
	doc[key] = value
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	f := map[string]any{key: value}
	for k, v := range b.f {
		f[k] = v
	}
	b.raw, b.f = string(raw), f
	return b
}

// ---------------------------------------------------------------- op creation

func (r *pmRun) noteCreated(resp propResp, mi int, typ model.OperationType, what string) *pmOp {
	r.t.Helper()
	var op model.Operation
	if err := resp.json(&op); err != nil {
		r.failf("%s: 201 body does not decode as an operation: %v", what, err)
	}
	if op.ID == uuid.Nil || op.TraceID == uuid.Nil || op.TraceID == op.ID {
		r.failf("%s: created op has bad ids: id=%s trace=%s", what, op.ID, op.TraceID)
	}
	if op.Type != typ || op.Status != model.OpPending || op.MachineID != r.m.machines[mi].id {
		r.failf("%s: created op = {type %s status %s machine %s}, want {%s pending %s}",
			what, op.Type, op.Status, op.MachineID, typ, r.m.machines[mi].id)
	}
	r.m.hiRank++
	o := &pmOp{id: op.ID, mi: mi, typ: typ, status: model.OpPending, rank: r.m.hiRank}
	r.m.ops = append(r.m.ops, o)
	return o
}

// lifecycleCreate is the model of POST /machines/:id/{deploy,commission}.
func (r *pmRun) lifecycleCreate(typ model.OperationType) {
	ref := r.drawRefPrefer("m", func(mi int) bool {
		mach := r.m.machines[mi]
		return !mach.locked && mach.pxe && pmStateIn(mach.state, pmLifecycleSources(typ)) && r.m.activeOp(mi) == nil
	})
	fields := map[string][]any{"via": {"bmc", "bmc", "bmc", "bmc", "bmc", "bmc", "", "pdu", "amt", "ipmi"}}
	wrong := map[string]any{"via": 7}
	if typ == model.OpDeploy {
		fields["destructive"] = []any{true, true, false}
		fields["wipe_all_disks"] = []any{true, false, false}
		fields["recovery"] = []any{true, false}
		wrong["destructive"] = "yes"
		wrong["wipe_all_disks"] = 1
		wrong["recovery"] = "true"
	}
	b := r.drawBody("body", fields, wrong)
	what := string(typ)
	before := r.env.pub.deltaCount()
	resp := r.call(what, propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/" + what, body: b.raw, ctype: b.ctype})

	if !b.parseable {
		// docs/dev/api.md: request bodies are JSON. A body the server cannot
		// bind (a syntax error, a field of the wrong JSON type, a non-JSON
		// content type) is refused before anything else is looked at; it
		// must not become an operation without the flags the caller sent.
		r.expect(what+" (unparseable body)", resp, http.StatusBadRequest)
		r.noNewOp(what, before)
		return
	}
	eff := b.f
	want := func() int {
		switch b.str(eff, "via") {
		case "", "bmc", "pdu", "amt":
		default:
			return http.StatusBadRequest
		}
		if c := ref.resolveCode(); c != 0 {
			return c
		}
		if typ == model.OpDeploy {
			if len(r.m.keys) == 0 {
				return http.StatusFailedDependency
			}
			if b.boolean(eff, "wipe_all_disks") && !b.boolean(eff, "destructive") {
				return http.StatusBadRequest
			}
		}
		switch b.str(eff, "via") {
		case "pdu", "amt":
			return http.StatusServiceUnavailable // needs NetBox
		}
		if ref.kind == pmRefMissing {
			return http.StatusConflict // TestCreateOperation_UnknownMachine pins this
		}
		mach := r.m.machines[ref.idx]
		if mach.locked || !mach.pxe || !pmStateIn(mach.state, pmLifecycleSources(typ)) || r.m.activeOp(ref.idx) != nil {
			return http.StatusConflict
		}
		return http.StatusCreated
	}()
	r.expect(what, resp, want)
	if want != http.StatusCreated {
		r.noNewOp(what, before)
		return
	}
	r.noteCreated(resp, ref.idx, typ, what)
	r.oneNewOp(what, before)
	if typ == model.OpDeploy {
		var op model.Operation
		_ = resp.json(&op)
		var p model.OpParams
		if err := json.Unmarshal(op.Params, &p); err != nil {
			r.failf("deploy: params do not decode: %v (%s)", err, op.Params)
		}
		if p.Destructive != b.boolean(eff, "destructive") || p.WipeAllDisks != b.boolean(eff, "wipe_all_disks") || p.Recovery != b.boolean(eff, "recovery") {
			r.failf("deploy: op params {destructive %v wipe %v recovery %v} differ from the request %s",
				p.Destructive, p.WipeAllDisks, p.Recovery, b.raw)
		}
		if p.WipeAllDisks && !p.Destructive {
			r.failf("deploy: created an op with wipe_all_disks and without destructive: %s", op.Params)
		}
		r.stats.add(fmt.Sprintf("deploy created destructive=%v wipe=%v recovery=%v", p.Destructive, p.WipeAllDisks, p.Recovery))
	}
}

func (r *pmRun) noNewOp(what string, before int) {
	r.t.Helper()
	if got := r.env.pub.deltaCount(); got != before {
		r.failf("%s: a refused request published %d op_delta message(s)", what, got-before)
	}
}

func (r *pmRun) oneNewOp(what string, before int) {
	r.t.Helper()
	if got := r.env.pub.deltaCount(); got != before+1 {
		r.failf("%s: op create published %d op_delta message(s), want 1", what, got-before)
	}
}

func (r *pmRun) power() {
	ref := r.drawRefPrefer("m", func(mi int) bool { return r.m.activeOp(mi) == nil })
	b := r.drawBody("body", map[string][]any{
		"action":      {"on", "off", "cycle", "cycle", "on", "off", "cycle", "reboot", ""},
		"boot_device": {"pxe", "disk", "", "", "usb"},
		"via":         {"bmc", "bmc", "bmc", "bmc", "", "pdu", "amt", "ipmi"},
	}, map[string]any{"action": 1, "boot_device": true, "via": 3})
	b = r.requireField(b, "action", "cycle")
	before := r.env.pub.deltaCount()
	resp := r.call("power", propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/power", body: b.raw, ctype: b.ctype})
	action, bootDev, via := b.str(b.f, "action"), b.str(b.f, "boot_device"), b.str(b.f, "via")
	want := func() int {
		if c := ref.resolveCode(); c != 0 {
			return c
		}
		if !b.parseable {
			return http.StatusBadRequest
		}
		switch via {
		case "", "bmc", "pdu", "amt":
		default:
			return http.StatusBadRequest
		}
		switch action {
		case "on", "off", "cycle":
		default:
			return http.StatusBadRequest
		}
		switch bootDev {
		case "", "pxe", "disk":
		default:
			return http.StatusBadRequest
		}
		if ref.kind == pmRefMissing {
			return http.StatusNotFound
		}
		if via == "pdu" && bootDev != "" {
			return http.StatusBadRequest
		}
		if via == "amt" && bootDev == "disk" {
			return http.StatusBadRequest
		}
		if via == "pdu" || via == "amt" {
			return http.StatusServiceUnavailable
		}
		if r.m.activeOp(ref.idx) != nil {
			return http.StatusConflict
		}
		return http.StatusCreated
	}()
	r.expect("power", resp, want)
	if want != http.StatusCreated {
		r.noNewOp("power", before)
		return
	}
	typ := map[string]model.OperationType{"on": model.OpPowerOn, "off": model.OpPowerOff, "cycle": model.OpPowerCycle}[action]
	r.noteCreated(resp, ref.idx, typ, "power")
	r.oneNewOp("power", before)
}

func (r *pmRun) boot() {
	ref := r.drawRefPrefer("m", func(mi int) bool { return r.m.activeOp(mi) == nil })
	b := r.drawBody("body", map[string][]any{
		"device":     {"pxe", "disk", "default", "default", "pxe", "disk", "usb", ""},
		"persistent": {true, false},
	}, map[string]any{"device": 1, "persistent": "yes"})
	b = r.requireField(b, "device", "pxe")
	before := r.env.pub.deltaCount()
	resp := r.call("boot", propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/boot", body: b.raw, ctype: b.ctype})
	device, persistent := b.str(b.f, "device"), b.boolean(b.f, "persistent")
	want := func() int {
		if c := ref.resolveCode(); c != 0 {
			return c
		}
		if !b.parseable {
			return http.StatusBadRequest
		}
		switch device {
		case "pxe", "disk", "default":
		default:
			return http.StatusBadRequest
		}
		if device == "default" && persistent {
			return http.StatusBadRequest
		}
		if ref.kind == pmRefMissing {
			return http.StatusNotFound
		}
		if r.m.activeOp(ref.idx) != nil {
			return http.StatusConflict
		}
		return http.StatusCreated
	}()
	r.expect("boot", resp, want)
	if want != http.StatusCreated {
		r.noNewOp("boot", before)
		return
	}
	r.noteCreated(resp, ref.idx, model.OpSetBootDevice, "boot")
	r.oneNewOp("boot", before)
}

func (r *pmRun) bmcReconcile() {
	ref := r.drawRef("m")
	b := r.drawBody("body", map[string][]any{"rotate_monitor": {true, false}}, map[string]any{"rotate_monitor": "yes"})
	before := r.env.pub.deltaCount()
	resp := r.call("bmc-reconcile", propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/bmc-reconcile", body: b.raw, ctype: b.ctype})
	want := http.StatusUnprocessableEntity // docs/dev/api.md: 422 without NetBox
	switch {
	case !b.parseable:
		want = http.StatusBadRequest
	case ref.resolveCode() != 0:
		want = ref.resolveCode()
	case ref.kind == pmRefMissing:
		want = http.StatusNotFound
	}
	r.expect("bmc-reconcile", resp, want)
	r.noNewOp("bmc-reconcile", before)
}

// ---------------------------------------------------------------- machine verbs

func (r *pmRun) checkMachineBody(what string, resp propResp, mi int) {
	r.t.Helper()
	var got model.Machine
	if err := resp.json(&got); err != nil {
		r.failf("%s: body does not decode as a machine: %v", what, err)
	}
	r.compareMachine(what, got, r.m.machines[mi])
}

func (r *pmRun) compareMachine(what string, got model.Machine, want *pmMachine) {
	r.t.Helper()
	if got.ID != want.id || got.NetBoxSerial != want.serial || got.State != want.state ||
		got.Locked != want.locked || got.PXEEligible != want.pxe || got.DesiredOS != want.desiredOS ||
		(len(got.CommissionData) > 0) != want.hasCD {
		r.failf("%s: machine %s is {state %s locked %v pxe %v desired_os %q commission_data %v serial %s}, model has {state %s locked %v pxe %v desired_os %q commission_data %v serial %s}",
			what, want.id, got.State, got.Locked, got.PXEEligible, got.DesiredOS, len(got.CommissionData) > 0, got.NetBoxSerial,
			want.state, want.locked, want.pxe, want.desiredOS, want.hasCD, want.serial)
	}
}

func (r *pmRun) setLock() {
	ref := r.drawRefPrefer("m", func(mi int) bool { return r.m.machines[mi].locked })
	lock := rapid.Bool().Draw(r.t, "lock")
	if ref.kind == pmRefOK && r.m.machines[ref.idx].locked && rapid.IntRange(0, 9).Draw(r.t, "unlockBias") < 6 {
		lock = false // an auto-locked running host must be unlocked before a redeploy
	}
	verb := map[bool]string{true: "lock", false: "unlock"}[lock]
	resp := r.call(verb, propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/" + verb})
	switch {
	case ref.resolveCode() != 0:
		r.expect(verb, resp, ref.resolveCode())
	case ref.kind == pmRefMissing:
		r.expect(verb+" (unknown machine)", resp, http.StatusNotFound)
	default:
		r.expect(verb, resp, http.StatusOK)
		r.m.machines[ref.idx].locked = lock
		r.checkMachineBody(verb, resp, ref.idx)
	}
}

// quarantine locks a machine while a claimed lifecycle operation is driving
// it: the one way a machine is locked when that operation later fails,
// which is when the documented auto-unlock on -> failed is observable.
func (r *pmRun) quarantine() {
	var c []int
	for i, m := range r.m.machines {
		if o := r.m.activeOp(i); o != nil && o.status == model.OpClaimed && pmLifecycleSources(o.typ) != nil && !m.locked {
			c = append(c, i)
		}
	}
	if len(c) == 0 {
		return
	}
	mi := rapid.SampledFrom(c).Draw(r.t, "quarantined")
	resp := r.call("lock (mid-operation)", propReq{method: http.MethodPost, path: "/api/v1/machines/" + r.m.machines[mi].id.String() + "/lock"})
	r.expect("lock (mid-operation)", resp, http.StatusOK)
	r.m.machines[mi].locked = true
	r.checkMachineBody("lock (mid-operation)", resp, mi)
}

func (r *pmRun) setPXE() {
	ref := r.drawRefPrefer("m", func(mi int) bool { return !r.m.machines[mi].pxe })
	on := rapid.Bool().Draw(r.t, "eligible")
	if ref.kind == pmRefOK && !r.m.machines[ref.idx].pxe && rapid.IntRange(0, 9).Draw(r.t, "activateBias") < 6 {
		on = true
	}
	verb := map[bool]string{true: "activate", false: "deactivate"}[on]
	resp := r.call(verb, propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/" + verb})
	switch {
	case ref.resolveCode() != 0:
		r.expect(verb, resp, ref.resolveCode())
	case ref.kind == pmRefMissing:
		r.expect(verb+" (unknown machine)", resp, http.StatusNotFound)
	case r.m.machines[ref.idx].locked && on:
		// "Locked machines cannot be PXE-enabled" (docs/user/operations.md):
		// activate is refused and the machine is left as it was. Deactivate
		// stays available on a locked machine.
		r.expect(verb+" (locked machine)", resp, http.StatusConflict)
		r.stats.add("activate: refused on a locked machine")
	default:
		r.expect(verb, resp, http.StatusOK)
		r.m.machines[ref.idx].pxe = on
		r.checkMachineBody(verb, resp, ref.idx)
	}
}

func (r *pmRun) abort() {
	ref := r.drawRefPrefer("m", func(mi int) bool { return r.m.activeOp(mi) != nil })
	beforeRemoved := r.env.pub.removedCount()
	beforeInv := len(r.env.inv.seen())
	resp := r.call("abort", propReq{method: http.MethodPost, path: "/api/v1/machines/" + ref.seg + "/abort"})
	switch {
	case ref.resolveCode() != 0:
		r.expect("abort", resp, ref.resolveCode())
		return
	case ref.kind == pmRefMissing:
		// The contract is 404. Abort answers 500 for a machine that does not
		// exist (the store's not-found error is not mapped to a status), so
		// the exact code is not held here; nothing may be published either
		// way.
		r.expect("abort (unknown machine)", resp, http.StatusNotFound, http.StatusInternalServerError)
		if got := r.env.pub.removedCount(); got != beforeRemoved {
			r.failf("abort (unknown machine): published %d op_removed", got-beforeRemoved)
		}
		return
	}
	r.expect("abort", resp, http.StatusOK)
	failed, durable := 0, false
	for _, o := range r.m.ops {
		if o.mi == ref.idx && o.active() {
			r.stats.add(fmt.Sprintf("abort failed a %s %s op", o.status, o.typ))
			failed++
			if r.m.fail(o) {
				durable = true
			}
		}
	}
	if failed == 0 {
		r.stats.add("abort with no active op")
	}
	r.checkMachineBody("abort", resp, ref.idx)
	// Every op that left the active set must be announced to the workers,
	// and a durable machine-state write must trigger an identity rebuild.
	if got := r.env.pub.removedCount() - beforeRemoved; got != failed {
		r.failf("abort: published %d op_removed, want %d", got, failed)
	}
	gotInv := len(r.env.inv.seen()) - beforeInv
	wantInv := 0
	if durable {
		wantInv = len(r.m.sites)
	}
	if gotInv != wantInv {
		r.failf("abort: %d site invalidation(s), want %d (durable=%v, %d site configs)", gotInv, wantInv, durable, len(r.m.sites))
	}
}

func (r *pmRun) patchMachine() {
	ref := r.drawRef("m")
	b := r.drawBody("body", map[string][]any{
		"desired_os":   {"noble", "jammy", ""},
		"state":        {"running", "new", "bogus"},
		"locked":       {true, false},
		"pxe_eligible": {true, false},
		"hostname":     {"renamed.pop-a"},
	}, map[string]any{"desired_os": 5})
	resp := r.call("patch machine", propReq{method: http.MethodPatch, path: "/api/v1/machines/" + ref.seg, body: b.raw, ctype: b.ctype})
	if ref.resolveCode() != 0 {
		r.expect("patch machine", resp, ref.resolveCode())
		return
	}
	// The body is validated before the machine is looked up, so a malformed
	// request is a 400 whether or not the machine exists.
	//
	// The PATCH body is bound to a map, so a field of an unexpected JSON
	// type is not a bind error: a non-string desired_os is skipped.
	if !b.parseable && b.shape != "type" {
		r.expect("patch machine (unparseable body)", resp, http.StatusBadRequest)
		return
	}
	var doc map[string]any
	if b.raw != "" && b.raw != "null" {
		if err := json.Unmarshal([]byte(b.raw), &doc); err != nil {
			r.failf("harness: patch body %q should be an object: %v", b.raw, err)
		}
	}
	if _, has := doc["state"]; has {
		// docs/dev/api.md: "a body containing state is rejected with 400".
		r.expect("patch machine (state)", resp, http.StatusBadRequest)
		return
	}
	if ref.kind == pmRefMissing {
		r.expect("patch machine (unknown machine)", resp, http.StatusNotFound)
		return
	}
	r.expect("patch machine", resp, http.StatusOK)
	if v, ok := doc["desired_os"].(string); ok {
		r.m.machines[ref.idx].desiredOS = v
	}
	r.checkMachineBody("patch machine", resp, ref.idx)
}

func (r *pmRun) getMachine() {
	ref := r.drawRef("m")
	resp := r.call("get machine", propReq{method: http.MethodGet, path: "/api/v1/machines/" + ref.seg})
	switch {
	case ref.resolveCode() != 0:
		r.expect("get machine", resp, ref.resolveCode())
	case ref.kind == pmRefMissing:
		r.expect("get machine", resp, http.StatusNotFound)
	default:
		r.expect("get machine", resp, http.StatusOK)
		r.checkMachineBody("get machine", resp, ref.idx)
	}
}

func (r *pmRun) listMachines() {
	state := rapid.SampledFrom([]string{"", "", "new", "commissioning", "commissioned", "deploying", "running", "failed", "bogus", "NEW"}).Draw(r.t, "state")
	serial := rapid.SampledFrom([]string{"", "", r.m.machines[0].serial, r.m.machines[len(r.m.machines)-1].serial, "nope", "sn-0"}).Draw(r.t, "serial")
	hostname := rapid.SampledFrom([]string{"", "", "", "", "host"}).Draw(r.t, "hostname")
	q := []string{}
	if state != "" {
		q = append(q, "state="+state)
	}
	if serial != "" {
		q = append(q, "serial="+serial)
	}
	if hostname != "" {
		q = append(q, "hostname="+hostname)
	}
	path := "/api/v1/machines"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	resp := r.call("list machines", propReq{method: http.MethodGet, path: path})
	if state != "" && !model.MachineState(state).Valid() {
		r.expect("list machines", resp, http.StatusBadRequest)
		return
	}
	if hostname != "" {
		r.expect("list machines", resp, http.StatusServiceUnavailable)
		return
	}
	r.expect("list machines", resp, http.StatusOK)
	var got []model.Machine
	if err := resp.json(&got); err != nil {
		r.failf("list machines: %v", err)
	}
	want := map[uuid.UUID]*pmMachine{}
	for _, mach := range r.m.machines { // brute-force filter of the model
		if state != "" && string(mach.state) != state {
			continue
		}
		if serial != "" && mach.serial != serial {
			continue
		}
		want[mach.id] = mach
	}
	if len(got) != len(want) {
		r.failf("list machines %s: %d rows, model has %d", path, len(got), len(want))
	}
	for _, g := range got {
		w, ok := want[g.ID]
		if !ok {
			r.failf("list machines %s: unexpected or duplicate machine %s", path, g.ID)
		}
		delete(want, g.ID)
		r.compareMachine("list machines", g, w)
	}
	r.stats.add(fmt.Sprintf("list machines rows=%d filtered=%v", len(got), len(q) > 0))
}

// ---------------------------------------------------------------- operations reads

func (r *pmRun) checkOpList(what, path string, filter func(*pmOp) bool) {
	r.t.Helper()
	resp := r.call(what, propReq{method: http.MethodGet, path: path})
	r.expect(what, resp, http.StatusOK)
	var got []model.Operation
	if err := resp.json(&got); err != nil {
		r.failf("%s: %v", what, err)
	}
	var want []*pmOp
	for _, o := range r.m.newestFirst() {
		if filter == nil || filter(o) {
			want = append(want, o)
		}
	}
	if len(got) != len(want) {
		r.failf("%s: %d operations, model has %d", what, len(got), len(want))
	}
	for i, g := range got {
		w := want[i]
		if g.ID != w.id {
			r.failf("%s: position %d is op %s, model expects %s (created_at desc)", what, i, g.ID, w.id)
		}
		if g.Type != w.typ || g.Status != w.status || g.MachineID != r.m.machines[w.mi].id || g.Phase != w.phase || g.ClaimedBy != w.claimedBy {
			r.failf("%s: op %s is {%s %s phase %q claimed_by %q}, model has {%s %s phase %q claimed_by %q}",
				what, g.ID, g.Type, g.Status, g.Phase, g.ClaimedBy, w.typ, w.status, w.phase, w.claimedBy)
		}
		if g.MachineName != r.m.machines[w.mi].serial {
			r.failf("%s: op %s machine_name %q, want the serial %q (no NetBox)", what, g.ID, g.MachineName, r.m.machines[w.mi].serial)
		}
		terminal := w.status == model.OpCompleted || w.status == model.OpFailed
		if terminal != (g.CompletedAt != nil) {
			r.failf("%s: op %s status %s but completed_at set=%v", what, g.ID, g.Status, g.CompletedAt != nil)
		}
	}
}

func (r *pmRun) listOps() {
	kind := rapid.SampledFrom([]string{"all", "machine", "machine", "missing", "bad"}).Draw(r.t, "filter")
	switch kind {
	case "all":
		r.checkOpList("list ops", "/api/v1/operations", nil)
	case "machine":
		mi := rapid.IntRange(0, len(r.m.machines)-1).Draw(r.t, "mi")
		r.checkOpList("list ops by machine", "/api/v1/operations?machine="+r.m.machines[mi].id.String(),
			func(o *pmOp) bool { return o.mi == mi })
	case "missing":
		r.checkOpList("list ops by unknown machine", "/api/v1/operations?machine="+pmMissingUUID,
			func(*pmOp) bool { return false })
	default:
		resp := r.call("list ops bad filter", propReq{method: http.MethodGet, path: "/api/v1/operations?machine=nope"})
		r.expect("list ops bad filter", resp, http.StatusBadRequest)
	}
}

func (r *pmRun) getOp() {
	kind := rapid.SampledFrom([]string{"op", "op", "op", "missing", "bad"}).Draw(r.t, "kind")
	if kind == "op" && len(r.m.ops) == 0 {
		kind = "missing"
	}
	switch kind {
	case "op":
		o := r.m.ops[rapid.IntRange(0, len(r.m.ops)-1).Draw(r.t, "op")]
		resp := r.call("get op", propReq{method: http.MethodGet, path: "/api/v1/operations/" + o.id.String()})
		r.expect("get op", resp, http.StatusOK)
		var g model.Operation
		if err := resp.json(&g); err != nil {
			r.failf("get op: %v", err)
		}
		if g.ID != o.id || g.Type != o.typ || g.Status != o.status || g.Phase != o.phase {
			r.failf("get op %s: {%s %s %q}, model has {%s %s %q}", o.id, g.Type, g.Status, g.Phase, o.typ, o.status, o.phase)
		}
	case "missing":
		resp := r.call("get op", propReq{method: http.MethodGet, path: "/api/v1/operations/" + pmMissingUUID})
		r.expect("get op", resp, http.StatusNotFound)
	default:
		resp := r.call("get op", propReq{method: http.MethodGet, path: "/api/v1/operations/nope"})
		r.expect("get op", resp, http.StatusBadRequest)
	}
}

// ---------------------------------------------------------------- environment (hub + sweeps)

func (r *pmRun) pickOp(label string, pred func(*pmOp) bool) *pmOp {
	var c []*pmOp
	for _, o := range r.m.ops {
		if pred(o) {
			c = append(c, o)
		}
	}
	if len(c) == 0 {
		return nil
	}
	return c[rapid.IntRange(0, len(c)-1).Draw(r.t, label)]
}

func (r *pmRun) envClaim() {
	o := r.pickOp("claimOp", func(o *pmOp) bool { return o.active() })
	if o == nil {
		r.stats.add("env claim: nothing active")
		return
	}
	worker := rapid.SampledFrom([]string{"w1", "w1", "w2"}).Draw(r.t, "worker")
	got, err := r.env.st.ClaimOperation(o.id, worker)
	r.logf("env claim %s (%s %s) by %s -> op=%v err=%v", o.id, o.typ, o.status, worker, got != nil, err)
	mach := r.m.machines[o.mi]
	switch {
	case o.status == model.OpClaimed && o.claimedBy == worker && o.phase == "":
		// idempotent re-claim by the holder before any phase is recorded
		if err != nil || got == nil {
			r.failf("env claim: re-claim by the holder returned op=%v err=%v", got != nil, err)
		}
		r.stats.add("env claim: idempotent re-claim")
	case o.status != model.OpPending:
		if err != nil || got != nil {
			r.failf("env claim: claiming a %s op returned op=%v err=%v, want claim-lost", o.status, got != nil, err)
		}
		r.stats.add("env claim: lost (not pending)")
	default:
		src := pmLifecycleSources(o.typ)
		if src != nil && (!pmStateIn(mach.state, src) || mach.locked) {
			if err == nil {
				r.failf("env claim: %s op on a machine in state %s locked=%v was claimed", o.typ, mach.state, mach.locked)
			}
			r.stats.add("env claim: refused by state/lock gate")
			return
		}
		if err != nil || got == nil {
			r.failf("env claim: pending %s op on %s/locked=%v returned op=%v err=%v", o.typ, mach.state, mach.locked, got != nil, err)
		}
		o.status, o.claimedBy = model.OpClaimed, worker
		switch o.typ {
		case model.OpDeploy:
			mach.state = model.MachineStateDeploying
		case model.OpCommission:
			mach.state = model.MachineStateCommissioning
		default: // a claim moves machine state for lifecycle operations only
		}
		r.stats.add("env claim: " + string(o.typ))
	}
}

func (r *pmRun) envPhase() {
	o := r.pickOp("phaseOp", func(o *pmOp) bool { return o.status == model.OpClaimed })
	if o == nil {
		return
	}
	phase := rapid.SampledFrom([]string{"power_cycle", "pxe_booting", "partitioning", "awaiting_phone_home", "awaiting_phone_home"}).Draw(r.t, "phase")
	if _, err := r.env.st.SetOperationPhase(o.id, phase); err != nil {
		r.failf("env phase: %v", err)
	}
	r.logf("env phase %s -> %s", o.id, phase)
	if o.phase != phase {
		o.oldPhase = false // phase_entered_at is bumped on a real change
	}
	o.phase = phase
	r.stats.add("env phase: " + phase)
}

func (r *pmRun) envComplete() {
	o := r.pickOp("completeOp", func(o *pmOp) bool { return o.status == model.OpClaimed })
	if o == nil {
		return
	}
	mach := r.m.machines[o.mi]
	switch o.typ {
	case model.OpCommission:
		if _, _, err := r.env.st.CommitCommissionResult(o.id, mach.id, json.RawMessage(`{"system":{"manufacturer":"Prop","product_name":"P1"}}`)); err != nil {
			r.failf("env complete commission: %v", err)
		}
		mach.state, mach.hasCD = model.MachineStateCommissioned, true
	case model.OpDeploy:
		if _, err := r.env.st.CompleteOperationWithState(o.id, model.MachineStateRunning, nil); err != nil {
			r.failf("env complete deploy: %v", err)
		}
		mach.state, mach.locked = model.MachineStateRunning, true // auto-lock
	default:
		if _, err := r.env.st.CompleteOperationWithState(o.id, "", nil); err != nil {
			r.failf("env complete %s: %v", o.typ, err)
		}
	}
	o.status = model.OpCompleted
	r.logf("env complete %s (%s)", o.id, o.typ)
	r.stats.add("env complete: " + string(o.typ))
}

func (r *pmRun) envFail() {
	o := r.pickOp("failOp", func(o *pmOp) bool { return o.active() })
	if o == nil {
		return
	}
	r.logf("env fail %s (%s %s)", o.id, o.typ, o.status)
	r.stats.add(fmt.Sprintf("env fail: %s %s", o.status, o.typ))
	if err := r.env.st.FailOperation(o.id, []byte(`{"error":"agent reported failure"}`)); err != nil {
		r.failf("env fail: %v", err)
	}
	r.m.fail(o)
}

// envSweep backdates one timestamp on one op (as time passing would) and
// runs the sweep ticker's four expiry passes in the ticker's order.
func (r *pmRun) envSweep() {
	if o := r.pickOp("sweepOp", func(o *pmOp) bool { return o.active() }); o != nil {
		col := rapid.SampledFrom([]string{"none", "created_at", "claimed_at", "phase_entered_at"}).Draw(r.t, "backdate")
		if col != "none" {
			if err := r.env.st.DB().Exec("UPDATE operations SET "+col+" = "+col+" - interval '2 days' WHERE id = ? AND "+col+" IS NOT NULL", o.id).Error; err != nil {
				r.failf("env sweep: backdate: %v", err)
			}
			switch col {
			case "created_at":
				o.oldCreated = true
				o.rank -= 1 << 20 // two days older; the whole case runs in seconds
			case "claimed_at":
				o.oldClaimed = o.status != model.OpPending // NULL until claimed
			case "phase_entered_at":
				o.oldPhase = true
			}
			r.logf("env backdate %s.%s", o.id, col)
		}
	}
	sweep := func(name string, run func() (int, error), pred func(*pmOp) bool) {
		n, err := run()
		if err != nil {
			r.failf("env sweep %s: %v", name, err)
		}
		want := 0
		for _, o := range r.m.ops {
			if o.active() && pred(o) {
				want++
				r.stats.add(fmt.Sprintf("sweep %s failed a %s %s op", name, o.status, o.typ))
				r.m.fail(o)
			}
		}
		if n != want {
			r.failf("env sweep %s: expired %d op(s), model expects %d", name, n, want)
		}
	}
	st := r.env.st
	sweep("pending", func() (int, error) { o, e := st.ExpirePendingOperations(24 * time.Hour); return len(o), e },
		func(o *pmOp) bool { return o.status == model.OpPending && o.oldCreated })
	sweep("stuck", func() (int, error) { o, e := st.ExpireStuckOperations(30 * time.Minute); return len(o), e },
		func(o *pmOp) bool {
			return o.status != model.OpPending && o.oldClaimed && o.phase != "awaiting_phone_home"
		})
	sweep("orphaned_claim", func() (int, error) { o, e := st.ExpireOrphanedClaims(2 * time.Minute); return len(o), e },
		func(o *pmOp) bool { return o.status == model.OpClaimed && o.oldClaimed && o.phase == "" })
	sweep("phone_home", func() (int, error) { o, e := st.ExpirePhoneHomeOperations(30 * time.Minute); return len(o), e },
		func(o *pmOp) bool {
			return o.status != model.OpPending && o.phase == "awaiting_phone_home" && o.oldPhase
		})
	r.logf("env sweep done")
}

// ---------------------------------------------------------------- full-state check

// checkAll reads every machine and operation back through the API and
// compares them with the model, then checks the invariants that must hold
// whatever the model says.
func (r *pmRun) checkAll() {
	r.t.Helper()
	resp := r.env.get("/api/v1/machines")
	if resp.code != http.StatusOK {
		r.failf("state check: GET /machines -> %s", resp)
	}
	var machines []model.Machine
	if err := resp.json(&machines); err != nil {
		r.failf("state check: %v", err)
	}
	if len(machines) != len(r.m.machines) {
		r.failf("state check: %d machines, model has %d", len(machines), len(r.m.machines))
	}
	byID := map[uuid.UUID]model.Machine{}
	for _, g := range machines {
		byID[g.ID] = g
	}
	for _, w := range r.m.machines {
		g, ok := byID[w.id]
		if !ok {
			r.failf("state check: machine %s missing from GET /machines", w.id)
		}
		r.compareMachine("state check", g, w)
	}

	resp = r.env.get("/api/v1/operations")
	if resp.code != http.StatusOK {
		r.failf("state check: GET /operations -> %s", resp)
	}
	var ops []model.Operation
	if err := resp.json(&ops); err != nil {
		r.failf("state check: %v", err)
	}
	if len(ops) != len(r.m.ops) {
		r.failf("state check: %d operations, model has %d", len(ops), len(r.m.ops))
	}
	active := map[uuid.UUID][]model.Operation{}
	newest := r.m.newestFirst()
	for i, g := range ops {
		w := newest[i]
		if g.ID != w.id || g.Status != w.status || g.Type != w.typ || g.Phase != w.phase || g.ClaimedBy != w.claimedBy {
			r.failf("state check: op[%d] = {%s %s %s phase %q by %q}, model has {%s %s %s phase %q by %q}",
				i, g.ID, g.Type, g.Status, g.Phase, g.ClaimedBy, w.id, w.typ, w.status, w.phase, w.claimedBy)
		}
		if g.Status == model.OpPending || g.Status == model.OpClaimed || g.Status == model.OpRunning {
			active[g.MachineID] = append(active[g.MachineID], g)
		}
	}
	// One filtered list per step, rotating through every machine's serial
	// and state, against a brute-force filter of the model.
	if n := len(r.m.machines); n > 0 {
		k := r.step % (2 * n)
		ref := r.m.machines[k/2]
		path := "/api/v1/machines?serial=" + ref.serial
		keep := func(m *pmMachine) bool { return m.serial == ref.serial }
		if k%2 == 1 {
			path = "/api/v1/machines?state=" + string(ref.state)
			keep = func(m *pmMachine) bool { return m.state == ref.state }
		}
		resp := r.env.get(path)
		var rows []model.Machine
		if resp.code != http.StatusOK || resp.json(&rows) != nil {
			r.failf("state check: GET %s -> %s", path, resp)
		}
		want := map[uuid.UUID]bool{}
		for _, m := range r.m.machines {
			if keep(m) {
				want[m.id] = true
			}
		}
		if len(rows) != len(want) {
			r.failf("state check: GET %s returned %d machines, a brute-force filter of the model finds %d", path, len(rows), len(want))
		}
		for _, g := range rows {
			if !want[g.ID] {
				r.failf("state check: GET %s returned machine %s, which the filter excludes", path, g.ID)
			}
		}
	}

	// Invariants independent of the model.
	for id, a := range active {
		if len(a) > 1 {
			r.failf("invariant: machine %s has %d active operations", id, len(a))
		}
	}
	for _, g := range machines {
		var wantType model.OperationType
		switch g.State {
		case model.MachineStateDeploying:
			wantType = model.OpDeploy
		case model.MachineStateCommissioning:
			wantType = model.OpCommission
		default:
			continue
		}
		a := active[g.ID]
		if len(a) != 1 || a[0].Type != wantType || a[0].Status == model.OpPending {
			r.failf("invariant: machine %s is %s with no claimed %s operation driving it (active ops: %d); nothing in the API can move it on",
				g.ID, g.State, wantType, len(a))
		}
	}
	r.stats.add("machine states seen: " + pmStateSummary(machines))
}

func pmStateSummary(ms []model.Machine) string {
	seen := map[string]bool{}
	for _, m := range ms {
		seen[string(m.State)] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// ---------------------------------------------------------------- the property

type pmAction struct {
	name   string
	weight int
	run    func(*pmRun)
	// ready reports whether the model says the action can make progress
	// right now. Most steps are drawn from the ready actions so histories
	// walk the lifecycle (create, claim, complete, unlock, redeploy) instead
	// of piling up refusals; the rest are drawn from every enabled action.
	ready func(*pmRun) bool
}

func (r *pmRun) anyMachine(pred func(mi int, m *pmMachine) bool) bool {
	for i, m := range r.m.machines {
		if pred(i, m) {
			return true
		}
	}
	return false
}

func (r *pmRun) anyOp(pred func(*pmOp) bool) bool {
	for _, o := range r.m.ops {
		if pred(o) {
			return true
		}
	}
	return false
}

func (r *pmRun) lifecycleReady(typ model.OperationType) bool {
	if typ == model.OpDeploy && len(r.m.keys) == 0 {
		return false
	}
	return r.anyMachine(func(mi int, m *pmMachine) bool {
		return !m.locked && m.pxe && pmStateIn(m.state, pmLifecycleSources(typ)) && r.m.activeOp(mi) == nil
	})
}

func pmActions() []pmAction {
	return []pmAction{
		{"deploy", 10, func(r *pmRun) { r.lifecycleCreate(model.OpDeploy) },
			func(r *pmRun) bool { return r.lifecycleReady(model.OpDeploy) }},
		{"commission", 8, func(r *pmRun) { r.lifecycleCreate(model.OpCommission) },
			func(r *pmRun) bool { return r.lifecycleReady(model.OpCommission) }},
		{"power", 4, (*pmRun).power,
			func(r *pmRun) bool {
				return r.anyMachine(func(mi int, _ *pmMachine) bool { return r.m.activeOp(mi) == nil })
			}},
		{"boot", 2, (*pmRun).boot,
			func(r *pmRun) bool {
				return r.anyMachine(func(mi int, _ *pmMachine) bool { return r.m.activeOp(mi) == nil })
			}},
		{"bmc-reconcile", 1, (*pmRun).bmcReconcile, nil},
		{"abort", 4, (*pmRun).abort, func(r *pmRun) bool { return r.anyOp((*pmOp).active) }},
		{"lock", 4, (*pmRun).setLock, func(r *pmRun) bool { return r.anyMachine(func(_ int, m *pmMachine) bool { return m.locked }) }},
		{"quarantine", 2, (*pmRun).quarantine, func(r *pmRun) bool {
			return r.anyMachine(func(mi int, m *pmMachine) bool {
				o := r.m.activeOp(mi)
				return o != nil && o.status == model.OpClaimed && pmLifecycleSources(o.typ) != nil && !m.locked
			})
		}},
		{"pxe", 4, (*pmRun).setPXE, func(r *pmRun) bool { return r.anyMachine(func(_ int, m *pmMachine) bool { return !m.pxe }) }},
		{"patch", 3, (*pmRun).patchMachine, nil},
		{"get-machine", 2, (*pmRun).getMachine, nil},
		{"list-machines", 2, (*pmRun).listMachines, nil},
		{"list-ops", 2, (*pmRun).listOps, nil},
		{"get-op", 1, (*pmRun).getOp, nil},
		{"ssh-key", 3, (*pmRun).sshKey, func(r *pmRun) bool { return len(r.m.keys) == 0 }},
		{"image", 3, (*pmRun).image, nil},
		{"script", 4, (*pmRun).script, nil},
		{"site-config", 3, (*pmRun).siteConfig, nil},
		{"site-worker", 3, (*pmRun).siteWorker, nil},
		{"misc-read", 2, (*pmRun).miscRead, nil},
		{"env-claim", 10, (*pmRun).envClaim, func(r *pmRun) bool { return r.anyOp(func(o *pmOp) bool { return o.status == model.OpPending }) }},
		{"env-phase", 3, (*pmRun).envPhase, func(r *pmRun) bool { return r.anyOp(func(o *pmOp) bool { return o.status == model.OpClaimed }) }},
		{"env-complete", 8, (*pmRun).envComplete, func(r *pmRun) bool { return r.anyOp(func(o *pmOp) bool { return o.status == model.OpClaimed }) }},
		{"env-fail", 2, (*pmRun).envFail, func(r *pmRun) bool { return r.anyOp((*pmOp).active) }},
		{"env-sweep", 2, (*pmRun).envSweep, func(r *pmRun) bool { return r.anyOp((*pmOp).active) }},
	}
}

// TestProp_OperatorAPIModel walks the machine and operation lifecycle: most
// steps are guided toward actions that can make progress.
func TestProp_OperatorAPIModel(t *testing.T) {
	runOperatorAPIModel(t, pmActions(), 6, 30)
}

// TestProp_OperatorAPICRUD is the same machine with the record families
// (SSH keys, images, scripts, site configs, site workers) and the read routes
// in the foreground, unguided: those actions hardly interact with the
// lifecycle and are starved when lifecycle progress is favored.
func TestProp_OperatorAPICRUD(t *testing.T) {
	keep := map[string]int{
		"ssh-key": 4, "image": 5, "script": 8, "site-config": 5, "site-worker": 6, "misc-read": 3,
		"list-machines": 1, "list-ops": 1, "get-op": 1, "patch": 2,
		"deploy": 3, "commission": 2, "env-claim": 3, "env-complete": 1, "abort": 1,
	}
	var actions []pmAction
	for _, a := range pmActions() {
		if w, ok := keep[a.name]; ok {
			a.weight = w
			actions = append(actions, a)
		}
	}
	runOperatorAPIModel(t, actions, 0, 30)
}

// runOperatorAPIModel runs the model-based property over the given action
// table. guided is how many steps in ten are drawn from the actions whose
// ready predicate holds; checks is the default number of histories.
func runOperatorAPIModel(t *testing.T, actions []pmAction, guided, checks int) {
	stats := newPropStats()
	defer stats.report(t)

	propCheck(t, checks, func(rt *rapid.T) {
		env := newPropEnv(t, nil)
		r := &pmRun{t: rt, env: env, stats: stats, m: &pModel{
			images:  map[string]*pmImage{},
			scripts: map[string]map[string]*pmScript{"storage": {}, "network": {}},
			sites:   map[string]*pmSiteConfig{},
		}}

		// Initial state: a small pool in generated resting states.
		nm := rapid.IntRange(1, 3).Draw(rt, "machines")
		for i := range nm {
			state := rapid.SampledFrom([]model.MachineState{
				model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateCommissioned,
				model.MachineStateFailed, model.MachineStateRunning,
			}).Draw(rt, fmt.Sprintf("state%d", i))
			mach := &pmMachine{
				id:     uuid.New(),
				serial: fmt.Sprintf("SN-%d", i),
				state:  state,
				locked: state == model.MachineStateRunning && rapid.Bool().Draw(rt, fmt.Sprintf("locked%d", i)),
				pxe:    rapid.IntRange(0, 5).Draw(rt, fmt.Sprintf("pxe%d", i)) > 0,
				hasCD:  state != model.MachineStateNew && rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("cd%d", i)) > 0,
			}
			row := &model.Machine{ID: mach.id, NetBoxSerial: mach.serial, State: mach.state, Locked: mach.locked, PXEEligible: mach.pxe}
			if mach.hasCD {
				row.CommissionData = json.RawMessage(`{"system":{"manufacturer":"Seed","product_name":"S1"}}`)
			}
			if err := env.st.CreateMachine(row); err != nil {
				rt.Fatalf("seed machine: %v", err)
			}
			r.m.machines = append(r.m.machines, mach)
			r.logf("seed machine %d %s state=%s locked=%v pxe=%v cd=%v", i, mach.id, mach.state, mach.locked, mach.pxe, mach.hasCD)
		}
		if rapid.IntRange(0, 9).Draw(rt, "seedKey") > 0 { // deploy needs a key; keep the no-key refusal reachable
			r.addKey(0, "seed")
		}
		if rapid.Bool().Draw(rt, "seedSite") {
			r.createSite("pop-a", "10.0.0.10", "10.0.0.250")
		}

		// Swarm: each case enables a random subset of the actions, so some
		// histories are creation-heavy, some never abort, some never sweep.
		var enabled []pmAction
		for _, a := range actions {
			if rapid.IntRange(0, 4).Draw(rt, "on_"+a.name) > 0 {
				for range a.weight {
					enabled = append(enabled, a)
				}
			}
		}
		if len(enabled) == 0 {
			enabled = actions
		}
		if guided == 0 { // the record-family run keeps every action on
			enabled = enabled[:0]
			for _, a := range actions {
				for range a.weight {
					enabled = append(enabled, a)
				}
			}
		}
		steps := rapid.IntRange(1, 80).Draw(rt, "steps")
		for i := range steps {
			r.step = i
			var ready []pmAction
			for _, a := range enabled {
				if a.ready != nil && a.ready(r) {
					ready = append(ready, a)
				}
			}
			var a pmAction
			if len(ready) > 0 && rapid.IntRange(0, 9).Draw(rt, "guided") < guided {
				a = rapid.SampledFrom(ready).Draw(rt, "readyAction")
			} else {
				a = rapid.SampledFrom(enabled).Draw(rt, "action")
			}
			a.run(r)
			r.checkAll()
		}
		stats.add("cases")
		stats.add(fmt.Sprintf("ops per case (bucket) %2d+", (len(r.m.ops)/5)*5))
		if r.m.unlockedByFail > 0 {
			stats.add("cases where a failing operation cleared a lock")
		}
	})
}
