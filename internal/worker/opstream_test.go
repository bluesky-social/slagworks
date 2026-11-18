package worker

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// The Tier-2 Phase-2 mandatory op-stream tests (plan
// docs/plans/2026-07-05-ops-out-of-snapshot.md, "Phase 2 must add these
// tests"). They exercise the worker's op layer directly through the Cache
// stream-apply surface (ApplyOpDelta / ApplyOpResync / ApplyOpRemoved) +
// ApplySnapshot, since that is where the re-wipe-window, version, tombstone,
// fail-closed, and untrusted semantics live.

func deployingMachine(c *Cache, id uuid.UUID, mac string) {
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 1,
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateDeploying, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}},
	})
}

// TestRewipeWindow_BothOrders: a deploy at awaiting_phone_home, then deliver
// op_removed(running) + an identity-running snapshot in BOTH orders. Boot
// must stay BootLocal throughout — never a re-wipe.
func TestRewipeWindow_BothOrders(t *testing.T) {
	run := func(t *testing.T, removedFirst bool) {
		c := NewCache(nil)
		id := uuid.New()
		mac := "52:54:00:re:wp:01"
		opID := uuid.New()
		deployingMachine(c, id, mac)
		seedOpsResync(c, model.Operation{
			ID: opID, MachineID: id, Type: model.OpDeploy,
			Status: model.OpClaimed, Phase: "awaiting_phone_home", Version: 1,
		})
		// Local phone-home: overlay -> running, op latched.
		c.RecordPhoneHome(id)
		if got := c.BootActionForMAC(mac); got != model.BootLocal {
			t.Fatalf("after phone-home: got %d, want BootLocal", got)
		}

		runningSnap := &model.SiteSnapshot{
			SeqNum: 2,
			Machines: []model.MachineSnapshot{{
				ID: id, State: model.MachineStateRunning, PXEEligible: true,
				Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
			}},
		}
		removed := func() { c.ApplyOpRemoved(opID.String(), id.String(), 2, model.MachineStateRunning) }
		identity := func() { c.ApplySnapshot(runningSnap) }

		if removedFirst {
			removed()
			if got := c.BootActionForMAC(mac); got != model.BootLocal {
				t.Fatalf("after op_removed: got %d, want BootLocal", got)
			}
			identity()
		} else {
			identity()
			if got := c.BootActionForMAC(mac); got != model.BootLocal {
				t.Fatalf("after identity-running: got %d, want BootLocal", got)
			}
			removed()
		}
		if got := c.BootActionForMAC(mac); got != model.BootLocal {
			t.Fatalf("final: got %d, want BootLocal", got)
		}
	}
	t.Run("op_removed first", func(t *testing.T) { run(t, true) })
	t.Run("identity-running first", func(t *testing.T) { run(t, false) })
}

// TestVersionIdempotency: out-of-order + duplicate deltas converge to the
// highest version. A lower-version delta is dropped; the held op keeps the
// highest-version state.
func TestVersionIdempotency(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:ve:rs:01"
	opID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 1,
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateCommissioned, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}},
	})
	// Clear untrusted (fresh cache is trusted-empty, but be explicit).
	seedOpsResync(c)

	// v3 arrives first (pending deploy).
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 3,
	}, "")
	// v1 (stale) arrives late — must be dropped.
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1, Phase: "stale",
	}, "")
	// Duplicate v3 — no-op.
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 3,
	}, "")

	op := c.GetOperation(opID.String())
	if op == nil || op.Version != 3 || op.Phase != "" {
		t.Fatalf("converged op = %+v, want version 3 no phase", op)
	}
}

// TestReconnectResync: a stale op map is corrected to global's active set by
// op_resync (full replace). An op present only in the old set is gone; a new
// op appears.
func TestReconnectResync(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	staleOp, freshOp := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum:   1,
		Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateCommissioned}},
	})
	seedOpsResync(c, model.Operation{ID: staleOp, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1})
	if c.GetOperation(staleOp.String()) == nil {
		t.Fatal("stale op should be present after first resync")
	}
	// Reconnect resync carries only the fresh op.
	seedOpsResync(c, model.Operation{ID: freshOp, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1})
	if c.GetOperation(staleOp.String()) != nil {
		t.Fatal("stale op must be dropped by resync full-replace")
	}
	if c.GetOperation(freshOp.String()) == nil {
		t.Fatal("fresh op must be present after resync")
	}
}

// TestFailClosedTransientBaseline: an effective state of deploying with NO
// active op in opState must return BootLocal (the deliberate Tier-2 change;
// the old table returned BootDeploy). Same for commissioning.
func TestFailClosedTransientBaseline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state model.MachineState
	}{
		{"deploying", model.MachineStateDeploying},
		{"commissioning", model.MachineStateCommissioning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(nil)
			id := uuid.New()
			mac := "52:54:00:fc:00:01"
			c.ApplySnapshot(&model.SiteSnapshot{
				SeqNum: 1,
				Machines: []model.MachineSnapshot{{
					ID: id, State: tc.state, PXEEligible: true,
					Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
				}},
			})
			// Trusted, but NO active op for this machine.
			seedOpsResync(c)
			if got := c.BootActionForMAC(mac); got != model.BootLocal {
				t.Fatalf("transient baseline %s with no active op: got %d, want BootLocal", tc.state, got)
			}
		})
	}
}

// TestTombstone: op_removed then a stale lower-version op_delta for the same
// op must NOT resurrect it, and boot stays fail-closed.
func TestTombstone(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:to:mb:01"
	opID := uuid.New()
	deployingMachine(c, id, mac)
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 5,
	})
	// Remove at version 6.
	c.ApplyOpRemoved(opID.String(), id.String(), 6, model.MachineStateFailed)
	if c.GetOperation(opID.String()) != nil {
		t.Fatal("op should be removed")
	}
	// Stale delta at version 5 (<= tombstone 6) must be discarded.
	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 5,
	}, model.MachineStateDeploying)
	if c.GetOperation(opID.String()) != nil {
		t.Fatal("tombstoned op must not be resurrected by a stale delta")
	}
	// Overlay moved to failed (durable) by op_removed; boot is fail-closed.
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("after tombstone: got %d, want BootLocal", got)
	}

	// A stale RESYNC carrying the removed op at a version <= its tombstone
	// must not resurrect it either (belt-and-suspenders for any future
	// register/resync reorder). The resync's other content still applies.
	other := uuid.New()
	c.ApplyOpResync([]model.Operation{
		{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 5},
		{ID: other, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1},
	}, nil)
	if c.GetOperation(opID.String()) != nil {
		t.Fatal("tombstoned op must not be resurrected by a stale resync")
	}
	if c.GetOperation(other.String()) == nil {
		t.Fatal("non-tombstoned resync content must still apply")
	}
	// A resync carrying the op at a HIGHER version than the tombstone is a
	// genuine re-appearance (global is authoritative) and must apply.
	c.ApplyOpResync([]model.Operation{
		{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 7},
	}, nil)
	if c.GetOperation(opID.String()) == nil {
		t.Fatal("a higher-version resync entry must apply (global is authoritative)")
	}
}

// TestCrossSiteOps_InvisibleToExecutorUntilInView pins the fleet-wide
// fan-out safety rule: op messages reach ALL workers, so a pending op for
// a machine NOT in this worker's identity view must be invisible to
// PendingLifecycleOps/PendingPowerOps (a pop1 worker must never claim —
// and then fail — a pop3 machine's op). The op is STORED (filtering is at
// the read site, not apply time), so when a later snapshot adds the
// machine, the op becomes visible without waiting for a reconnect resync.
func TestCrossSiteOps_InvisibleToExecutorUntilInView(t *testing.T) {
	c := NewCache(nil)
	localID, foreignID := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum:   1,
		Machines: []model.MachineSnapshot{{ID: localID, State: model.MachineStateCommissioned}},
	})
	// Resync carries the fleet-wide set: one op for our machine, one
	// lifecycle + one power op for a machine another site owns.
	seedOpsResync(c,
		model.Operation{ID: uuid.New(), MachineID: localID, Type: model.OpDeploy, Status: model.OpPending, Version: 1},
		model.Operation{ID: uuid.New(), MachineID: foreignID, Type: model.OpDeploy, Status: model.OpPending, Version: 1},
		model.Operation{ID: uuid.New(), MachineID: foreignID, Type: model.OpPowerOn, Status: model.OpPending, Version: 1},
	)

	lc := c.PendingLifecycleOps()
	if len(lc) != 1 || lc[0].MachineID != localID {
		t.Fatalf("lifecycle: got %d ops (%+v), want only the local machine's", len(lc), lc)
	}
	if pw := c.PendingPowerOps(); len(pw) != 0 {
		t.Fatalf("power: foreign machine's power op must be invisible, got %d", len(pw))
	}
	// The foreign ops are stored, just filtered at the read site.
	if c.OperationCount() != 3 {
		t.Fatalf("op set should store all 3 ops, got %d", c.OperationCount())
	}

	// A later identity snapshot adds the foreign machine to our view (e.g.
	// the delta raced the snapshot that enrolls it) — the ops become
	// visible without a reconnect.
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 2,
		Machines: []model.MachineSnapshot{
			{ID: localID, State: model.MachineStateCommissioned},
			{ID: foreignID, State: model.MachineStateCommissioned},
		},
	})
	if lc := c.PendingLifecycleOps(); len(lc) != 2 {
		t.Fatalf("after view adds machine: got %d lifecycle ops, want 2", len(lc))
	}
	if pw := c.PendingPowerOps(); len(pw) != 1 {
		t.Fatalf("after view adds machine: got %d power ops, want 1", len(pw))
	}
}

// TestUntrustedRehydration: a cold start with a persisted active deploy op
// must NOT actuate (no BootDeploy, executor paused) before the first
// op_resync; a persisted awaiting_phone_home op still forces BootLocal;
// normal actuation resumes after resync.
func TestUntrustedRehydration(t *testing.T) {
	dir := t.TempDir()
	dbPath := dir + "/worker.db"

	id := uuid.New()
	mac := "52:54:00:un:tr:01"
	opID := uuid.New()

	// First worker: persist an identity snapshot (commissioned + pending
	// deploy) and the op set, then close.
	{
		db, err := OpenLocalDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		c := NewCache(db)
		c.ApplySnapshot(&model.SiteSnapshot{
			SeqNum: 1,
			Machines: []model.MachineSnapshot{{
				ID: id, State: model.MachineStateCommissioned, PXEEligible: true,
				Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
			}},
		})
		// A persisted PENDING deploy op — on a trusted cache this would
		// actuate BootDeploy.
		seedOpsResync(c, model.Operation{
			ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1,
		})
	}

	// Second worker: rehydrate from the same DB — UNTRUSTED.
	db2, err := OpenLocalDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache(db2)

	// A commissioned+pending-deploy machine would normally BootDeploy, but
	// untrusted must NOT actuate.
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("untrusted cold start must not actuate, got %d, want BootLocal", got)
	}
	// Executor paused: no pending ops handed out.
	if got := c.PendingLifecycleOps(); len(got) != 0 {
		t.Fatalf("untrusted must pause executor, got %d pending ops", len(got))
	}

	// A persisted awaiting_phone_home op (deploying baseline) must still
	// force BootLocal on cold start — the conservative direction that stops
	// a mid-phone-home re-wipe. Rehydrate a fresh DB for this leg.
	t.Run("awaiting_phone_home forces BootLocal untrusted", func(t *testing.T) {
		dir2 := t.TempDir()
		p2 := dir2 + "/worker.db"
		{
			db, err := OpenLocalDB(p2)
			if err != nil {
				t.Fatal(err)
			}
			cc := NewCache(db)
			cc.ApplySnapshot(&model.SiteSnapshot{
				SeqNum: 1,
				Machines: []model.MachineSnapshot{{
					ID: id, State: model.MachineStateDeploying, PXEEligible: true,
					Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
				}},
			})
			seedOpsResync(cc, model.Operation{
				ID: opID, MachineID: id, Type: model.OpDeploy,
				Status: model.OpClaimed, Phase: "awaiting_phone_home", Version: 1,
			})
		}
		db3, err := OpenLocalDB(p2)
		if err != nil {
			t.Fatal(err)
		}
		cc := NewCache(db3)
		if got := cc.BootActionForMAC(mac); got != model.BootLocal {
			t.Fatalf("untrusted awaiting_phone_home: got %d, want BootLocal", got)
		}
	})

	// First op_resync clears untrusted; normal actuation resumes.
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1,
	})
	if got := c.BootActionForMAC(mac); got != model.BootDeploy {
		t.Fatalf("after resync (trusted): got %d, want BootDeploy", got)
	}
	if got := c.PendingLifecycleOps(); len(got) != 1 {
		t.Fatalf("after resync executor resumes, got %d pending ops, want 1", len(got))
	}
}

// TestAbortSweepDurablePropagation is the worker-side half of the abort/sweep
// durable-state rule: an op_removed carrying machine_state=failed moves the
// overlay to failed, and boot is fail-closed (BootLocal) throughout. The
// pending-expiry variant carries no state and leaves the machine at its
// resting baseline. (The store-side outcome plumbing is pinned in
// internal/store; this pins the worker's overlay convergence.)
func TestAbortSweepDurablePropagation(t *testing.T) {
	t.Run("durable failed", func(t *testing.T) {
		c := NewCache(nil)
		id := uuid.New()
		mac := "52:54:00:ab:01:01"
		opID := uuid.New()
		deployingMachine(c, id, mac)
		seedOpsResync(c, model.Operation{
			ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
		})
		// Sweep/abort fails the op with durable failed state.
		c.ApplyOpRemoved(opID.String(), id.String(), 2, model.MachineStateFailed)
		if got := c.MachineState(id); got != string(model.MachineStateFailed) {
			t.Fatalf("overlay after fail: got %q, want failed", got)
		}
		if got := c.BootActionForMAC(mac); got != model.BootLocal {
			t.Fatalf("failed machine boot: got %d, want BootLocal", got)
		}
	})
	t.Run("pending-expiry no state", func(t *testing.T) {
		c := NewCache(nil)
		id := uuid.New()
		opID := uuid.New()
		c.ApplySnapshot(&model.SiteSnapshot{
			SeqNum:   1,
			Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateCommissioned}},
		})
		seedOpsResync(c, model.Operation{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1})
		// Pending-expiry: op_removed with no machine_state.
		c.ApplyOpRemoved(opID.String(), id.String(), 2, "")
		// Machine stays at its resting baseline (commissioned) — no overlay.
		if got := c.MachineState(id); got != string(model.MachineStateCommissioned) {
			t.Fatalf("pending-expiry must leave resting state, got %q", got)
		}
	})
}

// awaitingDeploy seeds a deploying machine whose deploy op reached
// awaiting_phone_home LOCALLY (via the agent's deploy_complete) while global
// still holds the op at version 5, phase rsyncing_os.
func awaitingDeploy(t *testing.T, mac string) (*Cache, uuid.UUID, uuid.UUID) {
	t.Helper()
	c := NewCache(nil)
	id, opID := uuid.New(), uuid.New()
	deployingMachine(c, id, mac)
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy,
		Status: model.OpClaimed, Phase: "rsyncing_os", Version: 5,
	})
	c.UpdateOpPhase(opID.String(), "awaiting_phone_home")
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("precondition: got %d, want BootLocal", got)
	}
	return c, id, opID
}

// TestStaleEchoKeepsAwaitingPhoneHome covers a regression: global's echo of an
// earlier phase update arrives (higher Version, older phase) after the local
// deploy_complete. The op must stay awaiting_phone_home, so the post-install
// PXE (and its boot-prefix phase reports) still boots the disk.
func TestStaleEchoKeepsAwaitingPhoneHome(t *testing.T) {
	mac := "52:54:00:15:10:54"
	c, id, opID := awaitingDeploy(t, mac)

	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy,
		Status: model.OpClaimed, Phase: "installing_bootloader", Version: 6,
	}, "")
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("after stale echo delta: got %d, want BootLocal", got)
	}
	// The firmware PXE that follows must not move it either.
	c.UpdateOpPhase(opID.String(), "dhcp_ack")
	c.UpdateOpPhase(opID.String(), "ipxe_dhcp")
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("after post-install PXE phases: got %d, want BootLocal", got)
	}
	if op := c.GetOperation(opID.String()); op == nil || op.Version != 6 || op.Phase != "awaiting_phone_home" {
		t.Fatalf("op = %+v, want version 6 phase awaiting_phone_home", op)
	}
}

// TestStaleResyncKeepsAwaitingPhoneHome: a resync built from global's
// pre-deploy_complete row must not knock the op off awaiting_phone_home.
func TestStaleResyncKeepsAwaitingPhoneHome(t *testing.T) {
	mac := "52:54:00:15:10:55"
	c, id, opID := awaitingDeploy(t, mac)

	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy,
		Status: model.OpClaimed, Phase: "rsyncing_os", Version: 7,
	})
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("after stale resync: got %d, want BootLocal", got)
	}
}

// TestFailedDeltaLeavesAwaitingPhoneHome: the phone-home timeout's failure
// still applies; keepTerminalWait only blocks phase regressions on an op
// global still shows active.
func TestFailedDeltaLeavesAwaitingPhoneHome(t *testing.T) {
	mac := "52:54:00:15:10:56"
	c, id, opID := awaitingDeploy(t, mac)

	c.ApplyOpDelta(model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy,
		Status: model.OpFailed, Phase: "awaiting_phone_home", Version: 8,
	}, "")
	if op := c.GetOperation(opID.String()); op == nil || op.Status != model.OpFailed {
		t.Fatalf("op = %+v, want status failed", op)
	}
}

// TestRehydratedViewAcceptsEqualSeqOnce reproduces the pop1 rollout of
// 2026-10-24: an older worker decoded snapshot N without a field it didn't
// know and persisted it; after upgrading, the restarted worker rehydrated
// N from disk and dropped global's equal-SeqNum hello reply carrying the
// field, so it ran on the stripped copy indefinitely. The first
// equal-SeqNum snapshot over a disk view must apply; later equal-SeqNum
// snapshots stay no-ops.
func TestRehydratedViewAcceptsEqualSeqOnce(t *testing.T) {
	dbPath := t.TempDir() + "/worker.db"
	id := uuid.New()
	stripped := model.MachineSnapshot{
		ID: id, State: model.MachineStateRunning, Hostname: "spine-1.pop1",
		Interfaces: []model.SnapshotInterface{{MAC: "94:6d:ae:52:8a:64", Type: "physical"}},
	}
	{
		db, err := OpenLocalDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		NewCache(db).ApplySnapshot(&model.SiteSnapshot{SeqNum: 1327, Machines: []model.MachineSnapshot{stripped}})
	}

	db, err := OpenLocalDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCacheWithSerial(db, "9HNZ4Z2")
	if m := c.MachineByID(id); m == nil || m.Console != nil {
		t.Fatalf("precondition: rehydrated machine = %+v, want present without console", m)
	}
	// The hub answers a matching last_seq_num with no_change, so a disk
	// view must ask for the full snapshot.
	if got := c.HelloSeqNum(); got != 0 {
		t.Fatalf("HelloSeqNum over disk view = %d, want 0", got)
	}

	full := stripped
	full.Console = &model.SnapshotConsole{Server: "oob-gw-1.pop1", ServerSerial: "9HNZ4Z2", Port: "ttyS4", Baud: 115200}
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1327, Machines: []model.MachineSnapshot{full}})
	if m := c.MachineByID(id); m == nil || m.Console == nil || m.Console.Port != "ttyS4" {
		t.Fatalf("after equal-seq hello reply over disk view: machine = %+v, want console ttyS4", m)
	}
	if got := c.HelloSeqNum(); got != 1327 {
		t.Fatalf("HelloSeqNum after live apply = %d, want 1327", got)
	}

	// The live view is now authoritative: another equal-SeqNum snapshot is
	// a no-op again (the stale-race guard is unchanged).
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1327, Machines: []model.MachineSnapshot{stripped}})
	if m := c.MachineByID(id); m == nil || m.Console == nil {
		t.Fatalf("second equal-seq snapshot was applied: machine = %+v", m)
	}
}

// TestFreshCacheStillDropsEqualSeq: without a disk view, equal SeqNum is
// still a no-op.
func TestFreshCacheStillDropsEqualSeq(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 7, Machines: []model.MachineSnapshot{{ID: id, Hostname: "a"}}})
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 7, Machines: []model.MachineSnapshot{{ID: id, Hostname: "b"}}})
	if m := c.MachineByID(id); m == nil || m.Hostname != "a" {
		t.Fatalf("equal-seq snapshot applied on a fresh cache: %+v", m)
	}
}
