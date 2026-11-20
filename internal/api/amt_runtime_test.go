package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/ws"
)

const (
	amtDevID  = 4242
	amtIPID   = 777
	amtSerial = "MS01-SN-123"
)

// amtNetBox serves the two NetBox reads the AMT resolver makes: the device
// (with its nested oob_ip ref) and the referenced IPAddress (for status).
func amtNetBox(t *testing.T, oob *netbox.NestedIPAddress, ipStatus string) *netbox.Client {
	t.Helper()
	dev := netbox.Device{
		ID: amtDevID, Name: "router1.pop-a", Serial: amtSerial,
		Site:  &netbox.NestedRef{ID: 1, Name: "pop-a", Slug: "pop-a"},
		OOBIP: oob,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/dcim/devices/%d/", amtDevID), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, dev)
	})
	mux.HandleFunc(fmt.Sprintf("/api/ipam/ip-addresses/%d/", amtIPID), func(w http.ResponseWriter, _ *http.Request) {
		ip := netbox.IPAddress{ID: amtIPID, Address: "10.252.240.23/22", AssignedObjectType: "dcim.interface", AssignedObjectID: new(5)}
		if ipStatus != "" {
			ip.Status = &netbox.NestedValue{Value: ipStatus}
		}
		writeJSON(w, ip)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return netbox.NewClient(netbox.ClientConfig{BaseURL: srv.URL, Token: "t"})
}

func amtSecret(kind, user, pass string) awssecrets.Reader {
	return &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{
		"pop-a": {BySerial: map[string]awssecrets.AdminHost{amtSerial: {Kind: kind, Username: user, Password: pass}}},
	}}
}

// amtServer is opFlowServer (real store, pop-a site_config) plus NetBox, the
// admin secret, and one machine bound to the AMT device.
func amtServer(t *testing.T, nb *netbox.Client, secrets awssecrets.Reader) (*Server, uuid.UUID) {
	t.Helper()
	s, st, _, _ := opFlowServer(t)
	s.netbox, s.bmcSecrets = nb, secrets
	id := uuid.New()
	devID := amtDevID
	if err := st.CreateMachine(&model.Machine{ID: id, NetBoxDeviceID: &devID, NetBoxSerial: amtSerial,
		State: model.MachineStateRunning, PXEEligible: true}); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	return s, id
}

var activeOOB = &netbox.NestedIPAddress{ID: amtIPID, Address: "10.252.240.23/22"}

func TestResolveAMTRuntime_HappyPath(t *testing.T) {
	s, id := amtServer(t, amtNetBox(t, activeOOB, "active"), amtSecret(awssecrets.KindAMT, "admin", "amt-pw"))
	var p model.OpParams
	if err := s.resolveAMTRuntime(context.Background(), id, &p); err != nil {
		t.Fatalf("resolveAMTRuntime: %v", err)
	}
	want := model.AMTRuntime{Host: "10.252.240.23", Username: "admin", Password: "amt-pw"}
	if p.AMTRuntime == nil || *p.AMTRuntime != want {
		t.Fatalf("AMTRuntime = %+v, want %+v (mask stripped, kind=amt creds)", p.AMTRuntime, want)
	}
	if p.BMCRuntime != nil || p.PDURuntime != nil {
		t.Errorf("only amt_runtime may be stamped: %+v", p)
	}
}

// TestResolveAMTRuntime_HardFailures pins every gap as a hard error with the
// reason in the message (the operator gets it verbatim in the 422).
func TestResolveAMTRuntime_HardFailures(t *testing.T) {
	cases := []struct {
		name    string
		oob     *netbox.NestedIPAddress
		status  string
		secrets awssecrets.Reader
		wantSub string
	}{
		{"no oob_ip", nil, "active", amtSecret(awssecrets.KindAMT, "admin", "pw"), "no oob_ip"},
		{"reserved oob_ip", activeOOB, "reserved", amtSecret(awssecrets.KindAMT, "admin", "pw"), `status "reserved"`},
		{"bmc entry", activeOOB, "active", amtSecret(awssecrets.KindBMC, "ADMIN", "pw"), "not amt"},
		{"incomplete entry", activeOOB, "active", amtSecret(awssecrets.KindAMT, "admin", ""), "incomplete"},
		{"no entry", activeOOB, "active", &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{"pop-a": {BySerial: map[string]awssecrets.AdminHost{}}}}, "no entry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, id := amtServer(t, amtNetBox(t, tc.oob, tc.status), tc.secrets)
			var p model.OpParams
			err := s.resolveAMTRuntime(context.Background(), id, &p)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantSub)
			}
			var gap *transportGapError
			if !errors.As(err, &gap) {
				t.Errorf("err = %v is not a transportGapError, so it would be answered 503 instead of 422", err)
			}
			if p.AMTRuntime != nil {
				t.Errorf("a failed resolve must not stamp amt_runtime: %+v", p.AMTRuntime)
			}
		})
	}
}

// TestResolveVia_AutoSelectsByCredentialKind is plan D1: explicit via wins;
// omitted via picks amt for a kind=amt entry and bmc otherwise.
func TestResolveVia_AutoSelectsByCredentialKind(t *testing.T) {
	ctx := context.Background()
	s, id := amtServer(t, amtNetBox(t, activeOOB, "active"), amtSecret(awssecrets.KindAMT, "admin", "pw"))
	for in, want := range map[string]string{"": viaAMT, viaBMC: viaBMC, viaPDU: viaPDU, viaAMT: viaAMT} {
		got, err := s.resolveVia(ctx, id, in)
		if err != nil || got != want {
			t.Errorf("resolveVia(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	s, id = amtServer(t, amtNetBox(t, activeOOB, "active"), amtSecret(awssecrets.KindBMC, "ADMIN", "pw"))
	if got, err := s.resolveVia(ctx, id, ""); err != nil || got != viaBMC {
		t.Errorf("kind=bmc auto: got (%q, %v), want bmc", got, err)
	}
}

func decodeOp(t *testing.T, resp *http.Response) model.OpParams {
	t.Helper()
	var op model.Operation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		t.Fatalf("decode op: %v", err)
	}
	p, err := op.DecodeParams()
	if err != nil {
		t.Fatalf("decode params: %v", err)
	}
	return p
}

// TestPowerOp_AMTTransport drives the HTTP surface: an omitted via on an AMT
// machine stamps amt_runtime (never bmc_runtime); an explicit --via bmc is
// refused with the kind=amt reason; a disk boot override over AMT is a 400.
func TestPowerOp_AMTTransport(t *testing.T) {
	s, id := amtServer(t, amtNetBox(t, activeOOB, "active"), amtSecret(awssecrets.KindAMT, "admin", "amt-pw"))
	srv := httptest.NewServer(s.echo)
	defer srv.Close()
	path := "/api/v1/machines/" + id.String() + "/power"

	resp := postJSON(t, srv, path, `{"action":"cycle","boot_device":"pxe"}`)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("auto amt: status %d: %s", resp.StatusCode, b)
	}
	p := decodeOp(t, resp)
	_ = resp.Body.Close()
	if p.AMTRuntime == nil || p.AMTRuntime.Host != "10.252.240.23" || p.BMCRuntime != nil {
		t.Fatalf("auto amt params = %+v, want amt_runtime only", p)
	}
	if p.BootDevice != "pxe" {
		t.Errorf("boot_device = %q, want pxe", p.BootDevice)
	}
	// The power op is now active; abort it so the next create isn't
	// blocked by the one-active-op constraint.
	_ = postJSON(t, srv, "/api/v1/machines/"+id.String()+"/abort", `{}`).Body.Close()

	resp = postJSON(t, srv, path, `{"action":"cycle","via":"bmc"}`)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(b), "kind=amt") {
		t.Fatalf("explicit bmc on AMT: status %d body %s, want 422 naming kind=amt", resp.StatusCode, b)
	}

	resp = postJSON(t, srv, path, `{"action":"cycle","boot_device":"disk"}`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("disk over AMT: status %d, want 400", resp.StatusCode)
	}
}

// TestAMT_ModelGapIs422 is the AMT counterpart of TestViaPDU_ModelGapIs422.
// The machine's admin entry is kind=amt, so an omitted --via selects amt,
// but its NetBox device has no oob_ip: the AMT endpoint is unknown until
// someone sets it. Both the power op and the power-status read refuse with
// 422 and the AMT resolver's reason. An admin secret that cannot be read is
// a dependency outage and stays 503. No message carries a BMC prefix, and
// no operation is created.
func TestAMT_ModelGapIs422(t *testing.T) {
	type check struct {
		method, path, body string
		wantCode           int
		wantSub            string
	}
	cases := []struct {
		name    string
		oob     *netbox.NestedIPAddress
		secrets awssecrets.Reader
		checks  []check
	}{
		{
			name: "no oob_ip", oob: nil, secrets: amtSecret(awssecrets.KindAMT, "admin", "pw"),
			checks: []check{
				{http.MethodPost, "/power", `{"action":"cycle"}`, http.StatusUnprocessableEntity, `device "router1.pop-a" has no oob_ip in NetBox`},
				{http.MethodPost, "/commission", `{}`, http.StatusUnprocessableEntity, `device "router1.pop-a" has no oob_ip in NetBox`},
				{http.MethodGet, "/power", "", http.StatusUnprocessableEntity, `device "router1.pop-a" has no oob_ip in NetBox`},
			},
		},
		{
			// An explicit via=amt skips the credential-kind lookup, so the
			// AMT resolver is the first to read the (unreadable) secret.
			name: "admin secret unreadable", oob: activeOOB, secrets: &awssecrets.FakeReader{},
			checks: []check{
				{http.MethodPost, "/power", `{"action":"cycle","via":"amt"}`, http.StatusServiceUnavailable, "fetch admin secret for pop pop-a"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, id := amtServer(t, amtNetBox(t, tc.oob, "active"), tc.secrets)
			s.powerStatusRPC = &statusRPCRecorder{}
			s.echo.GET("/api/v1/machines/:id/power", s.powerStatusMachine)
			srv := httptest.NewServer(s.echo)
			defer srv.Close()

			for _, c := range tc.checks {
				url := srv.URL + "/api/v1/machines/" + id.String() + c.path
				req, _ := http.NewRequest(c.method, url, strings.NewReader(c.body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("%s %s: %v", c.method, c.path, err)
				}
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				var msg struct{ Message string }
				if err := json.Unmarshal(b, &msg); err != nil {
					t.Fatalf("%s %s: body is not an error message: %s", c.method, c.path, b)
				}
				if resp.StatusCode != c.wantCode {
					t.Errorf("%s %s: status %d, want %d: %s", c.method, c.path, resp.StatusCode, c.wantCode, msg.Message)
				}
				if !strings.Contains(msg.Message, c.wantSub) {
					t.Errorf("%s %s: message does not name the failure %q: %s", c.method, c.path, c.wantSub, msg.Message)
				}
				if !strings.HasPrefix(msg.Message, "resolve amt runtime: ") {
					t.Errorf("%s %s: want the AMT resolver's message, with no BMC prefix: %s", c.method, c.path, msg.Message)
				}
			}

			ops, err := s.store.ListOperations(&id)
			if err != nil {
				t.Fatalf("list ops: %v", err)
			}
			if len(ops) != 0 {
				t.Fatalf("refused requests created %d operation(s)", len(ops))
			}
		})
	}
}

// statusRPCRecorder captures what powerStatusMachine sends to the worker.
type statusRPCRecorder struct {
	fakeDHCPLeasesRPC
	bmc, amt json.RawMessage
}

func (r *statusRPCRecorder) PowerStatus(_ context.Context, _ string, _ uuid.UUID, bmc, amt json.RawMessage) (ws.PowerStatusResponseMsg, error) {
	r.bmc, r.amt = bmc, amt
	return ws.PowerStatusResponseMsg{State: "on", Transport: "amt"}, nil
}

// TestPowerStatus_AMTMachineSendsAMTRuntime: the status read for an AMT
// machine carries amt_runtime and no bmc_runtime.
func TestPowerStatus_AMTMachineSendsAMTRuntime(t *testing.T) {
	s, id := amtServer(t, amtNetBox(t, activeOOB, "active"), amtSecret(awssecrets.KindAMT, "admin", "amt-pw"))
	rec := &statusRPCRecorder{}
	s.powerStatusRPC = rec
	e := echo.New()
	e.GET("/api/v1/machines/:id/power", s.powerStatusMachine)
	srv := httptest.NewServer(e)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/machines/" + id.String() + "/power")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	if rec.bmc != nil {
		t.Errorf("bmc_runtime sent for an AMT machine: %s", rec.bmc)
	}
	var rt model.AMTRuntime
	if err := json.Unmarshal(rec.amt, &rt); err != nil || rt.Host != "10.252.240.23" || rt.Username != "admin" {
		t.Fatalf("amt_runtime = %s (%v), want the resolved AMT envelope", rec.amt, err)
	}
}
