package worker

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// TestCache_ClaimRollbackDoesNotReviveADeployTheAgentFailed
//
// docs/dev/state-machine.md: "The worker's local failure holds until
// global's op_removed for the op ... the host is not booted into the agent
// again and reports for the op are refused as terminal."
//
// The executor's claim bookkeeping breaks that. A claim RPC that gets no
// answer (global restarting, or its claim handler waiting on a database
// that is not answering) can stay in flight for its 30-second timeout, and
// the worker keeps serving meanwhile: a host with a pending deploy that an
// operator power-cycles by hand boots the installer, and its agent can
// fail (the disk safety check, for one) while that claim is still out.
// When the RPC then times out, RollBackClaim writes the op's status back to
// pending without looking at the failure. The op is pending again: the
// worker serves the installer for a deploy its agent has failed, and the
// executor claims it and power-cycles the host as soon as global answers.
// Every phase of that second run is dropped (the failure marker is still
// set), so it can never latch awaiting_phone_home either.
func TestCache_ClaimRollbackDoesNotReviveADeployTheAgentFailed(t *testing.T) {
	const mac = "02:00:00:00:00:01"
	c := NewCache(nil)
	machineID, opID := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateCommissioned, PXEEligible: true,
		Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: mac, Type: "physical"}},
	}}})
	// The deploy as the operator created it: pending.
	c.ApplyOpResync([]model.Operation{{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending, Version: 1,
	}}, nil)

	// The executor picks the op up and sends its claim. No answer yet.
	if pending := c.PendingLifecycleOps(); len(pending) != 1 {
		t.Fatalf("precondition: PendingLifecycleOps = %d ops, want the deploy", len(pending))
	}
	c.MarkOpClaimed(opID.String())

	// Meanwhile the host boots the installer and its agent fails the deploy.
	if err := c.HandleProgressReport(phaseReport(machineID, opID, "safety check")); err != nil {
		t.Fatalf("precondition: phase report refused: %v", err)
	}
	if err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(), MachineID: machineID.String(),
		Failed: true, Phase: "safety check", Error: "disk /dev/nvme0n1 has existing data; refusing to wipe",
	}); err != nil {
		t.Fatalf("precondition: failure report refused: %v", err)
	}
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("precondition: boot action after the agent's failure = %v, want BootLocal (%v)", got, model.BootLocal)
	}

	// The claim RPC times out, and executeOp rolls the claim back.
	c.RollBackClaim(opID.String())

	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Errorf("after the unanswered claim was rolled back, the worker serves boot action %v for the deploy its agent failed; want BootLocal (%v)", got, model.BootLocal)
	}
	if pending := c.PendingLifecycleOps(); len(pending) != 0 {
		t.Errorf("the failed deploy is back in PendingLifecycleOps (%d ops): the executor will claim it and power-cycle the host into the installer again", len(pending))
	}
	if op := c.GetOperation(opID.String()); op == nil || op.Status != model.OpFailed {
		t.Errorf("the op's local status is %v, want it held at failed until global's op_removed", op)
	}
}

// TestExecutor_DoesNotPowerCycleForADeployTheAgentFailedWhileTheClaimWasOut
// is the same failure with a claim that is answered late instead of never:
// global grants it after the agent's failure report. The executor goes on
// from the granted claim to set PXE and power-cycle the host, and marks the
// op running, without looking at the failure the worker is holding.
func TestExecutor_DoesNotPowerCycleForADeployTheAgentFailedWhileTheClaimWasOut(t *testing.T) {
	const mac = "02:00:00:00:00:01"
	c := NewCache(nil)
	machineID, opID := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateCommissioned, PXEEligible: true, BMCIP: "192.0.2.200",
		Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: mac, Type: "physical"}},
	}}})
	pending := model.Operation{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending, Version: 1}
	c.ApplyOpResync([]model.Operation{pending}, nil)

	ctrl := &cycleCountingController{}
	exec := NewExecutor(ExecutorConfig{
		Cache: c,
		Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			return ctrl, nil
		},
		// While the claim RPC is out, the host (rebooted by hand) runs the
		// installer and its agent fails the deploy. Then the claim is
		// granted.
		Claimer: claimerFunc(func(context.Context, uuid.UUID) (*model.Operation, error) {
			if err := c.HandleProgressReport(&model.ProgressReport{
				OperationID: opID.String(), MachineID: machineID.String(),
				Failed: true, Phase: "safety check", Error: "disk /dev/nvme0n1 has existing data; refusing to wipe",
			}); err != nil {
				t.Errorf("precondition: failure report refused: %v", err)
			}
			claimed := pending
			claimed.Status, claimed.ClaimedBy, claimed.Version = model.OpClaimed, "test-worker", 2
			return &claimed, nil
		}),
		WorkerID: "test-worker",
	})
	exec.SetLogger(testLogger())

	exec.executeOp(context.Background(), pending)

	if ctrl.cycles != 0 {
		t.Errorf("the executor power-cycled the host %d time(s) for a deploy whose agent had reported failure before the claim was granted; want 0", ctrl.cycles)
	}
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Errorf("boot action for the failed deploy's host = %v, want BootLocal (%v)", got, model.BootLocal)
	}
}
