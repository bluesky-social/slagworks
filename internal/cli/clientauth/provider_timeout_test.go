package clientauth

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// The tests below run each operation that talks to the identity provider
// against a provider that accepts the request and never answers: a wedged
// backend behind a proxy that does not time out. slagctl calls these with
// its command's context, which has no deadline, and go-oidc and oauth2 send
// the requests with http.DefaultClient, which has no timeout, so each must be
// bounded by providerTimeout and fail with an error that names the provider
// and says it did not answer. Without the bound, a refresh hangs holding the
// token-cache lock and every other invocation queues behind it.

const (
	pathDiscovery = "/.well-known/openid-configuration"
	pathToken     = "/token"
	pathDevice    = "/device"
)

// wedgedProvider is a fake identity provider. Requests to the paths in hang
// are accepted and never answered while the test runs. The other paths
// answer: discovery advertises the endpoints, the device endpoint issues a
// code and the token endpoint refuses every grant.
type wedgedProvider struct {
	*httptest.Server
	mu       sync.Mutex
	requests map[string]int
}

func newWedgedProvider(t *testing.T, hang ...string) *wedgedProvider {
	t.Helper()
	p := &wedgedProvider{requests: map[string]int{}}
	release := make(chan struct{})
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests[r.URL.Path]++
		p.mu.Unlock()
		for _, h := range hang {
			if r.URL.Path == h {
				<-release
				return
			}
		}
		switch r.URL.Path {
		case pathDiscovery:
			writeJSON(w, http.StatusOK, map[string]any{
				"issuer":                        p.URL,
				"authorization_endpoint":        p.URL + "/authorize",
				"token_endpoint":                p.URL + pathToken,
				"device_authorization_endpoint": p.URL + pathDevice,
				"jwks_uri":                      p.URL + "/jwks",
			})
		case pathDevice:
			writeJSON(w, http.StatusOK, map[string]any{
				"device_code": "dc", "user_code": "UC", "verification_uri": p.URL + "/verify",
				"expires_in": 600, "interval": 1,
			})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		}
	}))
	// Cleanups run last-in first-out: release the held handlers, then Close
	// (which waits for them).
	t.Cleanup(p.Close)
	t.Cleanup(func() { close(release) })
	return p
}

func (p *wedgedProvider) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[path]
}

// shortProviderTimeout shortens providerTimeout for one test, so that a test
// of the bound does not wait out the production 30s.
func shortProviderTimeout(t *testing.T) {
	t.Helper()
	old := providerTimeout
	providerTimeout = 250 * time.Millisecond
	t.Cleanup(func() { providerTimeout = old })
}

// seedExpiredSession caches an expired access token with a refresh token, so
// the next Bearer or CachedBearer refreshes.
func seedExpiredSession(t *testing.T, cfg Config) {
	t.Helper()
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeCache(path, session{binding: cfg.binding(), Token: &oauth2.Token{AccessToken: "at-0", RefreshToken: "rt-0", TokenType: "Bearer",
		Expiry: time.Now().Add(-time.Minute)}}); err != nil {
		t.Fatal(err)
	}
}

// expectNoAnswer runs call and requires it to give up well within the test's
// ceiling (providerTimeout is 250ms here) with an error that names the
// identity provider and says that it did not answer.
func expectNoAnswer(t *testing.T, issuer string, call func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("succeeded against an identity provider that never answered")
		}
		if !strings.Contains(err.Error(), issuer) || !strings.Contains(err.Error(), "did not answer") {
			t.Fatalf("error does not say that the identity provider %s did not answer: %v", issuer, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("still waiting on an identity provider that never answers after 10s")
	}
}

// TestCachedBearer_RefreshGetsNoAnswer: the refresh (the token-endpoint round
// trip made while holding the token-cache lock) gives up. One timeout bounds
// the whole refresh: oauth2 retries a failed token request with the other
// client-auth style, and that retry must not get a timeout of its own, which
// would double the time the lock is held.
func TestCachedBearer_RefreshGetsNoAnswer(t *testing.T) {
	shortProviderTimeout(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	idp := newWedgedProvider(t, pathToken)
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: idp.URL, ClientID: "c", LoopbackPort: 8765}
	seedExpiredSession(t, cfg)

	expectNoAnswer(t, idp.URL, func() error {
		_, err := CachedBearer(context.Background(), cfg)
		return err
	})
	if n := idp.count(pathToken); n != 1 {
		t.Fatalf("the refresh sent %d token requests, want 1: the auth-style retry ran on a fresh timeout", n)
	}
}

// TestBearer_DiscoveryGetsNoAnswer: Bearer, which every server-touching
// slagctl command calls, gives up when the whole provider is wedged, so
// discovery hangs too.
func TestBearer_DiscoveryGetsNoAnswer(t *testing.T) {
	shortProviderTimeout(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	idp := newWedgedProvider(t, pathDiscovery, pathToken)
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: idp.URL, ClientID: "c", Device: true, Stderr: io.Discard}
	seedExpiredSession(t, cfg)

	expectNoAnswer(t, idp.URL, func() error {
		_, err := Bearer(context.Background(), cfg)
		return err
	})
}

// TestBearer_RefreshGetsNoAnswerStartsNoLogin: when the refresh gets no
// answer, Bearer reports that instead of falling back to the interactive
// login, which would open a browser (or start a device flow) against the
// same provider and then wait on it again.
func TestBearer_RefreshGetsNoAnswerStartsNoLogin(t *testing.T) {
	shortProviderTimeout(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	idp := newWedgedProvider(t, pathToken)
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: idp.URL, ClientID: "c", Device: true, Stderr: io.Discard}
	seedExpiredSession(t, cfg)

	expectNoAnswer(t, idp.URL, func() error {
		_, err := Bearer(context.Background(), cfg)
		return err
	})
	if n := idp.count(pathDevice); n != 0 {
		t.Fatalf("Bearer started a device login (%d device-authorization requests) after the refresh got no answer", n)
	}
}

// TestLogin_DeviceAuthorizationGetsNoAnswer: `slagctl auth login --device`
// gives up when the device-authorization request gets no answer.
func TestLogin_DeviceAuthorizationGetsNoAnswer(t *testing.T) {
	shortProviderTimeout(t)
	idp := newWedgedProvider(t, pathDevice)
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: idp.URL, ClientID: "c", Device: true, Stderr: io.Discard}

	expectNoAnswer(t, idp.URL, func() error {
		_, err := Login(context.Background(), cfg)
		return err
	})
}

// TestLogin_CodeExchangeGetsNoAnswer: `slagctl auth login` gives up when the
// authorization-code exchange gets no answer. The test plays an operator
// whose browser did not open: it reads the authorize URL that login prints
// and sends the provider's redirect to the loopback callback. PATH is
// emptied so that openBrowser finds no browser to open.
func TestLogin_CodeExchangeGetsNoAnswer(t *testing.T) {
	shortProviderTimeout(t)
	t.Setenv("PATH", t.TempDir())
	idp := newWedgedProvider(t, pathToken)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	prompts, promptWriter := io.Pipe()
	t.Cleanup(func() { _ = promptWriter.Close() })
	go func() {
		lines := bufio.NewScanner(prompts)
		for lines.Scan() {
			line := strings.TrimSpace(lines.Text())
			if !strings.HasPrefix(line, idp.URL+"/authorize?") {
				continue
			}
			authURL, err := url.Parse(line)
			if err != nil {
				continue
			}
			q := authURL.Query()
			callback := q.Get("redirect_uri") + "?" + url.Values{"state": {q.Get("state")}, "code": {"code-1"}}.Encode()
			if resp, err := http.Get(callback); err == nil {
				_ = resp.Body.Close()
			}
		}
	}()
	cfg := Config{AppName: "slagctl", API: "https://api.example", Issuer: idp.URL, ClientID: "c", LoopbackPort: port, Stderr: promptWriter}

	expectNoAnswer(t, idp.URL, func() error {
		_, err := Login(context.Background(), cfg)
		return err
	})
	if n := idp.count(pathToken); n == 0 {
		t.Fatal("login never reached the code exchange")
	}
}
