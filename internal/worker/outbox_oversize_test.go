package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Tests for observations too large for the hub to read. The hub reads
// worker frames with a ws.MaxWorkerFrameBytes limit; a longer frame fails
// its read and closes the connection without an answer. Because the
// outbox row is never acked, every reconnect used to replay it first, so
// one oversized row cut every later observation and op update off from
// global for good, and the row survived worker restarts.

type oversizeAuth struct{ hash string }

func (a oversizeAuth) GetSiteWorkerByTokenHash(h string) (*model.SiteWorker, error) {
	if h != a.hash {
		return nil, errors.New("unknown token")
	}
	return &model.SiteWorker{ID: uuid.New(), SiteSlug: "site-a", WorkerID: "worker-a"}, nil
}

func (oversizeAuth) TouchSiteWorker(slug, workerID, fqdn string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}, nil
}

func (oversizeAuth) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

type oversizeRebuilder struct{}

func (oversizeRebuilder) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: slug}}, nil
}

// oversizeGlobal records what reaches global's observation and op_update
// handlers.
type oversizeGlobal struct {
	mu      sync.Mutex
	events  []string
	updates []ws.OpUpdateMsg
}

func (g *oversizeGlobal) HandleObservation(_ context.Context, obs model.Observation) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, obs.Event)
	return nil
}

func (g *oversizeGlobal) HandleOpUpdate(_ context.Context, msg ws.OpUpdateMsg) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.updates = append(g.updates, msg)
	return nil
}

func (g *oversizeGlobal) received() (events []string, updates int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.events), len(g.updates)
}

// startOversizeSite connects a worker WSConn, backed by a SQLite outbox,
// to a real ws.Hub and waits for the handshake to finish.
func startOversizeSite(t *testing.T) (*Cache, *LocalDB, *oversizeGlobal) {
	t.Helper()
	token := "worker-token"
	sum := sha256.Sum256([]byte(token))
	g := &oversizeGlobal{}
	hub := ws.NewHub(ws.HubConfig{
		Auth:            oversizeAuth{hash: hex.EncodeToString(sum[:])},
		Rebuilder:       oversizeRebuilder{},
		ObsHandler:      g,
		OpUpdateHandler: g,
	})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	db, err := OpenLocalDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	c := NewCache(db)
	w := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: token, WorkerID: "worker-a"}, c, db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = w.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !w.Connected() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !w.Connected() {
		t.Fatal("worker never connected")
	}
	return c, db, g
}

// waitOversize polls cond for up to 5s and reports whether it held.
func waitOversize(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestOutbox_OversizedObservationDoesNotWedgeSite: /api/v1/inventory
// queues its body verbatim as an inventory_import observation. A 2 MiB
// body, twice what the hub reads, must be dropped once instead of
// blocking the phone-home and deploy-completion op_update queued behind
// it, and its SQLite row must be gone so a restart does not replay it.
func TestOutbox_OversizedObservationDoesNotWedgeSite(t *testing.T) {
	c, db, g := startOversizeSite(t)

	body, _ := json.Marshal(map[string]any{
		"nics":    []map[string]string{{"mac": "aa:bb:cc:dd:ee:ff"}},
		"padding": strings.Repeat("x", 2<<20),
	})
	c.RecordInventoryReport(uuid.Nil, body)
	c.appendObservation(model.Observation{MachineID: uuid.New(), Event: "phone_home", Timestamp: time.Now()})
	c.appendOpUpdate(model.OperationUpdate{OperationID: uuid.NewString(), Status: string(model.OpCompleted)})

	delivered := waitOversize(func() bool {
		events, updates := g.received()
		return slices.Contains(events, "phone_home") && updates > 0
	})
	if !delivered {
		events, updates := g.received()
		t.Fatalf("after 5s global has events=%v op_updates=%d: the %d-byte inventory_import row "+
			"(hub read limit %d) blocks every later observation and op update",
			events, updates, len(body), ws.MaxWorkerFrameBytes)
	}
	if !waitOversize(func() bool { return len(db.ListPendingOutbox()) == 0 }) {
		t.Fatalf("SQLite outbox still holds %d rows; the oversized row would replay after a restart",
			len(db.ListPendingOutbox()))
	}
}

// TestOutbox_MaxObservationDataFitsFrame: a body at the intake cap that
// /api/v1/inventory and /api/v1/report enforce (ws.MaxObservationDataBytes)
// still encodes within ws.MaxWorkerFrameBytes, so a body the worker answers
// 200 for is one the drain can send.
func TestOutbox_MaxObservationDataFitsFrame(t *testing.T) {
	obs := model.Observation{
		MachineID:   uuid.New(),
		OperationID: uuid.New(),
		Event:       "bmc_reconcile_report", // the longest event name
		Timestamp:   time.Now(),
		Data:        json.RawMessage(`"` + strings.Repeat("x", ws.MaxObservationDataBytes-2) + `"`),
	}
	frame, err := observationFrame(uuid.NewString(), obs)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(frame) > ws.MaxWorkerFrameBytes {
		t.Fatalf("a %d-byte body encodes to a %d-byte frame, over the %d-byte limit",
			ws.MaxObservationDataBytes, len(frame), ws.MaxWorkerFrameBytes)
	}
}

// TestOutbox_ObservationFrameLimitMatchesHub pins the worker's drop
// threshold to the hub's read limit: a frame of exactly
// ws.MaxWorkerFrameBytes reaches global, and one byte more is dropped on
// the worker without costing the connection, so the observation queued
// behind it still arrives.
func TestOutbox_ObservationFrameLimitMatchesHub(t *testing.T) {
	c, db, g := startOversizeSite(t)

	// sized returns an observation whose encoded frame is n bytes. The
	// request_id drainOnce assigns is a UUID like the one used here, so
	// the length carries over.
	sized := func(event string, n int) model.Observation {
		obs := model.Observation{MachineID: uuid.New(), Event: event, Timestamp: time.Now(), Data: json.RawMessage(`""`)}
		frame, err := observationFrame(uuid.NewString(), obs)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		obs.Data = json.RawMessage(`"` + strings.Repeat("x", n-len(frame)) + `"`)
		return obs
	}
	c.appendObservation(sized("at_limit", ws.MaxWorkerFrameBytes))
	c.appendObservation(sized("over_limit", ws.MaxWorkerFrameBytes+1))
	c.appendObservation(model.Observation{MachineID: uuid.New(), Event: "marker", Timestamp: time.Now()})

	delivered := waitOversize(func() bool {
		events, _ := g.received()
		return slices.Contains(events, "at_limit") && slices.Contains(events, "marker")
	})
	events, _ := g.received()
	if !delivered {
		t.Fatalf("after 5s global has events=%v, want at_limit and marker", events)
	}
	if slices.Contains(events, "over_limit") {
		t.Fatalf("global received over_limit: the worker sent a %d-byte frame, over ws.MaxWorkerFrameBytes",
			ws.MaxWorkerFrameBytes+1)
	}
	if !waitOversize(func() bool { return len(db.ListPendingOutbox()) == 0 }) {
		t.Fatalf("SQLite outbox still holds %d rows after delivery", len(db.ListPendingOutbox()))
	}
}
