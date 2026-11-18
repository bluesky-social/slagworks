package ws

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Phase 3 (docs/plans/2026-07-13-dhcp-lease-visibility.md) coverage:
// the hub stamps Observation.SiteSlug/WorkerID from the authenticated
// connection identity — never from the worker's wire payload, which
// carries no such fields (ObservationMsg has none to forge).

// TestHub_Observation_StampsSiteSlugAndWorkerIDFromConn pins that the
// observation the ObsHandler receives carries the connecting worker's
// slug and worker_id, sourced from workerConn (auth-time identity),
// not anything the worker could have sent on the wire.
func TestHub_Observation_StampsSiteSlugAndWorkerIDFromConn(t *testing.T) {
	h, auth, _ := newTestHub(t)
	auth.register("tok-1", "pop3", "worker-9")
	obsHandler := &fakeObsHandler{}
	h.cfg.ObsHandler = obsHandler

	srv := httptest.NewServer(h)
	defer srv.Close()

	conn := dialHub(t, srv, "tok-1")
	if conn == nil {
		t.Fatal("dial failed")
	}
	defer conn.CloseNow()
	helloAndDrain(t, conn, "worker-9")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	reqID := "req-dhcp-lease-1"
	machineID := uuid.New()
	operationID := uuid.New()
	if err := conn.WriteTyped(ctx, TypeObservation, reqID, ObservationMsg{
		MachineID: machineID, OperationID: operationID,
		Event: "dhcp_lease", Data: []byte(`{"event":"allocate","mac":"aa:bb:cc:00:00:01","ip":"10.99.1.100"}`),
	}); err != nil {
		t.Fatal(err)
	}

	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != TypeRPCResponse || env.RequestID != reqID {
		t.Fatalf("got %+v, want rpc_response with reqID %q", env, reqID)
	}

	if obsHandler.count() != 1 {
		t.Fatalf("handler got %d obs, want 1", obsHandler.count())
	}
	obsHandler.mu.Lock()
	got := obsHandler.received[0]
	obsHandler.mu.Unlock()

	if got.SiteSlug != "pop3" {
		t.Errorf("SiteSlug = %q, want %q", got.SiteSlug, "pop3")
	}
	if got.WorkerID != "worker-9" {
		t.Errorf("WorkerID = %q, want %q", got.WorkerID, "worker-9")
	}
	if got.OperationID != operationID {
		t.Errorf("OperationID = %s, want %s", got.OperationID, operationID)
	}
}
