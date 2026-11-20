package worker

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// hideOutboxTable makes every read of the outbox table fail, as a damaged
// table does, and returns the function that puts it back.
func hideOutboxTable(t *testing.T, db *LocalDB) (restore func()) {
	t.Helper()
	if err := db.db.Exec("ALTER TABLE outbox RENAME TO outbox_hidden").Error; err != nil {
		t.Fatalf("hide outbox table: %v", err)
	}
	return func() {
		t.Helper()
		if err := db.db.Exec("ALTER TABLE outbox_hidden RENAME TO outbox").Error; err != nil {
			t.Fatalf("restore outbox table: %v", err)
		}
	}
}

func appendTestObservation(t *testing.T, db *LocalDB, event string) uint64 {
	t.Helper()
	id, err := db.AppendOutbox(model.Observation{MachineID: uuid.New(), Event: event, Timestamp: time.Now()})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	return id
}

// TestLocalDB_ListPendingOutbox_ReportsReadError is the regression for a
// table read whose error was dropped: an outbox table SQLite cannot read
// came back as "no rows", indistinguishable from an empty outbox, with
// nothing logged.
func TestLocalDB_ListPendingOutbox_ReportsReadError(t *testing.T) {
	db := openTestDB(t)
	db.SetLogger(quietLog)
	appendTestObservation(t, db, "phone_home")
	restore := hideOutboxTable(t, db)

	pending, err := db.ListPendingOutbox()
	if err == nil {
		t.Fatalf("an unreadable outbox table read as %d pending rows and no error", len(pending))
	}

	restore()
	pending, err = db.ListPendingOutbox()
	if err != nil || len(pending) != 1 {
		t.Fatalf("after the table is readable again: %d rows, err %v; want the 1 persisted row", len(pending), err)
	}
}

// TestOutbox_ReloadReportsReadErrorAndRecovers: a failed read must not pass
// for an empty outbox at startup or on reconnect, and the next reload picks
// the rows up once the table reads again.
func TestOutbox_ReloadReportsReadErrorAndRecovers(t *testing.T) {
	db := openTestDB(t)
	db.SetLogger(quietLog)
	appendTestObservation(t, db, "phone_home")
	restore := hideOutboxTable(t, db)

	c := NewCache(db)
	if d := c.OutboxDepth(); d != 0 {
		t.Fatalf("outbox depth %d from an unreadable table", d)
	}
	if err := c.ReloadOutboxFromDisk(); err == nil {
		t.Fatal("reload of an unreadable outbox table reported success: the caller treats the outbox as empty")
	}

	restore()
	if err := c.ReloadOutboxFromDisk(); err != nil {
		t.Fatalf("reload after the table is readable again: %v", err)
	}
	if d := c.OutboxDepth(); d != 1 {
		t.Fatalf("outbox depth %d after a successful reload, want the 1 persisted row", d)
	}
}

// TestWSConn_UnreadableOutboxFailsTheConnectionAndIsRetried: a worker that
// cannot read its durable outbox when it connects must not carry on as if
// nothing were queued. The row here stands for an observation sent on the
// previous connection and never acked (a phone_home, say): it is on disk
// only. The connection attempt fails, the reconnect loop retries it, and
// the row is delivered once the table reads again.
func TestWSConn_UnreadableOutboxFailsTheConnectionAndIsRetried(t *testing.T) {
	const wait = 2 * time.Minute // bounds a failing run; a passing one returns on the event
	poll := func(cond func() bool) bool {
		for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return cond()
	}

	g := &uplinkGlobal{}
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g, ObsHandler: g, OpUpdateHandler: g})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	localDB := openTestDB(t)
	localDB.SetLogger(quietLog)
	cache := NewCache(localDB)
	// Persisted after the cache was built: on disk, not in memory.
	appendTestObservation(t, localDB, "phone_home")
	restore := hideOutboxTable(t, localDB)

	wsc := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wsc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// A second hello is the reconnect loop retrying the failed attempt.
	if !poll(func() bool { hellos, _, _ := g.counts(); return hellos >= 2 }) {
		hellos, obs, _ := g.counts()
		t.Fatalf("the worker stayed on a connection it could not reload its outbox for (hellos %d, observations %d)", hellos, obs)
	}
	if _, obs, _ := g.counts(); obs != 0 {
		t.Fatalf("%d observations delivered while the outbox table was unreadable", obs)
	}

	restore()
	if !poll(func() bool { _, obs, _ := g.counts(); return obs == 1 }) {
		_, obs, _ := g.counts()
		t.Fatalf("the persisted observation was not delivered after the table became readable (observations %d)", obs)
	}
	if !poll(func() bool { p, err := localDB.ListPendingOutbox(); return err == nil && len(p) == 0 }) {
		t.Fatal("the delivered observation's row was never acked")
	}
}

// TestWSConn_OpStreamDownRunsFromTheFirstFailedAttempt: the reload-failure
// loop above completes a hello on every attempt and never reads the op_resync
// that follows it, so the op set stays frozen for as long as the loop lasts.
// The cache must count that outage from the first failed attempt, keep that
// time across the retries, and end it only when a connection's op_resync is
// applied.
func TestWSConn_OpStreamDownRunsFromTheFirstFailedAttempt(t *testing.T) {
	const wait = 2 * time.Minute // bounds a failing run; a passing one returns on the event
	poll := func(cond func() bool) bool {
		for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if cond() {
				return true
			}
		}
		return cond()
	}

	g := &uplinkGlobal{}
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g, ObsHandler: g, OpUpdateHandler: g})
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	t.Cleanup(hub.Close)

	localDB := openTestDB(t)
	localDB.SetLogger(quietLog)
	cache := NewCache(localDB)
	restore := hideOutboxTable(t, localDB)

	wsc := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = wsc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	if !poll(func() bool { return !streamDownSince(cache).IsZero() }) {
		t.Fatal("the first failed connection attempt was not recorded as the op stream going down")
	}
	first := streamDownSince(cache)
	// A third hello means the second attempt has ended too.
	if !poll(func() bool { hellos, _, _ := g.counts(); return hellos >= 3 }) {
		hellos, _, _ := g.counts()
		t.Fatalf("the worker did not keep retrying (hellos %d)", hellos)
	}
	if got := streamDownSince(cache); !got.Equal(first) {
		t.Fatalf("a later failed attempt moved the start of the outage from %v to %v; the trust window would never run out", first, got)
	}

	restore()
	if !poll(func() bool { return streamDownSince(cache).IsZero() }) {
		t.Fatalf("the op stream still counts as down since %v after a connection that applied its op_resync", streamDownSince(cache))
	}
}
