package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/model"
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

	prevURL, prevClient := apiURL, apiClient
	apiURL = srv.URL
	apiClient = slclient.New(slclient.Config{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Bearer: func(context.Context) (string, error) { return "stream-test-token", nil }})
	t.Cleanup(func() { apiURL, apiClient = prevURL, prevClient })
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

// TestStreams_RefusalCarriesGlobalsReason: global refuses a stream upgrade
// with a status and a body that says why (an expired token, a group the
// operator lacks, a parameter it cannot parse). Every stream slagctl opens
// returns that reason, not only the status code.
func TestStreams_RefusalCarriesGlobalsReason(t *testing.T) {
	const reason = `{"message":"not authorized: token carries none of the required groups"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, reason, http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	prevURL, prevClient := apiURL, apiClient
	apiURL = srv.URL
	apiClient = slclient.New(slclient.Config{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Bearer: func(context.Context) (string, error) { return "stream-test-token", nil }})
	t.Cleanup(func() { apiURL, apiClient = prevURL, prevClient })

	streams := []struct {
		name string
		open func(ctx context.Context) error
	}{
		{"machine logs", func(ctx context.Context) error {
			return runLogsStream(ctx, ws.LogStreamSubscribeMsg{}, func(slogkit.Envelope, int64) error { return nil }, false)
		}},
		{"machine logs --sol", func(ctx context.Context) error {
			return runConsoleStream(ctx, uuid.NewString(), "", true, false, true)
		}},
		{"watch console", func(ctx context.Context) error {
			return consoleSource(uuid.NewString())(ctx, func([]byte, bool) {}, func() {})
		}},
		{"watch logs", func(ctx context.Context) error {
			var lastSeen int64
			return streamLogEnvelopes(ctx, uuid.NewString(), uuid.NewString(), time.Time{}, &lastSeen, func(int64, slogkit.Envelope) {})
		}},
	}
	for _, s := range streams {
		t.Run(s.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := s.open(ctx)
			if err == nil {
				t.Fatal("the stream opened against a server that answers 403")
			}
			for _, want := range []string{"403", reason} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestOperationWatchPlain_SendsBearerHeader(t *testing.T) {
	seen := stubStreamServer(t, readOne)
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = watchPlain(ctx, model.Operation{ID: uuid.New(), MachineID: uuid.New()}, devNull, time.Hour)
	}()
	assertBearerNotInURL(t, seen, "/api/v1/logs/stream")
	cancel()
	<-done // watchPlain reads apiURL; let it stop before cleanup restores it
}
