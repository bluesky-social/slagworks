package snapshot

import (
	"encoding/json"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// TestBuild_EmitsOOBReservations is the end-to-end wiring test: an OOB
// prefix + interfaces on a NON-machine device (a PDU) must produce
// snap.Reservations even though the device has no slagworks Machine row.
// This is the core "reservations span all OOB devices, not just machines"
// guarantee.
func TestBuild_EmitsOOBReservations(t *testing.T) {
	in := Inputs{
		Site:        netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:      model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes:    []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		OOBPrefixes: []netbox.Prefix{{Prefix: "10.254.240.0/22"}},
		// A PDU device with a single mgmt interface — NOT a slagworks machine.
		Devices: []netbox.Device{{ID: 900, Name: "pdu-0206-left.pop1"}},
		Interfaces: []netbox.Interface{
			{ID: 1, Name: "mgmt", MgmtOnly: true, MACAddress: "28:29:86:AD:67:E5", Device: &netbox.NestedRef{ID: 900}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.254.240.152/22", AssignedObjectID: new(1), AssignedObjectType: "dcim.interface"},
		},
		// No Machines: the PDU is not a slagworks-managed host.
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(snap.Reservations) != 1 {
		t.Fatalf("expected 1 OOB reservation for the PDU, got %d: %+v", len(snap.Reservations), snap.Reservations)
	}
	r := snap.Reservations[0]
	if r.MAC != "28:29:86:ad:67:e5" || r.IP != "10.254.240.152" {
		t.Fatalf("wrong reservation: %+v", r)
	}
}

// TestBuild_NoOOBPrefixEmitsNoReservations: a site without a slagworks-oob
// prefix (pre-cutover) carries no reservations even if OOB-range IPs exist.
func TestBuild_NoOOBPrefixEmitsNoReservations(t *testing.T) {
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		Devices:  []netbox.Device{{ID: 900, Name: "pdu.pop3"}},
		Interfaces: []netbox.Interface{
			{ID: 1, Name: "mgmt", MgmtOnly: true, MACAddress: "28:29:86:AD:67:E5", Device: &netbox.NestedRef{ID: 900}},
		},
		IPs: []netbox.IPAddress{
			{Address: "10.254.240.152/22", AssignedObjectID: new(1), AssignedObjectType: "dcim.interface"},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(snap.Reservations) != 0 {
		t.Fatalf("expected no reservations without slagworks-oob prefix, got %+v", snap.Reservations)
	}
}

// oobPrefix is the canonical pop1 OOB prefix used across these tests.
func oobPrefix() []netbox.Prefix {
	return []netbox.Prefix{{Prefix: "10.254.240.0/22"}}
}

// TestBuild_EmitsOOBConfig: the OOB prefix (gateway_ip + dns/ntp/lease CFs)
// and its dynamic ip-ranges (one per gateway, owner CF) flow into snap.OOB.
func TestBuild_EmitsOOBConfig(t *testing.T) {
	oobCF := json.RawMessage(`{"gateway_ip":"10.254.243.253","dns_servers":"10.254.240.11, 10.254.240.12","ntp_servers":"10.254.240.1","dhcp_lease_seconds":300}`)
	in := Inputs{
		Site:        netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:      model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes:    []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		OOBPrefixes: []netbox.Prefix{{Prefix: "10.254.240.0/22", CustomFields: oobCF}},
		OOBRanges: []netbox.IPRange{
			{StartAddress: "10.254.241.100/22", EndAddress: "10.254.241.149/22", CustomFields: json.RawMessage(`{"slagworks_oob_owner":"10.254.240.11"}`)},
			{StartAddress: "10.254.241.150/22", EndAddress: "10.254.241.199/22", CustomFields: json.RawMessage(`{"slagworks_oob_owner":"10.254.240.12"}`)},
		},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	o := snap.OOB
	if o.Subnet != "10.254.240.0/22" || o.Gateway != "10.254.243.253" {
		t.Fatalf("subnet/gateway wrong: %+v", o)
	}
	if len(o.DNS) != 2 || o.DNS[0] != "10.254.240.11" || o.DNS[1] != "10.254.240.12" {
		t.Fatalf("DNS wrong (csv trim?): %+v", o.DNS)
	}
	if len(o.NTP) != 1 || o.NTP[0] != "10.254.240.1" {
		t.Fatalf("NTP wrong: %+v", o.NTP)
	}
	if o.LeaseSeconds != 300 {
		t.Fatalf("lease seconds wrong: %d", o.LeaseSeconds)
	}
	if len(o.DynamicSlices) != 2 {
		t.Fatalf("expected 2 dynamic slices, got %d: %+v", len(o.DynamicSlices), o.DynamicSlices)
	}
	// Slices carry stripped addrs + owner.
	byOwner := map[string]model.OOBDynamicSlice{}
	for _, s := range o.DynamicSlices {
		byOwner[s.Owner] = s
	}
	if s := byOwner["10.254.240.11"]; s.Start != "10.254.241.100" || s.End != "10.254.241.149" {
		t.Fatalf("gw-1 slice wrong: %+v", s)
	}
	if s := byOwner["10.254.240.12"]; s.Start != "10.254.241.150" || s.End != "10.254.241.199" {
		t.Fatalf("gw-2 slice wrong: %+v", s)
	}
}

// TestBuild_OOBRangeMissingOwnerErrors: a dynamic range without the owner CF
// is a data error (the worker couldn't tell whose slice it is).
func TestBuild_OOBRangeMissingOwnerErrors(t *testing.T) {
	in := Inputs{
		Site:        netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:      model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes:    []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		OOBPrefixes: []netbox.Prefix{{Prefix: "10.254.240.0/22", CustomFields: gatewayJSON("10.254.243.253")}},
		OOBRanges: []netbox.IPRange{
			{StartAddress: "10.254.241.100/22", EndAddress: "10.254.241.149/22"}, // no owner CF
		},
	}
	if _, err := Build(in); err == nil {
		t.Fatal("expected error on OOB range missing owner CF, got nil")
	}
}

// TestBuild_OOBDuplicateOwnerErrors: two ranges claiming the same owner is
// ambiguous.
func TestBuild_OOBDuplicateOwnerErrors(t *testing.T) {
	dup := json.RawMessage(`{"slagworks_oob_owner":"10.254.240.11"}`)
	in := Inputs{
		Site:        netbox.Site{Name: "pop1", Slug: "pop1"},
		Config:      model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes:    []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
		OOBPrefixes: []netbox.Prefix{{Prefix: "10.254.240.0/22", CustomFields: gatewayJSON("10.254.243.253")}},
		OOBRanges: []netbox.IPRange{
			{StartAddress: "10.254.241.100/22", EndAddress: "10.254.241.149/22", CustomFields: dup},
			{StartAddress: "10.254.241.150/22", EndAddress: "10.254.241.199/22", CustomFields: dup},
		},
	}
	if _, err := Build(in); err == nil {
		t.Fatal("expected error on duplicate slice owner, got nil")
	}
}

// TestBuild_NoOOBPrefixNoOOBConfig: pre-cutover, snap.OOB is the zero value.
func TestBuild_NoOOBPrefixNoOOBConfig(t *testing.T) {
	in := Inputs{
		Site:     netbox.Site{Name: "pop3", Slug: "pop3"},
		Config:   model.SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
		Prefixes: []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
	}
	snap, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if snap.OOB.Subnet != "" {
		t.Fatalf("expected zero OOBConfig pre-cutover, got %+v", snap.OOB)
	}
}

// ipsFor builds a []netbox.IPAddress from {ifaceID: cidr} pairs, mirroring
// the dispatcher's ListIPAddressesForInterfaces output. Each entry is one
// IPAddress row assigned to a dcim.interface.
func ipsFor(pairs ...struct {
	ifaceID int
	cidr    string
}) []netbox.IPAddress {
	out := make([]netbox.IPAddress, 0, len(pairs))
	for _, p := range pairs {
		id := p.ifaceID
		out = append(out, netbox.IPAddress{
			Address:            p.cidr,
			AssignedObjectID:   &id,
			AssignedObjectType: "dcim.interface",
		})
	}
	return out
}

func ipPair(ifaceID int, cidr string) struct {
	ifaceID int
	cidr    string
} {
	return struct {
		ifaceID int
		cidr    string
	}{ifaceID, cidr}
}

// TestBuildOOBReservations_HappyPath covers the core join: a mgmt_only BMC
// interface and a non-mgmt host interface, both with OOB IPs, both emitted —
// mgmt_only is INCLUDED (the whole point of the OOB reservation set). An
// interface with an IP outside the OOB prefix is skipped, and one with no
// MAC is skipped.
func TestBuildOOBReservations_HappyPath(t *testing.T) {
	ifaces := []netbox.Interface{
		{ID: 1, Name: "ipmi0", MACAddress: "AA:BB:CC:00:00:01", MgmtOnly: true},
		{ID: 2, Name: "eno3", MACAddress: "AA:BB:CC:00:00:02"},
		{ID: 3, Name: "fabric", MACAddress: "AA:BB:CC:00:00:03"}, // IP outside OOB
		{ID: 4, Name: "nomac"}, // no MAC -> skipped
	}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(2, "10.254.240.11/22"),
		ipPair(3, "10.254.26.5/24"), // not in OOB prefix
		ipPair(4, "10.254.240.99/22"),
	)

	got, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{
		"aa:bb:cc:00:00:01": "10.254.240.50",
		"aa:bb:cc:00:00:02": "10.254.240.11",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d reservations, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		if want[r.MAC] != r.IP {
			t.Errorf("reservation %s -> %s, want %s", r.MAC, r.IP, want[r.MAC])
		}
	}
}

// TestBuildOOBReservations_NoPrefixes: pre-cutover sites have no slagworks-oob
// prefix; the function returns nil and emits nothing even if OOB-range IPs
// happen to exist on interfaces.
func TestBuildOOBReservations_NoPrefixes(t *testing.T) {
	ifaces := []netbox.Interface{{ID: 1, MACAddress: "AA:BB:CC:00:00:01"}}
	ips := ipsFor(ipPair(1, "10.254.240.50/22"))
	got, err := buildOOBReservations(nil, ifaces, ips)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil reservations with no OOB prefixes, got %+v", got)
	}
}

// TestBuildOOBReservations_PrimaryMACFallback: NetBox 4.x stores the MAC as a
// primary_mac_address object, not inline. GetMAC must resolve it.
func TestBuildOOBReservations_PrimaryMACFallback(t *testing.T) {
	ifaces := []netbox.Interface{{
		ID:         1,
		Name:       "ipmi0",
		MgmtOnly:   true,
		PrimaryMAC: &netbox.MACAddressRef{MACAddress: "DE:AD:BE:EF:00:01"},
	}}
	ips := ipsFor(ipPair(1, "10.254.241.9/22"))
	got, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].MAC != "de:ad:be:ef:00:01" || got[0].IP != "10.254.241.9" {
		t.Fatalf("primary-MAC fallback wrong: %+v", got)
	}
}

// TestBuildOOBReservations_MultipleOOBIPsOnOneIface: more than one OOB IP on
// a single interface is a NetBox data error; Build must fail loudly rather
// than pick one (the bug roast caught: last-write-wins would silently drop
// all but one).
func TestBuildOOBReservations_MultipleOOBIPsOnOneIface(t *testing.T) {
	ifaces := []netbox.Interface{{ID: 1, Name: "ipmi0", MACAddress: "AA:BB:CC:00:00:01", MgmtOnly: true}}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(1, "10.254.240.51/22"), // second OOB IP on the same interface
	)
	_, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err == nil {
		t.Fatal("expected error on >1 OOB IP per interface, got nil")
	}
}

// TestBuildOOBReservations_SecondNonOOBIPOnIfaceOK: an interface with one OOB
// IP and one NON-OOB IP (e.g. a fabric address) is fine — only OOB IPs count
// toward the per-interface limit.
func TestBuildOOBReservations_SecondNonOOBIPOnIfaceOK(t *testing.T) {
	ifaces := []netbox.Interface{{ID: 1, Name: "eno3", MACAddress: "AA:BB:CC:00:00:01"}}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(1, "10.254.26.5/24"), // non-OOB, ignored
	)
	got, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].IP != "10.254.240.50" {
		t.Fatalf("expected single OOB reservation, got %+v", got)
	}
}

// TestBuildOOBReservations_DupMACDifferentIP: the same MAC mapped to two OOB
// IPs across interfaces is a NetBox data error; Build must fail loudly.
func TestBuildOOBReservations_DupMACDifferentIP(t *testing.T) {
	ifaces := []netbox.Interface{
		{ID: 1, MACAddress: "AA:BB:CC:00:00:01"},
		{ID: 2, MACAddress: "AA:BB:CC:00:00:01"}, // same MAC
	}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(2, "10.254.240.51/22"), // different IP
	)
	_, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err == nil {
		t.Fatal("expected error on duplicate MAC with different IPs, got nil")
	}
}

// TestBuildOOBReservations_DupIPDifferentMAC: the same IP claimed by two MACs
// is a NetBox data error; Build must fail loudly.
func TestBuildOOBReservations_DupIPDifferentMAC(t *testing.T) {
	ifaces := []netbox.Interface{
		{ID: 1, MACAddress: "AA:BB:CC:00:00:01"},
		{ID: 2, MACAddress: "AA:BB:CC:00:00:02"},
	}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(2, "10.254.240.50/22"), // same IP, different MAC
	)
	_, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err == nil {
		t.Fatal("expected error on duplicate IP with different MACs, got nil")
	}
}

// TestBuildOOBReservations_IdenticalDupIsNoError: the exact same (mac, ip)
// across two interfaces (e.g. NetBox returning a dupe) is collapsed, not an
// error.
func TestBuildOOBReservations_IdenticalDupIsNoError(t *testing.T) {
	ifaces := []netbox.Interface{
		{ID: 1, MACAddress: "AA:BB:CC:00:00:01"},
		{ID: 2, MACAddress: "AA:BB:CC:00:00:01"},
	}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(2, "10.254.240.50/22"),
	)
	got, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("identical dup should not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("identical dup should collapse to 1, got %d: %+v", len(got), got)
	}
}
