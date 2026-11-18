package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Tests for the rejected-observation backoff: when global
// rejects an observation non-terminally, the row is requeued with
// per-row exponential backoff instead of being re-sent immediately
// (observed live at ~2.3 retries/second against a transient NetBox
// failure). Conventions follow castpump_backoff_test.go: short
// injected bases, no real multi-second sleeps.

// takeSoleOutboxRow removes and returns the single entry currently in
// the cache's in-memory outbox, bypassing DrainOutbox's due-time
// filter. Used to iterate RequeueRejected on the same logical row the
// way the ackPending round-trip does in production.
func takeSoleOutboxRow(t *testing.T, c *Cache) PendingOutbox {
	t.Helper()
	c.outbox.mu.Lock()
	defer c.outbox.mu.Unlock()
	if len(c.outbox.pending) != 1 {
		t.Fatalf("outbox has %d entries, want 1", len(c.outbox.pending))
	}
	e := c.outbox.pending[0]
	c.outbox.pending = nil
	return e
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

	entry := PendingOutbox{RowID: 1, Obs: model.Observation{MachineID: uuid.New(), Event: "commission_report"}}
	_, delay, _ := c.RequeueRejected(entry)

	obs, _ := c.DrainOutbox()
	if len(obs) != 0 {
		t.Fatalf("rejected row returned before due: %+v", obs)
	}

	// Poll until due (delay is 50-60ms; bound the wait well above it).
	deadline := time.Now().Add(2 * time.Second)
	for {
		obs, _ = c.DrainOutbox()
		if len(obs) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("row never became due (delay was %v)", delay)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if obs[0].RowID != 1 || obs[0].Attempts != 1 {
		t.Errorf("got row %+v, want RowID=1 Attempts=1", obs[0])
	}
}

// TestDrainOutbox_TimerWakesWhenRowBecomesDue is the no-stranding
// test: the drain loop is wake-driven, so when DrainOutbox holds back
// a not-due row it MUST arrange a future wake. Without the retry
// timer, a rejected observation with nothing else flowing through the
// outbox would never be re-sent until a WS reconnect.
func TestDrainOutbox_TimerWakesWhenRowBecomesDue(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = 50 * time.Millisecond
	c.outbox.RejectBackoffMax = 200 * time.Millisecond

	entry := PendingOutbox{RowID: 2, Obs: model.Observation{MachineID: uuid.New(), Event: "phone_home"}}
	c.RequeueRejected(entry)
	// Consume the immediate wake RequeueRejected emits, then drain:
	// the row is held back and the retry timer is armed.
	drainWake(c)
	if obs, _ := c.DrainOutbox(); len(obs) != 0 {
		t.Fatalf("row should be held back, got %+v", obs)
	}

	// No further outbox activity. The ONLY thing that can wake us now
	// is the retry timer.
	select {
	case <-c.Wake:
	case <-time.After(2 * time.Second):
		t.Fatal("no wake fired when held-back row became due; row would strand")
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 1 || obs[0].RowID != 2 {
		t.Fatalf("post-wake drain: got %+v, want the held row", obs)
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

	entry := PendingOutbox{RowID: 3, Obs: model.Observation{MachineID: uuid.New(), Event: "commission_report"}}
	for i := 1; i <= 8; i++ {
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
		entry = takeSoleOutboxRow(t, c)
		if entry.NextAttempt.Before(before.Add(raw)) {
			t.Errorf("attempt %d: NextAttempt %v earlier than %v", i, entry.NextAttempt, before.Add(raw))
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

	entry := PendingOutbox{RowID: 4, Obs: model.Observation{MachineID: uuid.New(), Event: "commission_report"}}
	wantWarn := []bool{true, false, false, true, false, false}
	for i, want := range wantWarn {
		_, _, warn := c.RequeueRejected(entry)
		if warn != want {
			t.Errorf("attempt %d: warn = %v, want %v", i+1, warn, want)
		}
		entry = takeSoleOutboxRow(t, c)
	}
}

// TestDrainOutbox_MixedDueAndHeldOrdering: due rows are returned in
// their queue order while a backed-off row stays held, and the held
// row doesn't disturb the relative order of rows queued after it.
func TestDrainOutbox_MixedDueAndHeldOrdering(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = time.Minute // held row stays held for the whole test
	c.outbox.RejectBackoffMax = time.Minute

	mid := uuid.New()
	c.appendObservation(model.Observation{MachineID: mid, Event: "first"})
	c.RequeueRejected(PendingOutbox{RowID: 9, Obs: model.Observation{MachineID: mid, Event: "rejected"}})
	c.appendObservation(model.Observation{MachineID: mid, Event: "second"})

	obs, _ := c.DrainOutbox()
	if len(obs) != 2 {
		t.Fatalf("got %d due rows, want 2: %+v", len(obs), obs)
	}
	if obs[0].Obs.Event != "first" || obs[1].Obs.Event != "second" {
		t.Errorf("due rows out of order: %s, %s", obs[0].Obs.Event, obs[1].Obs.Event)
	}
	if depth := c.OutboxDepth(); depth != 1 {
		t.Errorf("held row count = %d, want 1", depth)
	}
}

// TestHandleObservationAck_NonTerminalRequeuesWithBackoff: a
// non-terminal rpc_response error puts the tracked entry back in the
// outbox with backoff state, and the row is NOT immediately
// re-drainable (the live-bug behavior was an instant re-send).
func TestHandleObservationAck_NonTerminalRequeuesWithBackoff(t *testing.T) {
	cache := NewCache(nil)
	cache.outbox.RejectBackoffBase = time.Minute
	cache.outbox.RejectBackoffMax = time.Minute
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	entry := PendingOutbox{RowID: 89, Obs: model.Observation{MachineID: uuid.New(), Event: "commission_report"}}
	w.trackAck("req-89", entry)

	data, _ := json.Marshal(ws.RPCResponseMsg{Error: "netbox: 502 bad gateway"})
	if !w.handleObservationAck("req-89", data) {
		t.Fatal("ack not consumed for tracked request_id")
	}

	if obs, _ := cache.DrainOutbox(); len(obs) != 0 {
		t.Fatalf("rejected row immediately re-drainable: %+v", obs)
	}
	got := takeSoleOutboxRow(t, cache)
	if got.RowID != 89 || got.Attempts != 1 || !got.NextAttempt.After(time.Now()) {
		t.Errorf("requeued row missing backoff state: %+v", got)
	}
}

// TestHandleObservationAck_TerminalDropsImmediately: a terminal
// rejection drops the row (acked away) with no requeue and no
// retry state.
func TestHandleObservationAck_TerminalDropsImmediately(t *testing.T) {
	cache := NewCache(nil)
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	entry := PendingOutbox{RowID: 7, Obs: model.Observation{MachineID: uuid.New(), Event: "commission_report"}}
	w.trackAck("req-7", entry)

	data, _ := json.Marshal(ws.RPCResponseMsg{Error: "machine not found", Terminal: true})
	if !w.handleObservationAck("req-7", data) {
		t.Fatal("ack not consumed for tracked request_id")
	}

	if depth := cache.OutboxDepth(); depth != 0 {
		t.Errorf("terminal rejection left %d rows in outbox, want 0", depth)
	}
}

// TestHandleObservationAck_SuccessAcksRow: the success path still
// removes the row without requeueing (regression guard around the
// new rejection plumbing).
func TestHandleObservationAck_SuccessAcksRow(t *testing.T) {
	cache := NewCache(nil)
	w := NewWSConn(WSConnConfig{}, cache, nil, nil)

	entry := PendingOutbox{RowID: 11, Obs: model.Observation{MachineID: uuid.New(), Event: "phone_home"}}
	w.trackAck("req-11", entry)

	data, _ := json.Marshal(ws.RPCResponseMsg{})
	if !w.handleObservationAck("req-11", data) {
		t.Fatal("ack not consumed for tracked request_id")
	}
	if depth := cache.OutboxDepth(); depth != 0 {
		t.Errorf("success ack left %d rows in outbox, want 0", depth)
	}
}
