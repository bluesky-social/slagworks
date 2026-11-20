package snapshot

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

// This file generates NetBox worlds shaped like a real deployment and holds
// the independent model the production-shape properties compare builds
// against (shape_prop_test.go).
//
// The shapes come from a counts-only survey of a deployment that has both
// whole-site configs and a site split between location-scoped configs, with
// up to a hundred or so devices and several hundred interfaces per site.
// What the survey found, and so what the generator draws:
//
//   - every scope has a slagworks-oob prefix with one or two dynamic ranges,
//     so the OOB reservation checks run on every build;
//   - a scope has one production prefix, or two on the same location (the
//     generator adds nested and enclosing ones as neighbouring shapes);
//   - 0 to 58 interfaces per device (median 4 to 6); about one device in ten
//     has none;
//   - bonds: a lag interface carrying the host address, with one to three
//     virtual children that share its MAC; lag members without addresses;
//   - a "data" virtual interface with ten addresses in one subnet, none of
//     them the device's primary_ip4;
//   - a router uplink with 5 to 13 addresses in two subnets, and a loopback
//     with two /32s, one of them an anycast address (NetBox role anycast,
//     or no role at all);
//   - an OOB gateway NIC with an OOB address and a second address in another
//     prefix (that prefix's gateway address), primary_ip4 on it;
//   - /31 links with both ends modelled (host and leaf), or one end only;
//   - a switch whose twenty ports share one MAC; MAC objects without number
//     limit per interface (0, 1, 2, 3, 5);
//   - one MAC modelled on about twenty servers per site (the host-visible
//     USB NIC of a BMC, whose factory MAC is the same on every chassis);
//   - mgmt_only interfaces on devices with no oob_ip (BMC-less switches);
//   - devices with no serial (no machine row), no primary_ip4 (one in ten),
//     no oob_ip (one in three), no rack, no position (one in five);
//   - device names with zero, one or two dots, and a few that are not
//     hostnames (spaces, capitals, underscores);
//   - addresses with status active, reserved and deprecated; /22, /23, /24,
//     /29, /30, /31 and /32 lengths; IPv4 only (the generator adds IPv6 as a
//     neighbouring shape);
//   - one console port per switch, cabled to a console-server port on an OOB
//     gateway in the same site, speed set or unset;
//   - device types half with and half without slagworks_kernel_params.
//
// Names, addresses and serials here are invented; none is copied from the
// surveyed deployment.

const (
	shapeTagProd   = "slagworks-production"
	shapeTagOOB    = "slagworks-oob"
	shapeRoleRange = "slagworks-oob-dynamic"
	// shapeSharedMAC stands in for a BMC USB NIC's factory MAC.
	shapeSharedMAC = "02:FF:FF:00:00:01"
)

type shapeKnobs struct {
	// Large draws worlds at the surveyed scale (tens of devices per scope)
	// instead of a handful.
	Large bool
	// MultiProd lets a scope's own location or site carry a second
	// production prefix.
	MultiProd bool
}

// shapeScope is one site_config and the address plan its devices draw from.
type shapeScope struct {
	cfg     model.SiteConfig
	octet   int // second octet of everything this scope numbers
	locID   int // the config's location, 0 for whole-site
	subLocs []int
	// counters for fresh addresses
	nOOB, nProd, nLeg, nLo int
	oobGWs                 []int // device IDs of this scope's OOB gateways
	leafPorts              []int // interface IDs of free leaf ports
}

type shapeGen struct {
	rt     *rapid.T
	k      shapeKnobs
	w      *shapeworld.World
	sd     *shapeworld.StoreData
	next   int
	scopes []*shapeScope
	dtypes []netbox.DeviceType
	// unclaimed devices live in a managed site but outside every config's
	// scope; the model must leave them out.
	unclaimedLoc map[string]int
	// siteGWs lists every OOB gateway of a NetBox site, whichever config
	// claims it: a console server can sit in the other cage.
	siteGWs map[string][]int
	// locIDs is a shuffled pool of location IDs, so a child location can
	// have a lower ID than its parent (as after a re-parenting in NetBox).
	locIDs []int
}

func (g *shapeGen) locID() int {
	id := g.locIDs[0]
	g.locIDs = g.locIDs[1:]
	return id
}

func (g *shapeGen) id() int { g.next++; return g.next }

func (g *shapeGen) mac() string {
	n := g.id()
	return fmt.Sprintf("02:00:00:%02X:%02X:%02X", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

func shapeCF(kv map[string]any) json.RawMessage {
	raw, err := json.Marshal(kv)
	if err != nil {
		panic(err)
	}
	return raw
}

func (sc *shapeScope) prodCIDR() string  { return fmt.Sprintf("10.%d.4.0/22", sc.octet) }
func (sc *shapeScope) prodGW() string    { return fmt.Sprintf("10.%d.7.254", sc.octet) }
func (sc *shapeScope) prod2CIDR() string { return fmt.Sprintf("10.%d.8.0/24", sc.octet) }
func (sc *shapeScope) prod2GW() string   { return fmt.Sprintf("10.%d.8.1", sc.octet) }
func (sc *shapeScope) oobCIDR() string   { return fmt.Sprintf("10.%d.12.0/23", sc.octet) }
func (sc *shapeScope) oobGW() string     { return fmt.Sprintf("10.%d.13.254", sc.octet) }

func (sc *shapeScope) freshOOB() string {
	sc.nOOB++
	return fmt.Sprintf("10.%d.%d.%d/23", sc.octet, 12+sc.nOOB/200, 10+sc.nOOB%200)
}

func (sc *shapeScope) freshProd() string {
	sc.nProd++
	return fmt.Sprintf("10.%d.%d.%d/22", sc.octet, 4+sc.nProd/200, 10+sc.nProd%200)
}

// freshLeg returns the leaf (even) and host (odd) ends of an unused /31.
func (sc *shapeScope) freshLeg() (leaf, host string) {
	n := sc.nLeg
	sc.nLeg++
	return fmt.Sprintf("10.%d.%d.%d/31", sc.octet, 24+n/100, (n%100)*2),
		fmt.Sprintf("10.%d.%d.%d/31", sc.octet, 24+n/100, (n%100)*2+1)
}

func (sc *shapeScope) freshLo() string {
	sc.nLo++
	return fmt.Sprintf("10.%d.31.%d/32", sc.octet, sc.nLo)
}

func (g *shapeGen) addIface(dev int, name, typ, mac string, mgmt bool) int {
	i := netbox.Interface{
		ID: g.id(), Name: name, Enabled: true, MgmtOnly: mgmt,
		Device: &netbox.NestedRef{ID: dev},
		Type:   &netbox.NestedValue{Value: typ},
	}
	if mac != "" {
		// NetBox 4.2+ reports a primary MAC three ways at once. Draw which
		// of them this row carries, so every GetMAC branch is reached.
		switch rapid.IntRange(0, 3).Draw(g.rt, "macForm") {
		case 0:
			i.MACAddress = mac
			i.PrimaryMAC = &netbox.MACAddressRef{ID: g.id(), MACAddress: mac}
			i.MACAddresses = []netbox.MACAddressRef{*i.PrimaryMAC}
		case 1:
			i.MACAddress = mac
		case 2:
			i.PrimaryMAC = &netbox.MACAddressRef{ID: g.id(), MACAddress: mac}
		case 3:
			// MAC objects assigned, none primary: GetMAC falls back to the
			// first.
			i.MACAddresses = []netbox.MACAddressRef{{ID: g.id(), MACAddress: mac}, {ID: g.id(), MACAddress: g.mac()}}
		}
	}
	g.w.Interfaces = append(g.w.Interfaces, i)
	return i.ID
}

func (g *shapeGen) addIP(iface int, addr, status string) int {
	return g.addIPRole(iface, addr, status, "")
}

func (g *shapeGen) addIPRole(iface int, addr, status, role string) int {
	ip := netbox.IPAddress{ID: g.id(), Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &iface}
	if status != "" {
		ip.Status = &netbox.NestedValue{Value: status}
	}
	if role != "" {
		ip.Role = &netbox.NestedValue{Value: role}
	}
	g.w.IPs = append(g.w.IPs, ip)
	return ip.ID
}

func (g *shapeGen) drawStatus() string {
	return rapid.SampledFrom([]string{"active", "active", "active", "active", "active", "active", "reserved", "deprecated", ""}).Draw(g.rt, "status")
}

// addDevice appends a device to a scope's site and returns a pointer to it
// in the world. loc 0 leaves the location unset.
func (g *shapeGen) addDevice(sc *shapeScope, kind string, loc int, serial bool) *netbox.Device {
	site := g.w.Sites[sc.cfg.NetBoxSite]
	n := g.id()
	d := netbox.Device{
		ID:     n,
		Site:   &netbox.NestedRef{ID: site.ID, Slug: site.Slug, Name: site.Name},
		Status: &netbox.NestedValue{Value: "active"},
	}
	switch rapid.IntRange(0, 9).Draw(g.rt, "nameShape") {
	case 0:
		d.Name = fmt.Sprintf("%s-%d.cage.%s", kind, n, site.Slug) // two dots
	case 1:
		d.Name = fmt.Sprintf("%s-%d", kind, n) // no zone
	default:
		d.Name = fmt.Sprintf("%s-%d.%s", kind, n, site.Slug)
	}
	if serial {
		d.Serial = fmt.Sprintf("SN%07d", n)
	}
	if loc != 0 {
		for _, l := range g.w.Locations[site.Slug] {
			if l.ID == loc {
				d.Location = &netbox.NestedRef{ID: l.ID, Slug: l.Slug, Name: l.Name}
			}
		}
	}
	dt := rapid.SampledFrom(g.dtypes).Draw(g.rt, "dtype")
	d.DeviceType = &netbox.NestedRef{ID: dt.ID, Slug: dt.Slug}
	d.Role = &netbox.NestedRef{ID: 1, Slug: kind}
	if rapid.IntRange(0, 9).Draw(g.rt, "rack") > 0 {
		d.Rack = &netbox.NestedRef{ID: 900 + rapid.IntRange(1, 4).Draw(g.rt, "rackN"), Name: "R1"}
		if rapid.IntRange(0, 4).Draw(g.rt, "pos") > 0 {
			p := float64(rapid.IntRange(1, 42).Draw(g.rt, "u"))
			if rapid.IntRange(0, 9).Draw(g.rt, "halfU") == 0 {
				p += 0.5
			}
			d.Position = &p
		}
	}
	g.w.Devices[site.Slug] = append(g.w.Devices[site.Slug], d)
	devs := g.w.Devices[site.Slug]
	return &devs[len(devs)-1]
}

// placeIn draws where in a scope a device sits: the config's own location,
// one of its child locations, or (whole-site only) no location.
func (g *shapeGen) placeIn(sc *shapeScope) int {
	if sc.locID == 0 {
		locs := g.w.Locations[sc.cfg.NetBoxSite]
		if len(locs) == 0 || rapid.IntRange(0, 4).Draw(g.rt, "noLoc") == 0 {
			return 0
		}
		return rapid.SampledFrom(locs).Draw(g.rt, "loc").ID
	}
	if len(sc.subLocs) > 0 && rapid.Bool().Draw(g.rt, "inChild") {
		return rapid.SampledFrom(sc.subLocs).Draw(g.rt, "childLoc")
	}
	return sc.locID
}

// addBMC gives a device a management interface with an OOB address and
// points oob_ip at it.
func (g *shapeGen) addBMC(sc *shapeScope, d *netbox.Device) {
	ifc := g.addIface(d.ID, "ipmi", "1000base-t", g.mac(), true)
	addr := sc.freshOOB()
	id := g.addIP(ifc, addr, "active")
	d.OOBIP = &netbox.NestedIPAddress{ID: id, Address: addr}
}

func (g *shapeGen) addUSBNIC(d *netbox.Device) {
	if rapid.IntRange(0, 2).Draw(g.rt, "usbNIC") == 0 {
		g.addIface(d.ID, "enx02ffff000001", "1000base-t", shapeSharedMAC, false)
	}
}

func (g *shapeGen) setPrimary(d *netbox.Device, ipID int) {
	if rapid.IntRange(0, 9).Draw(g.rt, "hasPrimary") > 0 {
		d.PrimaryIP4 = &netbox.NestedRef{ID: ipID}
	}
}

// legIface adds a routed NIC with the host end of a fresh /31, and usually
// puts the leaf end on a free leaf port so both ends are in scope.
func (g *shapeGen) legIface(sc *shapeScope, d *netbox.Device, name string) {
	ifc := g.addIface(d.ID, name, "25gbase-x-sfp28", g.mac(), false)
	leaf, host := sc.freshLeg()
	g.addIP(ifc, host, g.drawStatus())
	if len(sc.leafPorts) > 0 && rapid.IntRange(0, 4).Draw(g.rt, "leafEnd") > 0 {
		port := sc.leafPorts[0]
		sc.leafPorts = sc.leafPorts[1:]
		g.addIP(port, leaf, "active")
	}
}

func (g *shapeGen) deviceL3(sc *shapeScope) {
	d := g.addDevice(sc, "node", g.placeIn(sc), true)
	g.legIface(sc, d, "enp1s0f0np0")
	g.legIface(sc, d, "enp1s0f1np1")
	lo := g.addIface(d.ID, "lo", "virtual", "", false)
	loAddr := sc.freshLo()
	g.setPrimary(d, g.addIP(lo, loAddr, "active"))
	if rapid.IntRange(0, 5).Draw(g.rt, "v6") == 0 {
		g.addIP(lo, fmt.Sprintf("2001:db8:%d::%d/128", sc.octet, sc.nLo), "active")
	}
	if rapid.IntRange(0, 3).Draw(g.rt, "bmc") > 0 {
		g.addBMC(sc, d)
	}
	g.addUSBNIC(d)
}

func (g *shapeGen) deviceBond(sc *shapeScope, storage bool) {
	kind := "bond"
	if storage {
		kind = "store"
	}
	d := g.addDevice(sc, kind, g.placeIn(sc), true)
	bondMAC := g.mac()
	bond := g.addIface(d.ID, "bond0", "lag", bondMAC, false)
	for i, name := range []string{"enp2s0f0", "enp2s0f1"} {
		mac := bondMAC
		if i == 1 {
			mac = g.mac()
		}
		m := g.addIface(d.ID, name, "25gbase-lr", mac, false)
		mi := shapeIface(g.w, m)
		mi.LAG = &netbox.NestedRef{ID: bond, Name: "bond0"}
		if rapid.Bool().Draw(g.rt, "mtu") {
			mtu := 9000
			mi.MTU = &mtu
		}
	}
	if rapid.Bool().Draw(g.rt, "vlan") {
		shapeIface(g.w, bond).UntaggedVLAN = &netbox.NestedVLAN{ID: 50, VID: rapid.SampledFrom([]int{1, 100, 4000}).Draw(g.rt, "vid")}
	}
	bondIP := g.addIP(bond, sc.freshProd(), "active")
	g.setPrimary(d, bondIP)
	for i := range rapid.IntRange(0, 3).Draw(g.rt, "children") {
		child := g.addIface(d.ID, fmt.Sprintf("vlan%d", 100+i), "virtual", bondMAC, false)
		if rapid.Bool().Draw(g.rt, "childIP") {
			g.addIP(child, fmt.Sprintf("192.168.%d.%d/24", 100+i, 1+d.ID%250), g.drawStatus())
		}
	}
	if storage {
		// The surveyed "data" interface: a run of addresses in one subnet,
		// none of them the device's primary.
		data := g.addIface(d.ID, "data", "virtual", bondMAC, false)
		for range rapid.IntRange(2, 10).Draw(g.rt, "dataAddrs") {
			g.addIP(data, sc.freshProd(), "active")
		}
	}
	if rapid.IntRange(0, 3).Draw(g.rt, "bmc") > 0 {
		g.addBMC(sc, d)
	}
	g.addUSBNIC(d)
}

func (g *shapeGen) deviceRouter(sc *shapeScope) {
	d := g.addDevice(sc, "router", g.placeIn(sc), true)
	up := g.addIface(d.ID, "enp129s0f0np0", "100gbase-lr4", g.mac(), false)
	n := rapid.IntRange(2, 13).Draw(g.rt, "uplinkAddrs")
	for i := range n {
		if i%2 == 0 {
			g.addIP(up, fmt.Sprintf("192.0.2.%d/29", 2+i), "active")
		} else {
			g.addIPRole(up, fmt.Sprintf("198.51.100.%d/24", 16+i), g.drawStatus(),
				rapid.SampledFrom([]string{"", "", "secondary", "vip"}).Draw(g.rt, "uplinkRole"))
		}
	}
	lo := g.addIface(d.ID, "lo", "virtual", "", false)
	// The identity loopback sorts above the shared (anycast) address, as
	// surveyed. NetBox says which is which only when the operator set the
	// role.
	g.addIPRole(lo, sc.freshLo(), "active", rapid.SampledFrom([]string{"", "loopback"}).Draw(g.rt, "loRole"))
	g.addIPRole(lo, fmt.Sprintf("10.%d.6.230/32", sc.octet), "active",
		rapid.SampledFrom([]string{"anycast", "anycast", "anycast", ""}).Draw(g.rt, "anycastRole"))
	mgmt := g.addIface(d.ID, "eno1", "1000base-t", g.mac(), false)
	g.setPrimary(d, g.addIP(mgmt, sc.freshOOB(), "active"))
	g.addBMC(sc, d)
	g.addUSBNIC(d)
}

func (g *shapeGen) deviceOOBGW(sc *shapeScope) {
	d := g.addDevice(sc, "oobgw", g.placeIn(sc), true)
	nic := g.addIface(d.ID, "eno3", "1000base-t", g.mac(), false)
	own := sc.freshOOB()
	g.setPrimary(d, g.addIP(nic, own, "active"))
	if rapid.Bool().Draw(g.rt, "prodGWOnNIC") {
		// The surveyed gateway NIC: the OOB address plus the production
		// prefix's gateway address.
		g.addIP(nic, sc.prodGW()+"/22", "active")
	}
	g.legIface(sc, d, "enp59s0f0")
	if rapid.Bool().Draw(g.rt, "bmc") {
		g.addBMC(sc, d)
	}
	sc.oobGWs = append(sc.oobGWs, d.ID)
	g.siteGWs[sc.cfg.NetBoxSite] = append(g.siteGWs[sc.cfg.NetBoxSite], d.ID)
	// Each OOB gateway owns a slice of the dynamic pool.
	key := shapeworld.RangeKey(shapeRoleRange, sc.oobCIDR())
	slot := len(g.w.Ranges[key])
	if slot < 2 {
		g.w.Ranges[key] = append(g.w.Ranges[key], netbox.IPRange{
			ID:           g.id(),
			StartAddress: fmt.Sprintf("10.%d.13.%d/23", sc.octet, 100+slot*50),
			EndAddress:   fmt.Sprintf("10.%d.13.%d/23", sc.octet, 149+slot*50),
			Role:         &netbox.NestedRef{ID: 7, Slug: shapeRoleRange},
			CustomFields: shapeCF(map[string]any{"slagworks_oob_owner": strings.TrimSuffix(own, "/23")}),
		})
	}
}

func (g *shapeGen) deviceSwitch(sc *shapeScope) {
	d := g.addDevice(sc, "leaf", g.placeIn(sc), true)
	shared := rapid.Bool().Draw(g.rt, "sharedPortMAC")
	base := g.mac()
	nPorts := rapid.IntRange(2, 8).Draw(g.rt, "ports")
	if g.k.Large {
		nPorts = rapid.IntRange(8, 56).Draw(g.rt, "portsLarge")
	}
	for i := range nPorts {
		mac := base
		if !shared {
			mac = g.mac()
		}
		p := g.addIface(d.ID, fmt.Sprintf("swp%d", i+1), "100gbase-x-qsfp28", mac, false)
		sc.leafPorts = append(sc.leafPorts, p)
	}
	// A BMC-less switch: mgmt_only marks its own management NIC, and the
	// device has no oob_ip.
	eth0 := g.addIface(d.ID, "eth0", "1000base-t", g.mac(), true)
	g.setPrimary(d, g.addIP(eth0, sc.freshOOB(), "active"))
	if gws := g.siteGWs[sc.cfg.NetBoxSite]; len(gws) > 0 && rapid.IntRange(0, 3).Draw(g.rt, "console") > 0 {
		g.addConsole(sc, d.ID, rapid.SampledFrom(gws).Draw(g.rt, "consoleServer"), true)
		// A neighbour of the surveyed one-port shape: a second port, cabled
		// (which line is the console is then ambiguous) or spare.
		if rapid.IntRange(0, 3).Draw(g.rt, "console2") == 0 {
			g.addConsole(sc, d.ID, rapid.SampledFrom(gws).Draw(g.rt, "consoleServer2"), rapid.Bool().Draw(g.rt, "console2Cabled"))
		}
	}
}

func (g *shapeGen) addConsole(sc *shapeScope, dev, server int, cabled bool) {
	p := netbox.ConsolePort{ID: g.id(), Name: "console", Device: &netbox.NestedRef{ID: dev}}
	if rapid.Bool().Draw(g.rt, "speedSet") {
		p.Speed = &netbox.NestedIntValue{Value: rapid.SampledFrom([]int{9600, 115200}).Draw(g.rt, "baud")}
	}
	if cabled {
		srv := shapeDevice(g.w, server)
		p.ConnectedEndpointsType = "dcim.consoleserverport"
		p.ConnectedEndpointsReachable = true
		p.ConnectedEndpoints = []netbox.ConsolePortEndpoint{{
			ID:     g.id(),
			Name:   rapid.SampledFrom([]string{"ttyS4", "ttyUSB0", "Port 3"}).Draw(g.rt, "tty"),
			Device: &netbox.NestedRef{ID: srv.ID, Name: srv.Name},
		}}
	}
	g.w.ConsolePorts = append(g.w.ConsolePorts, p)
}

func (g *shapeGen) devicePDU(sc *shapeScope) {
	d := g.addDevice(sc, "pdu", g.placeIn(sc), rapid.IntRange(0, 4).Draw(g.rt, "serial") > 0)
	d.Name = fmt.Sprintf("PDU_%d %s", d.ID, strings.ToUpper(sc.cfg.NetBoxSite)) // not a hostname
	nic := g.addIface(d.ID, "net", "other", g.mac(), rapid.Bool().Draw(g.rt, "mgmt"))
	g.addIP(nic, sc.freshOOB(), "active")
}

func (g *shapeGen) devicePassive(sc *shapeScope) {
	d := g.addDevice(sc, "panel", g.placeIn(sc), rapid.Bool().Draw(g.rt, "serial"))
	d.Name = fmt.Sprintf("Patch Panel %d", d.ID)
}

// genShapeWorld draws a world in which every site_config builds: one OOB
// prefix per scope, a production prefix holding the ephemeral pool, and no
// OOB reservation conflicts (the edits add those).
func genShapeWorld(rt *rapid.T, k shapeKnobs) *shapeGen {
	g := &shapeGen{rt: rt, k: k, w: shapeworld.New(), sd: &shapeworld.StoreData{}, next: 100,
		unclaimedLoc: map[string]int{}, siteGWs: map[string][]int{}}
	g.locIDs = rapid.Permutation([]int{50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61}).Draw(rt, "locIDs")
	for i, slug := range []string{"srv-a", "srv-b", "switch-a", "gw-a"} {
		dt := netbox.DeviceType{ID: 20 + i, Slug: slug, Model: strings.ToUpper(slug)}
		if i != 3 {
			dt.Manufacturer = &netbox.Manufacturer{ID: 5 + i%2, Slug: []string{"acme", "supermicro"}[i%2]}
		}
		switch i {
		case 0:
			dt.CustomFields = shapeCF(map[string]any{"slagworks_kernel_params": "console=ttyS1,115200n8"})
		case 2:
			dt.CustomFields = shapeCF(map[string]any{"slagworks_kernel_params": "none"})
		case 3:
			dt.CustomFields = shapeCF(map[string]any{"slagworks_kernel_params": nil})
		}
		g.dtypes = append(g.dtypes, dt)
		g.w.DeviceTypes[dt.ID] = dt
	}

	// site-a: one whole-site config. site-b: two cages, each its own config,
	// plus a third location no config claims.
	layout := rapid.IntRange(0, 2).Draw(rt, "layout")
	if layout != 1 {
		g.w.Sites["site-a"] = netbox.Site{ID: g.id(), Name: "Site A", Slug: "site-a"}
		for range rapid.IntRange(0, 2).Draw(rt, "siteALocs") {
			id := g.locID()
			g.w.Locations["site-a"] = append(g.w.Locations["site-a"], netbox.Location{
				ID: id, Name: fmt.Sprintf("room-%d", id), Slug: fmt.Sprintf("room-%d", id), Site: &netbox.NestedRef{Slug: "site-a"}})
		}
		g.scopes = append(g.scopes, &shapeScope{octet: 101, cfg: model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a"}})
	}
	if layout != 0 {
		g.w.Sites["site-b"] = netbox.Site{ID: g.id(), Name: "Site B", Slug: "site-b"}
		for i := range 2 {
			id := g.locID()
			slug := fmt.Sprintf("cage-%d", i+1)
			g.w.Locations["site-b"] = append(g.w.Locations["site-b"], netbox.Location{ID: id, Name: slug, Slug: slug, Site: &netbox.NestedRef{Slug: "site-b"}})
			sc := &shapeScope{octet: 102 + i, locID: id, cfg: model.SiteConfig{Slug: "site-b-" + slug, NetBoxSite: "site-b", NetBoxLocation: slug}}
			parent := id
			for d := range rapid.IntRange(0, 2).Draw(rt, "childLocs") {
				cid := g.locID()
				cslug := fmt.Sprintf("%s-row-%d", slug, d+1)
				g.w.Locations["site-b"] = append(g.w.Locations["site-b"], netbox.Location{
					ID: cid, Name: cslug, Slug: cslug, Site: &netbox.NestedRef{Slug: "site-b"}, Parent: &netbox.NestedRef{ID: parent}, Depth: d + 1})
				sc.subLocs = append(sc.subLocs, cid)
				if rapid.Bool().Draw(rt, "nest") {
					parent = cid
				}
			}
			g.scopes = append(g.scopes, sc)
		}
		if rapid.Bool().Draw(rt, "unclaimedLoc") {
			id := g.locID()
			g.w.Locations["site-b"] = append(g.w.Locations["site-b"], netbox.Location{ID: id, Name: "storeroom", Slug: "storeroom", Site: &netbox.NestedRef{Slug: "site-b"}})
			g.unclaimedLoc["site-b"] = id
		}
	}

	for _, sc := range g.scopes {
		sc.cfg.EphemeralPoolStart = fmt.Sprintf("10.%d.7.100", sc.octet)
		sc.cfg.EphemeralPoolEnd = fmt.Sprintf("10.%d.7.160", sc.octet)
		if rapid.Bool().Draw(rt, "relayPool") {
			sc.cfg.RelayPools = []model.RelayPool{{
				Subnet: sc.prod2CIDR(), RangeStart: fmt.Sprintf("10.%d.8.100", sc.octet), RangeEnd: fmt.Sprintf("10.%d.8.150", sc.octet), Gateway: sc.prod2GW()}}
		}
		sc.cfg.Promiscuous = rapid.Bool().Draw(rt, "promiscuous")
		g.sd.Configs = append(g.sd.Configs, sc.cfg)
		// Registered site workers, stored out of order.
		for i := range rapid.IntRange(0, 3).Draw(rt, "workers") {
			g.sd.Workers = append(g.sd.Workers, model.SiteWorker{
				ID:       uuid.MustParse(fmt.Sprintf("00000000-0000-4000-a000-%06d%06d", sc.octet, i)),
				SiteSlug: sc.cfg.Slug, WorkerID: fmt.Sprintf("worker-%d.%s", 3-i, sc.cfg.Slug)})
		}
		site := g.w.Sites[sc.cfg.NetBoxSite]

		prodCF := map[string]any{"gateway_ip": sc.prodGW()}
		if rapid.Bool().Draw(rt, "prodDNS") {
			prodCF["dns_servers"] = fmt.Sprintf("10.%d.12.10, 10.%d.12.11", sc.octet, sc.octet)
			prodCF["dhcp_lease_seconds"] = 300
		}
		prod := []netbox.Prefix{{ID: g.id(), Prefix: sc.prodCIDR(), CustomFields: shapeCF(prodCF), Tags: []netbox.NestedTag{{Slug: shapeTagProd}}}}
		if k.MultiProd {
			// More production prefixes on the same site or location, after
			// the DHCP prefix as NetBox's address order would list them
			// (a longer prefix sorts after a shorter one with the same
			// network address).
			tagged := []netbox.NestedTag{{Slug: shapeTagProd}}
			if rapid.Bool().Draw(rt, "prodNested") {
				// Inside the DHCP prefix, with a gateway of its own or
				// none: the most specific prefix decides an interface's
				// gateway even when it has none.
				cf := map[string]any{}
				if rapid.Bool().Draw(rt, "nestedGW") {
					cf["gateway_ip"] = fmt.Sprintf("10.%d.4.1", sc.octet)
				}
				prod = append(prod, netbox.Prefix{ID: g.id(), Prefix: fmt.Sprintf("10.%d.4.0/25", sc.octet), CustomFields: shapeCF(cf), Tags: tagged})
			}
			if rapid.Bool().Draw(rt, "prod2") {
				prod = append(prod, netbox.Prefix{ID: g.id(), Prefix: sc.prod2CIDR(),
					CustomFields: shapeCF(map[string]any{"gateway_ip": sc.prod2GW(), "dhcp_lease_seconds": 600}), Tags: tagged})
			}
			if rapid.IntRange(0, 3).Draw(rt, "prodSuper") == 0 {
				// An enclosing supernet tagged too, so that hosts anywhere in
				// it resolve a gateway. It sorts first; the DHCP subnet must
				// stay the prefix the ephemeral pool lies in.
				prod = append([]netbox.Prefix{{ID: g.id(), Prefix: fmt.Sprintf("10.%d.0.0/16", sc.octet),
					CustomFields: shapeCF(map[string]any{"gateway_ip": fmt.Sprintf("10.%d.0.1", sc.octet), "dns_servers": "192.0.2.53"}), Tags: tagged}}, prod...)
			}
		}
		oob := netbox.Prefix{ID: g.id(), Prefix: sc.oobCIDR(), Tags: []netbox.NestedTag{{Slug: shapeTagOOB}},
			CustomFields: shapeCF(map[string]any{
				"gateway_ip": sc.oobGW(), "dns_servers": fmt.Sprintf("10.%d.12.10,10.%d.12.11", sc.octet, sc.octet),
				"ntp_servers": sc.oobGW(), "dhcp_lease_seconds": 300})}
		if sc.locID == 0 {
			for i := range prod {
				prod[i].ScopeType, prod[i].ScopeID = "dcim.site", site.ID
			}
			oob.ScopeType, oob.ScopeID = "dcim.site", site.ID
			g.w.SitePrefixes[shapeworld.SiteTagKey(site.Slug, shapeTagProd)] = prod
			g.w.SitePrefixes[shapeworld.SiteTagKey(site.Slug, shapeTagOOB)] = []netbox.Prefix{oob}
		} else {
			for i := range prod {
				prod[i].ScopeType, prod[i].ScopeID = "dcim.location", sc.locID
			}
			oob.ScopeType, oob.ScopeID = "dcim.location", sc.locID
			g.w.LocPrefixes[shapeworld.LocTagKey(sc.locID, shapeTagProd)] = prod
			g.w.LocPrefixes[shapeworld.LocTagKey(sc.locID, shapeTagOOB)] = []netbox.Prefix{oob}
			// A child location may carry its own production prefix; the
			// config's own still supplies the DHCP subnet.
			for i, cid := range sc.subLocs {
				var p netbox.Prefix
				switch rapid.IntRange(0, 5).Draw(rt, "childProd") {
				case 0: // beside the config's prefix
					p = netbox.Prefix{Prefix: fmt.Sprintf("10.%d.%d.0/24", sc.octet, 1+i),
						CustomFields: shapeCF(map[string]any{"gateway_ip": fmt.Sprintf("10.%d.%d.1", sc.octet, 1+i)})}
				case 1: // inside it, with its own gateway
					p = netbox.Prefix{Prefix: fmt.Sprintf("10.%d.4.%d/26", sc.octet, i*64),
						CustomFields: shapeCF(map[string]any{"gateway_ip": fmt.Sprintf("10.%d.4.%d", sc.octet, i*64+1)})}
				case 2: // inside it, with no gateway
					p = netbox.Prefix{Prefix: fmt.Sprintf("10.%d.4.%d/26", sc.octet, i*64)}
				default:
					continue
				}
				p.ID, p.ScopeType, p.ScopeID, p.Tags = g.id(), "dcim.location", cid, []netbox.NestedTag{{Slug: shapeTagProd}}
				g.w.LocPrefixes[shapeworld.LocTagKey(cid, shapeTagProd)] = []netbox.Prefix{p}
			}
		}

		// Devices. OOB gateways and switches first so later hosts can cable
		// to them and land /31 leaf ends on them.
		for range rapid.IntRange(0, 2).Draw(rt, "oobgws") {
			g.deviceOOBGW(sc)
		}
		// Now and then a dynamic range nobody can serve: no owner, or an
		// owner another range already names (which then costs both).
		if key := shapeworld.RangeKey(shapeRoleRange, sc.oobCIDR()); rapid.IntRange(0, 7).Draw(rt, "strayRange") == 0 {
			r := netbox.IPRange{ID: g.id(), StartAddress: fmt.Sprintf("10.%d.13.200/23", sc.octet), EndAddress: fmt.Sprintf("10.%d.13.220/23", sc.octet),
				Role: &netbox.NestedRef{ID: 7, Slug: shapeRoleRange}}
			if have := g.w.Ranges[key]; len(have) > 0 && rapid.Bool().Draw(rt, "strayOwnerCopied") {
				r.CustomFields = have[0].CustomFields
			}
			g.w.Ranges[key] = append(g.w.Ranges[key], r)
		}
		for range rapid.IntRange(0, 2).Draw(rt, "switches") {
			g.deviceSwitch(sc)
		}
		n := rapid.IntRange(0, 5).Draw(rt, "hosts")
		if k.Large {
			n = rapid.IntRange(20, 90).Draw(rt, "hostsLarge")
		}
		for range n {
			switch rapid.SampledFrom([]string{"l3", "l3", "l3", "bond", "bond", "store", "router", "pdu", "passive"}).Draw(rt, "kind") {
			case "l3":
				g.deviceL3(sc)
			case "bond":
				g.deviceBond(sc, false)
			case "store":
				g.deviceBond(sc, true)
			case "router":
				g.deviceRouter(sc)
			case "pdu":
				g.devicePDU(sc)
			case "passive":
				g.devicePassive(sc)
			}
		}
	}
	// Devices in a managed site that no config claims.
	for site, loc := range g.unclaimedLoc {
		for _, sc := range g.scopes {
			if sc.cfg.NetBoxSite == site {
				for range rapid.IntRange(0, 2).Draw(rt, "unclaimedDevs") {
					d := g.addDevice(sc, "spare", loc, true)
					nic := g.addIface(d.ID, "eno1", "1000base-t", g.mac(), false)
					g.addIP(nic, sc.freshOOB(), "active")
				}
				break
			}
		}
	}
	// site-b devices with no location: unclaimed by both cage configs.
	for _, sc := range g.scopes {
		if sc.locID != 0 && rapid.IntRange(0, 3).Draw(rt, "noLocDev") == 0 {
			d := g.addDevice(sc, "floating", 0, true)
			nic := g.addIface(d.ID, "eno1", "1000base-t", g.mac(), false)
			g.addIP(nic, sc.freshOOB(), "active")
		}
	}

	// Postgres: a machine row for every device with a serial, as the mirror
	// keeps it (unclaimed devices included: a device moved out of scope keeps
	// its row).
	for _, site := range shapeSortedKeys(g.w.Devices) {
		for _, d := range g.w.Devices[site] {
			if d.Serial == "" {
				continue
			}
			devID := d.ID
			m := model.Machine{
				ID:             uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", d.ID)),
				NetBoxDeviceID: &devID,
				NetBoxSerial:   d.Serial,
				State: rapid.SampledFrom([]model.MachineState{model.MachineStateNew, model.MachineStateNew, model.MachineStateRunning,
					model.MachineStateCommissioned, model.MachineStateFailed}).Draw(rt, "state"),
				PXEEligible: rapid.Bool().Draw(rt, "pxe"),
				Locked:      rapid.IntRange(0, 4).Draw(rt, "locked") == 0,
			}
			if m.State != model.MachineStateNew {
				m.CommissionData = json.RawMessage(fmt.Sprintf(
					`{"system":{"serial_number":%q,"chassis_serial":%q,"uuid":"11111111-2222-4333-8444-%012d","system_product":"SRV-A"},"disks":[{"serial":"D%d","wwn":"0x5000%08d"}]}`,
					d.Serial, d.Serial, d.ID, d.ID, d.ID))
			}
			g.sd.Machines = append(g.sd.Machines, m)
		}
	}
	g.sd.Images = []model.Image{{ID: uuid.MustParse("00000000-0000-4000-8000-0000000000aa"), Name: "noble-pxe", Version: "1", Arch: "amd64", SHA256: "x", URL: "u"}}
	g.sd.SSHKeys = []string{"ssh-ed25519 AAAA a", "ssh-ed25519 BBBB b"}
	sid := func(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-9000-%012d", n)) }
	g.sd.NetworkScripts = []model.NetworkScript{
		{ID: sid(1), Name: "100-l3", Priority: 100, Content: "#!/bin/sh\n# slagworks:match device_type=srv-a\n# slagworks:priority 100\n"},
		{ID: sid(2), Name: "150-oobgw", Priority: 150, Content: "#!/bin/sh\n# slagworks:match hostname=/^oobgw-/\n# slagworks:priority 150\n"},
		{ID: sid(3), Name: "900-broken", Priority: 900, Content: "#!/bin/sh\n# slagworks:mtach site=site-a\n"},
	}
	g.sd.Scripts = []model.StorageScript{
		{ID: sid(4), Name: "100-srv-a", Priority: 100, Content: "#!/bin/sh\n# slagworks:match system_product=SRV-A\n# slagworks:priority 100\n"},
	}
	return g
}

func shapeSortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- world accessors (by ID, so edits work on recorded worlds too) --------

func shapeIface(w *shapeworld.World, id int) *netbox.Interface {
	for i := range w.Interfaces {
		if w.Interfaces[i].ID == id {
			return &w.Interfaces[i]
		}
	}
	return nil
}

func shapeDevice(w *shapeworld.World, id int) *netbox.Device {
	for _, devs := range w.Devices {
		for i := range devs {
			if devs[i].ID == id {
				return &devs[i]
			}
		}
	}
	return nil
}

func shapeIfacesOf(w *shapeworld.World, dev int) []int {
	var out []int
	for _, i := range w.Interfaces {
		if i.Device != nil && i.Device.ID == dev {
			out = append(out, i.ID)
		}
	}
	return out
}

func shapeIPsOf(w *shapeworld.World, iface int) []int {
	var out []int
	for i, ip := range w.IPs {
		if ip.AssignedObjectID != nil && *ip.AssignedObjectID == iface && ip.AssignedObjectType == "dcim.interface" {
			out = append(out, i)
		}
	}
	return out
}

func shapeMaxID(w *shapeworld.World) int {
	m := 0
	for _, devs := range w.Devices {
		for _, d := range devs {
			m = max(m, d.ID)
		}
	}
	for _, i := range w.Interfaces {
		m = max(m, i.ID)
	}
	for _, ip := range w.IPs {
		m = max(m, ip.ID)
	}
	for _, p := range w.ConsolePorts {
		m = max(m, p.ID)
	}
	return m
}

func shapeClone(w *shapeworld.World) *shapeworld.World {
	raw, err := json.Marshal(w)
	if err != nil {
		panic(err)
	}
	out := shapeworld.New()
	if err := json.Unmarshal(raw, out); err != nil {
		panic(err)
	}
	return out
}

// ---- the independent model -------------------------------------------------
//
// Written from docs/dev/architecture.md ("The snapshot pipeline"),
// docs/user/configuration.md (site configs, prefixes) and the field comments
// in internal/model/snapshot.go. It shares no code with the builder, the
// dispatcher or scope.go: scope is an ancestor walk, addresses are net/netip.

type shapeModel struct {
	w  *shapeworld.World
	sd *shapeworld.StoreData
}

func (m shapeModel) location(site string, id int) *netbox.Location {
	for i, l := range m.w.Locations[site] {
		if l.ID == id {
			return &m.w.Locations[site][i]
		}
	}
	return nil
}

// cfgLocation is the ID of the config's location, 0 for a whole-site config,
// -1 when the location is not in the site.
func (m shapeModel) cfgLocation(cfg model.SiteConfig) int {
	if cfg.NetBoxLocation == "" {
		return 0
	}
	for _, l := range m.w.Locations[cfg.NetBoxSite] {
		if l.Slug == cfg.NetBoxLocation {
			return l.ID
		}
	}
	return -1
}

func (m shapeModel) under(site string, loc, root int) bool {
	for steps := 0; loc != 0 && steps < 64; steps++ {
		if loc == root {
			return true
		}
		l := m.location(site, loc)
		if l == nil || l.Parent == nil {
			return false
		}
		loc = l.Parent.ID
	}
	return false
}

func (m shapeModel) claims(cfg model.SiteConfig, d netbox.Device) bool {
	if d.Site == nil || d.Site.Slug != cfg.NetBoxSite {
		return false
	}
	root := m.cfgLocation(cfg)
	if root == 0 {
		return true
	}
	return root > 0 && d.Location != nil && m.under(cfg.NetBoxSite, d.Location.ID, root)
}

func (m shapeModel) claimed(cfg model.SiteConfig) []netbox.Device {
	var out []netbox.Device
	for _, d := range m.w.Devices[cfg.NetBoxSite] {
		if m.claims(cfg, d) {
			out = append(out, d)
		}
	}
	return out
}

// prefixes returns a config's prefixes with a tag in the order the docs
// give: a whole-site config's site-scoped prefixes; a location-scoped
// config's own location's first, then its descendants' by location ID.
func (m shapeModel) prefixes(cfg model.SiteConfig, tag string) []netbox.Prefix {
	root := m.cfgLocation(cfg)
	if root == 0 {
		return m.w.SitePrefixes[shapeworld.SiteTagKey(cfg.NetBoxSite, tag)]
	}
	out := append([]netbox.Prefix{}, m.w.LocPrefixes[shapeworld.LocTagKey(root, tag)]...)
	var rest []int
	for _, l := range m.w.Locations[cfg.NetBoxSite] {
		if l.ID != root && m.under(cfg.NetBoxSite, l.ID, root) {
			rest = append(rest, l.ID)
		}
	}
	sort.Ints(rest)
	for _, id := range rest {
		out = append(out, m.w.LocPrefixes[shapeworld.LocTagKey(id, tag)]...)
	}
	return out
}

func shapeCFString(raw json.RawMessage, key string) string {
	var cf map[string]any
	if json.Unmarshal(raw, &cf) != nil {
		return ""
	}
	s, _ := cf[key].(string)
	return s
}

func shapeCFInt(raw json.RawMessage, key string) int {
	var cf map[string]any
	if json.Unmarshal(raw, &cf) != nil {
		return 0
	}
	f, _ := cf[key].(float64)
	return int(f)
}

func shapeCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func shapeActive(ip netbox.IPAddress) bool {
	return ip.Status == nil || ip.Status.Value == "active"
}

func shapeAddr(s string) (netip.Addr, int, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Addr(), p.Bits(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a, a.BitLen(), true
	}
	return netip.Addr{}, 0, false
}

func shapeMAC(i netbox.Interface) string {
	mac := i.MACAddress
	if mac == "" && i.PrimaryMAC != nil {
		mac = i.PrimaryMAC.MACAddress
	}
	if mac == "" && len(i.MACAddresses) > 0 {
		mac = i.MACAddresses[0].MACAddress
	}
	return strings.ToLower(mac)
}

// activeIPs returns an interface's active addresses.
func (m shapeModel) activeIPs(iface int) []netbox.IPAddress {
	var out []netbox.IPAddress
	for _, ip := range m.w.IPs {
		if ip.AssignedObjectType == "dcim.interface" && ip.AssignedObjectID != nil && *ip.AssignedObjectID == iface && shapeActive(ip) {
			out = append(out, ip)
		}
	}
	return out
}

// shapeOwn reports whether NetBox's role leaves an address its interface's
// own: no role, or loopback. Secondary and shared (anycast, VIP, FHRP)
// addresses are not.
func shapeOwn(ip netbox.IPAddress) bool {
	return ip.Role == nil || ip.Role.Value == "" || ip.Role.Value == "loopback"
}

// chosenIP is the one address an interface's facts carry: the device's
// primary IPv4 when it is on the interface, else an IPv4 address, else an
// IPv6 address; within a family an own address before a secondary or shared
// one, then the lowest (ties: shorter prefix, then lower ID).
func (m shapeModel) chosenIP(iface, primary int) string {
	addrs := m.activeIPs(iface)
	if len(addrs) == 0 {
		return ""
	}
	type cand struct {
		v6   bool
		own  bool
		a    netip.Addr
		bits int
		ip   netbox.IPAddress
	}
	var cs []cand
	for _, ip := range addrs {
		if primary != 0 && ip.ID == primary {
			return ip.Address
		}
		a, bits, ok := shapeAddr(ip.Address)
		if !ok {
			continue
		}
		cs = append(cs, cand{!a.Unmap().Is4(), shapeOwn(ip), a.Unmap(), bits, ip})
	}
	if len(cs) == 0 {
		return addrs[0].Address
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].v6 != cs[j].v6 {
			return !cs[i].v6
		}
		if cs[i].own != cs[j].own {
			return cs[i].own
		}
		if c := cs[i].a.Compare(cs[j].a); c != 0 {
			return c < 0
		}
		if cs[i].bits != cs[j].bits {
			return cs[i].bits < cs[j].bits
		}
		return cs[i].ip.ID < cs[j].ip.ID
	})
	return cs[0].ip.Address
}

// gateway is the gateway_ip of the longest production or OOB prefix of the
// scope that contains addr, "" when none does or that prefix has none.
func (m shapeModel) gateway(cfg model.SiteConfig, addr string) string {
	a, _, ok := shapeAddr(addr)
	if !ok {
		return ""
	}
	best, bestBits := "", -1
	for _, p := range append(m.prefixes(cfg, shapeTagProd), m.prefixes(cfg, shapeTagOOB)...) {
		pp, err := netip.ParsePrefix(p.Prefix)
		if err != nil || !pp.Contains(a) || pp.Bits() <= bestBits {
			continue
		}
		best, bestBits = shapeCFString(p.CustomFields, "gateway_ip"), pp.Bits()
	}
	return best
}

type shapeFacts struct {
	Hostname, Serial, BMCIP, KernelParams, Manufacturer, Model string
	Console                                                    string
	Interfaces                                                 []string
}

type shapeExpect struct {
	Machines     map[string]shapeFacts
	Reservations []string
	DHCP         string
	Workers      []string
	RelayPools   []string
	OOB          string
}

func shapeStrip(cidr string) string {
	if before, _, ok := strings.Cut(cidr, "/"); ok {
		return before
	}
	return cidr
}

func shapeIfaceFact(id int, name, mac, typ, ip, lag string, vlan, mtu int, gw string) string {
	return fmt.Sprintf("%d|%s|%s|%s|%s|lag=%s|vlan=%d|mtu=%d|gw=%s", id, name, mac, typ, ip, lag, vlan, mtu, gw)
}

// expect recomputes what a config's snapshot must say.
func (m shapeModel) expect(cfg model.SiteConfig) shapeExpect {
	out := shapeExpect{Machines: map[string]shapeFacts{}}
	devs := m.claimed(cfg)
	byID := map[int]netbox.Device{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	serialOf := map[int]string{} // console servers resolve site-wide
	for _, d := range m.w.Devices[cfg.NetBoxSite] {
		serialOf[d.ID] = d.Serial
	}

	for _, mach := range m.sd.Machines {
		if mach.NetBoxDeviceID == nil {
			continue
		}
		d, ok := byID[*mach.NetBoxDeviceID]
		if !ok {
			continue
		}
		f := shapeFacts{Hostname: d.Name, Serial: mach.NetBoxSerial}
		if d.OOBIP != nil {
			f.BMCIP = shapeStrip(d.OOBIP.Address)
		}
		if d.DeviceType != nil {
			if dt, ok := m.w.DeviceTypes[d.DeviceType.ID]; ok {
				f.KernelParams = shapeCFString(dt.CustomFields, "slagworks_kernel_params")
				f.Model = dt.Model
				if dt.Manufacturer != nil {
					f.Manufacturer = dt.Manufacturer.Slug
				}
			}
		}
		var cabled []netbox.ConsolePort
		for _, p := range m.w.ConsolePorts {
			if p.Device != nil && p.Device.ID == d.ID && p.ConnectedEndpointsType == "dcim.consoleserverport" &&
				p.ConnectedEndpointsReachable && len(p.ConnectedEndpoints) == 1 &&
				p.ConnectedEndpoints[0].Device != nil && p.ConnectedEndpoints[0].Name != "" {
				cabled = append(cabled, p)
			}
		}
		if len(cabled) == 1 {
			ep := cabled[0].ConnectedEndpoints[0]
			baud := 115200
			if cabled[0].Speed != nil && cabled[0].Speed.Value > 0 {
				baud = cabled[0].Speed.Value
			}
			f.Console = fmt.Sprintf("%s|%s|%s|%d", ep.Device.Name, serialOf[ep.Device.ID], ep.Name, baud)
		}
		primary := 0
		if d.PrimaryIP4 != nil {
			primary = d.PrimaryIP4.ID
		}
		for _, ifc := range m.w.Interfaces {
			if ifc.Device == nil || ifc.Device.ID != d.ID {
				continue
			}
			typ := "physical"
			switch {
			case ifc.MgmtOnly && f.BMCIP != "":
				typ = "ipmi"
			case ifc.Type != nil && ifc.Type.Value == "lag":
				typ = "lag"
			}
			ip := m.chosenIP(ifc.ID, primary)
			lag, vlan, mtu := "", 0, 0
			if ifc.LAG != nil {
				lag = ifc.LAG.Name
			}
			if ifc.UntaggedVLAN != nil {
				vlan = ifc.UntaggedVLAN.VID
			}
			if ifc.MTU != nil {
				mtu = *ifc.MTU
			}
			gw := ""
			if ip != "" {
				gw = m.gateway(cfg, ip)
			}
			f.Interfaces = append(f.Interfaces, shapeIfaceFact(ifc.ID, ifc.Name, shapeMAC(ifc), typ, ip, lag, vlan, mtu, gw))
		}
		sort.Strings(f.Interfaces)
		out.Machines[mach.ID.String()] = f
	}

	// DHCP: one production prefix supplies subnet, gateway, DNS and lease;
	// relay pools are the configured ones, then one per /31 link. The
	// registered workers are listed sorted.
	if p, ok := m.dhcpPrefix(cfg); ok {
		out.DHCP = fmt.Sprintf("%s|%s|%s|%s|%v|%v|%d", p.Prefix, shapeCFString(p.CustomFields, "gateway_ip"),
			cfg.EphemeralPoolStart, cfg.EphemeralPoolEnd, cfg.Promiscuous,
			shapeCSV(shapeCFString(p.CustomFields, "dns_servers")), shapeCFInt(p.CustomFields, "dhcp_lease_seconds"))
	}
	for _, w := range m.sd.Workers {
		if w.SiteSlug == cfg.Slug {
			out.Workers = append(out.Workers, w.WorkerID)
		}
	}
	sort.Strings(out.Workers)
	for _, p := range cfg.RelayPools {
		out.RelayPools = append(out.RelayPools, fmt.Sprintf("%s|%s|%s|%s", p.Subnet, p.RangeStart, p.RangeEnd, p.Gateway))
	}
	links := map[netip.Prefix]netip.Addr{}
	for _, ifc := range m.w.Interfaces {
		if ifc.Device == nil {
			continue
		}
		if _, ok := byID[ifc.Device.ID]; !ok {
			continue
		}
		// Relay pools use the interface's topology class alone: a
		// management or bond interface never carries a routed leg.
		if ifc.MgmtOnly || (ifc.Type != nil && ifc.Type.Value == "lag") {
			continue
		}
		for _, ip := range m.activeIPs(ifc.ID) {
			p, err := netip.ParsePrefix(ip.Address)
			if err != nil || p.Bits() != 31 || !p.Addr().Is4() {
				continue
			}
			if cur, ok := links[p.Masked()]; !ok || p.Addr().Compare(cur) > 0 {
				links[p.Masked()] = p.Addr()
			}
		}
	}
	var linkKeys []netip.Prefix
	for l := range links {
		linkKeys = append(linkKeys, l)
	}
	sort.Slice(linkKeys, func(i, j int) bool { return linkKeys[i].Addr().Compare(linkKeys[j].Addr()) < 0 })
	for _, l := range linkKeys {
		host := links[l]
		peer := l.Addr()
		if peer == host {
			peer = host.Next()
		}
		out.RelayPools = append(out.RelayPools, fmt.Sprintf("%s|%s|%s|%s", l, host, host, peer))
	}

	// OOB: one prefix supplies the segment; a dynamic range under it is a
	// slice when it names an owner no other range names; a MAC with exactly
	// one active address inside the prefix, shared with no other MAC, is a
	// reservation.
	oob := m.prefixes(cfg, shapeTagOOB)
	if len(oob) == 1 {
		var slices []string
		ranges := m.w.Ranges[shapeworld.RangeKey(shapeRoleRange, oob[0].Prefix)]
		owners := map[string]int{}
		for _, r := range ranges {
			owners[shapeCFString(r.CustomFields, "slagworks_oob_owner")]++
		}
		for _, r := range ranges {
			if o := shapeCFString(r.CustomFields, "slagworks_oob_owner"); o == "" || owners[o] != 1 {
				continue
			}
			slices = append(slices, fmt.Sprintf("%s-%s@%s", shapeStrip(r.StartAddress), shapeStrip(r.EndAddress), shapeCFString(r.CustomFields, "slagworks_oob_owner")))
		}
		sort.Strings(slices)
		out.OOB = fmt.Sprintf("%s|%s|%v|%v|%d|%v", oob[0].Prefix, shapeCFString(oob[0].CustomFields, "gateway_ip"),
			shapeCSV(shapeCFString(oob[0].CustomFields, "dns_servers")), shapeCSV(shapeCFString(oob[0].CustomFields, "ntp_servers")),
			shapeCFInt(oob[0].CustomFields, "dhcp_lease_seconds"), slices)
	}
	out.Reservations = m.unambiguousReservations(cfg)
	return out
}

// dhcpPrefix is the production prefix the fabric DHCP config comes from: the
// longest one holding the config's ephemeral pool start, among the site's
// prefixes for a whole-site config and among the config's own location's
// for a location-scoped one (a child location's prefix never supplies it).
// When none holds the pool it is the first one listed.
func (m shapeModel) dhcpPrefix(cfg model.SiteConfig) (netbox.Prefix, bool) {
	var cands []netbox.Prefix
	if root := m.cfgLocation(cfg); root == 0 {
		cands = m.w.SitePrefixes[shapeworld.SiteTagKey(cfg.NetBoxSite, shapeTagProd)]
	} else {
		cands = m.w.LocPrefixes[shapeworld.LocTagKey(root, shapeTagProd)]
	}
	if len(cands) == 0 {
		all := m.prefixes(cfg, shapeTagProd)
		if len(all) == 0 {
			return netbox.Prefix{}, false
		}
		return all[0], true
	}
	best, bestBits := cands[0], -1
	if pool, err := netip.ParseAddr(cfg.EphemeralPoolStart); err == nil {
		for _, p := range cands {
			if pp, err := netip.ParsePrefix(p.Prefix); err == nil && pp.Contains(pool) && pp.Bits() > bestBits {
				best, bestBits = p, pp.Bits()
			}
		}
	}
	return best, true
}

// oobAddrs returns the active addresses of an interface that fall inside one
// of the scope's OOB prefixes, as bare addresses, one entry per NetBox row.
func (m shapeModel) oobAddrs(cfg model.SiteConfig, iface int) []string {
	var nets []netip.Prefix
	for _, p := range m.prefixes(cfg, shapeTagOOB) {
		if pp, err := netip.ParsePrefix(p.Prefix); err == nil {
			nets = append(nets, pp)
		}
	}
	var out []string
	for _, ip := range m.activeIPs(iface) {
		a, _, ok := shapeAddr(ip.Address)
		if !ok {
			continue
		}
		for _, n := range nets {
			if n.Contains(a) {
				out = append(out, a.String())
				break
			}
		}
	}
	return out
}

// oobConflict says why a scope's reservation set is ambiguous, "" if it is
// not: an interface with more than one OOB address, one MAC with two
// addresses, or one address on two MACs. The tests use it only to count how
// often an edit produced such a scope.
func (m shapeModel) oobConflict(cfg model.SiteConfig) string {
	byMAC, byIP := map[string]string{}, map[string]string{}
	for _, d := range m.claimed(cfg) {
		for _, id := range shapeIfacesOf(m.w, d.ID) {
			addrs := m.oobAddrs(cfg, id)
			if len(addrs) > 1 {
				return "an interface carries more than one OOB address"
			}
			mac := shapeMAC(*shapeIface(m.w, id))
			if len(addrs) == 0 || mac == "" {
				continue
			}
			if prev, ok := byMAC[mac]; ok && prev != addrs[0] {
				return "one MAC carries two OOB addresses"
			}
			if prev, ok := byIP[addrs[0]]; ok && prev != mac {
				return "one OOB address sits on two MACs"
			}
			byMAC[mac], byIP[addrs[0]] = addrs[0], mac
		}
	}
	return ""
}

// unambiguousReservations is the reservation set of a scope: every MAC whose
// interfaces carry exactly one distinct OOB address that no other MAC
// carries keeps its reservation; a MAC with two different OOB addresses, or
// whose address is also on another MAC, gets none. Address objects that
// repeat one address are one address.
func (m shapeModel) unambiguousReservations(cfg model.SiteConfig) []string {
	type cand struct{ mac, addr string }
	var cands []cand
	macAddrs, addrMACs := map[string]map[string]bool{}, map[string]map[string]bool{}
	for _, d := range m.claimed(cfg) {
		for _, id := range shapeIfacesOf(m.w, d.ID) {
			distinct := map[string]bool{}
			for _, a := range m.oobAddrs(cfg, id) {
				distinct[a] = true
			}
			mac := shapeMAC(*shapeIface(m.w, id))
			if mac == "" || len(distinct) == 0 {
				continue
			}
			if macAddrs[mac] == nil {
				macAddrs[mac] = map[string]bool{}
			}
			for a := range distinct {
				macAddrs[mac][a] = true
				if addrMACs[a] == nil {
					addrMACs[a] = map[string]bool{}
				}
				addrMACs[a][mac] = true
				cands = append(cands, cand{mac, a})
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		if len(macAddrs[c.mac]) != 1 || len(addrMACs[c.addr]) != 1 || seen[c.mac] {
			continue
		}
		seen[c.mac] = true
		out = append(out, c.mac+"="+c.addr)
	}
	sort.Strings(out)
	return out
}

// shapeProject reduces a built snapshot to the model's vocabulary.
func shapeProject(s model.SiteSnapshot) shapeExpect {
	out := shapeExpect{Machines: map[string]shapeFacts{}}
	for _, ms := range s.Machines {
		f := shapeFacts{Hostname: ms.Hostname, Serial: ms.NetBoxSerial, BMCIP: ms.BMCIP,
			KernelParams: ms.KernelParams, Manufacturer: ms.Manufacturer, Model: ms.Model}
		if ms.Console != nil {
			f.Console = fmt.Sprintf("%s|%s|%s|%d", ms.Console.Server, ms.Console.ServerSerial, ms.Console.Port, ms.Console.Baud)
		}
		for _, i := range ms.Interfaces {
			f.Interfaces = append(f.Interfaces, shapeIfaceFact(i.NetBoxID, i.Name, i.MAC, i.Type, i.IP, i.LAG, i.VLAN, i.MTU, i.Gateway))
		}
		sort.Strings(f.Interfaces)
		out.Machines[ms.ID.String()] = f
	}
	out.Workers = s.DHCP.Workers
	out.DHCP = fmt.Sprintf("%s|%s|%s|%s|%v|%v|%d", s.DHCP.Subnet, s.DHCP.Gateway, s.DHCP.EphemeralPoolStart, s.DHCP.EphemeralPoolEnd,
		s.DHCP.Promiscuous, s.DHCP.DNS, s.DHCP.LeaseSeconds)
	for _, p := range s.DHCP.RelayPools {
		out.RelayPools = append(out.RelayPools, fmt.Sprintf("%s|%s|%s|%s", p.Subnet, p.RangeStart, p.RangeEnd, p.Gateway))
	}
	if s.OOB.Subnet != "" {
		var slices []string
		for _, sl := range s.OOB.DynamicSlices {
			slices = append(slices, fmt.Sprintf("%s-%s@%s", sl.Start, sl.End, sl.Owner))
		}
		sort.Strings(slices)
		out.OOB = fmt.Sprintf("%s|%s|%v|%v|%d|%v", s.OOB.Subnet, s.OOB.Gateway, s.OOB.DNS, s.OOB.NTP, s.OOB.LeaseSeconds, slices)
	}
	for _, r := range s.Reservations {
		out.Reservations = append(out.Reservations, r.MAC+"="+r.IP)
	}
	sort.Strings(out.Reservations)
	return out
}

// shapeDiff describes the first disagreement between the model and a build,
// "" when they agree.
func shapeDiff(want, got shapeExpect) string {
	if want.DHCP != got.DHCP {
		return fmt.Sprintf("dhcp: want %s, got %s", want.DHCP, got.DHCP)
	}
	if fmt.Sprint(want.Workers) != fmt.Sprint(got.Workers) {
		return fmt.Sprintf("workers: want %v, got %v", want.Workers, got.Workers)
	}
	if fmt.Sprint(want.RelayPools) != fmt.Sprint(got.RelayPools) {
		return fmt.Sprintf("relay pools: want %v, got %v", want.RelayPools, got.RelayPools)
	}
	if want.OOB != got.OOB {
		return fmt.Sprintf("oob: want %s, got %s", want.OOB, got.OOB)
	}
	if fmt.Sprint(want.Reservations) != fmt.Sprint(got.Reservations) {
		return fmt.Sprintf("reservations: want %v, got %v", want.Reservations, got.Reservations)
	}
	for id, w := range want.Machines {
		g, ok := got.Machines[id]
		if !ok {
			return fmt.Sprintf("machine %s (%s) missing from the snapshot", id, w.Hostname)
		}
		if fmt.Sprintf("%+v", w) != fmt.Sprintf("%+v", g) {
			return fmt.Sprintf("machine %s:\n want %+v\n got  %+v", id, w, g)
		}
	}
	for id, g := range got.Machines {
		if _, ok := want.Machines[id]; !ok {
			return fmt.Sprintf("machine %s (%s) in the snapshot but not claimed by the config", id, g.Hostname)
		}
	}
	return ""
}
