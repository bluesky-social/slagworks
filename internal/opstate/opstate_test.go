package opstate

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestIsPowerOp(t *testing.T) {
	power := []model.OperationType{model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle}
	for _, ty := range power {
		if !IsPowerOp(ty) {
			t.Errorf("IsPowerOp(%q) = false, want true", ty)
		}
	}
	notPower := []model.OperationType{model.OpDeploy, model.OpCommission, model.OpSetBootDevice}
	for _, ty := range notPower {
		if IsPowerOp(ty) {
			t.Errorf("IsPowerOp(%q) = true, want false", ty)
		}
	}
}

func TestClaimTransition(t *testing.T) {
	tests := []struct {
		opType     model.OperationType
		wantTarget model.MachineState
		wantSource []model.MachineState
	}{
		{
			model.OpCommission, model.MachineStateCommissioning,
			[]model.MachineState{
				model.MachineStateNew, model.MachineStateCommissioned,
				model.MachineStateFailed, model.MachineStateRunning,
			},
		},
		{
			model.OpDeploy, model.MachineStateDeploying,
			[]model.MachineState{
				model.MachineStateCommissioned, model.MachineStateFailed,
				model.MachineStateRunning,
			},
		},
		{model.OpPowerOn, "", nil},
		{model.OpPowerOff, "", nil},
		{model.OpPowerCycle, "", nil},
		{model.OpSetBootDevice, "", nil},
	}
	for _, tc := range tests {
		gotTarget, gotSource := ClaimTransition(tc.opType)
		if gotTarget != tc.wantTarget {
			t.Errorf("ClaimTransition(%q) target = %q, want %q", tc.opType, gotTarget, tc.wantTarget)
		}
		if !slices.Equal(gotSource, tc.wantSource) {
			t.Errorf("ClaimTransition(%q) sources = %v, want %v", tc.opType, gotSource, tc.wantSource)
		}
	}
}

// TestCheckClaimPreconditions_LegalIllegalMatrix walks the full (op type ×
// machine state) matrix and asserts each cell is legal or rejected in line
// with ClaimTransition's allowed-source sets. Locked machines are always
// rejected for lifecycle ops.
func TestCheckClaimPreconditions_LegalIllegalMatrix(t *testing.T) {
	allStates := []model.MachineState{
		model.MachineStateNew, model.MachineStateCommissioning,
		model.MachineStateCommissioned, model.MachineStateDeploying,
		model.MachineStateRunning, model.MachineStateFailed,
	}
	mid := uuid.New()

	for _, opType := range []model.OperationType{model.OpCommission, model.OpDeploy} {
		_, restingStates := ClaimTransition(opType)
		for _, state := range allStates {
			wantLegal := slices.Contains(restingStates, state)

			// Unlocked: legal iff state is an allowed source.
			err := CheckClaimPreconditions(mid, opType, state, false, restingStates)
			if wantLegal && err != nil {
				t.Errorf("%s from %q (unlocked): got err %v, want legal", opType, state, err)
			}
			if !wantLegal && err == nil {
				t.Errorf("%s from %q (unlocked): got legal, want rejection", opType, state)
			}

			// Locked: always rejected, even from an otherwise-legal source.
			if lockedErr := CheckClaimPreconditions(mid, opType, state, true, restingStates); lockedErr == nil {
				t.Errorf("%s from %q (locked): got legal, want rejection", opType, state)
			}
		}
	}
}

// TestCheckClaimPreconditions_PowerOpsAlwaysPass: an empty resting set (power
// ops) means the op doesn't gate on machine state, so any state and even a
// locked machine passes — power ops are the stuck-machine recovery path.
func TestCheckClaimPreconditions_PowerOpsAlwaysPass(t *testing.T) {
	mid := uuid.New()
	for _, locked := range []bool{false, true} {
		if err := CheckClaimPreconditions(mid, model.OpPowerCycle, model.MachineStateRunning, locked, nil); err != nil {
			t.Errorf("power op with empty resting set (locked=%v): got err %v, want pass", locked, err)
		}
	}
}

func TestCanReclaim(t *testing.T) {
	const worker = "worker-a"
	base := func() *model.Operation {
		return &model.Operation{Status: model.OpClaimed, ClaimedBy: worker, Phase: ""}
	}
	if !CanReclaim(base(), worker) {
		t.Error("claimed by same worker, no phase: want re-claimable")
	}

	other := base()
	other.ClaimedBy = "worker-b"
	if CanReclaim(other, worker) {
		t.Error("claimed by different worker: want NOT re-claimable")
	}

	withPhase := base()
	withPhase.Phase = "partitioning"
	if CanReclaim(withPhase, worker) {
		t.Error("phase recorded: want NOT re-claimable (actuation started)")
	}

	pending := base()
	pending.Status = model.OpPending
	if CanReclaim(pending, worker) {
		t.Error("pending status: want NOT re-claimable")
	}
}

func TestStateColumns_AutoLockPolicy(t *testing.T) {
	tests := []struct {
		target     model.MachineState
		wantLocked any // nil = key absent
	}{
		{model.MachineStateRunning, true},
		{model.MachineStateFailed, false},
		{model.MachineStateNew, nil},
		{model.MachineStateCommissioned, nil},
		{model.MachineStateCommissioning, nil},
		{model.MachineStateDeploying, nil},
	}
	for _, tc := range tests {
		cols := StateColumns(tc.target)
		if cols["state"] != tc.target {
			t.Errorf("StateColumns(%q) state = %v, want %q", tc.target, cols["state"], tc.target)
		}
		got, present := cols["locked"]
		if tc.wantLocked == nil {
			if present {
				t.Errorf("StateColumns(%q): locked present (%v), want absent", tc.target, got)
			}
			continue
		}
		if !present || got != tc.wantLocked {
			t.Errorf("StateColumns(%q) locked = %v (present=%v), want %v", tc.target, got, present, tc.wantLocked)
		}
	}
}

func TestFailStateOutcome(t *testing.T) {
	tests := []struct {
		name              string
		wasPending        bool
		opType            model.OperationType
		hasCommissionData bool
		wantChange        bool
		wantTarget        model.MachineState
	}{
		{"pending never touches state", true, model.OpDeploy, false, false, ""},
		{"pending commission never touches state", true, model.OpCommission, false, false, ""},
		{"power op never touches state", false, model.OpPowerCycle, false, false, ""},
		// bmc_reconcile runs against in-service hosts; a failure must not
		// flip a running machine to failed (and auto-unlock it).
		{"bmc_reconcile never touches state", false, model.OpBMCReconcile, true, false, ""},
		// set_boot_device is an out-of-band BMC op too; a failed boot-set
		// on a running host must not mark it failed and unlock it.
		{"set_boot_device never touches state", false, model.OpSetBootDevice, true, false, ""},
		{"initial commission reverts to new", false, model.OpCommission, false, true, model.MachineStateNew},
		{"recommission goes to failed", false, model.OpCommission, true, true, model.MachineStateFailed},
		{"deploy failure goes to failed", false, model.OpDeploy, true, true, model.MachineStateFailed},
		{"deploy failure (no commission data) still failed", false, model.OpDeploy, false, true, model.MachineStateFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FailStateOutcome(tc.wasPending, tc.opType, tc.hasCommissionData)
			if got.ChangeState != tc.wantChange {
				t.Errorf("ChangeState = %v, want %v", got.ChangeState, tc.wantChange)
			}
			if got.ChangeState && got.Target != tc.wantTarget {
				t.Errorf("Target = %q, want %q", got.Target, tc.wantTarget)
			}
		})
	}
}

func TestStatusSets(t *testing.T) {
	// Terminal and active sets must be disjoint and together cover all five
	// statuses — a status that is neither would be invisible to both the CAS
	// guards and the active-op index.
	all := []model.OperationStatus{
		model.OpPending, model.OpClaimed, model.OpRunning,
		model.OpCompleted, model.OpFailed,
	}
	for _, st := range all {
		inTerminal := slices.Contains(TerminalStatuses, st)
		inActive := slices.Contains(ActiveStatuses, st)
		if inTerminal == inActive {
			t.Errorf("status %q: terminal=%v active=%v, want exactly one", st, inTerminal, inActive)
		}
	}
}
