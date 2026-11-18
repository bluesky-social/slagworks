package dhcp

import (
	"encoding/binary"
	"net"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics bundles the Prometheus collectors the DHCP handler emits.
// Construct via NewMetrics(reg) — no package-level globals (tests
// build fresh registries; MustRegister panics on duplicates). The
// handler nil-checks cfg.Metrics on every touch, so tests that don't
// care about metrics can leave it unset.
//
// The "pool" label is the pool's subnet in CIDR form — the direct
// pool and each relay pool get their own series. Active-lease counts
// are exposed via RegisterLeaseCollector (scrape-time walk of the
// lease table, so lazily-expired leases don't inflate the gauge).
type Metrics struct {
	RequestsTotal *prometheus.CounterVec // by DHCP message type
	OffersTotal   *prometheus.CounterVec // by pool
	AcksTotal     *prometheus.CounterVec // by pool
	DropsTotal    *prometheus.CounterVec // by reason
	PoolSize      *prometheus.GaugeVec   // by pool
}

// Drop reasons (DropsTotal label values). Enumerated so the handler
// and dashboards agree on spelling.
const (
	DropUnknownMAC    = "unknown_mac"     // KnownMACs filter rejected
	DropNoPool        = "no_pool"         // relayed giaddr matched no pool / cold start
	DropExhausted     = "pool_exhausted"  // no free IP in the matched pool
	DropOtherServer   = "other_server_id" // REQUEST named the other worker (RFC 2131 silence)
	DropUnhandledType = "unhandled_type"  // RELEASE / unexpected message type
	DropNAK           = "nak"             // REQUEST refused via NAK (peer slice, unknown dynamic addr on an unsliced segment, lease/reservation mismatch, out-of-subnet)
)

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_dhcp_requests_total",
			Help: "DHCP packets received, by message type (DISCOVER | REQUEST | ...).",
		}, []string{"type"}),

		OffersTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_dhcp_offers_total",
			Help: "DHCP OFFERs sent, by pool subnet.",
		}, []string{"pool"}),

		AcksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_dhcp_acks_total",
			Help: "DHCP ACKs sent, by pool subnet.",
		}, []string{"pool"}),

		DropsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "slagworks_site_dhcp_drops_total",
			Help: "DHCP packets received but not answered, by reason (unknown_mac | no_pool | pool_exhausted | other_server_id | unhandled_type | nak).",
		}, []string{"reason"}),

		PoolSize: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "slagworks_site_dhcp_pool_size",
			Help: "Total allocatable IPs in the pool range, by pool subnet. leases_active/pool_size near 1 = exhaustion imminent.",
		}, []string{"pool"}),
	}
	reg.MustRegister(
		m.RequestsTotal, m.OffersTotal, m.AcksTotal,
		m.DropsTotal, m.PoolSize,
	)
	return m
}

// drop increments DropsTotal. Nil-safe.
func (m *Metrics) drop(reason string) {
	if m != nil {
		m.DropsTotal.WithLabelValues(reason).Inc()
	}
}

// countRequest increments RequestsTotal. Nil-safe.
func (m *Metrics) countRequest(msgType string) {
	if m != nil {
		m.RequestsTotal.WithLabelValues(msgType).Inc()
	}
}

// offer increments OffersTotal for the pool. Nil-safe.
func (m *Metrics) offer(poolLabel string) {
	if m != nil {
		m.OffersTotal.WithLabelValues(poolLabel).Inc()
	}
}

// ack increments AcksTotal for the pool. Nil-safe.
func (m *Metrics) ack(poolLabel string) {
	if m != nil {
		m.AcksTotal.WithLabelValues(poolLabel).Inc()
	}
}

// setPoolGauges resets and repopulates PoolSize for the given pool
// state. Called from NewHandler and UpdatePools; reset-then-set so
// pools removed by a snapshot edit don't leave stale series behind.
// Nil-safe.
func (m *Metrics) setPoolGauges(p PoolConfig) {
	if m == nil {
		return
	}
	m.PoolSize.Reset()
	if p.RangeStart == nil {
		return
	}
	m.PoolSize.WithLabelValues(p.Subnet.String()).Set(rangeSize(p.RangeStart, p.RangeEnd))
	for _, rs := range p.RelaySubnets {
		m.PoolSize.WithLabelValues(rs.Subnet.String()).Set(rangeSize(rs.RangeStart, rs.RangeEnd))
	}
}

// rangeSize counts the IPs in [start, end] inclusive. Both were
// validated v4 at PoolConfig build time; returns 0 on anything else.
func rangeSize(start, end net.IP) float64 {
	s4, e4 := start.To4(), end.To4()
	if s4 == nil || e4 == nil {
		return 0
	}
	s := binary.BigEndian.Uint32(s4)
	e := binary.BigEndian.Uint32(e4)
	if e < s {
		return 0
	}
	return float64(e - s + 1)
}

// leaseCollector exposes slagworks_site_dhcp_leases_active{pool} as a
// scrape-time walk of the handler's lease table. A stored GaugeVec
// would drift: lease expiry is lazy (cleaned up during allocation),
// so counting unexpired entries at scrape time is the only honest
// number.
//
// It dereferences a getter rather than holding a *Handler because the
// site worker's standby mode tears down and recreates the handler on
// every VIP transition, while collector registration happens once per
// process. A nil getter result (no handler active) emits no samples.
type leaseCollector struct {
	handler func() *Handler
	desc    *prometheus.Desc
}

// RegisterLeaseCollector wires the active-lease gauge into the
// registry. handler is consulted at every scrape; nil means "no
// DHCP handler currently active" and emits nothing.
func RegisterLeaseCollector(reg prometheus.Registerer, handler func() *Handler) {
	reg.MustRegister(&leaseCollector{
		handler: handler,
		desc: prometheus.NewDesc(
			"slagworks_site_dhcp_leases_active",
			"Unexpired leases currently held, by pool subnet ('none' for leases outside every configured pool).",
			[]string{"pool"}, nil,
		),
	})
}

func (c *leaseCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *leaseCollector) Collect(ch chan<- prometheus.Metric) {
	h := c.handler()
	if h == nil {
		return
	}
	for poolLabel, n := range h.LeaseCountsByPool() {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), poolLabel)
	}
}
