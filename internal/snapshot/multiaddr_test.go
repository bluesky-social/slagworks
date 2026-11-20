package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// multiAddrInputs is a one-device site for the multi-address tests.
func multiAddrInputs(dev netbox.Device, machines []model.Machine, ifaces []netbox.Interface, ips ...netbox.IPAddress) Inputs {
	return Inputs{
		Site:       netbox.Site{Name: "site1", Slug: "site1"},
		Config:     model.SiteConfig{Slug: "site1"},
		Prefixes:   []netbox.Prefix{{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")}},
		Devices:    []netbox.Device{dev},
		Interfaces: ifaces,
		IPs:        ips,
		Machines:   machines,
	}
}

func ifaceIP(id int, addr string, iface int) netbox.IPAddress {
	return netbox.IPAddress{ID: id, Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &iface}
}

// TestBuildMultiAddressInterfaceIsOrderIndependent: a machine's network facts
// carry one address per interface (SnapshotInterface.IP). When a bound
// machine's NetBox interface had several status=active addresses, Build
// first kept whichever NetBox listed last, then refused the whole site. It
// must instead build, and choose the same address whatever order NetBox
// lists them in: the numerically lowest IPv4 address when the device's
// primary_ip4 is not among them.
func TestBuildMultiAddressInterfaceIsOrderIndependent(t *testing.T) {
	devID := 7
	dev := netbox.Device{ID: devID, Name: "host-1"}
	bound := []model.Machine{{ID: uuid.MustParse("00000000-0000-0000-0000-000000000007"), NetBoxDeviceID: &devID}}
	ifaces := []netbox.Interface{{
		ID: 70, Name: "bond0", Type: &netbox.NestedValue{Value: "lag"},
		Device: &netbox.NestedRef{ID: devID},
	}}
	// 10.253.0.99 sorts after 10.253.0.100 as a string and before it as an
	// address; the IPv6 address sorts first as a string.
	ips := []netbox.IPAddress{
		ifaceIP(1, "10.253.0.100/22", 70),
		ifaceIP(2, "10.253.0.99/22", 70),
		ifaceIP(3, "10.253.1.7/22", 70),
		ifaceIP(4, "2001:db8::5/64", 70),
	}
	const want = "10.253.0.99/22"
	rng := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		rng.Shuffle(len(ips), func(i, j int) { ips[i], ips[j] = ips[j], ips[i] })
		snap, err := Build(multiAddrInputs(dev, bound, ifaces, ips...))
		if err != nil {
			t.Fatalf("Build failed the site for a multi-address interface (order %v): %v", ips, err)
		}
		if got := snap.Machines[0].Interfaces[0]; got.IP != want || got.Gateway != "10.253.3.254" {
			t.Fatalf("bond0 facts = %+v for order %v, want ip %s with gateway 10.253.3.254", got, ips, want)
		}
	}

	// The same interface on a device no machine is bound to reaches no
	// machine's facts, and builds too.
	if _, err := Build(multiAddrInputs(dev, nil, ifaces, ips...)); err != nil {
		t.Errorf("unbound device's multi-address interface failed the site build: %v", err)
	}

	// A non-active address is never chosen, however low.
	retired := ifaceIP(5, "10.253.0.1/22", 70)
	retired.Status = &netbox.NestedValue{Value: "reserved"}
	snap, err := Build(multiAddrInputs(dev, bound, ifaces, append([]netbox.IPAddress{retired}, ips...)...))
	if err != nil {
		t.Fatalf("Build with a reserved address: %v", err)
	}
	if got := snap.Machines[0].Interfaces[0].IP; got != want {
		t.Errorf("bond0 ip = %s, want %s (reserved address must not be chosen)", got, want)
	}
}

// TestBuildMultiAddressInterfaceRule pins the choice: the device's
// primary_ip4 when it is one of the interface's active addresses, else the
// numerically lowest active IPv4 address (ties by prefix length, then NetBox
// ID), else, on an interface with no IPv4 address, the lowest IPv6 address.
// Within each family an address with no NetBox role (or role loopback) comes
// before one with any other role.
func TestBuildMultiAddressInterfaceRule(t *testing.T) {
	devID := 7
	bound := []model.Machine{{ID: uuid.MustParse("00000000-0000-0000-0000-000000000007"), NetBoxDeviceID: &devID}}
	ifaces := []netbox.Interface{{ID: 70, Name: "eno1", Device: &netbox.NestedRef{ID: devID}}}
	reserved := func(ip netbox.IPAddress) netbox.IPAddress {
		ip.Status = &netbox.NestedValue{Value: "reserved"}
		return ip
	}
	role := func(r string, ip netbox.IPAddress) netbox.IPAddress {
		ip.Role = &netbox.NestedValue{Value: r}
		return ip
	}
	for _, tc := range []struct {
		name    string
		primary int // primary_ip4 NetBox ID, 0 for none
		ips     []netbox.IPAddress
		want    string
	}{
		{"primary wins over a lower address", 2,
			[]netbox.IPAddress{ifaceIP(1, "10.253.0.5/22", 70), ifaceIP(2, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"primary elsewhere: lowest IPv4", 99,
			[]netbox.IPAddress{ifaceIP(1, "10.253.0.20/22", 70), ifaceIP(2, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"reserved primary is not chosen", 2,
			[]netbox.IPAddress{ifaceIP(1, "10.253.0.20/22", 70), reserved(ifaceIP(2, "10.253.0.2/22", 70)), ifaceIP(3, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"IPv4 before a lower-sorting IPv6", 0,
			[]netbox.IPAddress{ifaceIP(1, "2001:db8::1/64", 70), ifaceIP(2, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"IPv6 only: lowest IPv6", 0,
			[]netbox.IPAddress{ifaceIP(1, "2001:db8::10/64", 70), ifaceIP(2, "2001:db8::9/64", 70)}, "2001:db8::9/64"},
		{"same address: shorter prefix first", 0,
			[]netbox.IPAddress{ifaceIP(1, "10.253.0.9/32", 70), ifaceIP(2, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"numeric, not string, order", 0,
			[]netbox.IPAddress{ifaceIP(1, "10.253.0.10/22", 70), ifaceIP(2, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"unparseable address is never preferred", 0,
			[]netbox.IPAddress{ifaceIP(1, "!bogus", 70), ifaceIP(2, "10.253.0.9/22", 70)}, "10.253.0.9/22"},
		{"own address before a lower VIP", 0,
			[]netbox.IPAddress{role("vip", ifaceIP(1, "10.0.0.5/24", 70)), ifaceIP(2, "10.0.0.9/24", 70)}, "10.0.0.9/24"},
		{"own address before a lower secondary", 0,
			[]netbox.IPAddress{role("secondary", ifaceIP(1, "10.0.0.5/24", 70)), ifaceIP(2, "10.0.0.9/24", 70)}, "10.0.0.9/24"},
		{"loopback role is the interface's own", 0,
			[]netbox.IPAddress{role("anycast", ifaceIP(1, "10.0.6.1/32", 70)), role("loopback", ifaceIP(2, "10.0.31.1/32", 70))}, "10.0.31.1/32"},
		{"primary wins even with a shared role", 1,
			[]netbox.IPAddress{role("vrrp", ifaceIP(1, "10.0.0.20/24", 70)), ifaceIP(2, "10.0.0.9/24", 70)}, "10.0.0.20/24"},
		{"only shared addresses: lowest of them", 0,
			[]netbox.IPAddress{role("carp", ifaceIP(1, "10.0.0.20/24", 70)), role("hsrp", ifaceIP(2, "10.0.0.9/24", 70))}, "10.0.0.9/24"},
		{"shared IPv4 before own IPv6", 0,
			[]netbox.IPAddress{role("glbp", ifaceIP(1, "10.0.0.20/24", 70)), ifaceIP(2, "2001:db8::9/64", 70)}, "10.0.0.20/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := netbox.Device{ID: devID, Name: "host-1"}
			if tc.primary != 0 {
				dev.PrimaryIP4 = &netbox.NestedRef{ID: tc.primary}
			}
			rev := slices.Clone(tc.ips)
			slices.Reverse(rev)
			for _, ips := range [][]netbox.IPAddress{tc.ips, rev} {
				snap, err := Build(multiAddrInputs(dev, bound, ifaces, ips...))
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				if got := snap.Machines[0].Interfaces[0].IP; got != tc.want {
					t.Errorf("ip = %s, want %s (order %v)", got, tc.want, ips)
				}
			}
		})
	}
}

// TestBuildPrefersAnInterfacesOwnAddressOverASharedRoleAddress: NetBox marks
// an address shared between hosts with a role (anycast, vip, vrrp, hsrp,
// glbp, carp) and an extra address next to the interface's main one with
// role secondary. Neither is the interface's own address: when an interface
// carries one next to an address with no role, the facts must carry the
// role-less one, however the two compare numerically.
//
// The shipped L3 network scripts take "a /32 on the lo interface" as the
// host's identity loopback and render it as the BGP router-id and the
// announced prefix. A router's lo with an anycast /32 that sorts below its
// identity /32, and primary_ip4 on another interface, must still hand the
// scripts the identity /32.
func TestBuildPrefersAnInterfacesOwnAddressOverASharedRoleAddress(t *testing.T) {
	// Decoded from NetBox's JSON, as the client decodes it: role is a
	// {value,label} choice, null when unset.
	var ips []netbox.IPAddress
	if err := json.Unmarshal([]byte(`[
		{"id": 1, "address": "10.0.6.230/32", "status": {"value": "active"}, "role": {"value": "anycast", "label": "Anycast"},
		 "assigned_object_type": "dcim.interface", "assigned_object_id": 50},
		{"id": 2, "address": "10.0.31.1/32", "status": {"value": "active"}, "role": null,
		 "assigned_object_type": "dcim.interface", "assigned_object_id": 50},
		{"id": 3, "address": "10.0.12.20/23", "status": {"value": "active"}, "role": null,
		 "assigned_object_type": "dcim.interface", "assigned_object_id": 51}
	]`), &ips); err != nil {
		t.Fatal(err)
	}
	dev := 10
	snap, err := Build(Inputs{
		Site:     netbox.Site{ID: 1, Slug: "site-a"},
		Config:   model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a"},
		Prefixes: []netbox.Prefix{{ID: 1, Prefix: "10.0.4.0/22", CustomFields: gatewayJSON("10.0.7.254")}},
		// primary_ip4 is the management address, not on lo.
		Devices: []netbox.Device{{ID: dev, Name: "router-1.site-a", PrimaryIP4: &netbox.NestedRef{ID: 3}}},
		Interfaces: []netbox.Interface{
			{ID: 50, Name: "lo", Type: &netbox.NestedValue{Value: "virtual"}, Device: &netbox.NestedRef{ID: dev}},
			{ID: 51, Name: "eno1", MACAddress: "02:00:00:00:00:01", Device: &netbox.NestedRef{ID: dev}},
		},
		IPs:      ips,
		Machines: []model.Machine{{ID: uuid.MustParse("00000000-0000-0000-0000-0000000000aa"), NetBoxDeviceID: &dev, NetBoxSerial: "SN1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, ifc := range snap.Machines[0].Interfaces {
		if ifc.Name == "lo" {
			seen = true
			if ifc.IP != "10.0.31.1/32" || ifc.Gateway != "" {
				t.Fatalf("lo facts carry %s (gateway %q), want the host's own 10.0.31.1/32 with no gateway: an anycast-role address was chosen as the interface's address", ifc.IP, ifc.Gateway)
			}
		}
	}
	if !seen {
		t.Fatal("no lo interface in the machine's facts")
	}
}

// multiAddrProductionInputs models the two shapes that failed whole sites.
// A storage host carries ten active addresses on its data interface, the
// device's primary_ip4 being the lowest of them; a gateway carries two
// active addresses on one NIC while its primary_ip4 sits on another
// interface.
func multiAddrProductionInputs() Inputs {
	storageID, gwID := 7, 8
	in := Inputs{
		Site:     netbox.Site{Name: "site1", Slug: "site1"},
		Config:   model.SiteConfig{Slug: "site1"},
		Prefixes: []netbox.Prefix{{Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")}},
		Devices: []netbox.Device{
			{ID: storageID, Name: "storage-1", PrimaryIP4: &netbox.NestedRef{ID: 100}},
			{ID: gwID, Name: "gateway-1", PrimaryIP4: &netbox.NestedRef{ID: 200}},
		},
		Interfaces: []netbox.Interface{
			{ID: 70, Name: "data0", Device: &netbox.NestedRef{ID: storageID}},
			{ID: 80, Name: "eno1", Device: &netbox.NestedRef{ID: gwID}},
			{ID: 81, Name: "eno2", Device: &netbox.NestedRef{ID: gwID}},
		},
		Machines: []model.Machine{
			{ID: uuid.MustParse("00000000-0000-0000-0000-000000000007"), NetBoxDeviceID: &storageID},
			{ID: uuid.MustParse("00000000-0000-0000-0000-000000000008"), NetBoxDeviceID: &gwID},
		},
	}
	// Ten addresses on data0, listed highest first; the primary (ID 100) is
	// 10.253.1.10, the lowest.
	for i := 9; i >= 0; i-- {
		in.IPs = append(in.IPs, ifaceIP(100+i, fmt.Sprintf("10.253.1.%d/22", 10+i), 70))
	}
	in.IPs = append(in.IPs,
		ifaceIP(201, "10.253.2.21/22", 81),
		ifaceIP(200, "10.253.2.5/22", 80), // the gateway's primary, on eno1
		ifaceIP(202, "10.253.2.20/22", 81),
	)
	return in
}

// TestBuildMultiAddressProductionShapes: both production shapes must build,
// with a known address on each interface.
func TestBuildMultiAddressProductionShapes(t *testing.T) {
	snap, err := Build(multiAddrProductionInputs())
	if err != nil {
		t.Fatalf("Build failed the site: %v", err)
	}
	got := map[string]string{}
	for _, m := range snap.Machines {
		for _, i := range m.Interfaces {
			got[m.Hostname+"/"+i.Name] = i.IP
		}
	}
	want := map[string]string{
		"storage-1/data0": "10.253.1.10/22",
		"gateway-1/eno1":  "10.253.2.5/22",
		"gateway-1/eno2":  "10.253.2.20/22",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("interface addresses = %v, want %v", got, want)
	}
}

// TestBuildReportsAddressChoices: buildWithChoices names each multi-address
// interface once, with the address chosen, and leaves single-address
// interfaces out.
func TestBuildReportsAddressChoices(t *testing.T) {
	_, choices, err := buildWithChoices(multiAddrProductionInputs())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []AddressChoice{
		{Device: "storage-1", Interface: "data0", InterfaceID: 70, Chosen: "10.253.1.10/22", Active: 10},
		{Device: "gateway-1", Interface: "eno2", InterfaceID: 81, Chosen: "10.253.2.20/22", Active: 2},
	}
	if !reflect.DeepEqual(choices, want) {
		t.Fatalf("choices = %+v, want %+v", choices, want)
	}
}

// warnCounter counts WARN records whose message mentions multi-address
// interfaces.
type warnCounter struct {
	slog.Handler
	n *atomic.Int64
}

func (h warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn && strings.Contains(r.Message, "more than one active address") {
		h.n.Add(1)
	}
	return nil
}

func (h warnCounter) Enabled(context.Context, slog.Level) bool { return true }

// TestDispatcherReportsAddressChoicesOncePerChange: a site with
// multi-address interfaces rebuilds successfully every time, and the
// dispatcher warns about them once, then again only when the list changes;
// neither a repeated rebuild nor a different NetBox list order repeats it.
func TestDispatcherReportsAddressChoicesOncePerChange(t *testing.T) {
	var warns atomic.Int64
	old := slog.Default()
	slog.SetDefault(slog.New(warnCounter{slog.DiscardHandler, &warns}))
	defer slog.SetDefault(old)

	d, sink, fs, nb := dispatcherFixture(t)
	in := multiAddrProductionInputs()
	nb.devices, nb.ifaces, nb.ips = in.Devices, in.Interfaces, in.IPs
	fs.machines = in.Machines

	ctx := context.Background()
	for gen := int64(1); gen <= 3; gen++ {
		if _, err := d.rebuild(ctx, "pop3", gen); err != nil {
			t.Fatalf("rebuild %d failed the site: %v", gen, err)
		}
	}
	if _, err := d.BuildSnapshot(ctx, "pop3"); err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	slices.Reverse(nb.ips)
	if _, err := d.rebuild(ctx, "pop3", 4); err != nil {
		t.Fatalf("rebuild after reorder: %v", err)
	}
	if got := warns.Load(); got != 1 {
		t.Fatalf("%d warnings over five builds of unchanged NetBox data, want 1", got)
	}
	if got := sink.Count(); got != 1 {
		t.Fatalf("%d pushes over rebuilds of unchanged NetBox data, want 1 (list order must not change the snapshot)", got)
	}

	// The gateway's lower eno2 address is retired: the choice changes, so
	// the warning repeats once.
	for i := range nb.ips {
		if nb.ips[i].ID == 202 {
			nb.ips[i].Status = &netbox.NestedValue{Value: "deprecated"}
		}
	}
	for gen := int64(5); gen <= 6; gen++ {
		if _, err := d.rebuild(ctx, "pop3", gen); err != nil {
			t.Fatalf("rebuild %d: %v", gen, err)
		}
	}
	if got := warns.Load(); got != 2 {
		t.Fatalf("%d warnings after the list changed once, want 2", got)
	}

	// No multi-address interfaces left, then back: reported again.
	saved := nb.ips
	nb.ips = nil
	if _, err := d.rebuild(ctx, "pop3", 7); err != nil {
		t.Fatal(err)
	}
	nb.ips = saved
	if _, err := d.rebuild(ctx, "pop3", 8); err != nil {
		t.Fatal(err)
	}
	if got := warns.Load(); got != 3 {
		t.Fatalf("%d warnings after the list emptied and returned, want 3", got)
	}
}
