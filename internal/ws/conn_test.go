package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestConnReadWriteRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Start a WebSocket echo server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, nil)
		if err != nil {
			t.Logf("accept: %v", err)
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

		env, err := conn.ReadMessage(ctx)
		if err != nil {
			t.Logf("read: %v", err)
			return
		}
		// Echo it back.
		_ = conn.WriteMessage(ctx, env)
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close(websocket.StatusNormalClosure, "") }()

	// Send a typed message.
	err = client.WriteTyped(ctx, TypeHeartbeat, "", HeartbeatMsg{WorkerID: "w-1"})
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read the echo.
	env, err := client.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if env.Type != TypeHeartbeat {
		t.Fatalf("type: got %q, want %q", env.Type, TypeHeartbeat)
	}
}

// slowConn delivers what it reads a few KiB at a time with a pause before
// each read: a slow link that is still delivering.
type slowConn struct{ net.Conn }

func (c slowConn) Read(p []byte) (int, error) {
	time.Sleep(3 * time.Millisecond)
	if len(p) > 4<<10 {
		p = p[:4<<10]
	}
	return c.Conn.Read(p)
}

// TestConnReadMessageIdle: ReadMessageIdle gives up on a peer that sends
// nothing for the idle window, and only on that. A message that takes
// several idle windows to arrive in full is still read, because its bytes
// keep arriving; timing out whole messages instead would cut a slow site
// in the middle of every large upload, and its replay after the reconnect
// would be cut the same way.
func TestConnReadMessageIdle(t *testing.T) {
	// The idle window also covers the wait for the large message's first
	// bytes, which the server has to encode first; under the race detector
	// on a busy machine that took longer than 150ms. The 2 MiB payload at
	// 4 KiB per 3ms read still takes over three windows to arrive.
	const idle = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	release := make(chan struct{})
	payload := strings.Repeat("x", 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		if r.URL.Path == "/large" {
			_ = conn.WriteTyped(ctx, TypeLogBatch, "", payload)
		}
		<-release // otherwise silent, with the connection open
	}))
	defer srv.Close()
	defer close(release)

	dial := func(path string) *Conn {
		t.Helper()
		client := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				return slowConn{c}, err
			},
		}}
		c, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path, &websocket.DialOptions{HTTPClient: client})
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.CloseNow() })
		c.SetReadLimit(MaxMessageBytes)
		return c
	}

	t.Run("silent peer", func(t *testing.T) {
		c := dial("/silent")
		start := time.Now()
		_, err := c.ReadMessageIdle(ctx, idle)
		if !errors.Is(err, ErrPeerSilent) {
			t.Fatalf("read from a silent peer returned %v after %s; want ErrPeerSilent", err, time.Since(start))
		}
		if took := time.Since(start); took > 20*idle {
			t.Fatalf("gave up on a silent peer after %s; idle timeout %s", took, idle)
		}
	})

	t.Run("slow large message", func(t *testing.T) {
		c := dial("/large")
		start := time.Now()
		env, err := c.ReadMessageIdle(ctx, idle)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("a %d-byte message still arriving was cut after %s (idle timeout %s): %v", len(payload), took, idle, err)
		}
		var got string
		if err := json.Unmarshal(env.Data, &got); err != nil || got != payload {
			t.Fatalf("message arrived damaged: %d bytes, err %v", len(got), err)
		}
		if took < 2*idle {
			t.Fatalf("the message arrived in %s, under two idle windows (%s), so this run does not show that a slow message survives", took, 2*idle)
		}
	})
}

// TestDial_RefusalCarriesTheServersReason: a site worker whose connection
// global refuses logs Dial's error and nothing else, on every reconnect.
// Global answers a refused upgrade with a status and a sentence that says
// what is wrong ("missing worker token: send it as Authorization: Bearer
// <token>"); Dial must not reduce it to the bare status code. A proxy's
// error page arrives the same way, so the reason is kept to one bounded
// line.
func TestDial_RefusalCarriesTheServersReason(t *testing.T) {
	const reason = "missing worker token: send it as Authorization: Bearer <token>"
	// An HTML error page longer than the bound: its first part is kept on
	// one line, and nothing from past the first 512 bytes.
	page := "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\n" +
		strings.Repeat("<p>filler</p>\n", 60) + "AFTER-THE-BOUND</body></html>\n"
	cases := []struct {
		name    string
		status  int
		body    string
		want    []string
		notWant []string
	}{
		{name: "hub refusal", status: http.StatusUnauthorized, body: reason,
			want: []string{"401", reason}},
		{name: "proxy error page", status: http.StatusBadGateway, body: page,
			want: []string{"502", "<html> <head><title>502 Bad Gateway</title></head> <body> <p>filler</p>"}, notWant: []string{"\n", "AFTER-THE-BOUND"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, tc.body, tc.status)
			}))
			defer srv.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err == nil {
				_ = conn.CloseNow()
				t.Fatalf("Dial succeeded against a server that answers %d", tc.status)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Dial error %q does not contain %q", err, want)
				}
			}
			for _, bad := range tc.notWant {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("Dial error %q contains %q", err, bad)
				}
			}
		})
	}
}
