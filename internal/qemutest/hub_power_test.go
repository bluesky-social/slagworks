//go:build integration

package qemutest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func TestFakeOpClaimerCASReclaimAndTerminalPersistence(t *testing.T) {
	op := NewOperation(uuid.New(), model.OpPowerOn)
	fh := &FakeHub{
		activeOps: map[string][]model.Operation{"pop-test": {op}},
		opVersion: map[string]int{op.ID.String(): 1},
		opUpdates: make(map[string][]ws.OpUpdateMsg),
	}
	claimer := fakeOpClaimer{fh: fh}

	type result struct {
		worker string
		op     *model.Operation
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, worker := range []string{"worker-a", "worker-b"} {
		go func() {
			<-start
			claimed, err := claimer.ClaimOperation(op.ID, worker)
			results <- result{worker: worker, op: claimed, err: err}
		}()
	}
	close(start)

	var winner string
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatalf("claim by %s: %v", got.worker, got.err)
		}
		if got.op != nil {
			if winner != "" {
				t.Fatalf("both workers won the claim: %s and %s", winner, got.worker)
			}
			winner = got.worker
		}
	}
	if winner == "" {
		t.Fatal("neither worker won the pending-op claim")
	}

	// The winning worker may retry an unphased claim after losing the WS
	// response; another worker must continue to lose the CAS.
	reclaimed, err := claimer.ClaimOperation(op.ID, winner)
	if err != nil || reclaimed == nil || reclaimed.ClaimedBy != winner {
		t.Fatalf("idempotent reclaim = (%+v, %v), want claim held by %s", reclaimed, err, winner)
	}
	loser := "worker-a"
	if winner == loser {
		loser = "worker-b"
	}
	if claimed, err := claimer.ClaimOperation(op.ID, loser); err != nil || claimed != nil {
		t.Fatalf("losing reclaim = (%+v, %v), want (nil, nil)", claimed, err)
	}

	fh.recordOpUpdate(ws.OpUpdateMsg{
		OperationID: op.ID.String(),
		Status:      string(model.OpCompleted),
	})
	stored, err := claimer.GetOperationIfExists(op.ID)
	if err != nil || stored == nil || stored.Status != model.OpCompleted {
		t.Fatalf("stored terminal op = (%+v, %v), want completed", stored, err)
	}
	active, err := (fakeActiveOps{fh: fh}).ListActiveOperations()
	if err != nil {
		t.Fatalf("list active operations: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("terminal op resurrected in active resync: %+v", active)
	}
}

func TestWaitForOpUpdateCancellationRemovesWaiter(t *testing.T) {
	fh := &FakeHub{opUpdates: make(map[string][]ws.OpUpdateMsg)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fh.WaitForOpUpdate(ctx, uuid.NewString(), string(model.OpCompleted)); err == nil {
		t.Fatal("WaitForOpUpdate with canceled context returned nil error")
	}
	fh.mu.Lock()
	defer fh.mu.Unlock()
	if len(fh.opUpdateWaiters) != 0 {
		t.Fatalf("canceled waiter leaked: %d waiters remain", len(fh.opUpdateWaiters))
	}
}
