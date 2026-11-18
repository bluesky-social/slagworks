package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
)

// TestMachineOps_ListsAllByDefault pins that `machine ops <machine>` GETs
// the machine-scoped operation list and shows every op type when no filter
// flag is given.
func TestMachineOps_ListsAllByDefault(t *testing.T) {
	machineID := uuid.New()
	now := time.Now()
	deployID := uuid.New()
	abortID := uuid.New()
	ops := []model.Operation{
		{ID: deployID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpCompleted, CreatedAt: now},
		{ID: abortID, MachineID: machineID, Type: "abort", Status: model.OpCompleted, CreatedAt: now.Add(-time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("machine"); got != machineID.String() {
			t.Errorf("machine filter = %q, want %q", got, machineID)
		}
		_ = json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "ops", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deploy") {
		t.Errorf("expected deploy op in output, got:\n%s", out)
	}
	if !strings.Contains(out, "abort") {
		t.Errorf("default (no filter) must include all op types, got:\n%s", out)
	}
}

// TestMachineOps_PowerFilter pins --power: power-actuating ops (power_* +
// deploy/commission) appear; unrelated types (abort) are dropped.
func TestMachineOps_PowerFilter(t *testing.T) {
	machineID := uuid.New()
	now := time.Now()
	ops := []model.Operation{
		{ID: uuid.New(), MachineID: machineID, Type: model.OpPowerCycle, Status: model.OpFailed, CreatedAt: now},
		{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpCompleted, CreatedAt: now.Add(-time.Hour)},
		{ID: uuid.New(), MachineID: machineID, Type: "abort", Status: model.OpCompleted, CreatedAt: now.Add(-2 * time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "ops", "--power", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "power_cycle") {
		t.Errorf("expected power_cycle in --power output, got:\n%s", out)
	}
	if !strings.Contains(out, "deploy") {
		t.Errorf("deploy power-cycles the host, must appear under --power, got:\n%s", out)
	}
	if strings.Contains(out, "abort") {
		t.Errorf("--power must filter out non-power op types, got:\n%s", out)
	}
}

// TestMachineOps_DeployFilter pins --deploy: only deploy/commission appear.
func TestMachineOps_DeployFilter(t *testing.T) {
	machineID := uuid.New()
	now := time.Now()
	ops := []model.Operation{
		{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpCompleted, CreatedAt: now},
		{ID: uuid.New(), MachineID: machineID, Type: model.OpCommission, Status: model.OpCompleted, CreatedAt: now.Add(-time.Hour)},
		{ID: uuid.New(), MachineID: machineID, Type: model.OpPowerCycle, Status: model.OpCompleted, CreatedAt: now.Add(-2 * time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "ops", "--deploy", machineID.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deploy") || !strings.Contains(out, "commission") {
		t.Errorf("expected deploy and commission under --deploy, got:\n%s", out)
	}
	if strings.Contains(out, "power_cycle") {
		t.Errorf("--deploy must filter out power ops, got:\n%s", out)
	}
}

// TestMachineOps_PowerAndDeployMutuallyExclusive pins the guard.
func TestMachineOps_PowerAndDeployMutuallyExclusive(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "machine", "ops", "--power", "--deploy", uuid.New().String())
	if err == nil {
		t.Fatal("expected error when both --power and --deploy are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually-exclusive error, got: %v", err)
	}
}

// TestMachineCreds_ResolvesAnyDevice pins the any-NetBox-device behavior:
// `machine creds <ident>` passes the raw positional verbatim to the server's
// /devices/<ident>/creds endpoint WITHOUT routing it through the
// registered-machine resolver. A plain PDU name (no machine lookup, no UUID)
// must reach the endpoint and render its creds — that's how an operator
// fetches a BMC-less switch's PDU creds.
func TestMachineCreds_ResolvesAnyDevice(t *testing.T) {
	const pduName = "pdu-0102-left"
	var hitMachines bool
	mux := http.NewServeMux()
	// If creds wrongly routed through resolveMachine, it would hit
	// /api/v1/machines first; flag that so the test fails loudly.
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		hitMachines = true
		_ = json.NewEncoder(w).Encode([]model.Machine{})
	})
	mux.HandleFunc("/api/v1/devices/"+pduName+"/creds", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.DeviceCreds{
			Device:   pduName,
			NetBoxID: 42,
			Pop:      "pop1",
			Kind:     "pdu",
			MgmtIP:   "10.0.0.9",
			Username: "apc",
			Password: "secret",
		})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "machine", "creds", pduName)
	if err != nil {
		t.Fatal(err)
	}
	if hitMachines {
		t.Error("machine creds must NOT route through the registered-machine resolver (any-device lookup)")
	}
	if !strings.Contains(out, pduName) || !strings.Contains(out, "10.0.0.9") {
		t.Errorf("expected device creds rendered, got:\n%s", out)
	}
	if !strings.Contains(out, "pdu") {
		t.Errorf("expected pdu kind in output, got:\n%s", out)
	}
}

// TestMachineLogs_SolRejectsEnvelopeFlags pins the symmetric flag guard:
// envelope-stream filters (--since etc.) error when combined with --sol
// rather than being silently ignored.
func TestMachineLogs_SolRejectsEnvelopeFlags(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "machine", "logs", "--sol", "--since", "1h", uuid.New().String())
	if err == nil {
		t.Fatal("expected error combining --sol with --since")
	}
	if !strings.Contains(err.Error(), "cannot be combined with --sol") {
		t.Errorf("expected envelope-flag-with-sol error, got: %v", err)
	}
}

// TestMachineLogs_EnvelopeRejectsSolFlags pins the inverse guard: SOL-only
// flags (--from-start etc.) error in the default envelope mode.
func TestMachineLogs_EnvelopeRejectsSolFlags(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "machine", "logs", "--from-start", uuid.New().String())
	if err == nil {
		t.Fatal("expected error using --from-start without --sol")
	}
	if !strings.Contains(err.Error(), "requires --sol") {
		t.Errorf("expected requires-sol error, got: %v", err)
	}
}

// TestSlagctlTreeShape pins the command tree: the consolidated
// machine-scoped commands exist, and the dropped top-level nouns/commands
// (device, console, logs) plus `machine power history` are GONE. Guards
// against accidental re-add of the old spellings.
func TestSlagctlTreeShape(t *testing.T) {
	root := cmd.NewRootCmd()

	// Removed top-level commands.
	for _, name := range []string{"device", "console", "logs"} {
		if c := findChild(root, name); c != nil {
			t.Errorf("top-level %q must be removed", name)
		}
	}

	machine := findChild(root, "machine")
	if machine == nil {
		t.Fatal("machine command not found")
	}
	// New / kept machine subcommands.
	for _, name := range []string{"logs", "ops", "creds"} {
		if c := findChild(machine, name); c == nil {
			t.Errorf("machine %q must exist", name)
		}
	}
	// machine logs must carry --sol (replaces root console), --follow, --since.
	logs := findChild(machine, "logs")
	for _, f := range []string{"sol", "follow", "since"} {
		if logs.Flags().Lookup(f) == nil {
			t.Errorf("machine logs must expose --%s", f)
		}
	}
	// machine ops must carry --power and --deploy filters.
	ops := findChild(machine, "ops")
	for _, f := range []string{"power", "deploy"} {
		if ops.Flags().Lookup(f) == nil {
			t.Errorf("machine ops must expose --%s", f)
		}
	}

	// `machine power history` is dropped — power is an op type, covered by
	// `machine ops --power`.
	power := findChild(machine, "power")
	if power == nil {
		t.Fatal("machine power not found")
	}
	if c := findChild(power, "history"); c != nil {
		t.Error("machine power history must be removed (use machine ops --power)")
	}

	// The top-level operation noun stays.
	if findChild(root, "operation") == nil {
		t.Error("top-level operation noun must be kept")
	}
}

func findChild(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
