package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// The hub registers a worker's connection before it sends the hello reply
// (workerConn.serve), and on a cold slug that reply waits on a snapshot
// rebuild for up to RebuildTimeout. The worker accepts only snapshot or
// no_change as its first message and drops the connection on anything else
// (worker.WSConn.connectAndRun), so the hub-originated RPCs must not write
// to a connection in that window.

// preHelloGate is a Rebuilder that blocks until released, holding a
// cold-slug hello in the window between register and the hello reply.
type preHelloGate struct {
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func newPreHelloGate() *preHelloGate {
	return &preHelloGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *preHelloGate) RebuildAndWait(ctx context.Context, slug string) (model.SiteSnapshot, error) {
	g.enteredOnce.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return model.SiteSnapshot{}, ctx.Err()
	}
	return model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: slug}}, nil
}

// helloIntoGate dials the hub as workerID and sends its hello, returning
// once the hub has registered the connection and parked its hello in the
// gated rebuild: the connection is live and its hello reply is pending.
func helloIntoGate(ctx context.Context, t *testing.T, srv *httptest.Server, gate *preHelloGate, token, workerID string) *Conn {
	t.Helper()
	conn := dialHub(t, srv, token)
	if conn == nil {
		t.Fatalf("dial %s failed", workerID)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: workerID, Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("hub never started the cold-hello rebuild")
	}
	return conn
}

// isRPCRequest reports whether msgType is a hub-originated RPC request.
func isRPCRequest(msgType string) bool {
	return msgType == TypePowerStatus || msgType == TypeBootStatus || msgType == TypeDHCPLeases
}

// TestHub_RPCSkipsConnAwaitingHelloReply: an operator read issued while the
// site's only worker is waiting for its hello reply finds no worker (503 at
// the API) instead of writing the request as the worker's first message.
func TestHub_RPCSkipsConnAwaitingHelloReply(t *testing.T) {
	wantNoWorker := func(err error) error {
		if !errors.Is(err, ErrNoWorkerForSite) {
			return fmt.Errorf("got %v, want ErrNoWorkerForSite", err)
		}
		return nil
	}
	cases := []struct {
		name string
		// call runs the RPC and returns an error unless it reported that
		// the site has no worker to ask.
		call func(ctx context.Context, h *Hub) error
	}{
		{"power_status", func(ctx context.Context, h *Hub) error {
			_, err := h.PowerStatus(ctx, "pop1", uuid.New(), json.RawMessage(`{}`), nil)
			return wantNoWorker(err)
		}},
		{"boot_status", func(ctx context.Context, h *Hub) error {
			_, err := h.BootStatus(ctx, "pop1", uuid.New(), json.RawMessage(`{}`))
			return wantNoWorker(err)
		}},
		{"dhcp_leases", func(ctx context.Context, h *Hub) error {
			if res := h.DHCPLeases(ctx, "pop1"); res != nil {
				return fmt.Errorf("got %+v, want nil (no worker has its hello reply)", res)
			}
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := newFakeAuth()
			auth.register("tok-1", "pop1", "worker-1")
			gate := newPreHelloGate()
			h := NewHub(HubConfig{Auth: auth, Rebuilder: gate, RebuildTimeout: 10 * time.Second})
			srv := httptest.NewServer(h)
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn := helloIntoGate(ctx, t, srv, gate, "tok-1", "worker-1")

			rpcCtx, rpcCancel := context.WithTimeout(ctx, 300*time.Millisecond)
			rpcErr := tc.call(rpcCtx, h)
			rpcCancel()
			close(gate.release)

			first, err := conn.ReadMessage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if first.Type != TypeSnapshot && first.Type != TypeNoChange {
				t.Fatalf("first message after hello was %q; the worker accepts only snapshot or no_change "+
					"there and drops the connection on anything else", first.Type)
			}
			if rpcErr != nil {
				t.Error(rpcErr)
			}
		})
	}
}

// TestHub_RPCUsesPartnerWithHelloReply: in an HA pair where one worker has
// its hello reply and the other is still waiting for it, the RPCs go to the
// ready worker and the waiting one receives nothing but snapshots and its
// op_resync.
func TestHub_RPCUsesPartnerWithHelloReply(t *testing.T) {
	auth := newFakeAuth()
	auth.register("tok-1", "pop1", "worker-1")
	auth.register("tok-2", "pop1", "worker-2")
	gate := newPreHelloGate()
	h := NewHub(HubConfig{Auth: auth, Rebuilder: gate, RebuildTimeout: 10 * time.Second})
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// worker-1 parks in the cold rebuild. A Push then warms the slug (and
	// fans a snapshot out to worker-1, a valid first message), so
	// worker-2's hello is answered at once from the cache.
	waiting := helloIntoGate(ctx, t, srv, gate, "tok-1", "worker-1")
	h.Push("pop1", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop1"}})
	ready := dialHub(t, srv, "tok-2")
	if ready == nil {
		t.Fatal("dial worker-2 failed")
	}
	defer ready.CloseNow()
	helloAndDrain(t, ready, "worker-2")

	// worker-2 answers every RPC it receives.
	go func() {
		for {
			env, err := ready.ReadMessage(ctx)
			if err != nil {
				return
			}
			var respType string
			var data []byte
			switch env.Type {
			case TypePowerStatus:
				respType = TypePowerStatusResp
				data, _ = json.Marshal(PowerStatusResponseMsg{State: "on", Transport: "redfish"})
			case TypeDHCPLeases:
				respType = TypeDHCPLeasesResp
				data, _ = json.Marshal(DHCPLeasesResponseMsg{Leases: []DHCPLeaseInfo{{MAC: "aa:bb:cc:00:00:02"}}})
			default:
				continue
			}
			_ = ready.WriteTyped(ctx, respType, env.RequestID, RPCResponseMsg{Data: data})
		}
	}()

	powerCtx, powerCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	resp, powerErr := h.PowerStatus(powerCtx, "pop1", uuid.New(), json.RawMessage(`{}`), nil)
	powerCancel()
	leasesCtx, leasesCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	leases := h.DHCPLeases(leasesCtx, "pop1")
	leasesCancel()
	close(gate.release)

	if powerErr != nil || resp.State != "on" {
		t.Errorf("PowerStatus = %+v, %v; want worker-2's answer", resp, powerErr)
	}
	if len(leases) != 1 || leases[0].WorkerID != "worker-2" || leases[0].Err != "" {
		t.Errorf("DHCPLeases = %+v; want exactly worker-2's answer", leases)
	}

	for {
		env, err := waiting.ReadMessage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if isRPCRequest(env.Type) {
			t.Fatalf("worker-1 received %q before completing its hello", env.Type)
		}
		if env.Type == TypeOpResync {
			return
		}
	}
}
