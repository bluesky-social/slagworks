package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
)

func TestMetricsEndpointServes(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "test_counter", Help: "test"})
	reg.MustRegister(counter)
	counter.Inc()

	e := echo.New()
	RegisterMetrics(e, reg)
	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "test_counter") {
		t.Fatalf("expected test_counter in body; got %q", w.Body.String())
	}
}
