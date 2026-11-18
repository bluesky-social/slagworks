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
