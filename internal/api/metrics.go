package api

import (
	"github.com/labstack/echo/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bluesky-social/slagworks/internal/opsmetrics"
)

// RegisterMetrics wires GET /metrics on the given Echo router, serving
// from the supplied Prometheus registry. The handler is unauthenticated
// — deployment binds the API to the slag-admin ALB only, which is not
// internet-facing.
func RegisterMetrics(e *echo.Echo, reg *prometheus.Registry) {
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
	e.GET("/metrics", echo.WrapHandler(h))
}

// SetOpsMetrics wires the op-lifecycle metrics bundle so the expiry
// sweeps record what they kill. Optional; nil leaves sweeps
// log-only.
func (s *Server) SetOpsMetrics(m *opsmetrics.Metrics) { s.opsMetrics = m }

// observeSweep records n ops killed by the named sweep.
func (s *Server) observeSweep(sweep string, n int64) {
	if s.opsMetrics != nil {
		s.opsMetrics.SweepsFired.WithLabelValues(sweep).Add(float64(n))
	}
}
