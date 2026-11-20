package ws

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// orderProbe is an ObservationHandler that records when each dhcp_lease
// observation's handling starts and ends. The first one takes a while, as a
// handler does when it has to wait for a database connection.
type orderProbe struct {
	mu     sync.Mutex
	events []string
	n      int
}

func (p *orderProbe) HandleObservation(_ context.Context, obs model.Observation) error {
	if obs.Event != "dhcp_lease" {
		return nil
	}
	var ev struct {
		Event string `json:"event"`
	}
	_ = json.Unmarshal(obs.Data, &ev)
	p.mu.Lock()
	p.n++
	first := p.n == 1
	p.events = append(p.events, "start "+ev.Event)
	p.mu.Unlock()
	if first {
		time.Sleep(300 * time.Millisecond)
	}
	p.mu.Lock()
	p.events = append(p.events, "end "+ev.Event)
	p.mu.Unlock()
	return nil
}

func (p *orderProbe) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

// TestHub_AppliesAWorkersDHCPLeaseObservationsInTheOrderItSentThem: the
// lease ledger folds allocate, renew and delete events, and the fold is not
// commutative. A delete that is applied before the allocate it follows
// finds no open row and does nothing; the allocate then opens a row for a
// lease the worker no longer holds, and nothing ever closes it. The worker
// sends its outbox in order on one connection (and after a global restart
// sends minutes of it in one burst), so the hub must apply one worker's
// dhcp_lease observations one at a time, in arrival order. It starts a
// goroutine per observation instead, and two for the same lease race.
func TestHub_AppliesAWorkersDHCPLeaseObservationsInTheOrderItSentThem(t *testing.T) {
	probe := &orderProbe{}
	auth := newFakeAuth()
	auth.register("tok-1", "pop1", "worker-1")
	hub := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{snap: model.SiteSnapshot{SeqNum: 1}}, ObsHandler: probe})
	defer hub.Close()
	srv := httptest.NewServer(hub)
	defer srv.Close()
	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", Proto: OpStreamProto}); err != nil {
		t.Fatal(err)
	}

	// One lease, granted and then released: two outbox rows, sent in order.
	lease := func(event string) ObservationMsg {
		data, _ := json.Marshal(map[string]string{
			"event": event, "mac": "02:00:00:00:00:01", "ip": "192.0.2.20",
			"kind": "dynamic", "pool": "ephemeral", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
		return ObservationMsg{Event: "dhcp_lease", Timestamp: time.Now().UTC().Format(time.RFC3339), Data: data}
	}
	for _, event := range []string{"allocate", "delete"} {
		if err := conn.WriteTyped(ctx, TypeObservation, uuid.NewString(), lease(event)); err != nil {
			t.Fatalf("send %s: %v", event, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for len(probe.snapshot()) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("the hub handled only %v within 5s", probe.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := probe.snapshot()
	want := []string{"start allocate", "end allocate", "start delete", "end delete"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the hub handled one worker's dhcp_lease observations as %v, want %v: the delete was applied while the allocate before it was still in flight, which leaves the lease open in the ledger for good", got, want)
		}
	}
}
