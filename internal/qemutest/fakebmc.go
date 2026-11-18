//go:build integration

package qemutest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// FakeBMC credentials. Fixed test credentials: the site-worker power
// factory passes the op's BMCRuntime envelope creds into
// power.NewBMCController, and this server enforces Basic Auth against
// these exact values, so a test proves the real credential envelope
// flowed through bmclib into the fake BMC. gofish must fetch the service
// root before it installs Basic Auth on its client, so that one discovery
// endpoint is public; every Systems read or mutation enforces these values.
const (
	FakeBMCUser = "root"
	//nolint:gosec // G101: test-fixture fixed credential, not a real secret.
	FakeBMCPass = "fakebmc-pw"
)

// FakeBMC is a TLS Redfish BMC backed by QMP. It is the deterministic,
// integration-tested replacement for the historical Redfish-to-QMP fake
// (which was deleted); the current in-band BMCSim (IPMI/SOL) is unrelated
// and must not be repurposed for this path.
//
// It speaks the minimal Redfish surface bmclib's gofish provider actually
// requires: the service root, the Systems collection, GET/PATCH of
// System 1, and the ComputerSystem.Reset action. Every power-state read
// derives from a FRESH QMP query-status — the fake never caches a
// "powered on" flag, so a PowerStatus read proves what QEMU actually
// reports rather than what an earlier request set.
//
// Chassis representation: QEMU's paused/running run-state is the
// deterministic stand-in for chassis off/on. A genuinely stopped QEMU
// process cannot be turned on (QMP cont only resumes a paused VM), and
// system_powerdown depends on the guest's ACPI cooperating before an OS
// boots, so pause/resume is the only reliable test signalling.
type FakeBMC struct {
	srv       *httptest.Server
	host      string
	sock      string
	q         *QMP
	qmu       sync.Mutex
	mu        sync.Mutex
	theSystem systemState
	actions   []BMCAction
}

// systemState is the mutable Redfish System view. PowerState is
// intentionally NOT persisted here — it is always re-derived from QMP.
type systemState struct {
	bootTarget string // last BootSourceOverrideTarget accepted (Pxe/Hdd/None)
}

// BMCActionKind classifies a recorded request for test assertions.
type BMCActionKind int

const (
	// BMCActionReset is a ComputerSystem.Reset POST carrying a
	// ForceRestart/ForceOff/On reset type.
	BMCActionReset BMCActionKind = iota
	// BMCActionSetBoot is a PATCH of the System Boot object.
	BMCActionSetBoot
	// BMCActionPowerState is a GET of the System power state.
	BMCActionPowerState
)

// BMCAction is one recorded FakeBMC request.
type BMCAction struct {
	Kind       BMCActionKind
	ResetType  string // BMCActionReset
	BootTarget string // BMCActionSetBoot (Pxe/Hdd)
	State      string // BMCActionPowerState (On/Off)
	At         time.Time
}

// NewFakeBMC starts a TLS Redfish BMC bound to 127.0.0.1 on a random
// port. qmpSocketPath is the VM's dedicated FakeBMC QMP socket (see
// VMConfig.BMCQMPPath), which the fake dials lazily on first use — so
// the fake may be constructed before vm.Start, and the QMP link is
// established by the time bmclib's first request arrives.
func NewFakeBMC(t *testing.T, qmpSocketPath string) *FakeBMC {
	t.Helper()
	f := &FakeBMC{
		sock:      qmpSocketPath,
		theSystem: systemState{bootTarget: "None"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/redfish/v1", f.handleServiceRoot)
	// gofish uses common.DefaultServiceRoot, which includes the trailing
	// slash. Keep the slashless form too because real BMC probes use both.
	mux.HandleFunc("/redfish/v1/", f.handleServiceRoot)
	mux.HandleFunc("/redfish/v1/Systems", f.handleSystems)
	mux.HandleFunc("/redfish/v1/Systems/1", f.handleSystem)
	mux.HandleFunc("/redfish/v1/Systems/1/Actions/ComputerSystem.Reset", f.handleReset)
	f.srv = httptest.NewTLSServer(mux)
	f.host = strings.TrimPrefix(f.srv.URL, "https://") // 127.0.0.1:<random port>
	t.Cleanup(f.Close)
	return f
}

// Host returns the BMC's loopback host:port (e.g. "127.0.0.1:43473").
// This is what a test sets as the machine's BMCIP so the factory's
// BuildBMCDialOptions splits it into host + Redfish port.
func (f *FakeBMC) Host() string { return f.host }

// HTTPClient returns a client that trusts the httptest server certificate.
// It is used only for focused authentication-boundary assertions.
func (f *FakeBMC) HTTPClient() *http.Client { return f.srv.Client() }

// Actions returns a snapshot of every recorded request.
func (f *FakeBMC) Actions() []BMCAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BMCAction, len(f.actions))
	copy(out, f.actions)
	return out
}

// Close shuts down the TLS server. Registered via t.Cleanup.
func (f *FakeBMC) Close() {
	if f.srv != nil {
		f.srv.Close()
		f.srv = nil
	}
	f.qmu.Lock()
	if f.q != nil {
		_ = f.q.Close()
		f.q = nil
	}
	f.qmu.Unlock()
}

// qmpClient returns the FakeBMC's dedicated QMP connection, dialing it
// (with retry) the first time it is needed.
func (f *FakeBMC) qmpClient() (*QMP, error) {
	f.qmu.Lock()
	defer f.qmu.Unlock()
	if f.q != nil {
		return f.q, nil
	}
	q, err := newQMP(f.sock)
	if err != nil {
		return nil, err
	}
	f.q = q
	return q, nil
}

// powerState derives the Redfish PowerState from a fresh QMP
// query-status. running -> On; paused and the initial -S prelaunch
// state -> Off. Any other run-state is rejected so a test can't
// accidentally green-light on a weird state.
func (f *FakeBMC) powerState() (string, error) {
	q, err := f.qmpClient()
	if err != nil {
		return "", err
	}
	status, err := q.QueryStatus()
	if err != nil {
		return "", err
	}
	switch status {
	case "running":
		return "On", nil
	case "paused", "prelaunch":
		return "Off", nil
	default:
		return "", fmt.Errorf("unexpected QEMU run-state %q", status)
	}
}

func (f *FakeBMC) record(a BMCAction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.At = time.Now()
	f.actions = append(f.actions, a)
}

// requireAuth enforces Basic Auth on every Redfish request.
func (f *FakeBMC) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if !ok || u != FakeBMCUser || p != FakeBMCPass {
		w.Header().Set("WWW-Authenticate", `Basic realm="FakeBMC"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// handleServiceRoot serves GET /redfish/v1.
func (f *FakeBMC) handleServiceRoot(w http.ResponseWriter, r *http.Request) {
	// The slash form is a subtree pattern in net/http's ServeMux. Reject
	// unknown descendants explicitly so an unimplemented Redfish resource
	// cannot accidentally receive a plausible service-root response.
	if r.URL.Path != "/redfish/v1" && r.URL.Path != "/redfish/v1/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// gofish fetches the root before setupClientAuth. Real Redfish services
	// commonly expose this discovery document without authentication; all
	// linked resources remain protected by requireAuth.
	writeJSON(w, map[string]any{
		"@odata.type":    "#ServiceRoot.v1_12_0.ServiceRoot",
		"@odata.id":      "/redfish/v1",
		"Id":             "RootService",
		"Name":           "FakeBMC Root Service",
		"RedfishVersion": "1.12.0",
		"UUID":           "f47ac10b-58cc-4372-a567-0e02b2c3d479",
		"Systems":        map[string]any{"@odata.id": "/redfish/v1/Systems"},
	})
}

// handleSystems serves GET /redfish/v1/Systems.
func (f *FakeBMC) handleSystems(w http.ResponseWriter, r *http.Request) {
	if !f.requireAuth(w, r) {
		return
	}
	writeJSON(w, map[string]any{
		"@odata.type":         "#ComputerSystemCollection.ComputerSystemCollection",
		"@odata.id":           "/redfish/v1/Systems",
		"Name":                "Computer System Collection",
		"Members@odata.count": 1,
		"Members": []map[string]any{
			{"@odata.id": "/redfish/v1/Systems/1"},
		},
	})
}

// handleSystem serves GET/PATCH /redfish/v1/Systems/1.
func (f *FakeBMC) handleSystem(w http.ResponseWriter, r *http.Request) {
	if !f.requireAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, err := f.powerState()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.record(BMCAction{Kind: BMCActionPowerState, State: st})
		writeJSON(w, f.systemJSON(st))
	case http.MethodPatch:
		var body struct {
			Boot struct {
				BootSourceOverrideTarget string `json:"BootSourceOverrideTarget"`
			} `json:"Boot"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("bad PATCH body: %v", err), http.StatusBadRequest)
			return
		}
		target, err := f.applyBootOverride(body.Boot.BootSourceOverrideTarget)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.record(BMCAction{Kind: BMCActionSetBoot, BootTarget: target})
		writeJSON(w, map[string]any{"@odata.id": "/redfish/v1/Systems/1"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// applyBootOverride maps a Redfish BootSourceOverrideTarget to a QMP
// HMP boot_set target and issues it against the real VM. Pxe -> "n"
// (network), Hdd -> "c" (first disk). HTTP success is only returned
// after the QMP command succeeds.
func (f *FakeBMC) applyBootOverride(target string) (string, error) {
	hmp := ""
	switch target {
	case "Pxe":
		hmp = "n"
	case "Hdd":
		hmp = "c"
	case "None":
		hmp = ""
	default:
		return "", fmt.Errorf("unsupported boot override target %q", target)
	}
	if hmp != "" {
		q, err := f.qmpClient()
		if err != nil {
			return "", err
		}
		if err := q.SetBootOrder(hmp); err != nil {
			return "", fmt.Errorf("QMP boot_set %s: %w", hmp, err)
		}
	}
	f.mu.Lock()
	f.theSystem.bootTarget = target
	f.mu.Unlock()
	return target, nil
}

// handleReset serves POST ComputerSystem.Reset.
func (f *FakeBMC) handleReset(w http.ResponseWriter, r *http.Request) {
	if !f.requireAuth(w, r) {
		return
	}
	var body struct {
		ResetType string `json:"ResetType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, fmt.Sprintf("bad Reset body: %v", err), http.StatusBadRequest)
		return
	}
	q, err := f.qmpClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var actionErr error
	switch body.ResetType {
	case "ForceRestart":
		actionErr = q.Reset()
	case "ForceOff":
		actionErr = q.Stop()
	case "On":
		actionErr = q.Cont()
	default:
		http.Error(w, fmt.Sprintf("unsupported ResetType %q", body.ResetType), http.StatusBadRequest)
		return
	}
	if actionErr != nil {
		http.Error(w, fmt.Sprintf("QMP reset %s failed: %v", body.ResetType, actionErr), http.StatusInternalServerError)
		return
	}
	f.record(BMCAction{Kind: BMCActionReset, ResetType: body.ResetType})
	writeJSON(w, map[string]any{})
}

// systemJSON renders the Redfish System for a given power state, with
// the boot-override fields, reset target, and allowable reset values
// bmclib/gofish actually require.
func (f *FakeBMC) systemJSON(powerState string) map[string]any {
	f.mu.Lock()
	target := f.theSystem.bootTarget
	f.mu.Unlock()
	return map[string]any{
		"@odata.type":  "#ComputerSystem.v1_12_0.ComputerSystem",
		"@odata.id":    "/redfish/v1/Systems/1",
		"Id":           "1",
		"Name":         "fake-bmc-system",
		"Manufacturer": "FakeBMC",
		"Model":        "Integration Test System",
		"PowerState":   powerState,
		"Actions": map[string]any{
			"#ComputerSystem.Reset": map[string]any{
				"target": "/redfish/v1/Systems/1/Actions/ComputerSystem.Reset",
				"ResetType@Redfish.AllowableValues": []string{
					"On", "ForceOff", "ForceRestart", "GracefulShutdown",
				},
			},
		},
		"Boot": map[string]any{
			"BootSourceOverrideEnabled": "Once",
			"BootSourceOverrideMode":    "UEFI",
			"BootSourceOverrideTarget":  target,
			"BootSourceOverrideTarget@Redfish.AllowableValues": []string{
				"None", "Pxe", "Hdd",
			},
		},
	}
}
