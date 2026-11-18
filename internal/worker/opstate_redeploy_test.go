package worker

import (
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
// Go's randomized map iteration order. In production this dual-active state
// is unreachable — store/operations.go's idx_one_active_op_per_machine
// unique index enforces at most one active lifecycle op per machine, so a
// second op can only become active once the first is terminal. This test
// exists only to document that boundary and must NOT assert a specific
// winner or flip-flopping across iterations (that would pin flakiness as a
// requirement); it asserts only what is guaranteed: the result is one of
// the two ops.
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

	c.RecordPhoneHome(id)
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
	c.RecordPhoneHome(id)
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
