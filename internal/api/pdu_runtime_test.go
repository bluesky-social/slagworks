package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// TestParseOutletIndex_StrictAndFailClosed pins the outlet-name parse.
// This is safety-critical: a sloppy parse would actuate the wrong
// outlet (cutting power to a different device on the same PDU strip).
// The convention is exactly "Outlet <n>", 1-based, anchored.
func TestParseOutletIndex_StrictAndFailClosed(t *testing.T) {
	ok := map[string]int{
		"Outlet 1":  1,
		"Outlet 5":  5,
		"Outlet 24": 24,
	}
	for name, want := range ok {
		got, err := parseOutletIndex(name)
		if err != nil {
			t.Errorf("%q: unexpected error %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%q: got %d, want %d", name, got, want)
		}
	}

	bad := []string{
		"",            // empty
		"Outlet",      // no number
		"Outlet 0",    // 0 is not a valid 1-based outlet
		"Outlet 5x",   // trailing junk
		"outlet 5",    // wrong case
		"Outlet  5",   // double space
		"AB5",         // different convention
		"5",           // bare number
		"Outlet 5 ",   // trailing space
		" Outlet 5",   // leading space
		"Port 5",      // wrong word
		"Outlet -1",   // negative
		"Outlet 5\n6", // anchored: must reject embedded newline injection
	}
	for _, name := range bad {
		if got, err := parseOutletIndex(name); err == nil {
			t.Errorf("%q: expected error, got index %d", name, got)
		}
	}
}

// TestResolvePDURuntime_NoNetBox verifies that PDU resolution fails
// loudly without NetBox — unlike BMC resolution, which soft-fails,
// PDU resolution is only invoked on explicit --via pdu, so a missing
// prerequisite is a hard error rather than a silent degrade.
func TestResolvePDURuntime_NoNetBox(t *testing.T) {
	s := &Server{}
	var params model.OpParams
	if err := s.resolvePDURuntime(context.Background(), uuid.New(), &params); err == nil {
		t.Fatal("expected error when NetBox is not configured")
	}
}

// pduNetBox serves the two NetBox reads resolvePDURuntime makes before it
// reaches a PDU: the target device and its power-ports. A non-zero
// portsStatus makes the power-ports read fail with that HTTP status: NetBox
// unhealthy, as opposed to NetBox describing the device.
func pduNetBox(t *testing.T, devID int, ports []netbox.PowerPort, portsStatus int) *netbox.Client {
	t.Helper()
	dev := netbox.Device{
		ID: devID, Name: "switch-0.pop-a", Serial: "SN-PDU-TARGET",
		Site: &netbox.NestedRef{ID: 1, Name: "pop-a", Slug: "pop-a"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/api/dcim/devices/%d/", devID), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, dev)
	})
	mux.HandleFunc("/api/dcim/power-ports/", func(w http.ResponseWriter, _ *http.Request) {
		if portsStatus != 0 {
			http.Error(w, `{"detail":"database unavailable"}`, portsStatus)
			return
		}
		writeJSONList(w, ports)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return netbox.NewClient(netbox.ClientConfig{BaseURL: srv.URL, Token: "t"})
}

// TestViaPDU_ModelGapIs422: `slagctl machine power cycle <host> --via pdu`,
// or deploy / commission --via pdu, for a device whose power path NetBox
// does not model. NetBox answered, and its answer stays true until someone
// models the feeds, so the refusal is a 422 that names the gap, not a 503
// telling the operator (or their automation) to retry later. A power-ports
// read that NetBox itself fails is a dependency outage and stays 503. In
// every case the message is the PDU resolver's own; no BMC is involved, and
// no operation is created.
func TestViaPDU_ModelGapIs422(t *testing.T) {
	const devID = 700
	uncabled := []netbox.PowerPort{{ID: 1, Name: "PSU1", ConnectedEndpointsReachable: true}}
	cases := []struct {
		name        string
		ports       []netbox.PowerPort
		portsStatus int
		wantCode    int
		wantSub     string
	}{
		{"no power-ports modeled", nil, 0, http.StatusUnprocessableEntity, "device 700 has no power-ports modeled in NetBox"},
		{"power-port without a cable", uncabled, 0, http.StatusUnprocessableEntity, `power-port "PSU1" (id 1) has no connected outlet`},
		{"NetBox fails the read", nil, http.StatusInternalServerError, http.StatusServiceUnavailable, "list power-ports for device 700"},
	}
	routes := []struct{ path, body string }{
		{"/power", `{"action":"cycle","via":"pdu"}`},
		{"/deploy", `{"destructive":true,"via":"pdu"}`},
		{"/commission", `{"via":"pdu"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, st, _, _ := opFlowServer(t)
			s.netbox = pduNetBox(t, devID, tc.ports, tc.portsStatus)
			id := uuid.New()
			dev := devID
			if err := st.CreateMachine(&model.Machine{
				ID: id, NetBoxDeviceID: &dev, NetBoxSerial: "SN-PDU-TARGET",
				State: model.MachineStateCommissioned, PXEEligible: true,
			}); err != nil {
				t.Fatalf("seed machine: %v", err)
			}
			// Deploy refuses with 424 before resolving the transport when no
			// SSH key is registered.
			if err := st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
				t.Fatalf("seed ssh key: %v", err)
			}
			srv := httptest.NewServer(s.echo)
			defer srv.Close()

			for _, r := range routes {
				resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+r.path, r.body)
				b, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				var msg struct{ Message string }
				if err := json.Unmarshal(b, &msg); err != nil {
					t.Fatalf("POST %s via pdu: body is not an error message: %s", r.path, b)
				}
				if resp.StatusCode != tc.wantCode {
					t.Errorf("POST %s via pdu: status %d, want %d: %s", r.path, resp.StatusCode, tc.wantCode, msg.Message)
				}
				if !strings.Contains(msg.Message, tc.wantSub) {
					t.Errorf("POST %s via pdu: message does not name the failure %q: %s", r.path, tc.wantSub, msg.Message)
				}
				if !strings.HasPrefix(msg.Message, "resolve pdu runtime: ") {
					t.Errorf("POST %s via pdu: want the PDU resolver's message, with no BMC prefix: %s", r.path, msg.Message)
				}
			}

			ops, err := st.ListOperations(&id)
			if err != nil {
				t.Fatalf("list ops: %v", err)
			}
			if len(ops) != 0 {
				t.Fatalf("refused requests created %d operation(s)", len(ops))
			}
		})
	}
}

// TestPDURuntime_Shape pins the envelope wire format. The worker
// unmarshals op.Params["pdu_runtime"].outlets into model.PDUOutlet;
// renaming a JSON field here without the worker silently breaks
// actuation. resolvePDURuntime now stamps params.PDURuntime directly
// (no merge dance), so the shape is pinned by marshalling a populated
// OpParams and round-tripping the pdu_runtime block — plus confirming
// it composes with a sibling op-type field.
func TestPDURuntime_Shape(t *testing.T) {
	rt := model.PDURuntime{Outlets: []model.PDUOutlet{
		{PDUHost: "10.254.240.154", Outlet: 5, Serial: "ZA2451003321", Username: "apc", Password: "pw"},
	}}

	params := model.OpParams{BootDevice: "pxe", PDURuntime: &rt}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["boot_device"]; !ok {
		t.Errorf("marshalled params dropped boot_device key: %s", raw)
	}
	pduRaw, ok := obj["pdu_runtime"]
	if !ok {
		t.Fatalf("missing pdu_runtime key: %s", raw)
	}
	var got model.PDURuntime
	if err := json.Unmarshal(pduRaw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Outlets) != 1 || got.Outlets[0].PDUHost != "10.254.240.154" || got.Outlets[0].Outlet != 5 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
