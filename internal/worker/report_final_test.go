package worker

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// phaseReport is the best-effort report an agent sends at a phase boundary.
func phaseReport(machineID, opID uuid.UUID, phase string) *model.ProgressReport {
	return &model.ProgressReport{OperationID: opID.String(), MachineID: machineID.String(), Phase: phase}
}

// TestHandleProgressReport_OperationOverIsFinal is the worker half of the
// regression for an operator abort that did not stop the installer. Global
// failed the deploy and removed it from the worker, but the agent kept
// reporting and every report got the same answer as any other rejection, so
// the agent had no way to tell "your operation is over" from "try again" and
// carried on installing. A report for an operation the worker knows is over
// must be refused with model.ErrOperationNotActive, the final answer the
// agent stops on.
func TestHandleProgressReport_OperationOverIsFinal(t *testing.T) {
	newDeploy := func(t *testing.T) (c *Cache, machineID, opID uuid.UUID) {
		t.Helper()
		c = NewCache(nil)
		machineID, opID = uuid.New(), uuid.New()
		c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
			ID: machineID, State: model.MachineStateDeploying,
		}}})
		seedOpsResync(c, model.Operation{
			ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed, Version: 2,
		})
		if err := c.HandleProgressReport(phaseReport(machineID, opID, "finalizing")); err != nil {
			t.Fatalf("precondition: report for the active deploy was refused: %v", err)
		}
		return c, machineID, opID
	}

	t.Run("removed by global (abort or timeout sweep)", func(t *testing.T) {
		c, machineID, opID := newDeploy(t)
		c.ApplyOpRemoved(opID.String(), machineID.String(), 3, model.MachineStateFailed)

		err := c.HandleProgressReport(phaseReport(machineID, opID, "bmc_reconcile"))
		if !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("report for a removed operation: got %v, want model.ErrOperationNotActive", err)
		}
		if _, updates := c.DrainOutbox(); len(updates) != 1 || updates[0].Phase != "finalizing" {
			t.Fatalf("the refused report must queue nothing; op updates = %+v", updates)
		}
	})

	t.Run("deploy_complete for a removed operation", func(t *testing.T) {
		c, machineID, opID := newDeploy(t)
		c.ApplyOpRemoved(opID.String(), machineID.String(), 3, model.MachineStateFailed)

		err := c.HandleProgressReport(&model.ProgressReport{
			OperationID: opID.String(), MachineID: machineID.String(), Event: "deploy_complete",
		})
		if !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("deploy_complete for a removed operation: got %v, want model.ErrOperationNotActive", err)
		}
		if obs, _ := c.DrainOutbox(); len(obs) != 0 {
			t.Fatalf("the refused deploy_complete queued observations: %+v", obs)
		}
	})

	t.Run("failed locally by the agent's own failure report", func(t *testing.T) {
		c, machineID, opID := newDeploy(t)
		fail := phaseReport(machineID, opID, "finalizing")
		fail.Failed, fail.Error = true, "writing fstab: boom"
		if err := c.HandleProgressReport(fail); err != nil {
			t.Fatalf("failure report refused: %v", err)
		}

		err := c.HandleProgressReport(phaseReport(machineID, opID, "bmc_reconcile"))
		if !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("report for a locally failed operation: got %v, want model.ErrOperationNotActive", err)
		}
	})

	t.Run("removed while a second operation is active on the machine", func(t *testing.T) {
		c, machineID, opID := newDeploy(t)
		c.ApplyOpRemoved(opID.String(), machineID.String(), 3, model.MachineStateFailed)
		redeploy := uuid.New()
		c.ApplyOpDelta(model.Operation{
			ID: redeploy, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed, Version: 2,
		}, model.MachineStateDeploying)

		// The aborted deploy's agent finishes late and reports completion.
		err := c.HandleProgressReport(&model.ProgressReport{
			OperationID: opID.String(), MachineID: machineID.String(), Event: "deploy_complete",
		})
		if !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("stale deploy_complete: got %v, want model.ErrOperationNotActive", err)
		}
		if op := c.GetOperation(redeploy.String()); op == nil || op.Phase == "awaiting_phone_home" || op.Status != model.OpClaimed {
			t.Fatalf("the stale agent's report changed the new operation: %+v", op)
		}
		if obs, _ := c.DrainOutbox(); len(obs) != 0 {
			t.Fatalf("the stale agent's report queued observations: %+v", obs)
		}
	})
}

// TestHandleProgressReport_UnsynchronizedIsNotFinal pins the other side: the
// final answer stops an agent, so the worker gives it only when it knows the
// operation is over. A worker that restarted mid-deploy has not heard from
// global yet, and its agent's deploy may be perfectly healthy.
func TestHandleProgressReport_UnsynchronizedIsNotFinal(t *testing.T) {
	machineID, opID := uuid.New(), uuid.New()
	snap := &model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateDeploying,
	}}}
	op := model.Operation{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed, Version: 2}

	t.Run("restarted with a persisted operation set, before the first resync", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "worker.db")
		db, err := OpenLocalDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		first := NewCache(db)
		first.ApplySnapshot(snap)
		seedOpsResync(first, op)

		db2, err := OpenLocalDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		restarted := NewCache(db2)
		err = restarted.HandleProgressReport(phaseReport(machineID, opID, "rsyncing_os"))
		if err == nil {
			t.Fatal("precondition: an unsynchronized worker refuses reports")
		}
		if errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("an unsynchronized worker told the agent its operation is over: %v", err)
		}

		// The resync carries the operation: the deploy goes on.
		seedOpsResync(restarted, op)
		if err := restarted.HandleProgressReport(phaseReport(machineID, opID, "rsyncing_os")); err != nil {
			t.Fatalf("report after the resync was refused: %v", err)
		}
	})

	t.Run("started with no persisted operation set, before the first resync", func(t *testing.T) {
		c := NewCache(nil)
		c.ApplySnapshot(snap)
		err := c.HandleProgressReport(phaseReport(machineID, opID, "rsyncing_os"))
		if err == nil {
			t.Fatal("precondition: a worker that does not hold the operation refuses its reports")
		}
		if errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("a worker that never synchronized told the agent its operation is over: %v", err)
		}
	})

	t.Run("resynced without the operation: it ended while the worker was away", func(t *testing.T) {
		c := NewCache(nil)
		c.ApplySnapshot(snap)
		seedOpsResync(c) // global's active set, which no longer holds the op
		err := c.HandleProgressReport(phaseReport(machineID, opID, "rsyncing_os"))
		if !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("report for an operation absent from a resynced set: got %v, want model.ErrOperationNotActive", err)
		}
	})

	t.Run("resynced, then the connection to global dropped", func(t *testing.T) {
		c := NewCache(nil)
		c.ApplySnapshot(snap)
		aborted := uuid.New()
		seedOpsResync(c, model.Operation{ID: aborted, MachineID: machineID, Type: model.OpDeploy, Status: model.OpClaimed, Version: 2})
		c.ApplyOpRemoved(aborted.String(), machineID.String(), 3, model.MachineStateFailed)
		c.OpStreamDown(time.Now())

		// An operation global created after the drop is unknown here and
		// may be healthy, driven by the other worker of a pair.
		err := c.HandleProgressReport(phaseReport(machineID, opID, "rsyncing_os"))
		if err == nil {
			t.Fatal("precondition: a worker that does not hold the operation refuses its reports")
		}
		if errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("a worker cut off from global told the agent an operation it never heard of is over: %v", err)
		}
		// An end the worker did hear about stays final through the outage.
		if err := c.HandleProgressReport(phaseReport(machineID, aborted, "rsyncing_os")); !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("report for an operation whose removal the worker applied: got %v, want model.ErrOperationNotActive", err)
		}

		// The reconnect's resync is global's active set again.
		seedOpsResync(c)
		if err := c.HandleProgressReport(phaseReport(machineID, opID, "rsyncing_os")); !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("report for an operation absent from the reconnect's resync: got %v, want model.ErrOperationNotActive", err)
		}
	})

	t.Run("a report bound to the wrong machine is refused but not final", func(t *testing.T) {
		other := uuid.New()
		c := NewCache(nil)
		c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{ID: machineID}, {ID: other}}})
		seedOpsResync(c, op)
		err := c.HandleProgressReport(phaseReport(other, opID, "rsyncing_os"))
		if err == nil {
			t.Fatal("precondition: a machine/operation mismatch is refused")
		}
		if errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("a binding mismatch was reported as the operation being over: %v", err)
		}
	})
}

// TestHandleProgressReport_DisconnectedWorkerDoesNotEndUnknownOp: a worker
// whose connection to global is down does not tell an agent its operation is
// over merely because its op set does not hold that operation. Absence from a
// resynced set is proof only while the op stream is up: once it drops, global
// can create, claim and drive an operation this worker never hears of.
//
// An active/standby pair reaches this. The standby's connection to global is
// down when keepalived moves the VIP to it, a deploy that started meanwhile
// on the active worker is unknown to it, and the agent's next report (here
// the final deploy_complete of a finished install) lands on it. A 410 there
// halts the agent: no deploy_complete, no reboot, the host left in the
// recovery environment until global's stuck sweep fails the op.
//
// The test runs a real WSConn against a real ws.Hub, so the lost connection
// is the production event and not a test-only hook.
func TestHandleProgressReport_DisconnectedWorkerDoesNotEndUnknownOp(t *testing.T) {
	machineID := uuid.New()
	snap := pop1Snap(7, machineID, "host-1")
	snap.Machines[0].State = model.MachineStateDeploying

	// A global with no active operations: the hello op_resync is empty.
	_, url, stopGlobal := startGlobal(seqGlobal{snap: snap})
	globalUp := true
	defer func() {
		if globalUp {
			stopGlobal()
		}
	}()

	cache := NewCache(nil)
	wsc := NewWSConn(WSConnConfig{GlobalURL: url, Token: "tok-1", WorkerID: "worker-1"}, cache, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = wsc.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	deployComplete := func(opID uuid.UUID) error {
		return cache.HandleProgressReport(&model.ProgressReport{
			MachineID: machineID.String(), OperationID: opID.String(), Event: "deploy_complete",
		})
	}

	// Control: connected and resynced, an operation the set does not hold
	// is over. Until the hello op_resync is applied the answer is the
	// plain "not known yet" refusal, so waiting for the final one also
	// waits for the resync.
	waitFor("a connected, resynced worker to refuse an absent operation as final", func() bool {
		return errors.Is(deployComplete(uuid.New()), model.ErrOperationNotActive)
	})

	// The connection to global drops and stays down. A deploy is then
	// created, claimed and driven by the other worker; this one never
	// hears of it.
	stopGlobal()
	globalUp = false
	waitFor("the worker to notice the lost connection", func() bool { return !wsc.Connected() })

	err := deployComplete(uuid.New())
	if err == nil {
		t.Fatal("a report for an operation the worker does not hold was accepted")
	}
	if errors.Is(err, model.ErrOperationNotActive) {
		t.Fatalf("a worker with no connection to global refused the report as final (410 Gone, the agent halts): %v; "+
			"want a retryable refusal (409): it cannot know whether an operation created since it lost the op stream is over", err)
	}
}
