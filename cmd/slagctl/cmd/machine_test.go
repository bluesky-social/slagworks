package cmd_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func TestMachineList_StateFilter(t *testing.T) {
	var capturedURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		json.NewEncoder(w).Encode([]model.Machine{})
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "machine", "list", "--state", "commissioned")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedURL, "state=commissioned") {
		t.Errorf("expected state=commissioned in URL, got: %q", capturedURL)
	}
}

func TestMachineList_StateIcon(t *testing.T) {
	machines := []model.Machine{
		{ID: uuid.New(), NetBoxSerial: "SN-1", State: "commissioned", CreatedAt: time.Now()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(machines)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "list")
	if err != nil {
		t.Fatal(err)
	}
	// ● icon should appear for commissioned state
	if !strings.Contains(out, "●") {
		t.Errorf("expected ● icon for commissioned state, got: %q", out)
	}
}

func TestMachineList_RendersHostnameAndExplicitNone(t *testing.T) {
	machines := []model.Machine{
		{ID: uuid.New(), Hostname: "cpu1.pop1", NetBoxSerial: "SN-1", State: "running", CreatedAt: time.Now()},
		{ID: uuid.New(), NetBoxSerial: "SN-2", State: "new", CreatedAt: time.Now()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(machines)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"HOSTNAME", "cpu1.pop1", "(none)"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got: %q", want, out)
		}
	}
}

func TestMachineList_InvalidState(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "machine", "list", "--state", "borked")
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
	if !strings.Contains(err.Error(), "invalid state") {
		t.Errorf("expected invalid state error, got: %v", err)
	}
}

func TestMachineShow_RendersStepsTable(t *testing.T) {
	machineID := uuid.New()
	commissionData := json.RawMessage(`{
        "system": {"manufacturer":"ASUS","serial_number":"S1"},
        "steps": [
            {"name":"dmidecode_system","status":"ok","duration_ms":42},
            {"name":"lldp_neighbors","status":"failed","duration_ms":30000,"error":"no neighbor data"},
            {"name":"smartctl_disks","status":"partial","duration_ms":1200,"warning":"sda smart not supported"}
        ]
    }`)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String(), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(model.Machine{
			ID:             machineID,
			NetBoxSerial:   "SN-X",
			State:          model.MachineStateCommissioned,
			CommissionData: commissionData,
			CreatedAt:      time.Now(),
		})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "show", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Commission steps",
		"dmidecode_system",
		"lldp_neighbors",
		"smartctl_disks",
		"42ms",
		"30000ms",
		"no neighbor data",
		"sda smart not supported",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output, got: %s", want, out)
		}
	}
}

// TestMachineShow_EscapesTerminalControlsInSteps: commission_data is the
// commissioning target's own report, so step names, statuses and
// error/warning strings are host-controlled. The steps table must show
// escape sequences in them as text, not hand them to the terminal.
func TestMachineShow_EscapesTerminalControlsInSteps(t *testing.T) {
	machineID := uuid.New()
	commissionData := json.RawMessage(`{"steps":[
        {"name":"inventory\u001b]52;c;cHduZWQ=\u0007","status":"failed","duration_ms":5,"error":"disk\u001b[2Jgone"},
        {"name":"lldp","status":"ok\u001b[1A","duration_ms":7,"warning":"no\rneighbor\u009b2J"}
    ]}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String(), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(model.Machine{
			ID:             machineID,
			NetBoxSerial:   "SN-X",
			State:          model.MachineStateCommissioned,
			CommissionData: commissionData,
			CreatedAt:      time.Now(),
		})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "show", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	assertNoTerminalControls(t, out)
	for _, want := range []string{`inventory\x1b]52;c;cHduZWQ=\x07`, `disk\x1b[2Jgone`, `ok\x1b[1A`, `no\rneighbor\u009b2J`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the escaped text %q in output, got: %s", want, out)
		}
	}
}

// TestMachineShow_NoStepsArrayOmitsTable ensures older commission_data
// without a `steps` field doesn't produce a stray "Commission steps"
// header. Earlier commissions stored only the inventory; treating
// missing steps as all-ok means the table is simply absent.
func TestMachineShow_NoStepsArrayOmitsTable(t *testing.T) {
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String(), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(model.Machine{
			ID:             machineID,
			NetBoxSerial:   "SN-old",
			State:          model.MachineStateCommissioned,
			CommissionData: json.RawMessage(`{"system":{"manufacturer":"ASUS"}}`),
			CreatedAt:      time.Now(),
		})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "show", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Commission steps") {
		t.Errorf("expected no steps table, got: %s", out)
	}
}

func TestMachineCommission_PrintsHint(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/commission", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: opID, MachineID: machineID, Type: model.OpCommission})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "commission", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "slagctl operation watch") {
		t.Errorf("expected watch hint in output, got: %q", out)
	}
	if !strings.Contains(out, opID.String()) {
		t.Errorf("expected op ID in output, got: %q", out)
	}
}

// TestMachinePower_CycleSendsAction verifies the verb-after-power
// shape: `machine power cycle --machine-id <id>` POSTs {action: cycle}
// to /power. Replaces the old `power <id> pxe` semantics; boot-device
// management has moved out of the steady-state power path per the BMC
// runtime contract.
func TestMachinePower_CycleSendsAction(t *testing.T) {
	var capturedBody []byte
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/power", func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: uuid.New(), Type: model.OpPowerCycle})
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "machine", "power", "cycle", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("invalid body: %v", err)
	}
	if body["action"] != "cycle" {
		t.Errorf("expected action=cycle, got: %q", body["action"])
	}
	// boot_device is no longer set from the power path.
	if _, ok := body["boot_device"]; ok {
		t.Errorf("boot_device must not be present on power requests, got: %q", body["boot_device"])
	}
}

// TestMachinePower_StatusGETsAndRenders pins the read-path shape:
// `machine power status <id>` GETs /machines/:id/power (no POST, no
// op creation) and renders "<machine>: <State> (<transport>)" from
// the response.
func TestMachinePower_StatusGETsAndRenders(t *testing.T) {
	machineID := uuid.New()
	var gotMethod string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/power", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		json.NewEncoder(w).Encode(map[string]string{
			"machine_id": machineID.String(),
			"state":      "on",
			"transport":  "redfish",
		})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "power", "status", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET (read, no op), got %s", gotMethod)
	}
	if !strings.Contains(out, "On") {
		t.Errorf("expected power state in output, got: %q", out)
	}
	if !strings.Contains(out, "redfish") {
		t.Errorf("expected transport in output, got: %q", out)
	}
	if strings.Contains(out, "operation") {
		t.Errorf("status must not mention an operation, got: %q", out)
	}
}

// TestMachinePower_StatusSurfacesServerError pins error plumbing: the
// API's explanation (e.g. the 422 no-credential body or the 503
// no-worker body) must reach the operator, prefixed with the machine.
func TestMachinePower_StatusSurfacesServerError(t *testing.T) {
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/power", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{
			"message": "refusing power status: no BMC credential could be resolved for this machine",
		})
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "machine", "power", "status", machineID.String())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "no BMC credential") {
		t.Errorf("server explanation must reach the operator, got: %v", err)
	}
	if !strings.Contains(err.Error(), "power status") {
		t.Errorf("error should name the verb, got: %v", err)
	}
}

// TestMachinePower_StatusHasNoViaFlag pins that the status subcommand
// does not expose --via: the read path always asks the BMC (a PDU
// outlet read answers a different question — feed state, not box
// power) and the server picks the transport.
func TestMachinePower_StatusHasNoViaFlag(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "machine", "power", "status", "--via", "pdu", uuid.New().String())
	if err == nil {
		t.Fatal("expected unknown-flag error for --via on status")
	}
	if !strings.Contains(err.Error(), "via") {
		t.Errorf("expected flag error mentioning via, got: %v", err)
	}
}

func TestMachinePower_RejectsUnknownSubcommand(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	out, err := execCmd(t, cl, "machine", "power", "reboot", uuid.New().String())
	if err == nil {
		t.Fatalf("expected error for unknown power subcommand, got out=%q", out)
	}
}

// TestMachineBMCReconcile_CreatesOpAndPrintsID: `machine bmc-reconcile
// <machine>` POSTs (no body: re-assert the stored monitor password) to
// /machines/:id/bmc-reconcile and prints the created op id plus the watch
// hint, like the other op-creating commands.
func TestMachineBMCReconcile_CreatesOpAndPrintsID(t *testing.T) {
	machineID := uuid.New()
	opID := uuid.New()
	var method, body string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/bmc-reconcile", func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: opID, MachineID: machineID, Type: model.OpBMCReconcile})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "bmc-reconcile", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if body != "" {
		t.Errorf("without --rotate-monitor the request has no body, got %q", body)
	}
	if !strings.Contains(out, opID.String()) {
		t.Errorf("expected op ID in output, got: %q", out)
	}
	if !strings.Contains(out, "slagctl operation watch "+opID.String()) {
		t.Errorf("expected watch hint in output, got: %q", out)
	}
}

// TestMachineBMCReconcile_RotateMonitor: --rotate-monitor asks global for a
// freshly generated monitor password ({"rotate_monitor":true}), and its help
// warns that the gateways' exporter creds go stale until the oob-gw
// playbook re-renders them.
func TestMachineBMCReconcile_RotateMonitor(t *testing.T) {
	machineID := uuid.New()
	opID := uuid.New()
	var body map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/bmc-reconcile", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: opID, MachineID: machineID, Type: model.OpBMCReconcile})
	})
	cl := testServer(t, mux)
	if _, err := execCmd(t, cl, "machine", "bmc-reconcile", "--rotate-monitor", machineID.String()); err != nil {
		t.Fatal(err)
	}
	if body["rotate_monitor"] != true {
		t.Errorf("body = %v, want rotate_monitor: true", body)
	}

	sub, _, err := cmd.NewRootCmd().Find([]string{"machine", "bmc-reconcile"})
	if err != nil {
		t.Fatal(err)
	}
	flag := sub.Flags().Lookup("rotate-monitor")
	if flag == nil {
		t.Fatal("no --rotate-monitor flag")
	}
	if !strings.Contains(flag.Usage, "ansible-playbook oob-gw.yaml --tags sel_archiver,ipmi_exporter") {
		t.Errorf("--rotate-monitor help must name the re-render command, got %q", flag.Usage)
	}
}

// TestMachineBMCReconcile_JSON: --json prints the created op verbatim.
func TestMachineBMCReconcile_JSON(t *testing.T) {
	machineID := uuid.New()
	opID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/bmc-reconcile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: opID, MachineID: machineID, Type: model.OpBMCReconcile})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "--json", "machine", "bmc-reconcile", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	var op model.Operation
	if err := json.Unmarshal([]byte(out), &op); err != nil {
		t.Fatalf("--json output is not an operation: %v (%q)", err, out)
	}
	if op.ID != opID || op.Type != model.OpBMCReconcile {
		t.Errorf("op = %+v", op)
	}
}

// TestMachineBMCReconcile_SurfacesRefusal: a 422 (no admin entry, missing
// monitor secret) must fail the command with the server's reason.
func TestMachineBMCReconcile_SurfacesRefusal(t *testing.T) {
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/bmc-reconcile", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		json.NewEncoder(w).Encode(map[string]string{"message": "refusing bmc-reconcile: pop/pop2/bmc-creds-monitor does not exist"})
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "machine", "bmc-reconcile", machineID.String())
	if err == nil || !strings.Contains(err.Error(), "bmc-creds-monitor") {
		t.Fatalf("expected the server's refusal to surface, got %v", err)
	}
}

func TestMachineLock(t *testing.T) {
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/lock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(model.Machine{ID: machineID, NetBoxSerial: "SN-1", Locked: true})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "lock", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Locked") {
		t.Errorf("expected Locked in output, got: %q", out)
	}
}

func TestMachineUnlock(t *testing.T) {
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/unlock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(model.Machine{ID: machineID, NetBoxSerial: "SN-1", Locked: false})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "unlock", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Unlocked") {
		t.Errorf("expected Unlocked in output, got: %q", out)
	}
}

// TestMachineDeploy_NoUnlockFlag pins the operator guardrail that unlock
// must be a bare, standalone command: there is no `deploy --unlock` (or any
// unlock-combining flag) that would let a single invocation both unlock and
// redeploy an in-service host. Redeploying a running machine is always two
// deliberate steps — `machine unlock`, then `machine deploy --destructive`.
// If someone adds an unlock-combining flag to deploy, this fails.
func TestMachineDeploy_NoUnlockFlag(t *testing.T) {
	root := cmd.NewRootCmd()
	var machine *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "machine" {
			machine = c
			break
		}
	}
	if machine == nil {
		t.Fatal("machine command not found under root")
	}
	var deploy *cobra.Command
	for _, c := range machine.Commands() {
		if c.Name() == "deploy" {
			deploy = c
			break
		}
	}
	if deploy == nil {
		t.Fatal("deploy command not found under machine")
	}
	for _, name := range []string{"unlock", "force-unlock", "with-unlock"} {
		if f := deploy.Flags().Lookup(name); f != nil {
			t.Errorf("deploy must not expose an unlock-combining flag, found --%s", name)
		}
	}
}

// TestMachineTarget_RequiresExactlyOneArg verifies the positional
// arg shape on every machine-targeted command. Catches regressions
// where someone adds a new command and forgets bindMachineArg.
func TestMachineTarget_RequiresExactlyOneArg(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	if _, err := execCmd(t, cl, "machine", "show"); err == nil {
		t.Error("expected error when no positional arg is given")
	}
	if _, err := execCmd(t, cl, "machine", "show", "a", "b"); err == nil {
		t.Error("expected error when more than one positional arg is given")
	}
}

func TestMachineReadOnlyHelp_AdvertisesOneTarget(t *testing.T) {
	root := cmd.NewRootCmd()
	machine, _, err := root.Find([]string{"machine"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"preflight", "match"} {
		t.Run(name, func(t *testing.T) {
			target, _, err := machine.Find([]string{name})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(target.UseLine(), "<machine>"); got != 1 {
				t.Errorf("UseLine() advertises %d machine targets, want 1: %q", got, target.UseLine())
			}
			if err := target.Args(target, []string{"host-a"}); err != nil {
				t.Errorf("one target rejected: %v", err)
			}
			if err := target.Args(target, []string{"host-a", "host-b"}); err == nil {
				t.Error("two targets accepted; command must remain single-host")
			}
		})
	}
}

// TestMachineHostnameFlowsToURL verifies that a hostname positional
// arg resolves to the machine's UUID (via the new ?hostname= search
// endpoint) and puts the UUID in the URL slot. The dot in the
// hostname is what triggers shape-detection past the serial branch.
func TestMachineHostnameFlowsToURL(t *testing.T) {
	const hostname = "kworker-l34-1.pop1"
	machineID := uuid.New()
	var hostnameLookup int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("hostname") {
		case hostname:
			hostnameLookup++
			json.NewEncoder(w).Encode([]model.Machine{
				{ID: machineID, NetBoxSerial: "SN-1", Hostname: hostname},
			})
		case "":
			// serial lookup path — return empty so we fall through to hostname
			json.NewEncoder(w).Encode([]model.Machine{})
		default:
			t.Errorf("unexpected hostname query: %q", r.URL.Query().Get("hostname"))
		}
	})
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/lock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		json.NewEncoder(w).Encode(model.Machine{ID: machineID, NetBoxSerial: "SN-1", Locked: true})
	})
	cl := testServer(t, mux)
	if _, err := execCmd(t, cl, "machine", "lock", hostname); err != nil {
		t.Fatal(err)
	}
	if hostnameLookup != 1 {
		t.Errorf("expected exactly one ?hostname= lookup, got %d", hostnameLookup)
	}
}

func TestMachineList_LockIndicator(t *testing.T) {
	machines := []model.Machine{
		{ID: uuid.New(), NetBoxSerial: "SN-1", State: "commissioned", Locked: true, CreatedAt: time.Now()},
		{ID: uuid.New(), NetBoxSerial: "SN-2", State: "new", Locked: false, CreatedAt: time.Now()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(machines)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "locked") {
		t.Errorf("expected 'locked' indicator for SN-1, got: %q", out)
	}
	if !strings.Contains(out, "LOCK") {
		t.Errorf("expected LOCK column header, got: %q", out)
	}
}

func TestMachineDeploy_PrintsHint(t *testing.T) {
	opID := uuid.New()
	machineID := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/deploy", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: opID, MachineID: machineID, Type: model.OpDeploy})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "deploy", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "slagctl operation watch") {
		t.Errorf("expected watch hint in output, got: %q", out)
	}
}

// TestMachineDeploy_WipeAllDisksImpliesDestructive pins the request
// body shape: --wipe-all-disks alone must send BOTH wipe_all_disks
// and destructive (the API rejects the former without the latter).
func TestMachineDeploy_WipeAllDisksImpliesDestructive(t *testing.T) {
	machineID := uuid.New()
	var gotBody map[string]bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/deploy", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding deploy body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "deploy", "--wipe-all-disks", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !gotBody["wipe_all_disks"] {
		t.Errorf("expected wipe_all_disks in body, got %v", gotBody)
	}
	if !gotBody["destructive"] {
		t.Errorf("expected destructive implied by --wipe-all-disks, got %v", gotBody)
	}
	if !strings.Contains(out, "wiping ALL disks") {
		t.Errorf("expected wipe warning in output, got: %q", out)
	}
}
