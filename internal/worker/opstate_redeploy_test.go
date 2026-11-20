package worker

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// These tests pin two op-state invariants a qemu-integration debugging
// session (docs/archeology/handoffs/2026-07-20-preserve-data-disks-
// qemu-choreography.md) established the hard way while chasing a redeploy
// choreography bug: a second deploy on the same VM kept re-running the
// FIRST deploy's op instead of the second's. Pinning the invariants here
// catches a regression in `just test` seconds, not an 11-minute qemu CI
// cycle.

// TestDeployOpForMAC_TerminalOp1ActiveOp2_Deterministic pins the redeploy
// shape: once op1 is terminal (completed) and op2 is the only active deploy
// op left for the machine, readers must deterministically serve op2 no
// matter the (randomized) map iteration order over o.ops. A single read
// proves nothing about map-order independence, so this loops ~100 times.
func TestDeployOpForMAC_TerminalOp1ActiveOp2_Deterministic(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:rd:01:01"
	op1, op2 := uuid.New(), uuid.New()

	deployingMachine(c, id, mac)

	// op1: claimed, then completed at a higher version (terminal).
	c.ApplyOpDelta(model.Operation{
		ID: op1, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
	}, model.MachineStateDeploying)
	c.ApplyOpDelta(model.Operation{
		ID: op1, MachineID: id, Type: model.OpDeploy, Status: model.OpCompleted, Version: 2,
	}, "")

	// op2: the redeploy's own op, a fresh ID with its own version sequence,
	// claimed and active.
	c.ApplyOpDelta(model.Operation{
		ID: op2, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
	}, model.MachineStateDeploying)

	for i := 0; i < 100; i++ {
		got := c.DeployOpForMAC(mac)
		if got == nil || got.ID != op2 {
			t.Fatalf("iteration %d: DeployOpForMAC = %+v, want op2 %s (a terminal op1 must never be returned)", i, got, op2)
		}
		if action := c.BootActionForMAC(mac); action != model.BootDeploy {
			t.Fatalf("iteration %d: BootActionForMAC = %d, want BootDeploy", i, action)
		}
	}
}

// TestDeployOpForMAC_BothOpsActive_UndefinedTiebreakIsOK documents the
// negative shape that made single-VM redeploy tests flaky: if op1 is left
// ACTIVE (e.g. still claimed, awaiting_phone_home) at the same time op2 is
// also active for the same machine, deployOpForMAC has no tiebreak beyond
// Go's randomized map iteration order. Global never holds this dual-active
// state — store/operations.go's idx_one_active_op_per_machine unique index
// enforces at most one active lifecycle op per machine, so a second op can
// only become active once the first is terminal — but a worker can hold it
// briefly when op2's op_delta arrives ahead of op1's op_removed. A MAC-only
// reader has no op to bind to, so either answer is tolerated there; a report
// that names its op is held to that op (TestPhoneHome_CompletesTheOpItNames
// below). This test exists only to document that boundary and must NOT
// assert a specific winner or flip-flopping across iterations (that would
// pin flakiness as a requirement); it asserts only what is guaranteed: the
// result is one of the two ops.
func TestDeployOpForMAC_BothOpsActive_UndefinedTiebreakIsOK(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:rd:02:01"
	op1, op2 := uuid.New(), uuid.New()

	deployingMachine(c, id, mac)

	c.ApplyOpDelta(model.Operation{
		ID: op1, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed,
		Phase: "awaiting_phone_home", Version: 1,
	}, model.MachineStateDeploying)
	c.ApplyOpDelta(model.Operation{
		ID: op2, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
	}, model.MachineStateDeploying)

	got := c.DeployOpForMAC(mac)
	if got == nil || (got.ID != op1 && got.ID != op2) {
		t.Fatalf("got %+v, want one of {op1=%s, op2=%s}", got, op1, op2)
	}
}

// TestPhoneHome_CompletesTheOpItNames: a phone-home completes the deploy op
// it names, and no other.
//
// Global ends one deploy of a machine (here the phone-home timeout failed it)
// and the operator starts the next. Global commits both, but publishes the
// end and the new op from different goroutines, so the new op's op_delta can
// reach a worker before the old op's op_removed. Until the removal lands, the
// worker holds two active deploy ops for the machine: the unique index keeps
// global from ever holding that state, but the worker can. The first install
// is on the disk and its phone-home unit is still retrying. If that
// phone-home arrives in the window, the worker validates the op it names and
// must record the completion of that op. The durable phone_home observation
// is what global completes a deploy from, and global completes whichever
// active deploy op the observation carries, pending included.
func TestPhoneHome_CompletesTheOpItNames(t *testing.T) {
	// The worker once picked the op to complete by ranging over a map, so a
	// single run could pass by luck. Fifty independent caches cannot.
	for i := 0; i < 50; i++ {
		c := NewCache(nil)
		id := uuid.New()
		deployingMachine(c, id, "52:54:00:rd:05:01")

		installed := model.Operation{
			ID: uuid.New(), MachineID: id, Type: model.OpDeploy,
			Status: model.OpClaimed, Phase: "awaiting_phone_home", Version: 5,
		}
		redeploy := model.Operation{
			ID: uuid.New(), MachineID: id, Type: model.OpDeploy,
			Status: model.OpPending, Version: 0,
		}
		seedOpsResync(c, installed)
		// The next deploy's op_delta, ahead of the first one's op_removed.
		c.ApplyOpDelta(redeploy, "")

		if err := c.HandleProgressReport(&model.ProgressReport{
			MachineID: id.String(), OperationID: installed.ID.String(), Event: "phone_home",
		}); err != nil {
			t.Fatalf("phone-home for the installed deploy refused: %v", err)
		}

		obs, updates := c.DrainOutbox()
		var phoneHomes int
		for _, p := range obs {
			if p.Obs.Event != "phone_home" {
				continue
			}
			phoneHomes++
			if p.Obs.OperationID != installed.ID {
				t.Fatalf("run %d: the phone-home named deploy %s, but the worker queued a phone_home observation for %s (the pending redeploy is %s), which global would complete without it ever having run",
					i, installed.ID, p.Obs.OperationID, redeploy.ID)
			}
		}
		if phoneHomes != 1 {
			t.Fatalf("run %d: got %d phone_home observations, want 1: %+v", i, phoneHomes, obs)
		}
		var completed []string
		for _, u := range updates {
			if u.Status == string(model.OpCompleted) {
				completed = append(completed, u.OperationID)
			}
		}
		if len(completed) != 1 || completed[0] != installed.ID.String() {
			t.Fatalf("run %d: completed op_updates %v, want only the named deploy %s", i, completed, installed.ID)
		}
		if got := c.GetOperation(redeploy.ID.String()); got == nil || got.Status != model.OpPending {
			t.Fatalf("run %d: another op's phone-home touched the pending redeploy %s: %+v", i, redeploy.ID, got)
		}
	}
}

// TestApplyOpResync_PhoneHomedOverlayPinning pins the applyOpResync
// contract around the phoneHomed latch (opstate_cache.go applyOpResync,
// recordPhoneHome) that bit the FakeHub during the qemu redeploy debugging:
//   - a resync whose op list still CONTAINS the phone-homed op (active-
//     deploying, because global hasn't processed the completion yet) pins
//     the overlay to running — existing documented behavior.
//   - a resync whose op list OMITS the op prunes the phoneHomed latch and
//     the overlay follows the resync's derived states.
//   - the invariant that actually broke: applyOpResync does not filter by
//     op status — it trusts the reader contract that only ACTIVE ops are
//     ever handed to it (production: store reads WHERE active; qemutest:
//     fakeActiveOps in internal/qemutest/hub.go filters
//     OpCompleted/OpFailed before fanning). If a resync producer violates
//     that contract and feeds a TERMINAL op back in, phoneHomed's prune
//     check is membership-only (op present in the resync's set at all,
//     regardless of status) and does NOT prune it — so the running overlay
//     stays force-pinned on a completed op. That is the bug shape: a stale
//     running pin surviving a resync that should have retired it.
func TestApplyOpResync_PhoneHomedOverlayPinning(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:rd:03:01"
	opID := uuid.New()

	deployingMachine(c, id, mac)
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy,
		Status: model.OpClaimed, Phase: "awaiting_phone_home", Version: 1,
	})

	c.RecordPhoneHome(id, opID)
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("post phone-home: running overlay not applied, got %+v", got)
	}
	if !c.opState.phoneHomed[opID.String()] {
		t.Fatal("recordPhoneHome should latch the op in phoneHomed")
	}

	// Resync still carries the op, active-deploying (global hasn't caught
	// up to the completion yet). Pin: overlay stays running.
	c.ApplyOpResync([]model.Operation{
		{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1},
	}, map[string]model.MachineState{id.String(): model.MachineStateDeploying})

	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("resync still carrying the phone-homed op must not downgrade the overlay, got %+v", got)
	}
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("resync still carrying the phone-homed op: BootActionForMAC = %d, want BootLocal", got)
	}
	if !c.opState.phoneHomed[opID.String()] {
		t.Fatal("phoneHomed latch must still be present while the op is in the resync's set")
	}

	// Resync omits the op entirely (global's completion propagated; the op
	// left the active set). Pin: the latch is pruned and the overlay
	// follows the resync's derived states (none for this machine here), so
	// the machine reads its snapshot baseline (still `deploying` — no new
	// ApplySnapshot has landed) instead of the stale `running` pin.
	c.ApplyOpResync(nil, nil)

	if c.opState.phoneHomed[opID.String()] {
		t.Fatal("phoneHomed latch must be pruned once its op leaves the resync's active set")
	}
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateDeploying {
		t.Fatalf("after removing the running overlay, machine state = %+v, want deploying snapshot baseline", got)
	}

	// The key new pin: applyOpResync must never be FED a terminal op — it
	// does not defend against one itself. Reproduce the malformed-producer
	// case directly: bring the op back active (a reconnect resync clears
	// the op set entirely, so it must reappear before it can be latched
	// again), re-latch it via a fresh phone-home, then feed a resync whose
	// op list contains that same op marked COMPLETED. A real producer must
	// exclude terminal ops (that's the reader contract this test names);
	// this proves the consequence when one doesn't.
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 3,
	}, "")
	c.RecordPhoneHome(id, opID)
	if !c.opState.phoneHomed[opID.String()] {
		t.Fatal("recordPhoneHome should re-latch once the op is active again")
	}

	c.ApplyOpResync([]model.Operation{
		{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpCompleted, Version: 4},
	}, nil) // no derived states: a completed op contributes no ClaimTransition entry

	if !c.opState.phoneHomed[opID.String()] {
		t.Fatal("phoneHomed prune is membership-only and must not distinguish a present terminal op from a present active one")
	}
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("a terminal op present in a resync still force-pins the running overlay (the stale-pin failure mode), got %+v", got)
	}
}

// TestApplyOpDelta_LateClaimKeepsPhoneHomedRunning: a host can install on
// the pending-op boot clause and phone home before the claim's op_delta
// reaches this worker. That delta carries the claim's transient state
// (deploying) for an op whose install is already over. It must not move
// the machine back from running, exactly as an op_resync carrying the same
// claimed op does not (applyOpResync): with the machine at deploying, the
// installed OS's retried phone-home is processed in full and queues a
// second completion. A delta that ends the op still applies its state.
func TestApplyOpDelta_LateClaimKeepsPhoneHomedRunning(t *testing.T) {
	c := NewCache(nil)
	id, opID := uuid.New(), uuid.New()
	mac := "52:54:00:rd:04:01"
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 1,
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateCommissioned, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}},
	})
	c.ApplyOpResync(nil, nil)
	c.ApplyOpDelta(model.Operation{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 0}, "")

	report := func(event string) {
		t.Helper()
		if err := c.HandleProgressReport(&model.ProgressReport{
			MachineID: id.String(), OperationID: opID.String(), Event: event,
		}); err != nil {
			t.Fatalf("%s: %v", event, err)
		}
	}
	report("deploy_complete")
	report("phone_home")
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("after phone-home the machine must be running, got %+v", got)
	}
	_, before := c.DrainOutbox()

	// The claim's delta arrives late.
	c.ApplyOpDelta(model.Operation{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1},
		model.MachineStateDeploying)

	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("a late claim delta moved a phone-homed machine back to %q", got.State)
	}
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("BootActionForMAC = %d after the late claim delta, want BootLocal", got)
	}
	// The op stays completed, so a late report from the deploy agent cannot
	// fail a deploy that finished. A resync built before global processed
	// the phone-home is held to the same.
	stale := []func(){
		func() {},
		func() {
			c.ApplyOpResync([]model.Operation{{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1}},
				map[string]model.MachineState{id.String(): model.MachineStateDeploying})
		},
	}
	for _, apply := range stale {
		apply()
		if op := c.GetOperation(opID.String()); op == nil || op.Status != model.OpCompleted {
			t.Fatalf("a stale active row revived the phone-homed op: %+v", op)
		}
		err := c.HandleProgressReport(&model.ProgressReport{
			MachineID: id.String(), OperationID: opID.String(), Failed: true, Phase: "partitioning", Error: "late",
		})
		if !errors.Is(err, model.ErrOperationNotActive) {
			t.Fatalf("a late failure report for a phone-homed op must be refused as not active, got %v", err)
		}
		if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
			t.Fatalf("machine left running state: %+v", got)
		}
	}

	// The installed OS retries a phone-home whose answer it lost: an
	// acknowledgement, with no second completion.
	report("phone_home")
	obs, updates := c.DrainOutbox()
	for _, u := range updates {
		if u.Status == string(model.OpCompleted) {
			t.Errorf("the retried phone-home queued a second completion (the first drain held %d updates)", len(before))
		}
	}
	for _, o := range obs {
		if o.Obs.Event == "phone_home" {
			t.Error("the retried phone-home queued a second phone_home observation")
		}
	}

	// Global failing the op (say, its timeout sweep won the race) still
	// lands: a terminal delta's state is authoritative.
	c.ApplyOpDelta(model.Operation{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpFailed, Version: 2},
		model.MachineStateFailed)
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateFailed {
		t.Fatalf("a terminal delta must apply its machine state, got %+v", got)
	}
}
