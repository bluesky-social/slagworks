package worker

import (
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func phoneHomeReport(machineID, opID uuid.UUID) *model.ProgressReport {
	return &model.ProgressReport{OperationID: opID.String(), MachineID: machineID.String(), Event: "phone_home"}
}

// requireNoOutbox fails if the cache queued anything for global.
func requireNoOutbox(t *testing.T, c *Cache, when string) {
	t.Helper()
	if obs, updates := takeOutbox(c); len(obs) != 0 || len(updates) != 0 {
		t.Fatalf("%s queued side effects: observations=%+v updates=%+v", when, obs, updates)
	}
}

// TestHandleProgressReport_PhoneHomeOnRunningMachineIsNotAckedBlind is the
// regression for a phone-home answered with success and dropped. The
// installed OS phones home until its first 2xx and never again, so a 2xx is
// a promise that the deploy's completion is on its way to global. The retry
// acknowledgement used to key on the machine being held as running, whatever
// operation the report named: a standby worker whose view predated a redeploy
// (running machine, deploy op not yet at awaiting_phone_home or not held at
// all) acknowledged the new install's only phone-home and queued nothing, and
// global failed the healthy deploy at the phone-home timeout.
func TestHandleProgressReport_PhoneHomeOnRunningMachineIsNotAckedBlind(t *testing.T) {
	cases := []struct {
		name string
		seed func(c *Cache, machineID, opID uuid.UUID)
	}{
		{
			// The reported reproduction: a resynced, empty op set.
			name: "operation not held",
			seed: func(c *Cache, _, _ uuid.UUID) { c.ApplyOpResync(nil, nil) },
		},
		{
			name: "operation held pending, before the claim",
			seed: func(c *Cache, machineID, opID uuid.UUID) {
				seedOpsResync(c, model.Operation{
					ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending, Version: 1,
				})
			},
		},
		{
			// An earlier deploy of the same machine phoned home here and
			// was completed; that says nothing about the redeploy.
			name: "an earlier operation's phone-home was recorded",
			seed: func(c *Cache, machineID, _ uuid.UUID) {
				earlier := uuid.New()
				seedOpsResync(c, model.Operation{
					ID: earlier, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed,
					Phase: "awaiting_phone_home", Version: 1,
				})
				if err := c.HandleProgressReport(phoneHomeReport(machineID, earlier)); err != nil {
					t.Fatalf("earlier deploy's phone-home: %v", err)
				}
				c.ApplyOpRemoved(earlier.String(), machineID.String(), 2, model.MachineStateRunning)
				takeOutbox(c)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(nil)
			machineID, opID := uuid.New(), uuid.New()
			c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
				ID: machineID, State: model.MachineStateRunning,
			}}})
			tc.seed(c, machineID, opID)

			if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err == nil {
				t.Fatalf("phone-home for operation %s was acknowledged although this worker never recorded it "+
					"and holds no completion for it: the installed OS stops retrying and the deploy is failed at the phone-home timeout", opID)
			}
			requireNoOutbox(t, c, "refused phone-home")
		})
	}
}

// TestHandleProgressReport_StalePhoneHomeIsRecordedOnceTheViewCatchesUp
// follows the refusal to its end: the installed OS keeps retrying, the
// worker's op view catches up on its next resync, and the retry is then
// recorded as the deploy's durable completion.
func TestHandleProgressReport_StalePhoneHomeIsRecordedOnceTheViewCatchesUp(t *testing.T) {
	c := NewCache(nil)
	machineID, opID := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateRunning,
	}}})
	c.ApplyOpResync(nil, nil)

	if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err == nil {
		t.Fatal("phone-home accepted on a view that predates the redeploy")
	}
	requireNoOutbox(t, c, "refused phone-home")

	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed,
		Phase: "awaiting_phone_home", Version: 3,
	})
	if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err != nil {
		t.Fatalf("phone-home retry after the resync: %v", err)
	}
	obs, _ := takeOutbox(c)
	for _, p := range obs {
		if p.Obs.Event == "phone_home" && p.Obs.OperationID == opID && p.Obs.MachineID == machineID {
			return
		}
	}
	t.Fatalf("no phone_home observation names deploy op %s after the retry: %+v", opID, obs)
}

// TestHandleProgressReport_PhoneHomeRetryAckedWhenGlobalCompletedTheOp covers
// the acknowledgement a worker may give without having recorded the
// phone-home itself: global's own word that this exact deploy completed. The
// first phone-home went to another worker and its response was lost, so the
// retry lands here.
func TestHandleProgressReport_PhoneHomeRetryAckedWhenGlobalCompletedTheOp(t *testing.T) {
	newHeld := func(t *testing.T) (*Cache, uuid.UUID, uuid.UUID) {
		t.Helper()
		c := NewCache(nil)
		machineID, opID := uuid.New(), uuid.New()
		c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
			ID: machineID, State: model.MachineStateDeploying,
		}}})
		seedOpsResync(c, model.Operation{
			ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed,
			Phase: "awaiting_phone_home", Version: 2,
		})
		return c, machineID, opID
	}

	t.Run("op_removed carrying running", func(t *testing.T) {
		c, machineID, opID := newHeld(t)
		c.ApplyOpRemoved(opID.String(), machineID.String(), 3, model.MachineStateRunning)
		if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err != nil {
			t.Fatalf("retry of a phone-home for a deploy global completed: %v", err)
		}
		requireNoOutbox(t, c, "acknowledged retry")
		// The acknowledgement is for that op on that machine only.
		if err := c.HandleProgressReport(phoneHomeReport(machineID, uuid.New())); err == nil {
			t.Fatal("phone-home for a different operation acknowledged")
		}
		if err := c.HandleProgressReport(phoneHomeReport(uuid.New(), opID)); err == nil {
			t.Fatal("phone-home for a different machine acknowledged")
		}
	})

	t.Run("op_removed overtaking the op's delta", func(t *testing.T) {
		c := NewCache(nil)
		machineID, opID := uuid.New(), uuid.New()
		c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
			ID: machineID, State: model.MachineStateDeploying,
		}}})
		c.ApplyOpResync(nil, nil)
		c.ApplyOpRemoved(opID.String(), machineID.String(), 3, model.MachineStateRunning)
		if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err != nil {
			t.Fatalf("retry of a phone-home for a deploy global completed: %v", err)
		}
		requireNoOutbox(t, c, "acknowledged retry")
	})

	t.Run("failed op is not acknowledged", func(t *testing.T) {
		c, machineID, opID := newHeld(t)
		c.ApplyOpRemoved(opID.String(), machineID.String(), 3, model.MachineStateFailed)
		if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err == nil {
			t.Fatal("phone-home for a failed deploy acknowledged")
		}
		requireNoOutbox(t, c, "refused phone-home")
	})
}

// TestHandleProgressReport_RecordedPhoneHomeRetrySurvivesResyncAndRestart
// pins the other half: a phone-home this worker recorded stays acknowledged
// for that op after the op leaves the active set by a resync (no op_removed
// seen) and after a restart, so a lost 2xx never turns into a retry loop.
func TestHandleProgressReport_RecordedPhoneHomeRetrySurvivesResyncAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	machineID, opID := uuid.New(), uuid.New()
	c := NewCache(db)
	c.ApplySnapshot(snapMachine(machineID, "52:54:00:e1:00:07", model.MachineStateDeploying))
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed,
		Phase: "awaiting_phone_home", Version: 2,
	})
	if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err != nil {
		t.Fatalf("first phone-home: %v", err)
	}
	takeOutbox(c)

	// Global completed the op while this worker was disconnected: the
	// reconnect resync simply no longer carries it.
	c.ApplyOpResync(nil, nil)
	if err := c.HandleProgressReport(phoneHomeReport(machineID, opID)); err != nil {
		t.Fatalf("retry after the op left the resynced set: %v", err)
	}
	requireNoOutbox(t, c, "acknowledged retry")

	closeLocalDB(t, db)
	db2, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLocalDB(t, db2) })
	c2 := NewCache(db2)
	takeOutbox(c2)
	c2.ApplySnapshot(snapMachine(machineID, "52:54:00:e1:00:07", model.MachineStateRunning))
	c2.ApplyOpResync(nil, nil)
	if err := c2.HandleProgressReport(phoneHomeReport(machineID, opID)); err != nil {
		t.Fatalf("retry after a worker restart: %v", err)
	}
	requireNoOutbox(t, c2, "acknowledged retry after restart")
}
