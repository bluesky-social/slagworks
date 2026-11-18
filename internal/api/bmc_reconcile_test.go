package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

func bmcAdminSecret(kind string) *awssecrets.AdminSecret {
	return &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		"SN-ADM": {Kind: kind, Username: "ADMIN", Password: "admin-pw", ExtraConfig: map[string]any{}},
	}}
}

// TestBMCReconcileMachine_CreatesOp: the endpoint resolves the admin runtime
// exactly as power ops do and persists a bmc_reconcile op carrying it (the
// worker needs the admin cred for the LAN session and the serial for the
// identity gate). It is NOT lock-gated: reconciling BMC accounts on an
// in-service (running, auto-locked) host is the op's main use.
func TestBMCReconcileMachine_CreatesOp(t *testing.T) {
	s, id := adminOnlyServer(t, bmcAdminSecret(awssecrets.KindBMC))
	if err := s.store.SetMachineLocked(id, true); err != nil {
		t.Fatal(err)
	}
	pub := &fakeOpPublisher{}
	s.opPublisher = pub
	s.echo.POST("/api/v1/machines/:id/bmc-reconcile", s.bmcReconcileMachine)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/bmc-reconcile", `{}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	var op model.Operation
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	if op.Type != model.OpBMCReconcile || op.MachineID != id {
		t.Fatalf("op = %+v, want a bmc_reconcile op for %s", op, id)
	}
	p, err := op.DecodeParams()
	if err != nil {
		t.Fatal(err)
	}
	if p.BMCRuntime == nil || p.BMCRuntime.Username != "ADMIN" || p.BMCRuntime.Password != "admin-pw" ||
		p.BMCRuntime.ExpectedChassisSerial != "SN-ADM" {
		t.Errorf("params must carry the admin runtime + serial gate: %+v", p.BMCRuntime)
	}
	if p.PDURuntime != nil || p.AMTRuntime != nil {
		t.Errorf("bmc_reconcile is IPMI-LAN only: %+v", p)
	}
	if p.MonitorRuntime != nil || p.RotateMonitor {
		t.Errorf("no stored monitor entry and no --rotate-monitor: the op must carry neither, got %+v", p)
	}
	if len(pub.deltas) != 1 || pub.deltas[0].ID != op.ID {
		t.Errorf("pending op must be published as an op_delta: %+v", pub.deltas)
	}
}

const storedMonitorPW = "St0red!monPW"

// storedMonitorServer is adminOnlyServer with a kind=bmc admin entry, the
// given monitor entry stored for the device's serial (SN-ADM in popa), and
// the server's logs captured.
func storedMonitorServer(t *testing.T, entry awssecrets.BMCMonitorEntry) (*Server, uuid.UUID, *bytes.Buffer) {
	t.Helper()
	s, id := adminOnlyServer(t, bmcAdminSecret(awssecrets.KindBMC))
	s.bmcSecrets.(*awssecrets.FakeReader).Monitor = map[string]map[string]awssecrets.BMCMonitorEntry{
		"popa": {"SN-ADM": entry},
	}
	logs := &bytes.Buffer{}
	s.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s.echo.POST("/api/v1/machines/:id/bmc-reconcile", s.bmcReconcileMachine)
	return s, id, logs
}

func storedMonitorEntry() awssecrets.BMCMonitorEntry {
	return awssecrets.BMCMonitorEntry{Kind: awssecrets.KindBMC, Username: "monitor", Password: storedMonitorPW}
}

// createBMCReconcile POSTs body to the bmc-reconcile endpoint and returns the
// status and raw response body.
func createBMCReconcile(t *testing.T, s *Server, id uuid.UUID, body string) (int, []byte) {
	t.Helper()
	srv := httptest.NewServer(s.echo)
	defer srv.Close()
	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/bmc-reconcile", body)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestBMCReconcileMachine_StoredMonitor_CarriedToOp: when
// pop/<pop>/bmc-creds-monitor holds an entry for the device's serial, the
// op carries that password to the worker (monitor_runtime) so the worker
// re-asserts it instead of rotating. The password rides only the op params,
// the admin cred's path, and never a log line.
func TestBMCReconcileMachine_StoredMonitor_CarriedToOp(t *testing.T) {
	s, id, logs := storedMonitorServer(t, storedMonitorEntry())
	status, body := createBMCReconcile(t, s, id, `{}`)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	var op model.Operation
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	p, err := op.DecodeParams()
	if err != nil {
		t.Fatal(err)
	}
	if p.MonitorRuntime == nil || p.MonitorRuntime.Password != storedMonitorPW || p.RotateMonitor {
		t.Errorf("params must carry the stored monitor password and no rotation: %+v", p)
	}
	if strings.Contains(logs.String(), storedMonitorPW) {
		t.Errorf("the stored monitor password reached the server logs:\n%s", logs.String())
	}
	if len(op.Result) != 0 {
		t.Errorf("a pending op has no result, got %s", op.Result)
	}
}

// TestBMCReconcileMachine_RotateMonitor: rotate_monitor forces a freshly
// generated password even though one is stored: the op carries the rotate
// flag and NOT the stored password.
func TestBMCReconcileMachine_RotateMonitor(t *testing.T) {
	s, id, _ := storedMonitorServer(t, storedMonitorEntry())
	status, body := createBMCReconcile(t, s, id, `{"rotate_monitor":true}`)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, body)
	}
	if strings.Contains(string(body), storedMonitorPW) {
		t.Errorf("a rotation must not carry the stored password: %s", body)
	}
	var op model.Operation
	if err := json.Unmarshal(body, &op); err != nil {
		t.Fatal(err)
	}
	p, err := op.DecodeParams()
	if err != nil {
		t.Fatal(err)
	}
	if !p.RotateMonitor || p.MonitorRuntime != nil {
		t.Errorf("params = %+v, want rotate_monitor and no monitor_runtime", p)
	}
}

// TestBMCReconcileMachine_UnusableStoredMonitor_Refused: a stored entry that
// cannot be re-asserted (another account's name, no password) is refused
// with a 422 naming the fix, rather than silently rotated over or written to
// the monitor account under the wrong name. --rotate-monitor overrides it.
func TestBMCReconcileMachine_UnusableStoredMonitor_Refused(t *testing.T) {
	for name, entry := range map[string]awssecrets.BMCMonitorEntry{
		"another account": {Kind: awssecrets.KindBMC, Username: "exporter", Password: storedMonitorPW},
		"no password":     {Kind: awssecrets.KindBMC, Username: "monitor"},
	} {
		t.Run(name, func(t *testing.T) {
			s, id, _ := storedMonitorServer(t, entry)
			status, body := createBMCReconcile(t, s, id, `{}`)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", status, body)
			}
			for _, want := range []string{"pop/popa/bmc-creds-monitor", "SN-ADM", "--rotate-monitor"} {
				if !strings.Contains(string(body), want) {
					t.Errorf("body %s should mention %q", body, want)
				}
			}
			if strings.Contains(string(body), storedMonitorPW) {
				t.Errorf("refusal leaks the stored password: %s", body)
			}
			if ops, _ := s.store.ListOperations(&id); len(ops) != 0 {
				t.Errorf("refusal persisted %d op(s)", len(ops))
			}

			status, body = createBMCReconcile(t, s, id, `{"rotate_monitor":true}`)
			if status != http.StatusCreated {
				t.Errorf("--rotate-monitor must override an unusable stored entry: status %d body=%s", status, body)
			}
		})
	}
}

// monitorReadErrReader is a FakeReader whose monitor read fails the way an
// AWS outage would.
type monitorReadErrReader struct{ awssecrets.FakeReader }

func (*monitorReadErrReader) GetMonitorEntry(context.Context, string, string) (*awssecrets.BMCMonitorEntry, error) {
	return nil, errors.New("aws is down")
}

// TestResolveMonitorRuntime covers the monitor half of bmc_reconcile op
// creation without a database: the stored entry's password rides the op
// unless the op rotates or there is none; a missing secret, an unreadable
// one, or an entry that cannot be re-asserted refuses, never echoing the
// password.
func TestResolveMonitorRuntime(t *testing.T) {
	dev := &netbox.Device{Name: "kworker-a.popa", Serial: "SN-ADM", Site: &netbox.NestedRef{Slug: "popa"}}
	stored := func(e awssecrets.BMCMonitorEntry) awssecrets.Reader {
		return &awssecrets.FakeReader{Monitor: map[string]map[string]awssecrets.BMCMonitorEntry{"popa": {"SN-ADM": e}}}
	}
	cases := []struct {
		name       string
		reader     awssecrets.Reader
		rotate     bool
		wantPW     string // "" = no runtime
		wantStatus int    // 0 = no error
		wantInErr  []string
	}{
		{name: "stored entry is carried", reader: stored(storedMonitorEntry()), wantPW: storedMonitorPW},
		{name: "no entry: generate", reader: &awssecrets.FakeReader{}},
		{name: "rotate ignores the stored entry", reader: stored(storedMonitorEntry()), rotate: true},
		{
			name:   "rotate overrides an unusable entry",
			reader: stored(awssecrets.BMCMonitorEntry{Username: "exporter", Password: storedMonitorPW}), rotate: true,
		},
		{
			name: "missing secret", reader: &awssecrets.FakeReader{MonitorPresent: map[string]bool{"popa": false}},
			wantStatus: http.StatusUnprocessableEntity, wantInErr: []string{"pop/popa/bmc-creds-monitor does not exist"},
		},
		{
			name: "missing secret refuses a rotation too", reader: &awssecrets.FakeReader{MonitorPresent: map[string]bool{"popa": false}},
			rotate: true, wantStatus: http.StatusUnprocessableEntity, wantInErr: []string{"could not be stored"},
		},
		{
			name: "unreadable secret", reader: &monitorReadErrReader{},
			wantStatus: http.StatusServiceUnavailable, wantInErr: []string{"aws is down"},
		},
		{
			name:       "another account's entry",
			reader:     stored(awssecrets.BMCMonitorEntry{Username: "exporter", Password: storedMonitorPW}),
			wantStatus: http.StatusUnprocessableEntity, wantInErr: []string{`"exporter"`, "SN-ADM", "--rotate-monitor"},
		},
		{
			name:       "entry without a password",
			reader:     stored(awssecrets.BMCMonitorEntry{Username: "monitor"}),
			wantStatus: http.StatusUnprocessableEntity, wantInErr: []string{"password is empty", "--rotate-monitor"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{bmcSecrets: tc.reader}
			rt, err := s.resolveMonitorRuntime(context.Background(), dev, tc.rotate)
			if tc.wantStatus != 0 {
				var he *echo.HTTPError
				if !errors.As(err, &he) || he.Code != tc.wantStatus {
					t.Fatalf("err = %v, want HTTP %d", err, tc.wantStatus)
				}
				for _, want := range tc.wantInErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q should mention %q", err, want)
					}
				}
				if strings.Contains(err.Error(), storedMonitorPW) {
					t.Errorf("error leaks the stored password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveMonitorRuntime: %v", err)
			}
			switch {
			case tc.wantPW == "" && rt != nil:
				t.Errorf("runtime = %+v, want none", rt)
			case tc.wantPW != "" && (rt == nil || rt.Password != tc.wantPW):
				t.Errorf("runtime = %+v, want the stored password", rt)
			}
		})
	}
}

// TestBMCReconcileMachine_BadBody: a body that does not decode is a 400
// before anything is resolved, not a silent compliance run.
func TestBMCReconcileMachine_BadBody(t *testing.T) {
	s := &Server{log: slog.Default(), echo: echo.New()}
	s.echo.POST("/api/v1/machines/:id/bmc-reconcile", s.bmcReconcileMachine)
	status, body := createBMCReconcile(t, s, uuid.New(), `{"rotate_monitor":"yes"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
}

// TestBMCReconcileMachine_Refusals: every refusal is a 422 at creation (no
// op row), naming what to fix — a credless or unstorable reconcile would
// only fail later at the worker, after rotating a password nobody can store.
func TestBMCReconcileMachine_Refusals(t *testing.T) {
	cases := []struct {
		name    string
		secret  *awssecrets.AdminSecret
		monitor bool
		want    string
	}{
		{"no admin entry", &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{}}, true, "pop/popa/bmc-creds-admin"},
		{"amt endpoint", bmcAdminSecret(awssecrets.KindAMT), true, "Intel AMT"},
		{"monitor secret missing", bmcAdminSecret(awssecrets.KindBMC), false, "pop/popa/bmc-creds-monitor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, id := adminOnlyServer(t, tc.secret)
			s.bmcSecrets.(*awssecrets.FakeReader).MonitorPresent = map[string]bool{"popa": tc.monitor}
			s.echo.POST("/api/v1/machines/:id/bmc-reconcile", s.bmcReconcileMachine)
			srv := httptest.NewServer(s.echo)
			defer srv.Close()

			resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/bmc-reconcile", `{}`)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body %s should mention %q", body, tc.want)
			}
			ops, err := s.store.ListOperations(&id)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 0 {
				t.Errorf("refusal persisted %d op(s)", len(ops))
			}
		})
	}
}

// TestBMCReconcileMachine_NoNetBoxRefuses: without NetBox the admin entry
// can't be resolved at all. Power ops tolerate that in dev (the worker falls
// back to snapshot creds); a remote account reconcile must not — its safety
// gate needs the exact admin username — so it refuses.
func TestBMCReconcileMachine_NoNetBoxRefuses(t *testing.T) {
	s, st, _, _ := opFlowServer(t)
	id := seedMachine(t, st, model.MachineStateRunning)
	s.echo.POST("/api/v1/machines/:id/bmc-reconcile", s.bmcReconcileMachine)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/bmc-reconcile", `{}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "bmc-creds-admin") {
		t.Fatalf("status = %d body=%s, want 422 naming bmc-creds-admin", resp.StatusCode, body)
	}
}
