package logring

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// anyBytes is a Drain byte budget no test ring reaches.
const anyBytes int64 = 1 << 40

func env(msg string, ts time.Time) slogkit.Envelope {
	return slogkit.Envelope{
		TS:     ts,
		Level:  slogkit.LevelInfo,
		Source: slogkit.SourceSite,
		Msg:    msg,
	}
}

func TestPushDrainFIFO(t *testing.T) {
	r := New(1<<20, 0)
	now := time.Now()
	for i := range 5 {
		r.Push(env("msg", now.Add(time.Duration(i)*time.Millisecond)))
	}
	got := r.Drain(10, anyBytes)
	if len(got) != 5 {
		t.Fatalf("want 5 drained, got %d", len(got))
	}
	for i := range 4 {
		if !got[i].TS.Before(got[i+1].TS) {
			t.Errorf("FIFO violated at i=%d", i)
		}
	}
}

func TestDrainPartial(t *testing.T) {
	r := New(1<<20, 0)
	now := time.Now()
	for i := range 10 {
		r.Push(env("msg", now.Add(time.Duration(i)*time.Millisecond)))
	}
	first := r.Drain(3, anyBytes)
	if len(first) != 3 {
		t.Fatalf("want 3, got %d", len(first))
	}
	rest := r.Drain(100, anyBytes)
	if len(rest) != 7 {
		t.Fatalf("want 7 remaining, got %d", len(rest))
	}
}

// Drain never returns more than its byte budget, so a caller shipping
// each drain as one message has a hard message-size bound whatever the
// envelope sizes.
func TestDrainByteBudget(t *testing.T) {
	r := New(1<<20, 0)
	now := time.Now()
	small := env(strings.Repeat("s", 100), now)
	for range 5 {
		r.Push(small)
	}
	size := r.Stats().Bytes / 5

	// The count cap is not the binding limit; the byte budget is.
	if got := r.Drain(100, 2*size); len(got) != 2 {
		t.Fatalf("budget of two envelopes drained %d", len(got))
	}
	if got := r.Drain(100, 2*size+size/2); len(got) != 2 {
		t.Fatalf("budget of two and a half envelopes drained %d", len(got))
	}

	// An envelope larger than the whole budget can never be drained: it
	// is dropped and counted, and the envelopes behind it still drain.
	r.Push(env(strings.Repeat("b", 10*int(size)), now))
	r.Push(small)
	got := r.Drain(100, 2*size)
	if len(got) != 2 || got[0].Msg != small.Msg || got[1].Msg != small.Msg {
		t.Fatalf("drained %d envelopes around the oversized one, want the two small ones", len(got))
	}
	if st := r.Stats(); st.Count != 0 || st.Bytes != 0 || st.Dropped != 1 {
		t.Fatalf("after draining past the oversized envelope: %+v, want empty with one drop", st)
	}

	// A ring holding only an oversized envelope drains nothing, and does
	// not keep returning it.
	r.Push(env(strings.Repeat("b", 10*int(size)), now))
	if got := r.Drain(100, 2*size); got == nil || len(got) != 0 {
		t.Fatalf("drain of a lone oversized envelope returned %v, want an empty non-nil slice", got)
	}
	if st := r.Stats(); st.Count != 0 || st.Dropped != 2 {
		t.Fatalf("lone oversized envelope: %+v, want it dropped", st)
	}

	// A non-positive budget drains nothing and drops nothing.
	r.Push(small)
	if got := r.Drain(100, 0); got != nil {
		t.Fatalf("zero byte budget drained %d envelopes", len(got))
	}
	if st := r.Stats(); st.Count != 1 || st.Dropped != 2 {
		t.Fatalf("zero byte budget changed the ring: %+v, want the envelope still queued", st)
	}
}

func TestDropOldestUnderPressure(t *testing.T) {
	// Tight budget so each Push evicts something.
	r := New(512, 0)
	now := time.Now()
	for i := range 100 {
		r.Push(slogkit.Envelope{
			TS:     now.Add(time.Duration(i) * time.Millisecond),
			Level:  slogkit.LevelInfo,
			Source: slogkit.SourceAgent,
			// Pad msg to push past the budget quickly.
			Msg: strings.Repeat("x", 64),
			// Vary trace_id so envelopes are distinguishable.
			TraceID: uuid.New(),
		})
	}
	got := r.Drain(1000, anyBytes)
	stats := r.Stats()
	if stats.Dropped == 0 {
		t.Errorf("expected drops under pressure, got 0")
	}
	if len(got)+int(stats.Dropped) < 100 {
		t.Errorf("drained=%d + dropped=%d < pushed=100", len(got), stats.Dropped)
	}
	// What remains in the ring must be the newest items, not the oldest.
	for i := 0; i+1 < len(got); i++ {
		if !got[i].TS.Before(got[i+1].TS) {
			t.Errorf("post-eviction FIFO violated at i=%d", i)
		}
	}
}

func TestExpireByAge(t *testing.T) {
	r := New(1<<20, 50*time.Millisecond)
	old := time.Now().Add(-1 * time.Hour)
	r.Push(env("ancient", old))
	r.Push(env("ancient2", old))
	r.Push(env("fresh", time.Now()))
	got := r.Drain(10, anyBytes)
	if len(got) != 1 || got[0].Msg != "fresh" {
		t.Errorf("expected age-eviction to leave only the fresh envelope, got %+v", got)
	}
	if r.Stats().Dropped < 2 {
		t.Errorf("expected at least 2 age-drops, got %d", r.Stats().Dropped)
	}
}

func TestWaitWakesOnPush(t *testing.T) {
	r := New(1<<20, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.Push(env("wakeup", time.Now()))
	}()
	if !r.Wait(ctx) {
		t.Fatalf("Wait timed out before push")
	}
}

func TestWaitReturnsFalseOnCtxDone(t *testing.T) {
	r := New(1<<20, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if r.Wait(ctx) {
		t.Errorf("Wait should return false when ctx times out on empty ring")
	}
}

func TestCloseUnblocksWait(t *testing.T) {
	r := New(1<<20, 0)
	ctx := context.Background()
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.Close()
	}()
	r.Wait(ctx)
	// Should return promptly. After close, push is dropped.
	r.Push(env("post-close", time.Now()))
	if r.Stats().Dropped == 0 {
		t.Errorf("expected post-close push to drop, got dropped=0")
	}
}

func TestGrowsBeyondInitialCapacity(t *testing.T) {
	// Force the slice to grow past 1024 entries. Tight byte budget
	// would defeat this; use a permissive one.
	r := New(1<<30, 0)
	now := time.Now()
	for i := range 4096 {
		r.Push(env("m", now.Add(time.Duration(i)*time.Microsecond)))
	}
	got := r.Drain(1<<20, anyBytes)
	if len(got) != 4096 {
		t.Fatalf("want 4096, got %d (dropped=%d)", len(got), r.Stats().Dropped)
	}
}
