package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
)

func TestOperationList_MachineFilter(t *testing.T) {
	machineID := uuid.New().String()
	var capturedURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		json.NewEncoder(w).Encode([]model.Operation{})
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "operation", "list", "--machine", machineID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedURL, "machine="+machineID) {
		t.Errorf("expected machine filter in URL, got: %q", capturedURL)
	}
}

func TestOperationList_StatusColors(t *testing.T) {
	ops := []model.Operation{
		{ID: uuid.New(), MachineID: uuid.New(), Type: model.OpCommission, Status: model.OpCompleted, CreatedAt: time.Now()},
		{ID: uuid.New(), MachineID: uuid.New(), Type: model.OpDeploy, Status: model.OpFailed, CreatedAt: time.Now()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)
	// Both ops here are terminal (completed/failed); the default list is
	// active-only, so we need --all to see them.
	out, err := execCmd(t, cl, "operation", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	// Both icons should be present
	if !strings.Contains(out, "●") {
		t.Errorf("expected ● for completed, got: %q", out)
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("expected ✗ for failed, got: %q", out)
	}
}

// TestOperationList_ActiveOnlyByDefault: the bare `operation list`
// shows only non-terminal ops; completed/failed are hidden until --all.
func TestOperationList_ActiveOnlyByDefault(t *testing.T) {
	running := uuid.New()
	completed := uuid.New()
	failed := uuid.New()
	pending := uuid.New()
	now := time.Now()
	ops := []model.Operation{
		{ID: running, MachineID: uuid.New(), Type: model.OpDeploy, Status: model.OpRunning, CreatedAt: now},
		{ID: completed, MachineID: uuid.New(), Type: model.OpDeploy, Status: model.OpCompleted, CreatedAt: now},
		{ID: failed, MachineID: uuid.New(), Type: model.OpDeploy, Status: model.OpFailed, CreatedAt: now},
		{ID: pending, MachineID: uuid.New(), Type: model.OpCommission, Status: model.OpPending, CreatedAt: now},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)

	out, err := execCmd(t, cl, "operation", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, running.String()) {
		t.Errorf("expected running op %s in default list", running)
	}
	if !strings.Contains(out, pending.String()) {
		t.Errorf("expected pending op %s in default list", pending)
	}
	if strings.Contains(out, completed.String()) {
		t.Errorf("completed op %s should be hidden by default", completed)
	}
	if strings.Contains(out, failed.String()) {
		t.Errorf("failed op %s should be hidden by default", failed)
	}

	// --all surfaces everything.
	outAll, err := execCmd(t, cl, "operation", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []uuid.UUID{running, completed, failed, pending} {
		if !strings.Contains(outAll, id.String()) {
			t.Errorf("--all should include op %s", id)
		}
	}
}

// TestOperationList_FreshestAtBottom: rows are ordered by last-activity
// ascending so the most-recently-active op is the last line of the table.
func TestOperationList_FreshestAtBottom(t *testing.T) {
	now := time.Now()
	stale := uuid.New()
	fresh := uuid.New()
	// Both running. `stale` was created earlier AND entered its phase
	// earlier; `fresh` changed phase most recently. Server returns
	// created_at desc, so without our re-sort `fresh` (newer created)
	// would already be first — we instead key on PhaseEnteredAt to prove
	// the activity-based ordering, by making the STALE op the newer one
	// by creation but the OLDER one by phase activity.
	ops := []model.Operation{
		// newer creation, but last phase change was long ago
		{ID: stale, MachineID: uuid.New(), Type: model.OpDeploy, Status: model.OpRunning,
			CreatedAt: now.Add(-1 * time.Minute), PhaseEnteredAt: now.Add(-10 * time.Minute)},
		// older creation, but just changed phase
		{ID: fresh, MachineID: uuid.New(), Type: model.OpDeploy, Status: model.OpRunning,
			CreatedAt: now.Add(-30 * time.Minute), PhaseEnteredAt: now.Add(-2 * time.Second)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "operation", "list")
	if err != nil {
		t.Fatal(err)
	}
	staleIdx := strings.Index(out, stale.String())
	freshIdx := strings.Index(out, fresh.String())
	if staleIdx < 0 || freshIdx < 0 {
		t.Fatalf("both ops should be present; stale=%d fresh=%d\n%s", staleIdx, freshIdx, out)
	}
	if freshIdx < staleIdx {
		t.Errorf("freshest op should be at the BOTTOM (after stale); got fresh at %d before stale at %d\n%s",
			freshIdx, staleIdx, out)
	}
}

// TestOperationList_EmptyShowsMessage: an empty op list prints a plain
// "no operations" line instead of a lonely header row, which reads as a
// broken query.
func TestOperationList_EmptyShowsMessage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.Operation{})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "operation", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no operations") {
		t.Errorf("expected 'no operations' message, got: %q", out)
	}
	if strings.Contains(out, "MACHINE") {
		t.Errorf("empty list should not print a header row, got: %q", out)
	}
}

// TestOperationList_ShowsMachineName: the enriched MachineName from the
// server is what shows in the MACHINE column, not the raw machine UUID.
func TestOperationList_ShowsMachineName(t *testing.T) {
	machineID := uuid.New()
	ops := []model.Operation{
		{ID: uuid.New(), MachineID: machineID, MachineName: "cpu1.pop1",
			Type: model.OpDeploy, Status: model.OpRunning, CreatedAt: time.Now()},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ops)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "operation", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cpu1.pop1") {
		t.Errorf("expected machine name in output, got: %q", out)
	}
	// The full machine UUID should NOT appear — only the op ID is a UUID.
	if strings.Contains(out, machineID.String()) {
		t.Errorf("machine UUID should not appear when a name is present, got: %q", out)
	}
}

func TestOperationShow_JSON(t *testing.T) {
	opID := uuid.New()
	op := model.Operation{ID: opID, Type: model.OpCommission, Status: model.OpCompleted}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations/"+opID.String(), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(op)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "operation", "show", opID.String())
	if err != nil {
		t.Fatal(err)
	}
	var got model.Operation
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, out)
	}
}

func TestOperationWatch_CompletedImmediately(t *testing.T) {
	opID := uuid.New()
	op := model.Operation{
		ID:     opID,
		Type:   model.OpCommission,
		Status: model.OpCompleted,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations/"+opID.String(), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(op)
	})
	cl := testServer(t, mux)
	// Non-TTY path (tests don't have a terminal) — should exit 0
	_, err := execCmd(t, cl, "operation", "watch", opID.String(), "--timeout", "5s")
	if err != nil {
		t.Fatalf("expected no error for completed op, got: %v", err)
	}
}

func TestOperationWatch_FailedExitsError(t *testing.T) {
	opID := uuid.New()
	op := model.Operation{
		ID:     opID,
		Type:   model.OpCommission,
		Status: model.OpFailed,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations/"+opID.String(), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(op)
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "operation", "watch", opID.String(), "--timeout", "5s")
	if err == nil {
		t.Fatal("expected error for failed operation")
	}
	if !strings.Contains(err.Error(), "failed") {
		t.Errorf("expected failed error, got: %v", err)
	}
}

func TestOperationShow_InvalidUUID(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "operation", "show", "not-a-uuid")
	if err == nil {
		t.Fatal("expected error for invalid UUID")
	}
}

// TestOperationList_MachineFilterByHostname: passing a hostname to
// --machine resolves through the API and forwards the resolved UUID
// to /operations?machine=<uuid>.
func TestOperationList_MachineFilterByHostname(t *testing.T) {
	const hostname = "cpu1.pop1"
	machineID := uuid.New()
	var seenMachine string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("hostname") {
		case hostname:
			_ = json.NewEncoder(w).Encode([]model.Machine{
				{ID: machineID, NetBoxSerial: "S-1", Hostname: hostname},
			})
		case "":
			_ = json.NewEncoder(w).Encode([]model.Machine{})
		default:
			t.Errorf("unexpected hostname: %q", r.URL.Query().Get("hostname"))
		}
	})
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		seenMachine = r.URL.Query().Get("machine")
		_ = json.NewEncoder(w).Encode([]model.Operation{})
	})
	cl := testServer(t, mux)
	if _, err := execCmd(t, cl, "operation", "list", "--machine", hostname); err != nil {
		t.Fatal(err)
	}
	if seenMachine != machineID.String() {
		t.Errorf("expected /operations?machine=%s, got %q", machineID, seenMachine)
	}
}

// TestOperationWatch_ResolvesMachineToLatestActiveOp covers the
// machine-resolution affordance: `slagctl operation watch cpu1.pop1` finds the
// most recent non-terminal op for the resolved machine, then watches
// it. We return a completed status on the immediate op fetch so the
// watch returns 0 without WS plumbing.
func TestOperationWatch_ResolvesMachineToLatestActiveOp(t *testing.T) {
	const hostname = "cpu1.pop1"
	machineID := uuid.New()
	completedOp := uuid.New()
	runningOp := uuid.New()
	now := time.Now()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("hostname") {
		case hostname:
			_ = json.NewEncoder(w).Encode([]model.Machine{
				{ID: machineID, NetBoxSerial: "S-1", Hostname: hostname},
			})
		case "":
			_ = json.NewEncoder(w).Encode([]model.Machine{})
		default:
			t.Errorf("unexpected hostname: %q", r.URL.Query().Get("hostname"))
		}
	})
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.Operation{
			{ID: completedOp, MachineID: machineID, Status: model.OpCompleted, CreatedAt: now.Add(-2 * time.Hour)},
			{ID: runningOp, MachineID: machineID, Status: model.OpRunning, CreatedAt: now.Add(-5 * time.Minute)},
		})
	})
	mux.HandleFunc("/api/v1/operations/"+runningOp.String(), func(w http.ResponseWriter, r *http.Request) {
		// The watch loop picks runningOp; on first fetch we report
		// it terminal so watchOperation returns 0 without WS work.
		_ = json.NewEncoder(w).Encode(model.Operation{
			ID: runningOp, MachineID: machineID, Status: model.OpCompleted,
		})
	})
	cl := testServer(t, mux)
	if _, err := execCmd(t, cl, "operation", "watch", hostname, "--timeout", "5s"); err != nil {
		t.Fatalf("expected resolver to land on runningOp and exit clean, got: %v", err)
	}
}

// TestOperationWatch_NoActiveOpErrors: when every op for a machine is
// terminal, the resolver fails with a clear "no active operation"
// message rather than picking a stale completed op.
func TestOperationWatch_NoActiveOpErrors(t *testing.T) {
	const hostname = "cpu1.pop1"
	machineID := uuid.New()
	now := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("hostname") {
		case hostname:
			_ = json.NewEncoder(w).Encode([]model.Machine{
				{ID: machineID, NetBoxSerial: "S-1", Hostname: hostname},
			})
		case "":
			_ = json.NewEncoder(w).Encode([]model.Machine{})
		default:
			t.Errorf("unexpected hostname: %q", r.URL.Query().Get("hostname"))
		}
	})
	mux.HandleFunc("/api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]model.Operation{
			{ID: uuid.New(), MachineID: machineID, Status: model.OpCompleted, CreatedAt: now},
		})
	})
	cl := testServer(t, mux)
	_, err := execCmd(t, cl, "operation", "watch", hostname, "--timeout", "5s")
	if err == nil {
		t.Fatal("expected error when machine has no active op")
	}
	if !strings.Contains(err.Error(), "no active operation") {
		t.Errorf("expected 'no active operation' in error, got: %v", err)
	}
}
