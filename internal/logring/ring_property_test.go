package logring

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// Model-based property test for Ring. The reference model is a plain
// slice of (envelope, size) with linear-scan eviction: Push evicts from
// the front until the new envelope fits (or the model is empty), Drain
// first expires stale entries at the front, then takes entries from the
// front while both the count and the byte bound allow, dropping (and
// counting) any entry too large to ever fit the byte bound on its own.
//
// Envelope size is the marshalled JSON length, which is the Ring's
// documented budget unit. Sizes are made to vary by Msg length so the
// byte budget is crossed at different counts; ages are either fresh or
// two hours old so the maxAge=1h boundary is never within scheduling
// jitter of the wall clock.

type ringModelEntry struct {
	seq  int
	size int64
	old  bool
}

type ringModel struct {
	maxBytes int64
	ageOn    bool
	items    []ringModelEntry
	bytes    int64
	dropped  int64
	closed   bool
}

func (m *ringModel) push(e ringModelEntry) {
	if m.closed {
		m.dropped++
		return
	}
	for m.bytes+e.size > m.maxBytes && len(m.items) > 0 {
		m.bytes -= m.items[0].size
		m.items = m.items[1:]
		m.dropped++
	}
	m.items = append(m.items, e)
	m.bytes += e.size
}

func (m *ringModel) drain(maxCount int, maxBytes int64) []ringModelEntry {
	if maxCount <= 0 || maxBytes <= 0 {
		return nil
	}
	if m.ageOn {
		for len(m.items) > 0 && m.items[0].old {
			m.bytes -= m.items[0].size
			m.items = m.items[1:]
			m.dropped++
		}
	}
	var out []ringModelEntry
	var total int64
	for len(m.items) > 0 && len(out) < maxCount {
		e := m.items[0]
		if e.size <= maxBytes && total+e.size > maxBytes {
			break
		}
		m.items = m.items[1:]
		m.bytes -= e.size
		if e.size > maxBytes {
			m.dropped++
			continue
		}
		total += e.size
		out = append(out, e)
	}
	return out
}

// unbounded is a Drain byte bound no case's ring can reach.
const unbounded = int64(1) << 40

// checkDrain compares one Drain result with the model's and checks the
// byte bound on what Drain actually returned.
func checkDrain(t *rapid.T, label string, maxBytes int64, got []slogkit.Envelope, want []ringModelEntry) {
	if len(got) != len(want) {
		t.Fatalf("%s: got %d envelopes, model %d", label, len(got), len(want))
	}
	var total int64
	for j := range got {
		if s := seqOf(t, got[j]); s != want[j].seq {
			t.Fatalf("%s[%d]: got seq %d, model %d", label, j, s, want[j].seq)
		}
		if maxBytes < unbounded {
			total += sizeOf(got[j])
		}
	}
	if len(got) > 0 && total > maxBytes {
		t.Fatalf("%s: returned %d bytes, over the %d-byte bound", label, total, maxBytes)
	}
}

// envGen builds the envelopes one case pushes. Every envelope in a case
// shares one fresh and one stale timestamp and a fixed-width seq, so its
// marshalled size depends only on (msgLen, old) and is computed once.
type envGen struct {
	now   time.Time
	sizes map[envKey]int64
}

type envKey struct {
	msgLen int
	old    bool
}

func newEnvGen() *envGen { return &envGen{now: time.Now(), sizes: map[envKey]int64{}} }

func (g *envGen) env(seq, msgLen int, old bool) (slogkit.Envelope, int64) {
	ts := g.now
	if old {
		ts = ts.Add(-2 * time.Hour)
	}
	env := slogkit.Envelope{
		TS:     ts,
		Level:  slogkit.LevelInfo,
		Source: slogkit.SourceSite,
		Msg:    fmt.Sprintf("%07d:%s", seq, strings.Repeat("x", msgLen)),
	}
	k := envKey{msgLen, old}
	size, ok := g.sizes[k]
	if !ok {
		size = sizeOf(env)
		g.sizes[k] = size
	}
	return env, size
}

func seqOf(t *rapid.T, env slogkit.Envelope) int {
	digits, _, _ := strings.Cut(env.Msg, ":")
	n, err := strconv.Atoi(digits)
	if err != nil {
		t.Fatalf("unparseable seq in %q", env.Msg)
	}
	return n
}

func sizeOf(env slogkit.Envelope) int64 {
	buf, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return int64(len(buf))
}

func TestRing_Property_MatchesSliceModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		// Budgets: tiny (a couple of envelopes) through roomy. A typical
		// envelope here is ~80-400 bytes.
		maxBytes := rapid.SampledFrom([]int64{100, 300, 1000, 4000, 1 << 20}).Draw(t, "maxBytes")
		ageOn := rapid.Bool().Draw(t, "ageOn")
		var maxAge time.Duration
		if ageOn {
			maxAge = time.Hour
		}
		r := New(maxBytes, maxAge)
		m := &ringModel{maxBytes: maxBytes, ageOn: ageOn}
		g := newEnvGen()
		seq := 0

		// Swarm: per-case op weights so some histories are push-heavy
		// (forcing growth past the initial 1024 slots and wraparound) and
		// some drain-heavy.
		pushW := rapid.IntRange(1, 10).Draw(t, "pushW")
		drainW := rapid.IntRange(0, 5).Draw(t, "drainW")
		closeW := rapid.IntRange(0, 1).Draw(t, "closeW")
		nOps := rapid.IntRange(1, 100).Draw(t, "nOps")
		burst := rapid.IntRange(1, 1500).Draw(t, "burst")

		for i := 0; i < nOps; i++ {
			k := rapid.IntRange(0, pushW+drainW+closeW).Draw(t, "op")
			switch {
			case k < pushW:
				// Push a burst (sometimes large) to cross 1024-slot grow
				// boundaries with a non-zero head.
				n := 1
				msgLen := rapid.IntRange(0, 300).Draw(t, "msgLen")
				switch rapid.IntRange(0, 19).Draw(t, "isBurst") {
				case 0:
					n = rapid.IntRange(1, burst).Draw(t, "burstN")
				case 1:
					// Targeted: land exactly on and around the slice
					// growth thresholds (1024, 2048 slots) with small
					// envelopes so nothing is evicted and grow() runs
					// with whatever head offset earlier drains left.
					n = rapid.SampledFrom([]int{1023, 1024, 1025, 2047, 2048, 2049}).Draw(t, "growN")
					msgLen = 0
				}
				old := ageOn && rapid.IntRange(0, 4).Draw(t, "old") == 0
				for j := 0; j < n; j++ {
					env, size := g.env(seq, msgLen, old)
					m.push(ringModelEntry{seq: seq, size: size, old: old})
					r.Push(env)
					seq++
				}
			case k < pushW+drainW:
				maxCount := rapid.IntRange(-1, 2000).Draw(t, "maxCount")
				// Byte bounds from below one envelope (every head entry is
				// oversized and dropped) through a few envelopes to
				// effectively unbounded.
				drainBytes := rapid.SampledFrom([]int64{-1, 0, 60, 200, 450, 2000, unbounded}).Draw(t, "drainBytes")
				want := m.drain(maxCount, drainBytes)
				got := r.Drain(maxCount, drainBytes)
				checkDrain(t, fmt.Sprintf("Drain(%d, %d)", maxCount, drainBytes), drainBytes, got, want)
			default:
				m.closed = true
				r.Close()
			}

			st := r.Stats()
			if st.Count != len(m.items) || st.Bytes != m.bytes || st.Dropped != m.dropped {
				t.Fatalf("after op %d: stats %+v, model count=%d bytes=%d dropped=%d",
					i, st, len(m.items), m.bytes, m.dropped)
			}
			if st.Bytes > maxBytes && st.Count > 1 {
				t.Fatalf("over budget with %d entries: %d > %d", st.Count, st.Bytes, maxBytes)
			}
		}
		// Final full drain: everything left in the model comes out in order.
		want := m.drain(1<<30, unbounded)
		got := r.Drain(1<<30, unbounded)
		checkDrain(t, "final drain", unbounded, got, want)
	})
}

// TestRing_Property_GrowAfterWrap is the targeted pattern for the circular
// slice: push a, drain b (moving head off zero), then push c so the
// slice fills with a wrapped head and grow() has to unroll it, possibly
// more than once. Order and count must survive every growth.
func TestRing_Property_GrowAfterWrap(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := New(1<<30, 0)
		m := &ringModel{maxBytes: 1 << 30}
		g := newEnvGen()
		seq := 0
		rounds := rapid.IntRange(1, 3).Draw(t, "rounds")
		for k := 0; k < rounds; k++ {
			// Up to 2500 per round: enough to fill past the 1024- and
			// 2048-slot growth points with a wrapped head.
			a := rapid.IntRange(0, 2500).Draw(t, "a")
			for i := 0; i < a; i++ {
				env, size := g.env(seq, 0, false)
				m.push(ringModelEntry{seq: seq, size: size})
				r.Push(env)
				seq++
			}
			b := rapid.IntRange(0, 2500).Draw(t, "b")
			want := m.drain(b, unbounded)
			got := r.Drain(b, unbounded)
			checkDrain(t, fmt.Sprintf("round %d Drain(%d)", k, b), unbounded, got, want)
		}
		want := m.drain(1<<30, unbounded)
		got := r.Drain(1<<30, unbounded)
		checkDrain(t, "final drain", unbounded, got, want)
	})
}

// TestRing_Property_ConcurrentProducersConsumers: several producers push
// tagged envelopes while consumers Wait+Drain. With a budget large
// enough that nothing is evicted, every envelope is delivered exactly
// once and each producer's envelopes arrive in its push order. Close
// unblocks every waiter.
func TestRing_Property_ConcurrentProducersConsumers(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		producers := rapid.IntRange(1, 4).Draw(t, "producers")
		consumers := rapid.IntRange(1, 4).Draw(t, "consumers")
		perProducer := rapid.IntRange(0, 500).Draw(t, "perProducer")
		drainMax := rapid.IntRange(1, 64).Draw(t, "drainMax")

		r := New(1<<30, 0)
		var wg sync.WaitGroup
		for p := 0; p < producers; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				for i := 0; i < perProducer; i++ {
					r.Push(slogkit.Envelope{TS: time.Now(), Msg: fmt.Sprintf("%d:%d", p, i)})
				}
			}(p)
		}

		var mu sync.Mutex
		got := map[int][]int{}
		var violation string // first ordering or parse failure seen by a consumer
		var cwg sync.WaitGroup
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for c := 0; c < consumers; c++ {
			cwg.Add(1)
			go func() {
				defer cwg.Done()
				for {
					ok := r.Wait(ctx)
					batch := r.Drain(drainMax, unbounded)
					// Per-consumer batches preserve ring order, but two
					// consumers' batches interleave arbitrarily, so we
					// only check per-producer order within one batch and
					// the global multiset below.
					last := map[int]int{}
					mu.Lock()
					for _, e := range batch {
						var p, i int
						if _, err := fmt.Sscanf(e.Msg, "%d:%d", &p, &i); err != nil || p < 0 || p >= producers || i < 0 || i >= perProducer {
							if violation == "" {
								violation = fmt.Sprintf("drained an envelope no producer pushed: %q", e.Msg)
							}
							continue
						}
						if prev, seen := last[p]; seen && i <= prev && violation == "" {
							violation = fmt.Sprintf("producer %d reordered within one batch: %d after %d", p, i, prev)
						}
						last[p] = i
						got[p] = append(got[p], i)
					}
					mu.Unlock()
					if !ok && len(batch) == 0 {
						return
					}
				}
			}()
		}
		wg.Wait()
		// Liveness before Close: every envelope must be delivered by
		// the Push-side Signal alone. Close's Broadcast would otherwise
		// mask a lost wakeup.
		total := producers * perProducer
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			n := 0
			for _, v := range got {
				n += len(v)
			}
			mu.Unlock()
			if n == total {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("only %d/%d envelopes delivered before Close (lost wakeup)", n, total)
			}
			time.Sleep(time.Millisecond)
		}
		r.Close()
		done := make(chan struct{})
		go func() { cwg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("consumers did not exit after Close")
		}
		if violation != "" {
			t.Fatalf("%s", violation)
		}
		for p := 0; p < producers; p++ {
			seen := make([]int, perProducer)
			for _, i := range got[p] {
				seen[i]++
			}
			for i, n := range seen {
				if n != 1 {
					t.Fatalf("producer %d envelope %d delivered %d times", p, i, n)
				}
			}
		}
		if st := r.Stats(); st.Dropped != 0 || st.Count != 0 {
			t.Fatalf("stats after full drain: %+v", st)
		}
	})
}
