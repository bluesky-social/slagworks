package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
	"github.com/bluesky-social/slagworks/internal/power"
)

// lostReplyClaimer models store.ClaimOperation for a single op: a pending op
// is claimed by the caller, a re-claim by the holder is granted again while
// opstate.CanReclaim holds (the idempotent re-claim branch, B4), and anything
// else is a lost race. Every grant bumps the row's version, as the store
// does. While loseReply is set, the claim commits but its reply is lost, the
// way an RPC timeout or a dropped connection loses it; onCommit runs between
// the commit and the lost reply.
type lostReplyClaimer struct {
	workerID  string
	row       model.Operation // global's row for the op
	calls     int
	loseReply bool
	onCommit  func()
}

func (c *lostReplyClaimer) ClaimOp(_ context.Context, _ uuid.UUID) (*model.Operation, error) {
	c.calls++
	switch {
	case c.row.Status == model.OpPending:
		c.row.Status, c.row.ClaimedBy = model.OpClaimed, c.workerID
	case opstate.CanReclaim(&c.row, c.workerID):
	default:
		return nil, nil
	}
	c.row.Version++
	if c.loseReply {
		c.loseReply = false
		if c.onCommit != nil {
			c.onCommit()
		}
		return nil, errors.New("claim_op: RPC timeout")
	}
	cp := c.row
	return &cp, nil
}

// cycleCountingController is a mockPowerController that counts power cycles.
type cycleCountingController struct {
	mockPowerController
	cycles int
}

func (c *cycleCountingController) PowerCycle(ctx context.Context) error {
	c.cycles++
	return c.mockPowerController.PowerCycle(ctx)
}

// TestExecutor_ClaimCommittedReplyLost_IsReclaimed covers the worker side of
// the lost-claim-reply retry (B4): a claim that COMMITTED on global but whose
// reply was lost is re-claimed and the op driven exactly once, whatever order
// the executor's rollback and global's claimed row reach the cache in. That
// row says claimed by this worker with no phase. If it lands after the
// rollback it replaces the pending mark, and the pending-op readers never
// return a claimed op, so without a record of the unanswered claim the op
// sits claimed until ExpireOrphanedClaims fails it.
func TestExecutor_ClaimCommittedReplyLost_IsReclaimed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rowFirst bool // global's row lands while the claim RPC is still waiting
		asDelta  bool // the row comes as an op_delta, not a reconnect's op_resync
		dropped  bool // an op_resync without the op lands after the rollback, before the row
		midRPC   bool // an op_resync without the op lands while the claim RPC is still waiting
	}{
		// The reconnect's op_resync lands while the claim RPC is still
		// waiting out its deadline; the rollback then rewrites that row.
		{"resync before the rollback", true, false, false, false},
		// The reconnect takes longer than the claim RPC's 30s deadline.
		{"resync after the rollback", false, false, false, false},
		// Global commits the claim after the RPC deadline (a stalled hub
		// or database) and publishes its op_delta after the rollback.
		{"late op_delta after the rollback", false, true, false, false},
		// The op's machine leaves the site's scope while the worker is
		// reconnecting, so the resync leaves the op out; it comes back
		// still claimed by this worker when the machine returns.
		{"op left a resync and came back", false, true, true, false},
		// The same, with the resync that leaves the op out landing while
		// the claim RPC is still waiting: the rollback finds no row.
		{"op left a resync during the claim RPC and came back", false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewCache(nil)
			machineID := uuid.New()
			addMachineToCache(cache, &model.MachineSnapshot{ID: machineID, BMCIP: "10.0.0.100"})
			op := model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpPowerCycle, Status: model.OpPending}
			seedOpsResync(cache, op)

			claimer := &lostReplyClaimer{workerID: "test-worker", row: op, loseReply: true}
			globalRowArrives := func() {
				if tc.asDelta {
					cache.ApplyOpDelta(claimer.row, "")
				} else {
					seedOpsResync(cache, claimer.row)
				}
			}
			if tc.rowFirst {
				claimer.onCommit = globalRowArrives
			}
			if tc.midRPC {
				claimer.onCommit = func() { seedOpsResync(cache) }
			}
			ctrl := &cycleCountingController{}
			exec := NewExecutor(ExecutorConfig{
				Cache: cache,
				Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
					return ctrl, nil
				},
				Claimer:  claimer,
				WorkerID: "test-worker",
			})
			exec.SetLogger(testLogger())
			ctx := context.Background()

			// The claim commits on global, its reply is lost, and the
			// executor rolls its local mark back to pending.
			exec.processOps(ctx)
			if ctrl.cycles != 0 {
				t.Fatal("the executor actuated without a claim reply")
			}
			if tc.dropped {
				seedOpsResync(cache)
			}
			if !tc.rowFirst {
				globalRowArrives()
			}
			// The retry: global grants the re-claim of the op this worker
			// already holds, and the executor drives it.
			exec.processOps(ctx)
			// Global's row echoes back still claimed with no phase (a power
			// op records none) before global has applied the completion. The
			// executor has its answer for this claim and drove the op, so it
			// must not claim or actuate again.
			globalRowArrives()
			exec.processOps(ctx)

			if claimer.calls != 2 {
				t.Errorf("claim RPCs = %d, want 2 (the lost one and one retry)", claimer.calls)
			}
			if ctrl.cycles != 1 {
				t.Errorf("power cycles = %d, want 1", ctrl.cycles)
			}
			_, updates := takeOutbox(cache)
			completed := 0
			for _, u := range updates {
				if u.OperationID == op.ID.String() && u.Status == string(model.OpCompleted) {
					completed++
				}
			}
			if completed != 1 {
				t.Errorf("completed op_updates = %d, want 1", completed)
			}
		})
	}
}
