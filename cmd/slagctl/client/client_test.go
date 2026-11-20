package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/cmd/slagctl/client"
)

func TestGet_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/sites" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]map[string]string{{"id": "abc", "name": "pop3"}})
	}))
	defer ts.Close()

	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client()})
	var out []map[string]string
	if err := c.Get(context.Background(), "/api/v1/sites", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["name"] != "pop3" {
		t.Fatalf("unexpected response: %v", out)
	}
}

func TestGet_WithQuery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "commissioned" {
			t.Errorf("expected state=commissioned, got %q", r.URL.Query().Get("state"))
		}
		w.Write([]byte("[]"))
	}))
	defer ts.Close()

	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client()})
	var out []any
	q := url.Values{"state": {"commissioned"}}
	if err := c.Get(context.Background(), "/api/v1/machines", q, &out); err != nil {
		t.Fatal(err)
	}
}

func TestPost_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "pop3" {
			t.Errorf("unexpected body: %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": "123", "name": "pop3"})
	}))
	defer ts.Close()

	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client()})
	var out map[string]string
	if err := c.Post(context.Background(), "/api/v1/sites", map[string]string{"name": "pop3"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["id"] != "123" {
		t.Fatalf("unexpected response: %v", out)
	}
}

func TestDelete_Success(t *testing.T) {
	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()

	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client()})
	if err := c.Delete(context.Background(), "/api/v1/machines/123"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("handler not called")
	}
}

func TestAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "machine not found"})
	}))
	defer ts.Close()

	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client()})
	err := c.Get(context.Background(), "/api/v1/machines/nope", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*client.APIError)
	if !ok {
		t.Fatalf("expected *client.APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "machine not found" {
		t.Errorf("unexpected message: %q", apiErr.Message)
	}
}

// TestBearer_AskedOnEveryRequest: the client asks its bearer source for a
// token before each request, so a command that outlives one access token
// sends the refreshed one instead of the token it started with.
func TestBearer_AskedOnEveryRequest(t *testing.T) {
	var got []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte("{}"))
	}))
	defer ts.Close()

	n := 0
	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client(), Bearer: func(context.Context) (string, error) {
		n++
		return fmt.Sprintf("token-%d", n), nil
	}})
	ctx := context.Background()
	if err := c.Get(ctx, "/", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Post(ctx, "/", map[string]string{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "/"); err != nil {
		t.Fatal(err)
	}
	want := []string{"Bearer token-1", "Bearer token-2", "Bearer token-3"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Authorization headers = %q, want %q", got, want)
	}
}

// TestBearer_ErrorFailsTheRequest: when the bearer source cannot produce a
// token the request is not sent unauthenticated; the source's error comes
// back unchanged, so callers can still match it with errors.Is.
func TestBearer_ErrorFailsTheRequest(t *testing.T) {
	sent := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = true
	}))
	defer ts.Close()

	errNoToken := errors.New("no token")
	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client(), Bearer: func(context.Context) (string, error) {
		return "", errNoToken
	}})
	if err := c.Get(context.Background(), "/", nil, nil); !errors.Is(err, errNoToken) {
		t.Fatalf("Get error = %v, want the bearer source's error", err)
	}
	if sent {
		t.Fatal("the request was sent although the bearer source failed")
	}
}

// TestUnauthorized_RefreshesAndResendsOnce: a 401 is answered with one call
// to the refresher, naming the refused token, and the request is resent with
// its body and the new token, which later requests carry from the start.
func TestUnauthorized_RefreshesAndResendsOnce(t *testing.T) {
	type request struct{ auth, body string }
	var mu sync.Mutex
	var got []request
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, request{r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer new" {
			// Plain text, as an authenticating proxy in front of the API answers.
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("jwt expired"))
			return
		}
		w.Write([]byte("{}"))
	}))
	defer ts.Close()

	// cached stands for the token cache: the bearer source reads it for
	// every request and the refresher writes its replacement there.
	cached := "old"
	var rejected []string
	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client(),
		Bearer: func(context.Context) (string, error) { return cached, nil },
		Refresh: func(_ context.Context, tok string) (string, error) {
			rejected = append(rejected, tok)
			cached = "new"
			return cached, nil
		}})
	ctx := context.Background()
	if err := c.Post(ctx, "/api/v1/machines/x/power", map[string]string{"action": "cycle"}, nil); err != nil {
		t.Fatalf("POST after a refresh: %v", err)
	}
	if err := c.Get(ctx, "/api/v1/machines", nil, nil); err != nil {
		t.Fatalf("GET after a refresh: %v", err)
	}

	if len(rejected) != 1 || rejected[0] != "old" {
		t.Errorf("refresher called with %q, want once with the refused token", rejected)
	}
	want := []request{
		{"Bearer old", `{"action":"cycle"}`},
		{"Bearer new", `{"action":"cycle"}`},
		{"Bearer new", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("server saw %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestUnauthorized_StopsRefreshingWhenItDoesNotHelp: when a refresh fails, or
// the API refuses the refreshed token too, the client stops asking: one
// refresh per client, not one per request (a watch loop would otherwise call
// the identity provider on every tick). Every refusal names the command that
// fixes it, and wraps the refresher's error when there was one, so a caller
// that retries can tell a sign-in that is gone from a server refusing a
// token it should accept.
func TestUnauthorized_StopsRefreshingWhenItDoesNotHelp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"message": "invalid token"})
	}))
	defer ts.Close()

	for _, tc := range []struct {
		name       string
		refreshErr error
	}{
		{name: "refresh fails", refreshErr: errors.New("invalid_grant")},
		{name: "refreshed token refused too"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refreshes := 0
			c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client(),
				Bearer: func(context.Context) (string, error) { return "t-0", nil },
				Refresh: func(context.Context, string) (string, error) {
					refreshes++
					return fmt.Sprintf("t-%d", refreshes), tc.refreshErr
				}})
			for i := 0; i < 3; i++ {
				err := c.Get(context.Background(), "/api/v1/workers", nil, nil)
				var apiErr *client.APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
					t.Fatalf("request %d: err = %v, want the API's 401", i, err)
				}
				for _, want := range []string{"invalid token (HTTP 401)", "slagctl auth login"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("request %d: error %q does not contain %q", i, err, want)
					}
				}
				if got := errors.Unwrap(apiErr); got != tc.refreshErr {
					t.Errorf("request %d: the 401 wraps %v, want the refresher's error %v", i, got, tc.refreshErr)
				}
			}
			if refreshes != 1 {
				t.Errorf("refreshes = %d over three refused requests, want 1", refreshes)
			}
		})
	}
}

// TestForbidden_IsNotRefreshed: a 403 means the token is valid but lacks a
// group; a refresh cannot change that, and the server's message already
// names the fix.
func TestForbidden_IsNotRefreshed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"message": "not authorized: missing group"})
	}))
	defer ts.Close()

	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client(),
		Bearer: func(context.Context) (string, error) { return "t-0", nil },
		Refresh: func(context.Context, string) (string, error) {
			t.Error("a 403 was answered with a refresh")
			return "t-1", nil
		}})
	err := c.Get(context.Background(), "/api/v1/workers", nil, nil)
	if err == nil || err.Error() != "not authorized: missing group (HTTP 403)" {
		t.Fatalf("err = %v, want the server's 403 message unchanged", err)
	}
}

func TestInsecureWarning(t *testing.T) {
	tests := []struct {
		url      string
		wantWarn bool
	}{
		{"http://localhost:8081", false},
		{"http://127.0.0.1:8081", false},
		{"https://slagworks.example.com", false},
		{"http://oob-gw-1.example.com:8081", true},
	}
	for _, tt := range tests {
		c := client.New(client.Config{BaseURL: tt.url})
		warn := c.InsecureWarning()
		if tt.wantWarn && warn == "" {
			t.Errorf("url %s: expected warning, got none", tt.url)
		}
		if !tt.wantWarn && warn != "" {
			t.Errorf("url %s: unexpected warning: %q", tt.url, warn)
		}
	}
}

// TestTimeoutErrorIsActionable pins the timeout error rewrite: the
// operator-facing message must say how long we waited, name the target,
// and point at the op-list / server-log next steps — not just Go's
// "context deadline exceeded (Client.Timeout exceeded while awaiting
// headers)".
func TestTimeoutErrorIsActionable(t *testing.T) {
	blocked := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked // hold the response past the client timeout
	}))
	defer ts.Close()
	// LIFO: must release the handler BEFORE ts.Close() waits on it.
	defer close(blocked)

	hc := ts.Client()
	hc.Timeout = 50 * time.Millisecond
	c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: hc})
	err := c.Post(context.Background(), "/api/v1/machines/x/power", map[string]string{"action": "cycle"}, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	for _, want := range []string{
		"did not respond within",
		"POST /api/v1/machines/x/power",
		"slagctl operation list",
		"kubectl logs",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%s", want, err)
		}
	}
}
