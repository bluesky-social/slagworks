package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestHub_DHCPLeases_FansOutToAllWorkers pins the fan-out behavior
// PowerStatus deliberately does NOT have: DHCPLeases RPCs every
// connected worker for the slug (an HA cage's two workers each hold
// their own dynamic slice's leases), not just one.
func TestHub_DHCPLeases_FansOutToAllWorkers(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop1", "worker-1")
	auth.register("tok-2", "pop1", "worker-2")
	h.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn1 := dialHub(t, srv, "tok-1")
	if conn1 == nil {
		t.Fatal("dial worker-1 failed")
	}
	defer conn1.CloseNow()
	helloAndDrain(t, conn1, "worker-1")

	conn2 := dialHub(t, srv, "tok-2")
	if conn2 == nil {
		t.Fatal("dial worker-2 failed")
	}
	defer conn2.CloseNow()
	helloAndDrain(t, conn2, "worker-2")

	answer := func(conn *Conn, mac, ip string) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			env, err := conn.ReadMessage(ctx)
			if err != nil {
				return
			}
			if env.Type != TypeDHCPLeases {
				return
			}
			out, _ := json.Marshal(DHCPLeasesResponseMsg{
				Leases: []DHCPLeaseInfo{{MAC: mac, IP: ip, Kind: "dynamic", Pool: "10.99.1.0/24"}},
			})
			_ = conn.WriteTyped(ctx, TypeDHCPLeasesResp, env.RequestID, RPCResponseMsg{Data: out})
		}()
	}
	answer(conn1, "aa:bb:cc:00:00:01", "10.99.1.100")
	answer(conn2, "aa:bb:cc:00:00:02", "10.99.1.200")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	results := h.DHCPLeases(ctx, "pop1")
	if len(results) != 2 {
		t.Fatalf("got %d worker results, want 2: %+v", len(results), results)
	}

	byWorker := make(map[string]WorkerDHCPLeases, len(results))
	for _, r := range results {
		byWorker[r.WorkerID] = r
	}
	w1, ok := byWorker["worker-1"]
	if !ok || w1.Err != "" || len(w1.Leases) != 1 || w1.Leases[0].MAC != "aa:bb:cc:00:00:01" {
		t.Errorf("worker-1 result = %+v", w1)
	}
	w2, ok := byWorker["worker-2"]
	if !ok || w2.Err != "" || len(w2.Leases) != 1 || w2.Leases[0].MAC != "aa:bb:cc:00:00:02" {
		t.Errorf("worker-2 result = %+v", w2)
	}

	h.rpcMu.Lock()
	n := len(h.rpcPending)
	h.rpcMu.Unlock()
	if n != 0 {
		t.Errorf("rpcPending leaked %d entries", n)
	}
}

// TestHub_DHCPLeases_OneWorkerErrorsOtherStillAnswers pins the
// per-worker isolation DHCPLeases exists to provide: one worker
// timing out or erroring must not prevent the other's answer from
// being returned.
func TestHub_DHCPLeases_OneWorkerErrorsOtherStillAnswers(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop1", "worker-1")
	auth.register("tok-2", "pop1", "worker-2")
	h.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn1 := dialHub(t, srv, "tok-1")
	if conn1 == nil {
		t.Fatal("dial worker-1 failed")
	}
	defer conn1.CloseNow()
	helloAndDrain(t, conn1, "worker-1")

	conn2 := dialHub(t, srv, "tok-2")
	if conn2 == nil {
		t.Fatal("dial worker-2 failed")
	}
	defer conn2.CloseNow()
	helloAndDrain(t, conn2, "worker-2")

	// worker-1 replies with a hard error; worker-2 never reads its
	// request at all (simulating a wedged/pre-RPC worker) so its RPC
	// times out via ctx.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		env, err := conn1.ReadMessage(ctx)
		if err != nil {
			return
		}
		_ = conn1.WriteTyped(ctx, TypeDHCPLeasesResp, env.RequestID,
			RPCResponseMsg{Error: "site worker has no DHCP handler configured"})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	results := h.DHCPLeases(ctx, "pop1")
	if len(results) != 2 {
		t.Fatalf("got %d worker results, want 2: %+v", len(results), results)
	}

	byWorker := make(map[string]WorkerDHCPLeases, len(results))
	for _, r := range results {
		byWorker[r.WorkerID] = r
	}
	w1 := byWorker["worker-1"]
	if !strings.Contains(w1.Err, "no DHCP handler") {
		t.Errorf("worker-1 err = %q, want it to carry the worker's error", w1.Err)
	}
	w2 := byWorker["worker-2"]
	if w2.Err == "" {
		t.Errorf("worker-2 (never replied) must report a timeout error, got none: %+v", w2)
	}

	h.rpcMu.Lock()
	n := len(h.rpcPending)
	h.rpcMu.Unlock()
	if n != 0 {
		t.Errorf("rpcPending leaked %d entries", n)
	}
}

// TestHub_DHCPLeases_NoWorkers pins the empty-cage outcome: nil, not
// an error — the API layer turns this into a 200 with an empty
// workers array (an empty HA cage is an answerable question).
func TestHub_DHCPLeases_NoWorkers(t *testing.T) {
	h, _, _ := newTestHub(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := h.DHCPLeases(ctx, "pop9")
	if results != nil {
		t.Errorf("expected nil for a slug with no connected workers, got %+v", results)
	}
}
