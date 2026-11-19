package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestBuildGlobal_SmokeTest verifies the production global startup
// path wires up correctly against a real Postgres. Originally added
// to catch "production global.go was missing DSN, causing WebSocket
// routes to not register" — the class of regression this test
// catches is "forgot to plumb a field through globalCmd → api.Config".
//
// Without NetBox configured, the WS hub + snapshot pipeline are
// intentionally NOT brought up (production path honors this: an
// admin-only slagctl instance can run NetBox-less). The smoke test
// covers both variants.
func TestBuildGlobal_SmokeTest(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	// testpg truncates; we need the DSN to hand to buildGlobal. Pull
	// it from the package accessor.
	dsn := testpg.DSN()
	if dsn == "" {
		t.Fatal("testpg DSN is empty; container not started")
	}
	_ = db // testpg.DB already ran migrations; buildGlobal will re-run idempotently

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("without NetBox — WS routes NOT registered", func(t *testing.T) {
		ctx := t.Context()

		srv, err := buildGlobal(ctx, log, globalConfig{
			DSN:          dsn,
			OIDCIssuer:   "https://authentik.example/application/o/slagworks/",
			OIDCAudience: "test-client-id",
			OIDCGroups:   []string{"operators"},
		})
		if err != nil {
			t.Fatalf("buildGlobal without NetBox: %v", err)
		}

		httpSrv := httptest.NewServer(srv.Echo())
		defer httpSrv.Close()

		// /healthz should exist. Without a Readiness hook the
		// fallback handler is registered which returns 200.
		assertStatus(t, httpSrv.URL+"/healthz", http.StatusOK)
		assertNotStatus(t, httpSrv.URL+"/healthz", http.StatusNotFound)

		// Without NetBox, WS hub isn't set; the route ought NOT
		// respond successfully. Expect 404 (route not registered) or
		// similar. What we're guarding against is the route being
		// wired to a nil hub and panicking.
		resp, err := http.Get(httpSrv.URL + "/api/v1/ws/site")
		if err != nil {
			t.Fatalf("GET /api/v1/ws/site: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("expected error status for WS route without NetBox, got %d", resp.StatusCode)
		}
	})

	t.Run("with NetBox stub — WS route registered and requires auth", func(t *testing.T) {
		nbStub := newNetBoxStub(t)
		defer nbStub.Close()

		ctx := t.Context()

		srv, err := buildGlobal(ctx, log, globalConfig{
			DSN:          dsn,
			OIDCIssuer:   "https://authentik.example/application/o/slagworks/",
			OIDCAudience: "test-client-id",
			OIDCGroups:   []string{"operators"},
			NetBoxURL:    nbStub.URL,
			NetBoxToken:  "stub-token",
		})
		if err != nil {
			t.Fatalf("buildGlobal with NetBox: %v", err)
		}

		httpSrv := httptest.NewServer(srv.Echo())
		defer httpSrv.Close()

		// Core routes that must exist in production. /healthz and
		// /readyz may return 503 immediately after startup because
		// the reconciler hasn't ticked yet — that's expected. What
		// we're checking is that the route is REGISTERED (not 404)
		// and returns a real response.
		assertStatus(t, httpSrv.URL+"/healthz", -1)
		assertStatus(t, httpSrv.URL+"/readyz", -1)
		assertStatus(t, httpSrv.URL+"/metrics", http.StatusOK)

		// WS route MUST exist. No token → 401 (route wired, auth
		// rejects). The regression #252 would manifest as a 404
		// here (route not wired at all).
		resp, err := http.Get(httpSrv.URL + "/api/v1/ws/site")
		if err != nil {
			t.Fatalf("GET /api/v1/ws/site: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			t.Error("WS route returned 404: route not registered (DSN-plumbing regression)")
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("WS route without token: got %d, want 401", resp.StatusCode)
		}

		// With a valid-looking token (still not in DB), expect 401
		// again — auth still rejects but the route is definitely
		// there.
		req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/api/v1/ws/site", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		resp2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/ws/site with a bearer: %v", err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Errorf("WS route bad token: got %d, want 401", resp2.StatusCode)
		}
	})

	// Global starts without a webhook secret (NetBox reconcile does not
	// need it), says once at startup that the webhook endpoint is
	// disabled, and rejects deliveries; with a secret it says nothing.
	t.Run("webhook disabled without a secret", func(t *testing.T) {
		const disabledLine = "NetBox webhook endpoint disabled"
		cases := []struct {
			name         string
			secret       string
			wantDisabled bool
			wantStatus   int
		}{
			{"no secret", "", true, http.StatusServiceUnavailable},
			{"secret set", "configured-secret", false, http.StatusForbidden},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				nbStub := newNetBoxStub(t)
				defer nbStub.Close()
				var buf bytes.Buffer
				srv, err := buildGlobal(t.Context(), slog.New(slog.NewTextHandler(&buf, nil)), globalConfig{
					DSN:           dsn,
					OIDCIssuer:    "https://authentik.example/application/o/slagworks/",
					OIDCAudience:  "test-client-id",
					OIDCGroups:    []string{"operators"},
					NetBoxURL:     nbStub.URL,
					NetBoxToken:   "stub-token",
					WebhookSecret: tc.secret,
				})
				if err != nil {
					t.Fatalf("buildGlobal: %v", err)
				}
				logged := buf.String()
				if got := strings.Count(logged, disabledLine); tc.wantDisabled && got != 1 {
					t.Errorf("startup log has %d %q lines, want 1:\n%s", got, disabledLine, logged)
				} else if !tc.wantDisabled && got != 0 {
					t.Errorf("startup log says the webhook is disabled with a secret set:\n%s", logged)
				}
				if tc.wantDisabled && !strings.Contains(logged, "SLAGWORKS_WEBHOOK_SECRET") {
					t.Errorf("startup log should name SLAGWORKS_WEBHOOK_SECRET:\n%s", logged)
				}

				httpSrv := httptest.NewServer(srv.Echo())
				defer httpSrv.Close()
				resp, err := http.Post(httpSrv.URL+"/api/v1/hooks/netbox", "application/json",
					strings.NewReader(`{"event":"deleted","object_type":"dcim.device","data":{"id":1}}`))
				if err != nil {
					t.Fatalf("POST /api/v1/hooks/netbox: %v", err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != tc.wantStatus {
					t.Errorf("unsigned delivery: got %d, want %d", resp.StatusCode, tc.wantStatus)
				}
			})
		}
	})

	// Global says once at startup whether credential reads are gated by
	// credential groups, and by how many.
	t.Run("credential-read gating startup line", func(t *testing.T) {
		cases := []struct {
			name       string
			credGroups []string
			want       string
		}{
			{"unset", nil, "device credential reads open to every operator"},
			{"set", []string{"cred-group-a", "cred-group-b"}, "device credential reads restricted to SLAGWORKS_OIDC_CREDENTIAL_GROUPS"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				_, err := buildGlobal(t.Context(), slog.New(slog.NewTextHandler(&buf, nil)), globalConfig{
					DSN:                  dsn,
					OIDCIssuer:           "https://authentik.example/application/o/slagworks/",
					OIDCAudience:         "test-client-id",
					OIDCGroups:           []string{"operators"},
					OIDCCredentialGroups: tc.credGroups,
				})
				if err != nil {
					t.Fatalf("buildGlobal: %v", err)
				}
				logged := buf.String()
				if got := strings.Count(logged, "device credential reads"); got != 1 {
					t.Errorf("startup log has %d credential-read lines, want 1:\n%s", got, logged)
				}
				if !strings.Contains(logged, tc.want) {
					t.Errorf("startup log missing %q:\n%s", tc.want, logged)
				}
				if tc.credGroups != nil && !strings.Contains(logged, "groups=2") {
					t.Errorf("startup log should count the credential groups:\n%s", logged)
				}
			})
		}
	})

	t.Run("missing OIDC config is an error", func(t *testing.T) {
		ctx := context.Background()
		_, err := buildGlobal(ctx, log, globalConfig{
			DSN: dsn,
		})
		if err == nil {
			t.Fatal("expected error for missing OIDC config")
		}
		if !strings.Contains(err.Error(), "OIDC") {
			t.Errorf("error should mention OIDC config: %v", err)
		}
	})

	t.Run("bad DSN surfaces an error", func(t *testing.T) {
		ctx := context.Background()
		_, err := buildGlobal(ctx, log, globalConfig{
			DSN:          "host=nonexistent.invalid port=1 user=x password=x dbname=x sslmode=disable connect_timeout=1",
			OIDCIssuer:   "https://authentik.example/application/o/slagworks/",
			OIDCAudience: "test-client-id",
			OIDCGroups:   []string{"operators"},
		})
		if err == nil {
			t.Fatal("expected error for bad DSN")
		}
	})
}

// assertStatus fetches url and checks the response status. wantStatus
// < 0 means "don't care, just that it responded".
func assertStatus(t *testing.T, url string, wantStatus int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	if wantStatus > 0 && resp.StatusCode != wantStatus {
		t.Errorf("GET %s: got %d, want %d", url, resp.StatusCode, wantStatus)
	}
}

// assertNotStatus is the negative assertion: the response must NOT
// have the given status. Used to check "route is registered" (not 404)
// without caring about whether it's 200/401/503.
func assertNotStatus(t *testing.T, url string, unwantedStatus int) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == unwantedStatus {
		t.Errorf("GET %s: got %d (not expected)", url, resp.StatusCode)
	}
}

// netBoxStub stands in for a real NetBox when buildGlobal's WS
// pipeline needs a configured client. It answers the minimal routes
// the client touches during construction + readiness polling: nothing
// happens at build time, but the reconciler may poll /api/status/ as
// a liveness check, and a 404/500 would be reported.
type netBoxStub struct {
	*httptest.Server
}

func newNetBoxStub(t *testing.T) *netBoxStub {
	t.Helper()
	mux := http.NewServeMux()
	// Accept anything NetBox-ish; return empty JSON objects for GETs
	// and 201/200 for writes. The purpose of this stub is solely to
	// satisfy any construction-time probes — we're not exercising
	// real NetBox behavior in this smoke test.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	})
	srv := httptest.NewServer(mux)
	return &netBoxStub{Server: srv}
}

// TestMain shares the testpg container with other tests in this
// package (there's only site_test.go here currently, which has its
// own TestMain — Go will use the alphabetically-first file's if
// both are declared; we rely on this being the first).
func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Shutdown()
	os.Exit(code)
}

// Both group settings are comma-separated lists: entries are trimmed and
// blanks dropped, so a blank value is no groups at all.
func TestParseGroupList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"a", []string{"a"}},
		{" a , b ", []string{"a", "b"}},
		{"a,,b,", []string{"a", "b"}},
	}
	for _, tc := range cases {
		if got := parseGroupList(tc.raw); !slices.Equal(got, tc.want) {
			t.Errorf("parseGroupList(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// --oidc-credential-groups defaults to SLAGWORKS_OIDC_CREDENTIAL_GROUPS and
// the flag overrides it; either value goes through parseGroupList.
func TestGlobalCmd_CredentialGroupsFlag(t *testing.T) {
	t.Setenv("SLAGWORKS_OIDC_CREDENTIAL_GROUPS", " cred-group-a, ,cred-group-b ")
	cmd := globalCmd()
	f := cmd.Flags().Lookup("oidc-credential-groups")
	if f == nil {
		t.Fatal("globalCmd has no --oidc-credential-groups flag")
	}
	if got, want := parseGroupList(f.Value.String()), []string{"cred-group-a", "cred-group-b"}; !slices.Equal(got, want) {
		t.Errorf("from env: got %q, want %q", got, want)
	}
	if err := cmd.Flags().Parse([]string{"--oidc-credential-groups", "cred-group-c ,"}); err != nil {
		t.Fatal(err)
	}
	if got, want := parseGroupList(f.Value.String()), []string{"cred-group-c"}; !slices.Equal(got, want) {
		t.Errorf("from flag: got %q, want %q", got, want)
	}
}

// Without an operator group every token would be refused, so global must
// refuse to start and say which setting is missing.
func TestBuildGlobal_NoRequiredGroupsIsConfigError(t *testing.T) {
	_, err := buildGlobal(context.Background(), slog.New(slog.DiscardHandler), globalConfig{
		OIDCIssuer:   "https://idp.example/application/o/slagworks/",
		OIDCAudience: "client-abc",
	})
	if err == nil || !strings.Contains(err.Error(), "SLAGWORKS_OIDC_REQUIRED_GROUPS") {
		t.Fatalf("want a config error naming SLAGWORKS_OIDC_REQUIRED_GROUPS, got %v", err)
	}
}
