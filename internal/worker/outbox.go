package worker

import (
	"log/slog"
	"sync"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// outbox is the site worker's durable send queue for observations and
// operation updates. It owns crash-recovery persistence (via LocalDB),
// the in-memory queue, the per-row rejection backoff, and the wake
// signal the WS drain loop parks on.
//
// Split out of Cache (fable-review Tier 1 #4): this is one of the five
// responsibilities Cache used to mix. Everything here is guarded by mu;
// nothing here touches snapshot-derived read state or op-phase state, so
// it needs no coordination with the cache's other locks.
//
// Durability contract (unchanged from the pre-split Cache):
//   - appendObservation persists the observation to SQLite under the same
//     critical section that enqueues it, closing the "row in SQLite but
//     not in memory" window that reloadFromDisk would otherwise mistake
//     for a stranded row.
//   - opUpdates are best-effort and NOT persisted. A lost phase update
//     is cheap; a lost terminal status leaves the op to global's timeout
//     sweeps. A completion that must not be lost rides a durable
//     observation as well: the deploy's phone_home carries the op ID
//     (see Cache.RecordPhoneHome).
//   - ackObservation deletes the SQLite row only after global confirms
//     receipt via rpc_response{ok:true}.
//   - reloadFromDisk re-enqueues rows that were sent but not yet acked
//     when a connection drops, so a disconnect mid-ack-window doesn't
//     strand the observation until the next worker restart.
//   - newOutbox rehydrates unacked rows on restart and wakes the drain
//     loop, so they are re-sent on the first connection.
type outbox struct {
	mu        sync.Mutex
	pending   []PendingOutbox
	opUpdates []model.OperationUpdate

	// retryTimer wakes the drain loop when the earliest backed-off row
	// becomes due. The drain loop is otherwise purely wake-driven (wakeCh
	// fires on rehydrate/append/requeue), so when drain holds back not-due
	// rows and nothing else writes to the queue, no wake would ever fire and
	// the rows would strand until the next WS reconnect. drain (re)arms this
	// for the earliest NextAttempt whenever it holds rows back. Guarded by
	// mu. The callback is wake(), which is idempotent and non-blocking, so
	// a stale timer firing after a re-arm only costs one spurious drain.
	retryTimer *time.Timer

	// wakeCh signals that new data is available for drain (logs, op
	// updates, observations). Buffered(1) so writers never block. Cache
	// aliases this as Cache.Wake for the WS drain loop and tests.
	wakeCh chan struct{}

	// localDB persists the outbox for crash recovery. nil when running
	// without persistence (unit tests).
	localDB *LocalDB

	// RejectBackoffBase / RejectBackoffMax tune the per-row exponential
	// backoff applied when global rejects an observation non-terminally.
	// Zero values default to 1s base, 60s cap. Exported so
	// tests can inject short durations, mirroring CastPumpConfig's
	// ReconnectBackoff/MaxReconnectBackoff convention.
	RejectBackoffBase time.Duration
	RejectBackoffMax  time.Duration
}

// newOutbox builds an outbox, rehydrating any persisted pending rows from
// localDB (nil disables persistence). The wake channel is buffered(1).
//
// Rehydrated rows get a wake like any other enqueue: the drain loop is
// wake-driven, and reloadFromDisk on the first connection adds nothing
// (the rows are already in memory), so without this token a row left
// unacked by the previous process would wait for an unrelated append.
// The token sits in the buffered channel until the first connection's
// drain loop takes it.
func newOutbox(localDB *LocalDB) *outbox {
	o := &outbox{
		wakeCh:  make(chan struct{}, 1),
		localDB: localDB,
	}
	if localDB != nil {
		pending, err := localDB.ListPendingOutbox()
		if err != nil {
			// Not fatal here: the worker still serves boots from its
			// cache. Every connection to global starts with
			// reloadFromDisk, which reads the table again and fails the
			// connection if it still cannot.
			slog.Error("outbox rows persisted before this start could not be read; they are read again when the worker connects to global",
				"err", err)
		}
		if len(pending) > 0 {
			o.pending = pending
			o.wake()
		}
	}
	return o
}

// wake signals that new data is available for drain.
func (o *outbox) wake() {
	select {
	case o.wakeCh <- struct{}{}:
	default:
	}
}

func (o *outbox) appendObservation(obs model.Observation) {
	// Persist + enqueue under the same lock. The window where "row exists
	// in SQLite but not in pending" is what reloadFromDisk uses to recover
	// from disconnect, so we must close that window for the append path: a
	// concurrent reload would otherwise see the just-persisted row as
	// "stranded" and enqueue a duplicate.
	o.mu.Lock()
	var rowID uint64
	if o.localDB != nil {
		var err error
		rowID, err = o.localDB.AppendOutbox(obs)
		if err != nil {
			slog.Warn("outbox persist failed, keeping in-memory only", "err", err)
		}
	}
	o.pending = append(o.pending, PendingOutbox{RowID: rowID, Obs: obs})
	o.mu.Unlock()
	o.wake()
}

func (o *outbox) appendOpUpdate(u model.OperationUpdate) {
	o.mu.Lock()
	o.opUpdates = append(o.opUpdates, u)
	o.mu.Unlock()
	o.wake()
}

// depth returns the number of in-memory observations awaiting drain+ack.
func (o *outbox) depth() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

// drain returns pending observations (paired with SQLite row IDs for
// later acking) and operation updates, clearing the in-memory queues.
// SQLite rows remain until ackObservation fires on receipt of a
// successful rpc_response from global.
//
// Rows whose NextAttempt is in the future (backed off after a
// non-terminal rejection, see requeueRejected) are NOT returned: they
// stay queued, in order, until due. Whenever rows are held back, the
// retry timer is (re)armed for the earliest NextAttempt so the
// wake-driven drain loop is guaranteed a future wake even if nothing else
// touches the outbox — without it, held-back rows would strand until the
// next WS reconnect's reloadFromDisk.
func (o *outbox) drain() ([]PendingOutbox, []model.OperationUpdate) {
	o.mu.Lock()
	defer o.mu.Unlock()

	updates := o.opUpdates
	o.opUpdates = nil

	if len(o.pending) == 0 {
		o.pending = nil
		return nil, updates
	}

	now := time.Now()
	var due, held []PendingOutbox
	var earliest time.Time
	for _, p := range o.pending {
		if p.NextAttempt.After(now) {
			held = append(held, p)
			if earliest.IsZero() || p.NextAttempt.Before(earliest) {
				earliest = p.NextAttempt
			}
			continue
		}
		due = append(due, p)
	}
	o.pending = held
	if len(held) > 0 {
		o.armRetryTimerLocked(earliest)
	}
	return due, updates
}

// armRetryTimerLocked schedules a wake() at the given time so the drain
// loop re-runs once a held-back row becomes due. Caller must hold mu. A
// single timer is reused; re-arming always wins over whatever was
// scheduled before, which is safe because every drain pass that still
// holds rows back re-arms it, and a stale fire only costs one spurious
// drain pass (wake is non-blocking and idempotent).
func (o *outbox) armRetryTimerLocked(at time.Time) {
	d := max(time.Until(at), 0)
	if o.retryTimer == nil {
		o.retryTimer = time.AfterFunc(d, o.wake)
		return
	}
	o.retryTimer.Stop()
	o.retryTimer.Reset(d)
}

// rejectBackoffDefaults returns the effective (base, max) for the
// rejection backoff, applying the production defaults of 1s base / 60s cap
// when unset.
func (o *outbox) rejectBackoffDefaults() (base, maxDelay time.Duration) {
	base = o.RejectBackoffBase
	if base == 0 {
		base = time.Second
	}
	maxDelay = o.RejectBackoffMax
	if maxDelay == 0 {
		maxDelay = 60 * time.Second
	}
	return base, maxDelay
}

// requeueRejected returns a non-terminally rejected observation to the
// outbox with per-row exponential backoff: attempt N waits
// base*2^(N-1) plus up to 20% jitter, capped at RejectBackoffMax. Without
// this, a persistent transient rejection (e.g. NetBox down on the global
// side) re-sent the row as fast as the WS round-trip allowed — observed at
// ~2.3 retries/second, each one driving a NetBox call and a WARN log line
// on both ends.
//
// The row is appended rather than prepended: with per-row due-times,
// strict cross-row ordering is no longer meaningful. A NEWER observation
// for the same machine can therefore overtake a backed-off rejected row;
// that's acceptable because global's observation handlers are idempotent
// (see internal/commission/handler.go) — when the old row finally lands,
// re-applying it is harmless.
//
// Returns the updated attempt count, the computed delay, and whether the
// caller should log this rejection at WARN (first rejection of a row, or
// at most once per minute while pinned at the backoff cap) instead of
// Debug.
func (o *outbox) requeueRejected(entry PendingOutbox) (attempts int, delay time.Duration, warn bool) {
	base, capMax := o.rejectBackoffDefaults()

	entry.Attempts++
	raw := nextBackoff(base, capMax, entry.Attempts-1)
	atCap := raw >= capMax
	delay = applyJitter(raw, 0.2)

	now := time.Now()
	warn = entry.Attempts == 1
	if atCap && now.Sub(entry.capWarnAt) >= time.Minute {
		warn = true
		entry.capWarnAt = now
	}
	entry.NextAttempt = now.Add(delay)

	o.mu.Lock()
	o.pending = append(o.pending, entry)
	o.mu.Unlock()
	// Wake the drain loop even though the row isn't due: the resulting
	// drain pass holds it back and arms the retry timer, so timer arming
	// lives in exactly one place (drain).
	o.wake()
	return entry.Attempts, delay, warn
}

// requeue prepends unsent items back onto the in-memory queues. Called
// from the WS drain path when a send fails mid-batch: the caller hands
// back the remainder of whatever drain returned and the next wake retries.
// Items added after the drain (via appendObservation / appendOpUpdate
// during the failed send) are preserved and appear after the requeued
// prefix — same effective ordering as "had the drain not happened yet."
func (o *outbox) requeue(obs []PendingOutbox, updates []model.OperationUpdate) {
	if len(obs) == 0 && len(updates) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(obs) > 0 {
		o.pending = append(obs, o.pending...)
	}
	if len(updates) > 0 {
		o.opUpdates = append(updates, o.opUpdates...)
	}
	o.wake()
}

// ackObservation removes an observation from durable storage after global
// has acked its receipt via rpc_response{ok:true}. The in-memory queue
// entry is already gone (drain removed it and the WS layer has been
// holding it until ack arrived).
func (o *outbox) ackObservation(rowID uint64) {
	if o.localDB != nil && rowID != 0 {
		o.localDB.AckOutboxRow(rowID)
	}
}

// reloadFromDisk repopulates the in-memory outbox with any SQLite rows
// whose row IDs are not already present in memory.
//
// Called on every WS reconnect so observations that were in-flight when a
// previous connection dropped — sent to the wire but never acked — come
// back into the drain queue and get re-sent. Without this, a disconnect
// during the ack window would strand the observation in SQLite until the
// next worker restart.
//
// Holds mu across the SQLite read so a concurrent appendObservation can't
// finish its persist between our ListPending and the membership check —
// which would otherwise make us enqueue a duplicate when append later adds
// the same row to the in-memory slice.
//
// An error means the table could not be read and nothing was re-enqueued.
// The rows it holds are then neither in memory nor on their way to global,
// so the caller must not start draining as if the outbox were complete.
func (o *outbox) reloadFromDisk() error {
	if o.localDB == nil {
		return nil
	}
	o.mu.Lock()
	pending, err := o.localDB.ListPendingOutbox()
	if err != nil {
		o.mu.Unlock()
		return err
	}
	if len(pending) == 0 {
		o.mu.Unlock()
		return nil
	}
	seen := make(map[uint64]struct{}, len(o.pending))
	for _, p := range o.pending {
		if p.RowID != 0 {
			seen[p.RowID] = struct{}{}
		}
	}
	added := 0
	for _, p := range pending {
		if p.RowID == 0 {
			continue
		}
		if _, dup := seen[p.RowID]; dup {
			continue
		}
		o.pending = append(o.pending, p)
		added++
	}
	o.mu.Unlock()
	if added > 0 {
		o.wake()
	}
	return nil
}
