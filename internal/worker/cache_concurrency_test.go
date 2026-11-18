package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestCache_ConcurrentReadsDuringApply is the core property of the
// snapshotView decomposition (fable-review Tier 1 #4): pure reads and
// combined reads run concurrently with a stream of ApplySnapshot calls
// without racing (run under -race) and without ever observing a torn
// (machine, op) pair — every machine a reader sees resolves, and if it
// has an active op that op belongs to that machine.
func TestCache_ConcurrentReadsDuringApply(t *testing.T) {
	c := NewCache(nil)

	// Two machines with stable IDs/MACs; each apply flips their op set and
	// seqnum so the writer is genuinely churning the view + overlay.
	id1, id2 := uuid.New(), uuid.New()
	mac1, mac2 := "52:54:00:00:00:01", "52:54:00:00:00:02"
	// Ops now ride the op stream, not the snapshot. mkSnap builds the
	// identity half; a separate op-churn goroutine hammers the op layer
	// (ApplyOpResync) concurrently, so the test still exercises concurrent
	// op-layer writes against view swaps under -race.
	mkSnap := func(seq int64) *model.SiteSnapshot {
		return &model.SiteSnapshot{
			SeqNum: seq,
			Machines: []model.MachineSnapshot{
				{ID: id1, State: model.MachineStateCommissioned, PXEEligible: true,
					Interfaces: []model.SnapshotInterface{{MAC: mac1, Type: "physical"}}},
				{ID: id2, State: model.MachineStateDeploying, PXEEligible: true,
					Interfaces: []model.SnapshotInterface{{MAC: mac2, Type: "physical"}}},
			},
		}
	}
	deployOps := []model.Operation{
		{ID: uuid.New(), MachineID: id1, Type: model.OpDeploy, Status: model.OpPending, Version: 1},
		{ID: uuid.New(), MachineID: id2, Type: model.OpDeploy, Status: model.OpClaimed, Phase: "partitioning", Version: 1},
	}
	c.ApplySnapshot(mkSnap(1))

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: hammer ApplySnapshot with increasing seqnums.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var seq int64 = 2
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.ApplySnapshot(mkSnap(seq))
			seq++
		}
	}()

	// Op-churn writer: flip the op set on/off via the stream concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		withDeploy := true
		for {
			select {
			case <-stop:
				return
			default:
			}
			if withDeploy {
				seedOpsResync(c, deployOps...)
			} else {
				seedOpsResync(c)
			}
			withDeploy = !withDeploy
		}
	}()

	// Readers: pure reads (lock-free) + combined reads (stateMu). None may
	// race or observe an inconsistent view.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.ReservedIPForMAC(mac1)
				_ = c.GetDHCPConfig()
				_ = c.LastSeqNum()
				_ = c.MachineCount()
				// Combined read: a machine that resolves by MAC must also
				// resolve by ID within the same generation.
				if m := c.MachineByMAC(mac2); m != nil && m.ID != id2 {
					t.Errorf("torn read: mac2 resolved to %s, want %s", m.ID, id2)
				}
				_ = c.BootActionForMAC(mac1)
				_ = c.BootActionForMAC(mac2)
				if op := c.DeployOpForMAC(mac1); op != nil && op.MachineID != id1 {
					t.Errorf("torn read: deploy op for mac1 has machine %s, want %s", op.MachineID, id1)
				}
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Final apply lands cleanly and is observable.
	c.ApplySnapshot(mkSnap(1_000_000))
	if got := c.LastSeqNum(); got != 1_000_000 {
		t.Fatalf("final seqnum: got %d, want 1000000", got)
	}
}

// TestCache_ConcurrentOpStateUnderApply exercises the Tier 2 Phase 1 lock
// ownership: opState.mu now guards BOTH the op layer AND the view-pointer
// swap, replacing stateMu's old role. This hammers BootActionForMAC (a
// combined view+op read), ApplySnapshot (view swap + op full-replace under
// opState.mu), and RecordOpUpdate (op-status mutation under opState.mu)
// concurrently under -race. It asserts no data race and no torn read: a
// deploy op a reader observes always belongs to the machine it resolves.
func TestCache_ConcurrentOpStateUnderApply(t *testing.T) {
	c := NewCache(nil)

	id := uuid.New()
	mac := "52:54:00:00:00:aa"
	// Stable op IDs so RecordOpUpdate targets a live op across applies.
	opID1, opID2 := uuid.New(), uuid.New()
	mkSnap := func(seq int64) *model.SiteSnapshot {
		return &model.SiteSnapshot{
			SeqNum: seq,
			Machines: []model.MachineSnapshot{{
				ID: id, State: model.MachineStateDeploying, PXEEligible: true,
				Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
			}},
		}
	}
	c.ApplySnapshot(mkSnap(1))
	// Ops ride the stream now; seed the active set once (identity applies
	// no longer touch it) and let RecordOpUpdate mutate it in place.
	seedOpsResync(c,
		model.Operation{ID: opID1, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Phase: "partitioning"},
		model.Operation{ID: opID2, MachineID: id, Type: model.OpDeploy, Status: model.OpPending},
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer 1: churn the view + op set.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var seq int64 = 2
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.ApplySnapshot(mkSnap(seq))
			seq++
		}
	}()

	// Writer 2: mutate op status in-place (RecordOpUpdate takes opState.mu).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.RecordOpUpdate(opID1.String(), string(model.OpRunning), nil)
			c.MarkOpClaimed(opID2.String())
		}
	}()

	// Readers: combined view+op reads must never tear.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.BootActionForMAC(mac)
				if op := c.DeployOpForMAC(mac); op != nil && op.MachineID != id {
					t.Errorf("torn read: deploy op for mac has machine %s, want %s", op.MachineID, id)
				}
				if m := c.MachineByMAC(mac); m != nil && m.ID != id {
					t.Errorf("torn read: mac resolved to %s, want %s", m.ID, id)
				}
				_ = c.ActiveOpCount()
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	c.ApplySnapshot(mkSnap(1_000_000))
	if got := c.LastSeqNum(); got != 1_000_000 {
		t.Fatalf("final seqnum: got %d, want 1000000", got)
	}
}

// TestCache_SubscriberSingleFireUnderImageSyncOverlap pins the structural
// double-fire fix. The old ApplySnapshot released its lock across the
// (minutes-long) image sync, so a second apply arriving in that window
// could swap in and fire subscribers twice for overlapping applies. Now
// applyMu serializes the whole method, so N applies fire subscribers
// exactly N times even when a second apply is attempted while the first
// is blocked inside image sync.
func TestCache_SubscriberSingleFireUnderImageSyncOverlap(t *testing.T) {
	c := NewCache(nil)
	c.ImagesDir = "/does-not-matter-test-seam"

	// A slow image sync that blocks until we release it, so we can force a
	// second ApplySnapshot to arrive mid-sync of the first.
	syncEntered := make(chan struct{}, 1)
	releaseSync := make(chan struct{})
	var syncCalls atomic.Int64
	c.imageSyncFn = func(_ string, _ map[string]*model.Image) error {
		syncCalls.Add(1)
		select {
		case syncEntered <- struct{}{}:
		default:
		}
		<-releaseSync
		return nil
	}

	var fireCount atomic.Int64
	c.OnSnapshotApplied(func() { fireCount.Add(1) })

	img := []model.Image{{Name: "noble-pxe", URL: "s3://x", SHA256: "abc"}}

	// Apply #1: enters image sync and blocks there.
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Images: img})
	}()
	<-syncEntered // apply #1 is now parked inside image sync

	// Apply #2: attempted while #1 holds applyMu inside image sync. It must
	// block on applyMu (not swap in and double-fire).
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 2, Images: img})
	}()

	// Give apply #2 a chance to (wrongly) proceed if the lock were released.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done2:
		t.Fatal("apply #2 completed while apply #1 was mid-image-sync; applyMu not held across sync")
	default:
	}

	// Release #1's sync; both applies now complete in order.
	close(releaseSync)
	<-done1
	<-done2

	if got := fireCount.Load(); got != 2 {
		t.Fatalf("subscriber fired %d times for 2 applies, want exactly 2 (double-fire?)", got)
	}
	if got := syncCalls.Load(); got != 2 {
		t.Fatalf("image sync ran %d times for 2 applies, want 2", got)
	}
	if got := c.LastSeqNum(); got != 2 {
		t.Fatalf("final seqnum: got %d, want 2", got)
	}
}

// TestCache_StaleApplyNoSideEffect confirms the seqnum gate rejects a
// stale snapshot before any side effect: no view swap, no image sync, no
// subscriber fire.
func TestCache_StaleApplyNoSideEffect(t *testing.T) {
	c := NewCache(nil)
	c.ImagesDir = "/seam"
	var syncCalls, fireCount atomic.Int64
	c.imageSyncFn = func(_ string, _ map[string]*model.Image) error {
		syncCalls.Add(1)
		return nil
	}
	c.OnSnapshotApplied(func() { fireCount.Add(1) })

	img := []model.Image{{Name: "noble-pxe", SHA256: "abc"}}
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 5, Images: img})
	if syncCalls.Load() != 1 || fireCount.Load() != 1 {
		t.Fatalf("first apply: sync=%d fire=%d, want 1/1", syncCalls.Load(), fireCount.Load())
	}

	// Stale (SeqNum < current) and equal-SeqNum applies must be no-ops.
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 3, Images: img})
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 5, Images: img})
	if syncCalls.Load() != 1 {
		t.Errorf("stale/equal apply ran image sync: calls=%d, want 1", syncCalls.Load())
	}
	if fireCount.Load() != 1 {
		t.Errorf("stale/equal apply fired subscribers: fires=%d, want 1", fireCount.Load())
	}
	if c.LastSeqNum() != 5 {
		t.Errorf("seqnum moved on stale apply: got %d, want 5", c.LastSeqNum())
	}
}

// TestCache_ConditionalOverlayClear pins the Tier-2 re-wipe-window fix
// (the crux): an identity apply clears the op-driven machineState overlay
// for a machine ONLY when the applied baseline State equals the overlay
// value. RecordPhoneHome flips the overlay to running; a snapshot STILL
// reporting deploying must NOT clear it (that would open the
// `baseline=deploying + no active op → re-wipe` window); only a snapshot
// carrying baseline=running clears it.
func TestCache_ConditionalOverlayClear(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:ab:cd:11"
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 1,
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateDeploying, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}},
	})
	seedOpsResync(c, model.Operation{
		ID: uuid.New(), MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed, Phase: "awaiting_phone_home",
	})

	c.RecordPhoneHome(id)
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("post phone-home: running overlay not applied, got %+v", got)
	}

	// Case 1: baseline still deploying — overlay must PERSIST (the
	// conditional clear's whole point; unconditional clear here was the
	// re-wipe bug).
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 2,
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateDeploying, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}},
	})
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("baseline=deploying must NOT clear the running overlay, got %+v", got)
	}

	// Case 2: baseline caught up to running — overlay clears (baseline ==
	// overlay), and the machine reads running from the baseline.
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 3,
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateRunning, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}},
	})
	if got := c.MachineByID(id); got == nil || got.State != model.MachineStateRunning {
		t.Fatalf("baseline=running should clear overlay and read running, got %+v", got)
	}
}
