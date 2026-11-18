package api

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// Readiness is the subset of global-worker state /healthz exposes.
// *snapshot.Reconciler satisfies this interface in production; tests
// supply a fake. The readiness model matches docs/dev/api.md:
//
//   - /healthz returns 200 when FirstReconcileComplete() is true, else 503.
//   - Stale sites are surfaced in the body but NEVER flip 200 -> 503;
//     they're informational so operators notice, not gate criteria.
type Readiness interface {
	FirstReconcileComplete() bool
	StaleSites() map[string]string
}

// RegisterHealthz wires /healthz and /readyz on the given Echo router.
// Both paths share the same handler — /readyz exists for operators
// who expect the Kubernetes-style split. The AWS ALB health check
// should hit /healthz.
//
// Must be called BEFORE any registerRoutes() path that might otherwise
// register a conflicting handler.
func RegisterHealthz(e *echo.Echo, r Readiness, version, instanceID string) {
	handler := func(c *echo.Context) error {
		ready := r.FirstReconcileComplete()
		stale := r.StaleSites()
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		return c.JSON(status, map[string]any{
			"ready":       ready,
			"stale_sites": stale,
			"version":     version,
			"instance_id": instanceID,
		})
	}
	e.GET("/healthz", handler)
	e.GET("/readyz", handler)
}
