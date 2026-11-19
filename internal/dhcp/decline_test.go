package dhcp

import (
	"net"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// RFC 2131 4.3.3: a client that finds its offered address already in use
// (its own ARP check, 4.4.1) sends DHCPDECLINE and restarts at DISCOVER; the
// server MUST mark the address as not available. These tests drive the
// handler with the packets such a client (dhcpcd in the deploy initrd, for
// instance) sends.

// declinePkt builds a DHCPDECLINE the way RFC 2131 Table 5 specifies it:
// ciaddr 0, the declined address in option 50, the server in option 54.
func declinePkt(t *testing.T, mac net.HardwareAddr, ip, serverID net.IP) *dhcpv4.DHCPv4 {
	t.Helper()
	pkt, err := dhcpv4.New(
		dhcpv4.WithHwAddr(mac),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeDecline),
		dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(ip)),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(serverID)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

// bindLease runs DISCOVER/OFFER/REQUEST/ACK for mac and returns the address
// it was ACKed at.
func bindLease(t *testing.T, h *Handler, mac net.HardwareAddr) net.IP {
	t.Helper()
	offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatalf("setup: no OFFER for %s", mac)
	}
	ack := h.HandlePacket(mustRequestFromOffer(t, offer))
	if !isACK(ack) {
		t.Fatalf("setup: want ACK for %s, got %v", mac, ack)
	}
	return ack.YourIPAddr
}

// TestDecline_AddressIsNotOfferedAgainToTheDecliningClient is the
// regression: the handler used to drop DECLINE as an unexpected message
// type and keep the lease, so the client's next DISCOVER took the renew
// path and was offered the very address it had just declined. Each such
// DISCOVER also extended the lease, so the client looped
// DISCOVER/OFFER/REQUEST/ACK/DECLINE for as long as the conflict stood,
// with free addresses in the pool.
func TestDecline_AddressIsNotOfferedAgainToTheDecliningClient(t *testing.T) {
	cfg := testConfig()
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x21}

	declined := bindLease(t, h, mac)

	if reply := h.HandlePacket(declinePkt(t, mac, declined, cfg.ServerIP)); reply != nil {
		t.Fatalf("DECLINE must not be answered, got %v", reply.MessageType())
	}

	if h.HasLease(declined.String()) {
		t.Errorf("declined address %s still has an active lease (held by %q)",
			declined, h.MACForIP(declined.String()))
	}
	again := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64)))
	if again == nil {
		t.Fatal("DISCOVER after DECLINE got no OFFER although the pool has free addresses")
	}
	if again.YourIPAddr.Equal(declined) {
		t.Fatalf("DISCOVER after DECLINE was offered %s again, the address the client just declined as in use", declined)
	}
}

// The declined address is in use by some other host, so no client may be
// handed it while the quarantine lasts, not just the one that declined it.
func TestDecline_AddressIsWithheldFromOtherClients(t *testing.T) {
	cfg := testConfig()
	h := NewHandler(cfg)
	a := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x22}
	b := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x23}

	declined := bindLease(t, h, a)
	h.HandlePacket(declinePkt(t, a, declined, cfg.ServerIP))

	offer := h.HandlePacket(mustDiscover(t, b, withArch(iana.EFI_X86_64)))
	if offer == nil {
		t.Fatal("no OFFER for a second client although the pool has free addresses")
	}
	if offer.YourIPAddr.Equal(declined) {
		t.Fatalf("second client was offered %s, which another client just declined as in use", declined)
	}
}

// The quarantine lasts one lease duration; after that the address returns
// to the pool and the expired entry is pruned.
func TestDecline_QuarantineEndsAfterOneLeaseDuration(t *testing.T) {
	cfg := testConfig()
	cfg.RangeEnd = cfg.RangeStart // one address, so the test can tell when it returns
	h := NewHandler(cfg)
	a := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x24}
	b := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x25}

	declined := bindLease(t, h, a)
	before := time.Now()
	h.HandlePacket(declinePkt(t, a, declined, cfg.ServerIP))
	after := time.Now()

	h.mu.Lock()
	until, ok := h.declined[declined.String()]
	h.mu.Unlock()
	if !ok {
		t.Fatalf("declined address %s was not quarantined", declined)
	}
	if until.Before(before.Add(cfg.LeaseDuration)) || until.After(after.Add(cfg.LeaseDuration)) {
		t.Fatalf("quarantine ends at %v, want one lease duration (%v) after the DECLINE", until, cfg.LeaseDuration)
	}

	if offer := h.HandlePacket(mustDiscover(t, b, withArch(iana.EFI_X86_64))); offer != nil {
		t.Fatalf("only address %s is quarantined, yet a client was offered %s", declined, offer.YourIPAddr)
	}

	// Let the quarantine lapse without sleeping through a lease duration.
	h.mu.Lock()
	h.declined[declined.String()] = time.Now().Add(-time.Second)
	h.mu.Unlock()

	offer := h.HandlePacket(mustDiscover(t, b, withArch(iana.EFI_X86_64)))
	if offer == nil || !offer.YourIPAddr.Equal(declined) {
		t.Fatalf("after the quarantine ended, want an OFFER of %s, got %v", declined, offer)
	}
	h.mu.Lock()
	n := len(h.declined)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired quarantine entry was not pruned: %d left", n)
	}
}

// RFC 2131: option 54 names the server the DECLINE is for. With two site
// workers relaying the same segment, the other one must leave its own
// lease table alone.
func TestDecline_NamingAnotherServerIsIgnored(t *testing.T) {
	h := NewHandler(testConfig())
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x26}

	ip := bindLease(t, h, mac)
	h.HandlePacket(declinePkt(t, mac, ip, net.ParseIP("10.99.1.2")))

	if got := h.MACForIP(ip.String()); got != mac.String() {
		t.Fatalf("DECLINE addressed to another server changed the lease on %s: holder %q, want %s", ip, got, mac)
	}
	h.mu.Lock()
	n := len(h.declined)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("DECLINE addressed to another server quarantined %d address(es)", n)
	}
}

// A DECLINE acts only on the declining client's own lease, so a stray or
// forged one cannot take an address away from another client or withhold
// pool addresses the sender was never given.
func TestDecline_OfAnAddressNotLeasedToTheClientIsIgnored(t *testing.T) {
	cfg := testConfig()
	h := NewHandler(cfg)
	a := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x27}
	b := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x28}

	aIP := bindLease(t, h, a)
	bIP := bindLease(t, h, b)

	h.HandlePacket(declinePkt(t, b, aIP, cfg.ServerIP))                        // a's address
	h.HandlePacket(declinePkt(t, b, net.ParseIP("10.99.1.150"), cfg.ServerIP)) // a free one

	if got := h.MACForIP(aIP.String()); got != a.String() {
		t.Fatalf("b's DECLINE of %s changed a's lease: holder %q, want %s", aIP, got, a)
	}
	if got := h.MACForIP(bIP.String()); got != b.String() {
		t.Fatalf("b's DECLINE of addresses it does not hold dropped its own lease on %s: holder %q", bIP, got)
	}
	h.mu.Lock()
	n := len(h.declined)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("DECLINEs of addresses the sender does not lease quarantined %d address(es)", n)
	}
}

// A lease that has expired but not yet been swept is still the address this
// server last gave the client, and the conflict it reports is still real.
func TestDecline_OfAnExpiredUnsweptLeaseIsHonored(t *testing.T) {
	cfg := testConfig()
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x2d}

	ip := bindLease(t, h, mac)
	h.mu.Lock()
	h.leases[mac.String()].Expiry = time.Now().Add(-time.Second)
	h.mu.Unlock()

	h.HandlePacket(declinePkt(t, mac, ip, cfg.ServerIP))

	h.mu.Lock()
	_, stillListed := h.leases[mac.String()]
	_, quarantined := h.declined[ip.String()]
	h.mu.Unlock()
	if stillListed {
		t.Errorf("declined (expired) lease %s -> %s is still in the lease table", mac, ip)
	}
	if !quarantined {
		t.Errorf("declined address %s was not quarantined", ip)
	}
}

// The dropped lease leaves every place a lease lives: the persisted store
// (or a restart would reload it) and the lease ledger (as a "delete").
func TestDecline_DeletesPersistedLeaseAndEmitsDelete(t *testing.T) {
	store := newMemLeaseStore()
	rec := &eventRecorder{}
	cfg := testConfig()
	cfg.LeaseStore = store
	cfg.OnLeaseEvent = rec.record
	h := NewHandler(cfg)
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x29}

	ip := bindLease(t, h, mac)
	h.HandlePacket(declinePkt(t, mac, ip, cfg.ServerIP))

	for _, pl := range store.LoadLeases() {
		if pl.MAC.String() == mac.String() {
			t.Fatalf("declined lease %s -> %s is still persisted", mac, pl.IP)
		}
	}
	events := rec.snapshot()
	if len(events) == 0 {
		t.Fatal("no lease events recorded")
	}
	last := events[len(events)-1]
	if last.Event != "delete" || last.MAC.String() != mac.String() || !last.IP.Equal(ip) {
		t.Fatalf("last lease event = %s %s %s, want delete %s %s", last.Event, last.MAC, last.IP, mac, ip)
	}
}

// A static reservation is the only address its MAC may have, so a DECLINE
// does not move that MAC elsewhere: the reservation is offered again and
// the conflict is the operator's to resolve (the handler logs it).
func TestDecline_ReservationIsStillOffered(t *testing.T) {
	cfg := testConfig()
	reserved := net.ParseIP("10.99.1.50")
	mac := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x2a}
	cfg.ReserveIP = func(m string) net.IP {
		if m == mac.String() {
			return reserved
		}
		return nil
	}
	h := NewHandler(cfg)

	if ip := bindLease(t, h, mac); !ip.Equal(reserved) {
		t.Fatalf("setup: want reservation %s, got %s", reserved, ip)
	}
	h.HandlePacket(declinePkt(t, mac, reserved, cfg.ServerIP))

	offer := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64)))
	if offer == nil || !offer.YourIPAddr.Equal(reserved) {
		t.Fatalf("after DECLINE of its reservation, want an OFFER of %s, got %v", reserved, offer)
	}
}

// allocateIP releases h.mu while it ARP-probes a candidate. A DECLINE that
// lands in that window quarantines an address the allocator already judged
// free, so the post-probe re-check must honor the quarantine too. The probe
// callback here runs another client's whole DORA and DECLINE inside the
// window, deterministically.
func TestDecline_DuringARPProbeIsHonored(t *testing.T) {
	cfg := testConfig()
	a := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x2b}
	c := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x2c}
	var h *Handler
	var declined net.IP
	inWindow := false
	cfg.ARPProbe = func(net.IP) bool {
		if declined == nil && !inWindow {
			inWindow = true // a's own allocation probes too; don't recurse
			declined = bindLease(t, h, a)
			h.HandlePacket(declinePkt(t, a, declined, cfg.ServerIP))
			inWindow = false
		}
		return false
	}
	h = NewHandler(cfg)

	offer := h.HandlePacket(mustDiscover(t, c, withArch(iana.EFI_X86_64)))
	if declined == nil {
		t.Fatal("setup: the probe window never ran")
	}
	if offer == nil {
		t.Fatal("no OFFER although the pool has free addresses")
	}
	if offer.YourIPAddr.Equal(declined) {
		t.Fatalf("client was offered %s, which was declined while the allocator was probing it", declined)
	}
}
