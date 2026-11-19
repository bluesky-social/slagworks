package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Property test for the Registry's per-slug rebuild actors under generated
// schedules of Invalidate / RebuildAndWait / Stop, with rebuilds that take
// time and fail on a schedule. It runs inside a testing/synctest bubble, so
// time is virtual and every interleaving of timers is deterministic and
// instant.
//
// Properties:
//
//   - rebuilds for one slug never overlap (SeqNum ownership depends on it);
//   - no lost invalidation: once the schedule goes quiet, the last
//     successful rebuild of a slug started at or after its last Invalidate;
//   - a RebuildAndWait caller never receives the result of a rebuild that
//     started before it called (a hello must not be answered from a build
//     that predates it);
//   - every RebuildAndWait call returns, including across Stop, and nothing
//     rebuilds after Stop returns;
//   - a rebuild that keeps failing is retried until it succeeds.

type propRegEvent struct {
	At   time.Duration
	Kind int // 0 invalidate, 1 wait, 2 wait with a short deadline
	Slug string
}

type propRebuildErr struct{ idx int }

func (e propRebuildErr) Error() string { return fmt.Sprintf("injected rebuild failure #%d", e.idx) }

type propRegRun struct {
	Slug       string
	Start, End time.Duration
	Failed     bool
}

func TestPropRegistrySchedule(t *testing.T) {
	var stats struct {
		cases, invalidations, waiters, rebuilds, failures, deadline, midRebuildInv, stopped int
	}
	rapid.Check(t, func(rt *rapid.T) {
		slugs := []string{"a", "b"}
		n := rapid.IntRange(1, 25).Draw(rt, "nEvents")
		events := make([]propRegEvent, n)
		for i := range events {
			events[i] = propRegEvent{
				// Clustered offsets so events land inside debounce windows
				// and inside running rebuilds, not only between them.
				At:   time.Duration(rapid.SampledFrom([]int{0, 0, 1, 5, 20, 49, 50, 51, 120, 400}).Draw(rt, "gapMs")) * time.Millisecond,
				Kind: rapid.SampledFrom([]int{0, 0, 0, 1, 1, 2}).Draw(rt, "kind"),
				Slug: rapid.SampledFrom(slugs).Draw(rt, "slug"),
			}
		}
		// Outcome schedule per rebuild call (in call order across slugs);
		// calls past the end succeed instantly.
		durs := rapid.SliceOfN(rapid.SampledFrom([]int{0, 0, 10, 60, 200}), 0, 12).Draw(rt, "rebuildMs")
		fails := rapid.SliceOfN(rapid.Bool(), 0, 6).Draw(rt, "rebuildFails")
		stopEarly := rapid.IntRange(0, 5).Draw(rt, "stopEarly") == 0
		stats.cases++

		rapid.SyncTest(rt, func(rt *rapid.T) {
			// Virtual time stands still while goroutines are runnable, so
			// several observations can share an instant. now breaks the
			// tie by a nanosecond in observation order, which is what the
			// started-before / started-after checks below compare.
			base := time.Now()
			var clockMu sync.Mutex
			var lastStamp time.Duration
			now := func() time.Duration {
				clockMu.Lock()
				defer clockMu.Unlock()
				stamp := time.Since(base)
				if stamp <= lastStamp {
					stamp = lastStamp + time.Nanosecond
				}
				lastStamp = stamp
				return stamp
			}

			var mu sync.Mutex
			var runs []propRegRun
			inflight := map[string]int{}
			stoppedAt := time.Duration(-1)
			rebuild := func(ctx context.Context, slug string, gen int64) (model.SiteSnapshot, error) {
				mu.Lock()
				idx := len(runs)
				runs = append(runs, propRegRun{Slug: slug, Start: now(), End: -1})
				inflight[slug]++
				if inflight[slug] > 1 {
					mu.Unlock()
					panic("two rebuilds of slug " + slug + " overlap")
				}
				if stoppedAt >= 0 {
					mu.Unlock()
					panic("rebuild started after Stop returned")
				}
				mu.Unlock()
				if idx < len(durs) && durs[idx] > 0 {
					select {
					case <-time.After(time.Duration(durs[idx]) * time.Millisecond):
					case <-ctx.Done():
					}
				}
				failed := idx < len(fails) && fails[idx]
				mu.Lock()
				runs[idx].End, runs[idx].Failed = now(), failed
				inflight[slug]--
				mu.Unlock()
				if failed {
					return model.SiteSnapshot{}, propRebuildErr{idx}
				}
				return model.SiteSnapshot{SeqNum: int64(idx) + 1}, ctx.Err()
			}

			reg := newRegistry(rebuild).WithDebounce(50*time.Millisecond).WithRetry(100*time.Millisecond, 400*time.Millisecond)
			reg.SetLogger(slog.New(slog.DiscardHandler))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reg.Start(ctx)

			lastInv := map[string]time.Duration{}
			type waitRes struct {
				slug   string
				called time.Duration
				snap   model.SiteSnapshot
				err    error
				done   bool
			}
			var waits []*waitRes
			var wg sync.WaitGroup
			for _, ev := range events {
				time.Sleep(ev.At)
				switch ev.Kind {
				case 0:
					mu.Lock()
					if inflight[ev.Slug] > 0 {
						stats.midRebuildInv++
					}
					mu.Unlock()
					stats.invalidations++
					lastInv[ev.Slug] = now()
					reg.Invalidate(ev.Slug)
				default:
					stats.waiters++
					w := &waitRes{slug: ev.Slug, called: now()}
					waits = append(waits, w)
					wctx, wcancel := context.WithCancel(ctx)
					if ev.Kind == 2 {
						wctx, wcancel = context.WithTimeout(ctx, 30*time.Millisecond)
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer wcancel()
						snap, err := reg.RebuildAndWait(wctx, w.slug)
						mu.Lock()
						w.snap, w.err, w.done = snap, err, true
						mu.Unlock()
					}()
				}
			}

			if stopEarly {
				stats.stopped++
				reg.Stop()
				mu.Lock()
				stoppedAt = now()
				mu.Unlock()
			} else {
				// Quiesce: far longer than debounce + every scheduled
				// rebuild + the capped retry backoff for every failure.
				time.Sleep(time.Minute)
			}
			wg.Wait() // every waiter returns, stopped or not
			synctest.Wait()

			mu.Lock()
			defer mu.Unlock()
			stats.rebuilds += len(runs)
			for _, w := range waits {
				if !w.done {
					rt.Fatalf("RebuildAndWait(%s) called at %v never returned", w.slug, w.called)
				}
				var re propRebuildErr
				switch {
				case w.err == nil:
					idx := int(w.snap.SeqNum) - 1
					if idx < 0 || idx >= len(runs) || runs[idx].Slug != w.slug {
						rt.Fatalf("waiter for %s got a snapshot that is not from a rebuild of that slug: %+v", w.slug, w.snap)
					}
					if runs[idx].Start < w.called {
						rt.Fatalf("waiter for %s called at %v was answered from rebuild #%d, which started earlier at %v", w.slug, w.called, idx, runs[idx].Start)
					}
				case errors.As(w.err, &re):
					if runs[re.idx].Slug != w.slug || runs[re.idx].Start < w.called {
						rt.Fatalf("waiter for %s called at %v got the error of rebuild #%d (slug %s, started %v)", w.slug, w.called, re.idx, runs[re.idx].Slug, runs[re.idx].Start)
					}
				case errors.Is(w.err, context.DeadlineExceeded):
					stats.deadline++
				case errors.Is(w.err, ErrRegistryStopped), errors.Is(w.err, context.Canceled):
					if !stopEarly {
						rt.Fatalf("waiter for %s got %v but the registry was never stopped", w.slug, w.err)
					}
				default:
					rt.Fatalf("waiter for %s got unexpected error %v", w.slug, w.err)
				}
			}
			if !stopEarly {
				lastOK := map[string]time.Duration{}
				lastRun := map[string]propRegRun{}
				for _, r := range runs {
					if r.End < 0 {
						rt.Fatalf("rebuild of %s started at %v never finished", r.Slug, r.Start)
					}
					if r.Failed {
						stats.failures++
					} else {
						lastOK[r.Slug] = r.Start
					}
					lastRun[r.Slug] = r
				}
				var keys []string
				for s := range lastInv {
					keys = append(keys, s)
				}
				sort.Strings(keys)
				for _, s := range keys {
					ok, seen := lastOK[s]
					if !seen || ok < lastInv[s] {
						rt.Fatalf("lost invalidation: %s was last invalidated at %v but its last successful rebuild started at %v (seen=%v); runs=%+v", s, lastInv[s], ok, seen, runs)
					}
				}
				for s, r := range lastRun {
					if r.Failed {
						rt.Fatalf("slug %s: the last rebuild failed and was never retried; runs=%+v", s, runs)
					}
				}
				reg.Stop()
				stoppedAt = now()
			}
		})
	})
	t.Logf("coverage: %+v", stats)
}
