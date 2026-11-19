package snapshot

import (
	"slices"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// ifacePhys builds a physical NIC interface.
func ifacePhys(id int, name, mac string) netbox.Interface {
	return netbox.Interface{
		ID:         id,
		Name:       name,
		MACAddress: mac,
		Type:       &netbox.NestedValue{Value: "25gbase-x-sfp28"},
	}
}

// ifaceLo builds the identity loopback interface.
func ifaceLo(id int) netbox.Interface {
	return netbox.Interface{
		ID:   id,
		Name: "lo",
		Type: &netbox.NestedValue{Value: "virtual"},
	}
}

// TestLeafSideOf31 pins the gateway derivation: flip the last bit of a
// /31 host address. Non-/31s and garbage yield "".
func TestLeafSideOf31(t *testing.T) {
	cases := []struct{ in, want string }{
		{"10.254.29.1/31", "10.254.29.0"},
		{"10.254.29.2/31", "10.254.29.3"},
		{"10.254.28.63/31", "10.254.28.62"},
		{"10.254.26.10/24", ""}, // not a /31
		{"garbage", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := leafSideOf31(tc.in); got != tc.want {
			t.Errorf("leafSideOf31(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestL3LegRelayPools pins the per-leg pool generation: every
// /31-addressed physical NIC yields a single-address pool (subnet =
// the /31 network, range = the host side only, gateway = the leaf
// side). Loopbacks, ipmi, and unaddressed NICs yield nothing.
func TestL3LegRelayPools(t *testing.T) {
	ifaces := []netbox.Interface{
		ifacePhys(1, "eth0", "7c:c2:55:be:f6:0c"),
		ifacePhys(2, "eth1", "7c:c2:55:be:f6:0d"),
		ifaceLo(3),
		{ID: 4, Name: "ipmi0", MACAddress: "90:5a:08:15:6c:f2", MgmtOnly: true, Type: &netbox.NestedValue{Value: "1000base-t"}},
	}
	ips := []netbox.IPAddress{
		{Address: "10.254.29.9/31", AssignedObjectID: new(1), AssignedObjectType: "dcim.interface"},
		{Address: "10.254.29.11/31", AssignedObjectID: new(2), AssignedObjectType: "dcim.interface"},
		{Address: "10.254.31.67/32", AssignedObjectID: new(3), AssignedObjectType: "dcim.interface"},
		{Address: "10.254.241.25/22", AssignedObjectID: new(4), AssignedObjectType: "dcim.interface"},
	}
	pools := l3LegRelayPools(ifaces, ips)
	if len(pools) != 2 {
		t.Fatalf("expected 2 pools, got %d: %+v", len(pools), pools)
	}
	for i, want := range []struct{ subnet, start, gw string }{
		{"10.254.29.8/31", "10.254.29.9", "10.254.29.8"},
		{"10.254.29.10/31", "10.254.29.11", "10.254.29.10"},
	} {
		p := pools[i]
		if p.Subnet != want.subnet || p.RangeStart != want.start || p.RangeEnd != want.start || p.Gateway != want.gw {
			t.Errorf("pool[%d] = %+v, want subnet=%s range=%s gw=%s", i, p, want.subnet, want.start, want.gw)
		}
	}
}

// TestL3LegRelayPoolsIgnoreNetBoxOrder pins that the generated pools, and
// so the snapshot content hash, do not depend on the order NetBox lists IP
// addresses in. That order is not stable: the client fetches IPs in chunks
// of interface IDs, so adding an unrelated device can move a record to a
// different response. A different hash for the same data means a spurious
// snapshot push and SeqNum bump.
//
// The records are two host legs plus the leaf end of the first leg (a leaf
// modeled in the same site, its port addressed): every order must give the
// same two pools, in subnet order, with the host as the offered address.
func TestL3LegRelayPoolsIgnoreNetBoxOrder(t *testing.T) {
	ifaces := []netbox.Interface{
		ifacePhys(1, "eth0", "7c:c2:55:be:f6:0c"), // host leg A
		ifacePhys(2, "eth1", "7c:c2:55:be:f6:0d"), // host leg B
		ifacePhys(3, "swp1", "0c:42:a1:00:00:01"), // leaf port facing eth0
	}
	hostA := netbox.IPAddress{Address: "10.254.29.9/31", AssignedObjectID: new(1), AssignedObjectType: "dcim.interface"}
	hostB := netbox.IPAddress{Address: "10.254.29.11/31", AssignedObjectID: new(2), AssignedObjectType: "dcim.interface"}
	leafA := netbox.IPAddress{Address: "10.254.29.8/31", AssignedObjectID: new(3), AssignedObjectType: "dcim.interface"}

	want := []model.RelayPool{
		{Subnet: "10.254.29.8/31", RangeStart: "10.254.29.9", RangeEnd: "10.254.29.9", Gateway: "10.254.29.8"},
		{Subnet: "10.254.29.10/31", RangeStart: "10.254.29.11", RangeEnd: "10.254.29.11", Gateway: "10.254.29.10"},
	}
	orders := []struct {
		name string
		ips  []netbox.IPAddress
	}{
		{"hostA hostB leafA", []netbox.IPAddress{hostA, hostB, leafA}},
		{"hostA leafA hostB", []netbox.IPAddress{hostA, leafA, hostB}},
		{"hostB hostA leafA", []netbox.IPAddress{hostB, hostA, leafA}},
		{"hostB leafA hostA", []netbox.IPAddress{hostB, leafA, hostA}},
		{"leafA hostA hostB", []netbox.IPAddress{leafA, hostA, hostB}},
		{"leafA hostB hostA", []netbox.IPAddress{leafA, hostB, hostA}},
	}

	var wantHash string
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			snap, err := Build(Inputs{
				Site:       netbox.Site{Name: "pop1", Slug: "pop1"},
				Config:     model.SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
				Prefixes:   []netbox.Prefix{{Prefix: "10.0.0.0/24", CustomFields: gatewayJSON("10.0.0.1")}},
				Interfaces: ifaces,
				IPs:        order.ips,
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := snap.DHCP.RelayPools; !slices.Equal(got, want) {
				t.Errorf("relay pools = %+v\nwant %+v", got, want)
			}
			hash, err := snap.ContentHash()
			if err != nil {
				t.Fatalf("ContentHash: %v", err)
			}
			if wantHash == "" {
				wantHash = hash
			} else if hash != wantHash {
				t.Errorf("content hash = %s, want %s (the hash of the first order)", hash, wantHash)
			}
		})
	}
}
