package cmd_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		"access_token": jwt,
		"token_type":   "Bearer",
		"expiry":       time.Now().Add(time.Hour).Format(time.RFC3339),
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
