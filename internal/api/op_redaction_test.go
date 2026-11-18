package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// staticRebuilder hands the hub one fixed snapshot on a cold hello.
type staticRebuilder struct{ snap model.SiteSnapshot }

func (r staticRebuilder) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return r.snap, nil
}

// operatorDo sends an operator-authenticated request through the real
// router and returns the status and body.
func operatorDo(t *testing.T, srv *httptest.Server, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer op-tok")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// readOpDelta reads the next op_delta off the worker connection.
func readOpDelta(t *testing.T, conn *ws.Conn) model.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("worker read: %v", err)
	}
	if env.Type != ws.TypeOpDelta {
		t.Fatalf("worker got %q, want op_delta", env.Type)
	}
	var d ws.OpDeltaMsg
	if err := json.Unmarshal(env.Data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Operation
}

// TestOperatorReadsRedactCredentials_WorkerStillReceivesThem drives the
// production router (NewServer) and the real hub: an op created through the
// operator API reaches the connected site worker with its real credentials
// on the op stream, while the op-creation response and every operator read
// of the same op show only the redaction marker.
func TestOperatorReadsRedactCredentials_WorkerStillReceivesThem(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	devID := 300
	dev := netbox.Device{
		ID: devID, Name: "kworker-a.popa", Serial: "SN-ADM",
		Site: &netbox.NestedRef{ID: 1, Name: "popa", Slug: "popa"},
	}
	f := &netboxFixture{devices: []netbox.Device{dev}, deviceByID: map[int]netbox.Device{devID: dev}}
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: f.server(t).URL, Token: "t"})
	machineID := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: machineID, NetBoxSerial: "SN-ADM", NetBoxDeviceID: &devID, State: model.MachineStateRunning,
	}); err != nil {
		t.Fatal(err)
	}
	reader := &awssecrets.FakeReader{
		Secrets: map[string]*awssecrets.AdminSecret{"popa": bmcAdminSecret(awssecrets.KindBMC)},
		Monitor: map[string]map[string]awssecrets.BMCMonitorEntry{"popa": {"SN-ADM": storedMonitorEntry()}},
	}
	_, workerToken, err := st.RegisterSiteWorker("popa", "worker-1", "w1.popa")
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	snap := model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "popa"},
		Machines: []model.MachineSnapshot{{ID: machineID}}}
	hub := ws.NewHub(ws.HubConfig{Auth: st, Rebuilder: staticRebuilder{snap}, Logger: quiet})
	hub.Push("popa", snap)

	s := NewServer(Config{
		RequiredGroups: []string{"platform"},
		Store:          st, NetBox: nb, BMCSecrets: reader, WSHub: hub, OpPublisher: hub,
		Verifier: &fakeVerifier{claims: map[string]*tokenverify.Claims{
			"op-tok": {Subject: "op", Groups: []string{"platform"}},
		}},
	})
	s.SetLogger(quiet)
	srv := httptest.NewServer(s.Echo())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	worker, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws/site?token="+workerToken, nil)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}
	defer worker.CloseNow()
	if err := worker.WriteTyped(ctx, ws.TypeHello, "", ws.HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 1, Proto: ws.OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	// Hello answers no_change (the worker is current), then op_resync.
	for range 2 {
		if _, err := worker.ReadMessage(ctx); err != nil {
			t.Fatalf("hello reply: %v", err)
		}
	}

	creds := []string{"admin-pw", storedMonitorPW}
	assertOperatorSafe := func(what string, body []byte) {
		t.Helper()
		for _, c := range creds {
			if bytes.Contains(body, []byte(c)) {
				t.Errorf("%s returns a plaintext credential (%d bytes)", what, len(c))
			}
		}
		if !bytes.Contains(body, []byte(`"password":"`+model.RedactedMarker+`"`)) {
			t.Errorf("%s does not show the redaction marker where the credential was", what)
		}
	}

	// Power op (createOperation's shared tail) and bmc_reconcile (its own
	// handler, carrying the stored monitor password).
	var opIDs []uuid.UUID
	for _, c := range []struct{ path, body string }{
		{"/api/v1/machines/" + machineID.String() + "/power", `{"action":"cycle"}`},
		{"/api/v1/machines/" + machineID.String() + "/bmc-reconcile", `{}`},
	} {
		status, body := operatorDo(t, srv, http.MethodPost, c.path, c.body)
		if status != http.StatusCreated {
			t.Fatalf("POST %s: status %d: %s", c.path, status, body)
		}
		assertOperatorSafe("POST "+c.path, body)
		var created model.Operation
		if err := json.Unmarshal(body, &created); err != nil {
			t.Fatal(err)
		}
		opIDs = append(opIDs, created.ID)

		got := readOpDelta(t, worker)
		p, err := got.DecodeParams()
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != created.ID || p.BMCRuntime == nil || p.BMCRuntime.Password != "admin-pw" {
			t.Errorf("worker op_delta for %s lacks the real BMC credential (has bmc_runtime: %t)", c.path, p.BMCRuntime != nil)
		}
		if got.Type == model.OpBMCReconcile && (p.MonitorRuntime == nil || p.MonitorRuntime.Password != storedMonitorPW) {
			t.Errorf("worker op_delta for bmc_reconcile lacks the real monitor credential (has monitor_runtime: %t)", p.MonitorRuntime != nil)
		}

		// One active op per machine: abort this one before the next create.
		if status, body := operatorDo(t, srv, http.MethodPost, "/api/v1/machines/"+machineID.String()+"/abort", ""); status != http.StatusOK {
			t.Fatalf("abort: status %d: %s", status, body)
		}
		env, err := worker.ReadMessage(ctx)
		if err != nil || env.Type != ws.TypeOpRemoved {
			t.Fatalf("worker after abort: got %q (err %v), want op_removed", env.Type, err)
		}
	}

	for _, id := range opIDs {
		status, body := operatorDo(t, srv, http.MethodGet, "/api/v1/operations/"+id.String(), "")
		if status != http.StatusOK {
			t.Fatalf("GET op: status %d: %s", status, body)
		}
		assertOperatorSafe("GET /operations/:id", body)
	}
	for _, path := range []string{"/api/v1/operations", "/api/v1/operations?machine=" + machineID.String()} {
		status, body := operatorDo(t, srv, http.MethodGet, path, "")
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", path, status, body)
		}
		assertOperatorSafe("GET "+path, body)
		var ops []model.Operation
		if err := json.Unmarshal(body, &ops); err != nil {
			t.Fatal(err)
		}
		if len(ops) != len(opIDs) {
			t.Errorf("GET %s listed %d ops, want %d", path, len(ops), len(opIDs))
		}
	}

	// No scrub has run (that is ListenAndServe's sweep), so the stored row
	// still holds the credential: the markers above came from the response
	// redaction, not from the table.
	stored, err := st.GetOperation(opIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stored.Params, []byte("admin-pw")) {
		t.Error("the stored params lost the credential before any scrub ran")
	}
}

// TestListenAndServe_ScrubsTerminalCredentialsAtStartup: starting global's
// API strips credentials from ops that went terminal while it was down (and
// from all history on the first rollout), and leaves an active op's alone.
func TestListenAndServe_ScrubsTerminalCredentialsAtStartup(t *testing.T) {
	s, st, _, _ := opFlowServer(t)
	s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	params := json.RawMessage(`{"bmc_runtime":{"expected_chassis_serial":"SN","username":"ADMIN","password":"fixture-pw","extra_config":{}}}`)
	terminal := &model.Operation{MachineID: seedMachine(t, st, model.MachineStateRunning), Type: model.OpPowerCycle,
		Status: model.OpCompleted, Params: params}
	active := &model.Operation{MachineID: seedMachine(t, st, model.MachineStateRunning), Type: model.OpPowerCycle,
		Status: model.OpRunning, Params: params}
	for _, op := range []*model.Operation{terminal, active} {
		if err := st.CreateOperation(op); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx, "127.0.0.1:0") }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("ListenAndServe: %v", err)
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := st.GetOperation(terminal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(got.Params, []byte("fixture-pw")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the terminal op still stores its credential 10s after startup")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := st.GetOperation(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got.Params, []byte("fixture-pw")) {
		t.Error("the startup scrub stripped an active op's credential")
	}
}

// serializeThrough runs target through the operator serializer on a bare
// echo context and returns the body and error.
func serializeThrough(t *testing.T, target any) ([]byte, error) {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	err := operatorJSONSerializer{}.Serialize(c, target, "")
	return rec.Body.Bytes(), err
}

// TestOperatorJSONSerializer_Shapes: every shape an operator handler returns
// an op in is redacted, the caller's op (whose Params bytes the worker-facing
// publish shares) is left intact, and anything else passes through.
func TestOperatorJSONSerializer_Shapes(t *testing.T) {
	params := json.RawMessage(`{"bmc_runtime":{"expected_chassis_serial":"SN","username":"ADMIN","password":"fixture-pw","extra_config":{}}}`)
	orig := bytes.Clone(params)
	op := model.Operation{ID: uuid.New(), Type: model.OpPowerCycle, Params: params}
	for name, target := range map[string]any{
		"value":   op,
		"pointer": &op,
		"slice":   []model.Operation{op, op},
	} {
		body, err := serializeThrough(t, target)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bytes.Contains(body, []byte("fixture-pw")) {
			t.Errorf("%s: the credential reached the response", name)
		}
		if !bytes.Contains(body, []byte(`"password":"[redacted]"`)) || !bytes.Contains(body, []byte(`"username":"ADMIN"`)) {
			t.Errorf("%s: want the marker in place of the credential and the username kept: %s", name, body)
		}
	}
	if !bytes.Equal(op.Params, orig) {
		t.Fatal("serializing modified the caller's op params")
	}

	if body, err := serializeThrough(t, (*model.Operation)(nil)); err != nil || strings.TrimSpace(string(body)) != "null" {
		t.Errorf("nil op: body %q err %v, want null", body, err)
	}
	if body, err := serializeThrough(t, map[string]string{"k": "v"}); err != nil || !strings.Contains(string(body), `"k":"v"`) {
		t.Errorf("non-op target: body %q err %v", body, err)
	}
}

// TestOperatorJSONSerializer_RefusesUnhandledOpShape: a response type that
// carries an Operation the serializer doesn't know how to redact fails the
// request instead of shipping the credential.
func TestOperatorJSONSerializer_RefusesUnhandledOpShape(t *testing.T) {
	type wrapped struct {
		Op model.Operation `json:"op"`
	}
	type envelope struct {
		Data any `json:"data"`
	}
	params := json.RawMessage(`{"amt_runtime":{"host":"h","username":"u","password":"fixture-pw"}}`)
	op := model.Operation{Params: params}
	for name, target := range map[string]any{
		"struct field":          wrapped{Op: op},
		"map value":             map[string]model.Operation{"a": op},
		"pointers":              []*model.Operation{&op},
		"map of any":            map[string]any{"op": op},
		"slice of any":          []any{map[string]any{"nested": &op}},
		"struct with any field": envelope{Data: []model.Operation{op}},
	} {
		body, err := serializeThrough(t, target)
		if err == nil {
			t.Errorf("%s: want a refusal, got none", name)
		}
		if bytes.Contains(body, []byte("fixture-pw")) {
			t.Errorf("%s: the credential reached the response", name)
		}
	}

	// A value that could hold an op but doesn't is served, and a cyclic one
	// is refused rather than walked forever.
	if _, err := serializeThrough(t, envelope{Data: map[string]any{"k": 1}}); err != nil {
		t.Errorf("op-free envelope refused: %v", err)
	}
	if _, err := serializeThrough(t, wrappedPtr{}); err != nil {
		t.Errorf("nil op pointer refused: %v", err)
	}
	cyclic := []any{nil}
	cyclic[0] = cyclic
	if _, err := serializeThrough(t, cyclic); err == nil {
		t.Error("cyclic value: want a refusal, got none")
	}
}

type wrappedPtr struct {
	Op *model.Operation `json:"op"`
}
