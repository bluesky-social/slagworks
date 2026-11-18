package tailscale

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTailscale stands in for the Tailscale API: it asserts the OAuth Basic
// auth + form shape and the key-create body, then returns canned responses.
type fakeTailscale struct {
	t *testing.T

	// what the test wants the endpoints to do
	tokenStatus  int
	tokenBody    string
	keyStatus    int
	keyBody      string
	failNoBearer bool

	// captured for assertions
	gotClientID     string
	gotClientSecret string
	gotScope        string
	gotBearer       string
	gotKeyReq       keyCreateRequest
}

func (f *fakeTailscale) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		f.gotClientID, f.gotClientSecret, _ = r.BasicAuth()
		_ = r.ParseForm()
		f.gotScope = r.Form.Get("scope")
		if r.Form.Get("grant_type") != "client_credentials" {
			f.t.Errorf("oauth grant_type = %q, want client_credentials", r.Form.Get("grant_type"))
		}
		w.WriteHeader(f.tokenStatus)
		_, _ = io.WriteString(w, f.tokenBody)
	})
	mux.HandleFunc("/api/v2/tailnet/-/keys", func(w http.ResponseWriter, r *http.Request) {
		f.gotBearer = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.failNoBearer && f.gotBearer == "" {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &f.gotKeyReq); err != nil {
			f.t.Errorf("key-create body did not decode: %v (%s)", err, body)
		}
		w.WriteHeader(f.keyStatus)
		_, _ = io.WriteString(w, f.keyBody)
	})
	return mux
}

func newFakeMinter(t *testing.T, f *fakeTailscale) *Minter {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	m := NewMinter("cid-123", "csecret-xyz")
	m.baseURL = srv.URL
	return m
}

func TestMint_Success(t *testing.T) {
	f := &fakeTailscale{
		t:           t,
		tokenStatus: http.StatusOK,
		tokenBody:   `{"access_token":"tok-abc","token_type":"Bearer"}`,
		keyStatus:   http.StatusOK,
		keyBody:     `{"id":"kID9","key":"tskey-auth-kID9-secretpart","capabilities":{"devices":{"create":{"tags":["tag:pop2"]}}}}`,
	}
	m := newFakeMinter(t, f)

	got, err := m.Mint(context.Background(), KeyRequest{
		Tags:          []string{"tag:pop2"},
		ExpirySeconds: 3600,
		Description:   "slagworks deploy enroll",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got.Key != "tskey-auth-kID9-secretpart" {
		t.Errorf("Key = %q, want the minted key", got.Key)
	}
	if got.ID != "kID9" {
		t.Errorf("ID = %q, want kID9", got.ID)
	}

	// OAuth leg: Basic auth with the client creds, auth_keys scope.
	if f.gotClientID != "cid-123" || f.gotClientSecret != "csecret-xyz" {
		t.Errorf("oauth basic auth = %q:%q, want cid-123:csecret-xyz", f.gotClientID, f.gotClientSecret)
	}
	if f.gotScope != "auth_keys" {
		t.Errorf("oauth scope = %q, want auth_keys", f.gotScope)
	}
	// Key leg: used the token as bearer.
	if f.gotBearer != "tok-abc" {
		t.Errorf("key-create bearer = %q, want tok-abc", f.gotBearer)
	}
	// Key shape: single-use, persistent node, pre-authorized, correct tag.
	c := f.gotKeyReq.Capabilities.Devices.Create
	if c.Reusable {
		t.Error("minted key is reusable; want single-use (reusable=false)")
	}
	if c.Ephemeral {
		t.Error("minted key is ephemeral; want persistent node (ephemeral=false)")
	}
	if !c.Preauthorized {
		t.Error("minted key is not preauthorized; want preauthorized=true")
	}
	if len(c.Tags) != 1 || c.Tags[0] != "tag:pop2" {
		t.Errorf("minted key tags = %v, want [tag:pop2]", c.Tags)
	}
	if f.gotKeyReq.ExpirySeconds != 3600 {
		t.Errorf("expirySeconds = %d, want 3600", f.gotKeyReq.ExpirySeconds)
	}
}

func TestMint_OAuthForbidden(t *testing.T) {
	// The real-world failure when the client lacks auth_keys scope.
	f := &fakeTailscale{
		t:           t,
		tokenStatus: http.StatusForbidden,
		tokenBody:   `{"message":"OAuth client cannot grant scopes \"auth_keys\""}`,
	}
	m := newFakeMinter(t, f)
	_, err := m.Mint(context.Background(), KeyRequest{Tags: []string{"tag:pop2"}, ExpirySeconds: 3600})
	if err == nil {
		t.Fatal("expected error on 403 oauth, got nil")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "auth_keys") {
		t.Errorf("error should surface the 403 and body, got: %v", err)
	}
}

func TestMint_TagNotPermitted(t *testing.T) {
	// The real-world failure when the client doesn't own the requested tag.
	f := &fakeTailscale{
		t:           t,
		tokenStatus: http.StatusOK,
		tokenBody:   `{"access_token":"tok-abc"}`,
		keyStatus:   http.StatusBadRequest,
		keyBody:     `{"message":"requested tags [tag:pop2] are invalid or not permitted"}`,
	}
	m := newFakeMinter(t, f)
	_, err := m.Mint(context.Background(), KeyRequest{Tags: []string{"tag:pop2"}, ExpirySeconds: 3600})
	if err == nil {
		t.Fatal("expected error on tag-not-permitted, got nil")
	}
	if !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("error should surface the tag rejection, got: %v", err)
	}
}

func TestMint_Validation(t *testing.T) {
	m := NewMinter("c", "s")
	if _, err := m.Mint(context.Background(), KeyRequest{ExpirySeconds: 3600}); err == nil {
		t.Error("expected error for empty tags")
	}
	if _, err := m.Mint(context.Background(), KeyRequest{Tags: []string{"tag:pop2"}}); err == nil {
		t.Error("expected error for non-positive ExpirySeconds")
	}
}
