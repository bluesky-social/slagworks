// Package opsmetrics holds the operation-shaped Prometheus metrics
// (fable-review Tier 5): the review found observability was
// subsystem-shaped — rebuild counters, DHCP drops — while a 3am stuck
// deploy couldn't be localized to a phase from Grafana. These metrics
// make the OPERATION the unit: where do deploys spend time, how do
// ops end, which sweeps fire, and which .deb every worker runs.
//
// One struct, one registration, shared by the three surfaces that see
// op lifecycle events: hubhandlers.OpUpdate (phase transitions +
// worker-reported terminal status), the ws hub (hellos → reconnects +
// version drift), and the API server's expiry sweeps.
package opsmetrics

import "github.com/prometheus/client_golang/prometheus"

// Metrics bundles the op-lifecycle collectors. Construct via New —
// no package-level globals (tests use fresh registries).
type Metrics struct {
	// PhaseDuration observes time spent in each phase, labeled by the
	// phase EXITED. Fed by SetOperationPhase's transition result, so
	// it measures real wall-clock between phase entries — the "where
	// did this deploy's 6 minutes go" histogram.
	PhaseDuration *prometheus.HistogramVec

	// Outcomes counts terminal op transitions by type and outcome
	// (completed | failed). Worker-reported terminals and sweep kills
	// both land here.
	Outcomes *prometheus.CounterVec

	// SweepsFired counts ops killed per expiry sweep (pending, stuck,
	// orphaned_claim, phone_home). A sweep firing at all is a signal;
	// a sweep firing repeatedly is an incident shape.
	SweepsFired *prometheus.CounterVec

	// WorkerHellos counts WS hello handshakes per site. Steady-state
	// this moves only on deploys/restarts; a sawtooth is connection
	// churn that log-grepping used to be the only way to see.
	WorkerHellos *prometheus.CounterVec

	// WorkerVersion is an info-style gauge (=1) carrying each
	// connected worker's build SHA as a label. The HA
	// both-workers-must-upgrade footgun (bitten twice) is visible as
	// two series with different version labels on one site. The
	// worker and agent ship in the same .deb, so this IS the agent
	// version the site serves to deploys.
	WorkerVersion *prometheus.GaugeVec
}

func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		PhaseDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "slagworks_op_phase_duration_seconds",
			Help: "Wall-clock spent in each operation phase (labeled by the phase exited).",
			// 1s .. ~17min: phases range from sub-second (dhcp_ack)
			// to minutes (rsyncing_os, awaiting_phone_home).
			Buckets: prometheus.ExponentialBuckets(1, 2, 11),
		}, []string{"op_type", "phase"}),

		Outcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_operation_outcomes_total",
			Help: "Terminal operation transitions by type and outcome (completed | failed).",
		}, []string{"op_type", "outcome"}),

		SweepsFired: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_op_sweeps_fired_total",
			Help: "Operations killed by expiry sweeps (pending | stuck | orphaned_claim | phone_home).",
		}, []string{"sweep"}),

		WorkerHellos: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_ws_worker_hellos_total",
			Help: "WS hello handshakes per site; movement outside deploys is connection churn.",
		}, []string{"site"}),

		WorkerVersion: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slagworks_site_worker_version_info",
			Help: "1 per connected worker, carrying its build SHA; two versions on one site = HA upgrade drift.",
		}, []string{"site", "worker_id", "version"}),
	}
	reg.MustRegister(m.PhaseDuration, m.Outcomes, m.SweepsFired, m.WorkerHellos, m.WorkerVersion)
	return m
}
