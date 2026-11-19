package dhcp

import (
	"net"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// counterValue extracts a single labeled counter's value from the
// registry, 0 when the series doesn't exist yet.
func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("get metric: %v", err)
	}
	return testutil.ToFloat64(c)
}

// TestMetrics_DORAFlow pins the counter increments across a full
// DISCOVER → OFFER, REQUEST → ACK exchange: requests by type, offers
// and acks by pool subnet.
func TestMetrics_DORAFlow(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := testConfig()
	cfg.Metrics = NewMetrics(reg)
	h := NewHandler(cfg)

	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01}
	offer := h.HandlePacket(mustDiscover(t, mac))
	if offer == nil {
		t.Fatal("expected OFFER")
	}
	ack := h.HandlePacket(mustRequestFromOffer(t, offer))
	if ack == nil {
		t.Fatal("expected ACK")
	}

	pool := cfg.Subnet.String()
	if got := counterValue(t, cfg.Metrics.RequestsTotal, "DISCOVER"); got != 1 {
		t.Errorf("requests{DISCOVER} = %v, want 1", got)
	}
	if got := counterValue(t, cfg.Metrics.RequestsTotal, "REQUEST"); got != 1 {
		t.Errorf("requests{REQUEST} = %v, want 1", got)
	}
	if got := counterValue(t, cfg.Metrics.OffersTotal, pool); got != 1 {
		t.Errorf("offers{%s} = %v, want 1", pool, got)
	}
	if got := counterValue(t, cfg.Metrics.AcksTotal, pool); got != 1 {
		t.Errorf("acks{%s} = %v, want 1", pool, got)
	}
}

// TestMetrics_DropReasons pins the drop counter's reason labels for
// the four drop paths reachable without a relay: unknown MAC,
// other-server REQUEST, pool exhaustion, non-BOOTREQUEST opcode.
func TestMetrics_DropReasons(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := testConfig()
	// Single-IP pool so a second MAC exhausts it.
	cfg.RangeEnd = net.ParseIP("10.99.1.100")
	cfg.KnownMACs = func(mac string) bool {
		return !strings.HasPrefix(mac, "de:ad")
	}
	cfg.Metrics = NewMetrics(reg)
	h := NewHandler(cfg)

	// unknown_mac
	h.HandlePacket(mustDiscover(t, net.HardwareAddr{0xde, 0xad, 0x00, 0x00, 0x00, 0x01}))
	if got := counterValue(t, cfg.Metrics.DropsTotal, DropUnknownMAC); got != 1 {
		t.Errorf("drops{unknown_mac} = %v, want 1", got)
	}

	// pool_exhausted: first MAC takes the only IP, second gets nothing.
	if offer := h.HandlePacket(mustDiscover(t, net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x01})); offer == nil {
		t.Fatal("expected OFFER for first MAC")
	}
	h.HandlePacket(mustDiscover(t, net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x02}))
	if got := counterValue(t, cfg.Metrics.DropsTotal, DropExhausted); got != 1 {
		t.Errorf("drops{pool_exhausted} = %v, want 1", got)
	}

	// other_server_id: REQUEST naming a different server stays silent.
	req := mustDiscover(t, net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x01},
		dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithGeneric(dhcpv4.OptionServerIdentifier, net.ParseIP("10.99.1.2").To4()))
	if resp := h.HandlePacket(req); resp != nil {
		t.Fatal("expected silence for REQUEST naming another server")
	}
	if got := counterValue(t, cfg.Metrics.DropsTotal, DropOtherServer); got != 1 {
		t.Errorf("drops{other_server_id} = %v, want 1", got)
	}

	// not_bootrequest: a DISCOVER whose BOOTP op is not BOOTREQUEST is
	// counted as received and dropped under its own reason, even from an
	// unregistered MAC (the opcode is checked before the MAC filter).
	for _, mac := range []net.HardwareAddr{{0xaa, 0, 0, 0, 0, 0x03}, {0xde, 0xad, 0, 0, 0, 0x02}} {
		bogus := mustDiscover(t, mac)
		bogus.OpCode = dhcpv4.OpcodeBootReply
		if resp := h.HandlePacket(bogus); resp != nil {
			t.Fatalf("expected silence for op=BOOTREPLY DISCOVER from %s", mac)
		}
	}
	if got := counterValue(t, cfg.Metrics.DropsTotal, DropNotBootRequest); got != 2 {
		t.Errorf("drops{not_bootrequest} = %v, want 2", got)
	}
	if got := counterValue(t, cfg.Metrics.DropsTotal, DropUnknownMAC); got != 1 {
		t.Errorf("drops{unknown_mac} = %v after op=BOOTREPLY packets, want 1 (unchanged)", got)
	}
	if got := counterValue(t, cfg.Metrics.RequestsTotal, "DISCOVER"); got != 5 {
		t.Errorf("requests{DISCOVER} = %v, want 5", got)
	}
}

// TestMetrics_PoolSizeGauge pins pool_size for direct + relay pools,
// including the reset-on-update behavior (a removed relay pool's
// series must disappear, not linger at its old value).
func TestMetrics_PoolSizeGauge(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := testConfig() // .100-.200 = 101 IPs
	relay := RelaySubnet{
		Subnet:     net.IPNet{IP: net.ParseIP("10.99.2.0"), Mask: net.CIDRMask(24, 32)},
		RangeStart: net.ParseIP("10.99.2.10"),
		RangeEnd:   net.ParseIP("10.99.2.19"), // 10 IPs
		Gateway:    net.ParseIP("10.99.2.1"),
	}
	cfg.RelaySubnets = []RelaySubnet{relay}
	cfg.Metrics = NewMetrics(reg)
	h := NewHandler(cfg)

	direct, err := cfg.Metrics.PoolSize.GetMetricWithLabelValues(cfg.Subnet.String())
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(direct); got != 101 {
		t.Errorf("pool_size{direct} = %v, want 101", got)
	}
	relayG, err := cfg.Metrics.PoolSize.GetMetricWithLabelValues(relay.Subnet.String())
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(relayG); got != 10 {
		t.Errorf("pool_size{relay} = %v, want 10", got)
	}

	// Drop the relay pool; its series must vanish on UpdatePools.
	next := cfg.PoolConfig
	next.RelaySubnets = nil
	h.UpdatePools(next)
	if got := testutil.CollectAndCount(cfg.Metrics.PoolSize); got != 1 {
		t.Errorf("pool_size series after relay removal = %d, want 1", got)
	}
}

// TestMetrics_LeaseCollector pins the scrape-time active-lease walk:
// leases land under their containing pool's label, and a nil handler
// (standby teardown) emits nothing.
func TestMetrics_LeaseCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	cfg := testConfig()
	cfg.Metrics = NewMetrics(reg)
	h := NewHandler(cfg)
	RegisterLeaseCollector(reg, func() *Handler { return h })

	if h.HandlePacket(mustDiscover(t, net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x01})) == nil {
		t.Fatal("expected OFFER")
	}
	if h.HandlePacket(mustDiscover(t, net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x02})) == nil {
		t.Fatal("expected OFFER")
	}

	counts := h.LeaseCountsByPool()
	if counts[cfg.Subnet.String()] != 2 {
		t.Errorf("LeaseCountsByPool = %v, want 2 in %s", counts, cfg.Subnet.String())
	}

	// A configured-but-empty pool must still appear at 0 — a label
	// that vanishes between scrapes goes Prometheus-stale instead of
	// reading 0 (breaks sum() and emptied-pool dashboards).
	empty := NewHandler(testConfig())
	emptyCounts := empty.LeaseCountsByPool()
	if n, ok := emptyCounts[cfg.Subnet.String()]; !ok || n != 0 {
		t.Errorf("empty pool: counts = %v, want explicit 0 for configured subnet", emptyCounts)
	}

	// Collector path end-to-end through the registry.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range families {
		if mf.GetName() == "slagworks_site_dhcp_leases_active" {
			found = true
			if n := mf.GetMetric()[0].GetGauge().GetValue(); n != 2 {
				t.Errorf("leases_active = %v, want 2", n)
			}
		}
	}
	if !found {
		t.Error("leases_active not present in gather output")
	}

	// Nil handler (standby): collector emits no samples and Gather
	// must not error.
	reg2 := prometheus.NewRegistry()
	RegisterLeaseCollector(reg2, func() *Handler { return nil })
	if _, err := reg2.Gather(); err != nil {
		t.Fatalf("gather with nil handler: %v", err)
	}
}

// TestMetrics_NilSafe pins that an un-instrumented handler (Metrics
// nil, the shape every existing test and the qemu harness uses)
// handles a full DORA without panicking.
func TestMetrics_NilSafe(t *testing.T) {
	h := NewHandler(testConfig())
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x07}
	offer := h.HandlePacket(mustDiscover(t, mac))
	if offer == nil {
		t.Fatal("expected OFFER")
	}
	if h.HandlePacket(mustRequestFromOffer(t, offer)) == nil {
		t.Fatal("expected ACK")
	}
}
