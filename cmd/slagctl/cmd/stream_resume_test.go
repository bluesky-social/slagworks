package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/coder/websocket"
	"github.com/google/uuid"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
)

// streamTestServer runs handle for each request, numbering requests from 1,
// and points the slagctl globals at itself.
func streamTestServer(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *atomic.Int64 {
	t.Helper()
	var conns atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle(int(conns.Add(1)), w, r)
	}))
	t.Cleanup(srv.Close)
	prevURL, prevClient := apiURL, apiClient
	apiURL = srv.URL
	apiClient = slclient.New(slclient.Config{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Bearer: func(context.Context) (string, error) { return "stream-test-token", nil }})
	t.Cleanup(func() { apiURL, apiClient = prevURL, prevClient })
	return &conns
}

// droppingStreamServer serves one WebSocket per connection with serve(n, c),
// where n counts connections from 1, and points the slagctl globals at itself.
// Returning from serve drops the connection without a close frame. serve's ctx
// ends when the client goes away (the request context of a hijacked connection
// never does), so a serve that waits on it does not outlive the test.
func droppingStreamServer(t *testing.T, serve func(n int, ctx context.Context, c *websocket.Conn)) *atomic.Int64 {
	t.Helper()
	return streamTestServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		serve(n, c.CloseRead(r.Context()), c)
	})
}

var castHeader = []byte(`{"version":2,"width":80,"height":24}`)

// writeCastEvent sends one asciicast output event, as the server does.
func writeCastEvent(ctx context.Context, c *websocket.Conn, t float64, data string) {
	buf, _ := json.Marshal([3]any{t, "o", data})
	_ = c.Write(ctx, websocket.MessageText, buf)
}

// TestConsoleSource_ReconnectsAfterDrop pins the tui.ConsoleSource contract
// (cmd/slagctl/tui/run.go): a dropped console WebSocket is retried, not
// returned. Drops without a close frame are routine: a rollout of the proxy
// in front of the API, or a load balancer idle timeout on a quiet console.
func TestConsoleSource_ReconnectsAfterDrop(t *testing.T) {
	conns := droppingStreamServer(t, func(n int, ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageText, castHeader)
		if n == 1 {
			_ = c.Write(ctx, websocket.MessageText, []byte(`[0.1,"o","before-drop"]`))
			return // abnormal close: the deferred CloseNow sends no close frame
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`[0.2,"o","after-reconnect"]`))
		<-ctx.Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var got []string
	gotAfter := make(chan struct{})
	var once sync.Once
	returned := make(chan error, 1)
	go func() {
		returned <- consoleSource(uuid.NewString())(ctx, func(b []byte, _ bool) {
			mu.Lock()
			got = append(got, string(b))
			mu.Unlock()
			if strings.HasSuffix(string(b), "after-reconnect") {
				once.Do(func() { close(gotAfter) })
			}
		}, func() {}, func(bool) {})
	}()
	select {
	case <-gotAfter:
	case err := <-returned:
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("console source returned %v after the first connection dropped (connections=%d, bytes=%q); it must reconnect and keep streaming", err, conns.Load(), got)
	case <-ctx.Done():
		t.Fatalf("console source did not stream from a second connection within 10s (connections=%d)", conns.Load())
	}
}

// consoleRecorder runs a consoleSource and records what it reports.
type consoleRecorder struct {
	mu         sync.Mutex
	chunks     []string
	backfill   []bool
	reconnects []bool
	arrived    chan string
}

func recordConsole(t *testing.T) *consoleRecorder {
	t.Helper()
	r := &consoleRecorder{arrived: make(chan string, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consoleSource(uuid.NewString())(ctx,
			func(b []byte, fromBackfill bool) {
				r.mu.Lock()
				r.chunks = append(r.chunks, string(b))
				r.backfill = append(r.backfill, fromBackfill)
				r.mu.Unlock()
				r.arrived <- string(b)
			},
			func() {},
			func(reconnecting bool) {
				r.mu.Lock()
				r.reconnects = append(r.reconnects, reconnecting)
				r.mu.Unlock()
			})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// waitFor blocks until a chunk ending in suffix arrives.
func (r *consoleRecorder) waitFor(t *testing.T, suffix string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case b := <-r.arrived:
			if strings.HasSuffix(b, suffix) {
				return
			}
		case <-timeout:
			r.mu.Lock()
			defer r.mu.Unlock()
			t.Fatalf("no chunk ending in %q within 10s; got %q, reconnect reports %v", suffix, r.chunks, r.reconnects)
		}
	}
}

// TestConsoleSource_RedialResetsTerminalAndReportsReconnect: after a drop the
// source tells the pane it is reconnecting and then that it is back, and the
// redial's snapshot lands on a reset terminal. The snapshot is drawn for a
// fresh terminal; without the reset it would render inside whatever the cut
// left behind (here the alternate screen, a scroll region, reverse video, the
// line-drawing charset and half an escape sequence).
func TestConsoleSource_RedialResetsTerminalAndReportsReconnect(t *testing.T) {
	const dirty = "\x1b[?1049h\x1b[5;10r\x1b[7m\x1b(0lqqk\x1b[3"
	const snapshot = "\x1b[2J\x1b[Hscreen after the redial"
	droppingStreamServer(t, func(n int, ctx context.Context, c *websocket.Conn) {
		_ = c.Write(ctx, websocket.MessageText, castHeader)
		if n == 1 {
			writeCastEvent(ctx, c, 0.1, dirty)
			return
		}
		writeCastEvent(ctx, c, 0, snapshot)
		<-ctx.Done()
	})
	r := recordConsole(t)
	r.waitFor(t, snapshot)

	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(r.reconnects, []bool{true, false}) {
		t.Errorf("reconnect reports = %v, want [true false] (lost, then back)", r.reconnects)
	}
	if !r.backfill[len(r.backfill)-1] {
		t.Error("the redial's snapshot was reported as live output; it is history and must not reset the pane's staleness clock")
	}
	got := vt.NewEmulator(80, 24)
	for _, b := range r.chunks {
		_, _ = got.Write([]byte(b))
	}
	want := vt.NewEmulator(80, 24)
	_, _ = want.Write([]byte(snapshot))
	if got.Render() != want.Render() || got.IsAltScreen() {
		t.Errorf("screen after the redial (alt screen %v):\n%s\nwant the snapshot on a fresh terminal:\n%s", got.IsAltScreen(), got.Render(), want.Render())
	}
}

// TestConsoleSource_RetriesRefusedHandshake: a refused upgrade, here the 502 a
// proxy answers while nothing healthy is behind it, is retried like a drop and
// shows the reconnect banner meanwhile.
func TestConsoleSource_RetriesRefusedHandshake(t *testing.T) {
	streamTestServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			http.Error(w, "no healthy upstream", http.StatusBadGateway)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		ctx := c.CloseRead(r.Context())
		_ = c.Write(ctx, websocket.MessageText, castHeader)
		writeCastEvent(ctx, c, 0, "console is up")
		<-ctx.Done()
	})
	rec := recordConsole(t)
	rec.waitFor(t, "console is up")

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !slices.Equal(rec.reconnects, []bool{true, false}) {
		t.Errorf("reconnect reports = %v, want [true false]", rec.reconnects)
	}
}

// TestConsoleSource_StopsWhenSignInIsGone: the one failure the console
// source does not redial is a token fetch that only a new login can fix.
// Each redial would ask the identity provider to refresh a token it already
// refused, so the source raises the reconnect banner and returns the error.
func TestConsoleSource_StopsWhenSignInIsGone(t *testing.T) {
	conns := streamTestServer(t, func(int, http.ResponseWriter, *http.Request) {})
	asked := 0
	apiClient = slclient.New(slclient.Config{BaseURL: apiURL, Bearer: func(context.Context) (string, error) {
		asked++
		return "", fmt.Errorf("%w: the identity provider refused the refresh token", clientauth.ErrLoginRequired)
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var banner []bool
	err := consoleSource(uuid.NewString())(ctx, func([]byte, bool) {}, func() {}, func(on bool) { banner = append(banner, on) })
	if !errors.Is(err, clientauth.ErrLoginRequired) {
		t.Fatalf("console source returned %v, want the sign-in error", err)
	}
	if asked != 1 || conns.Load() != 0 {
		t.Errorf("token asked for %d times and %d requests sent, want 1 and 0", asked, conns.Load())
	}
	if !slices.Equal(banner, []bool{true}) {
		t.Errorf("reconnect banner calls = %v, want it raised once and left up", banner)
	}
}
