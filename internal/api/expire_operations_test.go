package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
)

// TestExpireOperations_PublishesOpsFailedBeforeAPartialError pins the
// sweep tick's publish contract when a sweep partly fails.
//
// Each expiry sweep fails its ops one transaction at a time and keeps going
// past a per-op error, returning BOTH the ops it did fail and the joined
// error. The ops it failed are terminal in the database: no later tick
// selects them again, and a connected worker drops an op only on op_removed
// (or op_resync at reconnect). So the tick must publish op_removed for every
// op a sweep failed, and fire the identity rebuild if any of them wrote
// durable machine state, even when the same sweep also returned an error.
// Otherwise the worker keeps an op that global has failed as active, and an
// active op is what authorizes it to serve a deploy/commission boot.
//
// Each case puts two ops in the state one sweep expires and makes the
// database reject the fail of one of them, standing in for a dropped
// connection, statement timeout or deadlock abort partway through a batch.
func TestExpireOperations_PublishesOpsFailedBeforeAPartialError(t *testing.T) {
	backdate := func(t *testing.T, st *store.Store, opID uuid.UUID, column string, age time.Duration) {
		t.Helper()
		if err := st.DB().Model(&model.Operation{}).Where("id = ?", opID).
			Update(column, time.Now().Add(-age)).Error; err != nil {
			t.Fatalf("backdate %s: %v", column, err)
		}
	}
	claim := func(t *testing.T, st *store.Store, opID uuid.UUID, phase string) {
		t.Helper()
		if _, err := st.ClaimOperation(opID, "worker-1"); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if phase == "" {
			return
		}
		if _, err := st.SetOperationPhase(opID, phase); err != nil {
			t.Fatalf("set phase %s: %v", phase, err)
		}
	}

	cases := []struct {
		sweep string
		// expire puts a fresh pending deploy op into the state this
		// sweep (and no earlier sweep in the tick) expires.
		expire func(t *testing.T, st *store.Store, opID uuid.UUID)
		// wantState is the durable machine state the fail writes, carried
		// on op_removed; "" means none, and then no identity rebuild.
		wantState model.MachineState
	}{
		{
			sweep: "pending",
			expire: func(t *testing.T, st *store.Store, opID uuid.UUID) {
				backdate(t, st, opID, "created_at", 25*time.Hour)
			},
		},
		{
			sweep: "stuck",
			expire: func(t *testing.T, st *store.Store, opID uuid.UUID) {
				claim(t, st, opID, "partitioning")
				backdate(t, st, opID, "claimed_at", 2*time.Hour)
			},
			wantState: model.MachineStateFailed,
		},
		{
			// Claimed with no phase, past the orphan grace but inside
			// the stuck timeout, so the stuck sweep leaves it alone.
			sweep: "orphaned_claim",
			expire: func(t *testing.T, st *store.Store, opID uuid.UUID) {
				claim(t, st, opID, "")
				backdate(t, st, opID, "claimed_at", 5*time.Minute)
			},
			wantState: model.MachineStateFailed,
		},
		{
			sweep: "phone_home",
			expire: func(t *testing.T, st *store.Store, opID uuid.UUID) {
				claim(t, st, opID, "awaiting_phone_home")
				backdate(t, st, opID, "phase_entered_at", 2*time.Hour)
			},
			wantState: model.MachineStateFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.sweep, func(t *testing.T) {
			s, st, inv, pub := opFlowServer(t)
			s.pendingOpTimeout = 24 * time.Hour
			s.opTimeout = 30 * time.Minute
			s.orphanedClaimGrace = 2 * time.Minute
			s.phoneHomeTimeout = 30 * time.Minute

			expiredOp := func() uuid.UUID {
				id := seedMachine(t, st, model.MachineStateCommissioned)
				op, err := st.CreateMachineOp(id, model.OpDeploy, nil, model.MachineStateCommissioned)
				if err != nil {
					t.Fatalf("create deploy op: %v", err)
				}
				tc.expire(t, st, op.ID)
				return op.ID
			}
			okOp := expiredOp()
			badOp := expiredOp()
			rejectOpUpdates(t, st, badOp)

			s.expireOperations()

			// Setup checks: the sweep failed one op and the fault kept
			// the other active, so the sweep returned an outcome AND an
			// error.
			if op, err := st.GetOperation(okOp); err != nil || op.Status != model.OpFailed {
				t.Fatalf("setup: healthy op = %+v, err %v; want failed by the sweep", op, err)
			}
			if op, err := st.GetOperation(badOp); err != nil || op.Status == model.OpFailed {
				t.Fatalf("setup: faulted op = %+v, err %v; want still active", op, err)
			}

			pub.mu.Lock()
			removed := append([]model.Operation(nil), pub.removed...)
			removedSt := append([]model.MachineState(nil), pub.removeSt...)
			pub.mu.Unlock()
			if len(removed) != 1 || removed[0].ID != okOp {
				t.Fatalf("op %s is failed in the database, but the tick published op_removed for %v; want exactly that op (the worker keeps it as an active op)",
					okOp, opIDs(removed))
			}
			if removedSt[0] != tc.wantState {
				t.Errorf("op_removed machine state = %q, want %q", removedSt[0], tc.wantState)
			}
			wantRebuilds := 0
			if tc.wantState != "" {
				wantRebuilds = 1
			}
			if got := len(inv.seen()); got != wantRebuilds {
				t.Errorf("identity rebuilds = %d, want %d", got, wantRebuilds)
			}
		})
	}
}

// rejectOpUpdates makes every UPDATE of one operations row fail, so the
// transaction that tries to fail that op errors and rolls back while other
// ops are unaffected. The trigger outlives the test's TRUNCATE (the testpg
// database is shared), so it is dropped on cleanup.
func rejectOpUpdates(t *testing.T, st *store.Store, opID uuid.UUID) {
	t.Helper()
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION test_reject_op_update() RETURNS trigger AS $$
		 BEGIN RAISE EXCEPTION 'injected fault'; END $$ LANGUAGE plpgsql`,
		fmt.Sprintf(`CREATE TRIGGER test_reject_op_update BEFORE UPDATE ON operations
		 FOR EACH ROW WHEN (OLD.id = '%s') EXECUTE FUNCTION test_reject_op_update()`, opID),
	} {
		if err := st.DB().Exec(q).Error; err != nil {
			t.Fatalf("install fault: %v", err)
		}
	}
	t.Cleanup(func() {
		st.DB().Exec(`DROP TRIGGER IF EXISTS test_reject_op_update ON operations`)
		st.DB().Exec(`DROP FUNCTION IF EXISTS test_reject_op_update()`)
	})
}

func opIDs(ops []model.Operation) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(ops))
	for _, op := range ops {
		ids = append(ids, op.ID)
	}
	return ids
}
