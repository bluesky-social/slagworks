package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

// emptyLogStore returns no rows; the tests below only look at the frame
// the server sends in reply to the subscribe message.
type emptyLogStore struct{}

func (emptyLogStore) Query(context.Context, logstore.QueryFilter) ([]logstore.Result, error) {
	return nil, nil
}

// TestLogsStreamMinLevel pins that a min_level outside debug|info|warn|error
// is refused with bad_filter before the stream starts. An unknown level used
// to be accepted and then ignored by the store, so `slagctl machine logs
// --level warning` streamed every level instead of reporting the typo.
// Case is not significant, so "WARN" is still accepted.
func TestLogsStreamMinLevel(t *testing.T) {
	s := &Server{echo: echo.New(), logStore: emptyLogStore{}}
	s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
	srv := httptest.NewServer(s.echo)
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		minLevel string
		wantType string
	}{
		{"warning", ws.TypeError},
		{"err", ws.TypeError},
		{" warn", ws.TypeError},
		{"WARN", ws.TypeSubscribed},
		{"", ws.TypeSubscribed},
	} {
		t.Run(strconv.Quote(tc.minLevel), func(t *testing.T) {
			env := subscribeLogs(t, srv, ws.LogStreamSubscribeMsg{MinLevel: tc.minLevel})
			if env.Type != tc.wantType {
				t.Fatalf("min_level=%q: first frame %q (%s), want %q", tc.minLevel, env.Type, env.Data, tc.wantType)
			}
			if env.Type != ws.TypeError {
				return
			}
			var e ws.ErrorMsg
			if err := json.Unmarshal(env.Data, &e); err != nil {
				t.Fatal(err)
			}
			if e.Code != "bad_filter" || !strings.Contains(e.Message, "debug, info, warn or error") {
				t.Fatalf("min_level=%q: error %+v, want bad_filter naming the valid levels", tc.minLevel, e)
			}
		})
	}
}

// subscribeLogs opens /api/v1/logs/stream, sends sub and returns the
// server's first frame.
func subscribeLogs(t *testing.T, srv *httptest.Server, sub ws.LogStreamSubscribeMsg) ws.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/logs/stream", nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow() //nolint:errcheck
	buf, err := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	var env ws.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

// logsStreamFixture is a real log store behind handleLogsStream.
type logsStreamFixture struct {
	st       *store.Store
	ls       *logstore.Store
	srv      *httptest.Server
	handlers sync.WaitGroup // running handleLogsStream calls
}

func newLogsStreamFixture(t *testing.T) *logsStreamFixture {
	t.Helper()
	return newLogsStreamFixtureAround(t, func(_ *logsStreamFixture, ls LogStreamStore) LogStreamStore { return ls })
}

// newLogsStreamFixtureAround is newLogsStreamFixture with the handler
// reading the log store through wrap's result, so a test can act between
// the handler's queries.
func newLogsStreamFixtureAround(t *testing.T, wrap func(*logsStreamFixture, LogStreamStore) LogStreamStore) *logsStreamFixture {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	ls := logstore.New(st.DB())
	f := &logsStreamFixture{st: st, ls: ls}
	s := &Server{echo: echo.New(), logStore: wrap(f, ls)}
	s.echo.GET("/api/v1/logs/stream", func(c *echo.Context) error {
		f.handlers.Add(1)
		defer f.handlers.Done()
		return s.handleLogsStream(c)
	})
	f.srv = httptest.NewServer(s.echo)
	t.Cleanup(f.srv.Close)
	// Runs after each subscribe's cleanup has closed its connection.
	// srv.Close does not wait for the handler of a hijacked (WebSocket)
	// connection, and the handler queries the shared testpg handle, so a
	// handler still polling when a later test registers gorm callbacks on
	// that handle is a data race (see
	// TestLogsStreamDeliversRowCommittedBehindHigherID).
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { f.handlers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("handleLogsStream still running 5s after the client closed the connection; a gone subscriber must stop polling the log store")
		}
	})
	return f
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

// An agent whose clock is behind global stamps ts in the past. A line
// global received after since must still stream on the live tail
// (`machine logs -f` shape).
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

// A reconnect sends last_seen_id with the same since. A slow-clocked line
// global received while the client was away is past the cursor and must
// stream (deploy TUI shape: machine OR op).
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

// received sets when global received the row with the given id. A row
// received more than liveTailWatermark ago is readable by the next query.
func (f *logsStreamFixture) received(t *testing.T, id int64, at time.Time) {
	t.Helper()
	if err := f.st.DB().Exec("UPDATE log_envelopes SET global_received_at = ? WHERE id = ?", at, id).Error; err != nil {
		t.Fatal(err)
	}
}

// A line from an emitter whose clock runs behind must stream whenever
// global receives it inside the subscription's window, including while the
// backfill is still paging. The stream used to bound since on the emitter's
// ts alone until its first empty page and only then admit a row by
// global_received_at as well. Until then each query filtered the
// slow-clocked line out and moved the cursor past its id with the next
// in-window row, so the later, wider queries could never return it: whether
// the line showed up depended on whether another emitter logged after it
// before the stream first ran dry. On a machine being deployed, something
// always does.
//
// The backfill here is two pages. Between the handler's first and second
// query, global receives a slow-clocked agent line and then an in-sync
// site line; both are past the watermark by the second query.
func TestLogsStream_SlowClockedLineReceivedDuringBackfillStreams(t *testing.T) {
	const backlog = 600 // more than one 500-row page
	machine := uuid.New()
	since := time.Now().Add(-10 * time.Minute)

	f := newLogsStreamFixtureAround(t, func(f *logsStreamFixture, ls LogStreamStore) LogStreamStore {
		return &firstQueryGap{
			LogStreamStore: ls,
			before:         func(context.Context) {},
			after: func() {
				// Read by the second query: received a second ago, which
				// is past the watermark and inside the window.
				at := time.Now().Add(-time.Second)
				f.received(t, f.insert(t, slogkit.Envelope{TS: time.Now().Add(-70 * time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: "slow-clock-during-backfill", MachineID: machine}), at)
				f.ancient(t, slogkit.Envelope{TS: since.Add(-time.Hour), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "ancient", MachineID: machine})
				f.received(t, f.insert(t, slogkit.Envelope{TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "in-sync-after", MachineID: machine}), at)
			},
		}
	})

	rows := make([]slogkit.Envelope, backlog)
	for i := range rows {
		rows[i] = slogkit.Envelope{TS: since.Add(time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "backlog-" + strconv.Itoa(i), MachineID: machine}
	}
	if err := f.ls.InsertBatch(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	if err := f.st.DB().Exec("UPDATE log_envelopes SET global_received_at = ? WHERE machine_id = ?", since.Add(time.Minute), machine).Error; err != nil {
		t.Fatal(err)
	}

	next := f.subscribe(t, ws.LogStreamSubscribeMsg{MachineID: machine.String(), SinceTimestamp: since.UTC().Format(time.RFC3339Nano)})
	for i := range backlog {
		if msg, ok := next(5 * time.Second); !ok || msg != "backlog-"+strconv.Itoa(i) {
			t.Fatalf("backfill row %d: got %q ok=%v, want backlog-%d", i, msg, ok, i)
		}
	}
	if msg, ok := next(5 * time.Second); !ok || msg != "slow-clock-during-backfill" {
		t.Fatalf("after the backlog: got %q ok=%v, want slow-clock-during-backfill (received inside the window while the backfill was paging, ts an hour behind); "+
			"the stream moved its cursor past the line without delivering it", msg, ok)
	}
	if msg, ok := next(5 * time.Second); !ok || msg != "in-sync-after" {
		t.Fatalf("after the slow-clocked line: got %q ok=%v, want in-sync-after (ancient must never stream)", msg, ok)
	}
	if msg, ok := next(time.Second); ok {
		t.Fatalf("stream delivered %q after its last row; ancient must never stream and nothing may repeat", msg)
	}
}

// The same line already stored when the client subscribes: global received
// it inside the window, between two in-window lines. The backfill must
// deliver it in id order instead of stepping over it.
func TestLogsStream_BackfillDeliversSlowClockedLineReceivedInWindow(t *testing.T) {
	f := newLogsStreamFixture(t)
	machine := uuid.New()
	since := time.Now().Add(-10 * time.Minute)
	at := time.Now().Add(-time.Minute)
	f.received(t, f.insert(t, slogkit.Envelope{TS: since.Add(time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "in-window-1", MachineID: machine}), at)
	f.received(t, f.insert(t, slogkit.Envelope{TS: time.Now().Add(-70 * time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: "slow-clock-stored", MachineID: machine}), at)
	f.ancient(t, slogkit.Envelope{TS: since.Add(-time.Hour), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "ancient", MachineID: machine})
	f.received(t, f.insert(t, slogkit.Envelope{TS: since.Add(2 * time.Minute), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "in-window-2", MachineID: machine}), at)

	next := f.subscribe(t, ws.LogStreamSubscribeMsg{MachineID: machine.String(), SinceTimestamp: since.UTC().Format(time.RFC3339Nano)})
	for _, want := range []string{"in-window-1", "slow-clock-stored", "in-window-2"} {
		if msg, ok := next(5 * time.Second); !ok || msg != want {
			t.Fatalf("backfill: got %q ok=%v, want %s", msg, ok, want)
		}
	}
	if msg, ok := next(time.Second); ok {
		t.Fatalf("stream delivered %q after its last row; ancient must never stream", msg)
	}
}
