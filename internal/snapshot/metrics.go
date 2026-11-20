package snapshot

import "github.com/prometheus/client_golang/prometheus"

// Metrics bundles every Prometheus metric the snapshot subsystem emits.
// Construct one via NewMetrics(reg). Do NOT declare these as package-
// level globals — tests create fresh registries, and package-level
// MustRegister panics on duplicate registration.
type Metrics struct {
	RebuildsTotal     *prometheus.CounterVec
	RebuildDuration   *prometheus.HistogramVec
	SnapshotSeq       *prometheus.GaugeVec
	DirtyGen          *prometheus.GaugeVec
	NetBoxAPIDuration *prometheus.HistogramVec
	NetBoxAPIErrors   *prometheus.CounterVec
	SiteStale         *prometheus.GaugeVec
	CastStorageBytes  *prometheus.GaugeVec
}

// NewMetrics constructs and registers the metric collectors on the
// provided registry. Panics on duplicate registration — that's the
// expected behavior if the caller accidentally wires two instances.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RebuildsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_snapshot_rebuilds_total",
			Help: "Snapshot rebuilds by site and outcome (success | skipped_no_change | error_netbox: a NetBox call failed or its data did not build | error_postgres).",
		}, []string{"site", "outcome"}),

		RebuildDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "slagworks_snapshot_rebuild_duration_seconds",
			Help:    "Wall-clock per snapshot rebuild.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}, []string{"site"}),

		SnapshotSeq: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slagworks_snapshot_seq",
			Help: "Current site_configs.last_snapshot_seq per site.",
		}, []string{"site"}),

		DirtyGen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slagworks_snapshot_dirty_gen",
			Help: "Current registry dirtyGen per site. Divergence from snapshot_seq indicates a stuck rebuild.",
		}, []string{"site"}),

		NetBoxAPIDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "slagworks_netbox_api_duration_seconds",
			Help:    "NetBox HTTP call duration by endpoint.",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
		}, []string{"endpoint"}),

		NetBoxAPIErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_netbox_api_errors_total",
			Help: "NetBox HTTP errors by endpoint and status code.",
		}, []string{"endpoint", "status"}),

		SiteStale: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slagworks_site_stale",
			Help: "1 if the site slug is missing/renamed in NetBox, else 0. Wired to alerting.",
		}, []string{"site"}),

		CastStorageBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slagworks_cast_storage_bytes",
			Help: "Stored SOL console-cast bytes per machine. Casts are kept forever (audit record); a machine climbing here has a chatty serial console.",
		}, []string{"machine_id"}),
	}
	reg.MustRegister(
		m.RebuildsTotal, m.RebuildDuration,
		m.SnapshotSeq, m.DirtyGen,
		m.NetBoxAPIDuration, m.NetBoxAPIErrors,
		m.SiteStale, m.CastStorageBytes,
	)
	return m
}
