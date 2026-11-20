package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// testStreamKeepalive replaces the 30s ping interval in these tests.
const testStreamKeepalive = 50 * time.Millisecond

// countingListener adds the bytes the server writes on every connection it
// accepts to n. coder/websocket writes and answers pings internally, so
// this is how a test sees one, whether or not the client is reading.
type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: c, n: l.n}, nil
}

type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Write(b []byte) (int, error) {
	k, err := c.Conn.Write(b)
	c.n.Add(int64(k))
	return k, err
}

// newCountingServer serves h and returns the WebSocket base URL and the
// count of bytes the server has written.
func newCountingServer(t *testing.T, h http.Handler) (string, *atomic.Int64) {
	t.Helper()
	n := new(atomic.Int64)
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = countingListener{Listener: srv.Listener, n: n}
	srv.Start()
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), n
}

func dialStreamURL(ctx context.Context, t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.Dial(ctx, url, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// subscribeAll sends a logs-stream subscribe matching every row and reads
// the subscribed reply.
func subscribeAll(ctx context.Context, t *testing.T, conn *websocket.Conn) {
	t.Helper()
	sub, err := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", ws.LogStreamSubscribeMsg{})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, sub); err != nil {
		t.Fatalf("write subscribe: %v", err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read subscribed: %v", err)
	}
	var env ws.Envelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Type != ws.TypeSubscribed {
		t.Fatalf("first frame %s, want %s", raw, ws.TypeSubscribed)
	}
}

// waitForWrite waits up to window for n to move past base and fails the
// test if it does not.
func waitForWrite(t *testing.T, n *atomic.Int64, base int64, window time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(window)
	for n.Load() == base {
		if time.Now().After(deadline) {
			t.Fatalf("idle %s stream sent no byte (not even a ping) for %v; with a %v keepalive it should ping about every %v",
				what, window, testStreamKeepalive, testStreamKeepalive)
		}
		time.Sleep(testStreamKeepalive / 5)
	}
}

// TestOperatorStreamsPingWhileIdle pins that the logs and console streams
// put bytes on the wire while they have nothing to send. A stream whose
// filter matches no new rows, or a console that prints nothing (a host at
// a login prompt), otherwise writes nothing for as long as it stays quiet,
// and a proxy or load balancer between slagctl and global closes a
// connection that carries no bytes for its idle timeout: `slagctl machine
// logs -f` and `--sol -f` then exit, and the watch TUI's console pane
// stops updating.
func TestOperatorStreamsPingWhileIdle(t *testing.T) {
	machineID := uuid.New()
	for _, tc := range []struct {
		name  string
		path  string
		setup func(*Server)
		// start completes the stream's handshake on a fresh connection.
		start func(ctx context.Context, t *testing.T, conn *websocket.Conn)
	}{
		{
			name: "logs",
			path: "/api/v1/logs/stream",
			setup: func(s *Server) {
				s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
				s.logStore = emptyLogStore{}
			},
			start: subscribeAll,
		},
		{
			name: "console",
			path: "/api/v1/console/stream?follow=true&machine=" + machineID.String(),
			setup: func(s *Server) {
				s.echo.GET("/api/v1/console/stream", s.handleConsoleStream)
				s.consoleStore = &fakeConsoleStore{cast: &model.ConsoleCast{
					ID: 1, MachineID: machineID, Width: 80, Height: 24, CastStartedAt: time.Now(),
				}}
			},
			start: func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
				if _, _, err := conn.Read(ctx); err != nil {
					t.Fatalf("read asciicast header: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{echo: echo.New(), streamKeepalive: testStreamKeepalive}
			tc.setup(s)
			base, n := newCountingServer(t, s.echo)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn := dialStreamURL(ctx, t, base+tc.path)
			tc.start(ctx, t, conn)

			// Keep reading, as every slagctl client does, so pings are
			// answered. Read returns only a data frame or an error, and an
			// idle stream should produce neither.
			readDone := make(chan error, 1)
			go func() {
				_, _, err := conn.Read(ctx)
				readDone <- err
			}()

			waitForWrite(t, n, n.Load(), 40*testStreamKeepalive, tc.name)
			time.Sleep(5 * testStreamKeepalive)
			select {
			case err := <-readDone:
				t.Fatalf("idle %s stream returned from Read (err=%v); the bytes it sent must be pings, and a client that reads must stay connected", tc.name, err)
			default:
			}
		})
	}
}

// growingLogStore serves the rows added so far, honoring the cursor and
// limit the stream queries with.
type growingLogStore struct {
	mu   sync.Mutex
	rows []logstore.Result
}

func (s *growingLogStore) add(id int64, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, logstore.Result{ID: id, Envelope: slogkit.Envelope{
		TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: msg,
	}})
}

func (s *growingLogStore) Query(_ context.Context, f logstore.QueryFilter) ([]logstore.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []logstore.Result
	for _, r := range s.rows {
		if r.ID > f.LastSeenID && (f.Limit == 0 || len(out) < f.Limit) {
			out = append(out, r)
		}
	}
	return out, nil
}

// TestLogsStreamKeepsClientThatStopsReading pins that an unanswered ping
// does not end a stream. A client stops reading, and so stops answering
// pings, whenever its output backs up: `slagctl machine logs -f | less`
// while the operator pages, or a terminal paused with ^S. It is still
// there, and gets the rows that arrived in the meantime once it reads
// again. The stream ends when the client closes or drops the connection.
func TestLogsStreamKeepsClientThatStopsReading(t *testing.T) {
	t.Parallel()
	st := &growingLogStore{}
	s := &Server{echo: echo.New(), logStore: st, streamKeepalive: testStreamKeepalive}
	s.echo.GET("/api/v1/logs/stream", s.handleLogsStream)
	base, n := newCountingServer(t, s.echo)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn := dialStreamURL(ctx, t, base+"/api/v1/logs/stream")
	subscribeAll(ctx, t, conn)

	// No Read for 20 keepalive intervals: the server pings, and every
	// ping in that time goes unanswered.
	waitForWrite(t, n, n.Load(), 40*testStreamKeepalive, "logs")
	time.Sleep(20 * testStreamKeepalive)

	st.add(1, "after the pause")
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("stream closed while the client was not reading: %v", err)
	}
	var env ws.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var m ws.LogStreamEnvelopeMsg
	if env.Type != ws.TypeLogStreamEnvelope || json.Unmarshal(env.Data, &m) != nil || m.Envelope.Msg != "after the pause" {
		t.Fatalf("got frame %s, want the row added while the client was not reading", raw)
	}
}
