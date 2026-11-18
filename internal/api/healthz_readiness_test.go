package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

type fakeReadiness struct {
	done  bool
	stale map[string]string
}

func (f *fakeReadiness) FirstReconcileComplete() bool  { return f.done }
func (f *fakeReadiness) StaleSites() map[string]string { return f.stale }

func TestHealthzReturns503BeforeFirstReconcile(t *testing.T) {
	e := echo.New()
	RegisterHealthz(e, &fakeReadiness{done: false}, "v0", "inst-1")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}

func TestHealthzReturns200WhenReady(t *testing.T) {
	e := echo.New()
	RegisterHealthz(e, &fakeReadiness{done: true}, "v0", "inst-1")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", w.Code)
	}
}

// Critical contract: stale sites appear in the body for operators but
// never flip the ALB health check to 503. The ALB stays green so long
// as FirstReconcileComplete() is true — stale sites are a per-site
// alarm, not a global outage.
func TestHealthz200WithStaleSitesInBody(t *testing.T) {
	e := echo.New()
	RegisterHealthz(e, &fakeReadiness{
		done:  true,
		stale: map[string]string{"old-site": "slug missing"},
	}, "v0", "inst-1")
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("stale must not flip 200->503, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "old-site") {
		t.Fatalf("stale site should appear in body: %s", w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["version"] != "v0" || body["instance_id"] != "inst-1" {
		t.Fatalf("unexpected metadata: %+v", body)
	}
}

func TestReadyzSharesHandler(t *testing.T) {
	e := echo.New()
	RegisterHealthz(e, &fakeReadiness{done: true}, "v0", "inst-1")
	req := httptest.NewRequest("GET", "/readyz", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
}
