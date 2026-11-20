package snapshot

import (
	"encoding/json"
	"slices"
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

// oobConflictInputs is a site with an OOB prefix, an OOB gateway's NIC
// (interface 100, MAC ...:01, its own address 10.0.12.11) and an unrelated
// BMC (interface 101, MAC ...:02, 10.0.12.20), plus extra interfaces and
// active address rows.
func oobConflictInputs(extraIfaces []netbox.Interface, extraIPs []netbox.IPAddress) Inputs {
	return Inputs{
		Site:   netbox.Site{ID: 1, Slug: "site-a", Name: "Site A"},
		Config: model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a", EphemeralPoolStart: "10.0.7.100", EphemeralPoolEnd: "10.0.7.160"},
		Prefixes: []netbox.Prefix{
			{ID: 1, Prefix: "10.0.4.0/22", CustomFields: gatewayJSON("10.0.7.254")},
		},
		OOBPrefixes: []netbox.Prefix{
			{ID: 2, Prefix: "10.0.12.0/23", CustomFields: gatewayJSON("10.0.13.254")},
		},
		Devices: []netbox.Device{{ID: 10, Name: "oobgw-1.site-a"}, {ID: 11, Name: "node-1.site-a"}},
		Interfaces: append([]netbox.Interface{
			{ID: 100, Name: "eno3", MACAddress: "02:00:00:00:00:01", Device: &netbox.NestedRef{ID: 10, Name: "oobgw-1.site-a"}},
			{ID: 101, Name: "ipmi", MACAddress: "02:00:00:00:00:02", MgmtOnly: true, Device: &netbox.NestedRef{ID: 11, Name: "node-1.site-a"}},
		}, extraIfaces...),
		IPs: append([]netbox.IPAddress{
			activeOOBIP(1000, 100, "10.0.12.11/23"), // the gateway's own OOB address
			activeOOBIP(1002, 101, "10.0.12.20/23"), // an unrelated BMC
		}, extraIPs...),
	}
}

func activeOOBIP(id, iface int, addr string) netbox.IPAddress {
	return netbox.IPAddress{ID: id, Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &iface,
		Status: &netbox.NestedValue{Value: "active"}}
}

// oobConflictCases are single NetBox saves that make one MAC's OOB
// reservation ambiguous, applied to oobConflictInputs. Each was a site-fatal
// Build error before ambiguous MACs were left out.
var oobConflictCases = []struct {
	name       string
	ifaces     []netbox.Interface
	ips        []netbox.IPAddress
	wantKept   []model.Reservation // must be in the snapshot
	wantAbsent []string            // MACs that must have no reservation
	wantReport []string            // the left-out lines, in order
}{
	{
		// The OOB gateway (VIP) address modelled on the gateway's NIC next
		// to its own address.
		name:       "second OOB address on one interface",
		ips:        []netbox.IPAddress{activeOOBIP(1001, 100, "10.0.13.254/23")},
		wantKept:   []model.Reservation{{MAC: "02:00:00:00:00:02", IP: "10.0.12.20"}},
		wantAbsent: []string{"02:00:00:00:00:01"},
		wantReport: []string{"02:00:00:00:00:01 on oobgw-1.site-a eno3 (NetBox interface 100): 2 OOB addresses (10.0.12.11, 10.0.13.254)"},
	},
	{
		// Two address objects with the same address on one interface:
		// nothing is ambiguous, the interface has one OOB address.
		name: "the same OOB address modelled twice on one interface",
		ips:  []netbox.IPAddress{activeOOBIP(1001, 100, "10.0.12.11/23")},
		wantKept: []model.Reservation{
			{MAC: "02:00:00:00:00:01", IP: "10.0.12.11"},
			{MAC: "02:00:00:00:00:02", IP: "10.0.12.20"},
		},
	},
	{
		// A VLAN child of the gateway's NIC shares its MAC and gets an OOB
		// address of its own.
		name: "two interfaces with one MAC and two OOB addresses",
		ifaces: []netbox.Interface{
			{ID: 102, Name: "eno3.100", MACAddress: "02:00:00:00:00:01", Device: &netbox.NestedRef{ID: 10, Name: "oobgw-1.site-a"}},
		},
		ips:        []netbox.IPAddress{activeOOBIP(1001, 102, "10.0.12.12/23")},
		wantKept:   []model.Reservation{{MAC: "02:00:00:00:00:02", IP: "10.0.12.20"}},
		wantAbsent: []string{"02:00:00:00:00:01"},
		wantReport: []string{"02:00:00:00:00:01 on oobgw-1.site-a eno3 (NetBox interface 100), oobgw-1.site-a eno3.100 (NetBox interface 102): 2 OOB addresses (10.0.12.11, 10.0.12.12)"},
	},
	{
		// The VIP modelled once on each of two gateways.
		name: "one OOB address on two MACs",
		ifaces: []netbox.Interface{
			{ID: 103, Name: "eno3", MACAddress: "02:00:00:00:00:03", Device: &netbox.NestedRef{ID: 12, Name: "oobgw-2.site-a"}},
			{ID: 104, Name: "eno3", MACAddress: "02:00:00:00:00:04", Device: &netbox.NestedRef{ID: 13, Name: "oobgw-3.site-a"}},
		},
		ips: []netbox.IPAddress{activeOOBIP(1003, 103, "10.0.13.254/23"), activeOOBIP(1004, 104, "10.0.13.254/23")},
		wantKept: []model.Reservation{
			{MAC: "02:00:00:00:00:01", IP: "10.0.12.11"},
			{MAC: "02:00:00:00:00:02", IP: "10.0.12.20"},
		},
		wantAbsent: []string{"02:00:00:00:00:03", "02:00:00:00:00:04"},
		wantReport: []string{
			"02:00:00:00:00:03 on oobgw-2.site-a eno3 (NetBox interface 103): OOB address 10.0.13.254 is also on 02:00:00:00:00:04",
			"02:00:00:00:00:04 on oobgw-3.site-a eno3 (NetBox interface 104): OOB address 10.0.13.254 is also on 02:00:00:00:00:03",
		},
	},
}

// TestBuildOOBReservationConflictCostsOneInterfaceNotTheSite: a MAC whose
// OOB reservation NetBox makes ambiguous costs that MAC its reservation, not
// the site its snapshot. A config that does not build keeps its worker on
// the last snapshot and makes every commission and deploy preflight in the
// config answer 503, so one NetBox save on one NIC used to stop a whole
// site provisioning.
func TestBuildOOBReservationConflictCostsOneInterfaceNotTheSite(t *testing.T) {
	for _, tc := range oobConflictCases {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := Build(oobConflictInputs(tc.ifaces, tc.ips))
			if err != nil {
				t.Fatalf("one ambiguous OOB reservation failed the whole site build: %v", err)
			}
			got := map[string]string{}
			for _, r := range snap.Reservations {
				got[r.MAC] = r.IP
			}
			for _, want := range tc.wantKept {
				if got[want.MAC] != want.IP {
					t.Errorf("reservation %s -> %s is missing or wrong (got %q); an unrelated interface must keep its reservation", want.MAC, want.IP, got[want.MAC])
				}
			}
			for _, mac := range tc.wantAbsent {
				if ip, ok := got[mac]; ok {
					t.Errorf("MAC %s got reservation %s, but its reservation is ambiguous and must be left out", mac, ip)
				}
			}
			if len(snap.Reservations) != len(tc.wantKept) {
				t.Errorf("reservations = %+v, want exactly %+v", snap.Reservations, tc.wantKept)
			}
			if snap.OOB.Subnet != "10.0.12.0/23" || snap.OOB.Gateway != "10.0.13.254" {
				t.Errorf("OOB segment config changed: %+v", snap.OOB)
			}
		})
	}
}

// TestBuildOOBReservations_ReportsEachLeftOutMACOnce: every left-out MAC is
// named once, with its interfaces and addresses, so the operator can find
// the NetBox save; a MAC that keeps its reservation is not named.
func TestBuildOOBReservations_ReportsEachLeftOutMACOnce(t *testing.T) {
	for _, tc := range oobConflictCases {
		t.Run(tc.name, func(t *testing.T) {
			in := oobConflictInputs(tc.ifaces, tc.ips)
			_, leftOut, err := buildOOBReservations(in.OOBPrefixes, in.Interfaces, in.IPs)
			if err != nil {
				t.Fatalf("buildOOBReservations: %v", err)
			}
			if !slices.Equal(leftOut, tc.wantReport) {
				t.Errorf("left-out report:\n got %q\nwant %q", leftOut, tc.wantReport)
			}
		})
	}
}

// TestBuildOOBRangeWithoutOwnerCostsThatRangeNotTheSite: a dynamic range the
// worker cannot attribute to exactly one gateway (no slagworks_oob_owner, or
// an owner another range also names) is left out and reported; the other
// slices and every reservation are unaffected.
func TestBuildOOBRangeWithoutOwnerCostsThatRangeNotTheSite(t *testing.T) {
	owned := netbox.IPRange{ID: 1, StartAddress: "10.0.13.100/23", EndAddress: "10.0.13.149/23",
		CustomFields: json.RawMessage(`{"slagworks_oob_owner":"10.0.12.11"}`)}
	cases := []struct {
		name       string
		bad        []netbox.IPRange
		wantReport []string
	}{
		{
			name: "owner null",
			bad: []netbox.IPRange{{ID: 2, StartAddress: "10.0.13.150/23", EndAddress: "10.0.13.199/23",
				CustomFields: json.RawMessage(`{"slagworks_oob_owner":null}`)}},
			wantReport: []string{"10.0.13.150/23-10.0.13.199/23 (NetBox IP range 2): no slagworks_oob_owner"},
		},
		{
			name:       "no custom fields",
			bad:        []netbox.IPRange{{ID: 2, StartAddress: "10.0.13.150/23", EndAddress: "10.0.13.199/23"}},
			wantReport: []string{"10.0.13.150/23-10.0.13.199/23 (NetBox IP range 2): no slagworks_oob_owner"},
		},
		{
			// A range copied from another with its owner: both name the
			// same gateway, so neither can be told apart.
			name: "two ranges name one owner",
			bad: []netbox.IPRange{
				{ID: 2, StartAddress: "10.0.13.150/23", EndAddress: "10.0.13.199/23", CustomFields: json.RawMessage(`{"slagworks_oob_owner":"10.0.12.12"}`)},
				{ID: 3, StartAddress: "10.0.13.200/23", EndAddress: "10.0.13.249/23", CustomFields: json.RawMessage(`{"slagworks_oob_owner":"10.0.12.12"}`)},
			},
			wantReport: []string{
				"10.0.13.150/23-10.0.13.199/23 (NetBox IP range 2): slagworks_oob_owner 10.0.12.12 is also the owner of another range",
				"10.0.13.200/23-10.0.13.249/23 (NetBox IP range 3): slagworks_oob_owner 10.0.12.12 is also the owner of another range",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := oobConflictInputs(nil, nil)
			in.OOBRanges = append([]netbox.IPRange{owned}, tc.bad...)
			snap, err := Build(in)
			if err != nil {
				t.Fatalf("one bad dynamic range failed the whole site build: %v", err)
			}
			want := []model.OOBDynamicSlice{{Start: "10.0.13.100", End: "10.0.13.149", Owner: "10.0.12.11"}}
			if !slices.Equal(snap.OOB.DynamicSlices, want) {
				t.Errorf("dynamic slices = %+v, want only the owned range %+v", snap.OOB.DynamicSlices, want)
			}
			if len(snap.Reservations) != 2 {
				t.Errorf("reservations = %+v, want both interfaces reserved", snap.Reservations)
			}
			_, leftOut, err := buildOOBConfig(in.OOBPrefixes, in.OOBRanges)
			if err != nil {
				t.Fatalf("buildOOBConfig: %v", err)
			}
			if !slices.Equal(leftOut, tc.wantReport) {
				t.Errorf("left-out report:\n got %q\nwant %q", leftOut, tc.wantReport)
			}
		})
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

	got, _, err := buildOOBReservations(oobPrefix(), ifaces, ips)
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
	got, _, err := buildOOBReservations(nil, ifaces, ips)
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
	got, _, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].MAC != "de:ad:be:ef:00:01" || got[0].IP != "10.254.241.9" {
		t.Fatalf("primary-MAC fallback wrong: %+v", got)
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
	got, leftOut, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].IP != "10.254.240.50" || len(leftOut) != 0 {
		t.Fatalf("expected single OOB reservation and nothing left out, got %+v, left out %q", got, leftOut)
	}
}

// TestBuildOOBReservations_IdenticalDupIsNoConflict: the exact same (mac, ip)
// across two interfaces (e.g. NetBox returning a dupe) is collapsed into one
// reservation and not reported.
func TestBuildOOBReservations_IdenticalDupIsNoConflict(t *testing.T) {
	ifaces := []netbox.Interface{
		{ID: 1, MACAddress: "AA:BB:CC:00:00:01"},
		{ID: 2, MACAddress: "AA:BB:CC:00:00:01"},
	}
	ips := ipsFor(
		ipPair(1, "10.254.240.50/22"),
		ipPair(2, "10.254.240.50/22"),
	)
	got, leftOut, err := buildOOBReservations(oobPrefix(), ifaces, ips)
	if err != nil {
		t.Fatalf("identical dup should not error: %v", err)
	}
	if len(got) != 1 || len(leftOut) != 0 {
		t.Fatalf("identical dup should collapse to 1 and not be reported, got %+v, left out %q", got, leftOut)
	}
}

// TestBuildOOBReservations_UnparseablePrefixErrors: the OOB prefix is the
// site's one OOB segment, so a prefix that does not parse is still an error.
func TestBuildOOBReservations_UnparseablePrefixErrors(t *testing.T) {
	ifaces := []netbox.Interface{{ID: 1, MACAddress: "AA:BB:CC:00:00:01"}}
	ips := ipsFor(ipPair(1, "10.254.240.50/22"))
	if _, _, err := buildOOBReservations([]netbox.Prefix{{Prefix: "not-a-prefix"}}, ifaces, ips); err == nil {
		t.Fatal("expected error on an unparseable OOB prefix, got nil")
	}
}
