package dhcp

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// Phase 1 (docs/plans/2026-07-13-dhcp-lease-visibility.md) coverage: the
// unknown-renew fix. A REQUEST for an address the handler has no lease for
// is answered by adopt-in-place (own dynamic slice, no conflict) or NAK'd —
// never ACK'd with a DIFFERENT yiaddr, which was the bug.

// TestAdopt_OwnSliceUnleasedIP_AdoptsInPlace: an INIT-REBOOT for an unleased
// address inside this gateway's own dynamic slice, with no prior lease for
// the MAC, is ACK'd at exactly the requested IP and a lease is written (both
// in-memory and to the LeaseStore).
func TestAdopt_OwnSliceUnleasedIP_AdoptsInPlace(t *testing.T) {
	store := newMemLeaseStore()
	cfg := testConfig()
	cfg.LeaseStore = store
	h := NewHandler(cfg)
	h.SetIfaceParams("eno3", splitPoolParams())

	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x01}
	reqIP := "10.254.241.110" // free, in my slice (.100-.149)

	reply := h.HandlePacketOn(initReboot(t, mac, reqIP), "eno3")
	if !isACK(reply) {
		t.Fatalf("expected ACK for adopt-in-place, got %v", reply)
	}
	if !reply.YourIPAddr.Equal(net.ParseIP(reqIP)) {
		t.Fatalf("expected ACK at requested IP %s, got %v", reqIP, reply.YourIPAddr)
	}

	if !h.HasLease(reqIP) {
		t.Fatal("expected adopted lease in the in-memory table")
	}
	saved := store.LoadLeases()
	found := false
	for _, pl := range saved {
		if pl.MAC.String() == mac.String() && pl.IP.Equal(net.ParseIP(reqIP)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected adopted lease persisted to LeaseStore, got %+v", saved)
	}
}

// TestAdopt_RebindingShape_AlsoAdopts: the same adopt-in-place path fires for
// a REBINDING REQUEST (ciaddr set, no option 50, no server-id) — not just
// INIT-REBOOT — since requestedAddr reads ciaddr too.
func TestAdopt_RebindingShape_AlsoAdopts(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x02}
	reqIP := "10.254.241.111"

	reply := h.HandlePacketOn(rebinding(t, mac, reqIP), "eno3")
	if !isACK(reply) {
		t.Fatalf("expected ACK for REBINDING adopt-in-place, got %v", reply)
	}
	if !reply.YourIPAddr.Equal(net.ParseIP(reqIP)) {
		t.Fatalf("expected ACK at requested IP %s, got %v", reqIP, reply.YourIPAddr)
	}
	if !h.HasLease(reqIP) {
		t.Fatal("expected adopted lease in the in-memory table")
	}
}

// TestAdopt_RefusedWhenLeasedToAnotherMAC: an unleased-for-THIS-MAC address
// that's actually held by a different MAC must NAK, not adopt and not
// silently reassign.
func TestAdopt_RefusedWhenLeasedToAnotherMAC(t *testing.T) {
	h := splitPoolHandler()
	holder := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x03}
	claimant := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x04}

	// holder acquires a lease via DORA.
	offer := h.HandlePacketOn(mustDiscover(t, holder), "eno3")
	if offer == nil {
		t.Fatal("expected offer to seed the held lease")
	}
	held := offer.YourIPAddr.String()

	reply := h.HandlePacketOn(initReboot(t, claimant, held), "eno3")
	if !isNAK(reply) {
		t.Fatalf("expected NAK when requested IP is held by another MAC, got %v", reply)
	}
	if got := h.MACForIP(held); got != holder.String() {
		t.Fatalf("held lease must not be reassigned: MACForIP(%s) = %s, want %s", held, got, holder)
	}
}

// TestClassify_ExistingLeaseMismatchNAKs: a MAC with an existing lease for
// IP A that REQUESTs a different IP B must be NAK'd — never silently ACK'd
// at B (would create a second lease) and never renewed at A instead of the
// requested B (would silently substitute the address).
func TestClassify_ExistingLeaseMismatchNAKs(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x05}

	offer := h.HandlePacketOn(mustDiscover(t, mac), "eno3")
	if offer == nil {
		t.Fatal("expected offer")
	}
	held := offer.YourIPAddr

	other := net.ParseIP("10.254.241.148") // different free address, same slice
	if held.Equal(other) {
		t.Fatal("test fixture collision: pick a different 'other' address")
	}

	reply := h.HandlePacketOn(initReboot(t, mac, other.String()), "eno3")
	if !isNAK(reply) {
		t.Fatalf("expected NAK for existing-lease mismatch, got %v", reply)
	}
	if got := h.MACForIP(held.String()); got != mac.String() {
		t.Fatalf("existing lease must be untouched by the mismatched REQUEST: MACForIP(%s) = %s", held, got)
	}
}

// TestClassify_ReservationRequest_ACKsAtReservedIP is a regression guard:
// a REQUEST naming the MAC's own reservation is ACK'd at that IP via the
// fallthrough path (unchanged behavior).
func TestClassify_ReservationRequest_ACKsAtReservedIP(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:00:01:06" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x06}

	reply := h.HandlePacket(initReboot(t, mac, "10.99.1.50"))
	if !isACK(reply) {
		t.Fatalf("expected ACK at reserved IP, got %v", reply)
	}
	if !reply.YourIPAddr.Equal(net.ParseIP("10.99.1.50")) {
		t.Fatalf("expected reserved IP, got %v", reply.YourIPAddr)
	}
}

// TestClassify_ReservationMismatchNAKs: a MAC WITH a reservation that
// REQUESTs a different address (not its reservation) must NAK — the
// reservation is definitive; allocateIP's reservation branch would silently
// serve the reservation regardless of what was requested if we let this
// fall through, ACKing at a different yiaddr than asked for.
func TestClassify_ReservationMismatchNAKs(t *testing.T) {
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == "52:54:00:00:01:07" {
			return net.ParseIP("10.99.1.50")
		}
		return nil
	}
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x07}

	reply := h.HandlePacket(initReboot(t, mac, "10.99.1.120")) // in dynamic pool, not the reservation
	if !isNAK(reply) {
		t.Fatalf("expected NAK for reservation mismatch, got %v", reply)
	}
}

// TestClassify_StaticBandNonReserved_NAKs: an in-subnet REQUEST for an
// address outside the dynamic pool's range, from a MAC with no reservation
// at all, must NAK (not adopt: it isn't even a dynamic address).
func TestClassify_StaticBandNonReserved_NAKs(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x08}

	// 10.254.240.50 is in the /22 OOB subnet but outside both dynamic
	// slices (.241.100-149 / .241.150-199) and this MAC has no reservation.
	reply := h.HandlePacketOn(initReboot(t, mac, "10.254.240.50"), "eno3")
	if !isNAK(reply) {
		t.Fatalf("expected NAK for non-reserved static-band REQUEST, got %v", reply)
	}
}

// TestSplitPool_FullDORA_StillWorks: DISCOVER->OFFER->REQUEST->ACK on the
// sliced (split-pool) segment must be unaffected by the classification
// added in front of allocateIP. The REQUEST here is SELECTING (server-id
// naming this gateway, option 50 = the offered IP), which classifyRequest
// resolves via the "existing lease match" fallthrough — the DISCOVER
// already wrote the lease.
func TestSplitPool_FullDORA_StillWorks(t *testing.T) {
	h := splitPoolHandler()
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x09}

	offer := h.HandlePacketOn(mustDiscover(t, mac, withArch(iana.EFI_X86_64)), "eno3")
	if offer == nil {
		t.Fatal("expected offer")
	}
	request := mustRequestFromOffer(t, offer)
	ack := h.HandlePacketOn(request, "eno3")
	if !isACK(ack) {
		t.Fatalf("expected ACK, got %v", ack)
	}
	if !offer.YourIPAddr.Equal(ack.YourIPAddr) {
		t.Fatalf("offer IP %v != ack IP %v", offer.YourIPAddr, ack.YourIPAddr)
	}
}

// TestUnsliced_FullDORA_StillWorks: same DORA regression, unsliced/base
// config — duplicated (deliberately) alongside TestDORA_FullCycle to keep
// the Phase-1 coverage self-contained in this file.
func TestUnsliced_FullDORA_StillWorks(t *testing.T) {
	h := NewHandler(testConfig())
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x0a}

	offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("expected offer")
	}
	ack := h.HandlePacket(mustRequestFromOffer(t, offer))
	if !isACK(ack) {
		t.Fatalf("expected ACK, got %v", ack)
	}
	if !offer.YourIPAddr.Equal(ack.YourIPAddr) {
		t.Fatalf("offer IP %v != ack IP %v", offer.YourIPAddr, ack.YourIPAddr)
	}
}

// TestOldBug_UnknownRenewNeverGetsDifferentYiaddr reconstructs the exact
// scenario that used to be mishandled: an unsliced segment, a pool with
// free addresses, and a REQUEST for an address the handler has no lease
// for. Before the fix, this fell through to allocateIP, which ignores the
// requested address and can allocate a DIFFERENT free IP, ACKing with that
// in yiaddr — a silent split between what the client keeps and what the
// lease table records. The fix requires this to NAK instead.
func TestOldBug_UnknownRenewNeverGetsDifferentYiaddr(t *testing.T) {
	h := NewHandler(testConfig()) // unsliced; pool .100-.200 has plenty of free IPs
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x01, 0x0b}

	reply := h.HandlePacket(initReboot(t, mac, "10.99.1.150")) // unknown to us, no lease exists
	if reply == nil {
		t.Fatal("expected a reply (NAK), got silence")
	}
	if isACK(reply) {
		t.Fatalf("old bug reproduced: got ACK with yiaddr %v for an unknown requested IP "+
			"(should NAK, never ACK a different address)", reply.YourIPAddr)
	}
	if !isNAK(reply) {
		t.Fatalf("expected NAK, got message type %v", reply.MessageType())
	}
}

// removableReservation returns a ReserveIP that reserves ip for reservedMAC
// until remove is called, standing in for a snapshot that drops the
// reservation while the MAC's lease on ip is still live.
func removableReservation(ip string) (reserve func(string) net.IP, remove func()) {
	present := true
	reserve = func(mac string) net.IP {
		if present && mac == reservedMAC {
			return net.ParseIP(ip)
		}
		return nil
	}
	return reserve, func() { present = false }
}

// TestClassify_OwnLeaseNoLongerServable_NAKs: a REQUEST naming an address is
// answered at that address or NAKed, never ACKed with a different one. Here
// the MAC DOES hold a live lease on the requested address, but a snapshot has
// since taken that address out of the pool, so the REQUEST must be NAKed and
// the client re-DISCOVERs. Before the fix, classifyRequest treated any match
// with the live lease as a normal renew, but allocateIP renews only a lease
// inside the pool's dynamic range: for any other lease it allocated a fresh
// address and ACKed that, or, on a reservations-only gateway, found nothing
// and the REQUEST was dropped as "pool exhausted".
func TestClassify_OwnLeaseNoLongerServable_NAKs(t *testing.T) {
	type scenario struct {
		h      *Handler
		iface  string
		held   string // address the DISCOVER must offer
		change func(h *Handler)
	}
	scenarios := []struct {
		name  string
		build func() scenario
	}{
		{
			name: "reservation removed, address outside the dynamic range",
			build: func() scenario {
				reserve, remove := removableReservation("10.99.1.20")
				cfg := testConfig() // dynamic range .100-.200
				cfg.ReserveIP = reserve
				return scenario{h: NewHandler(cfg), held: "10.99.1.20", change: func(*Handler) { remove() }}
			},
		},
		{
			name: "dynamic range reloaded away from the address",
			build: func() scenario {
				cfg := testConfig()
				cfg.RangeEnd = net.ParseIP("10.99.1.150")
				moved := cfg.PoolConfig
				moved.RangeStart = net.ParseIP("10.99.1.151")
				moved.RangeEnd = net.ParseIP("10.99.1.200")
				return scenario{h: NewHandler(cfg), held: "10.99.1.100", change: func(h *Handler) { h.UpdatePools(moved) }}
			},
		},
		{
			name: "split-pool slices rebalanced, address now in a peer's slice",
			build: func() scenario {
				h := NewHandler(testConfig())
				h.SetIfaceParams("eno3", splitPoolParams()) // mine .241.100-.149
				rebalanced := splitPoolParams()
				rebalanced.RangeStart = net.ParseIP("10.254.241.120")
				rebalanced.DynamicSlices = []DynamicSlice{
					{Start: net.ParseIP("10.254.241.120"), End: net.ParseIP("10.254.241.149")},
					{Start: net.ParseIP("10.254.241.100"), End: net.ParseIP("10.254.241.119")},
					{Start: net.ParseIP("10.254.241.150"), End: net.ParseIP("10.254.241.199")},
				}
				return scenario{h: h, iface: "eno3", held: "10.254.241.100", change: func(h *Handler) { h.SetIfaceParams("eno3", rebalanced) }}
			},
		},
		{
			name: "reservation removed on a reservations-only gateway",
			build: func() scenario {
				reserve, remove := removableReservation("10.254.240.50")
				cfg := testConfig()
				cfg.ReserveIP = reserve
				h := NewHandler(cfg)
				h.SetIfaceParams("eno3", reservationsOnlyParams())
				return scenario{h: h, iface: "eno3", held: "10.254.240.50", change: func(*Handler) { remove() }}
			},
		},
	}
	shapes := []struct {
		name  string
		build func(t *testing.T, mac net.HardwareAddr, ip string) *dhcpv4.DHCPv4
	}{
		{"RENEWING/REBINDING (ciaddr)", rebinding},
		{"INIT-REBOOT (option 50)", initReboot},
	}
	mac, _ := net.ParseMAC(reservedMAC)
	for _, sc := range scenarios {
		for _, shape := range shapes {
			t.Run(sc.name+"/"+shape.name, func(t *testing.T) {
				s := sc.build()
				offer := s.h.HandlePacketOn(mustDiscover(t, mac), s.iface)
				if offer == nil || offer.YourIPAddr.String() != s.held {
					t.Fatalf("setup: want an OFFER of %s, got %v", s.held, offer)
				}
				s.change(s.h)

				reply := s.h.HandlePacketOn(shape.build(t, mac, s.held), s.iface)
				switch {
				case reply == nil:
					t.Fatalf("REQUEST for %s got no reply; want NAK", s.held)
				case isACK(reply):
					t.Fatalf("REQUEST for %s was ACKed with yiaddr %v; want NAK", s.held, reply.YourIPAddr)
				case !isNAK(reply):
					t.Fatalf("REQUEST for %s got %v; want NAK", s.held, reply.MessageType())
				}
				for _, l := range s.h.Leases() {
					if l.MAC == mac.String() && l.IP != s.held {
						t.Fatalf("NAKed REQUEST for %s moved the MAC's lease to %s", s.held, l.IP)
					}
				}
			})
		}
	}
}

// TestClassify_RelayedLeaseUnicastRenew_NeverGetsDirectPoolAddress: a client
// that got its lease through a relay renews by unicast straight to the
// server (RFC 2131 RENEWING: ciaddr set, giaddr 0; the same packet shape the
// rebinding helper builds), so poolForRequest resolves the direct pool,
// which does not cover the relayed lease. Before the fix the renew was ACKed
// with a direct-pool address and router: an address from another subnet. It
// must be answered at the held address or NAKed, and after a NAK the
// client's re-DISCOVER through the relay gets the same address back.
func TestClassify_RelayedLeaseUnicastRenew_NeverGetsDirectPoolAddress(t *testing.T) {
	h := NewHandler(relayConfig()) // direct 10.99.1.0/24, relay 10.99.2.0/24
	mac, _ := net.ParseMAC("52:54:00:00:01:0c")
	relay := net.ParseIP("10.99.2.1")

	offer := h.HandlePacket(mustDiscover(t, mac, withGiaddr(relay)))
	if offer == nil || offer.YourIPAddr.String() != "10.99.2.100" {
		t.Fatalf("setup: want a relayed OFFER of 10.99.2.100, got %v", offer)
	}
	held := offer.YourIPAddr.String()

	reply := h.HandlePacket(rebinding(t, mac, held))
	switch {
	case reply == nil:
		t.Fatalf("unicast renew of %s got no reply; want NAK or ACK at %s", held, held)
	case isACK(reply):
		if reply.YourIPAddr.String() != held {
			t.Fatalf("unicast renew of %s was ACKed with yiaddr %v (router %v); want NAK or ACK at %s",
				held, reply.YourIPAddr, reply.Router(), held)
		}
	case !isNAK(reply):
		t.Fatalf("unicast renew of %s got %v; want NAK or ACK at %s", held, reply.MessageType(), held)
	}

	again := h.HandlePacket(mustDiscover(t, mac, withGiaddr(relay)))
	if again == nil || again.YourIPAddr.String() != held {
		t.Fatalf("re-DISCOVER through the relay: want an OFFER of %s, got %v", held, again)
	}
}
