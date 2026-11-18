package dhcp

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/iana"
)

// memLeaseStore is a test LeaseStore backed by a map, simulating
// a SQLite database that survives handler restarts.
type memLeaseStore struct {
	mu     sync.Mutex
	leases map[string]PersistedLease
}

func newMemLeaseStore() *memLeaseStore {
	return &memLeaseStore{leases: make(map[string]PersistedLease)}
}

func (s *memLeaseStore) SaveLease(mac net.HardwareAddr, ip net.IP, expiry time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leases[mac.String()] = PersistedLease{MAC: mac, IP: ip, ExpiresAt: expiry}
}

func (s *memLeaseStore) DeleteLease(mac net.HardwareAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.leases, mac.String())
}

func (s *memLeaseStore) LoadLeases() []PersistedLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PersistedLease, 0, len(s.leases))
	for _, l := range s.leases {
		out = append(out, l)
	}
	return out
}

// TestARPProbeSkipsOccupiedIP verifies that when an ARP probe detects an IP
// is already in use, the DHCP handler skips it and assigns the next available.
func TestARPProbeSkipsOccupiedIP(t *testing.T) {
	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.102")

	// Simulate .100 being occupied by another device
	cfg.ARPProbe = func(ip net.IP) bool {
		return ip.Equal(net.ParseIP("10.99.1.100"))
	}

	h := NewHandler(cfg)
	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")

	offer := h.HandlePacket(mustDiscover(t, mac1, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected offer")
	}
	ack := h.HandlePacket(mustRequestFromOffer(t, offer))
	if ack == nil {
		t.Fatal("expected ack")
	}

	ip := ack.YourIPAddr.String()
	if ip == "10.99.1.100" {
		t.Errorf("DHCP assigned %s which is occupied per ARP probe", ip)
	}
	if ip != "10.99.1.101" {
		t.Errorf("expected 10.99.1.101 (first free after occupied .100), got %s", ip)
	}
	t.Logf("correctly skipped .100 (occupied), assigned %s", ip)
}

// TestARPProbePoolExhaustionWhenAllOccupied verifies that if every IP in the
// range responds to ARP, the pool is reported as exhausted.
func TestARPProbePoolExhaustionWhenAllOccupied(t *testing.T) {
	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.101") // only 2 IPs

	// Every IP is occupied
	cfg.ARPProbe = func(ip net.IP) bool { return true }

	h := NewHandler(cfg)
	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")

	offer := h.HandlePacket(mustDiscover(t, mac1, withArch(iana.EFI_X86_64)))
	if offer != nil {
		t.Errorf("expected nil offer (pool exhausted by ARP), got offer for %s", offer.YourIPAddr)
	}
}

// TestLeasesSurviveRestart verifies that after a DHCP handler is destroyed
// and recreated (simulating a process restart), previously assigned IPs are
// not handed out to different MACs.
func TestLeasesSurviveRestart(t *testing.T) {
	store := newMemLeaseStore()

	cfg := testConfig()
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.102") // only 3 IPs
	cfg.LeaseStore = store

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")

	// --- Phase 1: First handler gives out leases ---
	h1 := NewHandler(cfg)

	offer1 := h1.HandlePacket(mustDiscover(t, mac1, withArch(iana.EFI_X86_64)))
	if offer1 == nil {
		t.Fatal("expected offer for mac1")
	}
	ack1 := h1.HandlePacket(mustRequestFromOffer(t, offer1))
	if ack1 == nil {
		t.Fatal("expected ack for mac1")
	}
	ip1 := ack1.YourIPAddr.String()
	t.Logf("mac1 got %s", ip1)

	offer2 := h1.HandlePacket(mustDiscover(t, mac2, withArch(iana.EFI_X86_64)))
	if offer2 == nil {
		t.Fatal("expected offer for mac2")
	}
	ack2 := h1.HandlePacket(mustRequestFromOffer(t, offer2))
	if ack2 == nil {
		t.Fatal("expected ack for mac2")
	}
	ip2 := ack2.YourIPAddr.String()
	t.Logf("mac2 got %s", ip2)

	if ip1 == ip2 {
		t.Fatalf("mac1 and mac2 got the same IP: %s", ip1)
	}

	// --- Phase 2: Simulate restart — new handler, same store ---
	h2 := NewHandler(cfg) // loads leases from store

	mac3, _ := net.ParseMAC("52:54:00:aa:bb:03")
	offer3 := h2.HandlePacket(mustDiscover(t, mac3, withArch(iana.EFI_X86_64)))
	if offer3 == nil {
		t.Fatal("expected offer for mac3")
	}
	ack3 := h2.HandlePacket(mustRequestFromOffer(t, offer3))
	if ack3 == nil {
		t.Fatal("expected ack for mac3")
	}
	ip3 := ack3.YourIPAddr.String()
	t.Logf("mac3 got %s", ip3)

	if ip3 == ip1 {
		t.Errorf("COLLISION: mac3 got %s which is still leased to mac1", ip3)
	}
	if ip3 == ip2 {
		t.Errorf("COLLISION: mac3 got %s which is still leased to mac2", ip3)
	}
}

// TestSameMACSameIPAfterRestart verifies that if a MAC re-requests after
// a restart, it gets the same IP it had before (lease persistence).
func TestSameMACSameIPAfterRestart(t *testing.T) {
	store := newMemLeaseStore()

	cfg := testConfig()
	cfg.LeaseStore = store

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")

	// --- Phase 1: Get a lease ---
	h1 := NewHandler(cfg)

	offer := h1.HandlePacket(mustDiscover(t, mac1, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected offer")
	}
	ack := h1.HandlePacket(mustRequestFromOffer(t, offer))
	if ack == nil {
		t.Fatal("expected ack")
	}
	originalIP := ack.YourIPAddr.String()

	// --- Phase 2: Restart, same MAC requests again ---
	h2 := NewHandler(cfg)

	offer2 := h2.HandlePacket(mustDiscover(t, mac1, withArch(iana.EFI_X86_64)))
	if offer2 == nil {
		t.Fatal("expected offer after restart")
	}
	ack2 := h2.HandlePacket(mustRequestFromOffer(t, offer2))
	if ack2 == nil {
		t.Fatal("expected ack after restart")
	}
	newIP := ack2.YourIPAddr.String()

	if newIP != originalIP {
		t.Errorf("MAC got different IP after restart: %s -> %s (expected same)", originalIP, newIP)
	}
}

// TestExpiredLeasesReclaimedAfterRestart verifies that expired leases from
// before a restart are properly reclaimed and available for new MACs.
func TestExpiredLeasesReclaimedAfterRestart(t *testing.T) {
	store := newMemLeaseStore()

	cfg := testConfig()
	cfg.LeaseDuration = 1 * time.Millisecond // expire immediately
	cfg.RangeStart = net.ParseIP("10.99.1.100")
	cfg.RangeEnd = net.ParseIP("10.99.1.100") // only 1 IP
	cfg.LeaseStore = store

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")

	// --- Phase 1: mac1 gets the only IP ---
	h1 := NewHandler(cfg)

	offer := h1.HandlePacket(mustDiscover(t, mac1, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected offer for mac1")
	}
	h1.HandlePacket(mustRequestFromOffer(t, offer))

	// Wait for lease to expire
	time.Sleep(10 * time.Millisecond)

	// --- Phase 2: Restart, mac2 should get the expired IP ---
	h2 := NewHandler(cfg)

	offer2 := h2.HandlePacket(mustDiscover(t, mac2, withArch(iana.EFI_X86_64)))
	if offer2 == nil {
		t.Fatal("expected offer for mac2 after restart — expired lease should be reclaimable")
	}
}
