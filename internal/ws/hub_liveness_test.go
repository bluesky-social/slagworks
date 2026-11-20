package ws

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestHub_HeartbeatsAnIdleWorkerAndDropsASilentOne: a connection whose
// path to the other end dies without a FIN or RST (a load balancer or NAT
// losing its flow state, a wedged proxy) looks open to both ends forever,
// so each end has to notice that the other has gone quiet. Global has to
// send something on an idle site, or the worker cannot tell an idle link
// from a dead one, and global has to drop a worker that stopped sending,
// or it keeps routing RPCs to a connection nobody reads.
//
// The test worker keeps its TCP connection open and keeps reading
// throughout; only what it sends changes.
func TestHub_HeartbeatsAnIdleWorkerAndDropsASilentOne(t *testing.T) {
	const (
		interval = 50 * time.Millisecond
		idle     = 500 * time.Millisecond
	)
	auth := newFakeAuth()
	auth.register("tok-1", "site-a", "w1")
	h := NewHub(HubConfig{
		Auth: auth, Rebuilder: &fakeRebuilder{}, RebuildTimeout: 2 * time.Second,
		HeartbeatInterval: interval, IdleTimeout: idle,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(h.Close)

	c := dialHub(t, srv, "tok-1")
	if c == nil {
		t.Fatal("dial failed")
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "w1", Proto: OpStreamProto}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var fromGlobal atomic.Int64
	go func() {
		for {
			env, err := c.ReadMessage(ctx)
			if err != nil {
				return
			}
			if env.Type == TypeHeartbeat {
				fromGlobal.Add(1)
			}
		}
	}()
	waitFor := func(d time.Duration, cond func() bool) bool {
		for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return cond()
	}
	if !waitFor(5*time.Second, func() bool { return h.IsWorkerConnected("site-a", "w1") }) {
		t.Fatal("worker not registered after hello")
	}

	// An idle site: the worker sends only its heartbeats, for four idle
	// windows. Global keeps it and heartbeats back.
	window := 4 * idle
	for end := time.Now().Add(window); time.Now().Before(end); time.Sleep(interval) {
		if err := c.WriteTyped(ctx, TypeHeartbeat, "", HeartbeatMsg{WorkerID: "w1"}); err != nil {
			t.Fatalf("worker heartbeat write: %v", err)
		}
		if !h.IsWorkerConnected("site-a", "w1") {
			t.Fatalf("global dropped a worker that heartbeats every %s (idle timeout %s)", interval, idle)
		}
	}
	if n := fromGlobal.Load(); n < 10 {
		t.Errorf("global sent %d heartbeats to an idle worker in %s; want one every %s", n, window, interval)
	}

	// The worker falls silent; its connection stays open.
	silentAt := time.Now()
	if !waitFor(10*idle, func() bool { return !h.IsWorkerConnected("site-a", "w1") }) {
		t.Fatalf("global still counts the worker connected %s after it fell silent (idle timeout %s)",
			time.Since(silentAt).Round(time.Millisecond), idle)
	}
}
