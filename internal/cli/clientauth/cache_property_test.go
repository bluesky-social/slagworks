package clientauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
	"pgregory.net/rapid"
)

// Property-based coverage for the token cache under concurrent CLI
// invocations. A rotating fake provider issues refresh-token families: each
// refresh token may be spent once, and spending a consumed one is a reuse
// (a real provider would revoke the session). Each step runs a batch of
// concurrent Token() calls (separate persistingTokenSource values, each
// opening its own flock, as separate processes would) alongside at most one
// operator action: a login (WriteCachedToken), a logout (Logout), an expiry
// (rewrite the current token expired), or a corruption (garbage or truncated
// file). The provider answers slowly so refreshes stay in flight across the
// operator action.
//
// The oracle is the provider's own ledger, not the cache code. The contracts
// are:
//   - one rotation per expiry: concurrent waiters reuse the rotated token
//   - every token handed out is valid
//   - no refresh token is spent twice while the cache is intact
//   - a logout is durable: no refresh in flight writes the session back
//   - a login is durable: no refresh of the old session overwrites it

type rotProvider struct {
	mu       sync.Mutex
	consumed map[string]bool
	reuses   []string
	issued   int
	delay    time.Duration
	srv      *httptest.Server
}

func newRotProvider(delay time.Duration) *rotProvider {
	p := &rotProvider{consumed: map[string]bool{}, delay: delay}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	return p
}

func (p *rotProvider) handle(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	rt := r.Form.Get("refresh_token")
	time.Sleep(p.delay)
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt == "" || p.consumed[rt] {
		p.reuses = append(p.reuses, rt)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	p.consumed[rt] = true
	p.issued++
	family := strings.SplitN(rt, "/", 2)[0]
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  fmt.Sprintf("%s/at%d", family, p.issued),
		"refresh_token": fmt.Sprintf("%s/rt%d", family, p.issued),
		"token_type":    "Bearer",
		"expires_in":    3600,
	})
}

func (p *rotProvider) config() *oauth2.Config {
	return &oauth2.Config{ClientID: "c", Endpoint: oauth2.Endpoint{TokenURL: p.srv.URL + "/token"}}
}

type cacheStep struct {
	refreshers int
	action     string // "", login, logout, expire, corrupt, truncate
	actionLag  time.Duration
}

// TestTokenCache_Property_ConcurrentInvocations runs one rapid.Check over
// random step sequences, asserting the contracts above after every step.
func TestTokenCache_Property_ConcurrentInvocations(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cases := 0
	rapid.Check(t, func(t *rapid.T) {
		cases++
		// Operator actions go through the public entry points (auth login's
		// WriteCachedToken, auth logout's Logout), so the lock they take is
		// the one exercised.
		app := Config{AppName: fmt.Sprintf("tokprop-%d", cases)}
		path, err := defaultTokenPath(app.AppName)
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(filepath.Dir(path))
		p := newRotProvider(time.Duration(rapid.IntRange(0, 15).Draw(t, "delayMs")) * time.Millisecond)
		defer p.srv.Close()

		family := 0
		newFamily := func(expired bool) *oauth2.Token {
			family++
			exp := time.Now().Add(time.Hour)
			if expired {
				exp = time.Now().Add(-time.Minute)
			}
			return &oauth2.Token{AccessToken: fmt.Sprintf("f%d/at0", family), RefreshToken: fmt.Sprintf("f%d/rt0", family),
				TokenType: "Bearer", Expiry: exp}
		}
		if err := writeCache(path, newFamily(true)); err != nil {
			t.Fatal(err)
		}
		externallyBroken := false // a corruption/truncation makes later refresh errors legitimate

		steps := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) cacheStep {
			return cacheStep{
				refreshers: rapid.IntRange(0, 4).Draw(t, "refreshers"),
				action:     rapid.SampledFrom([]string{"", "", "expire", "expire", "login", "logout", "corrupt", "truncate"}).Draw(t, "action"),
				actionLag:  time.Duration(rapid.IntRange(0, 10).Draw(t, "lagMs")) * time.Millisecond,
			}
		}), 1, 8).Draw(t, "steps")

		for si, st := range steps {
			var wg sync.WaitGroup
			type res struct {
				tok *oauth2.Token
				err error
			}
			results := make([]res, st.refreshers)
			brokenAtStart := externallyBroken
			expiredAtStart := false
			if cur, err := readCache(path); err == nil && !cur.Valid() {
				expiredAtStart = true
			}
			p.mu.Lock()
			issuedBefore := p.issued
			p.mu.Unlock()
			for i := range st.refreshers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					tok, err := newPersistingTokenSource(ctx, p.srv.URL, p.config(), path).Token()
					results[i] = res{tok, err}
				}(i)
			}
			var loginTok *oauth2.Token
			if st.action != "" {
				time.Sleep(st.actionLag)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				switch st.action {
				case "login":
					loginTok = newFamily(false)
					if err := WriteCachedToken(ctx, app, loginTok); err != nil {
						cancel()
						t.Fatal(err)
					}
					externallyBroken = false
				case "logout":
					if err := Logout(ctx, app); err != nil {
						cancel()
						t.Fatal(err)
					}
				case "expire":
					// Simulated passage of time: rewrite the current token as
					// expired atomically with respect to refreshers (under the
					// same lock), since real expiry is not a racing writer.
					lk := flock.New(path + ".lock")
					if err := lk.Lock(); err != nil {
						cancel()
						t.Fatal(err)
					}
					if cur, err := readCache(path); err == nil {
						cur.Expiry = time.Now().Add(-time.Minute)
						_ = writeCache(path, cur)
					}
					_ = lk.Unlock()
				case "corrupt":
					_ = os.WriteFile(path, []byte("{not json"), 0o600)
					externallyBroken = true
				case "truncate":
					if b, err := os.ReadFile(path); err == nil && len(b) > 2 {
						_ = os.WriteFile(path, b[:len(b)/2], 0o600)
						externallyBroken = true
					}
				}
				cancel()
			}
			wg.Wait()
			// One rotation per expiry: at most one refresh if the token on
			// disk at step start was expired, plus one for an expiry injected
			// during the step.
			expiries := 0
			if expiredAtStart {
				expiries++
			}
			if st.action == "expire" {
				expiries++
			}
			p.mu.Lock()
			issuedDelta := p.issued - issuedBefore
			p.mu.Unlock()
			if issuedDelta > expiries {
				t.Fatalf("step %d (%+v): %d refreshes for %d expiries; waiters did not reuse the rotated token", si, st, issuedDelta, expiries)
			}
			// Every refresher gets a valid token unless the cache was broken
			// or deleted under it: a corrupt or truncated file, or a logout
			// that landed first.
			mayFail := brokenAtStart || externallyBroken || st.action == "logout"
			for i, r := range results {
				if r.err != nil {
					if !mayFail {
						t.Fatalf("step %d (%+v) refresher %d: token error with the cache intact: %v", si, st, i, r.err)
					}
					continue
				}
				if !r.tok.Valid() {
					t.Fatalf("step %d refresher %d: returned an invalid/expired token %+v", si, i, r.tok)
				}
			}
			// No refresh token is ever spent twice while the cache is intact.
			p.mu.Lock()
			reuses := append([]string(nil), p.reuses...)
			p.mu.Unlock()
			if len(reuses) > 0 && !externallyBroken {
				t.Fatalf("step %d (%+v): refresh token(s) replayed: %v", si, st, reuses)
			}
			// A logout is durable: after the step settles, no token is on disk.
			if st.action == "logout" {
				if _, err := os.Stat(path); err == nil {
					onDisk, _ := readCache(path)
					t.Fatalf("step %d: token cache exists after logout (concurrent refresh wrote it back): %+v", si, onDisk)
				}
			}
			// A login is durable: its family is not overwritten by a refresh
			// of the old session.
			if loginTok != nil {
				onDisk, err := readCache(path)
				if err != nil {
					t.Fatalf("step %d: cache unreadable after login: %v", si, err)
				}
				want := strings.SplitN(loginTok.RefreshToken, "/", 2)[0]
				if got := strings.SplitN(onDisk.RefreshToken, "/", 2)[0]; got != want {
					t.Fatalf("step %d: after login the cache holds session %q, want the new login's %q", si, got, want)
				}
			}
			// Re-seed so later steps have something to work with.
			if _, err := os.Stat(path); err != nil {
				_ = writeCache(path, newFamily(rapid.Bool().Draw(t, fmt.Sprintf("reseedExpired%d", si))))
				externallyBroken = false
			}
		}
	})
}
