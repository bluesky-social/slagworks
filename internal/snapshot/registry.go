package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ErrRegistryStopped is returned to RebuildAndWait callers when the
// registry shuts down before (or while) their rebuild runs.
var ErrRegistryStopped = errors.New("snapshot registry stopped")

// RebuildFunc is the bumping rebuild path (gather → hash → bump →
// push). Only the Registry holds one — NewRegistry receives it from
// the Dispatcher's unexported rebuild method, so no code outside this
// package can bump a snapshot SeqNum except through an actor.
type RebuildFunc func(ctx context.Context, slug string, gen int64) (model.SiteSnapshot, error)

// Registry owns one rebuild actor per config slug. All rebuild
// triggers — webhook invalidations, worker hellos, reconcile ticks,
// op-claim pushes — become events on the slug's actor, so rebuilds
// for a slug are strictly sequential and a snapshot's SeqNum order
// equals its gather-time order by construction.
//
// SeqNum for a slug is owned by that slug's actor, within one
// process. BumpSiteSnapshotMeta stays a plain increment because, and
// only while, (a) the bumping rebuild path is unexported and
// single-holder, and (b) global runs single-replica. If either
// changes, per-slug serialization needs a pg advisory lock instead.
type Registry struct {
	rebuild      RebuildFunc
	debounce     time.Duration
	retryInitial time.Duration
	retryMax     time.Duration
	log          *slog.Logger

	mu     sync.Mutex
	actors map[string]*actor
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// actor is the per-slug rebuild owner. Its goroutine (Registry.run)
// is the only caller of the RebuildFunc for its slug.
type actor struct {
	slug string
	// trigger coalesces invalidation bursts: capacity 1, non-blocking
	// sends. A trigger arriving during a rebuild parks here and is
	// picked up immediately after, producing the follow-up pass that
	// mitigates torn reads.
	trigger chan struct{}
	// waiters carries hello-style callers awaiting a rebuild result.
	// Each element is a buffered one-shot channel; the actor's
	// delivery is non-blocking, so a caller that timed out and left
	// can never block the actor.
	waiters chan chan waitResult
	// dirtyGen counts triggers for telemetry (the dirty_gen metric,
	// passed to the RebuildFunc as gen). It plays no control-flow
	// role — coalescing is the trigger channel's job.
	dirtyGen atomic.Int64
}

type waitResult struct {
	snap model.SiteSnapshot
	err  error
}

// NewRegistry constructs a Registry driving the dispatcher's bumping
// rebuild path. This is the only way to obtain that path — see the
// ownership note on Registry.
func NewRegistry(d *Dispatcher) *Registry {
	return newRegistry(d.rebuild)
}

// newRegistry is the injectable core, used directly by tests.
func newRegistry(rebuild RebuildFunc) *Registry {
	return &Registry{
		rebuild:      rebuild,
		debounce:     500 * time.Millisecond,
		retryInitial: 5 * time.Second,
		retryMax:     5 * time.Minute, // the reconcile interval
		log:          slog.Default(),
		actors:       make(map[string]*actor),
	}
}

// WithDebounce overrides the debounce window. Tests use this to speed
// up convergence — production should stick with the default.
func (r *Registry) WithDebounce(d time.Duration) *Registry {
	r.debounce = d
	return r
}

// WithRetry overrides the error-retry backoff bounds (initial delay,
// doubling per consecutive failure, capped at max). Tests only.
func (r *Registry) WithRetry(initial, max time.Duration) *Registry {
	r.retryInitial = initial
	r.retryMax = max
	return r
}

func (r *Registry) SetLogger(log *slog.Logger) { r.log = log }

// Start begins the registry with the given parent context. Actors are
// spawned lazily per slug; all of them stop when the parent context is
// cancelled or Stop is called. Must be called before Invalidate /
// RebuildAndWait.
func (r *Registry) Start(parent context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ctx, r.cancel = context.WithCancel(parent)
}

// Stop shuts the registry down: (1) new triggers and waiters are
// rejected, (2) in-flight rebuilds see their ctx cancelled, (3) each
// actor drains its queued waiters with ErrRegistryStopped and stops
// its timers, (4) Stop returns once every actor goroutine has exited.
// A blocked RebuildAndWait caller always gets an answer: a result, its
// own ctx error, or ErrRegistryStopped.
func (r *Registry) Stop() {
	// Cancel under mu so actorFor can't wg.Add after wg.Wait returns:
	// either the spawn happens-before the cancel (Wait covers it), or
	// actorFor observes the cancelled ctx and doesn't spawn.
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// actorFor returns the slug's actor, spawning it on first use. Actors
// are never garbage-collected: site-configs are single-digit and
// deletion is rare, so an idle goroutine per deleted slug until
// restart is acceptable.
func (r *Registry) actorFor(slug string) *actor {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a, ok := r.actors[slug]; ok {
		return a
	}
	a := &actor{
		slug:    slug,
		trigger: make(chan struct{}, 1),
		waiters: make(chan chan waitResult, 64),
	}
	r.actors[slug] = a
	if r.ctx.Err() == nil {
		r.wg.Add(1)
		go r.run(a)
	}
	return a
}

// Invalidate marks a slug dirty. Non-blocking from any goroutine: the
// trigger channel has capacity 1, so a burst coalesces into one
// pending event (plus a catch-up pass if events land mid-rebuild).
func (r *Registry) Invalidate(slug string) {
	if slug == "" {
		// Callers that can't resolve a claiming config (e.g. a delete
		// for a device we don't track) pass "" deliberately; the next
		// reconcile tick refreshes the affected configs anyway.
		return
	}
	r.mu.Lock()
	started := r.ctx != nil && r.ctx.Err() == nil
	r.mu.Unlock()
	if !started {
		return
	}
	a := r.actorFor(slug)
	a.dirtyGen.Add(1)
	select {
	case a.trigger <- struct{}{}:
	default:
	}
}

// RebuildAndWait enqueues a waiter on the slug's actor and blocks
// until the next rebuild completes, returning its snapshot or error.
// Concurrent callers for the same slug batch onto one rebuild. Used by
// the WS hub's cold hello path; the caller's ctx bounds only the wait,
// not the rebuild itself (which is shared and runs under the
// registry's lifecycle ctx).
func (r *Registry) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	if slug == "" {
		return model.SiteSnapshot{}, errors.New("empty site slug")
	}
	r.mu.Lock()
	started := r.ctx != nil
	stopped := started && r.ctx.Err() != nil
	r.mu.Unlock()
	if !started || stopped {
		return model.SiteSnapshot{}, ErrRegistryStopped
	}
	a := r.actorFor(slug)
	ch := make(chan waitResult, 1) // buffered: the actor's delivery can't block on us
	select {
	case a.waiters <- ch:
	case <-ctx.Done():
		return model.SiteSnapshot{}, ctx.Err()
	case <-r.ctx.Done():
		return model.SiteSnapshot{}, ErrRegistryStopped
	}
	select {
	case res := <-ch:
		return res.snap, res.err
	case <-ctx.Done():
		return model.SiteSnapshot{}, ctx.Err()
	case <-r.ctx.Done():
		return model.SiteSnapshot{}, ErrRegistryStopped
	}
}

// run is the actor loop: the single owner of rebuilds for a.slug.
//
//	wait for a trigger, a waiter, or a due retry
//	debounce (sliding window; triggers restart it) — skipped for retries
//	drain queued waiters into a batch
//	rebuild; deliver the result to the batch
//	on error, schedule one retry (5s doubling, capped at retryMax)
//	a trigger that arrived during the rebuild is picked up immediately
func (r *Registry) run(a *actor) {
	defer r.wg.Done()

	backoff := r.retryInitial
	var retryTimer *time.Timer
	var pending []chan waitResult

	stopTimer := func() {
		if retryTimer != nil {
			retryTimer.Stop()
			retryTimer = nil
		}
	}
	shutdown := func() {
		stopTimer()
		// Drain queued waiters with the shutdown error. Belt and
		// suspenders — RebuildAndWait callers also select on the
		// registry ctx — but it makes the guarantee local.
		for _, w := range pending {
			w <- waitResult{err: ErrRegistryStopped}
		}
		for {
			select {
			case w := <-a.waiters:
				w <- waitResult{err: ErrRegistryStopped}
			default:
				return
			}
		}
	}

	for {
		var retryC <-chan time.Time
		if retryTimer != nil {
			retryC = retryTimer.C
		}
		retryFired := false
		select {
		case <-r.ctx.Done():
			shutdown()
			return
		case <-a.trigger:
		case w := <-a.waiters:
			pending = append(pending, w)
		case <-retryC:
			retryFired = true
			retryTimer = nil
		}

		// Sliding debounce: coalesce triggers until r.debounce of
		// quiet. Retries skip it — the backoff already spaced them.
		// Waiters arriving meanwhile queue in a.waiters (buffered)
		// and are drained below; they don't restart the window.
		if !retryFired && !r.debounceWait(a) {
			shutdown()
			return
		}
		stopTimer() // rebuilding now; any pending retry is moot

		for {
			select {
			case w := <-a.waiters:
				pending = append(pending, w)
				continue
			default:
			}
			break
		}

		gen := a.dirtyGen.Load()
		snap, err := r.rebuild(r.ctx, a.slug, gen)

		for _, w := range pending {
			w <- waitResult{snap: snap, err: err} // buffered one-shot; never blocks
		}
		pending = nil

		if err != nil {
			if r.ctx.Err() != nil {
				shutdown()
				return
			}
			r.log.Error("snapshot rebuild failed", "site", a.slug, "err", err,
				"retry_in", backoff)
			retryTimer = time.NewTimer(backoff)
			backoff = min(backoff*2, r.retryMax)
			continue
		}
		backoff = r.retryInitial
	}
}

// DirtyGen returns the trigger count for a slug (telemetry, tests).
func (r *Registry) DirtyGen(slug string) int64 {
	r.mu.Lock()
	a, ok := r.actors[slug]
	r.mu.Unlock()
	if !ok {
		return 0
	}
	return a.dirtyGen.Load()
}

// debounceWait blocks for the sliding debounce window: each further
// trigger restarts the timer, so a webhook burst coalesces until
// r.debounce of quiet. Returns false on shutdown.
func (r *Registry) debounceWait(a *actor) bool {
	t := time.NewTimer(r.debounce)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return false
		case <-a.trigger:
			if !t.Stop() {
				<-t.C
			}
			t.Reset(r.debounce)
		case <-t.C:
			return true
		}
	}
}
