package dhcp

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// splitPoolView builds an OOB IfaceParams modeling THIS gateway as the owner
// of the .100-.149 slice, with a peer owning .150-.199, over the 10.254.240.0/22
// OOB subnet. Registered on iface "eno3".
func splitPoolParams() IfaceParams {
	_, subnet, _ := net.ParseCIDR("10.254.240.0/22")
	return IfaceParams{
		ServerIP: net.ParseIP("10.254.240.11"),
		DynamicSlices: []DynamicSlice{
			{Start: net.ParseIP("10.254.241.100"), End: net.ParseIP("10.254.241.149")},
			{Start: net.ParseIP("10.254.241.150"), End: net.ParseIP("10.254.241.199")},
		},
		PoolConfig: PoolConfig{
			Subnet:      *subnet,
			RangeStart:  net.ParseIP("10.254.241.100"), // MY slice
			RangeEnd:    net.ParseIP("10.254.241.149"),
			Gateway:     net.ParseIP("10.254.240.1"),
			Promiscuous: true,
		},
	}
}

func splitPoolHandler() *Handler {
	cfg := testConfig()
	h := NewHandler(cfg)
	h.SetIfaceParams("eno3", splitPoolParams())
	return h
}

// initReboot builds an INIT-REBOOT REQUEST: requested-IP in option 50, no
// server-id, ciaddr=0 (broadcast).
func initReboot(t *testing.T, mac net.HardwareAddr, requestedIP string) *dhcpv4.DHCPv4 {
	t.Helper()
	req, err := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest))
	if err != nil {
		t.Fatal(err)
	}
	req.ClientHWAddr = mac
	req.UpdateOption(dhcpv4.OptRequestedIPAddress(net.ParseIP(requestedIP)))
	return req
}

// rebinding builds a REBINDING REQUEST: the bound address in ciaddr, no
// option 50, no server-id (broadcast).
func rebinding(t *testing.T, mac net.HardwareAddr, boundIP string) *dhcpv4.DHCPv4 {
	t.Helper()
	req, err := dhcpv4.New(dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest))
	if err != nil {
		t.Fatal(err)
	}
	req.ClientHWAddr = mac
	req.ClientIPAddr = net.ParseIP(boundIP)
	return req
}

func isNAK(reply *dhcpv4.DHCPv4) bool {
	return reply != nil && reply.MessageType() == dhcpv4.MessageTypeNak
}

func isACK(reply *dhcpv4.DHCPv4) bool {
	return reply != nil && reply.MessageType() == dhcpv4.MessageTypeAck
}

// TestSplitPool_NAKsPeerSliceInitReboot: an INIT-REBOOT for an address in the
// PEER's slice is NAK'd (force re-DISCOVER to a slice owner).
func TestSplitPool_NAKsPeerSliceInitReboot(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x01}
	reply := h.HandlePacketOn(initReboot(t, mac, "10.254.241.175"), "eno3") // peer slice
	if !isNAK(reply) {
		t.Fatalf("expected NAK for peer-slice INIT-REBOOT, got %v", reply)
	}
}

// TestSplitPool_NAKsPeerSliceRebinding: a REBINDING (ciaddr) for an address in
// the peer's slice is NAK'd — proves we read ciaddr, not just option 50.
func TestSplitPool_NAKsPeerSliceRebinding(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x02}
	reply := h.HandlePacketOn(rebinding(t, mac, "10.254.241.180"), "eno3") // peer slice
	if !isNAK(reply) {
		t.Fatalf("expected NAK for peer-slice REBINDING, got %v", reply)
	}
}

// TestSplitPool_RenewsMySlice: an INIT-REBOOT for an address the client holds
// in MY slice is ACK'd (renewed) with the SAME IP, not NAK'd. We seed the
// lease via a DISCOVER+REQUEST first (the allocator returns an existing
// in-range lease as-is), then INIT-REBOOT renews it.
func TestSplitPool_RenewsMySlice(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x03}

	// Acquire a lease from my slice (first free = .100).
	offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3")
	if offer == nil {
		t.Fatal("expected an offer to seed the lease")
	}
	held := offer.YourIPAddr.String()

	// INIT-REBOOT for the held address (in my slice) → ACK, same IP.
	reply := h.HandlePacketOn(initReboot(t, mac, held), "eno3")
	if !isACK(reply) {
		t.Fatalf("expected ACK for my-slice INIT-REBOOT, got %v", reply)
	}
	if reply.YourIPAddr.String() != held {
		t.Fatalf("expected to renew %s, got %v", held, reply.YourIPAddr)
	}
}

// TestSplitPool_NAKsOutsideSubnet: a request for an address outside the OOB
// subnet entirely is NAK'd (wrong segment).
func TestSplitPool_NAKsOutsideSubnet(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x04}
	reply := h.HandlePacketOn(initReboot(t, mac, "192.168.1.5"), "eno3")
	if !isNAK(reply) {
		t.Fatalf("expected NAK for out-of-subnet request, got %v", reply)
	}
}

// TestSplitPool_ReservationNotNAKd: a reserved MAC doing INIT-REBOOT for its
// reservation is ACK'd by either gateway regardless of slice.
func TestSplitPool_ReservationNotNAKd(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:00:00:05" {
			return net.ParseIP("10.254.240.152") // reserved, in neither dynamic slice
		}
		return nil
	}
	h := NewHandler(cfg)
	h.SetIfaceParams("eno3", splitPoolParams())

	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x05}
	reply := h.HandlePacketOn(initReboot(t, mac, "10.254.240.152"), "eno3")
	if !isACK(reply) {
		t.Fatalf("expected ACK for reserved-IP INIT-REBOOT, got %v", reply)
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.254.240.152")) {
		t.Fatalf("expected reserved IP, got %v", reply.YourIPAddr)
	}
}

// TestSplitPool_SelectingOtherServerStaysSilent: a SELECTING REQUEST naming
// the OTHER gateway (server-id != me) is ignored, NOT NAK'd — even for a
// peer-slice address. This is the RFC 2131 rule that prevents racing the
// selected gateway's ACK.
func TestSplitPool_SelectingOtherServerStaysSilent(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x06}
	req := initReboot(t, mac, "10.254.241.175") // peer-slice requested IP
	// Make it SELECTING by adding a server-id naming the peer gateway.
	req.UpdateOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.254.240.12")))
	reply := h.HandlePacketOn(req, "eno3")
	if reply != nil {
		t.Fatalf("expected silence for SELECTING naming another server, got %v", reply.MessageType())
	}
}

// TestSplitPool_ReservationsOnlyGatewayServesReservations: a gateway that
// owns NO dynamic slice (RangeStart nil) but has a valid OOB subnet/gateway
// must STILL serve its static reservations — poolForRequest must not drop
// the packet just because the dynamic range is empty. Regression guard for
// the reservations-only HIGH (poolForRequest keyed on RangeStart).
func TestSplitPool_ReservationsOnlyGatewayServesReservations(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.254.240.0/22")
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "28:29:86:ad:67:e5" {
			return net.ParseIP("10.254.240.152")
		}
		return nil
	}
	h := NewHandler(cfg)
	// OOB params with a subnet+gateway but NO dynamic range (this gateway
	// owns no slice) — still carries peer slices for NAK classification.
	h.SetIfaceParams("eno3", IfaceParams{
		ServerIP: net.ParseIP("10.254.240.99"),
		DynamicSlices: []DynamicSlice{
			{Start: net.ParseIP("10.254.241.100"), End: net.ParseIP("10.254.241.149")},
		},
		PoolConfig: PoolConfig{
			Subnet:      *subnet,
			Gateway:     net.ParseIP("10.254.240.1"),
			Promiscuous: true,
			// RangeStart/End deliberately nil — reservations-only.
		},
	})

	mac := net.HardwareAddr{0x28, 0x29, 0x86, 0xad, 0x67, 0xe5}
	offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3")
	if offer == nil {
		t.Fatal("reservations-only gateway must still serve a reservation, got nil")
	}
	if !offer.YourIPAddr.Equal(net.ParseIP("10.254.240.152")) {
		t.Fatalf("expected reserved IP 10.254.240.152, got %v", offer.YourIPAddr)
	}

	// A NON-reserved MAC on the reservations-only gateway gets no offer (the
	// slice-owning gateway answers instead) — and must not panic on the nil
	// range.
	unk := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x42}
	if reply := h.HandlePacketOn(mustDiscover(t, unk), "eno3"); reply != nil {
		t.Fatalf("non-reserved MAC on reservations-only gateway should get no offer, got %v", reply.YourIPAddr)
	}
}

// TestSplitPool_NoSlicesUnknownDynamicNAKs: without split slices
// (single-gateway / fabric), an INIT-REBOOT for a dynamic address with no
// matching lease is NAK'd, not silently adopted or allocated a different
// address. Adoption requires slice ownership (a deterministic single
// owner); an unsliced segment has none, so two active workers adopting
// from purely local state could grant the same address to different MACs.
// This is also the regression guard for the old bug this fix replaces: the
// previous behavior fell through to allocateIP, which ignores the
// requested address and could ACK a DIFFERENT free IP in yiaddr — see
// TestOldBug_UnknownRenewNeverGetsDifferentYiaddr.
func TestSplitPool_NoSlicesUnknownDynamicNAKs(t *testing.T) {
	h := NewHandler(testConfig()) // base config, no iface params, no slices
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, 0x07}
	// base pool is 10.99.1.100-200; request an in-pool addr via INIT-REBOOT
	// with no prior lease for this MAC.
	reply := h.HandlePacket(initReboot(t, mac, "10.99.1.120"))
	if !isNAK(reply) {
		t.Fatalf("unsliced segment must NAK an unknown dynamic REQUEST, got %v", reply)
	}
}

// pairSitePool is the fabric PoolConfig every worker of one site builds
// from the site snapshot: a base range and a relayed /24 pool, both shared,
// plus a generated /31 relay pool.
func pairSitePool() PoolConfig {
	return PoolConfig{
		Subnet:     net.IPNet{IP: net.IPv4(10, 9, 0, 0).To4(), Mask: net.CIDRMask(24, 32)},
		RangeStart: net.IPv4(10, 9, 0, 100).To4(),
		RangeEnd:   net.IPv4(10, 9, 0, 120).To4(),
		Gateway:    net.IPv4(10, 9, 0, 1).To4(),
		RelaySubnets: []RelaySubnet{{
			Subnet:     net.IPNet{IP: net.IPv4(10, 9, 1, 0).To4(), Mask: net.CIDRMask(24, 32)},
			RangeStart: net.IPv4(10, 9, 1, 100).To4(),
			RangeEnd:   net.IPv4(10, 9, 1, 120).To4(),
			Gateway:    net.IPv4(10, 9, 1, 1).To4(),
		}, {
			Subnet:     net.IPNet{IP: net.IPv4(10, 9, 2, 0).To4(), Mask: net.CIDRMask(31, 32)},
			RangeStart: net.IPv4(10, 9, 2, 1).To4(),
			RangeEnd:   net.IPv4(10, 9, 2, 1).To4(),
			Gateway:    net.IPv4(10, 9, 2, 0).To4(),
		}},
	}
}

// pairWorker builds worker index of a two-worker site the way the site
// worker does: the site pool narrowed to its share, its own server address,
// and an ARP probe on its own link, which cannot see a relayed subnet.
func pairWorker(serverIP net.IP, lease time.Duration, index int) *Handler {
	h := NewHandler(Config{
		ServerIP:      serverIP,
		DNS:           []net.IP{net.IPv4(10, 9, 0, 1).To4()},
		LeaseDuration: lease,
		HTTPBootPort:  8080,
		ARPProbe:      func(net.IP) bool { return false },
		PoolConfig:    pairSitePool().WorkerShare(index, 2),
	})
	h.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return h
}

func pairPacket(t *testing.T, mt dhcpv4.MessageType, mac net.HardwareAddr, giaddr net.IP, mods ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	t.Helper()
	p, err := dhcpv4.New(append([]dhcpv4.Modifier{
		dhcpv4.WithMessageType(mt), dhcpv4.WithHwAddr(mac), dhcpv4.WithGatewayIP(giaddr),
	}, mods...)...)
	if err != nil {
		t.Fatalf("build packet: %v", err)
	}
	return p
}

// pairRequest sends a SELECTING REQUEST for addr naming serverIP to both
// workers, as a dual-target relay does, and returns the address ACKed.
func pairRequest(t *testing.T, workers []*Handler, mac net.HardwareAddr, giaddr, addr, serverIP net.IP) net.IP {
	t.Helper()
	req := pairPacket(t, dhcpv4.MessageTypeRequest, mac, giaddr,
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(addr)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverIP)))
	var got net.IP
	for _, h := range workers {
		if rep := h.HandlePacket(req); rep != nil && rep.MessageType() == dhcpv4.MessageTypeAck {
			got = rep.YourIPAddr.To4()
		}
	}
	if got == nil {
		t.Fatalf("no ACK for %s", mac)
	}
	return got
}

// Two active workers of one site never lease one relayed address to two
// hosts. A relayed DISCOVER reaches both workers (dual-target relay); each
// offers and the host takes one offer. The worker whose offer was not taken
// forgets it after one lease time, because the host renews by unicast with
// the worker that granted the lease, so the two lease tables disagree about
// which addresses are free, and the ARP probe cannot see a relayed subnet.
// When both allocated from the whole range, the forgetful worker leased
// host 1's address to the next host that took its offer. The same happens
// when a VIP moves to a worker that starts from its own lease table.
func TestFabricShares_PairDoesNotReleaseRenewedAddress(t *testing.T) {
	const lease = 10 * time.Minute
	relay := net.IPv4(10, 9, 0, 250).To4() // giaddr: the leaf's address in the base subnet
	serverA, serverB := net.IPv4(10, 9, 0, 2).To4(), net.IPv4(10, 9, 0, 3).To4()
	workerA, workerB := pairWorker(serverA, lease, 0), pairWorker(serverB, lease, 1)
	both := []*Handler{workerA, workerB}

	// selecting runs DISCOVER against both workers and accepts the offer of
	// the one named, as a host does with the first OFFER it receives.
	selecting := func(mac net.HardwareAddr, take *Handler, takeServerIP net.IP) net.IP {
		t.Helper()
		var offered net.IP
		for _, h := range both {
			offer := h.HandlePacket(pairPacket(t, dhcpv4.MessageTypeDiscover, mac, relay))
			if offer == nil || offer.MessageType() != dhcpv4.MessageTypeOffer {
				t.Fatalf("no OFFER for %s", mac)
			}
			if h == take {
				offered = offer.YourIPAddr.To4()
			}
		}
		return pairRequest(t, both, mac, relay, offered, takeServerIP)
	}
	// elapse moves both workers' clocks forward (the handler compares
	// time.Now() with lease expiries only).
	elapse := func(d time.Duration) {
		for _, h := range both {
			h.mu.Lock()
			for _, l := range h.leases {
				l.Expiry = l.Expiry.Add(-d)
			}
			h.mu.Unlock()
		}
	}

	host1 := net.HardwareAddr{0x02, 0, 0, 0, 0x60, 0x01}
	host2 := net.HardwareAddr{0x02, 0, 0, 0, 0x60, 0x02}

	addr1 := selecting(host1, workerA, serverA)

	// Host 1 renews with worker A at half the lease time, twice; worker B,
	// whose offer was not taken, sees none of it and its offer expires.
	for range 2 {
		elapse(lease / 2)
		renew := workerA.HandlePacket(pairPacket(t, dhcpv4.MessageTypeRequest, host1, relay, dhcpv4.WithClientIP(addr1)))
		if renew == nil || renew.MessageType() != dhcpv4.MessageTypeAck || !renew.YourIPAddr.Equal(addr1) {
			t.Fatalf("host 1's renewal of %s with the worker that granted it was not ACKed: %v", addr1, renew)
		}
	}
	elapse(time.Minute)

	addr2 := selecting(host2, workerB, serverB)
	if addr2.Equal(addr1) {
		t.Fatalf("host 1 holds a current lease on %s from worker A (renewed a minute ago) and worker B leased the same address to host 2", addr1)
	}
}

// The same property with no time passing, on a relay pool that is not the
// base subnet: two hosts boot together, their relayed DISCOVERs reach the
// two workers in opposite orders, and each host takes the offer of the
// worker that heard it first. When both allocated the lowest address free
// in their own table, both hosts were ACKed the first address of the range.
func TestFabricShares_PairDoesNotLeaseOneAddressTwiceInABootWave(t *testing.T) {
	relay := net.IPv4(10, 9, 1, 1).To4() // giaddr inside the relay pool's subnet
	serverA, serverB := net.IPv4(10, 9, 0, 2).To4(), net.IPv4(10, 9, 0, 3).To4()
	workerA, workerB := pairWorker(serverA, 10*time.Minute, 0), pairWorker(serverB, 10*time.Minute, 1)
	both := []*Handler{workerA, workerB}

	host1 := net.HardwareAddr{0x02, 0, 0, 0, 0x61, 0x01}
	host2 := net.HardwareAddr{0x02, 0, 0, 0, 0x61, 0x02}

	discover := func(h *Handler, mac net.HardwareAddr) net.IP {
		t.Helper()
		offer := h.HandlePacket(pairPacket(t, dhcpv4.MessageTypeDiscover, mac, relay))
		if offer == nil || offer.MessageType() != dhcpv4.MessageTypeOffer {
			t.Fatalf("no OFFER for %s", mac)
		}
		return offer.YourIPAddr.To4()
	}

	// Worker A hears host 1 first, worker B hears host 2 first.
	offerA1 := discover(workerA, host1)
	offerB2 := discover(workerB, host2)
	discover(workerA, host2)
	discover(workerB, host1)

	addr1 := pairRequest(t, both, host1, relay, offerA1, serverA)
	addr2 := pairRequest(t, both, host2, relay, offerB2, serverB)
	if addr2.Equal(addr1) {
		t.Fatalf("worker A leased %s to host 1 and worker B leased the same address to host 2 in the same boot wave", addr1)
	}
}

// A worker NAKs a REBINDING for an address in its peer's share that it
// holds no lease for, so the host re-DISCOVERs into a share instead of being
// ACKed an address the peer may have leased to someone else.
func TestFabricShares_RebindingIntoPeerShareIsNAKed(t *testing.T) {
	relay := net.IPv4(10, 9, 1, 1).To4()
	workerB := pairWorker(net.IPv4(10, 9, 0, 3).To4(), 10*time.Minute, 1)
	host := net.HardwareAddr{0x02, 0, 0, 0, 0x62, 0x01}
	// 10.9.1.100 is the first address of worker A's share.
	rebind := pairPacket(t, dhcpv4.MessageTypeRequest, host, relay, dhcpv4.WithClientIP(net.IPv4(10, 9, 1, 100).To4()))
	if reply := workerB.HandlePacket(rebind); !isNAK(reply) {
		t.Fatalf("expected NAK for a REBINDING into the peer's share, got %v", reply)
	}
}

func TestWorkerShare_SplitsSharedRangesAndKeepsSlash31Whole(t *testing.T) {
	site := pairSitePool()
	a, b := site.WorkerShare(0, 2), site.WorkerShare(1, 2)

	ip := func(s string) net.IP { return net.ParseIP(s).To4() }
	checkRange := func(name string, gotStart, gotEnd net.IP, wantStart, wantEnd string) {
		t.Helper()
		if !gotStart.Equal(ip(wantStart)) || !gotEnd.Equal(ip(wantEnd)) {
			t.Errorf("%s = %v-%v, want %s-%s", name, gotStart, gotEnd, wantStart, wantEnd)
		}
	}
	// 21 addresses split 10 + 11, contiguous and covering the range.
	checkRange("worker 0 base", a.RangeStart, a.RangeEnd, "10.9.0.100", "10.9.0.109")
	checkRange("worker 1 base", b.RangeStart, b.RangeEnd, "10.9.0.110", "10.9.0.120")
	checkRange("worker 0 relay /24", a.RelaySubnets[0].RangeStart, a.RelaySubnets[0].RangeEnd, "10.9.1.100", "10.9.1.109")
	checkRange("worker 1 relay /24", b.RelaySubnets[0].RangeStart, b.RelaySubnets[0].RangeEnd, "10.9.1.110", "10.9.1.120")
	// The /31 pool's one client address is derived from giaddr: both keep it.
	checkRange("worker 0 relay /31", a.RelaySubnets[1].RangeStart, a.RelaySubnets[1].RangeEnd, "10.9.2.1", "10.9.2.1")
	checkRange("worker 1 relay /31", b.RelaySubnets[1].RangeStart, b.RelaySubnets[1].RangeEnd, "10.9.2.1", "10.9.2.1")

	// The site pool itself is not modified.
	checkRange("site base", site.RangeStart, site.RangeEnd, "10.9.0.100", "10.9.0.120")
	checkRange("site relay /24", site.RelaySubnets[0].RangeStart, site.RelaySubnets[0].RangeEnd, "10.9.1.100", "10.9.1.120")

	// A worker without a position takes no share of a split range.
	none := site.WorkerShare(-1, 2)
	if none.RangeStart != nil || none.RangeEnd != nil || none.RelaySubnets[0].RangeStart != nil {
		t.Errorf("index -1 kept a share: base %v-%v, relay %v", none.RangeStart, none.RangeEnd, none.RelaySubnets[0].RangeStart)
	}
	checkRange("index -1 relay /31", none.RelaySubnets[1].RangeStart, none.RelaySubnets[1].RangeEnd, "10.9.2.1", "10.9.2.1")

	// One worker keeps the whole range.
	solo := site.WorkerShare(0, 1)
	checkRange("sole worker base", solo.RangeStart, solo.RangeEnd, "10.9.0.100", "10.9.0.120")
}

func TestShareOfRange_PartsAreContiguousAndCoverTheRange(t *testing.T) {
	for _, tc := range []struct {
		start, end string
		n          int
	}{
		{"10.0.0.100", "10.0.0.150", 2}, // 51 addresses
		{"10.0.0.100", "10.0.0.160", 3},
		{"10.0.0.7", "10.0.0.7", 2}, // fewer addresses than workers
		{"10.0.0.250", "10.0.1.5", 4},
	} {
		start, end := net.ParseIP(tc.start).To4(), net.ParseIP(tc.end).To4()
		next := start
		for i := range tc.n {
			lo, hi := shareOfRange(start, end, i, tc.n)
			if lo == nil {
				continue
			}
			if !lo.Equal(next) {
				t.Errorf("%s-%s/%d part %d starts at %s, want %s", tc.start, tc.end, tc.n, i, lo, next)
			}
			next = nextIP(hi)
		}
		if !next.Equal(nextIP(end)) {
			t.Errorf("%s-%s/%d parts end before %s, want them to reach %s", tc.start, tc.end, tc.n, next, end)
		}
	}
}
