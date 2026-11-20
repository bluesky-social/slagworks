package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth/authfake"
	"github.com/bluesky-social/slagworks/internal/cli/userpath"
)

// Property-based coverage for slagctl's authentication lifecycle, driven
// through the real root command (PersistentPreRunE, authConfig, Bearer, the
// bearer source and refresh-on-401 of the REST client, and the WebSocket
// streams) against two fake slagworks servers, A and B, each with its own
// fake OIDC provider. All four share one virtual clock (authfake), so a
// history crosses many access-token lifetimes in milliseconds. The
// access-token lifetime is ten minutes.
//
// Operations (small pools so they interact): log in to A or B (`auth login
// --device`, or the same two writes done directly), log out, run a short
// command against A, B or the saved default, `auth whoami`, advance the
// clock (a laptop that slept), revoke the session at the provider, retire
// the keys a server accepts (it then refuses tokens the client still
// trusts), take a provider down or bring it back, leave the cache as an
// older release wrote it
// or corrupt, land an interactive login for one server after a login to the
// other (in half the cases the stub browser completes interactive logins,
// in the rest the operator gives up on them), a long `operation watch
// --plain` during which the clock advances
// past expiry, the stream drops, other invocations refresh the shared cache
// and the operation completes, and a `machine logs -f` whose stream drops.
//
// The model is a handful of fields (which server is saved, which server the
// cached session is for, how old its access token is, is it revoked, has the
// server retired its key, is the provider up), not the production code.
// Contracts checked:
//   - a command against the signed-in server, with a usable session (an
//     access token both sides accept, or a valid refresh token and a
//     reachable provider), succeeds without prompting; in particular it
//     never fails with 401
//   - a watch or a log follow with a usable session survives expiry and
//     stream drops: the watch ends with the operation's outcome, the follow
//     keeps running
//   - a token or refresh token issued for one server never reaches the other
//     server or the other identity provider, and a session cached for one
//     server is never replaced by a command against the other
//   - no refresh token is spent twice (no self-inflicted revocation)
//   - every authentication failure names the command that fixes it
//   - the token cache is never left unparseable or readable by others

// Patterns left out because slagctl does not meet the contract for them yet.
// Each constant names the behaviour; set it to true to see the failure.
const (
	// A stream dial the API answers with 401 while the cache still holds
	// the token as valid is not answered with a refresh, as a request is:
	// `machine logs` ends with the refusal although the refresh token is
	// good.
	genStreamRedialRefused = false
	// `auth whoami` with a token cache that does not parse reports "not
	// logged in (no cached token)", although a file is there and is the
	// problem.
	checkCorruptCacheLabel = false
)

const (
	cliLifetime = 10 * time.Minute
	// cliMargin keeps the model's verdicts away from the instants where the
	// token expires for the client or for the server, which the real seconds
	// an operation takes blur.
	cliMargin = 3 * time.Second
)

// cachedSession is the token cache's file format.
type cachedSession struct {
	API      string        `json:"api"`
	Issuer   string        `json:"issuer"`
	ClientID string        `json:"client_id"`
	Token    *oauth2.Token `json:"token"`
}

type cliWorld struct {
	clock   *authfake.Clock
	reg     *authfake.Registry
	prov    map[string]*authfake.Provider
	api     map[string]*authfake.API
	cfgHome string
	browsed string // file the stub browser appends URLs to
}

// stubBrowser puts `open` and `xdg-open` on PATH: they record the URL
// slagctl asked the browser to show.
func stubBrowser(t testing.TB, dir string) (browsed string) {
	t.Helper()
	browsed = filepath.Join(dir, "browsed")
	script := fmt.Sprintf("#!/bin/sh\necho \"$1\" >> %q\n", browsed)
	for _, n := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(script), 0o755); err != nil { //nolint:gosec // test stub must be executable
			t.Fatal(err)
		}
	}
	return browsed
}

func newCLIWorld(cfgHome, browsed string) *cliWorld {
	w := &cliWorld{clock: &authfake.Clock{}, reg: authfake.NewRegistry(), prov: map[string]*authfake.Provider{},
		api: map[string]*authfake.API{}, cfgHome: cfgHome, browsed: browsed}
	for _, n := range []string{"A", "B"} {
		w.prov[n] = authfake.NewProvider(w.clock, w.reg, cliLifetime)
		w.api[n] = authfake.NewAPI(w.clock, w.reg, w.prov[n].Issuer())
	}
	return w
}

func (w *cliWorld) close() {
	for _, n := range []string{"A", "B"} {
		w.api[n].Close()
		w.prov[n].Close()
	}
}

func (w *cliWorld) tokenPath() string { return filepath.Join(w.cfgHome, appName, "token.json") }

// cfgFor is the sign-in configuration `auth login --api <n>` arrives at.
func (w *cliWorld) cfgFor(n string) clientauth.Config {
	return clientauth.Config{AppName: appName, API: w.api[n].URL(), Issuer: w.prov[n].Issuer(),
		ClientID: w.api[n].ClientID, LoopbackPort: oidcLoopbackPort}
}

// serverOf names the server with the given API URL ("" when neither).
func (w *cliWorld) serverOf(apiURL string) string {
	for n, a := range w.api {
		if a.URL() == apiURL {
			return n
		}
	}
	return ""
}

// runCLI runs one slagctl invocation in-process and returns its output and
// error. Invocations never overlap: the cmd package keeps per-invocation
// state in package variables, so concurrency between invocations is modeled
// at the clientauth layer instead (otherProcess).
func runCLI(ctx context.Context, args ...string) (string, error) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

// session reads the cached session (nil when the file is missing or is not
// one).
func (w *cliWorld) session() *cachedSession {
	b, err := os.ReadFile(w.tokenPath())
	if err != nil {
		return nil
	}
	var s cachedSession
	if json.Unmarshal(b, &s) != nil || s.Token == nil {
		return nil
	}
	return &s
}

func (w *cliWorld) writeCacheFile(t *rapid.T, v any) {
	b, err := json.Marshal(v) // #nosec G117 -- test fixture token
	if err != nil {
		t.Fatal(err)
	}
	if err := userpath.WriteFile(w.tokenPath(), b); err != nil {
		t.Fatal(err)
	}
}

// advance moves the virtual clock forward by d and, as the client half of
// that, moves the cached expiry and the cached access token's exp claim back
// by d under the cache lock.
func (w *cliWorld) advance(t *rapid.T, d time.Duration) {
	w.clock.Advance(d)
	if err := os.MkdirAll(filepath.Dir(w.tokenPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	lk := flock.New(w.tokenPath() + ".lock")
	if err := lk.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()
	s := w.session()
	if s == nil {
		return
	}
	s.Token.Expiry = s.Token.Expiry.Add(-d)
	s.Token.AccessToken = w.reg.Shift(s.Token.AccessToken, d)
	w.writeCacheFile(t, s)
}

// otherProcess is a concurrent slagctl invocation against server n, modeled
// at the clientauth layer with the config the root command builds: it reads
// (and if needed refreshes) the shared cache.
func (w *cliWorld) otherProcess(ctx context.Context, n string) error {
	cfg, err := authConfig(w.api[n].URL())
	if err != nil {
		return err
	}
	_, err = clientauth.CachedBearer(ctx, cfg)
	return err
}

func (w *cliWorld) browsedURLs() []string {
	b, err := os.ReadFile(w.browsed)
	if err != nil {
		return nil
	}
	return strings.Fields(string(b))
}

// cliModel is the reference state: deliberately small and independent of the
// production code.
type cliModel struct {
	saved     string          // "", "A" or "B": the server the last `auth login` saved
	hasCache  bool            // the cache holds a session
	cacheFor  string          // the server that session was issued for
	broken    bool            // the cache file is there but is not a session
	corrupt   bool            // ... because it does not parse
	everIn    map[string]bool // servers logged in to at some point
	revoked   bool            // the cached session was revoked at its provider
	retired   bool            // the server retired the key that signed the cached access token
	rt        string          // the cached refresh token, to notice a rotation
	born      time.Time       // when the cached access token was issued
	virt      time.Duration   // virtual time added since
	down      map[string]bool
	approving bool // the stub browser completes interactive logins
}

func (m *cliModel) age() time.Duration { return m.virt + time.Since(m.born) }

// trusted and untrusted: the client takes the cached access token as
// unexpired (it treats a token as expired 10s early), or certainly does not.
func (m *cliModel) trusted() bool   { return m.age() < cliLifetime-10*time.Second-cliMargin }
func (m *cliModel) untrusted() bool { return m.age() > cliLifetime-10*time.Second+cliMargin }

// accepted and refused: the API takes the cached access token, or certainly
// does not.
func (m *cliModel) accepted() bool { return !m.retired && m.age() < cliLifetime-cliMargin }
func (m *cliModel) refused() bool  { return m.retired || m.age() > cliLifetime+cliMargin }

// signedIn: commands against target use the cached session.
func (m *cliModel) signedIn(target string) bool {
	return target != "" && m.saved == target && m.hasCache && m.cacheFor == target
}

func (m *cliModel) canRefresh() bool { return !m.revoked && !m.down[m.cacheFor] }

func (m *cliModel) usable(target string) bool {
	return m.signedIn(target) && (m.canRefresh() || (m.trusted() && m.accepted()))
}

// unusable: signed in, but certainly without a token the API takes or a way
// to get one. Between usable and unusable lie the seconds around an expiry,
// where the model gives no verdict.
func (m *cliModel) unusable(target string) bool {
	return !m.signedIn(target) || (!m.canRefresh() && (m.untrusted() || m.refused()))
}

// newSession records a login to n.
func (m *cliModel) newSession(n string) {
	m.hasCache, m.cacheFor, m.broken, m.corrupt, m.revoked, m.retired = true, n, false, false, false, false
	m.everIn[n] = true
}

// sync notices a rotated or replaced token in the cache: its access token is
// new, so its age starts over and it is signed with the server's current key.
func (m *cliModel) sync(w *cliWorld) {
	if s := w.session(); s != nil && s.Token.RefreshToken != m.rt {
		m.rt, m.born, m.virt, m.retired = s.Token.RefreshToken, time.Now(), 0, false
	}
}

type cliOp struct {
	kind   string
	target string
	d      time.Duration
	flag   bool
	sub    []string
}

func genCLIOp(kinds []string) *rapid.Generator[cliOp] {
	return rapid.Custom(func(t *rapid.T) cliOp {
		op := cliOp{kind: rapid.SampledFrom(kinds).Draw(t, "kind")}
		switch op.kind {
		case "login", "retire", "down", "up":
			op.target = rapid.SampledFrom([]string{"A", "B", "A"}).Draw(t, "server")
		case "cmd":
			op.target = rapid.SampledFrom([]string{"", "", "", "A", "B"}).Draw(t, "server")
		case "advance":
			op.d = rapid.SampledFrom([]time.Duration{time.Minute, 9 * time.Minute, 11 * time.Minute, 11 * time.Minute, 30 * time.Minute, 2 * time.Hour}).Draw(t, "d")
		case "old-format", "corrupt":
			op.flag = rapid.Bool().Draw(t, "flag")
		}
		return op
	})
}

// genOps is swarm testing: each case enables a random subset of the
// operations, with login, cmd and advance always present so sessions get
// used and expire. The operations that take a second or more of real time
// (the device login, the watch, the follow) appear at most once per case and
// in a minority of cases, which keeps the property's default run short;
// -rapid.checks buys depth.
func genOps() *rapid.Generator[[]cliOp] {
	optional := []string{"logout", "revoke", "retire", "down", "up", "old-format", "corrupt", "late-login", "late-login",
		"whoami", "advance", "login"}
	return rapid.Custom(func(t *rapid.T) []cliOp {
		kinds := []string{"login", "cmd", "cmd", "cmd", "cmd", "cmd", "cmd", "advance", "advance"}
		for _, k := range optional {
			if rapid.Bool().Draw(t, "enable-"+k) {
				kinds = append(kinds, k)
			}
		}
		ops := rapid.SliceOfN(genCLIOp(kinds), 3, 24).Draw(t, "ops")
		// State-aware prefix: most histories start signed in, so commands
		// reach the token paths instead of stopping at "no server", and
		// some have been signed in to both servers.
		switch pre := rapid.IntRange(0, 9).Draw(t, "prelogin"); {
		case pre > 6:
			ops = append([]cliOp{{kind: "login", target: "B"}, {kind: "login", target: "A"}}, ops...)
		case pre > 0:
			ops = append([]cliOp{{kind: "login", target: "A"}}, ops...)
		}
		insert := func(op cliOp) {
			at := rapid.IntRange(0, len(ops)).Draw(t, "at-"+op.kind)
			ops = append(ops[:at], append([]cliOp{op}, ops[at:]...)...)
		}
		// rapid favours a range's ends, so the rare draws test for a value
		// inside it.
		if rapid.IntRange(0, 24).Draw(t, "with-device-login")/2 == 5 {
			insert(cliOp{kind: "login-device", target: rapid.SampledFrom([]string{"A", "B"}).Draw(t, "device-server")})
		}
		if rapid.IntRange(0, 11).Draw(t, "with-watch")/2 == 2 {
			insert(cliOp{kind: "watch", sub: rapid.SliceOfN(rapid.SampledFrom([]string{"advance", "drop", "other"}), 1, 4).Draw(t, "during-watch")})
		}
		if rapid.IntRange(0, 11).Draw(t, "with-follow")/2 == 3 {
			during := []string{"advance", "drop", "drop"}
			if genStreamRedialRefused {
				during = append(during, "retire")
			}
			insert(cliOp{kind: "follow", sub: rapid.SliceOfN(rapid.SampledFrom(during), 1, 3).Draw(t, "during-follow")})
		}
		return ops
	})
}

// cliStats counts interesting events across a run so a quiet result can be
// told apart from a harness that never reached the interesting states.
type cliStats struct {
	cmds, cmdOK, rotations, prompts, promptLogins int
	afterRetire, otherServer, undecided           int
	watches, follows, redials, deviceLogins       int
	whoamis, whoamiCorrupt                        int
}

// TestCLITokenLifecycle_Property is the history property described above.
func TestCLITokenLifecycle_Property(t *testing.T) {
	base := t.TempDir()
	binDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	browsed := stubBrowser(t, binDir)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BSKY_CLI_NO_VERSION_GATE", "1")
	t.Setenv("SLAGWORKS_API", "")
	t.Setenv("SLAGWORKS_OIDC_ISSUER", "")
	t.Setenv("SLAGWORKS_OIDC_CLIENT_ID", "")
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	// Invocations replace these globals with a client bound to a case's
	// config directory; don't leave it to later tests.
	prevURL, prevClient := apiURL, apiClient
	t.Cleanup(func() { apiURL, apiClient = prevURL, prevClient })
	// The watch and the follow write their log lines, reconnect notices and
	// summaries straight to the process's stdout and stderr.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull, devNull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = prevOut, prevErr
		_ = devNull.Close()
	})

	stats := &cliStats{}
	cases := 0
	rapid.Check(t, func(rt *rapid.T) {
		cases++
		cfgHome := filepath.Join(base, fmt.Sprintf("case-%d", cases))
		if err := os.Setenv("XDG_CONFIG_HOME", cfgHome); err != nil {
			rt.Fatal(err)
		}
		_ = os.Remove(browsed)
		w := newCLIWorld(cfgHome, browsed)
		defer w.close()
		m := &cliModel{down: map[string]bool{}, everIn: map[string]bool{}}
		m.approving = rapid.Bool().Draw(rt, "browser-completes-logins")

		for i, op := range genOps().Draw(rt, "ops") {
			w.step(rt, stats, m, i, op)
			before := m.rt
			m.sync(w)
			if m.rt != before {
				stats.rotations++
			}
			w.checkInvariants(rt, m, i, op)
		}
	})
	t.Logf("cases=%d cmds=%d ok=%d (after the server retired its key: %d) rotations=%d prompts=%d promptLogins=%d otherServer=%d undecided=%d deviceLogins=%d watches=%d follows=%d redials=%d whoamis=%d (corrupt cache: %d)",
		cases, stats.cmds, stats.cmdOK, stats.afterRetire, stats.rotations, stats.prompts, stats.promptLogins, stats.otherServer,
		stats.undecided, stats.deviceLogins, stats.watches, stats.follows, stats.redials, stats.whoamis, stats.whoamiCorrupt)
}

func (w *cliWorld) step(t *rapid.T, stats *cliStats, m *cliModel, i int, op cliOp) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch op.kind {
	case "login":
		// What `auth login` does after the interactive flow, minus the flow.
		tok := w.prov[op.target].Mint()
		if err := clientauth.WriteCachedToken(ctx, w.cfgFor(op.target), tok); err != nil {
			t.Fatal(err)
		}
		if err := writeSavedConfig(savedConfig{API: w.api[op.target].URL(), OIDCIssuer: w.prov[op.target].Issuer(), OIDCClientID: w.api[op.target].ClientID}); err != nil {
			t.Fatal(err)
		}
		m.saved = op.target
		m.newSession(op.target)
	case "login-device":
		out, err := runCLI(ctx, "auth", "login", "--device", "--api", w.api[op.target].URL())
		if m.down[op.target] {
			if err == nil {
				t.Fatalf("op %d: device login succeeded with the provider down", i)
			}
			return
		}
		if err != nil {
			t.Fatalf("op %d: device login to %s failed: %v\n%s", i, op.target, err, out)
		}
		m.saved = op.target
		m.newSession(op.target)
		stats.deviceLogins++
	case "late-login":
		// The tail of Bearer's interactive fallback in an invocation that
		// started against the other server while that was the saved one,
		// landing after a later `auth login` to this one: it stores the
		// session and leaves the saved server alone.
		late := map[string]string{"A": "B", "B": "A"}[m.saved]
		if !m.everIn[late] {
			return
		}
		if err := clientauth.WriteCachedToken(ctx, w.cfgFor(late), w.prov[late].Mint()); err != nil {
			t.Fatal(err)
		}
		m.newSession(late)
	case "logout":
		if _, err := runCLI(ctx, "auth", "logout"); err != nil {
			t.Fatalf("op %d: logout: %v", i, err)
		}
		if _, err := os.Stat(w.tokenPath()); err == nil {
			t.Fatalf("op %d: token cache still present after logout", i)
		}
		m.hasCache, m.broken, m.corrupt = false, false, false
	case "advance":
		w.advance(t, op.d)
		m.virt += op.d
	case "revoke":
		if fam, _, ok := authfake.ParseGen(m.rt); ok && m.hasCache {
			w.prov[m.cacheFor].Revoke(fam)
			m.revoked = true
		}
	case "retire":
		w.api[op.target].RotateKeys()
		if m.hasCache && m.cacheFor == op.target {
			m.retired = true
		}
	case "old-format":
		// The cache as releases before the session format wrote it: a bare
		// OAuth token with no record of its server. With flag, also no saved
		// server, which the earliest releases did not write. The token must
		// not be used for any server.
		if s := w.session(); s != nil {
			w.writeCacheFile(t, s.Token)
			m.hasCache, m.broken = false, true
		}
		if op.flag {
			_ = os.Remove(filepath.Join(w.cfgHome, appName, "config.json"))
			m.saved = ""
		}
	case "corrupt":
		b, err := os.ReadFile(w.tokenPath())
		if err != nil {
			return
		}
		if op.flag {
			b = b[:len(b)/2]
		} else {
			b = []byte("{not json")
		}
		if err := os.WriteFile(w.tokenPath(), b, 0o600); err != nil {
			t.Fatal(err)
		}
		m.hasCache, m.broken, m.corrupt = false, true, true
	case "down":
		w.prov[op.target].SetDown(true)
		m.down[op.target] = true
	case "up":
		w.prov[op.target].SetDown(false)
		m.down[op.target] = false
	case "cmd":
		w.cmd(t, stats, m, i, op)
	case "whoami":
		w.whoami(t, stats, m, i)
	case "watch":
		w.watch(t, stats, m, i, op)
	case "follow":
		w.follow(t, stats, m, i, op)
	}
}

// cmd runs a short command. When slagctl opens the browser for an
// interactive login, the stub browser either completes it (approving) or the
// operator gives up, which ends the command.
func (w *cliWorld) cmd(t *rapid.T, stats *cliStats, m *cliModel, i int, op cliOp) {
	args := []string{"machine", "list", "--json"}
	target := op.target
	if target != "" {
		args = append(args, "--api", w.api[target].URL())
	} else {
		target = m.saved
	}
	usable, unusable := m.usable(target), m.unusable(target)
	otherServer := target != "" && m.saved == target && m.hasCache && m.cacheFor != target
	retired := m.signedIn(target) && m.retired

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	urlsBefore := len(w.browsedURLs())
	go func() {
		out, err := runCLI(ctx, args...)
		done <- result{out, err}
	}()
	prompted := false
	var res result
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-tick.C:
			urls := w.browsedURLs()
			if prompted || len(urls) == urlsBefore {
				continue
			}
			prompted = true
			if !m.approving {
				cancel()
				continue
			}
			go func(u string) {
				if resp, err := http.Get(u); err == nil { //nolint:gosec,noctx // the stub browser follows the URL slagctl handed it
					_ = resp.Body.Close()
				}
			}(urls[len(urls)-1])
		}
	}
	out, err := res.out, res.err
	prompted = prompted || len(w.browsedURLs()) > urlsBefore

	stats.cmds++
	if err == nil {
		stats.cmdOK++
		if retired {
			stats.afterRetire++
		}
	}
	if prompted {
		stats.prompts++
	}
	if otherServer {
		stats.otherServer++
	}
	desc := fmt.Sprintf("op %d: `slagctl %s` (target %q, model %+v, token age %s)", i, strings.Join(args, " "), target, *m, m.age().Round(time.Second))

	if prompted && (target == "" || m.saved != target || otherServer || m.down[target]) {
		t.Fatalf("%s opened a browser login, with no saved sign-in for that server, a session for another server in the cache, or its provider down", desc)
	}
	switch {
	case usable:
		if err != nil {
			t.Fatalf("%s against the signed-in server with a usable session failed: %v\n%s", desc, err, out)
		}
		if prompted {
			t.Fatalf("%s: a usable session prompted for an interactive login", desc)
		}
	case err != nil:
		if !strings.Contains(err.Error(), "slagctl auth login") {
			t.Fatalf("%s: the failure does not name the command that fixes it: %v", desc, err)
		}
	case !unusable:
		// The seconds around an expiry: either outcome is right.
		stats.undecided++
		if prompted {
			m.newSession(target)
		}
	default:
		// Success without a usable session is only an interactive login the
		// stub browser completed (Bearer's fallback), which replaces the
		// session.
		if !prompted || !m.approving {
			t.Fatalf("%s succeeded without a usable session and without a login", desc)
		}
		stats.promptLogins++
		m.newSession(target)
	}
}

// whoami checks `auth whoami`, which reads only the cache.
func (w *cliWorld) whoami(t *rapid.T, stats *cliStats, m *cliModel, i int) {
	stats.whoamis++
	if m.corrupt {
		stats.whoamiCorrupt++
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := runCLI(ctx, "auth", "whoami")
	if m.hasCache {
		if err != nil || !strings.Contains(out, "operator") {
			t.Fatalf("op %d: whoami with a cached session: %v\n%s", i, err, out)
		}
		return
	}
	if err == nil {
		t.Fatalf("op %d: whoami succeeded without a cached session (model %+v):\n%s", i, *m, out)
	}
	if !strings.Contains(err.Error(), "slagctl auth login") {
		t.Fatalf("op %d: whoami's failure does not name the command that fixes it: %v", i, err)
	}
	if checkCorruptCacheLabel && m.corrupt && strings.Contains(err.Error(), "no cached token") {
		t.Fatalf("op %d: whoami reports no cached token, but the cache file is there and does not parse: %v", i, err)
	}
}

// waitFor polls cond every few milliseconds until it holds or d passes.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// watch runs `operation watch --plain` and, while it runs, advances the
// clock past expiry, drops the stream, lets other invocations refresh the
// shared cache, then completes the operation.
func (w *cliWorld) watch(t *rapid.T, stats *cliStats, m *cliModel, i int, op cliOp) {
	n := m.saved
	if !m.signedIn(n) || !m.canRefresh() {
		return // starting without a refreshable session is the cmd op's business
	}
	stats.watches++
	w.api[n].DropStreams()
	opID := w.api[n].AddOp()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runCLI(ctx, "operation", "watch", opID, "--plain", "--timeout", "20s")
		done <- err
	}()
	if !waitFor(10*time.Second, func() bool { return w.api[n].OpenStreams() > 0 || len(done) > 0 }) || len(done) > 0 {
		t.Fatalf("op %d: `operation watch` with a refreshable session never opened the logs stream (model %+v)", i, *m)
	}
	dropped := false
	for _, s := range op.sub {
		switch s {
		case "advance":
			w.advance(t, 11*time.Minute)
			m.virt += 11 * time.Minute
		case "drop":
			if !dropped { // each drop costs the watch's one-second redial wait
				w.api[n].DropStreams()
				dropped = true
				stats.redials++
			}
		case "other":
			if err := w.otherProcess(ctx, n); err != nil {
				t.Fatalf("op %d: a concurrent invocation failed to use a refreshable session: %v", i, err)
			}
			m.sync(w)
		}
	}
	w.api[n].CompleteOp(opID)
	if err := <-done; err != nil {
		t.Fatalf("op %d: `operation watch` of an operation that completed, with a valid refresh token, failed: %v (during: %v, 401s so far: %d)",
			i, err, op.sub, w.api[n].Rejected())
	}
}

// follow runs `machine logs -f` and, while it runs, advances the clock past
// expiry, retires the server's key and drops the stream. A follow ends only
// when the operator interrupts it.
func (w *cliWorld) follow(t *rapid.T, stats *cliStats, m *cliModel, i int, op cliOp) {
	n := m.saved
	if !m.signedIn(n) || !m.canRefresh() {
		return
	}
	if !genStreamRedialRefused && m.retired && !m.untrusted() {
		// Given a machine's UUID the command's first request is the stream
		// dial, which is refused like a redial.
		return
	}
	stats.follows++
	w.api[n].DropStreams()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runCLI(ctx, "machine", "logs", w.api[n].MachineID(), "-f")
		done <- err
	}()
	attached := func() {
		if !waitFor(10*time.Second, func() bool { return w.api[n].OpenStreams() > 0 || len(done) > 0 }) || len(done) > 0 {
			var err error
			if len(done) > 0 {
				err = <-done
			}
			t.Fatalf("op %d: `machine logs -f` with a valid refresh token is not following (during: %v, model %+v, 401s so far: %d): %v",
				i, op.sub, *m, w.api[n].Rejected(), err)
		}
	}
	attached()
	dropped := false
	for _, s := range op.sub {
		switch s {
		case "advance":
			w.advance(t, 11*time.Minute)
			m.virt += 11 * time.Minute
		case "retire":
			w.api[n].RotateKeys()
			m.retired = true
		case "drop":
			if !dropped { // each drop costs the follow's one-second redial wait
				w.api[n].DropStreams()
				dropped = true
				stats.redials++
				attached()
				m.sync(w)
			}
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("op %d: an interrupted `machine logs -f` returned %v, want the interruption", i, err)
	}
}

func (w *cliWorld) checkInvariants(t *rapid.T, m *cliModel, i int, op cliOp) {
	for _, n := range []string{"A", "B"} {
		if l := w.api[n].Leaks(); len(l) > 0 {
			t.Fatalf("after op %d (%+v): server %s received %d token(s) issued for the other server", i, op, n, len(l))
		}
		if f := w.prov[n].Foreign(); len(f) > 0 {
			t.Fatalf("after op %d (%+v): provider %s received refresh token(s) issued by the other provider: %v", i, op, n, f)
		}
		if r := w.prov[n].Reuses(); len(r) > 0 {
			t.Fatalf("after op %d (%+v): provider %s saw refresh token(s) spent twice: %v", i, op, n, r)
		}
	}
	fi, err := os.Stat(w.tokenPath())
	if err != nil {
		if m.hasCache {
			t.Fatalf("after op %d (%+v): the token cache is gone, but nothing logged out", i, op)
		}
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("after op %d: token cache mode %v is readable by others", i, fi.Mode().Perm())
	}
	if m.broken {
		return
	}
	s := w.session()
	if s == nil {
		t.Fatalf("after op %d (%+v): token cache unparseable", i, op)
	}
	if got := w.serverOf(s.API); !m.hasCache || got != m.cacheFor || s.Issuer != w.prov[got].Issuer() {
		t.Fatalf("after op %d (%+v): the cache holds a session for server %q (issuer %s), want %q (model %+v)", i, op, got, s.Issuer, m.cacheFor, *m)
	}
}
