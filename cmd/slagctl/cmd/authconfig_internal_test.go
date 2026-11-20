package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
)

// isolateConfig points the slagctl config dir at a temp dir and clears the
// environment overrides.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SLAGWORKS_API", "")
	t.Setenv("SLAGWORKS_OIDC_ISSUER", "")
	t.Setenv("SLAGWORKS_OIDC_CLIENT_ID", "")
}

func TestResolveAPIURL_Precedence(t *testing.T) {
	isolateConfig(t)

	if _, err := resolveAPIURL(""); err == nil || !strings.Contains(err.Error(), "auth login --api") {
		t.Fatalf("with nothing configured, want an error naming `auth login --api`, got %v", err)
	}

	if err := writeSavedConfig(savedConfig{API: "https://saved.example"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := resolveAPIURL(""); got != "https://saved.example" {
		t.Errorf("saved config: got %q", got)
	}

	t.Setenv("SLAGWORKS_API", "https://env.example/")
	if got, _ := resolveAPIURL(""); got != "https://env.example" {
		t.Errorf("env beats saved config and loses its trailing slash: got %q", got)
	}
	if got, _ := resolveAPIURL("https://flag.example"); got != "https://flag.example" {
		t.Errorf("flag beats env: got %q", got)
	}
}

// clientConfigServer serves /api/v1/client-config and counts requests.
func clientConfigServer(t *testing.T, issuer, clientID string) (url string, hits *int) {
	t.Helper()
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/client-config" {
			http.NotFound(w, r)
			return
		}
		n++
		_ = json.NewEncoder(w).Encode(map[string]string{"oidc_issuer": issuer, "oidc_client_id": clientID})
	}))
	t.Cleanup(ts.Close)
	return ts.URL, &n
}

func TestLoginSettings_AsksServer(t *testing.T) {
	isolateConfig(t)
	api, hits := clientConfigServer(t, "https://idp.example/", "client-abc")
	issuer, clientID, err := loginSettings(context.Background(), api)
	if err != nil {
		t.Fatal(err)
	}
	if issuer != "https://idp.example/" || clientID != "client-abc" || *hits != 1 {
		t.Fatalf("got %q %q after %d requests", issuer, clientID, *hits)
	}
}

func TestLoginSettings_EnvOverridesNeedNoServer(t *testing.T) {
	isolateConfig(t)
	t.Setenv("SLAGWORKS_OIDC_ISSUER", "https://env-idp.example/")
	t.Setenv("SLAGWORKS_OIDC_CLIENT_ID", "env-client")
	issuer, clientID, err := loginSettings(context.Background(), "http://127.0.0.1:1")
	if err != nil || issuer != "https://env-idp.example/" || clientID != "env-client" {
		t.Fatalf("got %q %q err %v", issuer, clientID, err)
	}
}

// A server without the endpoint (older than this slagctl) yields an error
// that names the two ways out.
func TestLoginSettings_OldServer(t *testing.T) {
	isolateConfig(t)
	ts := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(ts.Close)
	_, _, err := loginSettings(context.Background(), ts.URL)
	if err == nil || !strings.Contains(err.Error(), "SLAGWORKS_OIDC_ISSUER") {
		t.Fatalf("want an error naming the env overrides, got %v", err)
	}
}

// bearerSource re-reads the token cache on every call, so a long command
// gets the token a refresh (or a login in another terminal) wrote after it
// started; and it re-checks the saved server every time, so once another
// terminal signs in to a different server this command gets an error that
// only a new login clears, never that server's token.
func TestBearerSource_FollowsTheCacheAndStaysOnItsServer(t *testing.T) {
	isolateConfig(t)
	ctx := context.Background()
	login := func(api, token string) {
		t.Helper()
		if err := writeSavedConfig(savedConfig{API: api, OIDCIssuer: "https://idp.example/", OIDCClientID: "client-abc"}); err != nil {
			t.Fatal(err)
		}
		cfg := clientauth.Config{AppName: appName, API: api, Issuer: "https://idp.example/", ClientID: "client-abc"}
		if err := clientauth.WriteCachedToken(ctx, cfg, &oauth2.Token{AccessToken: token, Expiry: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	bearer := bearerSource("https://prod.example")

	login("https://prod.example", "prod-1")
	if got, err := bearer(ctx); err != nil || got != "prod-1" {
		t.Fatalf("got %q err %v, want prod-1", got, err)
	}
	login("https://prod.example", "prod-2")
	if got, err := bearer(ctx); err != nil || got != "prod-2" {
		t.Fatalf("after the cache changed: got %q err %v, want prod-2", got, err)
	}

	login("https://other.example", "other-1")
	got, err := bearer(ctx)
	if got != "" || !errors.Is(err, clientauth.ErrLoginRequired) || !strings.Contains(err.Error(), "auth login --api https://prod.example") {
		t.Fatalf("after signing in to another server: got %q err %v, want no token and a sign-in error naming the login command", got, err)
	}
}

// A login to another server that finishes while bearerSource is reading the
// token (here, while it refreshes) is caught by its second look at the
// saved server: the request fails rather than go out with whatever the
// cache holds by then.
func TestBearerSource_RechecksTheServerAfterReadingTheToken(t *testing.T) {
	isolateConfig(t)
	ctx := context.Background()
	var idp *httptest.Server
	idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize",
				"token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/jwks"})
		case "/token":
			// Meanwhile another terminal signs in to a different server.
			if err := writeSavedConfig(savedConfig{API: "https://other.example", OIDCIssuer: idp.URL, OIDCClientID: "client-abc"}); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "prod-2", "refresh_token": "rt-2", "token_type": "Bearer", "expires_in": 600})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)
	if err := writeSavedConfig(savedConfig{API: "https://prod.example", OIDCIssuer: idp.URL, OIDCClientID: "client-abc"}); err != nil {
		t.Fatal(err)
	}
	expired := &oauth2.Token{AccessToken: "prod-1", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Minute)}
	cfg := clientauth.Config{AppName: appName, API: "https://prod.example", Issuer: idp.URL, ClientID: "client-abc"}
	if err := clientauth.WriteCachedToken(ctx, cfg, expired); err != nil {
		t.Fatal(err)
	}

	got, err := bearerSource("https://prod.example")(ctx)
	if got != "" || !errors.Is(err, clientauth.ErrLoginRequired) {
		t.Fatalf("got %q err %v; want no token and a sign-in error once the saved server changed", got, err)
	}
}

// The cached token belongs to the server of the last login. A command aimed
// at any other server must be refused before the token or its refresh token
// can go anywhere, and must never ask that server who to trust.
func TestAuthConfig_TokenIsBoundToTheLoginServer(t *testing.T) {
	isolateConfig(t)
	other, hits := clientConfigServer(t, "https://attacker-idp.example/", "x")

	if _, err := authConfig("https://prod.example"); err == nil || !strings.Contains(err.Error(), "auth login --api https://prod.example") {
		t.Fatalf("with no login, want an error naming the login command, got %v", err)
	}

	if err := writeSavedConfig(savedConfig{API: "https://prod.example", OIDCIssuer: "https://idp.example/", OIDCClientID: "client-abc"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := authConfig("https://prod.example")
	if err != nil || cfg.Issuer != "https://idp.example/" || cfg.ClientID != "client-abc" {
		t.Fatalf("login server: got %+v err %v", cfg, err)
	}

	if _, err := authConfig(other); err == nil {
		t.Fatal("a different server must not get the cached token's sign-in config")
	}
	if *hits != 0 {
		t.Fatalf("the other server was asked for sign-in settings %d times; want 0", *hits)
	}

	// The environment overrides are for logging in; they do not rebind an
	// existing token to another identity provider.
	t.Setenv("SLAGWORKS_OIDC_ISSUER", "https://attacker-idp.example/")
	t.Setenv("SLAGWORKS_OIDC_CLIENT_ID", "x")
	cfg, err = authConfig("https://prod.example")
	if err != nil || cfg.Issuer != "https://idp.example/" {
		t.Fatalf("env overrides must not change the issuer of a saved login: %+v err %v", cfg, err)
	}
}

// signInServer is a slagworks server with its own identity provider. Every
// token the provider mints carries the server's name, so the server can tell
// when it is handed another server's access token and the provider when it is
// handed another provider's refresh token.
type signInServer struct {
	name      string
	idp, api  *httptest.Server
	mu        sync.Mutex
	bearers   []string // bearer tokens the API received
	refreshes []string // refresh tokens the provider received
}

func newSignInServer(t *testing.T, name string) *signInServer {
	t.Helper()
	s := &signInServer{name: name}
	minted := 0
	s.idp = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": s.idp.URL, "authorization_endpoint": s.idp.URL + "/authorize",
				"token_endpoint": s.idp.URL + "/token", "jwks_uri": s.idp.URL + "/jwks",
				"device_authorization_endpoint": s.idp.URL + "/device",
			})
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc", "user_code": "UC", "verification_uri": s.idp.URL + "/verify",
				"expires_in": 30, "interval": 1,
			})
		case "/verify":
			// The device page exists; login checks it before polling.
			_, _ = w.Write([]byte("{}"))
		case "/token":
			_ = r.ParseForm()
			s.mu.Lock()
			if rt := r.Form.Get("refresh_token"); rt != "" {
				s.refreshes = append(s.refreshes, rt)
			}
			minted++
			at, rt := fmt.Sprintf("%s-at-%d", name, minted), fmt.Sprintf("%s-rt-%d", name, minted)
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": 600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.idp.Close)
	s.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/client-config" {
			_ = json.NewEncoder(w).Encode(map[string]string{"oidc_issuer": s.idp.URL, "oidc_client_id": "slagctl"})
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		s.bearers = append(s.bearers, tok)
		s.mu.Unlock()
		if !strings.HasPrefix(tok, name+"-at-") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid token"}`))
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(s.api.Close)
	return s
}

// runSlagctl runs one slagctl invocation in-process.
func runSlagctl(t *testing.T, timeout time.Duration, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.ExecuteContext(ctx)
}

// The token cache must hold, with the token, which server it was issued for,
// because the saved server (config.json) is a different file written at a
// different moment. Two invocations that interleave can leave the cache
// holding server A's session while the saved server is B:
//
//  1. `auth login --api A`.
//  2. Terminal 1 runs a command against A that needs an interactive login,
//     so its Bearer starts A's browser flow.
//  3. Meanwhile terminal 2 runs `auth login --device --api B` to completion,
//     writing B's token and then B as the saved server.
//  4. Terminal 1's browser login completes and Bearer stores A's new token.
//  5. `slagctl machine list`, which goes to the saved server, B.
//
// A failed config write at the end of `auth login` leaves the same split.
// Step 5 must not hand A's access token to B, nor (once it has expired) A's
// refresh token to B's identity provider; it must say the operator is not
// signed in to B.
func TestAuth_TokenNeverReachesAnotherServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expired bool // A's access token has expired by step 5, so it would be refreshed
	}{
		{name: "access token", expired: false},
		{name: "refresh token", expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("BSKY_CLI_NO_VERSION_GATE", "1")
			// No `open` or `xdg-open`: a test must never launch a browser.
			t.Setenv("PATH", t.TempDir())
			a, b := newSignInServer(t, "A"), newSignInServer(t, "B")

			if err := runSlagctl(t, 20*time.Second, "auth", "login", "--device", "--api", a.api.URL); err != nil { // 1
				t.Fatal(err)
			}
			if err := runSlagctl(t, 5*time.Second, "machine", "list", "--json"); err != nil {
				t.Fatalf("a command right after signing in to A: %v", err)
			}
			cfgA, err := authConfig(a.api.URL) // 2: what terminal 1's root command built
			if err != nil {
				t.Fatal(err)
			}
			if err := runSlagctl(t, 20*time.Second, "auth", "login", "--device", "--api", b.api.URL); err != nil { // 3
				t.Fatal(err)
			}
			// 4: terminal 1's login for A lands. Bearer caches it with the
			// same storeToken write WriteCachedToken makes, under cfgA.
			oc := &oauth2.Config{ClientID: cfgA.ClientID, Endpoint: oauth2.Endpoint{TokenURL: cfgA.Issuer + "/token"}}
			tokA, err := oc.Exchange(context.Background(), "code")
			if err != nil {
				t.Fatal(err)
			}
			if tc.expired {
				tokA.Expiry = time.Now().Add(-time.Minute)
				tokA.ExpiresIn = 0
			}
			if err := clientauth.WriteCachedToken(context.Background(), cfgA, tokA); err != nil {
				t.Fatal(err)
			}
			// 5. A short deadline: falling back to an interactive login here
			// would wait for a browser that never comes.
			err = runSlagctl(t, 3*time.Second, "machine", "list", "--json")

			b.mu.Lock()
			defer b.mu.Unlock()
			for _, tok := range b.bearers {
				if strings.HasPrefix(tok, "A-") {
					t.Fatalf("server B received access token %q minted by server A's identity provider", tok)
				}
			}
			for _, rt := range b.refreshes {
				if strings.HasPrefix(rt, "A-") {
					t.Fatalf("server B's identity provider received refresh token %q minted by A's", rt)
				}
			}
			if err == nil || !strings.Contains(err.Error(), "not signed in to "+b.api.URL) || !strings.Contains(err.Error(), "slagctl auth login") {
				t.Fatalf("with the cached session issued for A, a command against B: err = %v, want it to say it is not signed in to B and name `slagctl auth login`", err)
			}
		})
	}
}
