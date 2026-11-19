// Package logring implements the in-memory log envelope ring used by
// the site worker and the global daemon to buffer envelopes between
// the producer side (slog handler, SOL pump, /logs/ingest endpoint)
// and the forwarding side (WS to global on the site; pg writer on
// the global).
//
// Drop-oldest under pressure. No disk-backing. If the consumer is
// behind by ring capacity, the oldest envelopes are evicted; the
// drop counter is the operator-facing visibility on this happening.
package logring

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// Ring is a bounded byte-budgeted FIFO of envelopes. The bound is on
// total marshalled byte size, not entry count: a chatty SOL pump can
// produce small envelopes faster than a deploy with rich extras, but
// the operator cares about MiB of unforwarded log data, not entry
// count.
type Ring struct {
	maxBytes int64
	maxAge   time.Duration

	mu       sync.Mutex
	notEmpty *sync.Cond

	// items is a circular slice of pending envelopes. head is the
	// oldest (next to drain), tail is the next free slot. Empty when
	// head == tail and count == 0.
	items []entry
	head  int
	tail  int
	count int

	bytes int64

	dropped atomic.Int64

	closed bool
}

type entry struct {
	env  slogkit.Envelope
	size int64 // marshalled JSON size, used for the byte budget
}

// New returns a Ring with the given byte budget and per-envelope max
// age. Envelopes older than maxAge are evicted at the head when
// observed (lazy expiry; the ring is small, no need for a sweeper
// goroutine). maxAge=0 disables age-based eviction.
//
// On the site worker we use 100 MiB / 1h. On global the ring exists
// for stdout-funnel + pg writer — same shape, smaller budget is fine
// (16 MiB is plenty given pg latency is single-digit ms in-cluster).
func New(maxBytes int64, maxAge time.Duration) *Ring {
	r := &Ring{
		maxBytes: maxBytes,
		maxAge:   maxAge,
		items:    make([]entry, 0, 1024),
	}
	r.notEmpty = sync.NewCond(&r.mu)
	return r
}

// Push adds an envelope to the ring. Drops oldest if over budget.
// Push never blocks. Implements slogkit.Sink.
func (r *Ring) Push(env slogkit.Envelope) {
	buf, err := json.Marshal(env)
	if err != nil {
		// Marshalling slogkit.Envelope cannot fail — its MarshalJSON
		// is total. A panic here would mask a real bug in slogkit;
		// dropping silently would mask it longer. Increment the drop
		// counter and move on.
		r.dropped.Add(1)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		r.dropped.Add(1)
		return
	}

	// Evict from the head until we have room.
	size := int64(len(buf))
	for r.bytes+size > r.maxBytes && r.count > 0 {
		r.bytes -= r.items[r.head].size
		r.items[r.head] = entry{}
		r.head = (r.head + 1) % len(r.items)
		r.count--
		r.dropped.Add(1)
	}

	// Grow if needed. We only grow when the slice is full; the slice
	// doubles, capped at the byte budget anyway. A single envelope
	// larger than maxBytes is impossible in practice (envelopes are
	// bounded by SOL flush size + agent batch + slog record limits),
	// so we don't special-case it: it'd evict everything and itself.
	if r.count == len(r.items) {
		r.grow()
	}
	r.items[r.tail] = entry{env: env, size: size}
	r.tail = (r.tail + 1) % len(r.items)
	r.count++
	r.bytes += size
	r.notEmpty.Signal()
}

func (r *Ring) grow() {
	n := len(r.items) * 2
	if n == 0 {
		n = 1024
	}
	next := make([]entry, n)
	for i := 0; i < r.count; i++ {
		next[i] = r.items[(r.head+i)%len(r.items)]
	}
	r.items = next
	r.head = 0
	r.tail = r.count
}

// Drain removes and returns envelopes from the head: at most maxCount
// of them, totalling at most maxBytes of marshalled JSON, so a caller
// that ships each drain as one message gets a hard size bound. An
// envelope larger than maxBytes on its own can never be returned, so
// Drain drops it and counts it in Dropped rather than let it stall the
// ring. Returns zero-length slice (not nil) if nothing is drained, and
// nil without touching the ring if either bound is not positive.
func (r *Ring) Drain(maxCount int, maxBytes int64) []slogkit.Envelope {
	if maxCount <= 0 || maxBytes <= 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireHeadLocked()
	if r.count == 0 {
		return []slogkit.Envelope{}
	}
	out := make([]slogkit.Envelope, 0, min(maxCount, r.count))
	var total int64
	for r.count > 0 && len(out) < maxCount {
		e := r.items[r.head]
		oversized := e.size > maxBytes
		if !oversized && total+e.size > maxBytes {
			break
		}
		r.bytes -= e.size
		r.items[r.head] = entry{}
		r.head = (r.head + 1) % len(r.items)
		r.count--
		if oversized {
			r.dropped.Add(1)
			continue
		}
		total += e.size
		out = append(out, e.env)
	}
	return out
}

// expireHeadLocked drops envelopes older than maxAge from the head.
// Caller must hold r.mu.
func (r *Ring) expireHeadLocked() {
	if r.maxAge <= 0 {
		return
	}
	cutoff := time.Now().Add(-r.maxAge)
	for r.count > 0 {
		e := r.items[r.head]
		if !e.env.TS.Before(cutoff) {
			return
		}
		r.bytes -= e.size
		r.items[r.head] = entry{}
		r.head = (r.head + 1) % len(r.items)
		r.count--
		r.dropped.Add(1)
	}
}

// Wait blocks until the ring has at least one envelope or ctx is done.
// Returns false on ctx done. Drains in conjunction with Drain.
func (r *Ring) Wait(ctx context.Context) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.count == 0 && !r.closed {
		// Park a goroutine to broadcast when ctx cancels so Wait can
		// unblock. Without this the cond would wait forever after
		// shutdown.
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				r.mu.Lock()
				r.notEmpty.Broadcast()
				r.mu.Unlock()
			case <-stop:
			}
		}()
		r.notEmpty.Wait()
		close(stop)
		if ctx.Err() != nil {
			return false
		}
	}
	return r.count > 0
}

// Close marks the ring closed. Wait returns false from now on.
// Pending envelopes can still be drained.
func (r *Ring) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.notEmpty.Broadcast()
}

// Stats returns current ring metrics for Prometheus exposure and
// SIGTERM-drain progress reports.
type Stats struct {
	Count    int
	Bytes    int64
	Dropped  int64
	Capacity int64
}

func (r *Ring) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{
		Count:    r.count,
		Bytes:    r.bytes,
		Dropped:  r.dropped.Load(),
		Capacity: r.maxBytes,
	}
}
