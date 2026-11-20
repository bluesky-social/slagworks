package clientauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/cli/clientauth/authfake"
)

// Property-based coverage for the client token lifecycle as slagctl uses it
// for one request: CachedBearer (OIDC discovery, the cached session, the
// locked refresh) hands the API a bearer, and when the API refuses it,
// RefreshRejected replaces it. This extends cache_property_test.go, which
// drives persistingTokenSource against a bare token endpoint, with the parts
// that file leaves out: discovery, a virtual clock that crosses token
// lifetimes (a laptop that slept), a server clock ahead of or behind the
// client's, a server that retires the key the cached token was signed with,
// a provider that is down or slow, a revoked session, a provider
// that omits expires_in, a login to another server landing in the shared
// cache, and the API's verdict on every token handed out.
//
// Each step runs a batch of concurrent invocations (each takes its own flock,
// as separate processes would) alongside at most one operator or world
// action. The reference model is a few fields (is there a session and for
// which server, how old is its access token, is it revoked, is the provider
// reachable, how far ahead is the server's clock, has it retired the token's
// key); it never calls the code under test.
//
// Contracts:
//   - an invocation that ends with a token ends with one the API accepts
//   - with a usable session (an access token both sides accept, or a valid
//     refresh token and a reachable provider) an invocation succeeds
//   - a failure matches ErrLoginRequired exactly when no retry can clear it:
//     no session, a session for another server, a refused refresh token
//   - a session for another server is never handed out, and its refresh
//     token never reaches this server's identity provider
//   - no refresh token is presented twice
//   - logout and login are durable against refreshes in flight
//   - the cache is never unparseable or readable by others

type lcAction struct {
	kind  string
	d     time.Duration
	skew  time.Duration
	lagMs int
}

type lcStep struct {
	invocations int
	action      lcAction
}

// lcModel is the reference state. The age of the cached access token is the
// virtual time added since it was issued plus the real time elapsed.
type lcModel struct {
	hasCache bool
	other    bool // the cached session is for another server
	revoked  bool
	down     bool
	retired  bool   // the API retired the key that signed the cached access token
	rt       string // the cached refresh token, to notice a rotation
	born     time.Time
	virt     time.Duration
	skew     time.Duration
	omitNext bool
	lifetime time.Duration
}

// lcMargin keeps the model's verdicts away from the instants where the token
// expires for the client or for the server, which the real seconds a step
// takes and the whole-second exp claim blur.
const lcMargin = 3 * time.Second

func (m *lcModel) age() time.Duration { return m.virt + time.Since(m.born) }

// trusted and untrusted: the client takes the access token as unexpired
// (oauth2 treats a token as expired 10s early), or certainly does not.
func (m *lcModel) trusted() bool   { return m.age() < m.lifetime-10*time.Second-lcMargin }
func (m *lcModel) untrusted() bool { return m.age() > m.lifetime-10*time.Second+lcMargin }

// accepted and rejected: the API, whose clock runs skew ahead, takes the
// access token, or certainly does not.
func (m *lcModel) accepted() bool { return !m.retired && m.age()+m.skew < m.lifetime-lcMargin }
func (m *lcModel) rejected() bool { return m.retired || m.age()+m.skew > m.lifetime+lcMargin }

func (m *lcModel) session() bool { return m.hasCache && !m.other }

func (m *lcModel) usable() bool {
	return m.session() && ((!m.revoked && !m.down) || (m.trusted() && m.accepted()))
}

// needsRefresh: the invocation cannot end without asking the provider.
func (m *lcModel) needsRefresh() bool { return m.untrusted() || (m.trusted() && m.rejected()) }

func genLCStep() *rapid.Generator[lcStep] {
	kinds := []string{"", "", "advance", "advance", "advance", "advance", "advance", "advance", "login", "login", "login",
		"login-other", "logout", "revoke", "revoke", "revoke", "down", "down", "up", "up", "slow", "skew", "skew", "retire", "retire", "omit"}
	return rapid.Custom(func(t *rapid.T) lcStep {
		st := lcStep{invocations: rapid.IntRange(0, 4).Draw(t, "invocations")}
		st.action.kind = rapid.SampledFrom(kinds).Draw(t, "action")
		st.action.lagMs = rapid.IntRange(0, 10).Draw(t, "lagMs")
		switch st.action.kind {
		case "advance":
			st.action.d = rapid.SampledFrom([]time.Duration{
				time.Minute, 5 * time.Minute, 8*time.Minute + 30*time.Second, 9*time.Minute + 40*time.Second,
				9*time.Minute + 40*time.Second, 9*time.Minute + 55*time.Second, 10 * time.Minute, 11 * time.Minute, time.Hour,
			}).Draw(t, "d")
		case "skew":
			st.action.skew = rapid.SampledFrom([]time.Duration{
				0, 5 * time.Second, 30 * time.Second, 30 * time.Second, 2 * time.Minute, 2 * time.Minute, -30 * time.Second, -5 * time.Minute,
			}).Draw(t, "skew")
		}
		return st
	})
}

type lcStats struct {
	steps, invocations, ok, refreshes, afterReject, loginRequired, transient, otherServer, noExpiry int
}

func TestTokenLifecycle_Property(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var stats lcStats
	cases := 0
	rapid.Check(t, func(t *rapid.T) {
		cases++
		clock, reg := &authfake.Clock{}, authfake.NewRegistry()
		lifetime := 10 * time.Minute
		prov := authfake.NewProvider(clock, reg, lifetime)
		// A slow token endpoint keeps refreshes in flight across the step's
		// operator action (logout and login must win over them).
		prov.SetDelay(time.Duration(rapid.IntRange(0, 20).Draw(t, "delayMs")) * time.Millisecond)
		defer prov.Close()
		api := authfake.NewAPI(clock, reg, prov.Issuer())
		defer api.Close()
		// The other server the operator also signs in to, with its own provider.
		otherProv := authfake.NewProvider(clock, reg, lifetime)
		defer otherProv.Close()

		cfg := Config{AppName: fmt.Sprintf("lcprop-%d", cases), API: api.URL(), Issuer: prov.Issuer(), ClientID: "c", LoopbackPort: 1}
		otherCfg := Config{AppName: cfg.AppName, API: "https://other.example", Issuer: otherProv.Issuer(), ClientID: "c", LoopbackPort: 1}
		path, err := defaultTokenPath(cfg.AppName)
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(filepath.Dir(path))

		m := &lcModel{lifetime: lifetime}
		loginRT := ""
		login := func(ctx context.Context, to Config, p *authfake.Provider) {
			tok := p.Mint()
			loginRT = tok.RefreshToken
			if m.omitNext {
				tok.Expiry = time.Time{} // what oauth2 records without expires_in
			}
			if err := WriteCachedToken(ctx, to, tok); err != nil {
				t.Fatal(err)
			}
			m.hasCache, m.other, m.revoked, m.retired = true, to.API != cfg.API, false, false
			m.rt, m.born, m.virt = tok.RefreshToken, time.Now(), 0
		}
		if rapid.IntRange(0, 9).Draw(t, "startLoggedIn") > 0 {
			login(context.Background(), cfg, prov)
		}

		// invoke is what slagctl does for one request: fetch the bearer and,
		// when the API answers it with 401, ask once for a replacement.
		type res struct {
			tok     string
			err     error
			retried bool
			verdict string // why the API refused the token the invocation ended with
		}
		invoke := func() res {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			tok, err := CachedBearer(ctx, cfg)
			if err != nil {
				return res{err: err}
			}
			if ok, _ := api.Check(tok); ok {
				return res{tok: tok}
			}
			if tok, err = RefreshRejected(ctx, cfg, tok); err != nil {
				return res{err: err, retried: true}
			}
			_, why := api.Check(tok)
			return res{tok: tok, retried: true, verdict: why}
		}

		steps := rapid.SliceOfN(genLCStep(), 1, 12).Draw(t, "steps")
		for si, st := range steps {
			stats.steps++
			a := st.action
			// What must hold for a batch nothing races: the verdicts the model
			// gives now still stand when the invocations run.
			usable := m.usable()
			mustSignIn := !m.session() || (m.revoked && !m.down && m.needsRefresh())
			mustBeTransient := m.session() && m.down && m.needsRefresh()

			results := make([]res, st.invocations)
			var wg sync.WaitGroup
			for i := range st.invocations {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i] = invoke()
				}(i)
			}
			if a.kind != "" {
				time.Sleep(time.Duration(a.lagMs) * time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			// Actions that change what an invocation already in flight may
			// legitimately see. Advancing the clock is applied after the
			// batch instead (real expiry is not a racing writer).
			racy := false
			switch a.kind {
			case "login":
				login(ctx, cfg, prov)
				racy = true
			case "login-other":
				login(ctx, otherCfg, otherProv)
				racy = true
			case "logout":
				if err := Logout(ctx, cfg); err != nil {
					cancel()
					t.Fatal(err)
				}
				m.hasCache = false
				racy = true
			case "revoke":
				if m.session() {
					if fam, _, ok := authfake.ParseGen(m.rt); ok {
						prov.Revoke(fam)
						m.revoked = true
					}
				}
				racy = true
			case "down":
				prov.SetDown(true)
				m.down = true
				racy = true
			case "up":
				prov.SetDown(false)
				m.down = false
				racy = true
			case "slow":
				prov.SetDelay(time.Duration(rapid.IntRange(0, 30).Draw(t, fmt.Sprintf("slowMs%d", si))) * time.Millisecond)
			case "skew":
				api.SetSkew(a.skew)
				m.skew = a.skew
				racy = true
			case "retire":
				api.RotateKeys()
				m.retired = m.session()
				racy = true
			case "omit":
				m.omitNext = !m.omitNext
				prov.SetOmitExpiresIn(m.omitNext)
			}
			cancel()
			wg.Wait()

			for i, r := range results {
				stats.invocations++
				if m.other {
					stats.otherServer++
				}
				if r.err != nil {
					signIn := errors.Is(r.err, ErrLoginRequired)
					if signIn {
						stats.loginRequired++
					} else {
						stats.transient++
					}
					if racy {
						continue
					}
					if usable {
						t.Fatalf("step %d (%+v) invocation %d: failed with a usable session (model %+v, age %s): %v", si, st, i, *m, m.age(), r.err)
					}
					if mustSignIn && !signIn {
						t.Fatalf("step %d (%+v) invocation %d: only a new login clears this, but the error does not match ErrLoginRequired (model %+v): %v", si, st, i, *m, r.err)
					}
					if mustBeTransient && signIn {
						t.Fatalf("step %d (%+v) invocation %d: the provider is down, which a retry may clear, but the error matches ErrLoginRequired (model %+v): %v", si, st, i, *m, r.err)
					}
					continue
				}
				stats.ok++
				if r.retried {
					stats.afterReject++
				}
				// A key retired while the batch runs can land between an
				// invocation's refresh and the API's verdict on its result.
				if r.verdict != "" && a.kind != "retire" {
					t.Fatalf("step %d (%+v) invocation %d: ended with a token the API refuses (%s) after one refresh; model %+v, age %s", si, st, i, r.verdict, *m, m.age())
				}
				if !racy && !m.session() {
					t.Fatalf("step %d (%+v) invocation %d: got a token without a session for this server (model %+v)", si, st, i, *m)
				}
			}

			// Bring the model up to the session the step left in the cache: a
			// rotation there is a new access token.
			cur, rerr := readCache(path)
			if rerr == nil && cur.Token.RefreshToken != m.rt {
				stats.refreshes++
				m.rt, m.born, m.virt, m.retired = cur.Token.RefreshToken, time.Now(), 0, false
			}
			if a.kind == "retire" && rerr == nil && m.session() {
				// A refresh in flight when the key was retired may have
				// landed a token signed before or after it; ask the API.
				_, why := api.Check(cur.Token.AccessToken)
				m.retired = why == "signed with a retired key"
			}
			if rerr == nil && cur.Token.Expiry.IsZero() {
				t.Fatalf("step %d: the cached session reads back with no expiry although its access token carries one", si)
			}
			if a.kind == "advance" {
				clock.Advance(a.d)
				shiftExpiry(t, reg, path, a.d)
				m.virt += a.d
			}
			if m.omitNext {
				stats.noExpiry++
			}

			if l := api.Leaks(); len(l) > 0 {
				t.Fatalf("step %d (%+v): an invocation for this server ended with %d token(s) issued for the other", si, st, len(l))
			}
			if f := prov.Foreign(); len(f) > 0 {
				t.Fatalf("step %d (%+v): this server's provider was sent the other server's refresh token(s): %v", si, st, f)
			}
			if r := append(prov.Reuses(), otherProv.Reuses()...); len(r) > 0 {
				t.Fatalf("step %d (%+v): refresh token(s) presented twice: %v", si, st, r)
			}
			if n := otherProv.Refreshes(); n > 0 {
				t.Fatalf("step %d (%+v): invocations for this server refreshed the other server's session %d time(s)", si, st, n)
			}
			fi, err := os.Stat(path)
			if a.kind == "logout" && err == nil {
				t.Fatalf("step %d: token cache present after logout", si)
			}
			if a.kind == "login" || a.kind == "login-other" {
				// A login is durable: no refresh of the old session in
				// flight overwrites it, and it stays bound to its server.
				want := cfg
				if a.kind == "login-other" {
					want = otherCfg
				}
				if rerr != nil {
					t.Fatalf("step %d: cache unreadable after login: %v", si, rerr)
				}
				got, _, _ := authfake.ParseGen(cur.Token.RefreshToken)
				if fam, _, _ := authfake.ParseGen(loginRT); got != fam || cur.binding != want.binding() {
					t.Fatalf("step %d: after a login to %s the cache holds session %d for %+v, want the login's %d", si, want.API, got, cur.binding, fam)
				}
			}
			id, iderr := CachedIdentity(cfg)
			if err != nil {
				if iderr == nil {
					t.Fatalf("step %d: CachedIdentity reports %+v with no token cache", si, id)
				}
				continue
			}
			if fi.Mode().Perm()&0o077 != 0 {
				t.Fatalf("step %d: token cache mode %v", si, fi.Mode().Perm())
			}
			if rerr != nil {
				t.Fatalf("step %d: token cache unparseable: %v", si, rerr)
			}
			if iderr != nil || !id.Refreshable || id.PreferredUsername != "operator" {
				t.Fatalf("step %d: CachedIdentity of a cached session = %+v, %v", si, id, iderr)
			}
		}
	})
	t.Logf("cases=%d steps=%d invocations=%d ok=%d (after a 401: %d) rotations=%d loginRequired=%d transient=%d otherServer=%d noExpiry=%d",
		cases, stats.steps, stats.invocations, stats.ok, stats.afterReject, stats.refreshes, stats.loginRequired, stats.transient, stats.otherServer, stats.noExpiry)
}

// shiftExpiry is the client half of advancing the virtual clock: under the
// cache lock, move the cached expiry back by d (when the provider sent one)
// and the access token's own exp claim with it.
func shiftExpiry(t *rapid.T, reg *authfake.Registry, path string, d time.Duration) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	lk := flock.New(path + ".lock")
	if err := lk.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()
	// Not readCache: that fills a missing expiry in from the access token,
	// and the cache must keep the token as the provider sent it.
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var s session
	if json.Unmarshal(b, &s) != nil || s.Token == nil {
		return
	}
	if !s.Token.Expiry.IsZero() {
		s.Token.Expiry = s.Token.Expiry.Add(-d)
	}
	s.Token.AccessToken = reg.Shift(s.Token.AccessToken, d)
	if err := writeCache(path, s); err != nil {
		t.Fatal(err)
	}
}
