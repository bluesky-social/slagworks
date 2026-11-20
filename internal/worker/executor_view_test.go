package worker

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

// claimerFunc adapts a function to the Claimer interface.
type claimerFunc func(ctx context.Context, opID uuid.UUID) (*model.Operation, error)

func (f claimerFunc) ClaimOp(ctx context.Context, opID uuid.UUID) (*model.Operation, error) {
	return f(ctx, opID)
}

// TestExecutor_NoClaimForMachineThatLeftTheViewMidPass pins the claim-time
// half of the view-membership rule (pendingPowerOps doc, opstate_cache.go):
// the executor must never claim an op whose machine is not in its view.
//
// A pass ranges over the pending list it built when it started. Driving one
// op holds a BMC session for seconds, and an identity snapshot applied
// meanwhile can drop a later op's machine from the view (the machine moved to
// another site, or the site's scope was narrowed). Claiming that op wins it
// at a hub whose membership check has not caught up (it works from the
// slug's last pushed snapshot); the worker then finds MachineByID nil and
// reports the live op failed with "machine not in cache". A hub that has
// caught up declines the claim instead, which leaves this worker's local
// mark at claimed for an op that is still pending. The op must instead stay
// pending, here and on global, for the site that owns the machine now.
//
// One subtest per pending bucket: both loops in processOps range over a list
// built once per pass.
func TestExecutor_NoClaimForMachineThatLeftTheViewMidPass(t *testing.T) {
	for _, opType := range []model.OperationType{model.OpPowerOn, model.OpDeploy} {
		t.Run(string(opType), func(t *testing.T) {
			cache := NewCache(nil)
			a := model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.0.0.1"}
			b := model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.0.0.2"}
			cache.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{a, b}})
			opA := model.Operation{ID: uuid.New(), MachineID: a.ID, Type: opType, Status: model.OpPending}
			opB := model.Operation{ID: uuid.New(), MachineID: b.ID, Type: opType, Status: model.OpPending}
			cache.ApplyOpResync([]model.Operation{opA, opB}, nil)
			opsByID := map[uuid.UUID]model.Operation{opA.ID: opA, opB.ID: opB}

			// The pass drives whichever op it reaches first (map order).
			// While that op's controller is open, a snapshot arrives that
			// keeps only the driven machine: the other one left this site.
			var outOfViewClaims []uuid.UUID
			var claimed []uuid.UUID
			var stayed uuid.UUID
			seq := int64(1)
			exec := NewExecutor(ExecutorConfig{
				Cache: cache,
				Factory: func(_ context.Context, m *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
					keep := a
					if m.ID == b.ID {
						keep = b
					}
					stayed = keep.ID
					seq++
					cache.ApplySnapshot(&model.SiteSnapshot{SeqNum: seq, Machines: []model.MachineSnapshot{keep}})
					return &mockPowerController{}, nil
				},
				Claimer: claimerFunc(func(_ context.Context, id uuid.UUID) (*model.Operation, error) {
					op := opsByID[id]
					claimed = append(claimed, id)
					if cache.MachineByID(op.MachineID) == nil {
						outOfViewClaims = append(outOfViewClaims, id)
					}
					op.Status = model.OpClaimed
					return &op, nil
				}),
				WorkerID: "worker-1",
			})
			exec.SetLogger(testLogger())
			exec.processOps(context.Background())

			if len(outOfViewClaims) != 0 {
				t.Fatalf("the executor claimed %d op(s) whose machine had left its view since the pass began; "+
					"it then reports that live op failed as machine-not-in-cache", len(outOfViewClaims))
			}
			if len(claimed) != 1 || opsByID[claimed[0]].MachineID != stayed {
				t.Fatalf("claims: got %v, want exactly the op on the machine that stayed in view", claimed)
			}
			left := opA
			if stayed == a.ID {
				left = opB
			}
			if got := cache.GetOperation(left.ID.String()); got == nil || got.Status != model.OpPending {
				t.Errorf("op on the machine that left the view: got %+v, want it still pending locally", got)
			}
			_, updates := cache.DrainOutbox()
			for _, u := range updates {
				if u.OperationID == left.ID.String() {
					t.Errorf("queued op_update %+v for the op whose machine left the view; want none", u)
				}
			}
		})
	}
}
