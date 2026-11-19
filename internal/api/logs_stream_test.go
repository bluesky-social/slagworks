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
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/ws"
)

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
