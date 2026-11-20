package worker

// Regression tests for durable, acknowledged op updates. An op update (a
// phase, or a terminal status with its result) is an outbox entry like an
// observation: persisted before the call that records it returns, sent
// with a request_id, removed only once global answers it, and resent after
// a restart or reconnect that lost the answer. Before, op updates were kept
// in memory, written to the socket once and never acknowledged, so a power
// op's result was lost when global crashed between reading it and
// committing it, or when the worker restarted before sending it, and the
// stuck-op sweep failed an op whose host had been power-cycled.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// crashingOpUpdates is global's op_update handler that dies on the first
// delivery it reads: crash drops the connection the update arrived on, and
// commitFirst says whether the write committed before the crash (the ack is
// lost either way). Later deliveries commit normally.
type crashingOpUpdates struct {
	mu          sync.Mutex
	crash       func()
	commitFirst bool
	crashed     bool
	deliveries  []ws.OpUpdateMsg
	committed   []ws.OpUpdateMsg
}

func (h *crashingOpUpdates) HandleOpUpdate(_ context.Context, msg ws.OpUpdateMsg) error {
	h.mu.Lock()
	h.deliveries = append(h.deliveries, msg)
	first := !h.crashed
	h.crashed = true
	if !first || h.commitFirst {
		h.committed = append(h.committed, msg)
	}
	h.mu.Unlock()
	if first {
		h.crash()
		return errors.New("global crashed")
	}
	return nil
}

func (h *crashingOpUpdates) counts() (deliveries, committed int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.deliveries), len(h.committed)
}

// TestOpUpdate_ResentAfterGlobalCrash: global reads a power op's terminal
// op_update and crashes, before or after committing it; no answer reaches
// the worker. The worker keeps the update and sends it again on its next
// connection, and drops it only once global has answered.
func TestOpUpdate_ResentAfterGlobalCrash(t *testing.T) {
	for _, tc := range []struct {
		name        string
		commitFirst bool
		wantCommits int
	}{
		{name: "crash before commit", commitFirst: false, wantCommits: 1},
		// Committed, answer lost: the resend reaches global again, and its
		// handler must take it as the no-op it is.
		{name: "crash after commit", commitFirst: true, wantCommits: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &crashingOpUpdates{commitFirst: tc.commitFirst}
			e := newE2EEnvWith(t, t.TempDir(), h)
			defer e.close()
			h.crash = e.w.ForceDisconnect
			if !e2eWaitFor(20*time.Second, e.w.Connected) {
				t.Fatalf("worker never connected")
			}

			opID := uuid.NewString()
			e.cache.RecordOpUpdate(opID, string(model.OpCompleted), nil)

			if !e2eWaitFor(20*time.Second, func() bool {
				d, _ := h.counts()
				return d >= 2 && e.cache.OutboxDepth() == 0 && e.outboxRows() == 0
			}) {
				d, c := h.counts()
				t.Fatalf("op update not resent and acknowledged after global's crash: deliveries=%d commits=%d worker state: %s",
					d, c, e.workerState())
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.committed) != tc.wantCommits {
				t.Errorf("commits = %d, want %d", len(h.committed), tc.wantCommits)
			}
			for _, m := range h.deliveries {
				if m.OperationID != opID || m.Status != string(model.OpCompleted) {
					t.Errorf("delivered %+v, want the completion of %s", m, opID)
				}
			}
		})
	}
}

// TestOpUpdate_UnackedTerminalSurvivesRestart: a terminal op update is in
// SQLite before RecordOpUpdate returns. When the worker restarts after
// sending it but before global's answer, the new process sends it again.
func TestOpUpdate_UnackedTerminalSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCache(db)
	powerOp, deployOp := uuid.NewString(), uuid.NewString()
	c.RecordOpUpdate(powerOp, string(model.OpCompleted), nil)
	c.MarkOpFailed(deployOp, "partitioning", "sgdisk: device busy")

	if rows := mustListOutbox(t, db); len(rows) != 2 || rows[0].Update == nil || rows[1].Update == nil {
		t.Fatalf("op updates not persisted when recorded: %+v", rows)
	}
	if sent := c.DrainOutbox(); len(sent) != 2 {
		t.Fatalf("drained %d entries, want both updates", len(sent))
	}

	// The process dies before global's answers arrive.
	closeLocalDB(t, db)
	db2, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLocalDB(t, db2) })
	c2 := NewCache(db2)
	got := c2.DrainOutbox()
	if len(got) != 2 {
		t.Fatalf("after restart drained %+v, want both unacknowledged updates", got)
	}
	if u := got[0].Update; u == nil || u.OperationID != powerOp || u.Status != string(model.OpCompleted) {
		t.Errorf("first resend %+v, want the power op's completion", got[0])
	}
	u := got[1].Update
	if u == nil || u.OperationID != deployOp || u.Status != string(model.OpFailed) || u.Phase != "partitioning" {
		t.Fatalf("second resend %+v, want the deploy's failure at partitioning", got[1])
	}
	if r, err := model.DecodeResult(u.Result); err != nil || r.Error != "sgdisk: device busy" {
		t.Errorf("failure result %s (%v), want the agent's error", u.Result, err)
	}
	for _, e := range got {
		c2.AckOutbox(e)
	}
	if rows := mustListOutbox(t, db2); len(rows) != 0 {
		t.Errorf("acknowledged updates still on disk: %+v", rows)
	}
}

// TestOutbox_FailedAckDeleteHoldsTheChain: global acknowledged a phase but
// its row could not be deleted. The entry must stay, ahead of the op's
// later updates: dropped from memory with its row still on disk, it would
// come back on the next reload or restart after them and move global's
// phase backwards. It is sent again after a backoff, and the next ack
// deletes it and releases the chain.
func TestOutbox_FailedAckDeleteHoldsTheChain(t *testing.T) {
	db := openTestDB(t)
	db.SetLogger(quietLog)
	c := NewCache(db)
	c.outbox.RejectBackoffBase = time.Hour
	c.outbox.RejectBackoffMax = time.Hour
	op := uuid.NewString()
	c.appendOpUpdate(model.OperationUpdate{OperationID: op, Phase: "power_cycle"})
	c.appendOpUpdate(model.OperationUpdate{OperationID: op, Phase: "pxe_booting"})
	first := drainOne(t, c)

	restore := hideOutboxTable(t, db)
	c.AckOutbox(first)
	restore()
	if got := c.DrainOutbox(); len(got) != 0 {
		t.Fatalf("drained %+v after a failed delete, want nothing: power_cycle is backed off and pxe_booting waits behind it", got)
	}
	if rows := mustListOutbox(t, db); len(rows) != 2 {
		t.Fatalf("disk holds %d rows, want both updates", len(rows))
	}

	// The backoff elapses: power_cycle is sent again, and this ack deletes it.
	c.outbox.mu.Lock()
	for i := range c.outbox.entries {
		c.outbox.entries[i].NextAttempt = time.Time{}
	}
	c.outbox.mu.Unlock()
	again := drainOne(t, c)
	if again.Update == nil || again.Update.Phase != "power_cycle" {
		t.Fatalf("resent %+v, want power_cycle", again)
	}
	c.AckOutbox(again)
	next := drainOne(t, c)
	if next.Update == nil || next.Update.Phase != "pxe_booting" {
		t.Fatalf("after the delete succeeded drained %+v, want pxe_booting", next)
	}
	if rows := mustListOutbox(t, db); len(rows) != 1 || rows[0].RowID != next.RowID {
		t.Fatalf("disk holds %+v, want only pxe_booting's row", rows)
	}
}

// TestOutbox_OpChainKeepsRecordOrder: an op's updates and the observations
// that settle it reach global one at a time, in the order the worker
// recorded them. Here the agent reports the commission inventory and then
// fails: global must see the report first (the op completes) and the
// failure second (a no-op on a completed op), never the failure first,
// which would fail the op and refuse its inventory.
func TestOutbox_OpChainKeepsRecordOrder(t *testing.T) {
	c := NewCache(nil)
	machine, op := uuid.New(), uuid.New()
	c.UpdateOpPhase(op.String(), "reporting")
	c.recordCommissionReport(machine, op, []byte(`{"system":{}}`))
	c.MarkOpFailed(op.String(), "console_probe", "probe failed")
	c.RecordInventoryReport(machine, []byte(`{}`))

	var order []string
	for {
		batch := c.DrainOutbox()
		if len(batch) == 0 {
			break
		}
		if n := chainEntries(batch, op.String()); n > 1 {
			t.Fatalf("%d entries of the op's chain in flight at once: %+v", n, batch)
		}
		for _, e := range batch {
			order = append(order, describeEntry(e))
			c.AckOutbox(e)
		}
	}
	want := []string{"op_update phase=reporting", "inventory_import", "commission_report", "op_update failed"}
	if len(order) != len(want) {
		t.Fatalf("delivery order %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("delivery order %v, want %v", order, want)
		}
	}
}

// TestOutbox_DeployCompleteDoesNotHoldPhoneHome: deploy_complete names the
// deploy op but does not settle it, so while global keeps rejecting it
// (NetBox down while it stores the monitor credential) the phone_home that
// completes the deploy still goes out. The completion op update behind the
// phone_home waits for the phone_home's answer.
func TestOutbox_DeployCompleteDoesNotHoldPhoneHome(t *testing.T) {
	c := NewCache(nil)
	c.outbox.RejectBackoffBase = time.Hour
	c.outbox.RejectBackoffMax = time.Hour
	machine, op := uuid.New(), uuid.New()
	c.ApplySnapshot(snapMachine(machine, "52:54:00:e1:00:02", model.MachineStateDeploying))
	seedOpsResync(c, model.Operation{
		ID: op, MachineID: machine, Type: model.OpDeploy, Status: model.OpClaimed, Phase: "awaiting_phone_home",
	})
	c.recordDeployComplete(machine, op, []byte(`{"monitor":{"user":"monitor","password":"x"}}`))
	c.RequeueRejected(drainOne(t, c))

	c.RecordPhoneHome(machine)
	got := c.DrainOutbox()
	if len(got) != 1 || got[0].Obs.Event != "phone_home" || got[0].Obs.OperationID != op {
		t.Fatalf("drained %+v, want the phone_home naming the deploy op", got)
	}
	if more := c.DrainOutbox(); len(more) != 0 {
		t.Fatalf("the completion went out before the phone_home was answered: %+v", more)
	}
	c.AckOutbox(got[0])
	next := drainOne(t, c)
	if next.Update == nil || next.Update.Status != string(model.OpCompleted) {
		t.Fatalf("after the phone_home's ack drained %+v, want the completion", next)
	}
}

func chainEntries(batch []PendingOutbox, op string) int {
	n := 0
	for _, e := range batch {
		if (e.Update != nil && e.Update.OperationID == op) || (e.Update == nil && e.Obs.OperationID.String() == op) {
			n++
		}
	}
	return n
}

func describeEntry(e PendingOutbox) string {
	if u := e.Update; u != nil {
		if u.Status != "" {
			return "op_update " + u.Status
		}
		return "op_update phase=" + u.Phase
	}
	return e.Obs.Event
}
