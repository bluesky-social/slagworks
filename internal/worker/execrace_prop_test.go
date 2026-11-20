package worker

// Concurrency companion to the single-threaded executor property harness:
// the real Executor.Run loop (ticker + OpWake) on its own goroutine, the
// op-event stream applied from a second goroutine (what the WS read loop
// does), the outbox drained from a third (the WS drain loop). Meant for
// `go test -race`:
//
//	go test -race ./internal/worker/ -run TestProp_ExecutorRunConcurrent -count=1
//
// Properties: no data race, no deadlock (the run finishes), every op is won
// at most once and no power action happens without a win; once the stream
// goes quiet every op the hub still holds pending is driven.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

type raceHub struct {
	mu      sync.Mutex
	cache   *Cache
	ops     map[uuid.UUID]*model.Operation
	won     map[uuid.UUID]int
	claims  int
	actions int // power actions issued by the fake controller
}

func (h *raceHub) ClaimOp(_ context.Context, id uuid.UUID) (*model.Operation, error) {
	h.mu.Lock()
	h.claims++
	op := h.ops[id]
	if h.claims%7 == 0 {
		h.mu.Unlock()
		return nil, errors.New("claim_op write: connection reset") // not committed
	}
	if op == nil || op.Status != model.OpPending {
		h.mu.Unlock()
		return nil, nil
	}
	op.Status, op.ClaimedBy = model.OpClaimed, "worker-1"
	op.Version++
	h.won[id]++
	cp := *op
	h.mu.Unlock()
	h.cache.ApplyOpDelta(cp, "")
	return &cp, nil
}

type raceCtrl struct {
	mockPowerController
	hub *raceHub
}

func (c *raceCtrl) act() error {
	c.hub.mu.Lock()
	c.hub.actions++
	c.hub.mu.Unlock()
	return nil
}

func (c *raceCtrl) PowerOn(context.Context) error    { return c.act() }
func (c *raceCtrl) PowerOff(context.Context) error   { return c.act() }
func (c *raceCtrl) PowerCycle(context.Context) error { return c.act() }

func TestProp_ExecutorRunConcurrent(t *testing.T) {
	for seed := int64(1); seed <= 16; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) { raceRound(t, seed) })
	}
}

func raceRound(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	cache := NewCache(nil)
	machines := make([]model.MachineSnapshot, 4)
	for i := range machines {
		machines[i] = model.MachineSnapshot{ID: uuid.New(), BMCIP: fmt.Sprintf("10.0.0.%d", 10+i), State: model.MachineStateCommissioned, PXEEligible: true}
	}
	seq := int64(1)
	cache.ApplySnapshot(&model.SiteSnapshot{SeqNum: seq, Machines: machines})
	hub := &raceHub{cache: cache, ops: map[uuid.UUID]*model.Operation{}, won: map[uuid.UUID]int{}}
	cache.ApplyOpResync(nil, nil)

	exec := NewExecutor(ExecutorConfig{
		Cache: cache,
		Factory: func(context.Context, *model.MachineSnapshot, model.BMCRuntime, *model.PDURuntime, *model.AMTRuntime) (power.Controller, error) {
			return &raceCtrl{hub: hub}, nil
		},
		Claimer:  hub,
		WorkerID: "worker-1",
		Interval: time.Millisecond,
	})
	exec.SetLogger(testDiscardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = exec.Run(ctx) }()

	// The WS drain loop: terminal op_updates reach the hub, which answers
	// with op_removed.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-cache.Wake:
			}
			_, updates := cache.DrainOutbox()
			for _, u := range updates {
				id, err := uuid.Parse(u.OperationID)
				if err != nil || (u.Status != string(model.OpCompleted) && u.Status != string(model.OpFailed)) {
					continue
				}
				hub.mu.Lock()
				op := hub.ops[id]
				var removed *model.Operation
				if op != nil && op.Status != model.OpCompleted && op.Status != model.OpFailed {
					op.Status = model.OperationStatus(u.Status)
					op.Version++
					cp := *op
					removed = &cp
				}
				hub.mu.Unlock()
				if removed != nil {
					cache.ApplyOpRemoved(removed.ID.String(), removed.MachineID.String(), removed.Version, "")
				}
			}
		}
	}()

	// The WS read loop: a random op-event stream.
	types := []model.OperationType{model.OpPowerOn, model.OpPowerOff, model.OpPowerCycle, model.OpDeploy}
	activeOn := func(m uuid.UUID) *model.Operation {
		for _, op := range hub.ops {
			if op.MachineID == m && op.Status != model.OpCompleted && op.Status != model.OpFailed {
				return op
			}
		}
		return nil
	}
	for i := 0; i < 1500; i++ {
		m := machines[rng.Intn(len(machines))].ID
		switch rng.Intn(10) {
		case 0, 1, 2, 3, 4: // create (one active op per machine)
			hub.mu.Lock()
			var created *model.Operation
			if activeOn(m) == nil {
				op := &model.Operation{ID: uuid.New(), MachineID: m, Type: types[rng.Intn(len(types))], Status: model.OpPending}
				hub.ops[op.ID] = op
				cp := *op
				created = &cp
			}
			hub.mu.Unlock()
			if created != nil {
				cache.ApplyOpDelta(*created, "")
			}
		case 5: // operator abort
			hub.mu.Lock()
			var removed *model.Operation
			if op := activeOn(m); op != nil {
				op.Status = model.OpFailed
				op.Version++
				cp := *op
				removed = &cp
			}
			hub.mu.Unlock()
			if removed != nil {
				cache.ApplyOpRemoved(removed.ID.String(), removed.MachineID.String(), removed.Version, "")
			}
		case 6: // reconnect: full resync of the active set
			hub.mu.Lock()
			var ops []model.Operation
			for _, op := range hub.ops {
				if op.Status != model.OpCompleted && op.Status != model.OpFailed {
					ops = append(ops, *op)
				}
			}
			hub.mu.Unlock()
			cache.ApplyOpResync(ops, nil)
		case 7: // identity snapshot push
			seq++
			cache.ApplySnapshot(&model.SiteSnapshot{SeqNum: seq, Machines: machines})
		case 8: // boot-path reads racing the executor
			_ = cache.PendingPowerOps()
			_ = cache.ActiveOpCount()
			_ = cache.MachineByID(m)
		case 9:
			time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
		}
	}

	// Quiescence: a final resync, then every op still pending on the hub
	// must be claimed and every claimed power op reported.
	deadline := time.Now().Add(30 * time.Second)
	for {
		hub.mu.Lock()
		var ops []model.Operation
		open := 0
		for _, op := range hub.ops {
			switch {
			case op.Status == model.OpPending:
				open++
				ops = append(ops, *op)
			case op.Status == model.OpClaimed:
				ops = append(ops, *op)
				if op.Type != model.OpDeploy {
					open++ // a claimed power op must end in a terminal report
				}
			}
		}
		hub.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seed %d: %d op(s) still open on the hub 30s after the stream went quiet", seed, open)
		}
		cache.ApplyOpResync(ops, nil)
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	stopped := make(chan struct{})
	go func() {
		wg.Wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatalf("seed %d: the executor or the outbox drain did not stop within 30s of cancellation", seed)
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()
	wins := 0
	for id, n := range hub.won {
		wins += n
		if n > 1 {
			t.Errorf("seed %d: op %s was won %d times", seed, id, n)
		}
	}
	if hub.actions > wins {
		t.Errorf("seed %d: %d power actions for %d won claims", seed, hub.actions, wins)
	}
	if wins == 0 {
		t.Errorf("seed %d: no op was ever won; the round exercised nothing", seed)
	}
}
