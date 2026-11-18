package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
)

// resolvedOutlet returns a connected OutletResolution for use in tests.
func resolvedOutlet() api.OutletResolution {
	return api.OutletResolution{
		PDUName:          "pdu-0102-left.pop1",
		PDUMgmtIP:        "10.252.1.50",
		PDUSerial:        "PDU-SN-001",
		Outlet:           5,
		Connected:        true,
		DownstreamDevice: "cpu1-pop1",
		DownstreamSerial: "SN-CPU1",
		DownstreamPort:   "PSU1",
		Classification:   "sole",
		Warning:          "Outlet 5 may be the only effective power feed for cpu1-pop1; cycling it may hard-power-off cpu1-pop1.",
		PreflightToken:   "aabbcc.deadbeef0000000000000000000000000000000000000000000000000000000000",
	}
}

// TestPowerOutletCycle_HappyPath verifies the full resolve→POST→print-op-id flow
// when --yes bypasses the interactive confirm.
func TestPowerOutletCycle_HappyPath(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()

	var capturedResolveURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-0102-left.pop1/outlet/5/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			capturedResolveURL = r.URL.String()
			res := resolvedOutlet()
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-0102-left.pop1/outlet/5/power",
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpPowerCycle,
				Status:    model.OpPending,
			})
		})

	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "power", "outlet", "cycle", "pdu-0102-left.pop1", "5", "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify resolve was called with verb=cycle
	if !strings.Contains(capturedResolveURL, "verb=cycle") {
		t.Errorf("expected verb=cycle in resolve URL, got: %q", capturedResolveURL)
	}

	// Verify op ID and watch hint in stdout
	if !strings.Contains(out, opID.String()) {
		t.Errorf("expected op ID %s in output, got: %q", opID, out)
	}
	if !strings.Contains(out, "slagctl operation watch") {
		t.Errorf("expected watch hint in output, got: %q", out)
	}
}

// TestPowerOutletCycle_UnconnectedRefuses verifies that an unconnected outlet
// returns an error without POSTing.
func TestPowerOutletCycle_UnconnectedRefuses(t *testing.T) {
	postCalled := false

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-0102-left.pop1/outlet/3/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			res := api.OutletResolution{
				PDUName:        "pdu-0102-left.pop1",
				PDUMgmtIP:      "10.252.1.50",
				Outlet:         3,
				Connected:      false,
				Classification: "unconnected",
				Warning: "Outlet 3 on pdu-0102-left.pop1 has no cabled downstream device in NetBox; " +
					"refusing because the self-power interlock cannot determine what it feeds.",
			}
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-0102-left.pop1/outlet/3/power",
		func(w http.ResponseWriter, r *http.Request) {
			postCalled = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{})
		})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "cycle", "pdu-0102-left.pop1", "3", "--yes")
	if err == nil {
		t.Fatal("expected error for unconnected outlet, got nil")
	}
	if !strings.Contains(err.Error(), "unconnected") {
		t.Errorf("expected 'unconnected' in error, got: %v", err)
	}
	if postCalled {
		t.Error("POST should not have been called for an unconnected outlet")
	}
}

// TestPowerOutletCycle_YesBypassesConfirm verifies --yes sends the POST
// without interactive confirmation. (Overlaps happy-path; kept as an
// explicit invariant test for the flag's contract.)
func TestPowerOutletCycle_YesBypassesConfirm(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()
	postCalled := false

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/1/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			res := api.OutletResolution{
				PDUName:          "pdu-test",
				PDUMgmtIP:        "10.0.0.1",
				Outlet:           1,
				Connected:        true,
				DownstreamDevice: "some-device",
				DownstreamSerial: "SN-SOME",
				DownstreamPort:   "PSU1",
				Classification:   "sole",
				Warning:          "Outlet 1 may be the only effective power feed for some-device.",
				PreflightToken:   "aabbcc.deadbeef0000000000000000000000000000000000000000000000000000000000",
			}
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/1/power",
		func(w http.ResponseWriter, r *http.Request) {
			postCalled = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpPowerCycle,
			})
		})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "cycle", "pdu-test", "1", "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !postCalled {
		t.Error("expected POST to be called with --yes, but it was not")
	}
}

// TestPowerOutletCycle_NonIntegerOutletErrors verifies that a non-integer
// outlet arg fails early with a clear message, before any HTTP call.
func TestPowerOutletCycle_NonIntegerOutletErrors(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "cycle", "pdu-test", "notanumber", "--yes")
	if err == nil {
		t.Fatal("expected error for non-integer outlet, got nil")
	}
	if !strings.Contains(err.Error(), "notanumber") {
		t.Errorf("expected outlet arg in error, got: %v", err)
	}
	if callCount > 0 {
		t.Errorf("expected no HTTP calls for invalid outlet arg, got %d", callCount)
	}
}

// TestPowerOutletCycle_ZeroOutletErrors verifies outlet=0 is rejected (must be >= 1).
func TestPowerOutletCycle_ZeroOutletErrors(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "power", "outlet", "cycle", "pdu-test", "0", "--yes")
	if err == nil {
		t.Fatal("expected error for outlet=0, got nil")
	}
	if !strings.Contains(err.Error(), "integer >= 1") {
		t.Errorf("expected 'integer >= 1' in error, got: %v", err)
	}
}

// TestPowerOutletOn_VerbPassedThrough verifies `outlet on` sends verb=on to
// the resolve endpoint and on to the POST.
func TestPowerOutletOn_VerbPassedThrough(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()
	var resolveVerb, postVerb string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/2/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			resolveVerb = r.URL.Query().Get("verb")
			res := api.OutletResolution{
				PDUName:          "pdu-test",
				PDUMgmtIP:        "10.0.0.1",
				Outlet:           2,
				Connected:        true,
				DownstreamDevice: "some-device",
				DownstreamSerial: "SN-X",
				DownstreamPort:   "PSU1",
				Classification:   "sole",
				Warning:          "Outlet 2 may be the only effective power feed.",
				PreflightToken:   "aabbcc.deadbeef0000000000000000000000000000000000000000000000000000000000",
			}
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/2/power",
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			postVerb = body["verb"]
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpPowerOn,
			})
		})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "on", "pdu-test", "2", "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolveVerb != "on" {
		t.Errorf("expected resolve verb=on, got: %q", resolveVerb)
	}
	if postVerb != "on" {
		t.Errorf("expected POST verb=on, got: %q", postVerb)
	}
}

// TestPowerOutletOff_VerbPassedThrough verifies `outlet off` sends verb=off
// and prints the "strands device" note.
func TestPowerOutletOff_VerbPassedThrough(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()
	var postVerb string

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/4/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			res := api.OutletResolution{
				PDUName:          "pdu-test",
				PDUMgmtIP:        "10.0.0.1",
				Outlet:           4,
				Connected:        true,
				DownstreamDevice: "some-device",
				DownstreamSerial: "SN-Y",
				DownstreamPort:   "PSU1",
				Classification:   "sole",
				Warning:          "Outlet 4 may be the only effective power feed.",
				PreflightToken:   "aabbcc.deadbeef0000000000000000000000000000000000000000000000000000000000",
			}
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/4/power",
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			postVerb = body["verb"]
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpPowerOff,
			})
		})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "off", "pdu-test", "4", "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if postVerb != "off" {
		t.Errorf("expected POST verb=off, got: %q", postVerb)
	}
}

// TestPowerOutletCycle_AllowSelfPassedThrough verifies `--allow-self` on cycle
// is sent as allow_self:true in the POST body.
func TestPowerOutletCycle_AllowSelfPassedThrough(t *testing.T) {
	opID := uuid.New()
	var gotAllowSelf any
	postCalled := false

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/5/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			res := resolvedOutlet()
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/5/power",
		func(w http.ResponseWriter, r *http.Request) {
			postCalled = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotAllowSelf = body["allow_self"]
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{ID: opID, Type: model.OpPowerCycle})
		})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "cycle", "pdu-test", "5", "--yes", "--allow-self")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !postCalled {
		t.Fatal("expected POST to be called")
	}
	if gotAllowSelf != true {
		t.Errorf("allow_self in POST body = %v, want true", gotAllowSelf)
	}
}

// TestPowerOutletOn_AllowSelfFlagExists verifies the --allow-self flag is
// registered on `on` (not just cycle).
func TestPowerOutletOn_AllowSelfFlagExists(t *testing.T) {
	opID := uuid.New()
	var gotAllowSelf any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/2/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			res := resolvedOutlet()
			res.Outlet = 2
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/2/power",
		func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotAllowSelf = body["allow_self"]
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{ID: opID, Type: model.OpPowerOn})
		})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "on", "pdu-test", "2", "--yes", "--allow-self")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAllowSelf != true {
		t.Errorf("allow_self in POST body = %v, want true", gotAllowSelf)
	}
}

// TestPowerOutletOff_AllowSelfRejected verifies `--allow-self` on off is
// rejected: the flag is not registered on the off command, so cobra reports an
// unknown flag before any HTTP call.
func TestPowerOutletOff_AllowSelfRejected(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	})

	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "power", "outlet", "off", "pdu-test", "4", "--yes", "--allow-self")
	if err == nil {
		t.Fatal("expected error for --allow-self on off, got nil")
	}
	if !strings.Contains(err.Error(), "allow-self") {
		t.Errorf("expected 'allow-self' in error, got: %v", err)
	}
	if callCount > 0 {
		t.Errorf("expected no HTTP calls when --allow-self rejected on off, got %d", callCount)
	}
}

// TestPowerOutletCycle_JSONOutput verifies --json returns the operation JSON
// instead of the human-readable string.
func TestPowerOutletCycle_JSONOutput(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/7/power/resolve",
		func(w http.ResponseWriter, r *http.Request) {
			res := resolvedOutlet()
			res.Outlet = 7
			json.NewEncoder(w).Encode(res)
		})
	mux.HandleFunc("/api/v1/pdus/pdu-test/outlet/7/power",
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpPowerCycle,
			})
		})

	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "--json", "power", "outlet", "cycle", "pdu-test", "7", "--yes")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var op model.Operation
	if err := json.Unmarshal([]byte(out), &op); err != nil {
		t.Fatalf("expected valid JSON output, got: %q, parse error: %v", out, err)
	}
	if op.ID != opID {
		t.Errorf("expected op ID %s, got %s", opID, op.ID)
	}
}
