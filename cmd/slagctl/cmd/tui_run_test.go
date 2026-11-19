package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/cmd/slagctl/tui"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
)

// TestOpCommandLine_BMCReconcileRotate: the watch header reconstructs a
// rotating bmc_reconcile as `machine bmc-reconcile <host> --rotate-monitor`,
// so an operator can tell a deliberate rotation from a compliance run.
func TestOpCommandLine_BMCReconcileRotate(t *testing.T) {
	op := model.Operation{ID: uuid.New(), Type: model.OpBMCReconcile, Params: []byte(`{"rotate_monitor":true}`)}
	if got := opCommandLine(op, "cpu1.pop3"); got != "machine bmc-reconcile cpu1.pop3 --rotate-monitor" {
		t.Errorf("rotate: %q", got)
	}
	op.Params = []byte(`{"monitor_runtime":{"password":"x"}}`)
	if got := opCommandLine(op, "cpu1.pop3"); got != "machine bmc-reconcile cpu1.pop3" {
		t.Errorf("re-assert: %q", got)
	}
}

// TestWatchExitSummary covers the one-line terminal summary printed after the
// watch TUI tears down: each outcome must be distinguishable and name
// the last-known phase so a timed-out wait reads differently from a completion.
func TestWatchExitSummary(t *testing.T) {
	const opID = "f5cb281e-1111-2222-3333-444455556666"

	cases := []struct {
		name    string
		result  tui.RunResult
		want    []string // substrings that must be present
		exclude []string // substrings that must NOT be present
	}{
		{
			name:   "completed",
			result: tui.RunResult{Outcome: tui.OutcomeCompleted, LastPhase: "completed", Elapsed: 7*time.Minute + 3*time.Second},
			want:   []string{"completed", "7m3s", opID},
		},
		{
			name:   "failed names phase",
			result: tui.RunResult{Outcome: tui.OutcomeFailed, LastPhase: "ipxe_loaded", Elapsed: 4 * time.Minute},
			want:   []string{"FAILED", "ipxe_loaded", "4m0s", opID},
		},
		{
			name:   "incomplete is distinguishable from completed",
			result: tui.RunResult{Outcome: tui.OutcomeIncomplete, LastPhase: "awaiting agent report", Elapsed: 30 * time.Minute},
			want:   []string{"still in progress", "awaiting agent report", "30m0s", "operation watch"},
			// Must not read as a success.
			exclude: []string{"completed in"},
		},
		{
			name:   "incomplete with no phase observed",
			result: tui.RunResult{Outcome: tui.OutcomeIncomplete, LastPhase: "", Elapsed: time.Minute},
			want:   []string{"still in progress", "unknown"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := watchExitSummary(opID, tc.result)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("summary missing %q:\n%s", w, got)
				}
			}
			for _, x := range tc.exclude {
				if strings.Contains(got, x) {
					t.Errorf("summary should not contain %q:\n%s", x, got)
				}
			}
		})
	}
}

// TestFormatTerminalOpReport covers the one-line already-terminal summary:
// op ID, type, machine label, status word, CompletedAt, and the logs command
// hint must all appear in the right shape.
func TestFormatTerminalOpReport(t *testing.T) {
	completedAt := time.Date(2026, 7, 12, 2, 59, 1, 0, time.UTC)
	opID := uuid.MustParse("40bedfa6-27b8-498d-adf1-3c15b1f5c319")
	machineID := uuid.New()

	cases := []struct {
		name    string
		op      model.Operation
		machine string
		want    []string
		exclude []string
	}{
		{
			name: "completed with machine and timestamp",
			op: model.Operation{
				ID:          opID,
				MachineID:   machineID,
				Type:        model.OpCommission,
				Status:      model.OpCompleted,
				CompletedAt: &completedAt,
			},
			machine: "leaf1.pop3",
			want: []string{
				opID.String(),
				"commission",
				"leaf1.pop3",
				"already completed",
				"2026-07-12T02:59:01Z",
				"slagctl machine logs leaf1.pop3 --op " + opID.String(),
			},
		},
		{
			name: "failed with machine",
			op: model.Operation{
				ID:          opID,
				MachineID:   machineID,
				Type:        model.OpDeploy,
				Status:      model.OpFailed,
				CompletedAt: &completedAt,
			},
			machine: "cpu1.pop1",
			want: []string{
				opID.String(),
				"deploy",
				"cpu1.pop1",
				"already failed",
				"slagctl machine logs cpu1.pop1 --op " + opID.String(),
			},
		},
		{
			name: "no machine resolved falls back to UUID in logs hint",
			op: model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpDeploy,
				Status:    model.OpCompleted,
			},
			machine: "",
			want: []string{
				opID.String(),
				"already completed",
				"slagctl machine logs " + machineID.String() + " --op " + opID.String(),
			},
		},
		{
			name: "no CompletedAt omits the timestamp",
			op: model.Operation{
				ID:        opID,
				MachineID: machineID,
				Type:      model.OpCommission,
				Status:    model.OpCompleted,
			},
			machine: "leaf1.pop3",
			want: []string{
				opID.String(),
				"already completed",
			},
			// No (YYYY-...) timestamp section should appear.
			exclude: []string{"2026-"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTerminalOpReport(tc.op, tc.machine)
			// Strip ANSI for plain-string checks.
			plain := ansi.Strip(got)
			for _, w := range tc.want {
				if !strings.Contains(plain, w) {
					t.Errorf("report missing %q:\n%s", w, plain)
				}
			}
			for _, x := range tc.exclude {
				if strings.Contains(plain, x) {
					t.Errorf("report should not contain %q:\n%s", x, plain)
				}
			}
		})
	}
}
