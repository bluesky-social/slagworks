package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// upgradeRequest is what a stub server saw on a WebSocket upgrade.
type upgradeRequest struct {
	path, rawQuery, auth string
}

// stubStreamServer accepts one WebSocket upgrade per connection, records
// the request, runs serve on the connection and closes it. It points the
// slagctl globals at itself with a known token for the test's duration.
func stubStreamServer(t *testing.T, serve func(ctx context.Context, c *websocket.Conn)) <-chan upgradeRequest {
	t.Helper()
	seen := make(chan upgradeRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- upgradeRequest{path: r.URL.Path, rawQuery: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		serve(r.Context(), c)
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)

	prevURL, prevToken := apiURL, apiToken
	apiURL, apiToken = srv.URL, "stream-test-token"
	t.Cleanup(func() { apiURL, apiToken = prevURL, prevToken })
	return seen
}

// assertBearerNotInURL checks the upgrade carried the operator token in
// the Authorization header and nowhere in the URL (URLs reach access logs).
func assertBearerNotInURL(t *testing.T, seen <-chan upgradeRequest, wantPath string) {
	t.Helper()
	select {
	case got := <-seen:
		if got.path != wantPath {
			t.Errorf("path = %q, want %q", got.path, wantPath)
		}
		if got.auth != "Bearer stream-test-token" {
			t.Errorf("Authorization = %q, want %q", got.auth, "Bearer stream-test-token")
		}
		for _, bad := range []string{"token", "stream-test-token"} {
			if strings.Contains(got.rawQuery, bad) {
				t.Errorf("query %q carries %q; the token must not be in the URL", got.rawQuery, bad)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stub server saw no upgrade")
	}
}

func readOne(ctx context.Context, c *websocket.Conn) { _, _, _ = c.Read(ctx) }

func TestStreamDialOptions_BearerHeader(t *testing.T) {
	opts := streamDialOptions("abc")
	if got := opts.HTTPHeader.Get("Authorization"); got != "Bearer abc" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer abc")
	}
}

func TestMachineLogsStream_SendsBearerHeader(t *testing.T) {
	seen := stubStreamServer(t, readOne)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runLogsStream(ctx, ws.LogStreamSubscribeMsg{}, func(slogkit.Envelope, int64) error { return nil }, false)
	assertBearerNotInURL(t, seen, "/api/v1/logs/stream")
}

func TestMachineLogsSOL_SendsBearerHeader(t *testing.T) {
	// A non-asciicast first frame makes runConsoleStream return before it
	// touches the terminal.
	seen := stubStreamServer(t, func(ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"code":"x","message":"y"}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = runConsoleStream(ctx, uuid.NewString(), "", true, false, true)
	assertBearerNotInURL(t, seen, "/api/v1/console/stream")
}

func TestWatchTUIStreams_SendBearerHeader(t *testing.T) {
	t.Run("console", func(t *testing.T) {
		seen := stubStreamServer(t, func(context.Context, *websocket.Conn) {})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = consoleSource(uuid.NewString())(ctx, func([]byte, bool) {}, func() {})
		assertBearerNotInURL(t, seen, "/api/v1/console/stream")
	})
	t.Run("logs", func(t *testing.T) {
		seen := stubStreamServer(t, readOne)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var lastSeen int64
		_ = streamLogEnvelopes(ctx, uuid.NewString(), uuid.NewString(), time.Time{}, &lastSeen, func(int64, slogkit.Envelope) {})
		assertBearerNotInURL(t, seen, "/api/v1/logs/stream")
	})
}

func TestOperationWatchPlain_SendsBearerHeader(t *testing.T) {
	seen := stubStreamServer(t, readOne)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	es, err := NewEventStream(ctx, apiURL, apiToken, uuid.NewString(), uuid.NewString(), 0)
	if err != nil {
		t.Fatalf("NewEventStream: %v", err)
	}
	_ = es.Close()
	assertBearerNotInURL(t, seen, "/api/v1/ws/events")
}
