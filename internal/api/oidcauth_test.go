package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/beyond/tokenverify"
)

// fakeVerifier is a tokenverify.Verifier that maps a raw token string to a
// canned result, so the middleware can be exercised without any real JWT/IdP.
type fakeVerifier struct {
	claims map[string]*tokenverify.Claims
	err    map[string]error
}

func (f *fakeVerifier) Verify(_ context.Context, raw string) (*tokenverify.Claims, error) {
	if e, ok := f.err[raw]; ok {
		return nil, e
	}
	if c, ok := f.claims[raw]; ok {
		return c, nil
	}
	return nil, errors.New("unknown token")
}

// mountGuarded wires a single guarded route through the production middleware
// factoring (api.Group → oidcAuth), returning an echo instance to serve.
func mountGuarded(s *Server) *echo.Echo {
	e := echo.New()
	g := e.Group("/api/v1", s.oidcAuth)
	g.GET("/ping", func(c *echo.Context) error {
		// Echo the operator identity so tests can assert it was stashed.
		op := operatorFromContext(c)
		if op == nil {
			return c.String(http.StatusOK, "no-operator")
		}
		return c.String(http.StatusOK, "ok:"+op.Subject)
	})
	return e
}

func TestOIDCAuth_Gate(t *testing.T) {
	fv := &fakeVerifier{
		claims: map[string]*tokenverify.Claims{
			"good-platform": {Subject: "u-plat", Email: "p@x", Groups: []string{"authentik Admins", "platform"}},
			"good-nogroup":  {Subject: "u-nope", Email: "n@x", Groups: []string{"engineering"}},
			"empty-groups":  {Subject: "u-empty", Groups: nil},
		},
		err: map[string]error{
			"bad-sig": errors.New("signature verification failed"),
		},
	}
	// The reject paths log via s.log; give it a discard logger so the
	// hand-built literal doesn't nil-panic (NewServer always sets one).
	s := &Server{verifier: fv, authz: tokenverify.RequireAnyGroup("platform"),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e := mountGuarded(s)

	cases := []struct {
		name   string
		header string
		want   int
		body   string
	}{
		{"no header", "", http.StatusUnauthorized, ""},
		{"non-bearer", "Basic abc", http.StatusUnauthorized, ""},
		{"empty bearer", "Bearer ", http.StatusUnauthorized, ""},
		{"bad signature", "Bearer bad-sig", http.StatusUnauthorized, ""},
		{"valid but wrong group", "Bearer good-nogroup", http.StatusForbidden, ""},
		{"valid but no groups", "Bearer empty-groups", http.StatusForbidden, ""},
		{"valid platform member", "Bearer good-platform", http.StatusOK, "ok:u-plat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.body != "" && w.Body.String() != tc.body {
				t.Errorf("body=%q want=%q", w.Body.String(), tc.body)
			}
		})
	}
}

// makeJWT builds an UNSIGNED (alg=none-style) JWT with the given payload claims
// so tests can exercise the unverified diagnostic decode. The signature segment
// is arbitrary — unverifiedTokenFields never checks it.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := b64(map[string]any{"alg": "none", "typ": "JWT"})
	return header + "." + b64(claims) + ".sig"
}

func TestUnverifiedTokenFields(t *testing.T) {
	cases := []struct {
		name            string
		raw             string
		iss, aud, expEx string
	}{
		{
			name: "well-formed with string aud",
			raw:  makeJWT(t, map[string]any{"iss": "https://idp.example.com/application/o/slagworks/", "aud": "client-abc", "exp": 1783781035}),
			iss:  "https://idp.example.com/application/o/slagworks/", aud: "client-abc", expEx: "1783781035",
		},
		{
			name: "issuer skew (tailnet host)",
			raw:  makeJWT(t, map[string]any{"iss": "https://idp.example.com/application/o/slagworks/", "aud": "client-abc", "exp": 1783781035}),
			iss:  "https://idp.example.com/application/o/slagworks/", aud: "client-abc", expEx: "1783781035",
		},
		{"opaque non-JWT", "not-a-jwt", "", "", ""},
		{"two segments only", "aaa.bbb", "", "", ""},
		{"bad base64 payload", "aaa.!!!.ccc", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss, aud, exp := unverifiedTokenFields(tc.raw)
			if iss != tc.iss || aud != tc.aud || exp != tc.expEx {
				t.Errorf("got (iss=%q aud=%q exp=%q) want (iss=%q aud=%q exp=%q)", iss, aud, exp, tc.iss, tc.aud, tc.expEx)
			}
		})
	}
}

// TestOIDCAuth_LogsRejection asserts the middleware emits a diagnostic warn line
// naming the token's issuer on a verification failure — the signal that makes an
// issuer-skew rejection debuggable from the server logs (the reason this exists).
func TestOIDCAuth_LogsRejection(t *testing.T) {
	skewIss := "https://idp.example.com/application/o/slagworks/"
	badTok := makeJWT(t, map[string]any{"iss": skewIss, "aud": "client-abc", "exp": 1783781035})
	fv := &fakeVerifier{err: map[string]error{badTok: errors.New("oidc: id token issued by a different provider")}}

	var buf bytes.Buffer
	s := &Server{
		verifier:     fv,
		authz:        tokenverify.RequireAnyGroup("platform"),
		log:          slog.New(slog.NewTextHandler(&buf, nil)),
		oidcIssuer:   "https://idp.example.com/application/o/slagworks/",
		oidcAudience: "client-abc",
	}
	e := mountGuarded(s)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/ping", nil)
	req.Header.Set("Authorization", "Bearer "+badTok)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=401", w.Code)
	}
	logged := buf.String()
	for _, want := range []string{
		"token verification failed",
		"token_iss=" + skewIss,                 // the offending issuer is visible
		"expected_iss=https://idp.example.com", // next to what the server wanted
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("reject log missing %q\nfull log: %s", want, logged)
		}
	}
}

// (group-membership semantics — HasGroup, RequireAnyGroup fail-closed on
// empty — are tested in github.com/bluesky-social/beyond/tokenverify.)
