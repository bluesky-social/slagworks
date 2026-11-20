package worker

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// The tests in this file pin the rule that a failure the agent reported
// holds until global's op_removed for the op (docs/dev/state-machine.md,
// "Timeouts and phase-based expiration"; keepLocalFailure in
// opstate_cache.go) against the executor's own claim bookkeeping, not only
// against the op stream.
//
// The agent can fail a deploy the executor has not finished claiming. While
// a deploy is pending, a commissioned or failed host that PXEs (an operator
// reboots it by hand) gets the installer on the pending-op boot clause, and
// the claim RPC can wait out its 30-second deadline on a global that is
// restarting or whose database is not answering. processOps also works
// through the pending list it built when its pass began, so the agent can
// fail an op in that list before the executor reaches it.
//
// The executor's bookkeeping used to write over such a failure: the claim
// attempt set the op to claimed, the rollback of an unanswered claim set it
// back to pending, and the running mark after a granted claim set it to
// running. The op was active again, so the worker served the installer for
// a deploy its agent had failed, and the executor claimed it and
// power-cycled the host into it. The failure marker still blocked every
// phase of that second run, so it could never latch awaiting_phone_home
// either.

// agentFailureMAC is the boot MAC of the deploy target in these tests.
const agentFailureMAC = "02:00:00:00:00:01"

// pendingDeploy seeds a commissioned machine with a deploy op that global
// holds pending (no claim has landed). It returns the cache, the op, and a
// function that has the host's agent, running on the pending-op boot clause,
// report the deploy failed.
func pendingDeploy(t *testing.T) (*Cache, model.Operation, func()) {
	t.Helper()
	c := NewCache(nil)
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateCommissioned, PXEEligible: true, BMCIP: "192.0.2.10",
		Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: agentFailureMAC, Type: "physical"}},
	}}})
	op := model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending,
		Version: 1, Params: abortTestParams(t, model.OpParams{}),
	}
	seedOpsResync(c, op)
	if got := c.BootActionForMAC(agentFailureMAC); got != model.BootDeploy {
		t.Fatalf("precondition: boot action for the pending deploy = %d, want BootDeploy (%d)", got, model.BootDeploy)
	}
	fail := func() {
		t.Helper()
		for _, r := range []*model.ProgressReport{
			phaseReport(machineID, op.ID, "safety check"),
			{
				OperationID: op.ID.String(), MachineID: machineID.String(),
				Failed: true, Phase: "safety check", Error: "disk has existing data; refusing to wipe",
			},
		} {
			if err := c.HandleProgressReport(r); err != nil {
				t.Fatalf("precondition: agent report %+v refused: %v", r, err)
			}
		}
		if got := c.BootActionForMAC(agentFailureMAC); got != model.BootLocal {
			t.Fatalf("precondition: boot action after the agent's failure = %d, want BootLocal (%d)", got, model.BootLocal)
		}
	}
	return c, op, fail
}

// assertFailureHeld checks that the deploy is still failed locally: the
// host boots its disk, and the executor's pending readers do not return the
// op for another claim.
func assertFailureHeld(t *testing.T, c *Cache, op model.Operation) {
	t.Helper()
	if got := c.BootActionForMAC(agentFailureMAC); got != model.BootLocal {
		t.Errorf("boot action = %d, want BootLocal (%d): the worker serves the installer for a deploy its agent failed", got, model.BootLocal)
	}
	if pending := c.PendingLifecycleOps(); len(pending) != 0 {
		t.Errorf("PendingLifecycleOps returned %d op(s): the executor will claim the failed deploy and power-cycle the host into the installer", len(pending))
	}
	if got := c.GetOperation(op.ID.String()); got == nil || got.Status != model.OpFailed {
		t.Errorf("op = %+v, want status failed until global's op_removed", got)
	}
}

// TestClaimBookkeepingKeepsAgentFailure: none of the executor's local status
// writes revives a deploy whose agent reported failure before the write.
func TestClaimBookkeepingKeepsAgentFailure(t *testing.T) {
	cases := []struct {
		name string
		run  func(c *Cache, opID string, fail func())
	}{
		// The claim RPC gets no answer within its deadline, and executeOp
		// rolls the claim back.
		{"rollback of an unanswered claim", func(c *Cache, opID string, fail func()) {
			c.MarkOpClaimed(opID)
			fail()
			c.RollBackClaim(opID)
		}},
		// executeOp reaches the op from a pass list built while it was
		// still pending.
		{"claim attempt", func(c *Cache, opID string, fail func()) {
			fail()
			c.MarkOpClaimed(opID)
		}},
		// Global grants the claim late, and runLifecycleOp marks the op
		// running.
		{"running mark after a late grant", func(c *Cache, opID string, fail func()) {
			c.MarkOpClaimed(opID)
			fail()
			c.MarkOpStatus(opID, model.OpRunning)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, op, fail := pendingDeploy(t)
			tc.run(c, op.ID.String(), fail)
			assertFailureHeld(t, c, op)
		})
	}
}

// TestExecutor_NoHostWriteAfterTheAgentFailedTheDeploy: once the agent's
// failure lands while the executor is claiming or driving the deploy, the
// executor makes no further claim and no further write to the host or its
// BMC, as when global's op_removed lands mid-run
// (TestExecutor_OpRemovedMidRun_NoFurtherHostWrites), and the deploy stays
// failed. The agent's failure op_update is already queued for global, so
// the executor reports nothing of its own.
func TestExecutor_NoHostWriteAfterTheAgentFailedTheDeploy(t *testing.T) {
	cases := []struct {
		name     string
		during   string // the call during which the failure lands; ClaimOp is the claim RPC
		claimErr error  // the claim RPC's error; nil grants the claim
	}{
		{"claim granted after the failure", "ClaimOp", nil},
		{"claim timed out after the failure", "ClaimOp", errors.New("claim_op: RPC timeout")},
		{"failure during the serial gate", "ChassisSerial", nil},
		{"failure while reading the power state", "PowerStatus", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, op, fail := pendingDeploy(t)
			log := &callLog{}
			landed := false
			log.during = func(name string) {
				if name == tc.during && !landed {
					landed = true
					fail()
				}
			}
			exec := NewExecutor(ExecutorConfig{
				Cache: c,
				Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
					return loggingCtrl{log: log}, nil
				},
				Claimer: claimerFunc(func(context.Context, uuid.UUID) (*model.Operation, error) {
					log.call("ClaimOp")
					if tc.claimErr != nil {
						return nil, tc.claimErr
					}
					claimed := op
					claimed.Status, claimed.ClaimedBy, claimed.Version = model.OpClaimed, "test-worker", 2
					return &claimed, nil
				}),
				WorkerID: "test-worker",
			})
			exec.SetLogger(testLogger())

			// The second pass is where a claim rolled back to pending would
			// be retried.
			exec.processOps(context.Background())
			exec.processOps(context.Background())

			if !landed {
				t.Fatalf("the executor never reached %s (calls: %v)", tc.during, log.calls)
			}
			for _, call := range log.calls[slices.Index(log.calls, tc.during)+1:] {
				if call == "ClaimOp" || slices.Contains(hostWrites, call) {
					t.Errorf("the executor issued %s after the agent reported the deploy failed during %s (calls: %v)", call, tc.during, log.calls)
				}
			}
			assertFailureHeld(t, c, op)
		})
	}
}

// TestExecutor_NoClaimForADeployTheAgentFailedSinceThePassBegan: processOps
// works through the pending list it built when its pass began, and driving
// the ops ahead of a deploy (a hung claim RPC alone takes 30 seconds) gives
// the deploy's agent time to fail it first. When the executor reaches the
// deploy it must leave it unclaimed: a granted claim would move the machine
// to deploying on global for an op this worker already holds failed, and
// the op would end on global as an orphaned claim instead of with the
// agent's reason whenever the agent's failure op_update is lost.
func TestExecutor_NoClaimForADeployTheAgentFailedSinceThePassBegan(t *testing.T) {
	c, op, fail := pendingDeploy(t)
	log := &callLog{}
	exec := NewExecutor(ExecutorConfig{
		Cache: c,
		Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			log.call("open controller")
			return loggingCtrl{log: log}, nil
		},
		Claimer: claimerFunc(func(context.Context, uuid.UUID) (*model.Operation, error) {
			log.call("ClaimOp")
			claimed := op
			claimed.Status, claimed.ClaimedBy, claimed.Version = model.OpClaimed, "test-worker", 2
			return &claimed, nil
		}),
		WorkerID: "test-worker",
	})
	exec.SetLogger(testLogger())

	pass := c.PendingLifecycleOps()
	if len(pass) != 1 {
		t.Fatalf("precondition: pending lifecycle ops = %d, want the deploy", len(pass))
	}
	fail()
	for _, stale := range pass {
		exec.executeOp(context.Background(), stale)
	}

	if len(log.calls) != 0 {
		t.Errorf("the executor claimed or drove a deploy its agent failed after the pass began (calls: %v)", log.calls)
	}
	assertFailureHeld(t, c, op)
}
