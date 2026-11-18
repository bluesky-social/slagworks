package healthserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestHandleHealthz(t *testing.T) {
	s := New(":0", nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.handleHealthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ok" {
		t.Fatalf("expected ok, got %q", resp["status"])
	}
}

func TestHandleReadyz_NoCheck(t *testing.T) {
	s := New(":0", nil)

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "ready" {
		t.Fatalf("expected ready, got %q", resp["status"])
	}
}

func TestHandleReadyz_CheckPasses(t *testing.T) {
	s := New(":0", func(ctx context.Context) error {
		return nil
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleReadyz_ImageSyncGatesReadiness(t *testing.T) {
	// Simulates the site worker's readyCheck: connected=true but images not yet synced.
	var imagesSynced atomic.Bool
	s := New(":0", func(_ context.Context) error {
		if !imagesSynced.Load() {
			return fmt.Errorf("images not yet synced")
		}
		return nil
	})

	// Before images are synced: should be 503
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("before image sync: expected 503, got %d", rec.Code)
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["reason"] != "images not yet synced" {
		t.Fatalf("expected 'images not yet synced', got %q", resp["reason"])
	}

	// After images are synced: should be 200
	imagesSynced.Store(true)
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	s.handleReadyz(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after image sync: expected 200, got %d", rec.Code)
	}
}

// TestMetricsRoute pins the /metrics conditional: absent without a
// registry (404), serving Prometheus text format with one.
func TestMetricsRoute(t *testing.T) {
	bare := New(":0", nil)
	rec := httptest.NewRecorder()
	bare.buildMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no registry: expected 404, got %d", rec.Code)
	}

	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "slagworks_test_gauge", Help: "x"})
	reg.MustRegister(g)
	g.Set(42)

	s := New(":0", nil)
	s.SetMetricsRegistry(reg)
	rec = httptest.NewRecorder()
	s.buildMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("with registry: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "slagworks_test_gauge 42") {
		t.Fatalf("metrics body missing gauge:\n%s", rec.Body.String())
	}
}

func TestHandleReadyz_CheckFails(t *testing.T) {
	s := New(":0", func(ctx context.Context) error {
		return fmt.Errorf("global API unreachable")
	})

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	s.handleReadyz(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "not ready" {
		t.Fatalf("expected 'not ready', got %q", resp["status"])
	}
	if resp["reason"] != "global API unreachable" {
		t.Fatalf("expected reason, got %q", resp["reason"])
	}
}
