package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
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
	s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	conn, status := dialStream(t, srv, "/api/v1/logs/stream", "", nil)
	if conn == nil {
		t.Fatalf("dial logs stream: status %d, want 101 upgrade", status)
	}
	defer conn.CloseNow() //nolint:errcheck
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

// logsStreamFixture is a real log store behind handleLogsStream.
type logsStreamFixture struct {
	st  *store.Store
	ls  *logstore.Store
	srv *httptest.Server
}

func newLogsStreamFixture(t *testing.T) *logsStreamFixture {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	ls := logstore.New(st.DB())
	s := &Server{echo: echo.New(), logStore: ls}
	s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
	srv := httptest.NewServer(s.echo)
	t.Cleanup(srv.Close)
	return &logsStreamFixture{st: st, ls: ls, srv: srv}
}

// insert stores e as global would on receipt (global_received_at = now())
// and returns its row id. Each test uses a distinct msg per row.
func (f *logsStreamFixture) insert(t *testing.T, e slogkit.Envelope) int64 {
	t.Helper()
	if err := f.ls.InsertBatch(context.Background(), []slogkit.Envelope{e}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := f.st.DB().Raw("SELECT id FROM log_envelopes WHERE msg = ?", e.Msg).Scan(&id).Error; err != nil {
		t.Fatal(err)
	}
	return id
}

// ancient stores e as old history: global received it at its ts, so both
// clocks place it before any since the tests use. No read may deliver it.
func (f *logsStreamFixture) ancient(t *testing.T, e slogkit.Envelope) {
	t.Helper()
	id := f.insert(t, e)
	if err := f.st.DB().Exec("UPDATE log_envelopes SET global_received_at = ? WHERE id = ?", e.TS, id).Error; err != nil {
		t.Fatal(err)
	}
}

// subscribe opens the stream and returns a reader for the next envelope's
// msg; ok is false when none arrives within d.
func (f *logsStreamFixture) subscribe(t *testing.T, sub ws.LogStreamSubscribeMsg) func(d time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.srv.URL, "http")+"/api/v1/logs/stream", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	raw, err := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	return func(d time.Duration) (string, bool) {
		rctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		for {
			_, raw, err := conn.Read(rctx)
			if err != nil {
				return "", false
			}
			var env ws.Envelope
			if json.Unmarshal(raw, &env) != nil || env.Type != ws.TypeLogStreamEnvelope {
				continue
			}
			var m ws.LogStreamEnvelopeMsg
			if err := json.Unmarshal(env.Data, &m); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			return m.Envelope.Msg, true
		}
	}
}

// An agent whose clock is behind global stamps ts in the past. since bounds
// only the ts-ordered backfill; once the stream is past its cursor, a line
// global received after since must still stream (`machine logs -f` shape).
func TestLogsStream_LiveTailDeliversLineFromSlowClockedEmitter(t *testing.T) {
	f := newLogsStreamFixture(t)
	machine := uuid.New()
	since := time.Now().Add(-10 * time.Minute)
	f.insert(t, slogkit.Envelope{TS: since.Add(time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "in-window", MachineID: machine})
	f.ancient(t, slogkit.Envelope{TS: since.Add(-time.Hour), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "ancient", MachineID: machine})

	next := f.subscribe(t, ws.LogStreamSubscribeMsg{MachineID: machine.String(), SinceTimestamp: since.UTC().Format(time.RFC3339Nano)})
	if msg, ok := next(5 * time.Second); !ok || msg != "in-window" {
		t.Fatalf("backfill: got %q ok=%v, want in-window", msg, ok)
	}
	f.insert(t, slogkit.Envelope{TS: time.Now().Add(-70 * time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: "slow-clock-live", MachineID: machine})
	if msg, ok := next(5 * time.Second); !ok || msg != "slow-clock-live" {
		t.Fatalf("live tail: got %q ok=%v, want slow-clock-live (received after since, ts an hour behind; ancient must never stream)", msg, ok)
	}
}

// A reconnect sends last_seen_id with the same since. The resume continues
// the live tail past the cursor, so a slow-clocked line global received
// while the client was away must stream (deploy TUI shape: machine OR op).
func TestLogsStream_ResumeDeliversLineFromSlowClockedEmitter(t *testing.T) {
	f := newLogsStreamFixture(t)
	machine, op := uuid.New(), uuid.New()
	since := time.Now().Add(-10 * time.Minute)
	seen := f.insert(t, slogkit.Envelope{TS: since.Add(time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceGlobal, Msg: "seen", MachineID: machine, OpID: op})
	f.ancient(t, slogkit.Envelope{TS: since.Add(-time.Hour), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "ancient", MachineID: machine})
	f.insert(t, slogkit.Envelope{TS: time.Now().Add(-70 * time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: "slow-clock-missed", MachineID: machine, OpID: op})

	next := f.subscribe(t, ws.LogStreamSubscribeMsg{
		MachineID: machine.String(), OpID: op.String(),
		SinceTimestamp: since.UTC().Format(time.RFC3339Nano), LastSeenID: seen,
	})
	if msg, ok := next(5 * time.Second); !ok || msg != "slow-clock-missed" {
		t.Fatalf("resume: got %q ok=%v, want slow-clock-missed (past last_seen_id, received after since; ancient must never stream)", msg, ok)
	}
}
