package snapshot

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Property-based coverage for the per-machine network facts Build hands to
// the network-config scripts (MachineSnapshot.Interfaces), the per-/31 relay
// pools, and the FQDN. The generator draws NetBox-shaped sites from SMALL
// pools (three devices, a handful of interface names, a few dozen addresses)
// so interfaces collide on names, bonds share members, IPs pile onto one
// interface, both ends of a /31 show up, and prefixes nest or duplicate.
//
// Oracles:
//   - an independent reference model of the facts contract written from the
//     SnapshotInterface doc comments (net/netip, no production helpers);
//   - one address per interface: a bound machine's interface with more than
//     one active address never fails the build; its facts carry the
//     device's primary_ip4 when that is one of them, else the numerically
//     lowest IPv4 address, else the lowest IPv6 address, preferring within
//     a family addresses whose NetBox role is unset or loopback;
//   - relay pools: one per /31 link seen on an active physical-class
//     interface address, offering the higher address of the ends present;
//   - metamorphic: permuting Devices, Interfaces, IPs, OOB prefixes and the
//     non-primary production prefixes must not change any machine's facts,
//     relay pools or FQDN, including which address a multi-address
//     interface gets (NetBox list order is not a contract).

// ---------- generator ----------

type genOpts struct {
	multiIP bool // allow >1 active IP per interface (Build chooses one)
}

var ifaceNamePool = []string{"eno1", "eno2", "enp1s0f0", "enp1s0f1", "bond0", "bond1", "lo", "ipmi0", "bond0.100", "eth0"}

var ifaceTypePool = []string{"1000base-t", "25gbase-x-sfp28", "lag", "virtual", "bridge", ""}

var macPool = []string{"7C:C2:55:42:FC:30", "7c:c2:55:42:fc:31", "aa:bb:cc:dd:ee:01", "AA:BB:CC:DD:EE:02", "3c:ec:ef:de:d0:89"}

// addrPool spans: an L2 /22 with a gateway, nested /24 and /16 prefixes,
// several /31 pairs (host odd side), loopback /32s, an OOB /23, IPv6.
var addrPool = []string{
	"10.253.0.22/22", "10.253.0.23/22", "10.253.1.5/24",
	"10.254.29.9/31", "10.254.29.11/31", "10.254.29.13/31",
	"10.254.31.67/32", "10.254.31.68/32",
	"10.253.241.108/23", "10.253.240.5/23",
	"192.168.7.4/24",
	"2001:db8::22/64", "2001:db8:1::1/128", "fe80::1/64",
}

// leafSide maps a host-side /31 to its leaf-side partner, drawn onto a
// separate leaf device so both ends of a link are in scope.
var leafSide = map[string]string{
	"10.254.29.9/31":  "10.254.29.8/31",
	"10.254.29.11/31": "10.254.29.10/31",
	"10.254.29.13/31": "10.254.29.12/31",
}

type prefixSpec struct {
	cidr, gw string
	oob      bool
}

var prefixPool = []prefixSpec{
	{"10.253.0.0/22", "10.253.3.254", false},
	{"10.253.0.0/16", "10.253.255.254", false},
	{"10.253.1.0/24", "", false},
	{"10.254.29.0/24", "", false},
	{"2001:db8::/64", "2001:db8::1", false},
	{"10.253.240.0/23", "10.253.240.1", true},
	{"192.168.7.0/24", "192.168.7.1", true},
}

// leafDeviceID is the device that holds the leaf side of the /31 links.
const leafDeviceID = 50

type genSite struct {
	in   Inputs
	opts genOpts
}

func drawSite(t *rapid.T, opts genOpts) genSite {
	var gs genSite
	nDev := rapid.IntRange(1, 3).Draw(t, "nDev")
	in := Inputs{
		Site:   netbox.Site{Name: "site1", Slug: "site1"},
		Config: model.SiteConfig{Slug: "site1", EphemeralPoolStart: "10.253.0.100", EphemeralPoolEnd: "10.253.0.200"},
	}
	// Prefixes: primary first (always the /22), then a random subset.
	in.Prefixes = []netbox.Prefix{{ID: 1, Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.3.254")}}
	pid := 2
	for i, p := range prefixPool[1:] {
		if !rapid.Bool().Draw(t, fmt.Sprintf("prefix%d", i)) {
			continue
		}
		np := netbox.Prefix{ID: pid, Prefix: p.cidr}
		pid++
		if p.gw != "" {
			np.CustomFields = gatewayJSON(p.gw)
		}
		if p.oob {
			in.OOBPrefixes = append(in.OOBPrefixes, np)
		} else {
			in.Prefixes = append(in.Prefixes, np)
		}
	}
	// At most one OOB prefix: buildOOBConfig rejects >1 (tested elsewhere).
	if len(in.OOBPrefixes) > 1 {
		in.OOBPrefixes = in.OOBPrefixes[:1]
	}
	// The same CIDR twice with different gateways: an ambiguous gateway the
	// reference model reports as "?".
	if rapid.IntRange(0, 3).Draw(t, "dupPrefix") == 0 {
		in.Prefixes = append(in.Prefixes, netbox.Prefix{ID: pid, Prefix: "10.253.0.0/22", CustomFields: gatewayJSON("10.253.0.1")})
	}

	ifID := 100
	ipID := 1000
	usedAddr := map[string]bool{}
	for d := 1; d <= nDev; d++ {
		dev := netbox.Device{ID: d, Name: rapid.SampledFrom([]string{"hot-spare-1.pop2", "kworker-1.site1", "host", "a.b.c", "x."}).Draw(t, fmt.Sprintf("dev%dname", d))}
		if rapid.Bool().Draw(t, fmt.Sprintf("dev%doob", d)) {
			dev.OOBIP = &netbox.NestedIPAddress{Address: "10.253.241.200/23"}
		}
		in.Devices = append(in.Devices, dev)
		nIf := rapid.IntRange(0, 6).Draw(t, fmt.Sprintf("dev%dnIf", d))
		var names []string
		var devIPs []int
		for k := 0; k < nIf; k++ {
			pre := fmt.Sprintf("d%di%d", d, k)
			name := rapid.SampledFrom(ifaceNamePool).Draw(t, pre+"name")
			iface := netbox.Interface{
				ID:       ifID,
				Name:     name,
				Device:   &netbox.NestedRef{ID: d, Name: dev.Name},
				MgmtOnly: rapid.IntRange(0, 5).Draw(t, pre+"mgmt") == 0,
			}
			ifID++
			if ty := rapid.SampledFrom(ifaceTypePool).Draw(t, pre+"type"); ty != "" {
				iface.Type = &netbox.NestedValue{Value: ty}
			}
			switch rapid.IntRange(0, 3).Draw(t, pre+"macKind") {
			case 0:
			case 1:
				iface.MACAddress = rapid.SampledFrom(macPool).Draw(t, pre+"mac")
			case 2:
				iface.PrimaryMAC = &netbox.MACAddressRef{MACAddress: rapid.SampledFrom(macPool).Draw(t, pre+"mac")}
			case 3:
				iface.MACAddresses = []netbox.MACAddressRef{
					{MACAddress: rapid.SampledFrom(macPool).Draw(t, pre+"mac")},
					{MACAddress: rapid.SampledFrom(macPool).Draw(t, pre+"mac2")},
				}
			}
			if len(names) > 0 && rapid.IntRange(0, 2).Draw(t, pre+"lagp") == 0 {
				iface.LAG = &netbox.NestedRef{Name: rapid.SampledFrom(names).Draw(t, pre+"lag")}
			}
			if rapid.Bool().Draw(t, pre+"hasVLAN") {
				iface.UntaggedVLAN = &netbox.NestedVLAN{VID: rapid.SampledFrom([]int{1, 100, 4094}).Draw(t, pre+"vid")}
			}
			if rapid.Bool().Draw(t, pre+"hasMTU") {
				mtu := rapid.SampledFrom([]int{1500, 9000, 9216}).Draw(t, pre+"mtu")
				iface.MTU = &mtu
			}
			names = append(names, name)
			in.Interfaces = append(in.Interfaces, iface)

			// IPs on this interface.
			maxActive := 1
			if opts.multiIP {
				maxActive = 3
			}
			nIP := rapid.IntRange(0, 3).Draw(t, pre+"nIP")
			active := 0
			for j := 0; j < nIP; j++ {
				addr := rapid.SampledFrom(addrPool).Draw(t, fmt.Sprintf("%sip%d", pre, j))
				if usedAddr[addr] {
					continue // NetBox addresses are unique within a VRF
				}
				status := rapid.SampledFrom([]string{"", "active", "reserved", "deprecated"}).Draw(t, fmt.Sprintf("%sipst%d", pre, j))
				isActive := status == "" || status == "active"
				if isActive {
					if active >= maxActive {
						status = "reserved"
					} else {
						active++
					}
				}
				usedAddr[addr] = true
				ip := netbox.IPAddress{ID: ipID, Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: new(iface.ID)}
				ipID++
				if status != "" {
					ip.Status = &netbox.NestedValue{Value: status}
				}
				// NetBox IP role: mostly unset; loopback counts as the
				// interface's own address, the rest do not.
				if r := rapid.SampledFrom([]string{"", "", "", "loopback", "secondary", "anycast", "vip"}).Draw(t, fmt.Sprintf("%siprole%d", pre, j)); r != "" {
					ip.Role = &netbox.NestedValue{Value: r}
				}
				in.IPs = append(in.IPs, ip)
				devIPs = append(devIPs, ip.ID)
			}
		}
		// primary_ip4: any of the device's addresses (active or not, on
		// any of its interfaces), or none.
		if len(devIPs) > 0 && rapid.Bool().Draw(t, fmt.Sprintf("dev%dhasPrimary", d)) {
			in.Devices[len(in.Devices)-1].PrimaryIP4 = &netbox.NestedRef{ID: rapid.SampledFrom(devIPs).Draw(t, fmt.Sprintf("dev%dprimary", d))}
		}
	}
	// Optionally put the leaf side of each used /31 on a separate leaf
	// device's physical port (a whole-site scope fetches leaf interfaces).
	if rapid.Bool().Draw(t, "leaf") {
		leafID := leafDeviceID
		in.Devices = append(in.Devices, netbox.Device{ID: leafID, Name: "leaf-1.site1"})
		for host, leaf := range sortedMap(leafSide) {
			if !usedAddr[host] || usedAddr[leaf] {
				continue
			}
			iface := netbox.Interface{ID: ifID, Name: "swp" + strings.ReplaceAll(leaf, "/", "_"), Device: &netbox.NestedRef{ID: leafID}, Type: &netbox.NestedValue{Value: "25gbase-x-sfp28"}}
			ifID++
			in.Interfaces = append(in.Interfaces, iface)
			in.IPs = append(in.IPs, netbox.IPAddress{ID: ipID, Address: leaf, AssignedObjectType: "dcim.interface", AssignedObjectID: new(iface.ID)})
			ipID++
		}
	}
	// Machines: bind a random subset of devices (and occasionally an
	// unbound or dangling machine).
	for d := 1; d <= nDev; d++ {
		if !rapid.Bool().Draw(t, fmt.Sprintf("bind%d", d)) {
			continue
		}
		did := d
		in.Machines = append(in.Machines, model.Machine{ID: uuid.NewSHA1(uuid.Nil, fmt.Appendf(nil, "m%d", d)), NetBoxDeviceID: &did})
	}
	if rapid.IntRange(0, 4).Draw(t, "dangling") == 0 {
		dangling := 999
		in.Machines = append(in.Machines, model.Machine{ID: uuid.NewSHA1(uuid.Nil, []byte("dangling")), NetBoxDeviceID: &dangling})
	}
	tmpl := rapid.SampledFrom([]string{"", "{name}", "{node}.internal.{zone}.example.net", "{name}.example.net"}).Draw(t, "tmpl")
	ht, err := ParseHostnameTemplate(tmpl)
	if err != nil {
		t.Fatalf("template %q: %v", tmpl, err)
	}
	in.HostnameTemplate = ht
	gs.in = in
	gs.opts = opts
	return gs
}

func sortedMap(m map[string]string) func(func(string, string) bool) {
	return func(yield func(string, string) bool) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !yield(k, m[k]) {
				return
			}
		}
	}
}

// ---------- reference model ----------

// refIface is the model's view of one expected fact.
type refIface struct {
	NetBoxID int
	Name     string
	MAC      string
	Type     string
	IPs      []string // every active address assigned to the interface
	IP       string   // the one the facts must carry, "" when there is none
	LAG      string
	VLAN     int
	MTU      int
	Gateway  map[string]string // ip -> expected gateway
}

func refActive(ip netbox.IPAddress) bool {
	return ip.Status == nil || ip.Status.Value == "active"
}

func refMAC(i netbox.Interface) string {
	switch {
	case i.MACAddress != "":
		return strings.ToLower(i.MACAddress)
	case i.PrimaryMAC != nil && i.PrimaryMAC.MACAddress != "":
		return strings.ToLower(i.PrimaryMAC.MACAddress)
	case len(i.MACAddresses) > 0:
		return strings.ToLower(i.MACAddresses[0].MACAddress)
	}
	return ""
}

func refType(i netbox.Interface, hasBMC bool) string {
	if i.MgmtOnly && hasBMC {
		return "ipmi"
	}
	if i.Type != nil && i.Type.Value == "lag" {
		return "lag"
	}
	return "physical"
}

// refGateway: gateway_ip of the most specific containing prefix (production
// + OOB), "" if that prefix carries none. Ties at equal length are
// ambiguous; the model reports them as "?" so the caller can skip.
func refGateway(prefixes []netbox.Prefix, cidr string) string {
	addr, err := netip.ParsePrefix(cidr)
	if err != nil {
		return ""
	}
	best, bestLen := "", -1
	tie := false
	for _, p := range prefixes {
		pp, err := netip.ParsePrefix(p.Prefix)
		if err != nil {
			continue
		}
		if !pp.Masked().Contains(addr.Addr()) {
			continue
		}
		var cf struct {
			GatewayIP string `json:"gateway_ip"`
		}
		_ = json.Unmarshal(p.CustomFields, &cf)
		switch {
		case pp.Bits() > bestLen:
			best, bestLen, tie = cf.GatewayIP, pp.Bits(), false
		case pp.Bits() == bestLen && cf.GatewayIP != best:
			tie = true
		}
	}
	if tie {
		return "?"
	}
	return best
}

func refFacts(in Inputs, devID int) []refIface {
	var dev *netbox.Device
	for i := range in.Devices {
		if in.Devices[i].ID == devID {
			dev = &in.Devices[i]
		}
	}
	hasBMC := dev != nil && dev.OOBIP != nil && dev.OOBIP.Address != ""
	allPrefixes := append(append([]netbox.Prefix{}, in.Prefixes...), in.OOBPrefixes...)
	var out []refIface
	for _, ni := range in.Interfaces {
		if ni.Device == nil || ni.Device.ID != devID {
			continue
		}
		r := refIface{NetBoxID: ni.ID, Name: ni.Name, MAC: refMAC(ni), Type: refType(ni, hasBMC), Gateway: map[string]string{}}
		if ni.LAG != nil {
			r.LAG = ni.LAG.Name
		}
		if ni.UntaggedVLAN != nil {
			r.VLAN = ni.UntaggedVLAN.VID
		}
		if ni.MTU != nil {
			r.MTU = *ni.MTU
		}
		var active []netbox.IPAddress
		for _, ip := range in.IPs {
			if ip.AssignedObjectID != nil && *ip.AssignedObjectID == ni.ID && ip.AssignedObjectType == "dcim.interface" && refActive(ip) {
				r.IPs = append(r.IPs, ip.Address)
				r.Gateway[ip.Address] = refGateway(allPrefixes, ip.Address)
				active = append(active, ip)
			}
		}
		sort.Strings(r.IPs)
		primary := 0
		if dev != nil && dev.PrimaryIP4 != nil {
			primary = dev.PrimaryIP4.ID
		}
		r.IP = refChoose(active, primary)
		out = append(out, r)
	}
	return out
}

// refChoose is the model's one-address rule: the device's primary_ip4 when
// it is one of the interface's active addresses; otherwise the first of
// them ordered IPv4 before IPv6, then the interface's own addresses (NetBox
// role unset or loopback) before the rest, then by address value, then by
// prefix length, then by NetBox ID.
func refChoose(active []netbox.IPAddress, primary int) string {
	if len(active) == 0 {
		return ""
	}
	for _, ip := range active {
		if primary != 0 && ip.ID == primary {
			return ip.Address
		}
	}
	own := func(ip netbox.IPAddress) bool {
		return ip.Role == nil || ip.Role.Value == "loopback"
	}
	sorted := append([]netbox.IPAddress(nil), active...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := netip.MustParsePrefix(sorted[i].Address), netip.MustParsePrefix(sorted[j].Address)
		switch {
		case a.Addr().Is4() != b.Addr().Is4():
			return a.Addr().Is4()
		case own(sorted[i]) != own(sorted[j]):
			return own(sorted[i])
		case a.Addr() != b.Addr():
			return a.Addr().Less(b.Addr())
		case a.Bits() != b.Bits():
			return a.Bits() < b.Bits()
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted[0].Address
}

// refPools: one pool per /31 link seen on an active physical-class
// interface address (not mgmt_only, not a lag), in subnet order. The NetBox
// records do not say which end of a link is the host, so the pool offers
// the higher address among the ends present and names the other end as
// gateway: the host under a host-odd layout, or the only end modeled.
//
// That rule offers the leaf's address when the leaf holds the higher end,
// or when the machine's own end sits on a bond or mgmt_only interface and
// only the leaf's port counts. The site worker derives both ends of a /31
// pool from the relay's giaddr, so the recorded host side never changes a
// DHCP answer.
func refPools(in Inputs) []string {
	isPhys := map[int]bool{}
	for _, i := range in.Interfaces {
		isPhys[i.ID] = !i.MgmtOnly && (i.Type == nil || i.Type.Value != "lag")
	}
	host := map[netip.Prefix]netip.Addr{}
	for _, ip := range in.IPs {
		if ip.AssignedObjectID == nil || !refActive(ip) || !isPhys[*ip.AssignedObjectID] {
			continue
		}
		p, err := netip.ParsePrefix(ip.Address)
		if err != nil || p.Bits() != 31 || !p.Addr().Is4() {
			continue
		}
		if prev, ok := host[p.Masked()]; !ok || prev.Less(p.Addr()) {
			host[p.Masked()] = p.Addr()
		}
	}
	links := make([]netip.Prefix, 0, len(host))
	for link := range host {
		links = append(links, link)
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Addr().Less(links[j].Addr()) })
	var out []string
	for _, link := range links {
		h := host[link]
		peer := h.As4()
		peer[3] ^= 1
		out = append(out, fmt.Sprintf("%s %s-%s gw=%s", link, h, h, netip.AddrFrom4(peer)))
	}
	return out
}

// refMultiIP lists the NetBox IDs of bound machines' interfaces that carry
// more than one active address.
func refMultiIP(in Inputs) []int {
	var out []int
	for _, m := range in.Machines {
		for _, r := range refFacts(in, *m.NetBoxDeviceID) {
			if len(r.IPs) > 1 {
				out = append(out, r.NetBoxID)
			}
		}
	}
	return out
}

// ---------- comparison ----------

type factKey struct {
	machine uuid.UUID
	netbox  int
}

func factsByKey(snap model.SiteSnapshot) map[factKey]model.SnapshotInterface {
	out := map[factKey]model.SnapshotInterface{}
	for _, m := range snap.Machines {
		for _, f := range m.Interfaces {
			out[factKey{m.ID, f.NetBoxID}] = f
		}
	}
	return out
}

func machineView(snap model.SiteSnapshot) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, m := range snap.Machines {
		facts := append([]model.SnapshotInterface(nil), m.Interfaces...)
		sort.Slice(facts, func(i, j int) bool { return facts[i].NetBoxID < facts[j].NetBoxID })
		b, _ := json.Marshal(struct {
			FQDN, Hostname string
			F              []model.SnapshotInterface
		}{m.FQDN, m.Hostname, facts})
		out[m.ID] = string(b)
	}
	return out
}

// poolView renders the relay pools in the order the snapshot carries them:
// that order feeds the snapshot content hash, so it is part of the contract.
func poolView(snap model.SiteSnapshot) []string {
	var out []string
	for _, p := range snap.DHCP.RelayPools {
		out = append(out, fmt.Sprintf("%s %s-%s gw=%s", p.Subnet, p.RangeStart, p.RangeEnd, p.Gateway))
	}
	return out
}

func permute[T any](t *rapid.T, label string, xs []T) []T {
	out := append([]T(nil), xs...)
	for i := len(out) - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("%s%d", label, i))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func permutedInputs(t *rapid.T, in Inputs) Inputs {
	p := in
	p.Devices = permute(t, "pDev", in.Devices)
	p.Interfaces = permute(t, "pIf", in.Interfaces)
	p.IPs = permute(t, "pIP", in.IPs)
	p.OOBPrefixes = permute(t, "pOOB", in.OOBPrefixes)
	if len(in.Prefixes) > 1 {
		p.Prefixes = append([]netbox.Prefix{in.Prefixes[0]}, permute(t, "pPfx", in.Prefixes[1:])...)
	}
	return p
}

// coverage counters (reported in -v output).
var (
	cvCases, cvFacts, cvMultiIP, cvLAG, cvGateway, cvPools, cvBothEnds, cvIPv6, cvFQDNEmpty, cvMgmtLag atomic.Int64
)

func checkFacts(t *rapid.T, gs genSite) {
	in := gs.in
	snap, err := Build(in)
	// A machine's facts carry one address per interface. A site with
	// several active addresses on a bound machine's interface still
	// builds; the per-interface check below holds the facts to the one
	// address the rule picks, and the permutation at the end holds that
	// choice independent of NetBox list order.
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(refMultiIP(in)) > 0 {
		cvMultiIP.Add(1)
	}
	cvCases.Add(1)
	got := factsByKey(snap)

	// Every bound machine with a known device: facts == reference model.
	devKnown := map[int]netbox.Device{}
	for _, d := range in.Devices {
		devKnown[d.ID] = d
	}
	for _, m := range in.Machines {
		ms := findMachine(snap, m.ID)
		if ms == nil {
			t.Fatalf("machine %s missing from snapshot", m.ID)
		}
		d, ok := devKnown[*m.NetBoxDeviceID]
		if !ok {
			if len(ms.Interfaces) != 0 || ms.Hostname != "" || ms.FQDN != "" {
				t.Fatalf("dangling machine %s got facts %+v", m.ID, ms)
			}
			continue
		}
		if ms.Hostname != d.Name {
			t.Fatalf("hostname = %q, want %q", ms.Hostname, d.Name)
		}
		if ms.FQDN == "" {
			cvFQDNEmpty.Add(1)
		}
		ref := refFacts(in, d.ID)
		if len(ms.Interfaces) != len(ref) {
			t.Fatalf("machine on device %d: %d facts, want %d\n got=%+v\nref=%+v", d.ID, len(ms.Interfaces), len(ref), ms.Interfaces, ref)
		}
		for _, r := range ref {
			f, ok := got[factKey{m.ID, r.NetBoxID}]
			if !ok {
				t.Fatalf("iface %d (%s) missing from facts", r.NetBoxID, r.Name)
			}
			cvFacts.Add(1)
			if f.Name != r.Name || f.MAC != r.MAC || f.Type != r.Type || f.LAG != r.LAG || f.VLAN != r.VLAN || f.MTU != r.MTU {
				t.Fatalf("iface %d: got %+v, want %+v", r.NetBoxID, f, r)
			}
			if r.LAG != "" {
				cvLAG.Add(1)
			}
			if r.Type == "lag" && ifaceMgmtOnly(in, r.NetBoxID) {
				cvMgmtLag.Add(1)
			}
			if len(r.IPs) == 0 {
				if f.IP != "" || f.Gateway != "" {
					t.Fatalf("iface %d: unexpected ip %q gw %q (no active IP modeled)", r.NetBoxID, f.IP, f.Gateway)
				}
				continue
			}
			if f.IP != r.IP {
				t.Fatalf("iface %d: ip %q, want %q of active addresses %v (device primary_ip4 %+v)", r.NetBoxID, f.IP, r.IP, r.IPs, d.PrimaryIP4)
			}
			if strings.Contains(r.IP, ":") {
				cvIPv6.Add(1)
			}
			if want := r.Gateway[r.IP]; want != "?" && f.Gateway != want {
				t.Fatalf("iface %d ip %s: gateway %q, want %q", r.NetBoxID, f.IP, f.Gateway, want)
			}
			if f.Gateway != "" {
				cvGateway.Add(1)
			}
		}
	}

	// Relay pools: exactly the reference list, in subnet order.
	wantPools := refPools(in)
	if got := poolView(snap); !reflect.DeepEqual(got, wantPools) {
		t.Fatalf("relay pools %v, want %v", got, wantPools)
	}
	cvPools.Add(int64(len(wantPools)))
	for _, d := range in.Devices {
		if d.ID == leafDeviceID {
			cvBothEnds.Add(1)
		}
	}

	// Metamorphic: permuted NetBox list order gives the same snapshot view.
	pin := permutedInputs(t, in)
	psnap, err := Build(pin)
	if err != nil {
		t.Fatalf("Build(permuted): %v", err)
	}
	if a, b := machineView(snap), machineView(psnap); !reflect.DeepEqual(a, b) {
		t.Fatalf("machine facts depend on NetBox list order:\n orig=%v\n perm=%v", a, b)
	}
	if a, b := poolView(snap), poolView(psnap); !reflect.DeepEqual(a, b) {
		t.Fatalf("relay pools depend on NetBox list order:\n orig=%v\n perm=%v", a, b)
	}
}

func findMachine(snap model.SiteSnapshot, id uuid.UUID) *model.MachineSnapshot {
	for i := range snap.Machines {
		if snap.Machines[i].ID == id {
			return &snap.Machines[i]
		}
	}
	return nil
}

func ifaceMgmtOnly(in Inputs, id int) bool {
	for _, i := range in.Interfaces {
		if i.ID == id {
			return i.MgmtOnly
		}
	}
	return false
}

func reportCoverage(t *testing.T) {
	t.Logf("coverage: cases=%d facts=%d lagMembers=%d mgmtOnlyLags=%d gateways=%d ipv6=%d multiIPSites=%d pools=%d leafInScope=%d fqdnEmpty=%d",
		cvCases.Load(), cvFacts.Load(), cvLAG.Load(), cvMgmtLag.Load(), cvGateway.Load(), cvIPv6.Load(), cvMultiIP.Load(), cvPools.Load(), cvBothEnds.Load(), cvFQDNEmpty.Load())
}

// TestProp_NetFacts checks the facts contract on sites with at most one
// active address per interface.
func TestProp_NetFacts(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		checkFacts(t, drawSite(t, genOpts{}))
	})
	reportCoverage(t)
}

// TestProp_NetFacts_MultipleActiveIPs widens the generator to several
// active addresses on one interface: Build must still build the site and
// give each bound machine's interface the one address the rule picks,
// whatever order NetBox lists them in.
func TestProp_NetFacts_MultipleActiveIPs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		checkFacts(t, drawSite(t, genOpts{multiIP: true}))
	})
	reportCoverage(t)
}
