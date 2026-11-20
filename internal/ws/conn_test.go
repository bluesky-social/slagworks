package ws

import (
	"context"
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

func TestConnPing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, nil)
		if err != nil {
			return
		}
		// Keep reading to process control frames (server needs active reader).
		for {
			_, err := conn.ReadMessage(ctx)
			if err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, err := Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = client.Close(websocket.StatusNormalClosure, "") }()

	// coder/websocket requires a concurrent reader to process pong frames.
	go func() {
		for {
			_, err := client.ReadMessage(ctx)
			if err != nil {
				return
			}
		}
	}()

	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
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
