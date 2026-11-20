package clientauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// deviceProvider is a fake identity provider for the device-authorization grant.
// Its device page (the verification_uri it hands out) is served by page; a
// nil page answers 404, like a provider that issues device codes but serves
// no page to enter them on.
type deviceProvider struct {
	srv   *httptest.Server
	polls atomic.Int32 // device_code polls of the token endpoint
}

type deviceProviderOpts struct {
	notAdvertised bool             // discovery omits device_authorization_endpoint
	endpoint404   bool             // the device endpoint answers 404
	page          http.HandlerFunc // serves /device; nil answers 404
	approve       bool             // token polls succeed; otherwise authorization_pending
}

func newDeviceProvider(t *testing.T, o deviceProviderOpts) *deviceProvider {
	t.Helper()
	p := &deviceProvider{}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		d := map[string]any{
			"issuer":                 p.srv.URL,
			"authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint":         p.srv.URL + "/token",
			"jwks_uri":               p.srv.URL + "/jwks",
		}
		if !o.notAdvertised {
			d["device_authorization_endpoint"] = p.srv.URL + "/device-code"
		}
		writeJSON(w, http.StatusOK, d)
	})
	mux.HandleFunc("/device-code", func(w http.ResponseWriter, r *http.Request) {
		if o.endpoint404 {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code": "dc", "user_code": "ABCD-EFGH",
			"verification_uri": p.srv.URL + "/device", "expires_in": 3, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		p.polls.Add(1)
		if !o.approve {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "authorization_pending"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "at", "refresh_token": "rt", "token_type": "Bearer", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		if o.page == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found"))
			return
		}
		o.page(w, r)
	})
	mux.HandleFunc("/enter-code", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("enter your code")) })
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *deviceProvider) login(ctx context.Context, stderr io.Writer) error {
	_, err := Login(ctx, Config{AppName: "slagctl", API: "https://api.example", Issuer: p.srv.URL, ClientID: "c", Device: true, Stderr: stderr})
	return err
}

// TestLoginDevice_UnavailableFlowNamesTheWayOut: when the device-code flow
// cannot complete, device login fails at once with an error that says what
// is missing and to log in without --device.
//
// The page rows are the shape that used to hang: the provider advertises
// device_authorization_endpoint and issues a code, but the verification page
// it names answers 404 (Authentik without a brand-level device-code flow, or
// a proxy in front of the provider that does not route the page). The client
// printed the dead URL, polled authorization_pending until the code expired
// and returned "waiting for device approval: context deadline exceeded".
func TestLoginDevice_UnavailableFlowNamesTheWayOut(t *testing.T) {
	redirectTo := func(path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, path, http.StatusFound) }
	}
	cases := []struct {
		name string
		opts deviceProviderOpts
		want []string // substrings beyond "without --device" and the command
	}{
		{name: "device endpoint not advertised", opts: deviceProviderOpts{notAdvertised: true},
			want: []string{"advertises no device-code endpoint"}},
		{name: "device endpoint answers 404", opts: deviceProviderOpts{endpoint404: true},
			want: []string{"/device-code", "404"}},
		{name: "device page answers 404", opts: deviceProviderOpts{},
			want: []string{"/device answers 404 Not Found"}},
		{name: "device page redirects to a 404", opts: deviceProviderOpts{page: redirectTo("/nowhere")},
			want: []string{"/device answers 404 Not Found"}},
		{name: "device page answers 410", opts: deviceProviderOpts{page: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) }},
			want: []string{"/device answers 410 Gone"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newDeviceProvider(t, tc.opts)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var stderr bytes.Buffer
			err := p.login(ctx, &stderr)
			if err == nil {
				t.Fatal("device login succeeded although the device-code flow cannot complete")
			}
			for _, s := range append([]string{"without --device", "`slagctl auth login`"}, tc.want...) {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error does not contain %q: %v", s, err)
				}
			}
			if n := p.polls.Load(); n != 0 {
				t.Errorf("polled the token endpoint %d times for an approval that cannot happen", n)
			}
			if strings.Contains(stderr.String(), "To authenticate") {
				t.Errorf("told the operator to open a page that does not exist:\n%s", stderr.String())
			}
		})
	}
}

// TestLoginDevice_PageCheckDoesNotRefuseAWorkingFlow: the check of the
// device page refuses only a page that answers 404 or 410. A page that
// redirects to a live one, one that turns away scripted clients (403), and
// one this host fails to fetch (it may still open on the operator's other
// device) all go on to the normal flow.
func TestLoginDevice_PageCheckDoesNotRefuseAWorkingFlow(t *testing.T) {
	cases := []struct {
		name string
		opts deviceProviderOpts
	}{
		{name: "page answers 200", opts: deviceProviderOpts{page: func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("enter your code")) }}},
		{name: "page redirects to a live page", opts: deviceProviderOpts{page: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/enter-code", http.StatusFound)
		}}},
		{name: "page refuses scripted clients", opts: deviceProviderOpts{page: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }}},
		{name: "page fetch fails", opts: deviceProviderOpts{page: func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close() // drop the connection without a response
			}
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.opts.approve = true
			p := newDeviceProvider(t, tc.opts)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var stderr bytes.Buffer
			if err := p.login(ctx, &stderr); err != nil {
				t.Fatalf("device login: %v", err)
			}
			if !strings.Contains(stderr.String(), "To authenticate, open:") {
				t.Errorf("the operator was not shown the device page:\n%s", stderr.String())
			}
		})
	}
}
