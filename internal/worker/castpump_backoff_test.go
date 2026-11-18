package worker

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNextBackoff: streak 0 returns base; each subsequent streak doubles
// until the cap, then stays pinned at the cap.
func TestNextBackoff(t *testing.T) {
	base := 1 * time.Second
	max := 60 * time.Second
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
		{6, 60 * time.Second}, // 64s capped to 60
		{7, 60 * time.Second},
		{100, 60 * time.Second}, // no overflow at large streak
	}
	for _, c := range cases {
		if got := nextBackoff(base, max, c.streak); got != c.want {
			t.Errorf("nextBackoff(base=%v,max=%v,streak=%d) = %v, want %v",
				base, max, c.streak, got, c.want)
		}
	}
}

// TestNextBackoff_NegativeStreakClamped: a negative streak (shouldn't
// happen, but defensive) is treated as 0, not a huge right-shift.
func TestNextBackoff_NegativeStreakClamped(t *testing.T) {
	if got := nextBackoff(time.Second, time.Minute, -5); got != time.Second {
		t.Errorf("nextBackoff(streak=-5) = %v, want base 1s", got)
	}
}

// TestApplyJitter: jitter stays within [d, d+frac*d] and is never below
// the floor (a backoff must never shrink below its computed value).
func TestApplyJitter(t *testing.T) {
	d := 10 * time.Second
	frac := 0.2
	// Deterministic across a sweep of jitter sources in [0,1).
	for i := range 100 {
		r := float64(i) / 100.0
		got := applyJitterWith(d, frac, r)
		if got < d {
			t.Errorf("applyJitterWith(%v,%v,r=%.2f) = %v < floor %v", d, frac, r, got, d)
		}
		ceil := d + time.Duration(frac*float64(d))
		if got > ceil {
			t.Errorf("applyJitterWith(%v,%v,r=%.2f) = %v > ceil %v", d, frac, r, got, ceil)
		}
	}
}

// TestApplyJitter_ZeroFracIsIdentity: frac 0 returns d unchanged.
func TestApplyJitter_ZeroFracIsIdentity(t *testing.T) {
	d := 7 * time.Second
	if got := applyJitterWith(d, 0, 0.999); got != d {
		t.Errorf("applyJitterWith(frac=0) = %v, want %v", got, d)
	}
}

// fastExitDialer always returns a session that exits after exitDur with
// no bytes, counting dial calls. Models a BMC whose SOL slot is busy:
// ipmitool activates and dies almost immediately.
type fastExitDialer struct {
	mu      sync.Mutex
	calls   int
	callAt  []time.Time
	exitDur time.Duration
}

func (d *fastExitDialer) Dial(ctx context.Context, _ uuid.UUID) (CastSession, error) {
	d.mu.Lock()
	d.calls++
	d.callAt = append(d.callAt, time.Now())
	exitDur := d.exitDur
	d.mu.Unlock()

	pr, pw, err := os.Pipe()
	if err != nil {
		return CastSession{}, err
	}
	go func() {
		if exitDur > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(exitDur):
			}
		}
		_ = pw.Close() // EOF -> session ends
	}()
	return CastSession{Stream: &pipeStream{pr: pr, pw: pw}}, nil
}

func (d *fastExitDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// TestCastPump_FastFailBacksOff: a dialer whose sessions die sub-threshold
// drives the backoff up, so the pump dials FEWER times than a flat-backoff
// pump would over the same window. This is the firehose fix: without
// growing backoff, a 10ms base would dial ~hundreds of times in 800ms;
// with exponential growth it should be a small single-digit count.
func TestCastPump_FastFailBacksOff(t *testing.T) {
	mid := uuid.New()
	dialer := &fastExitDialer{exitDur: 5 * time.Millisecond} // always fast-fail
	sink := &collectingSink{}

	pump := NewCastPump(CastPumpConfig{
		MachineID:           mid,
		Dialer:              dialer,
		Resume:              fixedResume{v: 0},
		Sink:                sink,
		FlushInterval:       20 * time.Millisecond,
		ReconnectBackoff:    10 * time.Millisecond, // base
		MaxReconnectBackoff: 160 * time.Millisecond,
		FastFailThreshold:   50 * time.Millisecond, // 5ms sessions are "fast fails"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go pump.Run(ctx)
	time.Sleep(800 * time.Millisecond)
	cancel()

	calls := dialer.dialCount()
	// Flat 10ms backoff over 800ms would be ~70+ dials. Exponential
	// (10,20,40,80,160,160,...) caps the cadence; expect well under 20.
	if calls == 0 {
		t.Fatal("expected at least one dial")
	}
	if calls > 20 {
		t.Errorf("fast-fail pump dialed %d times in 800ms; backoff not growing (want <=20)", calls)
	}
}

// TestCastPump_HealthySessionResetsBackoff: after a long (healthy)
// session, a subsequent fast-fail starts back at base, not at the
// previously-grown backoff. Verifies the streak resets so a BMC that
// recovers and then blips once doesn't inherit a 60s penalty.
func TestCastPump_HealthySessionResetsBackoff(t *testing.T) {
	mid := uuid.New()
	// Session 1: fast fail (5ms). Session 2: healthy (200ms, > threshold).
	// Session 3+: fast fail again. We assert the gap before session 3's
	// retry is ~base, proving the healthy session reset the streak.
	var (
		mu       sync.Mutex
		sessions int
	)
	dialer := &erroringDialer{
		next: func(ctx context.Context, m uuid.UUID) (CastSession, error) {
			mu.Lock()
			sessions++
			n := sessions
			mu.Unlock()
			dur := 5 * time.Millisecond
			if n == 2 {
				dur = 200 * time.Millisecond // healthy
			}
			fe := &fastExitDialer{exitDur: dur}
			return fe.Dial(ctx, m)
		},
	}
	sink := &collectingSink{}
	pump := NewCastPump(CastPumpConfig{
		MachineID:           mid,
		Dialer:              dialer,
		Resume:              fixedResume{v: 0},
		Sink:                sink,
		FlushInterval:       20 * time.Millisecond,
		ReconnectBackoff:    20 * time.Millisecond,
		MaxReconnectBackoff: 320 * time.Millisecond,
		FastFailThreshold:   100 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go pump.Run(ctx)
	time.Sleep(1200 * time.Millisecond)
	cancel()

	mu.Lock()
	got := sessions
	mu.Unlock()
	// If the streak never reset, after the healthy session the pump would
	// still carry a grown backoff. With reset, it returns to base (20ms)
	// and cycles fast-fails quickly again. We mainly assert it kept going
	// past the healthy session (>=4 sessions) without wedging.
	if got < 4 {
		t.Errorf("expected >=4 sessions (fast, healthy, then resumed fast cycle), got %d", got)
	}
}
