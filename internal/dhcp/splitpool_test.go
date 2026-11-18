package dhcp

import (
	"net"
	"testing"

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
