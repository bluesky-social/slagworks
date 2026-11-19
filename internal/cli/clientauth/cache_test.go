package clientauth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
)

func tokenPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "slagctl", "token.json")
}

func TestCacheRoundTripAndPerms(t *testing.T) {
	path := tokenPath(t)
	tok := &oauth2.Token{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		Expiry:       time.Now().Add(time.Hour),
		TokenType:    "Bearer",
	}
	if err := writeCache(path, tok); err != nil {
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
	if got.AccessToken != "at-1" || got.RefreshToken != "rt-1" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestReadCacheMissing(t *testing.T) {
	if _, err := readCache(tokenPath(t)); err == nil {
		t.Error("expected error reading nonexistent cache")
	}
}

func TestDeleteCache(t *testing.T) {
	path := tokenPath(t)
	_ = writeCache(path, &oauth2.Token{AccessToken: "x", Expiry: time.Now().Add(time.Hour)})
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
	if err := writeCache(path, &oauth2.Token{AccessToken: "x", Expiry: time.Now().Add(time.Hour)}); err != nil {
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
	if err := writeCache(path, expired); err != nil {
		t.Fatal(err)
	}

	ts := newPersistingTokenSource(context.Background(), fe.config(), path)
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if tok.AccessToken != "at-refreshed" {
		t.Errorf("access token = %q, want at-refreshed", tok.AccessToken)
	}
	// the rotated refresh token must be persisted
	onDisk, err := readCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk.RefreshToken != "rt-1-next" {
		t.Errorf("on-disk refresh token = %q, want rt-1-next (rotation not persisted)", onDisk.RefreshToken)
	}
	if onDisk.AccessToken != "at-refreshed" {
		t.Errorf("on-disk access token = %q, want at-refreshed", onDisk.AccessToken)
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
	_ = writeCache(path, valid)

	ts := newPersistingTokenSource(context.Background(), fe.config(), path)
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
	_ = writeCache(path, expired)

	ts := newPersistingTokenSource(context.Background(), fe.config(), path)
	if _, err := ts.Token(); err == nil {
		t.Fatal("expected refresh error for a consumed refresh token")
	}
	// cache must be untouched (still the old token, not corrupted)
	onDisk, err := readCache(path)
	if err != nil {
		t.Fatalf("cache should still be readable: %v", err)
	}
	if onDisk.RefreshToken != "rt-dead" || onDisk.AccessToken != "at-old" {
		t.Errorf("cache was modified on failed refresh: %+v", onDisk)
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
	_ = writeCache(path, &oauth2.Token{
		AccessToken:  "at-old",
		RefreshToken: "rt-1",
		Expiry:       time.Now().Add(-time.Minute),
		TokenType:    "Bearer",
	})

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	toks := make([]*oauth2.Token, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := newPersistingTokenSource(context.Background(), fe.config(), path)
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
	if err := writeCache(path, &oauth2.Token{
		AccessToken: "old-session-at1", RefreshToken: "old-session-rt1",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	oc := &oauth2.Config{ClientID: "cid", Endpoint: oauth2.Endpoint{TokenURL: p.srv.URL + "/token"}}
	done := make(chan error, 1)
	go func() {
		_, err := newPersistingTokenSource(context.Background(), oc, path).Token()
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
		t.Fatalf("token cache is back after logout: refresh token %q is on disk", onDisk.RefreshToken)
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
	if onDisk.RefreshToken != "new-session-rt" {
		t.Fatalf("after login the cache holds %q, want the new login's new-session-rt", onDisk.RefreshToken)
	}
}

// TestBearer_LoginWaitsForInFlightRefresh: a command whose Bearer found no
// usable token runs the interactive login and caches the result while
// another invocation's refresh of the previous session is in flight. The new
// login must be what is cached afterwards.
func TestBearer_LoginWaitsForInFlightRefresh(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := newGatedProvider(t)
	cfg := Config{AppName: "slagctl", Issuer: p.srv.URL, ClientID: "cid", Device: true, Stderr: io.Discard}
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
	if onDisk.RefreshToken != "new-session-rt" {
		t.Fatalf("after Bearer's login the cache holds %q, want the new login's new-session-rt", onDisk.RefreshToken)
	}
}
