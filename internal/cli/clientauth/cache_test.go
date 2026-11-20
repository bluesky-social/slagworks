package clientauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
)

func tokenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "slagctl", "token.json")
}

// testBinding is what the tests below bind their sessions to where the
// binding itself is not under test.
var testBinding = binding{API: "https://api.example", Issuer: "https://idp.example/", ClientID: "test"}

func testSession(tok *oauth2.Token) session { return session{binding: testBinding, Token: tok} }

func TestCacheRoundTripAndPerms(t *testing.T) {
	path := tokenPath(t)
	tok := &oauth2.Token{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		Expiry:       time.Now().Add(time.Hour),
		TokenType:    "Bearer",
	}
	if err := writeCache(path, testSession(tok)); err != nil {
		t.Fatalf("writeCache: %v", err)
	}
	// file perms 0600, dir perms 0700
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file perms = %o, want 0600", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(path))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir perms = %o, want 0700", di.Mode().Perm())
	}
	got, err := readCache(path)
	if err != nil {
		t.Fatalf("readCache: %v", err)
	}
	if got.Token.AccessToken != "at-1" || got.Token.RefreshToken != "rt-1" || got.binding != testBinding {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// TestReadCache_ExpiryFromAccessToken: a cached token without an expiry (its
// token response had no expires_in) takes the expiry from the access token's
// exp claim. An expiry the response did give is kept, and an access token
// whose exp cannot be read stays without one.
func TestReadCache_ExpiryFromAccessToken(t *testing.T) {
	exp := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	recorded := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name string
		tok  oauth2.Token
		want time.Time
	}{
		{"JWT without expiry", oauth2.Token{AccessToken: mkJWT(t, map[string]any{"exp": exp.Unix()})}, exp},
		{"JWT with exp 0", oauth2.Token{AccessToken: mkJWT(t, map[string]any{"exp": 0})}, time.Unix(0, 0)},
		{"JWT with a fractional exp", oauth2.Token{AccessToken: mkJWT(t, map[string]any{"exp": float64(exp.Unix()) + 0.5})}, exp},
		{"JWT with a claim the identity display does not model", oauth2.Token{AccessToken: mkJWT(t, map[string]any{"exp": exp.Unix(), "groups": "admins"})}, exp},
		{"JWT with expiry", oauth2.Token{AccessToken: mkJWT(t, map[string]any{"exp": exp.Unix()}), Expiry: recorded}, recorded},
		{"JWT without exp claim", oauth2.Token{AccessToken: mkJWT(t, map[string]any{"sub": "op"})}, time.Time{}},
		{"opaque token", oauth2.Token{AccessToken: "opaque-at"}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tokenPath(t)
			if err := writeCache(path, testSession(&tc.tok)); err != nil {
				t.Fatal(err)
			}
			got, err := readCache(path)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Token.Expiry.Equal(tc.want) {
				t.Errorf("Expiry = %v, want %v", got.Token.Expiry, tc.want)
			}
		})
	}
}

func TestReadCacheMissing(t *testing.T) {
	if _, err := readCache(tokenPath(t)); err == nil {
		t.Error("expected error reading nonexistent cache")
	}
}

// TestCachedBearer_BareTokenIsNoSession: a cache holding a bare token, with
// no record of what it was issued for (the layout before the cache recorded
// one), cannot be tied to any server, so it is never handed out.
func TestCachedBearer_BareTokenIsNoSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := Config{AppName: "slagctl", API: testBinding.API, Issuer: testBinding.Issuer, ClientID: testBinding.ClientID, LoopbackPort: 8765}
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	bare := `{"access_token":"at-bare","refresh_token":"rt-bare","token_type":"Bearer","expiry":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`
	if err := os.WriteFile(path, []byte(bare), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := CachedBearer(context.Background(), cfg); err == nil {
		t.Fatalf("CachedBearer handed out %q from a cache that does not say what it was issued for", got)
	}
}

func TestDeleteCache(t *testing.T) {
	path := tokenPath(t)
	_ = writeCache(path, testSession(&oauth2.Token{AccessToken: "x", Expiry: time.Now().Add(time.Hour)}))
	if err := deleteCache(path); err != nil {
		t.Fatalf("deleteCache: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("cache file should be gone")
	}
	// deleting again is a no-op, not an error
	if err := deleteCache(path); err != nil {
		t.Errorf("second deleteCache should be no-op: %v", err)
	}
}

// TestDeleteCache_LockStillExcludes: deleting the cache under the lock must
// leave the lock working. Unlinking the held lock file would let the next
// invocation create and lock a fresh inode while the holder (and anyone
// already queued on the old inode) still believes it is exclusive.
func TestDeleteCache_LockStillExcludes(t *testing.T) {
	path := tokenPath(t)
	if err := writeCache(path, testSession(&oauth2.Token{AccessToken: "x", Expiry: time.Now().Add(time.Hour)})); err != nil {
		t.Fatal(err)
	}
	holder := flock.New(path + ".lock")
	if err := holder.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Unlock() }()

	if err := deleteCache(path); err != nil {
		t.Fatalf("deleteCache: %v", err)
	}

	newcomer := flock.New(path + ".lock")
	got, err := newcomer.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	if got {
		_ = newcomer.Unlock()
		t.Fatal("a second invocation took the token lock while it was held: deleteCache unlinked the lock file")
	}
}

// fakeTokenEndpoint serves the oauth2 token-refresh endpoint. It tracks which
// refresh tokens it has seen and rejects reuse (mimicking a provider that
// rotates refresh tokens), so tests can assert we don't replay a consumed
// refresh token.
type fakeTokenEndpoint struct {
	mu       sync.Mutex
	consumed map[string]bool
	issued   int
	srv      *httptest.Server
}

func newFakeTokenEndpoint() *fakeTokenEndpoint {
	f := &fakeTokenEndpoint{consumed: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeTokenEndpoint) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rt := r.Form.Get("refresh_token")
	f.mu.Lock()
	defer f.mu.Unlock()
	if rt == "" || f.consumed[rt] {
		// reused/invalid refresh token -> reject (rotation semantics)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	f.consumed[rt] = true
	f.issued++
	newRT := rt + "-next"
	resp := map[string]any{
		"access_token":  "at-refreshed",
		"refresh_token": newRT,
		"token_type":    "Bearer",
		"expires_in":    3600,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeTokenEndpoint) config() *oauth2.Config {
	return &oauth2.Config{
		ClientID: "test",
		Endpoint: oauth2.Endpoint{TokenURL: f.srv.URL + "/token"},
	}
}

func (f *fakeTokenEndpoint) close() { f.srv.Close() }

// TestPersistingTokenSource_RefreshAndWriteBack: an expired token with a
// refresh token gets refreshed, the rotated token is written back to disk, and
// the returned token is the refreshed one.
func TestPersistingTokenSource_RefreshAndWriteBack(t *testing.T) {
	fe := newFakeTokenEndpoint()
	defer fe.close()
	path := tokenPath(t)
	expired := &oauth2.Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-1",
		Expiry:       time.Now().Add(-time.Minute), // expired
		TokenType:    "Bearer",
	}
	if err := writeCache(path, testSession(expired)); err != nil {
		t.Fatal(err)
	}

	ts := newPersistingTokenSource(context.Background(), fe.config(), testBinding, path)
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if tok.AccessToken != "at-refreshed" {
		t.Errorf("access token = %q, want at-refreshed", tok.AccessToken)
	}
	// the rotated refresh token must be persisted, still bound to its server
	onDisk, err := readCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Token.RefreshToken != "rt-1-next" {
		t.Errorf("on-disk refresh token = %q, want rt-1-next (rotation not persisted)", onDisk.Token.RefreshToken)
	}
	if onDisk.Token.AccessToken != "at-refreshed" {
		t.Errorf("on-disk access token = %q, want at-refreshed", onDisk.Token.AccessToken)
	}
	if onDisk.binding != testBinding {
		t.Errorf("on-disk binding = %+v, want %+v", onDisk.binding, testBinding)
	}
}

// TestPersistingTokenSource_ValidTokenNoRefresh: a still-valid cached token is
// returned without contacting the token endpoint.
func TestPersistingTokenSource_ValidTokenNoRefresh(t *testing.T) {
	fe := newFakeTokenEndpoint()
	defer fe.close()
	path := tokenPath(t)
	valid := &oauth2.Token{
		AccessToken:  "at-valid",
		RefreshToken: "rt-1",
		Expiry:       time.Now().Add(time.Hour),
		TokenType:    "Bearer",
	}
	_ = writeCache(path, testSession(valid))

	ts := newPersistingTokenSource(context.Background(), fe.config(), testBinding, path)
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-valid" {
		t.Errorf("got %q, want at-valid (should not refresh)", tok.AccessToken)
	}
	if fe.issued != 0 {
		t.Errorf("token endpoint hit %d times, want 0 for a valid token", fe.issued)
	}
}

// TestPersistingTokenSource_RefreshErrorPreservesCache: a failing refresh must
// NOT overwrite the cache with a bad/empty token.
func TestPersistingTokenSource_RefreshErrorPreservesCache(t *testing.T) {
	fe := newFakeTokenEndpoint()
	defer fe.close()
	path := tokenPath(t)
	// refresh token already consumed -> endpoint will reject it
	fe.consumed["rt-dead"] = true
	expired := &oauth2.Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-dead",
		Expiry:       time.Now().Add(-time.Minute),
		TokenType:    "Bearer",
	}
	_ = writeCache(path, testSession(expired))

	ts := newPersistingTokenSource(context.Background(), fe.config(), testBinding, path)
	if _, err := ts.Token(); err == nil {
		t.Fatal("expected refresh error for a consumed refresh token")
	}
	// cache must be untouched (still the old token, not corrupted)
	onDisk, err := readCache(path)
	if err != nil {
		t.Fatalf("cache should still be readable: %v", err)
	}
	if onDisk.Token.RefreshToken != "rt-dead" || onDisk.Token.AccessToken != "at-old" {
		t.Errorf("cache was modified on failed refresh: %+v", onDisk.Token)
	}
}

// TestPersistingTokenSource_LoginRequiredOnlyWhenNoRetryCanHelp: a refresh
// the provider refuses, an expired token with no refresh token, and a cache
// that a logout removed all match ErrLoginRequired, so a long-running
// command stops instead of retrying them; a provider that is down, failing
// or rate-limiting does not, so the command keeps retrying.
func TestPersistingTokenSource_LoginRequiredOnlyWhenNoRetryCanHelp(t *testing.T) {
	answer := func(status int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	expired := func(refreshToken string) *oauth2.Token {
		return &oauth2.Token{AccessToken: "at-old", RefreshToken: refreshToken, Expiry: time.Now().Add(-time.Minute)}
	}
	for _, tc := range []struct {
		name     string
		provider string
		cached   *oauth2.Token // nil: no cache file
		bound    binding       // what the cached session was issued for
		want     bool
	}{
		{"refresh token refused", answer(http.StatusBadRequest), expired("rt"), testBinding, true},
		{"client refused", answer(http.StatusUnauthorized), expired("rt"), testBinding, true},
		{"no refresh token", answer(http.StatusOK), expired(""), testBinding, true},
		{"logged out", answer(http.StatusOK), nil, testBinding, true},
		{"session for another server", answer(http.StatusOK), expired("rt"), binding{API: "https://other.example", Issuer: testBinding.Issuer, ClientID: testBinding.ClientID}, true},
		{"provider failing", answer(http.StatusServiceUnavailable), expired("rt"), testBinding, false},
		{"provider rate-limiting", answer(http.StatusTooManyRequests), expired("rt"), testBinding, false},
		{"provider unreachable", down.URL, expired("rt"), testBinding, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tokenPath(t)
			if tc.cached != nil {
				if err := writeCache(path, session{binding: tc.bound, Token: tc.cached}); err != nil {
					t.Fatal(err)
				}
			}
			oc := &oauth2.Config{ClientID: "test", Endpoint: oauth2.Endpoint{TokenURL: tc.provider + "/token"}}
			_, err := newPersistingTokenSource(context.Background(), oc, testBinding, path).Token()
			if err == nil {
				t.Fatal("Token() succeeded; want an error")
			}
			if got := errors.Is(err, ErrLoginRequired); got != tc.want {
				t.Fatalf("errors.Is(%q, ErrLoginRequired) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

// TestCachedBearer_LoginRequiredBeforeRefreshing: the checks CachedBearer
// makes before it would contact the provider (nothing cached, an expired
// token with no refresh token) match ErrLoginRequired too.
func TestCachedBearer_LoginRequiredBeforeRefreshing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx := context.Background()
	// The issuer is never contacted: both cases fail before a refresh.
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: "http://127.0.0.1:1", ClientID: "test", LoopbackPort: 1}
	if _, err := CachedBearer(ctx, cfg); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("with nothing cached: err = %v, want ErrLoginRequired", err)
	}
	if err := WriteCachedToken(ctx, cfg, &oauth2.Token{AccessToken: "at-old", Expiry: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := CachedBearer(ctx, cfg); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("with an expired token and no refresh token: err = %v, want ErrLoginRequired", err)
	}
}

// TestPersistingTokenSource_ConcurrentRefreshSingleRotation: with N concurrent
// Token() calls on one expired token, the lock must serialize them so the
// refresh token is rotated exactly once (not N times, which would invalidate
// the session). The waiters read the freshly-written valid token and skip.
func TestPersistingTokenSource_ConcurrentRefreshSingleRotation(t *testing.T) {
	fe := newFakeTokenEndpoint()
	defer fe.close()
	path := tokenPath(t)
	_ = writeCache(path, testSession(&oauth2.Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-1",
		Expiry:       time.Now().Add(-time.Minute),
		TokenType:    "Bearer",
	}))

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	toks := make([]*oauth2.Token, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := newPersistingTokenSource(context.Background(), fe.config(), testBinding, path)
			toks[i], errs[i] = ts.Token()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	// Exactly one refresh should have happened (single rotation).
	if fe.issued != 1 {
		t.Errorf("token endpoint issued %d times, want exactly 1 (lock should serialize + dedup)", fe.issued)
	}
	for i, tok := range toks {
		if tok != nil && tok.AccessToken != "at-refreshed" {
			t.Errorf("goroutine %d got %q, want at-refreshed", i, tok.AccessToken)
		}
	}
}

// The tests below race a logout or a login against a refresh in flight in
// another invocation. The refresher holds the cache lock across the
// token-endpoint round trip and then writes the rotated token; a logout or
// login that changes the cache without taking the lock lands inside that
// window and is then overwritten by the refresh of the old session.

// gatedProvider is a fake OIDC provider. Refresh-token grants are held until
// the test releases them and then rotate the old session; device-code grants
// (the interactive login Bearer falls back to) issue a new session.
type gatedProvider struct {
	srv             *httptest.Server
	refreshInFlight chan struct{} // receives when a refresh reaches the endpoint
	loginIssued     chan struct{} // receives when a login's token is served
	release         func()        // lets held refreshes answer
}

func newGatedProvider(t *testing.T) *gatedProvider {
	t.Helper()
	p := &gatedProvider{
		refreshInFlight: make(chan struct{}, 1),
		loginIssued:     make(chan struct{}, 1),
	}
	released := make(chan struct{})
	var once sync.Once
	p.release = func() { once.Do(func() { close(released) }) }
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	// Signals never block a handler, so an unexpected extra request cannot
	// wedge the server's shutdown.
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                        p.srv.URL,
			"authorization_endpoint":        p.srv.URL + "/authorize",
			"token_endpoint":                p.srv.URL + "/token",
			"device_authorization_endpoint": p.srv.URL + "/device",
			"jwks_uri":                      p.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"device_code": "dc", "user_code": "UC", "verification_uri": p.srv.URL + "/verify",
			"expires_in": 600, "interval": 1,
		})
	})
	mux.HandleFunc("/verify", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("enter your code"))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") == "refresh_token" {
			signal(p.refreshInFlight)
			<-released
			writeJSON(w, map[string]any{
				"access_token": "old-session-at2", "refresh_token": "old-session-rt2",
				"token_type": "Bearer", "expires_in": 3600,
			})
			return
		}
		writeJSON(w, map[string]any{
			"access_token": "new-session-at", "refresh_token": "new-session-rt",
			"token_type": "Bearer", "expires_in": 3600,
		})
		signal(p.loginIssued)
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(func() { p.release(); p.srv.Close() })
	return p
}

// startRefresh seeds path with an expired old session and refreshes it as
// another invocation would, returning once the refresh request has reached
// the token endpoint, i.e. while the refresher holds the cache lock.
func (p *gatedProvider) startRefresh(t *testing.T, path string) <-chan error {
	t.Helper()
	if err := writeCache(path, testSession(&oauth2.Token{
		AccessToken: "old-session-at1", RefreshToken: "old-session-rt1",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute),
	})); err != nil {
		t.Fatal(err)
	}
	oc := &oauth2.Config{ClientID: "cid", Endpoint: oauth2.Endpoint{TokenURL: p.srv.URL + "/token"}}
	done := make(chan error, 1)
	go func() {
		_, err := newPersistingTokenSource(context.Background(), oc, testBinding, path).Token()
		done <- err
	}()
	select {
	case <-p.refreshInFlight:
	case err := <-done:
		t.Fatalf("refresh finished before reaching the token endpoint: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("refresh never reached the token endpoint")
	}
	return done
}

// duringRefresh runs op while the refresh from startRefresh is held, then
// releases the refresh and waits for both. Once ready fires (nil: at once), op
// gets a settle period to finish before the release: an op that writes without
// the lock finishes well inside it, and one that waits for the lock is still
// blocked when it ends. It returns op's error.
func (p *gatedProvider) duringRefresh(t *testing.T, refreshDone <-chan error, ready <-chan struct{}, op func() error) error {
	t.Helper()
	opDone := make(chan error, 1)
	go func() { opDone <- op() }()
	if ready != nil {
		select {
		case <-ready:
		case err := <-opDone:
			t.Fatalf("operation finished without reaching its write: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("operation never reached its write")
		}
	}
	var opErr error
	opFinished := false
	select {
	case opErr = <-opDone:
		opFinished = true
	case <-time.After(200 * time.Millisecond):
	}
	p.release()
	if err := <-refreshDone; err != nil {
		t.Fatalf("in-flight refresh: %v", err)
	}
	if !opFinished {
		opErr = <-opDone
	}
	return opErr
}

// TestLogout_WaitsForInFlightRefresh: `slagctl auth logout` while another
// invocation is refreshing. Logout must not report success and then have the
// refresh write the (freshly rotated) session back.
func TestLogout_WaitsForInFlightRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := Config{AppName: "slagctl"}
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		t.Fatal(err)
	}
	p := newGatedProvider(t)
	refreshDone := p.startRefresh(t, path)

	if err := p.duringRefresh(t, refreshDone, nil, func() error { return Logout(context.Background(), cfg) }); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if onDisk, err := readCache(path); err == nil {
		t.Fatalf("token cache is back after logout: refresh token %q is on disk", onDisk.Token.RefreshToken)
	}
}

// TestWriteCachedToken_WaitsForInFlightRefresh: `slagctl auth login` while
// another invocation is refreshing the previous session. The new login must
// be what is cached afterwards, not the refreshed old session.
func TestWriteCachedToken_WaitsForInFlightRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := Config{AppName: "slagctl"}
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		t.Fatal(err)
	}
	p := newGatedProvider(t)
	refreshDone := p.startRefresh(t, path)

	login := &oauth2.Token{
		AccessToken: "new-session-at", RefreshToken: "new-session-rt",
		TokenType: "Bearer", Expiry: time.Now().Add(time.Hour),
	}
	if err := p.duringRefresh(t, refreshDone, nil, func() error { return WriteCachedToken(context.Background(), cfg, login) }); err != nil {
		t.Fatalf("WriteCachedToken: %v", err)
	}
	onDisk, err := readCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Token.RefreshToken != "new-session-rt" {
		t.Fatalf("after login the cache holds %q, want the new login's new-session-rt", onDisk.Token.RefreshToken)
	}
}

// TestBearer_LoginWaitsForInFlightRefresh: a command whose Bearer found no
// usable token runs the interactive login and caches the result while
// another invocation's refresh of the previous session is in flight. The new
// login must be what is cached afterwards.
func TestBearer_LoginWaitsForInFlightRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := newGatedProvider(t)
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: p.srv.URL, ClientID: "cid", Device: true, Stderr: io.Discard}
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		t.Fatal(err)
	}
	refreshDone := p.startRefresh(t, path)
	// The refresher already read the old session under the lock. Remove it so
	// Bearer finds no usable token and falls back to the interactive login;
	// what matters here is where Bearer's write of that login lands.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	var bearer string
	if err := p.duringRefresh(t, refreshDone, p.loginIssued, func() error {
		var err error
		bearer, err = Bearer(context.Background(), cfg)
		return err
	}); err != nil {
		t.Fatalf("Bearer: %v", err)
	}
	if bearer != "new-session-at" {
		t.Errorf("Bearer returned %q, want the new login's new-session-at", bearer)
	}
	onDisk, err := readCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.Token.RefreshToken != "new-session-rt" {
		t.Fatalf("after Bearer's login the cache holds %q, want the new login's new-session-rt", onDisk.Token.RefreshToken)
	}
	if onDisk.binding != cfg.binding() {
		t.Fatalf("Bearer's login is cached as issued for %+v, want %+v", onDisk.binding, cfg.binding())
	}
}

// TestBearer_RefreshNeverReturnsAnotherServersSession: a command against
// server A finds A's access token expired and, while it runs OIDC discovery
// before taking the cache lock to refresh, `auth login --api B` completes in
// another invocation. Under the lock the cache holds B's fresh session. The
// command must not hand B's token to A, refresh it with A's provider, or
// replace it with a login of its own.
func TestBearer_RefreshNeverReturnsAnotherServersSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var cfgA, cfgB Config
	var mu sync.Mutex
	var refreshes []string // refresh tokens A's provider received
	idpA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			// B's login lands between Bearer's first read and its lock.
			if err := WriteCachedToken(context.Background(), cfgB, &oauth2.Token{
				AccessToken: "B-at-1", RefreshToken: "B-rt-1", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour),
			}); err != nil {
				t.Errorf("B's login: %v", err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": cfgA.Issuer, "authorization_endpoint": cfgA.Issuer + "/authorize",
				"token_endpoint": cfgA.Issuer + "/token", "jwks_uri": cfgA.Issuer + "/jwks",
				"device_authorization_endpoint": cfgA.Issuer + "/device",
			})
		case "/token":
			_ = r.ParseForm()
			mu.Lock()
			refreshes = append(refreshes, r.Form.Get("refresh_token"))
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "A-at-2", "refresh_token": "A-rt-2", "token_type": "Bearer", "expires_in": 600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idpA.Close)
	cfgA = Config{AppName: "slagctl", API: "https://a.example", Issuer: idpA.URL, ClientID: "slagctl", Device: true, Stderr: io.Discard}
	cfgB = Config{AppName: "slagctl", API: "https://b.example", Issuer: "https://idp-b.invalid/", ClientID: "slagctl", Device: true}
	if err := WriteCachedToken(context.Background(), cfgA, &oauth2.Token{
		AccessToken: "A-at-1", RefreshToken: "A-rt-1", TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := Bearer(ctx, cfgA)
	if got == "B-at-1" {
		t.Fatal("Bearer for server A returned server B's access token")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, rt := range refreshes {
		if rt == "B-rt-1" {
			t.Fatal("server A's identity provider received server B's refresh token")
		}
	}
	if err == nil {
		t.Fatalf("Bearer for server A returned %q with only B's session cached, want an error", got)
	}
	path, err := defaultTokenPath(cfgA.AppName)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := readCache(path)
	if err != nil || onDisk.Token.AccessToken != "B-at-1" {
		t.Fatalf("B's login was replaced: cache %+v err %v", onDisk, err)
	}
}

// TestBearer_RefreshesTokenIssuedWithoutExpiresIn: RFC 6749 section 5.1 makes
// expires_in optional. A login or refresh whose response omitted it leaves
// oauth2.Token.Expiry zero, which oauth2 reads as "never expires". The access
// token is a JWT whose exp the API enforces, so once that passes both entry
// points must refresh from the cached refresh token instead of handing out a
// token every API call rejects until the operator logs in again.
func TestBearer_RefreshesTokenIssuedWithoutExpiresIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bearer func(context.Context, Config) (string, error)
	}{
		{"Bearer", Bearer},
		{"CachedBearer", CachedBearer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			expired := mkJWT(t, map[string]any{"sub": "op", "exp": time.Now().Add(-time.Minute).Unix()})
			fresh := mkJWT(t, map[string]any{"sub": "op", "exp": time.Now().Add(10 * time.Minute).Unix()})

			// The provider rotates refresh tokens and never sends expires_in.
			// It advertises no device endpoint, so a Bearer that wrongly falls
			// back to an interactive login fails at once instead of waiting.
			var refreshes atomic.Int32
			var idp *httptest.Server
			idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize",
						"token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/jwks",
					})
				case "/token":
					refreshes.Add(1)
					_ = r.ParseForm()
					if r.Form.Get("refresh_token") != "rt-0" {
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fresh, "refresh_token": "rt-1", "token_type": "Bearer"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer idp.Close()

			cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: idp.URL, ClientID: "cid", Device: true, Stderr: io.Discard}
			path, err := defaultTokenPath(cfg.AppName)
			if err != nil {
				t.Fatal(err)
			}
			// What a login without expires_in leaves once the access token's
			// lifetime has passed.
			if err := writeCache(path, session{binding: cfg.binding(), Token: &oauth2.Token{AccessToken: expired, RefreshToken: "rt-0", TokenType: "Bearer"}}); err != nil {
				t.Fatal(err)
			}

			got, err := tc.bearer(context.Background(), cfg)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got == expired {
				t.Fatalf("%s handed out the cached access token whose exp has passed instead of refreshing it", tc.name)
			}
			if got != fresh {
				t.Fatalf("%s returned %q, want the refreshed access token", tc.name, got)
			}

			// The refreshed token also came without expires_in; the next
			// invocation uses it until its own exp, without another refresh.
			got, err = tc.bearer(context.Background(), cfg)
			if err != nil {
				t.Fatalf("second %s: %v", tc.name, err)
			}
			if got != fresh {
				t.Errorf("second %s returned %q, want the refreshed access token", tc.name, got)
			}
			if n := refreshes.Load(); n != 1 {
				t.Errorf("token endpoint called %d times, want 1", n)
			}
		})
	}
}
