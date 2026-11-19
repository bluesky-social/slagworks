package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/tailscale"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Credential-surface property: drive the production router (NewServer), the
// real hub and the real store with a random history of operator requests,
// worker op transitions, secret rotations and dependency faults, while every
// credential the system resolves is a unique canary. After every step, no
// canary ever issued may appear in
//
//   - any operator HTTP response body (the one exception is the plaintext
//     credential endpoint, for a caller in a credential group);
//   - anything global logged (server, hub, store, NetBox client, slog
//     default);
//   - any log_envelopes row (the op events global emits);
//   - after a scrub, any terminal operation row.
//
// The reference model is the surfaceModel below: which canary each device
// holds right now, and every canary ever issued. It also checks the worker
// side of the contract: an active op's stored params carry exactly the
// credential current at creation, so the redaction is not hiding a missing
// or stale one.
//
// The site worker's bearer token is a canary too: a worker connects with it
// in the Authorization header, and a worker that still sends it in the
// upgrade URL's query is refused without the token reaching the request log.
//
// One device's NetBox name is not a hostname (it has spaces). The tailnet
// hostname a deploy op carries is derived from that name and later written
// to the kernel command line, so such a device's deploy must carry no
// tailnet enrollment at all, and every hostname that is carried must be a
// single command-line token.

// surfaceNetBox is a small NetBox stub with the endpoints credential
// resolution touches, and a switch that fails every request.
type surfaceNetBox struct {
	devices    map[int]netbox.Device
	interfaces []netbox.Interface
	ips        map[int]netbox.IPAddress
	ports      map[int][]netbox.PowerPort
	outlets    map[int][]netbox.PowerOutlet
	down       atomic.Bool
}

func (f *surfaceNetBox) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if f.down.Load() {
				http.Error(w, `{"detail":"netbox is down"}`, http.StatusInternalServerError)
				return
			}
			h(w, r)
		}
	}
	idFrom := func(path, prefix string) string {
		return strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/")
	}
	mux.HandleFunc("/api/dcim/devices/", guard(func(w http.ResponseWriter, r *http.Request) {
		if id := idFrom(r.URL.Path, "/api/dcim/devices/"); id != "" {
			n, _ := strconv.Atoi(id)
			d, ok := f.devices[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, d)
			return
		}
		q := r.URL.Query()
		var out []netbox.Device
		for _, d := range f.devices {
			if n := q.Get("name"); n != "" && d.Name != n {
				continue
			}
			if sn := q.Get("serial"); sn != "" && d.Serial != sn {
				continue
			}
			if ids := q["id"]; len(ids) > 0 {
				match := false
				for _, id := range ids {
					match = match || id == strconv.Itoa(d.ID)
				}
				if !match {
					continue
				}
			}
			out = append(out, d)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		writeJSONList(w, out)
	}))
	mux.HandleFunc("/api/dcim/interfaces/", guard(func(w http.ResponseWriter, r *http.Request) {
		ids := r.URL.Query()["device_id"]
		var out []netbox.Interface
		for _, ni := range f.interfaces {
			for _, id := range ids {
				if ni.Device != nil && strconv.Itoa(ni.Device.ID) == id {
					out = append(out, ni)
				}
			}
		}
		writeJSONList(w, out)
	}))
	mux.HandleFunc("/api/ipam/ip-addresses/", guard(func(w http.ResponseWriter, r *http.Request) {
		if id := idFrom(r.URL.Path, "/api/ipam/ip-addresses/"); id != "" {
			n, _ := strconv.Atoi(id)
			ip, ok := f.ips[n]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, ip)
			return
		}
		ids := r.URL.Query()["assigned_object_id"]
		var out []netbox.IPAddress
		for _, ip := range f.ips {
			for _, id := range ids {
				if ip.AssignedObjectID != nil && strconv.Itoa(*ip.AssignedObjectID) == id {
					out = append(out, ip)
				}
			}
		}
		writeJSONList(w, out)
	}))
	mux.HandleFunc("/api/dcim/power-ports/", guard(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("device_id"))
		writeJSONList(w, f.ports[n])
	}))
	mux.HandleFunc("/api/dcim/power-outlets/", guard(func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("device_id"))
		writeJSONList(w, f.outlets[n])
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// surfaceSecrets is the secrets backend double: admin and monitor entries
// the test rewrites to rotate credentials, and a switch that fails reads.
type surfaceSecrets struct {
	mu      sync.Mutex
	admin   map[string]awssecrets.AdminHost       // serial -> entry
	monitor map[string]awssecrets.BMCMonitorEntry // serial -> entry
	fail    bool
}

func (s *surfaceSecrets) GetAdmin(_ context.Context, pop string) (*awssecrets.AdminSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, errors.New("awssecrets: GetSecretValue pop/" + pop + "/bmc-creds-admin: operation error Secrets Manager: ThrottlingException")
	}
	out := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{}}
	for k, v := range s.admin {
		out.BySerial[k] = v
	}
	return out, nil
}

func (s *surfaceSecrets) GetMonitorEntry(_ context.Context, pop, serial string) (*awssecrets.BMCMonitorEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return nil, errors.New("GetSecretValue pop/" + pop + "/bmc-creds-monitor: operation error Secrets Manager: ThrottlingException")
	}
	if e, ok := s.monitor[serial]; ok {
		return &e, nil
	}
	return nil, nil
}

func (s *surfaceSecrets) AdminSecretExists(context.Context, string) (bool, error)   { return true, nil }
func (s *surfaceSecrets) MonitorSecretExists(context.Context, string) (bool, error) { return true, nil }

// surfaceMinter mints a fresh canary tailnet key per call.
type surfaceMinter struct {
	mu     sync.Mutex
	issue  func() string
	minted []string
}

func (m *surfaceMinter) Mint(context.Context, tailscale.KeyRequest) (tailscale.MintedKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.issue()
	m.minted = append(m.minted, k)
	return tailscale.MintedKey{Key: k, ID: "kid-" + strconv.Itoa(len(m.minted))}, nil
}

type surfaceDevice struct {
	name    string
	serial  string
	nbID    int
	kind    string
	machine uuid.UUID
}

// surfaceModel is the reference: the credential each device holds now, and
// every credential ever issued (all of which stay forbidden in operator
// output for the rest of the case).
type surfaceModel struct {
	seq     int
	issued  []string
	admin   map[string]string // serial -> current password
	monitor map[string]string // serial -> current stored monitor password
}

func (m *surfaceModel) fresh(label string) string {
	m.seq++
	// Characters JSON, HTML-escaping encoders, shells and SQL treat
	// specially ride along, so an escaped rendering is still caught by the
	// distinctive core.
	c := fmt.Sprintf("CNR%d%sQZ", m.seq, label)
	m.issued = append(m.issued, c)
	return c
}

type surfaceHarness struct {
	t       *testing.T
	st      *store.Store
	nb      *surfaceNetBox
	secrets *surfaceSecrets
	minter  *surfaceMinter
	logs    *surfaceLog
	srv     *httptest.Server
	devices []surfaceDevice
	pdus    []surfaceDevice
	token   string // site-worker bearer token
	stats   map[string]int
}

const (
	// surfaceBadName is a NetBox device name (free text) that would add
	// kernel parameters if it reached the deploy command line.
	surfaceBadName   = "srv-d slagworks.destructive x.popa"
	surfaceOpTok     = "op-tok"     // operator in a credential group
	surfaceNoCredTok = "nocred-tok" // operator, no credential group
)

func newSurfaceHarness(t *testing.T) *surfaceHarness {
	t.Helper()
	h := &surfaceHarness{t: t, logs: &surfaceLog{}, stats: map[string]int{}}
	h.st = store.New(testpg.DB(t, store.Migrate))
	log := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prev := slog.Default()
	slog.SetDefault(log)
	t.Cleanup(func() { slog.SetDefault(prev) })
	h.st.SetLogger(log)

	site := &netbox.NestedRef{ID: 1, Name: "popa", Slug: "popa"}
	h.devices = []surfaceDevice{
		{name: "srv-a.popa", serial: "SN-A", nbID: 101, kind: awssecrets.KindBMC},
		{name: "srv-b.popa", serial: "SN-B", nbID: 102, kind: awssecrets.KindBMC},
		{name: "nuc-c.popa", serial: "SN-C", nbID: 103, kind: awssecrets.KindAMT},
		{name: surfaceBadName, serial: "SN-D", nbID: 104, kind: awssecrets.KindBMC},
	}
	h.pdus = []surfaceDevice{
		{name: "pdu-1", serial: "SN-P1", nbID: 201, kind: awssecrets.KindPDU},
		{name: "pdu-2", serial: "SN-P2", nbID: 202, kind: awssecrets.KindPDU},
	}
	h.nb = &surfaceNetBox{
		devices: map[int]netbox.Device{},
		ips:     map[int]netbox.IPAddress{},
		ports:   map[int][]netbox.PowerPort{},
		outlets: map[int][]netbox.PowerOutlet{},
	}
	active := &netbox.NestedValue{Value: "active"}
	for _, d := range append(append([]surfaceDevice{}, h.devices...), h.pdus...) {
		dev := netbox.Device{ID: d.nbID, Name: d.name, Serial: d.serial, Site: site}
		if d.kind == awssecrets.KindAMT {
			dev.OOBIP = &netbox.NestedIPAddress{ID: 9100, Address: "10.9.0.23/24"}
			h.nb.ips[9100] = netbox.IPAddress{ID: 9100, Address: "10.9.0.23/24", Status: active}
		}
		h.nb.devices[d.nbID] = dev
	}
	for i, p := range h.pdus {
		ifID := 9001 + i
		h.nb.interfaces = append(h.nb.interfaces, netbox.Interface{ID: ifID, Name: "mgmt0", MgmtOnly: true, Device: &netbox.NestedRef{ID: p.nbID}})
		h.nb.ips[9200+i] = netbox.IPAddress{ID: 9200 + i, Address: fmt.Sprintf("10.9.1.%d/24", 10+i), Status: active,
			AssignedObjectType: "dcim.interface", AssignedObjectID: &ifID}
	}
	// srv-a: one feed on pdu-1 outlet 5. srv-b: feeds on pdu-1 outlet 6 and
	// pdu-2 outlet 6. nuc-c: no power-ports modeled.
	feed := func(dev surfaceDevice, pdu surfaceDevice, outlet, portID, outletID int) {
		name := "Outlet " + strconv.Itoa(outlet)
		h.nb.ports[dev.nbID] = append(h.nb.ports[dev.nbID], netbox.PowerPort{
			ID: portID, Name: "PSU" + strconv.Itoa(portID), Device: &netbox.NestedRef{ID: dev.nbID, Name: dev.name},
			ConnectedEndpoints:          []netbox.PowerOutlet{{ID: outletID, Name: name, Device: &netbox.NestedRef{ID: pdu.nbID, Name: pdu.name}}},
			ConnectedEndpointsReachable: true,
		})
		h.nb.outlets[pdu.nbID] = append(h.nb.outlets[pdu.nbID], netbox.PowerOutlet{
			ID: outletID, Name: name, Device: &netbox.NestedRef{ID: pdu.nbID, Name: pdu.name},
			ConnectedEndpoints:          []netbox.PowerPort{{ID: portID, Name: "PSU", Device: &netbox.NestedRef{ID: dev.nbID, Name: dev.name}}},
			ConnectedEndpointsReachable: true,
		})
	}
	feed(h.devices[0], h.pdus[0], 5, 6001, 5001)
	feed(h.devices[1], h.pdus[0], 6, 6002, 5002)
	feed(h.devices[1], h.pdus[1], 6, 6003, 5003)

	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: h.nb.server(t).URL, Token: "nb-token"})
	nb.SetLogger(log)

	h.secrets = &surfaceSecrets{}
	h.minter = &surfaceMinter{}
	snap := model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "popa"}}
	hub := ws.NewHub(ws.HubConfig{Auth: h.st, Rebuilder: staticRebuilder{snap}, Logger: log})
	hub.Push("popa", snap)
	s := NewServer(Config{
		RequiredGroups:   []string{"platform"},
		CredentialGroups: []string{"creds"},
		Store:            h.st, NetBox: nb, BMCSecrets: h.secrets, TailscaleMinter: h.minter,
		WSHub: hub, OpPublisher: hub,
		Verifier: &fakeVerifier{claims: map[string]*tokenverify.Claims{
			surfaceOpTok:     {Subject: "op", Groups: []string{"platform", "creds"}},
			surfaceNoCredTok: {Subject: "op2", Groups: []string{"platform"}},
		}},
	})
	s.SetLogger(log)
	h.srv = httptest.NewServer(s.Echo())
	t.Cleanup(h.srv.Close)
	return h
}

// reset puts the database and the doubles back to a case's initial state.
func (h *surfaceHarness) reset(rt *rapid.T, m *surfaceModel) {
	h.logs.reset()
	for _, tbl := range []string{"log_envelopes", "operations", "machines", "site_workers", "ssh_keys", "site_configs"} {
		if err := h.st.DB().Exec("DELETE FROM " + tbl).Error; err != nil {
			rt.Fatalf("reset %s: %v", tbl, err)
		}
	}
	if err := h.st.CreateSiteConfig(&model.SiteConfig{Slug: "popa", NetBoxSite: "popa",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250"}, nil); err != nil {
		rt.Fatalf("seed site_config: %v", err)
	}
	if err := h.st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
		rt.Fatalf("seed ssh key: %v", err)
	}
	seed := func(d *surfaceDevice) {
		d.machine = uuid.New()
		nbID := d.nbID
		if err := h.st.CreateMachine(&model.Machine{ID: d.machine, NetBoxSerial: d.serial, NetBoxDeviceID: &nbID,
			State: model.MachineStateCommissioned, PXEEligible: true}); err != nil {
			rt.Fatalf("seed machine: %v", err)
		}
	}
	for i := range h.devices {
		seed(&h.devices[i])
	}
	for i := range h.pdus {
		seed(&h.pdus[i])
	}
	_, tok, err := h.st.RegisterSiteWorker("popa", "worker-1", "w1.popa")
	if err != nil {
		rt.Fatalf("register worker: %v", err)
	}
	h.token = tok
	h.nb.down.Store(false)

	h.secrets.mu.Lock()
	h.secrets.fail = false
	h.secrets.admin = map[string]awssecrets.AdminHost{}
	h.secrets.monitor = map[string]awssecrets.BMCMonitorEntry{}
	h.secrets.mu.Unlock()
	for _, d := range append(append([]surfaceDevice{}, h.devices...), h.pdus...) {
		h.rotateAdmin(m, d)
	}
	for _, d := range h.devices[:2] {
		h.rotateMonitor(m, d)
	}
	h.minter.mu.Lock()
	h.minter.issue = func() string { return m.fresh("ts") }
	h.minter.minted = nil
	h.minter.mu.Unlock()
}

func (h *surfaceHarness) rotateAdmin(m *surfaceModel, d surfaceDevice) {
	pw := `p"\<` + m.fresh("adm") + `>&'`
	m.admin[d.serial] = pw
	h.secrets.mu.Lock()
	defer h.secrets.mu.Unlock()
	h.secrets.admin[d.serial] = awssecrets.AdminHost{Kind: d.kind, Username: "ADMIN", Password: pw,
		ExtraConfig: map[string]any{"ipmi_cipher": float64(17)}}
}

func (h *surfaceHarness) rotateMonitor(m *surfaceModel, d surfaceDevice) {
	pw := m.fresh("mon")
	m.monitor[d.serial] = pw
	h.secrets.mu.Lock()
	defer h.secrets.mu.Unlock()
	h.secrets.monitor[d.serial] = awssecrets.BMCMonitorEntry{Kind: awssecrets.KindBMC, Username: "monitor", Password: pw}
}

func (h *surfaceHarness) do(rt *rapid.T, token, method, path, body string) (int, []byte) {
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if err != nil {
		rt.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		rt.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		rt.Fatal(err)
	}
	h.stats[fmt.Sprintf("http_%dxx", resp.StatusCode/100)]++
	return resp.StatusCode, raw
}

// forbid fails if any issued canary (or, when checkToken, the worker's
// bearer token) appears in text.
func (h *surfaceHarness) forbid(rt *rapid.T, m *surfaceModel, where string, text []byte, checkToken bool) {
	for _, c := range m.issued {
		if bytes.Contains(text, []byte(c)) {
			rt.Fatalf("%s carries credential canary %s:\n%s", where, c, redactForReport(text, m.issued))
		}
	}
	if checkToken && h.token != "" && bytes.Contains(text, []byte(h.token)) {
		rt.Fatalf("%s carries the site worker's bearer token:\n%s", where, bytes.ReplaceAll(text, []byte(h.token), []byte("<WORKER-TOKEN>")))
	}
	h.stats["surface_checks"]++
}

// redactForReport keeps failure output free of everything but the canary
// names, trimming to the neighbourhood of the first hit.
func redactForReport(text []byte, canaries []string) []byte {
	for _, c := range canaries {
		if i := bytes.Index(text, []byte(c)); i >= 0 {
			lo, hi := max(0, i-400), min(len(text), i+400)
			return text[lo:hi]
		}
	}
	return text
}

// checkBackground scans everything that is not an HTTP response: the log
// and the op-event rows.
func (h *surfaceHarness) checkBackground(rt *rapid.T, m *surfaceModel, checkToken bool) {
	h.forbid(rt, m, "global's log", []byte(h.logs.String()), checkToken)
	var payloads []string
	if err := h.st.DB().Raw("SELECT payload::text FROM log_envelopes").Scan(&payloads).Error; err != nil {
		rt.Fatalf("read log_envelopes: %v", err)
	}
	h.stats["log_envelopes_scanned"] += len(payloads)
	h.forbid(rt, m, "log_envelopes", []byte(strings.Join(payloads, "\n")), checkToken)
}

// checkCreated verifies the worker-facing half for a freshly created op: the
// stored params carry exactly the credentials current right now.
func (h *surfaceHarness) checkCreated(rt *rapid.T, m *surfaceModel, d surfaceDevice, body []byte) uuid.UUID {
	var created model.Operation
	if err := json.Unmarshal(body, &created); err != nil {
		rt.Fatalf("decode created op: %v: %s", err, body)
	}
	stored, err := h.st.GetOperation(created.ID)
	if err != nil {
		rt.Fatalf("get created op: %v", err)
	}
	p, err := stored.DecodeParams()
	if err != nil {
		rt.Fatalf("stored params do not decode: %v", err)
	}
	shown, err := created.DecodeParams()
	if err != nil {
		rt.Fatalf("response params do not decode: %v", err)
	}
	n := 0
	if p.BMCRuntime != nil {
		n++
		if p.BMCRuntime.Password != m.admin[d.serial] {
			rt.Fatalf("op %s carries a BMC password that is not the device's current one (stale or wrong device)", created.Type)
		}
		if shown.BMCRuntime == nil || shown.BMCRuntime.Password != model.RedactedMarker ||
			shown.BMCRuntime.Username != "ADMIN" || shown.BMCRuntime.ExpectedChassisSerial != d.serial {
			rt.Fatalf("operator view of bmc_runtime is wrong: %s", body)
		}
	}
	if p.AMTRuntime != nil {
		n++
		if p.AMTRuntime.Password != m.admin[d.serial] {
			rt.Fatalf("op %s carries an AMT password that is not the device's current one", created.Type)
		}
		if shown.AMTRuntime == nil || shown.AMTRuntime.Password != model.RedactedMarker || shown.AMTRuntime.Host != "10.9.0.23" {
			rt.Fatalf("operator view of amt_runtime is wrong: %s", body)
		}
	}
	if p.PDURuntime != nil {
		n++
		if len(p.PDURuntime.Outlets) == 0 {
			rt.Fatalf("pdu op has no outlets: %s", body)
		}
		for i, o := range p.PDURuntime.Outlets {
			if o.Password != m.admin[o.Serial] {
				rt.Fatalf("pdu outlet %d carries a password that is not PDU %s's current one", i, o.Serial)
			}
			if shown.PDURuntime == nil || len(shown.PDURuntime.Outlets) != len(p.PDURuntime.Outlets) ||
				shown.PDURuntime.Outlets[i].Password != model.RedactedMarker ||
				shown.PDURuntime.Outlets[i].PDUHost != o.PDUHost || shown.PDURuntime.Outlets[i].Outlet != o.Outlet {
				rt.Fatalf("operator view of pdu_runtime is wrong: %s", body)
			}
		}
	}
	if n != 1 {
		rt.Fatalf("op carries %d transport runtime blocks, want exactly 1: %s", n, body)
	}
	if p.MonitorRuntime != nil {
		if p.MonitorRuntime.Password != m.monitor[d.serial] {
			rt.Fatalf("bmc_reconcile op carries a monitor password that is not the stored one")
		}
		if shown.MonitorRuntime == nil || shown.MonitorRuntime.Password != model.RedactedMarker {
			rt.Fatalf("operator view of monitor_runtime is wrong: %s", body)
		}
		h.stats["ops_with_monitor_runtime"]++
	}
	if p.TailscaleRuntime != nil {
		h.minter.mu.Lock()
		last := h.minter.minted[len(h.minter.minted)-1]
		h.minter.mu.Unlock()
		if p.TailscaleRuntime.AuthKey != last {
			rt.Fatalf("deploy op carries a tailnet key that is not the one just minted")
		}
		if shown.TailscaleRuntime == nil || shown.TailscaleRuntime.AuthKey != model.RedactedMarker {
			rt.Fatalf("operator view of tailscale_runtime is wrong: %s", body)
		}
		if host := p.TailscaleRuntime.Hostname; d.name == surfaceBadName || host == "" || strings.ContainsAny(host, " \t\r\n${}\"'\\") {
			rt.Fatalf("device %q got the tailnet hostname %q, which is not a single kernel command-line token", d.name, host)
		}
		h.stats["ops_with_tailscale_runtime"]++
	} else if d.name == surfaceBadName && created.Type == model.OpDeploy {
		h.stats["deploys_without_enrollment_for_bad_name"]++
	}
	h.stats["ops_created"]++
	h.stats["ops_created_"+string(created.Type)]++
	return created.ID
}

func TestProp_CredentialSurface(t *testing.T) {
	h := newSurfaceHarness(t)
	defer func() {
		keys := make([]string, 0, len(h.stats))
		for k := range h.stats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%d", k, h.stats[k])
		}
		t.Logf("coverage:%s", b.String())
		// The checks above pass on an empty history, so a run of useful
		// size that never created an op, reached a terminal row or served a
		// credential has stopped testing anything.
		if t.Failed() || h.stats["cases"] < 50 {
			return
		}
		for _, k := range []string{"ops_created", "ops_with_tailscale_runtime", "completes", "fails",
			"terminal_rows_scanned", "creds_served", "worker_connects", "log_envelopes_scanned"} {
			if h.stats[k] == 0 {
				t.Errorf("%d histories never reached %q; the generator or the harness no longer exercises it", h.stats["cases"], k)
			}
		}
	}()

	rapid.Check(t, func(rt *rapid.T) {
		m := &surfaceModel{admin: map[string]string{}, monitor: map[string]string{}}
		h.reset(rt, m)
		h.stats["cases"]++
		var ops []uuid.UUID
		all := append(append([]surfaceDevice{}, h.devices...), h.pdus...)

		// Step kinds are weighted so histories reach terminal ops and the
		// scrub: creations, then transitions, reads and the scrub, with
		// rotations and one-step dependency faults in between.
		weighted := []int{0, 0, 1, 2, 2, 3, 4, 5, 5, 6, 7, 8, 8, 8, 8, 9, 9, 10, 11, 12, 13, 14, 14, 15, 16, 17, 18}
		nbDownFor, secretsDownFor := 0, 0
		// Every step costs several database round trips, so histories are
		// kept short; -rapid.checks buys depth through more of them.
		steps := rapid.IntRange(1, 20).Draw(rt, "steps")
		for range steps {
			if nbDownFor > 0 {
				nbDownFor--
			}
			h.nb.down.Store(nbDownFor > 0)
			if secretsDownFor > 0 {
				secretsDownFor--
			}
			h.secrets.mu.Lock()
			h.secrets.fail = secretsDownFor > 0
			h.secrets.mu.Unlock()

			d := h.devices[rapid.IntRange(0, len(h.devices)-1).Draw(rt, "device")]
			via := rapid.SampledFrom([]string{"", "", "bmc", "pdu", "amt", "serial"}).Draw(rt, "via")
			mpath := "/api/v1/machines/" + d.machine.String()
			kind := rapid.SampledFrom(weighted).Draw(rt, "step")
			h.stats[fmt.Sprintf("step_%02d", kind)]++
			post := func(path, body string) {
				status, resp := h.do(rt, surfaceOpTok, http.MethodPost, path, body)
				h.forbid(rt, m, "POST "+path+" response", resp, true)
				if status == http.StatusCreated {
					ops = append(ops, h.checkCreated(rt, m, d, resp))
				}
			}
			switch kind {
			case 0, 1:
				action := rapid.SampledFrom([]string{"on", "off", "cycle", "reset"}).Draw(rt, "action")
				boot := rapid.SampledFrom([]string{"", "", "pxe", "disk"}).Draw(rt, "boot")
				post(mpath+"/power", fmt.Sprintf(`{"action":%q,"via":%q,"boot_device":%q}`, action, via, boot))
			case 2:
				post(mpath+"/deploy", fmt.Sprintf(`{"destructive":%t,"via":%q}`, rapid.Bool().Draw(rt, "destructive"), via))
			case 3:
				post(mpath+"/commission", fmt.Sprintf(`{"via":%q}`, via))
			case 4:
				post(mpath+"/boot", fmt.Sprintf(`{"device":%q}`, rapid.SampledFrom([]string{"pxe", "disk", "default", "usb"}).Draw(rt, "bootdev")))
			case 5:
				post(mpath+"/bmc-reconcile", fmt.Sprintf(`{"rotate_monitor":%t}`, rapid.Bool().Draw(rt, "rotate")))
			case 6:
				status, resp := h.do(rt, surfaceOpTok, http.MethodPost, mpath+"/abort", "")
				h.forbid(rt, m, "abort response", resp, true)
				if status == http.StatusOK {
					h.stats["aborts"]++
				}
			case 7:
				// PDU outlet power: resolve, then post with the token.
				pdu := h.pdus[rapid.IntRange(0, len(h.pdus)-1).Draw(rt, "pdu")]
				outlet := rapid.SampledFrom([]int{5, 6, 7}).Draw(rt, "outlet")
				verb := rapid.SampledFrom([]string{"cycle", "on", "off"}).Draw(rt, "verb")
				base := fmt.Sprintf("/api/v1/pdus/%s/outlet/%d/power", pdu.name, outlet)
				status, resp := h.do(rt, surfaceOpTok, http.MethodGet, base+"/resolve?verb="+verb, "")
				h.forbid(rt, m, "outlet resolve response", resp, true)
				if status == http.StatusOK {
					var res struct {
						PreflightToken string `json:"preflight_token"`
					}
					_ = json.Unmarshal(resp, &res)
					status, resp = h.do(rt, surfaceOpTok, http.MethodPost, base, fmt.Sprintf(`{"verb":%q,"preflight_token":%q}`, verb, res.PreflightToken))
					h.forbid(rt, m, "outlet power response", resp, true)
					if status == http.StatusCreated {
						ops = append(ops, h.checkCreated(rt, m, pdu, resp))
						h.stats["outlet_ops_created"]++
					}
				}
			case 8:
				// Worker transition on a known op.
				if len(ops) == 0 {
					break
				}
				id := ops[rapid.IntRange(0, len(ops)-1).Draw(rt, "op")]
				switch rapid.IntRange(0, 2).Draw(rt, "transition") {
				case 0:
					if _, err := h.st.ClaimOperation(id, "worker-1"); err == nil {
						h.stats["claims"]++
					}
				case 1:
					if err := h.st.CompleteOperation(id, nil); err == nil {
						h.stats["completes"]++
					}
				default:
					res := model.OpResult{Error: "bmc 10.9.0.5 power cycle: connection timed out"}.Marshal()
					if err := h.st.FailOperation(id, res); err == nil {
						h.stats["fails"]++
					}
				}
			case 9:
				// The sweep's scrub, then no terminal row holds a canary.
				if _, err := h.st.ScrubTerminalOperationCredentials(); err != nil {
					rt.Fatalf("scrub: %v", err)
				}
				var rows []string
				if err := h.st.DB().Raw("SELECT params::text FROM operations WHERE status IN ('completed','failed') AND params IS NOT NULL").Scan(&rows).Error; err != nil {
					rt.Fatal(err)
				}
				h.stats["terminal_rows_scanned"] += len(rows)
				h.forbid(rt, m, "terminal operation rows after the scrub", []byte(strings.Join(rows, "\n")), true)
			case 10:
				x := all[rapid.IntRange(0, len(all)-1).Draw(rt, "rotate")]
				h.rotateAdmin(m, x)
				h.stats["admin_rotations"]++
			case 11:
				h.rotateMonitor(m, h.devices[rapid.IntRange(0, 1).Draw(rt, "rotateMon")])
				h.stats["monitor_rotations"]++
			case 12:
				nbDownFor = 2 // this step's background check, then one request step
				h.stats["netbox_faults"]++
			case 13:
				secretsDownFor = 2
				h.stats["secret_faults"]++
			case 14:
				// Operator reads of ops.
				paths := []string{"/api/v1/operations", "/api/v1/operations?machine=" + d.machine.String(), "/api/v1/machines", mpath,
					mpath + "/power", mpath + "/boot", mpath + "/preflight"}
				for _, id := range ops {
					paths = append(paths, "/api/v1/operations/"+id.String())
				}
				for _, p := range paths {
					_, resp := h.do(rt, surfaceOpTok, http.MethodGet, p, "")
					h.forbid(rt, m, "GET "+p+" response", resp, true)
				}
				h.stats["read_sweeps"]++
			case 15:
				// The plaintext credential endpoint, without the group.
				x := all[rapid.IntRange(0, len(all)-1).Draw(rt, "credsDevice")]
				ident := rapid.SampledFrom([]string{x.name, x.serial, strconv.Itoa(x.nbID)}).Draw(rt, "ident")
				status, resp := h.do(rt, surfaceNoCredTok, http.MethodGet, "/api/v1/devices/"+url.PathEscape(ident)+"/creds", "")
				if status != http.StatusForbidden {
					rt.Fatalf("credential endpoint answered %d to an operator outside every credential group", status)
				}
				h.forbid(rt, m, "refused credential read", resp, true)
				h.stats["creds_refused"]++
			case 16:
				// The plaintext credential endpoint, with the group: the
				// current credential, and only that device's.
				x := all[rapid.IntRange(0, len(all)-1).Draw(rt, "credsDevice")]
				status, resp := h.do(rt, surfaceOpTok, http.MethodGet, "/api/v1/devices/"+x.serial+"/creds", "")
				if status == http.StatusOK {
					var got DeviceCreds
					if err := json.Unmarshal(resp, &got); err != nil {
						rt.Fatal(err)
					}
					if got.Password != m.admin[x.serial] || got.Serial != x.serial {
						rt.Fatalf("credential endpoint returned a credential that is not %s's current one", x.serial)
					}
					for _, c := range m.issued {
						if strings.Contains(m.admin[x.serial], c) {
							continue
						}
						if bytes.Contains(resp, []byte(c)) {
							rt.Fatalf("credential endpoint for %s also carries another credential (%s)", x.serial, c)
						}
					}
					h.stats["creds_served"]++
				} else {
					h.forbid(rt, m, "failed credential read", resp, true)
				}
			case 17:
				// Malformed and unauthenticated requests.
				_, resp := h.do(rt, "bogus-token", http.MethodGet, "/api/v1/operations", "")
				h.forbid(rt, m, "unauthenticated response", resp, true)
				_, resp = h.do(rt, surfaceOpTok, http.MethodPost, mpath+"/power", `{"action":`)
				h.forbid(rt, m, "malformed body response", resp, true)
				_, resp = h.do(rt, surfaceOpTok, http.MethodGet, "/api/v1/operations/not-a-uuid", "")
				h.forbid(rt, m, "bad id response", resp, true)
			case 18:
				// A site worker connects and disconnects: first the way an
				// old build did, token in the upgrade URL (refused), then
				// with the Authorization header.
				h.logs.mark()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				hubURL := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/v1/ws/site"
				if legacy, err := ws.Dial(ctx, hubURL+"?token="+h.token, nil); err == nil {
					_ = legacy.CloseNow()
					cancel()
					rt.Fatalf("the hub accepted a worker token from the upgrade URL")
				}
				conn, err := ws.Dial(ctx, hubURL, &websocket.DialOptions{
					HTTPHeader: http.Header{"Authorization": {"Bearer " + h.token}}})
				if err != nil {
					cancel()
					rt.Fatalf("worker dial with its bearer token: %v", err)
				}
				_ = conn.WriteTyped(ctx, ws.TypeHello, "", ws.HelloMsg{WorkerID: "worker-1", LastSeqNum: 1, Proto: ws.OpStreamProto})
				_, _ = conn.ReadMessage(ctx)
				_ = conn.CloseNow()
				cancel()
				h.stats["worker_connects"]++
				// Both request log lines are written when their handlers
				// return; wait for them so the check below sees them.
				deadline := time.Now().Add(5 * time.Second)
				for h.logs.countSinceMark("/api/v1/ws/site") < 2 {
					if time.Now().After(deadline) {
						rt.Fatalf("no request log lines for the two worker upgrades within 5s")
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			h.checkBackground(rt, m, true)
		}

		// End of history: scrub, and nothing terminal holds a credential.
		if _, err := h.st.ScrubTerminalOperationCredentials(); err != nil {
			rt.Fatalf("final scrub: %v", err)
		}
		var rows []string
		if err := h.st.DB().Raw("SELECT params::text FROM operations WHERE status IN ('completed','failed') AND params IS NOT NULL").Scan(&rows).Error; err != nil {
			rt.Fatal(err)
		}
		h.stats["terminal_rows_scanned"] += len(rows)
		h.forbid(rt, m, "terminal operation rows after the final scrub", []byte(strings.Join(rows, "\n")), true)
		h.stats["canaries_issued"] += len(m.issued)
		h.logs.reset()
	})
}

// surfaceLog is a goroutine-safe log sink.
type surfaceLog struct {
	mu sync.Mutex
	b  bytes.Buffer
	at int
}

func (l *surfaceLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *surfaceLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.Reset()
	l.at = 0
}

// mark remembers the current end of the log for countSinceMark.
func (l *surfaceLog) mark() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at = l.b.Len()
}

// countSinceMark counts the lines written since mark that contain sub.
func (l *surfaceLog) countSinceMark(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range strings.Split(string(l.b.Bytes()[l.at:]), "\n") {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

func (l *surfaceLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
