package worker

import "github.com/prometheus/client_golang/prometheus"

// SiteMetrics bundles the site worker's own Prometheus collectors,
// served from the :9090 health server's /metrics. DHCP metrics live in
// internal/dhcp (dhcp.NewMetrics); both register into the same
// registry. Construct via NewSiteMetrics(reg) — no package-level
// globals, so tests can build fresh registries.
//
// Point-in-time gauges (ws_connected, outbox depth, ops_active,
// snapshot seq, logring drops) are NOT here: they're scrape-time
// GaugeFunc/CounterFunc closures over live state, registered by the
// site command (registerSiteGauges in cmd/slagworksd/site.go).
type SiteMetrics struct {
	TFTPFetchesTotal     *prometheus.CounterVec // by outcome
	HTTPBootFetchesTotal *prometheus.CounterVec // by route group + status class
	ClaimsTotal          *prometheus.CounterVec // by outcome
	SnapshotAppliesTotal prometheus.Counter
}

// Claim outcomes (ClaimsTotal label values).
const (
	ClaimWon  = "won"
	ClaimLost = "lost" // already claimed (normally: the other worker won the race)
	ClaimErr  = "error"
)

// TFTP fetch outcomes (TFTPFetchesTotal label values).
const (
	TFTPOk       = "ok"
	TFTPDenied   = "denied_unknown_ip"
	TFTPNotFound = "not_found"
	TFTPError    = "error"
)

func NewSiteMetrics(reg prometheus.Registerer) *SiteMetrics {
	m := &SiteMetrics{
		TFTPFetchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_tftp_fetches_total",
			Help: "TFTP read requests, by outcome (ok | denied_unknown_ip | not_found | error).",
		}, []string{"outcome"}),

		HTTPBootFetchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_httpboot_requests_total",
			Help: "HTTP boot-server requests, by route group (boot | files | logs | other) and status class (2xx | 3xx | 4xx | 5xx).",
		}, []string{"route", "status"}),

		ClaimsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_op_claims_total",
			Help: "Op claim attempts, by outcome (won | lost | error). 'lost' = another worker claimed first; expected under dual-worker, alarming if one worker always loses.",
		}, []string{"outcome"}),

		SnapshotAppliesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "slagworks_site_snapshot_applies_total",
			Help: "Snapshots applied since process start.",
		}),
	}
	reg.MustRegister(
		m.TFTPFetchesTotal, m.HTTPBootFetchesTotal,
		m.ClaimsTotal, m.SnapshotAppliesTotal,
	)
	return m
}

// claim increments ClaimsTotal if metrics are wired. Nil-safe so the
// executor works in tests that don't build a registry.
func (m *SiteMetrics) claim(outcome string) {
	if m != nil {
		m.ClaimsTotal.WithLabelValues(outcome).Inc()
	}
}
