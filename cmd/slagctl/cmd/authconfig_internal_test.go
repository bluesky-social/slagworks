package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
