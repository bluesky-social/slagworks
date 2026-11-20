package cmd_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/cli/clierr"
	"github.com/bluesky-social/slagworks/internal/cli/cliupdate"
)

// isolateUserDirs points the token cache and update-check state at temp dirs
// so tests never read or write the developer's real ones.
func isolateUserDirs(t *testing.T) (configHome, stateHome string) {
	t.Helper()
	configHome, stateHome = t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("BSKY_CLI_NO_VERSION_GATE", "")
	return configHome, stateHome
}

// writeTokenCache writes a cached session whose access token is an unsigned
// JWT carrying the given username, at the path slagctl has always used.
func writeTokenCache(t *testing.T, configHome, username string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"sub": "abc123", "preferred_username": username})
	if err != nil {
		t.Fatal(err)
	}
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	tok, err := json.Marshal(map[string]any{
		"api":       "https://slagworks.example",
		"issuer":    "https://idp.example/",
		"client_id": "slagctl",
		"token": map[string]any{
			"access_token": jwt,
			"token_type":   "Bearer",
			"expiry":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configHome, "slagctl", "token.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tok, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthWhoamiAndLogoutUseTokenCache(t *testing.T) {
	configHome, _ := isolateUserDirs(t)
	path := writeTokenCache(t, configHome, "operator@example.com")

	out, err := execCmd(t, nil, "auth", "whoami")
	if err != nil {
		t.Fatalf("auth whoami: %v", err)
	}
	if !strings.Contains(out, "username: operator@example.com") {
		t.Errorf("whoami output = %q, want the cached identity", out)
	}

	if _, err := execCmd(t, nil, "auth", "logout"); err != nil {
		t.Fatalf("auth logout: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("token cache should be removed by logout, stat err = %v", err)
	}

	if _, err := execCmd(t, nil, "auth", "whoami"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Errorf("whoami after logout: err = %v, want a not-logged-in error", err)
	}
}

// TestAuthWhoamiWithoutSessionNamesLoginCommand: with no cached token (a
// fresh install, or after `auth logout`), `auth whoami` fails, and like every
// other not-signed-in path in slagctl the error names the command that fixes
// it rather than ending at the token file's "no such file or directory".
func TestAuthWhoamiWithoutSessionNamesLoginCommand(t *testing.T) {
	isolateUserDirs(t)

	_, err := execCmd(t, nil, "auth", "whoami")
	if err == nil {
		t.Fatal("auth whoami with no cached token succeeded")
	}
	for _, want := range []string{"not logged in", "run `slagctl auth login`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("auth whoami with no cached token: err = %q, want it to contain %q", err, want)
		}
	}
}

func TestDoctorChecksWithoutToken(t *testing.T) {
	isolateUserDirs(t)

	out, err := execCmd(t, nil, "doctor", "--json")
	if code, _ := clierr.Code(err); code != 1 {
		t.Fatalf("doctor without a token: exit code %d (err %v), want 1", code, err)
	}
	var findings []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &findings); err != nil {
		t.Fatalf("doctor --json output did not parse: %v\n%s", err, out)
	}
	want := map[string]string{
		"binary-provenance":    "warn", // test binaries are dev builds
		"state-dir-writable":   "ok",
		"slagctl-token":        "fail",
		"slagctl-connectivity": "fail",
	}
	if len(findings) != len(want) {
		t.Fatalf("doctor ran %d checks, want %d: %+v", len(findings), len(want), findings)
	}
	for _, f := range findings {
		if want[f.Name] != f.Status {
			t.Errorf("check %q status = %q, want %q", f.Name, f.Status, want[f.Name])
		}
	}
}

// TestDoctorTokenCheckWithExpiredAccessToken: access tokens are short-lived
// (ten minutes is typical) and every command renews an expired one with the
// cached refresh token, without a login. doctor must judge the session the
// same way: an expired access token with a refresh token is a healthy
// session, so the token check passes and doctor exits 0; only a session with
// no refresh token needs `slagctl auth login`.
func TestDoctorTokenCheckWithExpiredAccessToken(t *testing.T) {
	jwt := func(exp time.Time) string {
		claims, err := json.Marshal(map[string]any{"sub": "abc123", "preferred_username": "operator", "exp": exp.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
	}
	type finding struct{ Name, Status, Detail string }
	runDoctor := func(t *testing.T, refreshToken string) (map[string]finding, error) {
		t.Helper()
		configHome, _ := isolateUserDirs(t)
		t.Setenv("SLAGWORKS_API", "")

		// The identity provider grants a fresh access token for the cached
		// refresh token; the API accepts only that fresh token.
		fresh := jwt(time.Now().Add(10 * time.Minute))
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
				if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != refreshToken {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token": fresh, "refresh_token": "rt-next", "token_type": "Bearer", "expires_in": 600,
				})
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(idp.Close)
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fresh {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"ready":true,"version":"test"}`))
		}))
		t.Cleanup(api.Close)

		expired := time.Now().Add(-time.Minute)
		dir := filepath.Join(configHome, "slagctl")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]any{
			"config.json": map[string]string{"api": api.URL, "oidc_issuer": idp.URL, "oidc_client_id": "slagctl"},
			"token.json": map[string]any{
				"api": api.URL, "issuer": idp.URL, "client_id": "slagctl",
				"token": map[string]any{
					"access_token": jwt(expired), "refresh_token": refreshToken,
					"token_type": "Bearer", "expiry": expired.Format(time.RFC3339),
				},
			},
		} {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		// Not execCmd: an injected client skips resolving the saved server,
		// which the connectivity check needs.
		root := cmd.NewRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(new(bytes.Buffer))
		root.SetArgs([]string{"doctor", "--json"})
		runErr := root.Execute()
		var findings []finding
		if err := json.Unmarshal(out.Bytes(), &findings); err != nil {
			t.Fatalf("doctor --json output did not parse: %v\n%s", err, out.String())
		}
		byName := map[string]finding{}
		for _, f := range findings {
			byName[f.Name] = f
		}
		return byName, runErr
	}

	t.Run("refresh token cached", func(t *testing.T) {
		got, err := runDoctor(t, "rt-cached")
		if f := got["slagctl-token"]; f.Status != "ok" || !strings.Contains(f.Detail, "operator") {
			t.Errorf("slagctl-token = %+v, want ok naming the operator: the refresh token keeps the session alive", f)
		}
		// The connectivity check refreshes for real, proving the session works.
		if f := got["slagctl-connectivity"]; f.Status != "ok" {
			t.Errorf("slagctl-connectivity = %+v, want ok", f)
		}
		if err != nil {
			t.Errorf("doctor exited with %v, want success", err)
		}
	})

	t.Run("no refresh token", func(t *testing.T) {
		got, err := runDoctor(t, "")
		if f := got["slagctl-token"]; f.Status != "fail" || !strings.Contains(f.Detail, "slagctl auth login") {
			t.Errorf("slagctl-token = %+v, want fail naming `slagctl auth login`", f)
		}
		if code, _ := clierr.Code(err); code != 1 {
			t.Errorf("doctor exit code %d (err %v), want 1", code, err)
		}
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestUpdateHelperSkipsAPIAuth runs the hidden helper with no cached session,
// an unreachable issuer and no GitHub token. It must not go through the
// root's OIDC login (which would open a browser from a detached process); it
// must query the public repository's latest release anonymously and record
// it.
func TestUpdateHelperSkipsAPIAuth(t *testing.T) {
	_, stateHome := isolateUserDirs(t)
	t.Setenv("SLAGWORKS_OIDC_ISSUER", "http://127.0.0.1:1/")
	// No token source: slagworks releases are public, so the check runs
	// without one.
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir())

	// The release check uses http.DefaultClient. Answer it here so the test
	// never reaches GitHub. No test in this package runs in parallel, so
	// nothing else sees the swap.
	var requests []string
	origClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("release check sent Authorization %q with no token configured", auth)
		}
		if r.URL.String() != "https://api.github.com/repos/bluesky-social/slagworks/releases/latest" {
			return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody, Request: r}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v9.9.9"}`)),
			Request:    r,
		}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = origClient })

	root, _ := cmd.NewRootCmdWithFinish()
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{cliupdate.HelperCommandName, "slagctl"})
	if err := root.Execute(); err != nil {
		t.Fatalf("update helper: %v", err)
	}

	if len(requests) != 1 {
		t.Fatalf("release check requests = %q, want one to the slagworks releases/latest endpoint", requests)
	}
	state, err := os.ReadFile(filepath.Join(stateHome, "slagctl", "update-check.json"))
	if err != nil {
		t.Fatalf("helper recorded no update-check state: %v", err)
	}
	if !strings.Contains(string(state), `"latest_seen":"v9.9.9"`) {
		t.Errorf("state = %s, want latest_seen v9.9.9 from an anonymous check", state)
	}
}

func TestMajorGateRefusesStaleRelease(t *testing.T) {
	_, stateHome := isolateUserDirs(t)
	cmd.SetClientVersion("v1.2.3")
	t.Cleanup(func() { cmd.SetClientVersion("dev") })

	statePath := filepath.Join(stateHome, "slagctl", "update-check.json")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte(`{"latest_seen":"v2.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true, "version": "v2.0.0"})
	})
	cl := testServer(t, mux)

	_, err := execCmd(t, cl, "site-config", "list")
	if err == nil {
		t.Fatal("a binary a major version behind should be refused")
	}
	if code, ok := clierr.Code(err); !ok || code != 3 {
		t.Errorf("exit code = (%d, %t), want (3, true)", code, ok)
	}
	if !strings.Contains(err.Error(), "'brew upgrade slagctl'") {
		t.Errorf("refusal = %q, want it to name the upgrade command", err)
	}

	// version is exempt: it is how an operator inspects a stale binary.
	out, err := execCmd(t, cl, "version")
	if err != nil {
		t.Fatalf("version should run on a stale binary: %v", err)
	}
	if !strings.Contains(out, "client: v1.2.3") {
		t.Errorf("version output = %q", out)
	}
}

// refusedTokenWorld signs slagctl in to a fake API and identity provider with
// a cached access token, at-0, that slagctl still trusts (it expires in 20s,
// outside the 10s early-expiry margin oauth2 applies) but the API refuses
// with 401, as it does when the API's clock runs ahead of the provider's or
// the provider has rotated its signing key. The provider rotates rt-0 to
// at-1/rt-1 when refreshOK is set and answers invalid_grant otherwise. The
// API accepts any other token unless refuseAll is set: a server that rejects
// valid tokens (a misconfigured issuer or audience). refreshes reports how
// many refreshes the provider granted.
func refusedTokenWorld(t *testing.T, refreshOK, refuseAll bool) (configHome string, refreshes func() int) {
	t.Helper()
	configHome, _ = isolateUserDirs(t)
	t.Setenv("SLAGWORKS_API", "")

	var mu sync.Mutex
	issued := 0
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
			_ = r.ParseForm()
			mu.Lock()
			defer mu.Unlock()
			if !refreshOK || r.Form.Get("refresh_token") != fmt.Sprintf("rt-%d", issued) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			issued++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": fmt.Sprintf("at-%d", issued), "refresh_token": fmt.Sprintf("rt-%d", issued),
				"token_type": "Bearer", "expires_in": 600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(idp.Close)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if refuseAll || tok == "" || tok == "at-0" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"invalid token"}`))
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(api.Close)

	dir := filepath.Join(configHome, "slagctl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{
		"config.json": map[string]string{"api": api.URL, "oidc_issuer": idp.URL, "oidc_client_id": "slagctl"},
		"token.json": map[string]any{
			"api": api.URL, "issuer": idp.URL, "client_id": "slagctl",
			"token": map[string]any{
				"access_token": "at-0", "refresh_token": "rt-0", "token_type": "Bearer",
				"expiry": time.Now().Add(20 * time.Second).Format(time.RFC3339),
			},
		},
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return configHome, func() int { mu.Lock(); defer mu.Unlock(); return issued }
}

// runSignedIn runs slagctl through the real root command: the cached-token
// lookup and the API client it builds, with no injected client.
func runSignedIn(args ...string) error {
	root := cmd.NewRootCmd()
	root.SetArgs(args)
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	return root.ExecuteContext(context.Background())
}

// TestCommand_RetriesA401WithARefreshedToken: when the API refuses an access
// token slagctl still trusts and the refresh token is valid, the command
// refreshes once, writes the rotated session back, and resends the request.
func TestCommand_RetriesA401WithARefreshedToken(t *testing.T) {
	configHome, refreshes := refusedTokenWorld(t, true, false)
	if err := runSignedIn("machine", "list", "--json"); err != nil {
		t.Fatalf("machine list with a refused access token and a valid refresh token failed: %v (refreshes: %d)", err, refreshes())
	}
	if n := refreshes(); n != 1 {
		t.Errorf("refreshes = %d, want 1", n)
	}
	b, err := os.ReadFile(filepath.Join(configHome, "slagctl", "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cached struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal(b, &cached); err != nil {
		t.Fatal(err)
	}
	if cached.Token.AccessToken != "at-1" || cached.Token.RefreshToken != "rt-1" {
		t.Errorf("cached session = %s/%s, want the rotated at-1/rt-1 written back", cached.Token.AccessToken, cached.Token.RefreshToken)
	}
}

// TestCommand_A401NamesTheLoginCommand: when the API refuses the token and a
// refresh cannot fix it, the error keeps the server's message and says what
// to run. A server that refuses even the refreshed token gets one refresh,
// not one per attempt.
func TestCommand_A401NamesTheLoginCommand(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		refreshOK, refuseAll bool
		wantRefreshes        int
	}{
		{name: "dead refresh token", refreshOK: false, refuseAll: false, wantRefreshes: 0},
		{name: "server refuses the refreshed token too", refreshOK: true, refuseAll: true, wantRefreshes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, refreshes := refusedTokenWorld(t, tc.refreshOK, tc.refuseAll)
			err := runSignedIn("machine", "list", "--json")
			if err == nil {
				t.Fatal("machine list succeeded although the API refuses the token")
			}
			for _, want := range []string{"invalid token", "slagctl auth login"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if n := refreshes(); n != tc.wantRefreshes {
				t.Errorf("refreshes = %d, want %d", n, tc.wantRefreshes)
			}
		})
	}
}
