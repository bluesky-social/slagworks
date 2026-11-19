package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
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
