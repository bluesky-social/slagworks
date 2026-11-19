package dhcp

import (
	"net"
	"testing"

	"github.com/insomniacslk/dhcp/iana"
)

// A static reservation and another MAC's live lease can name the same
// address in two ways: the reservation moves to a new MAC (the address is
// reassigned in NetBox) while the old holder's lease is still live, or the
// reservation sits inside the dynamic range and a dynamic client drew the
// address first. Either way the lease table must keep at most one unexpired
// lease per address. The reservation takes precedence (Config.ReserveIP), so
// the other MAC's lease is revoked and that MAC is NAKed at its next renew.

// duplicateLeases returns every address that Leases() attributes to more
// than one MAC, mapped to those MACs. Empty when the table is consistent.
func duplicateLeases(h *Handler) map[string][]string {
	byIP := map[string][]string{}
	for _, l := range h.Leases() {
		byIP[l.IP] = append(byIP[l.IP], l.MAC)
	}
	for ip, macs := range byIP {
		if len(macs) < 2 {
			delete(byIP, ip)
		}
	}
	return byIP
}

// The address is reassigned in NetBox from A to B while A still holds a
// live lease on it. B's DISCOVER is offered the address, A's lease is
// revoked (lease table, LeaseStore and a "delete" lease event), and A's
// next renew is NAKed instead of being honored.
func TestReservation_MovedToAnotherMAC_RevokesOldHoldersLease(t *testing.T) {
	macA := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x0a}
	macB := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x0b}
	reservedIP := net.ParseIP("10.99.1.20") // outside the dynamic range
	owner := macA.String()

	rec := &eventRecorder{}
	store := newMemLeaseStore()
	cfg := testConfig()
	cfg.OnLeaseEvent = rec.record
	cfg.LeaseStore = store
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == owner {
			return reservedIP
		}
		return nil
	}
	h := NewHandler(cfg)

	offerA := h.HandlePacket(mustDiscover(t, macA, withArch(iana.EFI_X86_64)))
	if offerA == nil || !offerA.YourIPAddr.Equal(reservedIP) {
		t.Fatalf("setup: want %s offered to A, got %v", reservedIP, offerA)
	}
	if ack := h.HandlePacket(mustRequestFromOffer(t, offerA)); !isACK(ack) {
		t.Fatalf("setup: want A ACKed, got %v", ack)
	}

	owner = macB.String() // a new snapshot moves the reservation to B
	eventsBefore := len(rec.snapshot())

	offerB := h.HandlePacket(mustDiscover(t, macB, withArch(iana.EFI_X86_64)))
	if offerB == nil || !offerB.YourIPAddr.Equal(reservedIP) {
		t.Fatalf("want the reserved address %s offered to B, got %v", reservedIP, offerB)
	}
	if dups := duplicateLeases(h); len(dups) != 0 {
		t.Fatalf("one address is leased to two MACs at once: %v", dups)
	}
	if got := h.MACForIP(reservedIP.String()); got != macB.String() {
		t.Fatalf("MACForIP(%s) = %q, want B (%s)", reservedIP, got, macB)
	}
	for _, pl := range store.LoadLeases() {
		if pl.MAC.String() == macA.String() {
			t.Errorf("A's revoked lease is still in the LeaseStore (%s); a restart would reload the duplicate", pl.IP)
		}
	}
	events := rec.snapshot()[eventsBefore:]
	if len(events) != 2 ||
		events[0].Event != "delete" || events[0].MAC.String() != macA.String() || !events[0].IP.Equal(reservedIP) ||
		events[1].Event != "allocate" || events[1].MAC.String() != macB.String() || !events[1].IP.Equal(reservedIP) {
		t.Errorf("lease events = %+v, want [delete A %s, allocate B %s]", events, reservedIP, reservedIP)
	}

	// A, unaware, renews the address it was holding. It has no lease and
	// no reservation any more, so it must be told to stop using it.
	if reply := h.HandlePacket(rebinding(t, macA, reservedIP.String())); !isNAK(reply) {
		t.Fatalf("want A's renew of %s NAKed, got %v", reservedIP, reply)
	}
	if dups := duplicateLeases(h); len(dups) != 0 {
		t.Fatalf("after A's renew, one address is leased to two MACs: %v", dups)
	}
}

// The reservation lies inside the dynamic range (the docs say the pool
// should not overlap reservations, but nothing rejects it). A dynamic client
// draws the address first; the reserved host then asks for it with an
// INIT-REBOOT REQUEST, the shape a host that already has the address
// configured sends after a reboot. The reserved host is ACKed, the dynamic
// client is NAKed at its next renew and moves to another address, and the
// table never holds two leases on one address.
func TestReservation_InsideDynamicRange_RevokesDynamicClientsLease(t *testing.T) {
	dynamicMAC := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x0d}
	reservedMAC := net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0x0e}
	reservedIP := net.ParseIP("10.99.1.100") // first address of the dynamic range
	cfg := testConfig()
	cfg.ReserveIP = func(mac string) net.IP {
		if mac == reservedMAC.String() {
			return reservedIP
		}
		return nil
	}
	h := NewHandler(cfg)

	offerD := h.HandlePacket(mustDiscover(t, dynamicMAC, withArch(iana.EFI_X86_64)))
	if offerD == nil || !offerD.YourIPAddr.Equal(reservedIP) {
		t.Fatalf("setup: want the dynamic client to draw %s, got %v", reservedIP, offerD)
	}
	if ack := h.HandlePacket(mustRequestFromOffer(t, offerD)); !isACK(ack) {
		t.Fatalf("setup: want the dynamic client ACKed, got %v", ack)
	}

	ackR := h.HandlePacket(initReboot(t, reservedMAC, reservedIP.String()))
	if !isACK(ackR) || !ackR.YourIPAddr.Equal(reservedIP) {
		t.Fatalf("want the reserved host ACKed at %s, got %v", reservedIP, ackR)
	}
	if dups := duplicateLeases(h); len(dups) != 0 {
		t.Fatalf("one address is leased to two MACs at once: %v", dups)
	}
	if got := h.MACForIP(reservedIP.String()); got != reservedMAC.String() {
		t.Fatalf("MACForIP(%s) = %q, want the reserved host (%s)", reservedIP, got, reservedMAC)
	}

	if reply := h.HandlePacket(rebinding(t, dynamicMAC, reservedIP.String())); !isNAK(reply) {
		t.Fatalf("want the dynamic client's renew of %s NAKed, got %v", reservedIP, reply)
	}
	offerD = h.HandlePacket(mustDiscover(t, dynamicMAC, withArch(iana.EFI_X86_64)))
	if offerD == nil || offerD.YourIPAddr.Equal(reservedIP) {
		t.Fatalf("want the dynamic client moved off %s on re-DISCOVER, got %v", reservedIP, offerD)
	}
	if dups := duplicateLeases(h); len(dups) != 0 {
		t.Fatalf("after the dynamic client re-DISCOVERs, one address is leased to two MACs: %v", dups)
	}
}
