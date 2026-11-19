package snapshot

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// fakeRebuild captures rebuild invocations, optionally firing a hook
// during execution and returning a configured error. Fields that the
// test writes while the rebuild goroutine may be reading (`err`,
// `duringHook`, `delay`) use atomics to keep -race happy.
type fakeRebuild struct {
	calls      atomic.Int32
	err        atomic.Pointer[error]
	duringHook atomic.Pointer[func()]
	delay      atomic.Int64 // nanoseconds
}

func newFakeRebuild() *fakeRebuild { return &fakeRebuild{} }

func (f *fakeRebuild) setErr(err error) {
	if err == nil {
		f.err.Store(nil)
		return
	}
	f.err.Store(&err)
}

func (f *fakeRebuild) setHook(h func()) {
	if h == nil {
		f.duringHook.Store(nil)
		return
	}
	f.duringHook.Store(&h)
}

func (f *fakeRebuild) fn(ctx context.Context, slug string, gen int64) (model.SiteSnapshot, error) {
	f.calls.Add(1)
	if hp := f.duringHook.Load(); hp != nil {
		(*hp)()
	}
	if d := time.Duration(f.delay.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return model.SiteSnapshot{}, ctx.Err()
		}
	}
	if ep := f.err.Load(); ep != nil {
		return model.SiteSnapshot{}, *ep
	}
	return model.SiteSnapshot{}, nil
}

// waitForCalls polls until rb.calls >= want, or times out.
func waitForCalls(t *testing.T, rb *fakeRebuild, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rb.calls.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d rebuilds, saw %d", want, rb.calls.Load())
}

func TestRegistryDebouncesBurst(t *testing.T) {
	rb := newFakeRebuild()
	reg := newRegistry(rb.fn).WithDebounce(30 * time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	// 10 invalidations inside the debounce window collapse to 1 rebuild.
	for range 10 {
		reg.Invalidate("pop3")
	}
	waitForCalls(t, rb, 1, 500*time.Millisecond)
	// Give extra time to confirm no second rebuild erroneously fires.
	time.Sleep(100 * time.Millisecond)
	if got := rb.calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 rebuild, got %d", got)
	}
}

func TestRegistryQuietReinvalidationProducesTwoRebuilds(t *testing.T) {
	rb := newFakeRebuild()
	reg := newRegistry(rb.fn).WithDebounce(30 * time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	reg.Invalidate("pop3")
	waitForCalls(t, rb, 1, 500*time.Millisecond)
	reg.Invalidate("pop3")
	waitForCalls(t, rb, 2, 500*time.Millisecond)
}

// TestRegistrySlidingDebounce asserts the window RESTARTS on each new
// trigger: events spaced tighter than the debounce keep coalescing
// into one rebuild even when their total span exceeds the window.
func TestRegistrySlidingDebounce(t *testing.T) {
	rb := newFakeRebuild()
	reg := newRegistry(rb.fn).WithDebounce(60 * time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	// 6 triggers, 20ms apart: 120ms total span (2x the window), but
	// no 60ms of quiet until the last one — one rebuild.
	for range 6 {
		reg.Invalidate("pop3")
		time.Sleep(20 * time.Millisecond)
	}
	waitForCalls(t, rb, 1, 500*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if got := rb.calls.Load(); got != 1 {
		t.Fatalf("sliding debounce should coalesce the burst into 1 rebuild, got %d", got)
	}
}

func TestRegistryMidRebuildInvalidationCausesReRun(t *testing.T) {
	rb := newFakeRebuild()
	rb.delay.Store(int64(80 * time.Millisecond))
	var reg *Registry
	rb.setHook(func() {
		// Re-invalidate exactly once, during the first rebuild.
		if rb.calls.Load() == 1 {
			reg.Invalidate("pop3")
		}
	})
	reg = newRegistry(rb.fn).WithDebounce(20 * time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	reg.Invalidate("pop3")
	// The mid-rebuild trigger parks in the capacity-1 channel and is
	// picked up right after the first rebuild — the torn-read
	// follow-up pass. Expect >= 2.
	waitForCalls(t, rb, 2, 2*time.Second)
}

// TestRegistryRetryBackoffCadence pins the plan's retry contract: on
// persistent failure the actor retries at the backoff cadence (initial,
// doubling, capped) — NOT at debounce cadence, and NOT waiting for the
// reconcile tick.
func TestRegistryRetryBackoffCadence(t *testing.T) {
	rb := newFakeRebuild()
	rb.setErr(errors.New("netbox down"))
	// debounce 1ms so it can't mask the backoff; initial 50ms, cap 100ms.
	reg := newRegistry(rb.fn).WithDebounce(time.Millisecond).WithRetry(50*time.Millisecond, 100*time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	start := time.Now()
	reg.Invalidate("pop3")

	// Call 1 fires ~immediately (post-debounce). Retries then follow at
	// 50ms, 100ms, 100ms, ... (doubling capped at 100ms). Four calls
	// need >= 50+100+100 = 250ms; debounce-cadence retries would need
	// only ~4ms and reconcile-gated retries would never get there.
	waitForCalls(t, rb, 4, 3*time.Second)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("4 failing rebuilds finished in %v — retries are not honoring backoff (expected >= 250ms)", elapsed)
	}

	// A healthy rebuild ends the retry loop.
	rb.setErr(nil)
	prev := rb.calls.Load()
	waitForCalls(t, rb, prev+1, 2*time.Second)
	time.Sleep(200 * time.Millisecond)
	if got := rb.calls.Load(); got != prev+1 {
		t.Fatalf("retry loop kept firing after a successful rebuild: %d extra calls", got-prev-1)
	}
}

// TestRegistryMidRebuildErrorReinvalidation guards the case where an
// Invalidate lands during a FAILING rebuild: the trigger parks in the
// channel and the loop must service it (or the retry) rather than
// losing the invalidation until the reconcile tick.
func TestRegistryMidRebuildErrorReinvalidation(t *testing.T) {
	rb := newFakeRebuild()
	rb.delay.Store(int64(80 * time.Millisecond))
	rb.setErr(errors.New("boom"))
	var reg *Registry
	rb.setHook(func() {
		if rb.calls.Load() == 1 {
			reg.Invalidate("pop3")
		}
	})
	reg = newRegistry(rb.fn).WithDebounce(20*time.Millisecond).WithRetry(20*time.Millisecond, 100*time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	reg.Invalidate("pop3")
	waitForCalls(t, rb, 2, 2*time.Second)
}

// TestRegistryStopDoesNotPanic ensures Stop is clean and post-Stop
// invalidations are inert (no goroutine spawn, no rebuild).
func TestRegistryStopDoesNotPanic(t *testing.T) {
	rb := newFakeRebuild()
	reg := newRegistry(rb.fn).WithDebounce(20 * time.Millisecond)
	reg.Start(context.Background())

	reg.Invalidate("pop3")
	waitForCalls(t, rb, 1, 500*time.Millisecond)
	reg.Stop()

	// Post-Stop invalidations must be inert.
	reg.Invalidate("pop3")
	time.Sleep(80 * time.Millisecond)
	if got := rb.calls.Load(); got != 1 {
		t.Fatalf("post-Stop Invalidate triggered a rebuild; got %d calls", got)
	}
}

func TestRegistryDirtyGenObservable(t *testing.T) {
	rb := newFakeRebuild()
	reg := newRegistry(rb.fn).WithDebounce(1 * time.Second)
	reg.Start(context.Background())
	defer reg.Stop()

	if reg.DirtyGen("pop3") != 0 {
		t.Fatal("new site should have dirtyGen 0")
	}
	reg.Invalidate("pop3")
	reg.Invalidate("pop3")
	reg.Invalidate("pop3")
	if got := reg.DirtyGen("pop3"); got != 3 {
		t.Fatalf("dirtyGen should be 3 after 3 invalidates, got %d", got)
	}
}

// TestRegistryWaitersBatch asserts the hello path's coalescing: N
// concurrent RebuildAndWait callers for a cold slug share ONE rebuild
// and all receive its snapshot. This replaces the hub's deleted
// singleflight (formerly TestHub_ConcurrentHelloCoalescesRebuild).
func TestRegistryWaitersBatch(t *testing.T) {
	rb := newFakeRebuild()
	reg := newRegistry(rb.fn).WithDebounce(50 * time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	// All 5 waiters enqueue within the debounce window, so the loop
	// drains them into one batch before the single rebuild.
	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, errs[i] = reg.RebuildAndWait(ctx, "pop3")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("waiter %d: %v", i, err)
		}
	}
	if got := rb.calls.Load(); got != 1 {
		t.Fatalf("expected 5 waiters to batch onto 1 rebuild, got %d rebuilds", got)
	}
}

// TestRegistryWaiterReceivesError asserts a failing rebuild propagates
// its error to the waiters (the hub turns it into a hello error reply).
func TestRegistryWaiterReceivesError(t *testing.T) {
	rb := newFakeRebuild()
	rb.setErr(errors.New("netbox down"))
	reg := newRegistry(rb.fn).WithDebounce(time.Millisecond).WithRetry(time.Hour, time.Hour)
	reg.Start(context.Background())
	defer reg.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := reg.RebuildAndWait(ctx, "pop3")
	if err == nil || err.Error() != "netbox down" {
		t.Fatalf("expected rebuild error to reach the waiter, got %v", err)
	}
}

// TestRegistryWaiterTimeoutDoesNotBlockActor: a waiter that gives up
// (ctx expiry) must not wedge the actor — later callers still get
// served. This pins the buffered one-shot channel contract.
func TestRegistryWaiterTimeoutDoesNotBlockActor(t *testing.T) {
	rb := newFakeRebuild()
	rb.delay.Store(int64(150 * time.Millisecond))
	reg := newRegistry(rb.fn).WithDebounce(time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	// First caller times out mid-rebuild and leaves.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err := reg.RebuildAndWait(ctx, "pop3")
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}

	// The actor must still be alive: a second caller succeeds.
	rb.delay.Store(0)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := reg.RebuildAndWait(ctx2, "pop3"); err != nil {
		t.Fatalf("actor wedged after abandoned waiter: %v", err)
	}
}

// TestRegistryStopUnblocksWaiters pins shutdown step 3: a blocked
// RebuildAndWait caller gets ErrRegistryStopped (or the cancelled
// rebuild's ctx error), never a hang.
func TestRegistryStopUnblocksWaiters(t *testing.T) {
	rb := newFakeRebuild()
	rb.delay.Store(int64(10 * time.Second)) // rebuild would outlive the test without shutdown ctx-cancel
	reg := newRegistry(rb.fn).WithDebounce(time.Millisecond)
	reg.Start(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := reg.RebuildAndWait(context.Background(), "pop3")
		done <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the waiter enqueue
	reg.Stop()

	select {
	case err := <-done:
		if !errors.Is(err, ErrRegistryStopped) && !errors.Is(err, context.Canceled) {
			t.Fatalf("expected shutdown error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RebuildAndWait caller still blocked after Stop")
	}
}

// TestRegistryB1InterleavingImpossible is the reason this type exists:
// fable-review B1 was two rebuild paths (invalidator + hub hello
// singleflight) interleaving gather/bump for the same slug, letting a
// stale-content snapshot commit with a higher SeqNum. With one actor
// per slug, EVERY trigger source serializes: rebuilds never overlap
// and bump order equals gather order by construction.
func TestRegistryB1InterleavingImpossible(t *testing.T) {
	var mu sync.Mutex
	var inFlight, maxInFlight int
	seq := int64(0)
	var seqs []int64

	rebuild := func(ctx context.Context, slug string, gen int64) (model.SiteSnapshot, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()

		time.Sleep(10 * time.Millisecond) // widen any would-be overlap window

		mu.Lock()
		seq++ // stands in for BumpSiteSnapshotMeta's sequence draw
		s := seq
		seqs = append(seqs, s)
		inFlight--
		mu.Unlock()
		return model.SiteSnapshot{SeqNum: s}, nil
	}

	reg := newRegistry(rebuild).WithDebounce(time.Millisecond)
	reg.Start(context.Background())
	defer reg.Stop()

	// Hammer the same slug from both trigger sources concurrently:
	// invalidations (webhook path) and RebuildAndWait (hello path).
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			reg.Invalidate("pop3")
		})
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = reg.RebuildAndWait(ctx, "pop3")
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if maxInFlight != 1 {
		t.Fatalf("B1: %d rebuilds overlapped for one slug; actor must serialize", maxInFlight)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("B1: SeqNum regressed at index %d: %v", i, seqs)
		}
	}
}

// TestRegistrySlugsIndependent: actors are per-slug; a slow rebuild on
// one slug must not delay another's.
func TestRegistrySlugsIndependent(t *testing.T) {
	slow := make(chan struct{})
	var fastDone atomic.Bool
	rebuild := func(ctx context.Context, slug string, gen int64) (model.SiteSnapshot, error) {
		if slug == "slow" {
			select {
			case <-slow:
			case <-ctx.Done():
			}
			return model.SiteSnapshot{}, nil
		}
		fastDone.Store(true)
		return model.SiteSnapshot{}, nil
	}
	reg := newRegistry(rebuild).WithDebounce(time.Millisecond)
	reg.Start(context.Background())
	defer func() { close(slow); reg.Stop() }()

	reg.Invalidate("slow")
	time.Sleep(20 * time.Millisecond) // slow actor is now stuck in its rebuild
	reg.Invalidate("fast")

	deadline := time.Now().Add(2 * time.Second)
	for !fastDone.Load() {
		if time.Now().After(deadline) {
			t.Fatal("fast slug's rebuild blocked behind slow slug's")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
