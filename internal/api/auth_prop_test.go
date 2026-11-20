package api

// Authentication and authorization properties of the route table.
//
// docs/dev/api.md, "Auth": every route is in one of the classes read, admin,
// creds, worker, hmac or none. These tests build the production router with
// every optional component wired (so every conditional route is registered)
// and check, for every registered route and for generated variations of
// method, path and credential placement, that nothing outside the documented
// `none` class answers without the credential its class requires.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/ws"
)

const propWebhookSecret = "prop-webhook-secret"

// propFullEnv builds the production router with every optional component
// present: NetBox, the secret store, the hub, the webhook receiver, the
// stream stores, readiness and metrics. logSink, when non-nil, receives the
// server's and the hub's log output.
func propFullEnv(t *testing.T, nb *propNetBox, logSink io.Writer) (*propEnv, *ws.Hub) {
	t.Helper()
	if nb == nil {
		nb = newPropNetBox()
	}
	if logSink == nil {
		logSink = io.Discard
	}
	log := slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var hub *ws.Hub
	env := newPropEnv(t, func(cfg *Config) {
		hub = ws.NewHub(ws.HubConfig{Auth: cfg.Store, Rebuilder: staticRebuilder{model.SiteSnapshot{SeqNum: 1}}, Logger: log})
		wh := netbox.NewWebhookHandler(nil, nil, propWebhookSecret)
		wh.SetLogger(log)
		cfg.NetBox = nb.client(t)
		cfg.BMCSecrets = &awssecrets.FakeReader{}
		cfg.WSHub = hub
		cfg.WebhookHandler = wh
		cfg.WebhookSecret = propWebhookSecret
		cfg.Readiness = &fakeReadiness{done: true}
		cfg.PrometheusRegistry = prometheus.NewRegistry()
		cfg.PowerStatusRPC = fakeOutletRPC{}
		cfg.SnapshotBuilder = fakePreflightBuilder{}
		cfg.WorkerLiveness = fakeWorkerLiveness{}
		cfg.WorkerTools = fakeWorkerTools{}
		cfg.OIDCIssuer = "https://idp.example/application/o/slagworks/"
		cfg.OIDCAudience = "slagworks-cli"
	})
	env.srv.SetLogger(log)
	return env, hub
}

// propAuthClass is a route's documented class.
type propAuthClass string

// propDocumentedRoutes parses the route tables of docs/dev/api.md into
// "METHOD /full/path" -> auth class.
func propDocumentedRoutes(t *testing.T) map[string]propAuthClass {
	t.Helper()
	raw, err := os.ReadFile("../../docs/dev/api.md")
	if err != nil {
		t.Fatalf("read docs/dev/api.md: %v", err)
	}
	row := regexp.MustCompile("(?m)^\\| (GET|POST|PUT|PATCH|DELETE) \\| `([^`]+)` \\| (read|admin|creds|worker|hmac|none) \\|")
	wsHead := regexp.MustCompile("(?m)^### `([^`]+)` \\(auth: `(read|admin|creds|worker|hmac|none)`\\)")
	out := map[string]propAuthClass{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		path := m[2]
		if !strings.HasPrefix(path, "/api/v1") && path != "/healthz" && path != "/readyz" && path != "/metrics" {
			path = "/api/v1" + path // "All paths are under /api/v1."
		}
		out[m[1]+" "+path] = propAuthClass(m[3])
	}
	for _, m := range wsHead.FindAllStringSubmatch(string(raw), -1) {
		out["GET "+m[1]] = propAuthClass(m[2])
	}
	return out
}

func propRegisteredRoutes(e *echo.Echo) []echo.RouteInfo {
	var routes []echo.RouteInfo
	for _, r := range e.Router().Routes() {
		// A group with middleware registers its own not-found routes (so an
		// unknown path under /api/v1 answers 401, not 404); they are not API
		// routes.
		if r.Method == echo.RouteNotFound {
			continue
		}
		routes = append(routes, r)
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})
	return routes
}

// propConcretePath fills a route template's parameters.
func propConcretePath(tmpl string) string {
	segs := strings.Split(tmpl, "/")
	for i, s := range segs {
		switch {
		case s == ":n":
			segs[i] = "1"
		case strings.HasPrefix(s, ":"):
			segs[i] = pmMissingUUID
		}
	}
	return strings.Join(segs, "/")
}

// TestProp_RouteTableMatchesDocs holds docs/dev/api.md to its own claim: "If
// a route is registered there and missing here, or listed here and not
// registered, this page is wrong."
func TestProp_RouteTableMatchesDocs(t *testing.T) {
	env, _ := propFullEnv(t, nil, nil)
	documented := propDocumentedRoutes(t)
	if len(documented) < 50 {
		t.Fatalf("parsed only %d documented routes; the docs table format changed", len(documented))
	}
	registered := map[string]bool{}
	for _, r := range propRegisteredRoutes(env.srv.Echo()) {
		key := r.Method + " " + r.Path
		// The docs write every parameter of the script families as :name or :id.
		registered[key] = true
		if _, ok := documented[key]; !ok {
			t.Errorf("registered but not documented: %s", key)
		}
	}
	for key := range documented {
		if !registered[key] {
			t.Errorf("documented but not registered: %s", key)
		}
	}
}

type propCred struct {
	name string
	set  func(r *http.Request)
	// want is the status the operator middleware must answer with.
	want int
}

// propBadCreds are requests that do not carry a valid operator credential
// in the one place the contract reads it (the Authorization header).
func propBadCreds() []propCred {
	return []propCred{
		{"no credential", func(*http.Request) {}, 401},
		{"basic", func(r *http.Request) { r.Header.Set("Authorization", "Basic b3A6b3A=") }, 401},
		{"empty bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }, 401},
		{"bearer whitespace", func(r *http.Request) { r.Header.Set("Authorization", "Bearer    ") }, 401},
		{"unknown token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer not-a-token") }, 401},
		{"token without scheme", func(r *http.Request) { r.Header.Set("Authorization", propOperatorToken) }, 401},
		{"two tokens", func(r *http.Request) { r.Header.Set("Authorization", "Bearer not-a-token "+propOperatorToken) }, 401},
		{"token in query", func(r *http.Request) {
			q := r.URL.Query()
			q.Set("token", propOperatorToken)
			q.Set("access_token", propOperatorToken)
			r.URL.RawQuery = q.Encode()
		}, 401},
		{"token in cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: "token", Value: propOperatorToken})
			r.AddCookie(&http.Cookie{Name: "Authorization", Value: "Bearer " + propOperatorToken})
		}, 401},
		{"token in proxy headers", func(r *http.Request) {
			r.Header.Set("Proxy-Authorization", "Bearer "+propOperatorToken)
			r.Header.Set("X-Forwarded-Authorization", "Bearer "+propOperatorToken)
			r.Header.Set("X-Remote-User", "op")
			r.Header.Set("X-Forwarded-User", "op")
			r.Header.Set("X-Forwarded-Groups", "platform")
		}, 401},
		{"valid token outside the required group", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+propNoGroupToken) }, 403},
	}
}

// TestProp_EveryRouteRequiresItsCredential enumerates the registered routes.
func TestProp_EveryRouteRequiresItsCredential(t *testing.T) {
	env, _ := propFullEnv(t, nil, nil)
	documented := propDocumentedRoutes(t)
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		env.h.ServeHTTP(w, req)
		return w
	}
	counts := map[propAuthClass]int{}
	for _, route := range propRegisteredRoutes(env.srv.Echo()) {
		key := route.Method + " " + route.Path
		class, ok := documented[key]
		if !ok {
			class = "read" // undocumented: hold it to the operator class
		}
		counts[class]++
		path := propConcretePath(route.Path)
		switch class {
		case "none":
			w := serve(httptest.NewRequest(route.Method, path, nil))
			if w.Code != http.StatusOK {
				t.Errorf("%s (public): %d without a credential, want 200", key, w.Code)
			}
		case "worker":
			for _, q := range []string{"", "?token=", "?token=deadbeef", "?token=" + propOperatorToken} {
				req := httptest.NewRequest(route.Method, path+q, nil)
				req.Header.Set("Authorization", "Bearer "+propOperatorToken) // an operator is not a worker
				if w := serve(req); w.Code != http.StatusUnauthorized {
					t.Errorf("%s%s (worker): %d, want 401", key, q, w.Code)
				}
			}
		case "hmac":
			body := []byte(`{"event":"updated","object_type":"ipam.prefix","data":{}}`)
			mac := hmac.New(sha512.New, []byte(propWebhookSecret))
			mac.Write(body)
			good := hex.EncodeToString(mac.Sum(nil))
			for _, sig := range []string{"", "00", good[:len(good)-1], strings.ToUpper(good), good + "00", "sha512=" + good} {
				req := httptest.NewRequest(route.Method, path, bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+propOperatorToken) // an operator is not NetBox
				if sig != "" {
					req.Header.Set("X-Hook-Signature", sig)
				}
				if w := serve(req); w.Code != http.StatusForbidden {
					t.Errorf("%s (hmac) with signature %q: %d, want 403", key, sig, w.Code)
				}
			}
			req := httptest.NewRequest(route.Method, path, bytes.NewReader(body))
			req.Header.Set("X-Hook-Signature", good)
			if w := serve(req); w.Code != http.StatusOK {
				t.Errorf("%s (hmac) with the correct signature: %d, want 200", key, w.Code)
			}
		default: // read, admin, creds
			for _, cred := range propBadCreds() {
				req := httptest.NewRequest(route.Method, path, strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				cred.set(req)
				if w := serve(req); w.Code != cred.want {
					t.Errorf("%s with %s: %d, want %d\n  body: %s", key, cred.name, w.Code, cred.want, w.Body.String())
				}
			}
			// The operator credential is honored on the operator classes. A
			// creds route additionally needs a credential group.
			for token, wantAllowed := range map[string]bool{propOperatorToken: class != "creds", propCredToken: true} {
				req := httptest.NewRequest(route.Method, path, strings.NewReader(`{}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				w := serve(req)
				refused := w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden
				if refused == wantAllowed {
					t.Errorf("%s with %s: %d, allowed should be %v\n  body: %s", key, token, w.Code, wantAllowed, w.Body.String())
				}
			}
		}
	}
	t.Logf("routes by documented class: %v", counts)
	if counts["read"]+counts["admin"] < 50 || counts["none"] != 4 || counts["creds"] != 2 || counts["worker"] != 1 || counts["hmac"] != 1 {
		t.Errorf("unexpected class census %v: the route table or the docs changed shape", counts)
	}
}

// propPublicPaths are the only paths that may answer without a credential
// (docs/dev/api.md class `none`), plus the two with their own non-operator
// credential.
var propPublicPaths = map[string]bool{
	"/healthz": true, "/readyz": true, "/metrics": true, "/api/v1/client-config": true,
}

// TestProp_NoRequestShapeBypassesOperatorAuth generates requests against
// every protected route with variations of method, path spelling, parameter
// content and headers, none carrying a valid operator credential, and
// requires that the handler never runs: the only possible answers are the
// middleware's 401/403 or the router's 404/405.
func TestProp_NoRequestShapeBypassesOperatorAuth(t *testing.T) {
	env, _ := propFullEnv(t, nil, nil)
	documented := propDocumentedRoutes(t)
	var protected []echo.RouteInfo
	for _, r := range propRegisteredRoutes(env.srv.Echo()) {
		switch documented[r.Method+" "+r.Path] {
		case "none", "worker", "hmac":
		default:
			protected = append(protected, r)
		}
	}
	creds := propBadCreds()
	stats := newPropStats()
	defer stats.report(t)

	propCheck(t, 100, func(rt *rapid.T) {
		route := rapid.SampledFrom(protected).Draw(rt, "route")
		segs := strings.Split(route.Path, "/")
		for i, s := range segs {
			if strings.HasPrefix(s, ":") {
				segs[i] = rapid.SampledFrom([]string{
					pmMissingUUID, "1", "x", "a.b", "..", ".", "%2e%2e", "%2F", "client-config", "healthz",
					"x;y=1", "x%00", "x%20y", "x?", "ＡＢ", "-", "*",
				}).Draw(rt, fmt.Sprintf("param%d", i))
			}
		}
		path := strings.Join(segs, "/")
		switch rapid.SampledFrom([]string{"plain", "plain", "plain", "slash", "dslash", "upper", "dot", "updown", "encoded", "prefix", "suffix"}).Draw(rt, "spelling") {
		case "slash":
			path += "/"
		case "dslash":
			path = strings.Replace(path, "/api/v1/", "/api//v1//", 1)
		case "upper":
			path = strings.ToUpper(path)
		case "dot":
			path = strings.Replace(path, "/api/v1/", "/api/./v1/./", 1)
		case "updown":
			path = strings.Replace(path, "/api/v1/", "/api/v1/client-config/../", 1)
		case "encoded":
			path = strings.Replace(path, "/api/v1/", "/%61pi/v%31/", 1)
		case "prefix":
			path = "/healthz/.." + path
		case "suffix":
			path += rapid.SampledFrom([]string{".json", "%00", ";a=b", "/..", "/../client-config", "/../../../healthz"}).Draw(rt, "suffix")
		}
		method := route.Method
		if rapid.IntRange(0, 3).Draw(rt, "otherMethod") == 0 {
			method = rapid.SampledFrom([]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "QUERY", "PROPFIND", "get", "CONNECT"}).Draw(rt, "method")
		}
		u, err := url.Parse("http://slag.example" + path)
		if err != nil {
			rt.Skip("not a parseable URL")
		}
		req := &http.Request{
			Method: method, URL: u, Host: "slag.example", Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"action":"cycle"}`)), ContentLength: 18,
			RequestURI: u.RequestURI(), RemoteAddr: "192.0.2.7:4242",
		}
		req.Header.Set("Content-Type", "application/json")
		cred := rapid.SampledFrom(creds).Draw(rt, "cred")
		cred.set(req)
		req.RequestURI = req.URL.RequestURI()
		// Headers some stacks honor to rewrite the method or the path.
		if rapid.Bool().Draw(rt, "overrides") {
			req.Header.Set("X-HTTP-Method-Override", "GET")
			req.Header.Set("X-Original-URL", "/healthz")
			req.Header.Set("X-Rewrite-URL", "/api/v1/client-config")
			req.Header.Set("X-Forwarded-Prefix", "/healthz")
		}
		if rapid.Bool().Draw(rt, "upgrade") { // a WebSocket upgrade attempt
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		}

		w := httptest.NewRecorder()
		env.h.ServeHTTP(w, req)
		stats.add(fmt.Sprintf("status %d", w.Code))
		switch w.Code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
			return
		}
		// Anything else means some handler ran. That is acceptable only when
		// the request, as the server's router sees it, IS one of the public
		// routes (a dot-segment spelling can legitimately name one).
		if propPublicPaths[u.Path] && method == http.MethodGet {
			stats.add("resolved to a public route")
			return
		}
		rt.Fatalf("%s %s with %s answered %d: a handler ran without an operator credential\n  body: %s",
			method, path, cred.name, w.Code, w.Body.String())
	})
}
