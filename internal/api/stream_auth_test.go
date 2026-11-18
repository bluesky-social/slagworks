package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// streamAuthServer builds the production router (NewServer) with the log
// and console stream stores wired and a fake verifier that knows one
// operator token ("op-tok", in the required group) and one token outside
// it ("nogroup-tok"). It returns the store, the server and one machine
// that exists.
func streamAuthServer(t *testing.T) (*store.Store, *httptest.Server, uuid.UUID) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	machineID := uuid.New()
	if err := st.CreateMachine(&model.Machine{ID: machineID, NetBoxSerial: "SN-STREAM"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{
		Store:          st,
		RequiredGroups: []string{"platform"},
		LogStore:       logstore.New(st.DB()),
		ConsoleStore:   NewConsoleStoreAdapter(st),
		Verifier: &fakeVerifier{claims: map[string]*tokenverify.Claims{
			"op-tok":      {Subject: "op", Groups: []string{"platform"}},
			"nogroup-tok": {Subject: "eng", Groups: []string{"engineering"}},
		}},
	})
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(s.Echo())
	t.Cleanup(srv.Close)
	return st, srv, machineID
}

// dialStream attempts a WebSocket upgrade with the given bearer token
// (none when empty) and extra headers, returning the connection (nil on a
// refused upgrade) and the handshake status.
func dialStream(t *testing.T, srv *httptest.Server, path, token string, extra http.Header) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := http.Header{}
	for k, v := range extra {
		h[k] = v
	}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path,
		&websocket.DialOptions{HTTPHeader: h})
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		return nil, status
	}
	return conn, status
}

func consoleCastCount(t *testing.T, st *store.Store, machineID uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := st.DB().Model(&model.ConsoleCast{}).Where("machine_id = ?", machineID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStreamRoutesRequireOperatorAuth pins that the log and console
// streams sit behind the same operator authentication and group check as
// the rest of the read API: no token is 401, a token outside the required
// groups is 403, and only an operator token gets the upgrade.
func TestStreamRoutesRequireOperatorAuth(t *testing.T) {
	_, srv, machineID := streamAuthServer(t)

	routes := []struct {
		name string
		path string
		// check runs on an accepted connection and asserts the stream
		// actually serves the operator.
		check func(t *testing.T, conn *websocket.Conn)
	}{
		{
			name: "logs stream",
			path: "/api/v1/logs/stream",
			check: func(t *testing.T, conn *websocket.Conn) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				buf, err := ws.MarshalEnvelope(ws.TypeLogStreamSubscribe, "", ws.LogStreamSubscribeMsg{})
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.Write(ctx, websocket.MessageText, buf); err != nil {
					t.Fatalf("write subscribe: %v", err)
				}
				_, raw, err := conn.Read(ctx)
				if err != nil {
					t.Fatalf("read subscribed: %v", err)
				}
				var env ws.Envelope
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
				if env.Type != ws.TypeSubscribed {
					t.Fatalf("first frame type = %q, want %q", env.Type, ws.TypeSubscribed)
				}
			},
		},
		{
			name: "console stream",
			path: "/api/v1/console/stream?machine=" + machineID.String(),
			check: func(t *testing.T, conn *websocket.Conn) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, raw, err := conn.Read(ctx)
				if err != nil {
					t.Fatalf("read header: %v", err)
				}
				var header map[string]any
				if err := json.Unmarshal(raw, &header); err != nil {
					t.Fatalf("header decode: %v / %q", err, raw)
				}
				if v, _ := header["version"].(float64); v != 2 {
					t.Fatalf("header version = %v, want 2", header["version"])
				}
			},
		},
		{
			name: "console stream snapshot",
			path: "/api/v1/console/stream?snapshot=true&machine=" + machineID.String(),
			check: func(t *testing.T, conn *websocket.Conn) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, _, err := conn.Read(ctx); err != nil {
					t.Fatalf("read header: %v", err)
				}
			},
		},
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			if conn, status := dialStream(t, srv, r.path, "", nil); conn != nil || status != http.StatusUnauthorized {
				t.Fatalf("no token: status=%d upgraded=%v, want 401 and no upgrade", status, conn != nil)
			}
			if conn, status := dialStream(t, srv, r.path, "not-a-token", nil); conn != nil || status != http.StatusUnauthorized {
				t.Fatalf("invalid token: status=%d upgraded=%v, want 401 and no upgrade", status, conn != nil)
			}
			if conn, status := dialStream(t, srv, r.path, "nogroup-tok", nil); conn != nil || status != http.StatusForbidden {
				t.Fatalf("token outside required groups: status=%d upgraded=%v, want 403 and no upgrade", status, conn != nil)
			}
			conn, status := dialStream(t, srv, r.path, "op-tok", nil)
			if conn == nil {
				t.Fatalf("operator token: status=%d, want 101 upgrade", status)
			}
			defer conn.CloseNow() //nolint:errcheck
			r.check(t, conn)
		})
	}
}

// TestStreamRoutesRejectCrossOriginUpgrade pins the Origin rule: an
// upgrade without an Origin header (slagctl) or from the server's own
// origin is accepted, a browser upgrade from any other origin is refused
// even with a valid operator token.
func TestStreamRoutesRejectCrossOriginUpgrade(t *testing.T) {
	_, srv, machineID := streamAuthServer(t)
	for _, path := range []string{
		"/api/v1/logs/stream",
		"/api/v1/console/stream?machine=" + machineID.String(),
	} {
		t.Run(path, func(t *testing.T) {
			cross := http.Header{"Origin": {"https://attacker.example"}}
			if conn, status := dialStream(t, srv, path, "op-tok", cross); conn != nil || status != http.StatusForbidden {
				t.Fatalf("cross-origin: status=%d upgraded=%v, want 403 and no upgrade", status, conn != nil)
			}
			same := http.Header{"Origin": {srv.URL}}
			conn, status := dialStream(t, srv, path, "op-tok", same)
			if conn == nil {
				t.Fatalf("same-origin: status=%d, want 101 upgrade", status)
			}
			_ = conn.CloseNow()
		})
	}
}

// TestConsoleStreamUnknownMachine404NoRow pins that the console stream
// refuses a machine id that names no machine before it writes anything:
// the old behaviour created a console_casts row for any UUID supplied.
func TestConsoleStreamUnknownMachine404NoRow(t *testing.T) {
	st, srv, _ := streamAuthServer(t)
	unknown := uuid.New()
	path := "/api/v1/console/stream?machine=" + unknown.String()

	if conn, status := dialStream(t, srv, path, "op-tok", nil); conn != nil || status != http.StatusNotFound {
		t.Fatalf("unknown machine: status=%d upgraded=%v, want 404 and no upgrade", status, conn != nil)
	}
	if n := consoleCastCount(t, st, unknown); n != 0 {
		t.Fatalf("console_casts rows for unknown machine = %d, want 0", n)
	}
	// Unauthenticated callers never reach the lookup or the write either.
	if conn, status := dialStream(t, srv, path, "", nil); conn != nil || status != http.StatusUnauthorized {
		t.Fatalf("unknown machine, no token: status=%d upgraded=%v, want 401", status, conn != nil)
	}
	if n := consoleCastCount(t, st, unknown); n != 0 {
		t.Fatalf("console_casts rows after unauthenticated request = %d, want 0", n)
	}
}
