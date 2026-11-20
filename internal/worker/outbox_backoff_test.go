package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Tests for the rejected-entry backoff: when global rejects an outbox
// entry non-terminally, it is requeued with per-row exponential backoff
// instead of being re-sent immediately (observed live at ~2.3
// retries/second against a transient NetBox failure). Conventions follow
// castpump_backoff_test.go: short injected bases, no real multi-second
// sleeps.

// soleEntry returns the single entry in the cache's outbox after marking
// it in flight, bypassing DrainOutbox's due-time filter. Used to iterate
// RequeueRejected on the same logical row the way the ackPending
// round-trip does in production.
func soleEntry(t *testing.T, c *Cache) PendingOutbox {
	t.Helper()
	c.outbox.mu.Lock()
	defer c.outbox.mu.Unlock()
	if len(c.outbox.entries) != 1 {
		t.Fatalf("outbox has %d entries, want 1", len(c.outbox.entries))
	}
	c.outbox.entries[0].inFlight = true
	return c.outbox.entries[0]
}

// drainOne drains the outbox and returns its single due entry.
func drainOne(t *testing.T, c *Cache) PendingOutbox {
	t.Helper()
	got := c.DrainOutbox()
	if len(got) != 1 {
		t.Fatalf("drained %d entries, want 1: %+v", len(got), got)
	}
	return got[0]
}

func drainWake(c *Cache) {
	select {
	case <-c.Wake:
	default:
	}
}

// TestDrainOutbox_RejectedRowHeldUntilDue: a rejected row is not
// returned by DrainOutbox while its NextAttempt is in the future, and
// is returned once due.
func TestDrainOutbox_RejectedRowHeldUntilDue(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = 50 * time.Millisecond
	c.outbox.RejectBackoffMax = 200 * time.Millisecond

	c.appendObservation(model.Observation{MachineID: uuid.New(), Event: "commission_report"})
	_, delay, _ := c.RequeueRejected(drainOne(t, c))

	if got := c.DrainOutbox(); len(got) != 0 {
		t.Fatalf("rejected row returned before due: %+v", got)
	}

	// Poll until due (delay is 50-60ms; bound the wait well above it).
	deadline := time.Now().Add(2 * time.Second)
	var got []PendingOutbox
	for {
		got = c.DrainOutbox()
		if len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row never became due (delay was %v)", delay)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got[0].Obs.Event != "commission_report" || got[0].Attempts != 1 {
		t.Errorf("got row %+v, want the commission_report with Attempts=1", got[0])
	}
}

// TestDrainOutbox_TimerWakesWhenRowBecomesDue is the no-stranding
// test: the drain loop is wake-driven, so when DrainOutbox holds back
// a not-due row it MUST arrange a future wake. Without the retry
// timer, a rejected entry with nothing else flowing through the
// outbox would never be re-sent until a WS reconnect.
func TestDrainOutbox_TimerWakesWhenRowBecomesDue(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = 50 * time.Millisecond
	c.outbox.RejectBackoffMax = 200 * time.Millisecond

	c.RecordOpUpdate(uuid.NewString(), string(model.OpCompleted), nil)
	c.RequeueRejected(drainOne(t, c))
	// Consume the immediate wake RequeueRejected emits, then drain:
	// the row is held back and the retry timer is armed.
	drainWake(c)
	if got := c.DrainOutbox(); len(got) != 0 {
		t.Fatalf("row should be held back, got %+v", got)
	}

	// No further outbox activity. The ONLY thing that can wake us now
	// is the retry timer.
	select {
	case <-c.Wake:
	case <-time.After(2 * time.Second):
		t.Fatal("no wake fired when held-back row became due; row would strand")
	}
	got := c.DrainOutbox()
	if len(got) != 1 || got[0].Update == nil {
		t.Fatalf("post-wake drain: got %+v, want the held op update", got)
	}
}

// TestRequeueRejected_AttemptsGrowAndCap: attempts increment per
// rejection, the pre-jitter delay doubles from base, jitter stays
// within [raw, 1.2*raw], and the delay saturates at the cap.
func TestRequeueRejected_AttemptsGrowAndCap(t *testing.T) {
	base := 10 * time.Millisecond
	capMax := 80 * time.Millisecond
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = base
	c.outbox.RejectBackoffMax = capMax

	c.appendObservation(model.Observation{MachineID: uuid.New(), Event: "commission_report"})
	for i := 1; i <= 8; i++ {
		entry := soleEntry(t, c)
		before := time.Now()
		attempts, delay, _ := c.RequeueRejected(entry)
		if attempts != i {
			t.Fatalf("iteration %d: attempts = %d", i, attempts)
		}
		raw := nextBackoff(base, capMax, i-1)
		ceil := raw + time.Duration(0.2*float64(raw))
		if delay < raw || delay > ceil {
			t.Errorf("attempt %d: delay %v outside [%v, %v]", i, delay, raw, ceil)
		}
		c.outbox.mu.Lock()
		next := c.outbox.entries[0].NextAttempt
		c.outbox.mu.Unlock()
		if next.Before(before.Add(raw)) {
			t.Errorf("attempt %d: NextAttempt %v earlier than %v", i, next, before.Add(raw))
		}
	}
}

// TestRequeueRejected_WarnDemotion: first rejection of a row warns,
// in-between rejections don't, the first cap-saturated rejection
// warns again, and an immediately following cap rejection is
// rate-limited (sub-minute) back to debug.
func TestRequeueRejected_WarnDemotion(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = 10 * time.Millisecond
	c.outbox.RejectBackoffMax = 80 * time.Millisecond // cap reached at attempt 4

	c.appendObservation(model.Observation{MachineID: uuid.New(), Event: "commission_report"})
	wantWarn := []bool{true, false, false, true, false, false}
	for i, want := range wantWarn {
		_, _, warn := c.RequeueRejected(soleEntry(t, c))
		if warn != want {
			t.Errorf("attempt %d: warn = %v, want %v", i+1, warn, want)
		}
	}
}

// TestDrainOutbox_MixedDueAndHeldOrdering: due rows are returned in
// their record order while a backed-off row stays held, and the held
// row doesn't disturb the relative order of the rows around it.
func TestDrainOutbox_MixedDueAndHeldOrdering(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = time.Minute // held row stays held for the whole test
	c.outbox.RejectBackoffMax = time.Minute

	mid := uuid.New()
	c.appendObservation(model.Observation{MachineID: mid, Event: "first"})
	c.appendObservation(model.Observation{MachineID: mid, Event: "rejected"})
	c.appendObservation(model.Observation{MachineID: mid, Event: "second"})
	sent := c.DrainOutbox()
	if len(sent) != 3 {
		t.Fatalf("drained %d entries, want 3", len(sent))
	}
	c.RequeueRejected(sent[1])
	c.RequeueOutbox([]PendingOutbox{sent[0], sent[2]})

	got := c.DrainOutbox()
	if len(got) != 2 {
		t.Fatalf("got %d due rows, want 2: %+v", len(got), got)
	}
	if got[0].Obs.Event != "first" || got[1].Obs.Event != "second" {
		t.Errorf("due rows out of order: %s, %s", got[0].Obs.Event, got[1].Obs.Event)
	}
	if depth := c.OutboxDepth(); depth != 3 {
		t.Errorf("unacknowledged entries = %d, want 3 (two in flight, one held)", depth)
	}
}

// TestDrainOutbox_OrderedChainWaitsForHeldEntry: an op's later updates
// wait behind an earlier one global rejected transiently, whatever its
// backoff, so global never sees them out of order; entries outside the
// chain keep flowing.
func TestDrainOutbox_OrderedChainWaitsForHeldEntry(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = time.Minute
	c.outbox.RejectBackoffMax = time.Minute

	op := uuid.NewString()
	c.appendOpUpdate(model.OperationUpdate{OperationID: op, Phase: "power_cycle"})
	first := drainOne(t, c)
	c.appendOpUpdate(model.OperationUpdate{OperationID: op, Phase: "pxe_booting"})
	c.appendObservation(model.Observation{MachineID: uuid.New(), Event: "dhcp_lease"})
	c.RequeueRejected(first)

	got := c.DrainOutbox()
	if len(got) != 1 || got[0].Obs.Event != "dhcp_lease" {
		t.Fatalf("drained %+v, want only the dhcp_lease: pxe_booting must wait for power_cycle", got)
	}

	// The backoff elapses; power_cycle goes out again, pxe_booting still waits.
	c.outbox.mu.Lock()
	for i := range c.outbox.entries {
		c.outbox.entries[i].NextAttempt = time.Time{}
	}
	c.outbox.mu.Unlock()
	resent := drainOne(t, c)
	if resent.Update == nil || resent.Update.Phase != "power_cycle" {
		t.Fatalf("resent %+v, want power_cycle", resent)
	}
	c.AckOutbox(resent)
	next := drainOne(t, c)
	if next.Update == nil || next.Update.Phase != "pxe_booting" {
		t.Fatalf("after power_cycle's ack drained %+v, want pxe_booting", next)
	}
}

// TestHandleOutboxAck_NonTerminalRequeuesWithBackoff: a non-terminal
// rpc_response error puts the tracked entry back in the queue with
// backoff state, and the row is NOT immediately re-drainable (the
// live-bug behavior was an instant re-send).
func TestHandleOutboxAck_NonTerminalRequeuesWithBackoff(t *testing.T) {
	cache := NewCache(nil)
	cache.outbox.RejectBackoffBase = time.Minute
	cache.outbox.RejectBackoffMax = time.Minute
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	cache.appendObservation(model.Observation{MachineID: uuid.New(), Event: "commission_report"})
	w.trackAck("req-89", drainOne(t, cache))

	data, _ := json.Marshal(ws.RPCResponseMsg{Error: "netbox: 502 bad gateway"})
	if !w.handleOutboxAck("req-89", data) {
		t.Fatal("ack not consumed for tracked request_id")
	}

	if got := cache.DrainOutbox(); len(got) != 0 {
		t.Fatalf("rejected row immediately re-drainable: %+v", got)
	}
	got := soleEntry(t, cache)
	if got.Attempts != 1 || !got.NextAttempt.After(time.Now()) {
		t.Errorf("requeued row missing backoff state: %+v", got)
	}
}

// TestHandleOutboxAck_MalformedResponseRequeues: a response the worker
// cannot decode says nothing about whether global committed the entry, so
// it is retried like a transient rejection rather than dropped or left in
// flight until a reconnect.
func TestHandleOutboxAck_MalformedResponseRequeues(t *testing.T) {
	cache := NewCache(nil)
	cache.outbox.RejectBackoffBase = time.Minute
	cache.outbox.RejectBackoffMax = time.Minute
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	cache.RecordOpUpdate(uuid.NewString(), string(model.OpCompleted), nil)
	w.trackAck("req-1", drainOne(t, cache))
	if !w.handleOutboxAck("req-1", json.RawMessage(`{"error":`)) {
		t.Fatal("ack not consumed for tracked request_id")
	}
	got := soleEntry(t, cache)
	if got.Attempts != 1 || !got.NextAttempt.After(time.Now()) {
		t.Errorf("entry after a malformed response: %+v, want requeued with backoff", got)
	}
}

// TestHandleOutboxAck_TerminalDropsImmediately: a terminal rejection
// drops the row (acked away) with no requeue and no retry state.
func TestHandleOutboxAck_TerminalDropsImmediately(t *testing.T) {
	cache := NewCache(nil)
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	cache.RecordOpUpdate(uuid.NewString(), string(model.OpFailed), nil)
	w.trackAck("req-7", drainOne(t, cache))

	data, _ := json.Marshal(ws.RPCResponseMsg{Error: "operation not found", Terminal: true})
	if !w.handleOutboxAck("req-7", data) {
		t.Fatal("ack not consumed for tracked request_id")
	}

	if depth := cache.OutboxDepth(); depth != 0 {
		t.Errorf("terminal rejection left %d rows in outbox, want 0", depth)
	}
}

// TestHandleOutboxAck_SuccessAcksRow: the success path removes the
// row without requeueing.
func TestHandleOutboxAck_SuccessAcksRow(t *testing.T) {
	cache := NewCache(nil)
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	cache.appendObservation(model.Observation{MachineID: uuid.New(), Event: "phone_home"})
	w.trackAck("req-11", drainOne(t, cache))

	data, _ := json.Marshal(ws.RPCResponseMsg{})
	if !w.handleOutboxAck("req-11", data) {
		t.Fatal("ack not consumed for tracked request_id")
	}
	if depth := cache.OutboxDepth(); depth != 0 {
		t.Errorf("success ack left %d rows in outbox, want 0", depth)
	}
}
