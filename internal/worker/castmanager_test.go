package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// countingCastDialer wraps fakeCastDialer and counts Dial calls, so
// watchdog/reconnect tests can assert how many sessions were started.
type countingCastDialer struct {
	starts atomic.Int64
	inner  *fakeCastDialer
}

func (d *countingCastDialer) Dial(ctx context.Context, id uuid.UUID) (CastSession, error) {
	d.starts.Add(1)
	return d.inner.Dial(ctx, id)
}

// fakeCastLister is a settable CastMachineLister for manager tests.
type fakeCastLister struct {
	mu     sync.Mutex
	active []uuid.UUID
	state  string
}

func (f *fakeCastLister) setActive(ids ...uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = append([]uuid.UUID(nil), ids...)
}

func (f *fakeCastLister) ListActiveMachineIDs() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.active...)
}

func (f *fakeCastLister) MachineState(uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == "" {
		return string(model.MachineStateRunning)
	}
	return f.state
}

func newManager(t *testing.T, lister *fakeCastLister, dialer CastDialer) *CastManager {
	t.Helper()
	m := NewCastManager(CastManagerConfig{
		Dialer: dialer,
		Sink:   &collectingSink{},
		Resume: fixedResume{v: 0},
		Cache:  lister,
	})
	m.SetBackoff(5 * time.Millisecond)
	return m
}

func TestCastManager_SpawnsPumpPerActiveMachine(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	lister := &fakeCastLister{}
	lister.setActive(id1, id2)
	dialer := &fakeCastDialer{exitDur: time.Hour} // stay connected

	m := newManager(t, lister, dialer)
	m.Start(context.Background())
	defer m.Shutdown()
	m.Reconcile()

	// Two pumps should be tracked.
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.pumps) == 2
	})
}

func TestCastManager_TearsDownDroppedMachines(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	lister := &fakeCastLister{}
	lister.setActive(id1, id2)
	dialer := &fakeCastDialer{exitDur: time.Hour}

	m := newManager(t, lister, dialer)
	m.Start(context.Background())
	defer m.Shutdown()
	m.Reconcile()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.pumps) == 2
	})

	// Drop id2 from the active set and reconcile: pump set shrinks to 1.
	lister.setActive(id1)
	m.Reconcile()
	m.mu.Lock()
	n := len(m.pumps)
	_, has1 := m.pumps[id1]
	_, has2 := m.pumps[id2]
	m.mu.Unlock()
	if n != 1 || !has1 || has2 {
		t.Fatalf("after drop: pumps=%d has1=%v has2=%v, want 1/true/false", n, has1, has2)
	}
}

func TestCastManager_ShutdownStopsAllPumps(t *testing.T) {
	id1, id2 := uuid.New(), uuid.New()
	lister := &fakeCastLister{}
	lister.setActive(id1, id2)
	dialer := &fakeCastDialer{exitDur: time.Hour}

	m := newManager(t, lister, dialer)
	m.Start(context.Background())
	m.Reconcile()
	waitFor(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.pumps) == 2
	})

	m.Shutdown()
	m.mu.Lock()
	n := len(m.pumps)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("after shutdown: pumps=%d, want 0", n)
	}
}

func TestCastManager_ReconcileNoOpBeforeStart(t *testing.T) {
	lister := &fakeCastLister{}
	lister.setActive(uuid.New())
	m := newManager(t, lister, &fakeCastDialer{exitDur: time.Hour})
	// No Start() called — Reconcile must be a safe no-op.
	m.Reconcile()
	m.mu.Lock()
	n := len(m.pumps)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("Reconcile before Start spawned %d pumps, want 0", n)
	}
}

// TestCastPump_SilenceWatchdogReconnects verifies the ported watchdog:
// a session that goes silent past the threshold (while state says
// output should flow) gets killed and re-dialed. We drive it via a
// dialer whose session produces one byte then goes quiet, with a tiny
// threshold, and assert the dialer is called more than once.
func TestCastPump_SilenceWatchdogReconnects(t *testing.T) {
	dialer := &countingCastDialer{inner: &fakeCastDialer{
		scripts: nil,              // no bytes => immediately "silent"
		exitDur: 10 * time.Second, // session would otherwise stay up
	}}
	sink := &collectingSink{}
	pump := NewCastPump(CastPumpConfig{
		MachineID:         uuid.New(),
		Dialer:            dialer,
		Resume:            fixedResume{v: 0},
		Sink:              sink,
		ReconnectBackoff:  5 * time.Millisecond,
		SilenceThreshold:  40 * time.Millisecond,
		SilenceCheckEvery: 10 * time.Millisecond,
		StateFunc:         func() string { return string(model.MachineStateRunning) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pump.Run(ctx) }()

	// With a 40ms silence threshold and a quiet session, the watchdog
	// should kill+re-dial repeatedly. Expect >=3 dial calls within ~1s.
	waitFor(t, func() bool { return dialer.starts.Load() >= 3 })
	cancel()
	<-done
}

// TestCastPump_SilenceWatchdogSuppressedWhenStateQuiet verifies the
// state gate: a machine whose state says "don't expect output"
// (failed) does NOT get watchdog-reconnected on silence.
func TestCastPump_SilenceWatchdogSuppressedWhenStateQuiet(t *testing.T) {
	dialer := &countingCastDialer{inner: &fakeCastDialer{exitDur: 10 * time.Second}}
	sink := &collectingSink{}
	pump := NewCastPump(CastPumpConfig{
		MachineID:         uuid.New(),
		Dialer:            dialer,
		Resume:            fixedResume{v: 0},
		Sink:              sink,
		ReconnectBackoff:  5 * time.Millisecond,
		SilenceThreshold:  40 * time.Millisecond,
		SilenceCheckEvery: 10 * time.Millisecond,
		StateFunc:         func() string { return string(model.MachineStateFailed) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pump.Run(ctx) }()

	// Give the watchdog several check intervals; it must NOT force a
	// reconnect, so the single long-lived session stays at 1 dial.
	time.Sleep(200 * time.Millisecond)
	got := dialer.starts.Load()
	cancel()
	<-done
	if got != 1 {
		t.Fatalf("watchdog reconnected a quiet-state machine: %d dials, want 1", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 1s")
}

// TestCastManager_MassDropSharedDeadline pins the B8 fix: dropping N
// wedged pumps waits ONE shared 2s grace, not 2s per pump. Reconcile
// runs on the snapshot-apply path, so a serial wait would stall apply
// for minutes on a mass drop (site-config change, machine sweep).
func TestCastManager_MassDropSharedDeadline(t *testing.T) {
	lister := &fakeCastLister{}
	m := newManager(t, lister, &fakeCastDialer{})
	ctx := t.Context()
	m.Start(ctx)

	// Inject 5 pumps whose done channels never close (wedged ipmitool).
	m.mu.Lock()
	for range 5 {
		m.pumps[uuid.New()] = &castPumpHandle{
			cancel: func() {},
			done:   make(chan struct{}), // never closed
		}
	}
	m.mu.Unlock()

	lister.setActive() // want = empty -> all 5 dropped
	start := time.Now()
	m.Reconcile()
	elapsed := time.Since(start)

	// Shared deadline: ~2s. The old serial wait would take ~10s.
	if elapsed > 4*time.Second {
		t.Fatalf("mass drop took %v; shared 2s deadline violated (serial per-pump wait?)", elapsed)
	}
}

// TestCastManager_ShutdownSharedDeadline: same property for Shutdown's
// 5s grace — total, not per pump.
func TestCastManager_ShutdownSharedDeadline(t *testing.T) {
	lister := &fakeCastLister{}
	m := newManager(t, lister, &fakeCastDialer{})
	ctx := t.Context()
	m.Start(ctx)

	m.mu.Lock()
	for range 4 {
		m.pumps[uuid.New()] = &castPumpHandle{
			cancel: func() {},
			done:   make(chan struct{}),
		}
	}
	m.mu.Unlock()

	start := time.Now()
	m.Shutdown()
	elapsed := time.Since(start)

	// Shared deadline: ~5s. The old serial wait would take ~20s.
	if elapsed > 8*time.Second {
		t.Fatalf("shutdown took %v; shared 5s deadline violated", elapsed)
	}
}
