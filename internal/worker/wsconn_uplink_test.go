package worker

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// uplinkGlobal stands in for global behind a real ws.Hub: one site, one
// worker token. It counts hellos (a reconnect shows up as a second hello)
// and records the observations and op_updates it receives.
type uplinkGlobal struct {
	mu      sync.Mutex
	hellos  int
	obs     []model.Observation
	updates []ws.OpUpdateMsg
	// changed is signalled after every hello, observation and op_update,
	// so await blocks on what global received and not on the clock.
	changed chan struct{}
}

// uplinkWait bounds every wait in these tests. It is how long a test that is
// going to fail takes to say so, not how long a passing one takes: a pass
// returns as soon as what it waits for happens. A 16 MiB message crosses the
// race-instrumented encoder, WebSocket and decoder in a few seconds on a
// loaded machine, so the bound is far above that.
const uplinkWait = 2 * time.Minute

func (g *uplinkGlobal) signal() {
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

// await blocks until cond holds over what global has received, and reports
// whether it did within uplinkWait.
func (g *uplinkGlobal) await(cond func(hellos, obs, updates int) bool) bool {
	deadline := time.NewTimer(uplinkWait)
	defer deadline.Stop()
	for {
		if cond(g.counts()) {
			return true
		}
		select {
		case <-g.changed:
		case <-deadline.C:
			return cond(g.counts())
		}
	}
}

func (g *uplinkGlobal) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if hash != store.HashWorkerToken("tok-1") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "site-a", WorkerID: "worker-1"}, nil
}

func (g *uplinkGlobal) TouchSiteWorker(slug, workerID, _ string, serial *string) (*model.SiteWorker, error) {
	if serial != nil { // a hello; heartbeats pass nil
		g.mu.Lock()
		g.hellos++
		g.mu.Unlock()
		g.signal()
	}
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (g *uplinkGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (g *uplinkGlobal) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "site-a"}}, nil
}

func (g *uplinkGlobal) HandleObservation(_ context.Context, o model.Observation) error {
	g.mu.Lock()
	g.obs = append(g.obs, o)
	g.mu.Unlock()
	g.signal()
	return nil
}

func (g *uplinkGlobal) HandleOpUpdate(_ context.Context, m ws.OpUpdateMsg) error {
	g.mu.Lock()
	g.updates = append(g.updates, m)
	g.mu.Unlock()
	g.signal()
	return nil
}

func (g *uplinkGlobal) counts() (hellos, obs, updates int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hellos, len(g.obs), len(g.updates)
}

// connectUplink starts a real hub and a WSConn connected to it, and waits
// for the worker's first sync.
func connectUplink(t *testing.T) (*uplinkGlobal, *Cache, *LocalDB) {
	t.Helper()
	g := &uplinkGlobal{changed: make(chan struct{}, 1)}
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g, ObsHandler: g, OpUpdateHandler: g})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	localDB := openTestDB(t)
	cache := NewCache(localDB)
	wsc := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wsc.Run(ctx)
	}()
	// Cleanups run last-registered first: stop the worker before the
	// test DB directory, the hub and the server go away.
	t.Cleanup(func() {
		cancel()
		<-done
	})

	sctx, scancel := context.WithTimeout(ctx, uplinkWait)
	defer scancel()
	if err := wsc.WaitForSync(sctx); err != nil {
		t.Fatalf("worker never synced: %v", err)
	}
	return g, cache, localDB
}

// eventually polls cond for up to uplinkWait and reports whether it became
// true. It is for worker-side state with nothing to block on (outbox rows on
// disk); what global received is waited for with uplinkGlobal.await.
func eventually(cond func() bool) bool {
	for deadline := time.Now().Add(uplinkWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// jsonBlob returns a valid JSON object of a little over n bytes.
func jsonBlob(n int) []byte {
	b := append([]byte(`{"nics":[],"blob":"`), bytes.Repeat([]byte("x"), n)...)
	return append(b, `"}`...)
}

// An inventory report inside what httpboot's /api/v1/inventory admits
// (10 MiB) used to exceed the hub's 1 MiB read limit: the hub closed the
// connection, the row was never acked, and the reconnect replayed it first,
// forever. It must be delivered and acked, the connection must stay up,
// and an op_update queued after it must reach global.
func TestWSConn_LargeInventoryReportDoesNotWedgeUplink(t *testing.T) {
	g, cache, localDB := connectUplink(t)

	body := jsonBlob(2 << 20)
	cache.RecordInventoryReport(uuid.Nil, body)
	delivered := g.await(func(_, obs, _ int) bool { return obs == 1 })
	acked := eventually(func() bool { return len(mustListOutbox(t, localDB)) == 0 })

	// An op the worker is driving reports completion.
	cache.RecordOpUpdate(uuid.NewString(), "completed", nil)
	updated := g.await(func(_, _, upd int) bool { return upd == 1 })

	hellos, obs, upd := g.counts()
	if !delivered {
		t.Errorf("a %d-byte inventory observation was never delivered to global (observations received: %d)", len(body), obs)
	}
	if !acked {
		t.Errorf("the observation's outbox row was never acked")
	}
	if !updated {
		t.Errorf("the op_update queued after it was never delivered (op_updates received: %d): the op it reports would time out on global", upd)
	}
	if hellos != 1 {
		t.Errorf("the worker connected %d times: the observation is replayed from the durable outbox on every reconnect", hellos)
	}
}

// An observation the worker can never send (larger than any message the
// hub reads, or not encodable at all) must be dropped from the outbox with
// an error instead of being sent or requeued, so it cannot block the
// observations and op_updates queued behind it.
func TestWSConn_UndeliverableObservationIsDropped(t *testing.T) {
	cases := map[string][]byte{
		"larger than ws.MaxMessageBytes": jsonBlob(ws.MaxMessageBytes),
		"not valid JSON":                 []byte(`{"nics":`),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			g, cache, localDB := connectUplink(t)

			cache.RecordInventoryReport(uuid.Nil, data)
			cache.RecordInventoryReport(uuid.Nil, jsonBlob(16))
			cache.RecordOpUpdate(uuid.NewString(), "completed", nil)

			if !g.await(func(_, obs, upd int) bool { return obs == 1 && upd == 1 }) {
				_, obs, upd := g.counts()
				t.Fatalf("what was queued behind the undeliverable observation never arrived: observations %d (want 1), op_updates %d (want 1)", obs, upd)
			}
			if !eventually(func() bool { return len(mustListOutbox(t, localDB)) == 0 && cache.OutboxDepth() == 0 }) {
				t.Errorf("the undeliverable observation is still queued: %d rows on disk, %d in memory",
					len(mustListOutbox(t, localDB)), cache.OutboxDepth())
			}
			if hellos, _, _ := g.counts(); hellos != 1 {
				t.Errorf("the worker connected %d times, want 1", hellos)
			}
		})
	}
}

// The drain's size check and the hub's read limit agree to the byte: an
// inventory observation whose message is exactly ws.MaxMessageBytes is
// delivered, and one a byte longer is dropped without a reconnect.
func TestWSConn_ObservationSizeCheckMatchesHubLimit(t *testing.T) {
	// The message around an inventory report with an empty blob, encoded
	// as RecordInventoryReport and drainOnce build it (the request ID is
	// always a 36-character UUID).
	empty, err := observationFrame(uuid.NewString(), model.Observation{
		MachineID: uuid.Nil,
		Event:     "inventory_import",
		Timestamp: time.Now(),
		Data:      jsonBlob(0),
	})
	if err != nil {
		t.Fatal(err)
	}
	atLimit := ws.MaxMessageBytes - len(empty)

	for _, tc := range []struct {
		name      string
		blob      int
		delivered bool
	}{
		{"exactly ws.MaxMessageBytes", atLimit, true},
		{"one byte over", atLimit + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, cache, _ := connectUplink(t)

			cache.RecordInventoryReport(uuid.Nil, jsonBlob(tc.blob))
			cache.RecordOpUpdate(uuid.NewString(), "completed", nil)

			// The hub handles op_updates in its read loop and
			// observations on their own goroutine, so wait for both.
			want := 0
			if tc.delivered {
				want = 1
			}
			if !g.await(func(_, obs, upd int) bool { return obs == want && upd == 1 }) {
				_, obs, upd := g.counts()
				t.Fatalf("observations delivered: %d (want %d), op_updates: %d (want 1)", obs, want, upd)
			}
			if hellos, _, _ := g.counts(); hellos != 1 {
				t.Errorf("the worker connected %d times, want 1", hellos)
			}
		})
	}
}

// A body at the intake cap that /api/v1/inventory and /api/v1/report
// enforce (ws.MaxObservationDataBytes) still encodes within
// ws.MaxMessageBytes, so a body the worker answers 200 for is one the
// drain can send.
func TestObservationFrame_IntakeCapFitsMessageLimit(t *testing.T) {
	data := append([]byte(`"`), bytes.Repeat([]byte("x"), ws.MaxObservationDataBytes-2)...)
	data = append(data, '"')
	frame, err := observationFrame(uuid.NewString(), model.Observation{
		MachineID:   uuid.New(),
		OperationID: uuid.New(),
		Event:       "bmc_reconcile_report", // the longest event name
		Timestamp:   time.Now(),
		Data:        data,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(frame) > ws.MaxMessageBytes {
		t.Fatalf("a %d-byte body encodes to a %d-byte message, over the %d-byte limit",
			len(data), len(frame), ws.MaxMessageBytes)
	}
}
