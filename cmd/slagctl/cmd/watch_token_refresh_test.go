package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/cmd/slagctl/tui"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// tokenWatchWorld is a slagctl signed in (saved config, cached token at-0 /
// rt-0) to a fake API that enforces the bearer on every request, REST and
// WebSocket upgrade alike, and serves one running deploy and its logs
// stream. Its identity provider refreshes with rotating refresh tokens
// while refreshOK is set and answers invalid_grant otherwise.
type tokenWatchWorld struct {
	t        *testing.T
	dir      string
	opID     uuid.UUID
	upgraded chan struct{}     // one value per accepted logs-stream upgrade
	bound    map[string]string // what the cached session was issued for

	mu        sync.Mutex
	refreshOK bool
	valid     map[string]bool // access tokens the API accepts
	issued    int             // refreshes the provider granted
	refreshes int             // token-endpoint requests the provider answered
	rejected  []string        // "<path> <token>" for each 401
	completed bool
	streams   map[*websocket.Conn]context.CancelFunc
}

func newTokenWatchWorld(t *testing.T, refreshOK bool) *tokenWatchWorld {
	t.Helper()
	cfgHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("BSKY_CLI_NO_VERSION_GATE", "1")
	t.Setenv("SLAGWORKS_API", "")
	// The watch's PersistentPreRunE replaces these globals with a client
	// bound to this test's config directory; don't leave it to later tests.
	prevURL, prevClient := apiURL, apiClient
	t.Cleanup(func() { apiURL, apiClient = prevURL, prevClient })
	w := &tokenWatchWorld{
		t: t, dir: filepath.Join(cfgHome, "slagctl"), opID: uuid.New(), upgraded: make(chan struct{}, 16),
		refreshOK: refreshOK, valid: map[string]bool{"at-0": true}, streams: map[*websocket.Conn]context.CancelFunc{},
	}

	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(rw).Encode(map[string]any{
				"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize",
				"token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/jwks",
			})
		case "/token":
			_ = r.ParseForm()
			w.mu.Lock()
			defer w.mu.Unlock()
			w.refreshes++
			if !w.refreshOK || r.Form.Get("refresh_token") != fmt.Sprintf("rt-%d", w.issued) {
				rw.WriteHeader(http.StatusBadRequest)
				_, _ = rw.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			w.issued++
			at := fmt.Sprintf("at-%d", w.issued)
			w.valid[at] = true
			_ = json.NewEncoder(rw).Encode(map[string]any{"access_token": at,
				"refresh_token": fmt.Sprintf("rt-%d", w.issued), "token_type": "Bearer", "expires_in": 600})
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(idp.Close)

	machineID := uuid.New()
	api := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.mu.Lock()
		ok := w.valid[tok]
		if !ok {
			w.rejected = append(w.rejected, r.URL.Path+" "+tok)
		}
		done := w.completed
		w.mu.Unlock()
		if !ok {
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"message":"invalid token"}`))
			return
		}
		switch r.URL.Path {
		case "/api/v1/operations/" + w.opID.String():
			st := model.OpRunning
			if done {
				st = model.OpCompleted
			}
			_ = json.NewEncoder(rw).Encode(model.Operation{ID: w.opID, MachineID: machineID, Type: model.OpDeploy, Status: st, CreatedAt: time.Now()})
		case "/api/v1/logs/stream":
			c, err := websocket.Accept(rw, r, nil)
			if err != nil {
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			w.mu.Lock()
			w.streams[c] = cancel
			w.mu.Unlock()
			w.upgraded <- struct{}{}
			_, _, _ = c.Read(ctx) // the subscribe frame
			for ctx.Err() == nil {
				w.mu.Lock()
				done := w.completed
				w.mu.Unlock()
				if done {
					e := slogkit.Envelope{TS: time.Now(), Source: slogkit.SourceGlobal, OpID: w.opID, Msg: "operation completed",
						Extra: map[string]any{"status": string(model.OpCompleted)}}
					buf, _ := ws.MarshalEnvelope(ws.TypeLogStreamEnvelope, "", ws.LogStreamEnvelopeMsg{ID: 1, Envelope: e})
					_ = c.Write(ctx, websocket.MessageText, buf)
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			_, _, _ = c.Read(ctx) // until dropped, or the client hangs up
			cancel()
			_ = c.CloseNow()
		default:
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.writeJSON("config.json", map[string]string{"api": api.URL, "oidc_issuer": idp.URL, "oidc_client_id": "slagctl"})
	w.bound = map[string]string{"api": api.URL, "issuer": idp.URL, "client_id": "slagctl"}
	w.writeToken(time.Now().Add(time.Hour))
	return w
}

// writeToken caches at-0 with its refresh token, expiring at expiry, as the
// session a login to this world's server leaves.
func (w *tokenWatchWorld) writeToken(expiry time.Time) {
	w.t.Helper()
	w.writeJSON("token.json", map[string]any{
		"api": w.bound["api"], "issuer": w.bound["issuer"], "client_id": w.bound["client_id"],
		"token": map[string]any{"access_token": "at-0", "refresh_token": "rt-0", "token_type": "Bearer",
			"expiry": expiry.Format(time.RFC3339)},
	})
}

func (w *tokenWatchWorld) writeJSON(name string, v any) {
	w.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, name), b, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

// watch runs `slagctl operation watch <op> --plain` with timeout in the
// background and waits for it to open the logs stream.
func (w *tokenWatchWorld) watch(timeout time.Duration) <-chan error {
	w.t.Helper()
	result := make(chan error, 1)
	go func() {
		root := NewRootCmd()
		root.SetArgs([]string{"operation", "watch", w.opID.String(), "--plain", "--timeout", timeout.String()})
		result <- root.ExecuteContext(context.Background())
	}()
	select {
	case <-w.upgraded:
	case <-time.After(5 * time.Second):
		w.t.Fatal("the watch never opened the logs stream")
	}
	return result
}

// expireAndDrop lets at-0 expire, as ten minutes passing would (the API
// stops accepting it and the cache records it as expired), then drops every
// open stream and completes the operation.
func (w *tokenWatchWorld) expireAndDrop() {
	w.t.Helper()
	w.mu.Lock()
	delete(w.valid, "at-0")
	w.mu.Unlock()
	w.writeToken(time.Now().Add(-time.Minute))
	w.mu.Lock()
	defer w.mu.Unlock()
	for c, cancel := range w.streams {
		cancel()
		_ = c.CloseNow()
	}
	w.streams = map[*websocket.Conn]context.CancelFunc{}
	w.completed = true
}

// TestOperationWatch_RefreshesAnExpiredTokenOnReconnect: `operation watch`
// outlives the access token it started with (production tokens last 10
// minutes, deploys take longer). When the logs stream then drops (a global
// restart, a proxy idle timeout, a laptop that slept) the reconnect, and
// every op-row poll, must carry a refreshed token: the operator's refresh
// token is valid, so the watch must end with the operation's outcome.
//
// Steps: sign in (saved config + cached token at-0 / rt-0); start the watch;
// once it is attached, let at-0 expire; drop the stream; complete the
// operation. Expected: the watch returns nil. Before the fix it kept
// presenting at-0, every reconnect and poll answered 401, the 401s were
// swallowed, and the watch ended at --timeout reporting the operation as
// unfinished.
func TestOperationWatch_RefreshesAnExpiredTokenOnReconnect(t *testing.T) {
	w := newTokenWatchWorld(t, true)
	result := w.watch(6 * time.Second)
	w.expireAndDrop()

	err := <-result
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		firstRejected := ""
		if len(w.rejected) > 0 {
			firstRejected = w.rejected[0]
		}
		t.Fatalf("watch of a completed operation with a valid refresh token failed: %v\n(401s answered: %d, e.g. %q; refreshes done: %d)",
			err, len(w.rejected), firstRejected, w.issued)
	}
}

// TestOperationWatch_EndsWhenTheSignInCannotBeRefreshed: the same watch,
// but the identity provider refuses the refresh token (revoked, or the
// session ended). No retry can succeed, so the watch must stop within one
// op-row poll interval with an error that names `slagctl auth login`,
// rather than poll until --timeout, and must not keep asking the provider
// to refresh once a second while it waits.
func TestOperationWatch_EndsWhenTheSignInCannotBeRefreshed(t *testing.T) {
	w := newTokenWatchWorld(t, false)
	start := time.Now()
	result := w.watch(time.Minute)
	w.expireAndDrop()

	var err error
	select {
	case err = <-result:
	case <-time.After(3 * plainWatchPollInterval):
		t.Fatalf("the watch was still running %s after its sign-in became unrefreshable", time.Since(start).Round(time.Second))
	}
	if err == nil || !strings.Contains(err.Error(), "slagctl auth login") {
		t.Fatalf("watch error = %v, want one naming `slagctl auth login`", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// One refused refresh from the stream's reconnect and one from the poll
	// that ends the watch. golang.org/x/oauth2 sends a refused refresh twice
	// (once for each way of presenting the client ID it tries), so that is
	// four token requests; a reconnect loop would add two a second.
	if w.refreshes > 4 {
		t.Fatalf("the provider was asked to refresh %d times in %s; the watch kept retrying a refused refresh token",
			w.refreshes, time.Since(start).Round(time.Second))
	}
}

// errSignedOut is what apiClient returns once the sign-in cannot be
// refreshed (see bearerSource).
var errSignedOut = fmt.Errorf("%w: the identity provider refused the refresh token", clientauth.ErrLoginRequired)

// TestWatchTUIPoll_StopsTheWatchWhenSignedOut: the TUI ignores op-row poll
// errors, so its poll is wrapped to end the watch (cancel its context with
// the error as the cause) when the sign-in is gone, and only then.
func TestWatchTUIPoll_StopsTheWatchWhenSignedOut(t *testing.T) {
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	failWith := func(err error) tui.OpPoller {
		return func(context.Context) (bool, bool, string, error) { return false, false, "", err }
	}

	_, _, _, _ = stopOnLoginRequired(failWith(errors.New("connection refused")), stop)(ctx)
	if ctx.Err() != nil {
		t.Fatalf("a transient poll error ended the watch: %v", context.Cause(ctx))
	}
	if _, _, _, err := stopOnLoginRequired(failWith(errSignedOut), stop)(ctx); !errors.Is(err, errSignedOut) {
		t.Fatalf("poll error = %v, want it passed through", err)
	}
	if !errors.Is(context.Cause(ctx), errSignedOut) {
		t.Fatalf("watch context cause = %v, want the sign-in error", context.Cause(ctx))
	}
}

// TestListWatch_EndsWhenSignedOut: `<list> -w` shows a failed refresh and
// tries again in 3s, except when the sign-in is gone: that ends the watch
// with the error instead of asking the identity provider every 3s, for as
// long as the watch is left open, to refresh a token it refused.
func TestListWatch_EndsWhenSignedOut(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		calls := 0
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := watchPlainList(ctx, func() (string, error) { calls++; return "", errSignedOut })
		if !errors.Is(err, errSignedOut) || calls != 1 {
			t.Fatalf("watchPlainList = %v after %d fetches; want the sign-in error after 1", err, calls)
		}
	})
	t.Run("tui", func(t *testing.T) {
		m, cmd := newListWatchModel(nil).Update(listFetchErrMsg{errSignedOut})
		if got := m.(listWatchModel).signInErr; !errors.Is(got, errSignedOut) {
			t.Fatalf("signInErr = %v, want the sign-in error", got)
		}
		if cmd == nil {
			t.Fatal("no command after a sign-in error; want tea.Quit")
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatal("the watch did not quit after a sign-in error")
		}

		m, _ = newListWatchModel(nil).Update(listFetchErrMsg{errors.New("connection refused")})
		if lm := m.(listWatchModel); lm.signInErr != nil || lm.fetchErr == nil {
			t.Fatalf("a transient fetch error: signInErr = %v, fetchErr = %v; want it shown and retried", lm.signInErr, lm.fetchErr)
		}
	})
}
