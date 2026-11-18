package dhcp

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// oobView returns an IfaceParams modeling the OOB segment: its own serverIP,
// DNS, a direct /22 pool, promiscuous addressing.
func oobIfaceParams() IfaceParams {
	return IfaceParams{
		ServerIP: net.ParseIP("10.254.240.11"),
		DNS:      []net.IP{net.ParseIP("10.254.240.11"), net.ParseIP("10.254.240.12")},
		NTP:      []net.IP{net.ParseIP("10.254.240.1")},
		PoolConfig: PoolConfig{
			Subnet:      net.IPNet{IP: net.ParseIP("10.254.240.0"), Mask: net.CIDRMask(22, 32)},
			RangeStart:  net.ParseIP("10.254.241.100"),
			RangeEnd:    net.ParseIP("10.254.241.199"),
			Gateway:     net.ParseIP("10.254.240.1"),
			Promiscuous: true,
		},
	}
}

// TestHandlePacketOn_PerInterfaceServerIP: a direct DISCOVER on the OOB
// interface must use the OOB serverIP/gateway/DNS, NOT the base config's.
// This is the I-SEG-1 invariant: arrival interface selects the segment.
func TestHandlePacketOn_PerInterfaceServerIP(t *testing.T) {
	cfg := testConfig() // base ServerIP 10.99.1.1
	h := NewHandler(cfg)
	h.SetIfaceParams("eno3", oobIfaceParams())

	mac := net.HardwareAddr{0x28, 0x29, 0x86, 0xad, 0x67, 0xe5}
	offer := h.HandlePacketOn(mustDiscover(t, mac, withPXEClient()), "eno3")
	if offer == nil {
		t.Fatal("expected an offer on the OOB interface")
	}
	// Offered IP must be from the OOB pool, not the base 10.99.1.x pool.
	if got := offer.YourIPAddr.String(); got[:11] != "10.254.241." {
		t.Fatalf("offered IP %s not from OOB pool 10.254.241.x", got)
	}
	// server-id + next-server (siaddr) must be the OOB serverIP.
	if sid := offer.ServerIdentifier(); !sid.Equal(net.ParseIP("10.254.240.11")) {
		t.Fatalf("server-id %v, want OOB 10.254.240.11", sid)
	}
	if !offer.ServerIPAddr.Equal(net.ParseIP("10.254.240.11")) {
		t.Fatalf("next-server %v, want OOB 10.254.240.11", offer.ServerIPAddr)
	}
	// DNS must be the OOB resolvers, and NTP (option 42) present.
	dns := offer.DNS()
	if len(dns) != 2 || !dns[0].Equal(net.ParseIP("10.254.240.11")) {
		t.Fatalf("OOB DNS wrong: %v", dns)
	}
	if ntp := offer.Options.Get(dhcpv4.OptionNTPServers); len(ntp) != 4 {
		t.Fatalf("expected 1 NTP server (4 bytes), got %v", ntp)
	}
}

// TestHandlePacketOn_BaseInterfaceUnaffected: a packet with no iface (the
// single-interface case) still uses the base config even when an OOB
// override is registered. Proves the overlay doesn't leak into the base.
func TestHandlePacketOn_BaseInterfaceUnaffected(t *testing.T) {
	h := NewHandler(testConfig())
	h.SetIfaceParams("eno3", oobIfaceParams())

	mac := net.HardwareAddr{0xde, 0xad, 0x00, 0x00, 0x00, 0x01}
	offer := h.HandlePacket(mustDiscover(t, mac)) // iface ""
	if offer == nil {
		t.Fatal("expected an offer on the base interface")
	}
	if got := offer.YourIPAddr.String(); got[:9] != "10.99.1.1" && got[:8] != "10.99.1." {
		t.Fatalf("base offer IP %s not from base pool 10.99.1.x", got)
	}
	if sid := offer.ServerIdentifier(); !sid.Equal(net.ParseIP("10.99.1.1")) {
		t.Fatalf("base server-id %v, want 10.99.1.1", sid)
	}
}

// TestBootEligible_GatesBootfile: with a BootEligible gate, an ineligible
// MAC gets an address but NO bootfile; an eligible MAC gets the bootfile.
// This is the promiscuous-OOB blast-radius control (no boot URL to idle
// BMCs) and proves imaging still works for eligible hosts.
func TestBootEligible_GatesBootfile(t *testing.T) {
	cfg := testConfig()
	eligible := map[string]bool{"aa:bb:cc:00:00:02": true}
	cfg.BootEligible = func(mac string) bool { return eligible[mac] }
	h := NewHandler(cfg)

	idle := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01}
	offer := h.HandlePacket(mustDiscover(t, idle, withPXEClient()))
	if offer == nil {
		t.Fatal("ineligible MAC should still get an address")
	}
	if offer.BootFileName != "" {
		t.Fatalf("ineligible MAC must get NO bootfile, got %q", offer.BootFileName)
	}

	booting := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02}
	offer2 := h.HandlePacket(mustDiscover(t, booting, withPXEClient()))
	if offer2 == nil || offer2.BootFileName == "" {
		t.Fatalf("eligible MAC must get a bootfile, got %+v", offer2)
	}
}

// TestBootEligible_NilAlwaysServes: a nil BootEligible preserves the current
// always-serve-bootfile behavior (single-purpose provisioning default).
func TestBootEligible_NilAlwaysServes(t *testing.T) {
	h := NewHandler(testConfig()) // BootEligible nil
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x03}
	offer := h.HandlePacket(mustDiscover(t, mac, withPXEClient()))
	if offer == nil || offer.BootFileName == "" {
		t.Fatalf("nil BootEligible must always serve a bootfile, got %+v", offer)
	}
}

// TestAnyPromiscuous: reports true if the base OR any registered interface
// is promiscuous. The base testConfig is non-promiscuous.
func TestAnyPromiscuous(t *testing.T) {
	h := NewHandler(testConfig())
	if h.AnyPromiscuous() {
		t.Fatal("base config is non-promiscuous; AnyPromiscuous should be false")
	}
	h.SetIfaceParams("eno3", oobIfaceParams()) // OOB is promiscuous
	if !h.AnyPromiscuous() {
		t.Fatal("OOB interface is promiscuous; AnyPromiscuous should be true")
	}
}

// TestHandlePacketOn_OOBReservationServed: a reserved MAC on the OOB
// interface gets its reserved IP (outside the dynamic pool) with the OOB
// serverIP. Models a BMC with a static reservation.
func TestHandlePacketOn_OOBReservationServed(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "28:29:86:ad:67:e5" {
			return net.ParseIP("10.254.240.152")
		}
		return nil
	}
	h := NewHandler(cfg)
	h.SetIfaceParams("eno3", oobIfaceParams())

	mac := net.HardwareAddr{0x28, 0x29, 0x86, 0xad, 0x67, 0xe5}
	offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3")
	if offer == nil {
		t.Fatal("expected an offer for the reserved BMC")
	}
	if !offer.YourIPAddr.Equal(net.ParseIP("10.254.240.152")) {
		t.Fatalf("reserved IP %v, want 10.254.240.152", offer.YourIPAddr)
	}
	if !offer.ServerIdentifier().Equal(net.ParseIP("10.254.240.11")) {
		t.Fatalf("server-id %v, want OOB 10.254.240.11", offer.ServerIdentifier())
	}
}

// TestViewForIface_PartialOverrideFallsBack: an IfaceParams that sets only
// some fields (here: a pool + promiscuous, but nil DNS/NTP/ServerIP) must
// inherit the base config's DNS/serverIP rather than emit nil options.
func TestViewForIface_PartialOverrideFallsBack(t *testing.T) {
	cfg := testConfig() // base ServerIP 10.99.1.1, DNS 1.1.1.1
	h := NewHandler(cfg)
	// Partial: only the pool is set (its own subnet), everything else nil.
	h.SetIfaceParams("eno3", IfaceParams{
		PoolConfig: PoolConfig{
			Subnet:      net.IPNet{IP: net.ParseIP("10.254.240.0"), Mask: net.CIDRMask(22, 32)},
			RangeStart:  net.ParseIP("10.254.241.100"),
			RangeEnd:    net.ParseIP("10.254.241.199"),
			Gateway:     net.ParseIP("10.254.240.1"),
			Promiscuous: true,
		},
	})
	mac := net.HardwareAddr{0x28, 0x29, 0x86, 0xad, 0x67, 0xe5}
	offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3")
	if offer == nil {
		t.Fatal("expected an offer")
	}
	// Pool came from the override...
	if offer.YourIPAddr.String()[:11] != "10.254.241." {
		t.Fatalf("expected OOB pool IP, got %v", offer.YourIPAddr)
	}
	// ...but serverIP + DNS fall back to base (override left them nil).
	if !offer.ServerIdentifier().Equal(net.ParseIP("10.99.1.1")) {
		t.Fatalf("nil override ServerIP should fall back to base 10.99.1.1, got %v", offer.ServerIdentifier())
	}
	dns := offer.DNS()
	if len(dns) != 1 || !dns[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Fatalf("nil override DNS should fall back to base 1.1.1.1, got %v", dns)
	}
}

// TestSetIfaceParams_Runtime: SetIfaceParams can be called after handler
// construction (snapshot hot-reload), and takes effect on the next packet.
func TestSetIfaceParams_Runtime(t *testing.T) {
	h := NewHandler(testConfig())
	mac := net.HardwareAddr{0x28, 0x29, 0x86, 0xad, 0x67, 0xe5}

	// Before registering eno3, a packet on eno3 falls back to the base
	// config (non-promiscuous, base pool). With KnownMACs unset it still
	// serves from the base pool.
	if offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3"); offer == nil {
		t.Fatal("base fallback should serve")
	} else if offer.YourIPAddr.String()[:8] != "10.99.1." {
		t.Fatalf("pre-register should use base pool, got %v", offer.YourIPAddr)
	}

	h.SetIfaceParams("eno3", oobIfaceParams())
	offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3")
	if offer == nil || offer.YourIPAddr.String()[:11] != "10.254.241." {
		t.Fatalf("post-register should use OOB pool, got %+v", offer)
	}
}
