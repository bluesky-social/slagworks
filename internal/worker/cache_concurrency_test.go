package worker

import (
	"errors"
	"slices"
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

// blockingImageSync is an imageSyncFn that records the SHA of each image
// set it is handed and the most syncs it saw running at once, and blocks
// every sync until release is closed.
type blockingImageSync struct {
	entered chan struct{} // receives once per sync, on entry
	release chan struct{}
	err     error // returned by every sync

	mu      sync.Mutex
	shas    []string
	running int
	maxRun  int
}

func newBlockingImageSync() *blockingImageSync {
	return &blockingImageSync{entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (b *blockingImageSync) sync(_ string, images map[string]*model.Image) error {
	sha := "" // an empty image set
	if img, ok := images["noble-pxe"]; ok {
		sha = img.SHA256
	}
	b.mu.Lock()
	b.shas = append(b.shas, sha)
	b.running++
	b.maxRun = max(b.maxRun, b.running)
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.running--
	b.mu.Unlock()
	return b.err
}

// synced returns the SHAs of the image sets synced so far and the most
// syncs that ran at once.
func (b *blockingImageSync) synced() (shas []string, maxRun int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.shas...), b.maxRun
}

// imageSyncInFlight reports whether c has an image sync requested and not
// yet finished.
func imageSyncInFlight(c *Cache) bool {
	c.imageSyncMu.Lock()
	defer c.imageSyncMu.Unlock()
	return c.imageSyncInFlightLocked()
}

func imageSnap(seq int64, sha string, machines ...model.MachineSnapshot) *model.SiteSnapshot {
	return &model.SiteSnapshot{SeqNum: seq, Machines: machines,
		Images: []model.Image{{Name: "noble-pxe", URL: "https://images.example.net/noble-pxe.tar.gz", SHA256: sha}}}
}

// applyWithin runs ApplySnapshot and fails the test if it has not returned
// within 2s: the image download must not run on the caller's goroutine,
// which in production is the WS read loop.
func applyWithin(t *testing.T, c *Cache, snap *model.SiteSnapshot) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); c.ApplySnapshot(snap) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("ApplySnapshot(seq %d) blocked on the image download", snap.SeqNum)
	}
}

// TestCache_ImageSyncRunsOffTheApplyPath: an apply returns, persisted and
// with its subscribers fired exactly once, while an image download is in
// progress. Downloads never overlap, and applies that arrive during one
// collapse into a single sync of the newest image set.
func TestCache_ImageSyncRunsOffTheApplyPath(t *testing.T) {
	c := NewCache(nil)
	c.ImagesDir = "/does-not-matter-test-seam"
	b := newBlockingImageSync()
	c.imageSyncFn = b.sync
	var fireCount atomic.Int64
	c.OnSnapshotApplied(func() { fireCount.Add(1) })

	applyWithin(t, c, imageSnap(1, "sha-1"))
	<-b.entered // the first download is in progress
	applyWithin(t, c, imageSnap(2, "sha-2"))
	applyWithin(t, c, imageSnap(3, "sha-3"))

	if got := fireCount.Load(); got != 3 {
		t.Fatalf("subscriber fired %d times for 3 applies, want exactly 3", got)
	}
	if got := c.LastSeqNum(); got != 3 {
		t.Fatalf("seqnum during the download: got %d, want 3", got)
	}

	close(b.release)
	<-b.entered // the newest set's sync
	waitFor(t, func() bool { return !imageSyncInFlight(c) })
	shas, maxRun := b.synced()
	if !slices.Equal(shas, []string{"sha-1", "sha-3"}) {
		t.Errorf("synced image sets %q, want the first and then only the newest: [sha-1 sha-3]", shas)
	}
	if maxRun != 1 {
		t.Errorf("%d image syncs ran at once, want 1", maxRun)
	}
}

// TestCache_EmptyImageSetSupersedesWaitingSet: a snapshot that removes every
// image replaces a set still waiting behind a download, so the syncer does
// not go on to download an image the applied snapshot no longer names.
func TestCache_EmptyImageSetSupersedesWaitingSet(t *testing.T) {
	c := NewCache(nil)
	c.ImagesDir = "/does-not-matter-test-seam"
	b := newBlockingImageSync()
	c.imageSyncFn = b.sync

	applyWithin(t, c, imageSnap(1, "sha-1"))
	<-b.entered
	applyWithin(t, c, imageSnap(2, "sha-2"))
	applyWithin(t, c, &model.SiteSnapshot{SeqNum: 3})
	close(b.release)
	<-b.entered
	waitFor(t, func() bool { return !imageSyncInFlight(c) })
	if shas, _ := b.synced(); !slices.Equal(shas, []string{"sha-1", ""}) {
		t.Errorf("synced image sets %q, want [sha-1 \"\"]: the removed image must not be downloaded", shas)
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
	waitFor(t, func() bool { return !imageSyncInFlight(c) })
	if syncCalls.Load() != 1 || fireCount.Load() != 1 {
		t.Fatalf("first apply: sync=%d fire=%d, want 1/1", syncCalls.Load(), fireCount.Load())
	}

	// Stale (SeqNum < current) and equal-SeqNum applies must be no-ops. A
	// request would leave a sync in flight or already counted.
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 3, Images: img})
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 5, Images: img})
	if imageSyncInFlight(c) || syncCalls.Load() != 1 {
		t.Errorf("stale/equal apply requested an image sync: in flight=%v calls=%d, want false/1",
			imageSyncInFlight(c), syncCalls.Load())
	}
	if fireCount.Load() != 1 {
		t.Errorf("stale/equal apply fired subscribers: fires=%d, want 1", fireCount.Load())
	}
	if c.LastSeqNum() != 5 {
		t.Errorf("seqnum moved on stale apply: got %d, want 5", c.LastSeqNum())
	}
}

// TestCache_LifecycleOpsWaitForImageSync: commission and deploy boot the
// image on disk, so while a sync is in flight (the disk can still hold the
// previous release under the same name) a pending lifecycle op is held
// back from every path that would start it: the executor's pending list,
// its re-claim of a claim whose reply was lost, and the boot decision for a
// host that PXEs on its own. Power ops and an op already claimed are not
// held. When the sync ends, whether it succeeded or failed, the executor is
// woken and the ops are released.
func TestCache_LifecycleOpsWaitForImageSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"sync succeeds", nil},
		{"sync fails", errors.New("download returned 503")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(nil)
			c.ImagesDir = "/seam"
			b := newBlockingImageSync()
			b.err = tc.err
			c.imageSyncFn = b.sync

			const mac, claimedMAC = "52:54:00:00:00:31", "52:54:00:00:00:32"
			id, claimedID := uuid.New(), uuid.New()
			machine := func(id uuid.UUID, mac string) model.MachineSnapshot {
				return model.MachineSnapshot{ID: id, State: model.MachineStateCommissioned, PXEEligible: true,
					Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}}}
			}
			applyWithin(t, c, imageSnap(1, "sha-new", machine(id, mac), machine(claimedID, claimedMAC)))
			<-b.entered
			reclaim := model.Operation{ID: uuid.New(), MachineID: claimedID, Type: model.OpDeploy, Status: model.OpPending, Version: 1}
			seedOpsResync(c,
				model.Operation{ID: uuid.New(), MachineID: id, Type: model.OpDeploy, Status: model.OpPending, Version: 1},
				model.Operation{ID: uuid.New(), MachineID: id, Type: model.OpPowerOn, Status: model.OpPending, Version: 1},
				reclaim,
			)
			// The executor claimed reclaim, the reply was lost, and global's
			// claimed row replaced the rollback's pending mark.
			c.MarkOpClaimed(reclaim.ID.String())
			c.RollBackClaim(reclaim.ID.String())
			reclaim.Status, reclaim.ClaimedBy, reclaim.Version = model.OpClaimed, "worker-1", 2
			c.ApplyOpDelta(reclaim, model.MachineStateDeploying)
			select {
			case <-c.OpWake:
			default:
			}

			if got := c.PendingLifecycleOps(); len(got) != 0 {
				t.Errorf("PendingLifecycleOps during the image sync = %d ops, want none: the deploy would boot the previous image", len(got))
			}
			if got := c.ReclaimableOps("worker-1"); len(got) != 0 {
				t.Errorf("ReclaimableOps during the image sync = %d ops, want none: the re-claimed deploy would boot the previous image", len(got))
			}
			if got := c.BootActionForMAC(mac); got != model.BootLocal {
				t.Errorf("BootActionForMAC for a pending deploy during the image sync = %v, want BootLocal", got)
			}
			if got := c.BootActionForMAC(claimedMAC); got != model.BootDeploy {
				t.Errorf("BootActionForMAC for a claimed deploy during the image sync = %v, want BootDeploy: it is already being driven", got)
			}
			if got := c.PendingPowerOps(); len(got) != 1 {
				t.Errorf("PendingPowerOps during the image sync = %d ops, want 1: power ops do not wait for images", len(got))
			}

			close(b.release)
			select {
			case <-c.OpWake:
			case <-time.After(2 * time.Second):
				t.Fatal("the executor was not woken when the image sync ended")
			}
			if got := c.PendingLifecycleOps(); len(got) != 1 {
				t.Errorf("PendingLifecycleOps after the image sync = %d ops, want 1", len(got))
			}
			if got := c.ReclaimableOps("worker-1"); len(got) != 1 {
				t.Errorf("ReclaimableOps after the image sync = %d ops, want 1", len(got))
			}
			if got := c.BootActionForMAC(mac); got != model.BootDeploy {
				t.Errorf("BootActionForMAC for a pending deploy after the image sync = %v, want BootDeploy", got)
			}
		})
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
