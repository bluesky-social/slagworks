package dhcp

import (
	"net"
	"testing"
	"time"
)

// TestLeases_UnexpiredOnly pins that Leases() returns live leases with
// their MAC/IP/expiry/pool, skips expired ones without deleting them
// (a read must not mutate), and reports "none" for a lease outside
// every configured pool.
func TestLeases_UnexpiredOnly(t *testing.T) {
	cfg := testConfig()
	h := NewHandler(cfg)

	live := net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x01}
	h.setLease(live.String(), live, net.ParseIP("10.99.1.100"), time.Now().Add(time.Hour))

	expired := net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x02}
	h.setLease(expired.String(), expired, net.ParseIP("10.99.1.101"), time.Now().Add(-time.Minute))

	outside := net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x03}
	h.setLease(outside.String(), outside, net.ParseIP("10.1.1.1"), time.Now().Add(time.Hour))

	leases := h.Leases()
	if len(leases) != 2 {
		t.Fatalf("Leases() returned %d entries, want 2 (expired must be excluded): %+v", len(leases), leases)
	}

	byMAC := make(map[string]LeaseInfo, len(leases))
	for _, l := range leases {
		byMAC[l.MAC] = l
	}

	got, ok := byMAC[live.String()]
	if !ok {
		t.Fatalf("live lease missing from snapshot: %+v", leases)
	}
	if got.IP != "10.99.1.100" {
		t.Errorf("live lease IP = %q, want 10.99.1.100", got.IP)
	}
	if got.Pool != cfg.Subnet.String() {
		t.Errorf("live lease pool = %q, want %q", got.Pool, cfg.Subnet.String())
	}

	if _, ok := byMAC[expired.String()]; ok {
		t.Errorf("expired lease must not appear in the snapshot")
	}

	got, ok = byMAC[outside.String()]
	if !ok {
		t.Fatalf("outside-pool lease missing from snapshot: %+v", leases)
	}
	if got.Pool != "none" {
		t.Errorf("outside-pool lease pool = %q, want \"none\"", got.Pool)
	}

	// The read must not mutate the table: the expired entry is still
	// there (lazy cleanup happens only during allocation).
	h.mu.Lock()
	_, stillPresent := h.leases[expired.String()]
	h.mu.Unlock()
	if !stillPresent {
		t.Error("Leases() deleted an expired entry; it must be a read-only snapshot")
	}
}

// TestLeases_Empty pins the empty-table case: no leases, no panic, a
// non-nil empty slice.
func TestLeases_Empty(t *testing.T) {
	h := NewHandler(testConfig())
	leases := h.Leases()
	if len(leases) != 0 {
		t.Errorf("Leases() on an empty handler = %+v, want empty", leases)
	}
}

// TestLeases_PoolLabel_ReservationsOnlyBasePool pins that a lease on a
// reservations-only base pool (valid Subnet, nil dynamic RangeStart —
// an OOB gateway that owns no dynamic slice but still serves its
// static reservations, see poolForRequest) labels correctly instead
// of falling through to "none" (roast finding, 2026-07-13: the label
// check incorrectly gated on RangeStart rather than Subnet.IP).
func TestLeases_PoolLabel_ReservationsOnlyBasePool(t *testing.T) {
	cfg := Config{
		ServerIP: net.ParseIP("10.99.1.1"),
		PoolConfig: PoolConfig{
			Subnet:  net.IPNet{IP: net.ParseIP("10.99.1.0"), Mask: net.CIDRMask(24, 32)},
			Gateway: net.ParseIP("10.99.1.1"),
			// RangeStart intentionally nil: reservations-only.
		},
		LeaseDuration: 10 * time.Minute,
	}
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x01}
	h.setLease(mac.String(), mac, net.ParseIP("10.99.1.50"), time.Now().Add(time.Hour))

	leases := h.Leases()
	if len(leases) != 1 {
		t.Fatalf("got %d leases, want 1: %+v", len(leases), leases)
	}
	if leases[0].Pool != cfg.Subnet.String() {
		t.Errorf("reservations-only pool: Pool = %q, want %q", leases[0].Pool, cfg.Subnet.String())
	}
}

// TestLeases_PoolLabel_PerInterfacePool pins that a lease landed via a
// per-interface pool (registered via SetIfaceParams — the OOB takeover
// path) labels under that interface's subnet, not "none" (roast
// finding, 2026-07-13: poolLabelForIP never consulted h.ifaceParams).
func TestLeases_PoolLabel_PerInterfacePool(t *testing.T) {
	h := NewHandler(testConfig()) // base pool 10.99.1.0/24
	oob := oobIfaceParams()       // 10.254.240.0/22, range 10.254.241.100-199
	h.SetIfaceParams("eno3", oob)

	mac := net.HardwareAddr{0xaa, 0, 0, 0, 0, 0x02}
	h.setLease(mac.String(), mac, net.ParseIP("10.254.241.150"), time.Now().Add(time.Hour))

	leases := h.Leases()
	if len(leases) != 1 {
		t.Fatalf("got %d leases, want 1: %+v", len(leases), leases)
	}
	if leases[0].Pool != oob.Subnet.String() {
		t.Errorf("per-interface pool: Pool = %q, want %q", leases[0].Pool, oob.Subnet.String())
	}
}
