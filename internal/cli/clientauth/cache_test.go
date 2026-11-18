package clientauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
