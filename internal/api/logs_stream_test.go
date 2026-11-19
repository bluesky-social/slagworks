package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// firstQueryGap wraps the stream's log store and runs before and after
// around the handler's first Query only. handleLogsStream queries from a
// single goroutine, so the counter needs no lock.
type firstQueryGap struct {
	LogStreamStore
	before func(ctx context.Context)
	after  func()
	calls  int
}

func (g *firstQueryGap) Query(ctx context.Context, f logstore.QueryFilter) ([]logstore.Result, error) {
	g.calls++
	if g.calls == 1 {
		g.before(ctx)
		defer g.after()
	}
	return g.LogStreamStore.Query(ctx, f)
}

// TestLogsStreamDeliversRowCommittedBehindHigherID pins that a subscriber's
// first query cannot step its cursor past a log_envelopes id whose
// transaction has not committed yet. The lifecycle emitters in
// internal/store/op_events.go insert inside the operation's transaction
// (logstore.InsertTx), so their id is allocated before commit; a worker
// batch (autocommit InsertBatch) can take the next id and commit first. If
// the subscriber's first query lands in that gap, it sees only the higher
// id, and the lower one must still be delivered once its transaction
// commits.
func TestLogsStreamDeliversRowCommittedBehindHigherID(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	ls := logstore.New(st.DB())
	machineID := uuid.New()
	envelope := func(src slogkit.Source, msg string) slogkit.Envelope {
		return slogkit.Envelope{
			TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: src,
			Msg: msg, MachineID: machineID,
		}
	}

	gap := &firstQueryGap{LogStreamStore: ls}
	var lifecycleTx *gorm.DB
	gap.before = func(ctx context.Context) {
		// A lifecycle emitter takes id N inside a transaction that stays
		// open across the subscriber's first query.
		lifecycleTx = st.DB().Begin()
		if err := logstore.InsertTx(lifecycleTx, envelope(slogkit.SourceGlobal, "operation completed")); err != nil {
			t.Errorf("InsertTx: %v", err)
		}
		// A worker batch takes id N+1 and commits immediately.
		if err := ls.InsertBatch(ctx, []slogkit.Envelope{envelope(slogkit.SourceSite, "worker line")}); err != nil {
			t.Errorf("InsertBatch: %v", err)
		}
	}
	gap.after = func() {
		if err := lifecycleTx.Commit().Error; err != nil {
			t.Errorf("commit lifecycle tx: %v", err)
		}
	}

	s := &Server{echo: echo.New(), logStore: gap}
	handlerDone := make(chan struct{})
	s.echo.GET("/api/v1/logs/stream", func(c *echo.Context) error {
		defer close(handlerDone)
		return s.handleLogsStream(c)
	})
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	conn, status := dialStream(t, srv, "/api/v1/logs/stream", "", nil)
	if conn == nil {
		t.Fatalf("dial logs stream: status %d, want 101 upgrade", status)
	}
	// srv.Close does not wait for the handler of a hijacked (WebSocket)
	// connection, and the handler queries the shared testpg handle, so the
	// test waits for it here: a handler still polling when a later test
	// registers gorm callbacks on that handle is a data race.
	defer func() {
		_ = conn.CloseNow()
		select {
		case <-handlerDone:
		case <-time.After(5 * time.Second):
			t.Errorf("handleLogsStream still running 5s after the client closed the connection; a gone subscriber must stop polling the log store")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sub, err := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", ws.LogStreamSubscribeMsg{MachineID: machineID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatal(err)
	}

	var got []ws.LogStreamEnvelopeMsg
	for len(got) < 2 {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			break
		}
		var env ws.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != ws.TypeLogStreamEnvelope {
			continue
		}
		var m ws.LogStreamEnvelopeMsg
		if err := json.Unmarshal(env.Data, &m); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}

	var msgs []string
	var ids []int64
	for _, m := range got {
		msgs = append(msgs, m.Envelope.Msg)
		ids = append(ids, m.ID)
	}
	if len(got) != 2 || msgs[0] != "operation completed" || msgs[1] != "worker line" || ids[0] >= ids[1] {
		t.Fatalf("stream delivered %q (ids %v); want [\"operation completed\" \"worker line\"] in id order", msgs, ids)
	}
}
