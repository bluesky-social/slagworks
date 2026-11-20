package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// State-machine property test for CastManager over a small pool of
// machines. A tracking dialer counts open console sessions per machine
// (Dial opens, stream Close closes) and records any moment where a
// machine has two open sessions: the BMC has one SOL slot, and two
// pumps on one console is the failure the drop-then-wait logic exists
// to prevent.
//
// Ops: set the active set (any subset of the pool) and Reconcile;
// inject per-session faults (dial error, fast exit, long-lived);
// Shutdown at the end. Invariants checked after every Reconcile:
//   - the pump map equals the active set;
//   - no machine outside the active set has an open session;
//   - no machine ever had two sessions open at once;
// and after Shutdown: zero open sessions and no leaked goroutines.

type trackStream struct {
	d    *trackDialer
	id   uuid.UUID
	once sync.Once
	done chan struct{}
}

func (s *trackStream) Read(p []byte) (int, error) {
	<-s.done
	return 0, io.EOF
}
func (s *trackStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *trackStream) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.d.mu.Lock()
		s.d.open[s.id]--
		s.d.mu.Unlock()
	})
	return nil
}

type trackDialer struct {
	mu         sync.Mutex
	open       map[uuid.UUID]int
	dials      map[uuid.UUID]int
	violations []string
	// fault mode per machine: 0 long-lived, 1 dial error, 2 exit after 1ms
	mode map[uuid.UUID]int
}

func newTrackDialer() *trackDialer {
	return &trackDialer{open: map[uuid.UUID]int{}, dials: map[uuid.UUID]int{}, mode: map[uuid.UUID]int{}}
}

func (d *trackDialer) Dial(ctx context.Context, id uuid.UUID) (CastSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials[id]++
	switch d.mode[id] {
	case 1:
		return CastSession{}, errors.New("injected dial failure")
	}
	d.open[id]++
	if d.open[id] > 1 {
		d.violations = append(d.violations, fmt.Sprintf("machine %s has %d open sessions", id, d.open[id]))
	}
	s := &trackStream{d: d, id: id, done: make(chan struct{})}
	if d.mode[id] == 2 {
		go func() { time.Sleep(time.Millisecond); s.Close() }()
	}
	return CastSession{Stream: s}, nil
}

func TestCastManager_Property_OnePumpPerActiveMachine(t *testing.T) {
	baseline := runtime.NumGoroutine()
	rapid.Check(t, func(t *rapid.T) {
		pool := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
		lister := &fakeCastLister{}
		dialer := newTrackDialer()
		m := NewCastManager(CastManagerConfig{
			Dialer: dialer,
			Sink:   &collectingSink{},
			Resume: fixedResume{v: 0},
			Cache:  lister,
		})
		m.SetBackoff(time.Millisecond)
		m.maxReconnectBackoff = 2 * time.Millisecond
		m.Start(context.Background())
		// A failed assertion skips the timed Shutdown below; this one
		// keeps a failing case's pumps from running into the next case
		// while rapid shrinks. Shutdown is idempotent.
		defer m.Shutdown()

		active := map[uuid.UUID]bool{}
		n := rapid.IntRange(1, 25).Draw(t, "nOps")
		for i := 0; i < n; i++ {
			switch rapid.IntRange(0, 3).Draw(t, "op") {
			case 0, 1:
				var ids []uuid.UUID
				active = map[uuid.UUID]bool{}
				for j, id := range pool {
					if rapid.Bool().Draw(t, fmt.Sprintf("in%d", j)) {
						ids = append(ids, id)
						active[id] = true
					}
				}
				lister.setActive(ids...)
				m.Reconcile()
			case 2:
				id := pool[rapid.IntRange(0, len(pool)-1).Draw(t, "faultID")]
				mode := rapid.IntRange(0, 2).Draw(t, "mode")
				dialer.mu.Lock()
				dialer.mode[id] = mode
				dialer.mu.Unlock()
			case 3:
				// Let pumps cycle through backoff/redial a bit.
				time.Sleep(time.Duration(rapid.IntRange(0, 5).Draw(t, "sleepMs")) * time.Millisecond)
			}

			m.mu.Lock()
			for id := range m.pumps {
				if !active[id] {
					m.mu.Unlock()
					t.Fatalf("op %d: pump for inactive machine %s", i, id)
				}
			}
			npumps := len(m.pumps)
			m.mu.Unlock()
			if npumps != len(active) && i > 0 {
				// Only meaningful right after a Reconcile; a fault/sleep
				// op leaves the last Reconcile's set in place.
				t.Fatalf("op %d: %d pumps for %d active machines", i, npumps, len(active))
			}
			dialer.mu.Lock()
			for _, id := range pool {
				if !active[id] && dialer.open[id] != 0 {
					dialer.mu.Unlock()
					t.Fatalf("op %d: inactive machine %s still has %d open sessions after Reconcile", i, id, dialer.open[id])
				}
			}
			if len(dialer.violations) > 0 {
				v := dialer.violations[0]
				dialer.mu.Unlock()
				t.Fatalf("op %d: %s", i, v)
			}
			dialer.mu.Unlock()
		}

		start := time.Now()
		m.Shutdown()
		if d := time.Since(start); d > time.Second {
			t.Fatalf("Shutdown took %v", d)
		}
		dialer.mu.Lock()
		for id, c := range dialer.open {
			if c != 0 {
				dialer.mu.Unlock()
				t.Fatalf("after Shutdown machine %s has %d open sessions", id, c)
			}
		}
		dialer.mu.Unlock()
		// Reconcile after Shutdown must be a no-op.
		lister.setActive(pool...)
		m.Reconcile()
		m.mu.Lock()
		if len(m.pumps) != 0 {
			m.mu.Unlock()
			t.Fatalf("Reconcile after Shutdown spawned %d pumps", len(m.pumps))
		}
		m.mu.Unlock()
	})
	// Goroutine leak check across all cases (pump goroutines, tickers,
	// AfterFunc closers, per-session watchdogs).
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseline+2 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutine leak: %d now vs %d baseline\n%s", g, baseline, buf[:n])
	}
}
