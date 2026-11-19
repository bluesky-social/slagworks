package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// TestRestartedWorker_ResendsPersistedObservationOnConnect: a worker
// process that stops with an unacked observation in worker.db (global
// unreachable, crash, host reboot, auto-upgrade restart) must send that
// observation on its first connection after the restart, even when
// nothing else is ever appended to the outbox. The drain loop is
// wake-driven, so the rehydrated row needs a wake of its own; without
// one it sat in memory until an unrelated observation happened to wake
// the loop, which on a quiet site can outlive the op's stuck timeout.
func TestRestartedWorker_ResendsPersistedObservationOnConnect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	machineID := uuid.New()

	// First process: a commission report is queued and persisted while
	// global is unreachable, then the process stops before any ack.
	db1, err := OpenLocalDB(path)
	if err != nil {
		t.Fatalf("open worker.db: %v", err)
	}
	NewCache(db1).appendObservation(model.Observation{
		MachineID: machineID,
		Event:     "commission_report",
		Timestamp: time.Now(),
		Data:      []byte(`{}`),
	})
	closeLocalDB(t, db1)

	// Global comes up.
	const token = "worker-token"
	sum := sha256.Sum256([]byte(token))
	obs := &recordingObsHandler{}
	hub := ws.NewHub(ws.HubConfig{
		Auth:       restartTestAuth{tokenHash: hex.EncodeToString(sum[:])},
		Rebuilder:  restartTestRebuilder{},
		ObsHandler: obs,
	})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	defer hub.Close()

	// Second process: same worker.db, connect, append nothing.
	db2, err := OpenLocalDB(path)
	if err != nil {
		t.Fatalf("reopen worker.db: %v", err)
	}
	w := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: token, WorkerID: "worker-a"}, NewCache(db2), db2, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
		closeLocalDB(t, db2)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(obs.received()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if !w.Connected() {
		t.Fatal("restarted worker never connected to global")
	}
	got := obs.received()
	if len(got) == 0 {
		t.Fatal("global received no observations within 5s of the restarted worker connecting; " +
			"the persisted commission report is stranded until some other observation is appended")
	}
	if got[0].MachineID != machineID || got[0].Event != "commission_report" {
		t.Fatalf("global received %s for machine %s, want the persisted commission_report for %s",
			got[0].Event, got[0].MachineID, machineID)
	}
}

// restartTestAuth accepts exactly one worker token.
type restartTestAuth struct{ tokenHash string }

func (a restartTestAuth) GetSiteWorkerByTokenHash(h string) (*model.SiteWorker, error) {
	if h != a.tokenHash {
		return nil, errors.New("unknown token")
	}
	return &model.SiteWorker{ID: uuid.New(), SiteSlug: "site-a", WorkerID: "worker-a"}, nil
}

func (restartTestAuth) TouchSiteWorker(slug, workerID, fqdn string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}, nil
}

func (restartTestAuth) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

// restartTestRebuilder answers every hello with an empty snapshot.
type restartTestRebuilder struct{}

func (restartTestRebuilder) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: slug}}, nil
}

// recordingObsHandler acks every observation and records it.
type recordingObsHandler struct {
	mu  sync.Mutex
	got []model.Observation
}

func (r *recordingObsHandler) HandleObservation(_ context.Context, obs model.Observation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, obs)
	return nil
}

func (r *recordingObsHandler) received() []model.Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]model.Observation(nil), r.got...)
}
