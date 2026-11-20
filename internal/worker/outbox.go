package worker

import (
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// outbox is the site worker's durable send queue for observations and
// operation updates. It owns crash-recovery persistence (via LocalDB),
// the in-memory queue, the per-row rejection backoff, the per-operation
// delivery order, and the wake signal the WS drain loop parks on.
//
// Split out of Cache (fable-review Tier 1 #4): this is one of the five
// responsibilities Cache used to mix. Everything here is guarded by mu
// (appends are serialized by appendMu as well); nothing here touches
// snapshot-derived read state or op-phase state, so it needs no
// coordination with the cache's other locks.
//
// Durability contract:
//   - append persists the entry to SQLite, then queues it, before it
//     returns. Appends are serialized, so SQLite row order is record
//     order, and memory and disk hold the same entries.
//   - entries holds every entry global has not acknowledged, in the order
//     they were recorded, including the ones in flight: an entry leaves
//     memory and disk together, in ack, once global acknowledged it
//     (rpc_response without error, sent after its handler committed) or
//     rejected it terminally.
//   - reloadFromDisk makes every in-flight entry due again on a
//     reconnect: the connection it went out on is gone, and global may or
//     may not have committed it. Delivery is at least once; global's
//     handlers are idempotent.
//   - newOutbox rehydrates unacked rows on restart and wakes the drain
//     loop, so they are re-sent on the first connection. Rows that
//     startup read could not load are picked up by reloadFromDisk on a
//     later connection.
//
// Order: the entries that settle an operation (every op update, and the
// observations that complete or fail an op, see orderedOp) are delivered
// one at a time per operation, in record order. drain hands out such an
// entry only when no earlier entry of the same operation is still
// unacknowledged, so global commits them in the order the worker recorded
// them whatever order its handlers run in, and a replay after a lost ack
// can only repeat the latest entry global saw for that op, never an older
// one after a newer. Other entries (no op, or observations that do not
// settle one) go out as soon as they are due.
type outbox struct {
	// appendMu serializes the methods that write the SQLite table (append,
	// ack) and reloadFromDisk, which reads it: row order then follows
	// record order, and a reload never merges a snapshot of the table that
	// an append or ack has since changed. It is taken before mu and never
	// while holding it, so drain and the requeues never wait on a disk
	// write.
	appendMu sync.Mutex

	mu      sync.Mutex
	entries []PendingOutbox
	nextSeq uint64

	// retryTimer wakes the drain loop when the earliest backed-off row
	// becomes due. The drain loop is otherwise purely wake-driven (wakeCh
	// fires on rehydrate/append/requeue/ack), so when drain holds back
	// not-due rows and nothing else writes to the queue, no wake would ever
	// fire and the rows would strand until the next WS reconnect. drain
	// (re)arms this for the earliest NextAttempt whenever it holds rows
	// back. Guarded by mu. The callback is wake(), which is idempotent and
	// non-blocking, so a stale timer firing after a re-arm only costs one
	// spurious drain.
	retryTimer *time.Timer

	// wakeCh signals that new data is available for drain (logs, op
	// updates, observations). Buffered(1) so writers never block. Cache
	// aliases this as Cache.Wake for the WS drain loop and tests.
	wakeCh chan struct{}

	// localDB persists the outbox for crash recovery. nil when running
	// without persistence (unit tests).
	localDB *LocalDB

	// RejectBackoffBase / RejectBackoffMax tune the per-row exponential
	// backoff applied when global rejects an entry non-terminally.
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
// wake-driven, so without this token a row left unacked by the previous
// process would wait for an unrelated append. The token sits in the
// buffered channel until the first connection's drain loop takes it.
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
		for _, e := range pending {
			o.nextSeq++
			e.seq = o.nextSeq
			o.entries = append(o.entries, e)
		}
		if len(o.entries) > 0 {
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

// orderedOp reports whether e belongs to an operation's ordered chain, and
// which: every op update, and the observations whose handler completes or
// fails the op they name (Cache.RecordPhoneHome, recordCommissionReport,
// recordBMCReconcileReport). deploy_complete names its op but only
// validates it and stores the monitor credential: it stays out of the
// chain, so a NetBox outage that keeps rejecting it does not hold back the
// phone_home that completes the deploy.
func orderedOp(e PendingOutbox) (string, bool) {
	if e.Update != nil {
		// An agent report's op ID reaches UpdateOpPhase / MarkOpFailed as
		// the string the agent sent; key the chain by the canonical form
		// the observations use.
		if id, err := uuid.Parse(e.Update.OperationID); err == nil {
			return id.String(), true
		}
		return e.Update.OperationID, true
	}
	if e.Obs.OperationID == uuid.Nil {
		return "", false
	}
	switch e.Obs.Event {
	case "phone_home", "commission_report", "bmc_reconcile_report":
		return e.Obs.OperationID.String(), true
	}
	return "", false
}

func (o *outbox) appendObservation(obs model.Observation) {
	o.append(PendingOutbox{Obs: obs})
}

func (o *outbox) appendOpUpdate(u model.OperationUpdate) {
	o.append(PendingOutbox{Update: &u})
}

// append persists e and queues it; it returns once both are done. A
// failed persist keeps the entry in memory only: it is still delivered and
// acknowledged, but a restart loses it.
func (o *outbox) append(e PendingOutbox) {
	o.appendMu.Lock()
	defer o.appendMu.Unlock()
	if o.localDB != nil {
		rowID, err := o.localDB.AppendOutbox(e)
		if err != nil {
			slog.Warn("outbox persist failed, keeping in-memory only", "err", err)
		}
		e.RowID = rowID
	}
	o.mu.Lock()
	o.nextSeq++
	e.seq = o.nextSeq
	o.entries = append(o.entries, e)
	o.mu.Unlock()
	o.wake()
}

// depth returns the number of entries global has not acknowledged yet,
// in flight included.
func (o *outbox) depth() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.entries)
}

// indexLocked returns the position of the entry with sequence number seq,
// or -1. Caller must hold mu.
func (o *outbox) indexLocked(seq uint64) int {
	for i := range o.entries {
		if o.entries[i].seq == seq {
			return i
		}
	}
	return -1
}

// drain returns the entries to send now, in record order, and marks them
// in flight: they stay in the outbox until ack, requeue, requeueRejected
// or reloadFromDisk.
//
// An entry is returned when it is not already in flight, is due (rows
// backed off after a non-terminal rejection wait for their NextAttempt),
// and, for an entry of an operation's ordered chain, when no earlier entry
// of that chain is still unacknowledged. Whenever rows are held back for
// their NextAttempt, the retry timer is (re)armed for the earliest one so
// the wake-driven drain loop is guaranteed a future wake even if nothing
// else touches the outbox.
func (o *outbox) drain() []PendingOutbox {
	o.mu.Lock()
	defer o.mu.Unlock()

	now := time.Now()
	var out []PendingOutbox
	var earliest time.Time
	blocked := map[string]bool{}
	for i := range o.entries {
		e := &o.entries[i]
		op, ordered := orderedOp(*e)
		if ordered && blocked[op] {
			continue
		}
		switch {
		case e.inFlight:
		case e.NextAttempt.After(now):
			if earliest.IsZero() || e.NextAttempt.Before(earliest) {
				earliest = e.NextAttempt
			}
		default:
			e.inFlight = true
			out = append(out, *e)
		}
		if ordered {
			blocked[op] = true
		}
	}
	if !earliest.IsZero() {
		o.armRetryTimerLocked(earliest)
	}
	return out
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

// requeueRejected returns a non-terminally rejected in-flight entry to the
// queue with per-row exponential backoff: attempt N waits base*2^(N-1)
// plus up to 20% jitter, capped at RejectBackoffMax. Without this, a
// persistent transient rejection (e.g. NetBox down on the global side)
// re-sent the row as fast as the WS round-trip allowed — observed at ~2.3
// retries/second, each one driving a NetBox call and a WARN log line on
// both ends.
//
// The entry keeps its place in record order. An unordered entry queued
// after it can overtake it while it waits; that's acceptable because
// global's observation handlers are idempotent (see
// internal/commission/handler.go). The later entries of its operation's
// ordered chain wait for it.
//
// Returns the updated attempt count, the computed delay, and whether the
// caller should log this rejection at WARN (first rejection of a row, or
// at most once per minute while pinned at the backoff cap) instead of
// Debug. An entry the outbox no longer holds changes nothing.
func (o *outbox) requeueRejected(entry PendingOutbox) (attempts int, delay time.Duration, warn bool) {
	base, capMax := o.rejectBackoffDefaults()

	o.mu.Lock()
	i := o.indexLocked(entry.seq)
	if i < 0 {
		o.mu.Unlock()
		return entry.Attempts, 0, false
	}
	e := &o.entries[i]
	e.Attempts++
	raw := nextBackoff(base, capMax, e.Attempts-1)
	atCap := raw >= capMax
	delay = applyJitter(raw, 0.2)

	now := time.Now()
	warn = e.Attempts == 1
	if atCap && now.Sub(e.capWarnAt) >= time.Minute {
		warn = true
		e.capWarnAt = now
	}
	e.NextAttempt = now.Add(delay)
	e.inFlight = false
	attempts = e.Attempts
	o.mu.Unlock()
	// Wake the drain loop even though the row isn't due: the resulting
	// drain pass holds it back and arms the retry timer, so timer arming
	// lives in exactly one place (drain).
	o.wake()
	return attempts, delay, warn
}

// requeue returns in-flight entries that were never sent to the queue.
// Called from the WS drain path when a send fails mid-batch: the caller
// hands back the remainder of whatever drain returned and the next wake
// retries. The entries keep their place in record order.
func (o *outbox) requeue(entries []PendingOutbox) {
	if len(entries) == 0 {
		return
	}
	o.mu.Lock()
	for _, e := range entries {
		if i := o.indexLocked(e.seq); i >= 0 {
			o.entries[i].inFlight = false
		}
	}
	o.mu.Unlock()
	o.wake()
}

// reloadFromDisk is the reset for a new WS connection, called on every
// (re)connect before the drain loop starts.
//
// Every in-flight entry becomes due again: whatever went out on the
// previous connection got no acknowledgement the new one could carry, and
// global may or may not have committed it.
//
// The table is also read again, and a row memory lacks is queued ahead of
// everything in memory, in row order. In-process appends always reach
// memory (appendMu is held here, so none is between its SQLite write and
// its queueing), so such a row is one the startup read could not load:
// older than anything this process recorded.
//
// An error means the table could not be read and nothing was added. Rows
// a failed startup read left behind are then neither in memory nor on
// their way to global, so the caller must not start draining as if the
// outbox were complete.
func (o *outbox) reloadFromDisk() error {
	o.appendMu.Lock()
	defer o.appendMu.Unlock()
	var disk []PendingOutbox
	if o.localDB != nil {
		var err error
		if disk, err = o.localDB.ListPendingOutbox(); err != nil {
			return err
		}
	}
	o.mu.Lock()
	inMemory := make(map[uint64]bool, len(o.entries))
	for i := range o.entries {
		if o.entries[i].RowID != 0 {
			inMemory[o.entries[i].RowID] = true
		}
		o.entries[i].inFlight = false
	}
	var missing []PendingOutbox
	for _, e := range disk {
		if !inMemory[e.RowID] {
			o.nextSeq++
			e.seq = o.nextSeq
			missing = append(missing, e)
		}
	}
	if len(missing) > 0 {
		o.entries = append(missing, o.entries...)
	}
	queued := len(o.entries)
	o.mu.Unlock()
	if queued > 0 {
		o.wake()
	}
	return nil
}

// ack removes an entry from durable storage and then from memory once
// global acknowledged it (or rejected it terminally, or the worker cannot
// send it at all). Removing an entry of an ordered chain unblocks the next
// one of its operation, so ack wakes the drain loop for it.
//
// The row goes first: a row left on disk after its entry left memory would
// come back on the next restart or reload after the later entries of its
// chain, and replay an older phase over a newer one. So a failed delete
// keeps the entry, backed off like a transient rejection: the resend is
// answered without effect (global already committed it) and ack tries the
// delete again. appendMu keeps ack out of reloadFromDisk's window between
// reading the table and merging it.
func (o *outbox) ack(entry PendingOutbox) {
	o.appendMu.Lock()
	defer o.appendMu.Unlock()
	if o.localDB != nil && entry.RowID != 0 {
		if err := o.localDB.AckOutboxRow(entry.RowID); err != nil {
			slog.Error("outbox entry acknowledged but its row could not be deleted; keeping it and sending it again after a backoff",
				append(outboxLogAttrs(entry), "row_id", entry.RowID, "err", err)...)
			o.requeueRejected(entry)
			return
		}
	}
	o.mu.Lock()
	i := o.indexLocked(entry.seq)
	if i >= 0 {
		o.entries = append(o.entries[:i], o.entries[i+1:]...)
	}
	o.mu.Unlock()
	if i < 0 {
		return
	}
	if _, ordered := orderedOp(entry); ordered {
		o.wake()
	}
}
