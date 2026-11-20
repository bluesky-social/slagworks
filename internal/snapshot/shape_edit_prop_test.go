package snapshot

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

// One-object NetBox edits: each is what an operator does with one save in the
// NetBox UI (or one API write), confined to a single device and the
// interfaces, addresses and console ports that belong to it. They are found
// and applied by NetBox ID, so the same vocabulary runs over a generated
// world and over a recorded deployment.

var shapeEditKinds = []string{
	"ip-add-oob", "ip-add-oob-gateway", "ip-add-copy", "ip-add-prod", "ip-add-foreign", "ip-add-v6", "ip-add-leg",
	"ip-status", "ip-role", "ip-move", "ip-del",
	"mac-clear", "mac-fresh", "mac-copy-same-device", "mac-copy-other-device", "mac-case",
	"iface-add", "iface-del", "iface-mgmt", "iface-type", "iface-lag",
	"dev-serial-clear", "dev-serial-set", "dev-serial-copy", "dev-primary-clear", "dev-primary-set", "dev-oob-clear", "dev-oob-set",
	"dev-rack-clear", "dev-position", "dev-loc-clear", "dev-loc-move", "dev-type", "dev-rename", "dev-del",
	"console-add", "console-uncable",
}

// shapeConflictKinds are the edits that can make a scope's OOB reservation
// set ambiguous.
var shapeConflictKinds = []string{"ip-add-oob", "ip-add-oob-gateway", "ip-add-copy", "ip-status", "ip-move", "mac-copy-same-device", "mac-copy-other-device"}

var shapeRoles = []string{"", "loopback", "secondary", "anycast", "vip", "vrrp"}

var shapeStatuses = []string{"active", "active", "reserved", "deprecated", "dhcp", "slaac"}

var shapeOddNames = []string{"", " ", "UPPER.Site-A", "two words", "under_score.site-a", "trailing.dot.", "-lead.site-a",
	"a.b.c.d.e", strings.Repeat("x", 70) + ".site-a", "nïce.site-a", "node/1.site-a", "plain"}

type shapeEditor struct {
	rt   *rapid.T
	w    *shapeworld.World
	sd   *shapeworld.StoreData
	next int
}

func newShapeEditor(rt *rapid.T, w *shapeworld.World, sd *shapeworld.StoreData) *shapeEditor {
	return &shapeEditor{rt: rt, w: w, sd: sd, next: shapeMaxID(w) + 1000}
}

func (e *shapeEditor) id() int { e.next++; return e.next }

func (e *shapeEditor) mac() string {
	n := e.id()
	return fmt.Sprintf("06:00:00:%02X:%02X:%02X", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

// used reports whether any address object in the world has this host address.
func (e *shapeEditor) used(a netip.Addr) bool {
	for _, ip := range e.w.IPs {
		if got, _, ok := shapeAddr(ip.Address); ok && got == a {
			return true
		}
	}
	return false
}

// freeIn returns an unused host address of a prefix in CIDR form, "" when
// the prefix is unusable.
func (e *shapeEditor) freeIn(prefix string) string {
	p, err := netip.ParsePrefix(prefix)
	if err != nil || !p.Addr().Is4() || p.Bits() > 30 {
		return ""
	}
	size := 1 << (32 - p.Bits())
	a := p.Masked().Addr()
	skip := rapid.IntRange(1, size-2).Draw(e.rt, "hostOffset")
	for range skip {
		a = a.Next()
	}
	for range size {
		if p.Contains(a) && a != p.Masked().Addr() && !e.used(a) {
			return fmt.Sprintf("%s/%d", a, p.Bits())
		}
		a = a.Next()
		if !p.Contains(a) {
			a = p.Masked().Addr().Next()
		}
	}
	return ""
}

func (e *shapeEditor) addIP(iface int, addr, status string) {
	ip := netbox.IPAddress{ID: e.id(), Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &iface,
		Status: &netbox.NestedValue{Value: status}}
	e.w.IPs = append(e.w.IPs, ip)
}

func (e *shapeEditor) pickIface(dev int, label string) int {
	ids := shapeIfacesOf(e.w, dev)
	if len(ids) == 0 {
		return 0
	}
	return rapid.SampledFrom(ids).Draw(e.rt, label)
}

// pickIP returns the index in w.IPs of one of the device's addresses, -1 if
// it has none.
func (e *shapeEditor) pickIP(dev int, label string) int {
	var idx []int
	for _, ifc := range shapeIfacesOf(e.w, dev) {
		idx = append(idx, shapeIPsOf(e.w, ifc)...)
	}
	if len(idx) == 0 {
		return -1
	}
	return rapid.SampledFrom(idx).Draw(e.rt, label)
}

func (e *shapeEditor) setMAC(ifc *netbox.Interface, mac string) {
	ifc.MACAddress, ifc.PrimaryMAC, ifc.MACAddresses = "", nil, nil
	if mac != "" {
		ref := netbox.MACAddressRef{ID: e.id(), MACAddress: mac}
		ifc.MACAddress, ifc.PrimaryMAC, ifc.MACAddresses = mac, &ref, []netbox.MACAddressRef{ref}
	}
}

// apply performs one edit of the given kind on device dev, which the config
// cfg claims. It returns a description, or "" when the edit does not apply
// to this device (nothing changed).
func (e *shapeEditor) apply(kind string, cfg model.SiteConfig, dev int) string {
	m := shapeModel{e.w, e.sd}
	d := shapeDevice(e.w, dev)
	if d == nil {
		return ""
	}
	oob := m.prefixes(cfg, shapeTagOOB)
	prod := m.prefixes(cfg, shapeTagProd)
	switch kind {
	case "ip-add-oob", "ip-add-prod", "ip-add-foreign", "ip-add-v6", "ip-add-leg", "ip-add-oob-gateway":
		ifc := e.pickIface(dev, "iface")
		if ifc == 0 {
			return ""
		}
		var addr string
		switch kind {
		case "ip-add-oob":
			if len(oob) > 0 {
				addr = e.freeIn(oob[0].Prefix)
			}
		case "ip-add-oob-gateway":
			// The OOB gateway address itself, modelled on a NIC the way a
			// VIP is.
			if len(oob) > 0 {
				if gw := shapeCFString(oob[0].CustomFields, "gateway_ip"); gw != "" {
					if p, err := netip.ParsePrefix(oob[0].Prefix); err == nil {
						addr = fmt.Sprintf("%s/%d", gw, p.Bits())
					}
				}
			}
		case "ip-add-prod":
			if len(prod) > 0 {
				addr = e.freeIn(prod[0].Prefix)
			}
		case "ip-add-foreign":
			addr = e.freeIn("203.0.113.0/24")
		case "ip-add-v6":
			addr = fmt.Sprintf("2001:db8:ffff::%x/64", e.id())
		case "ip-add-leg":
			addr = fmt.Sprintf("10.250.%d.%d/31", rapid.IntRange(0, 3).Draw(e.rt, "legNet"), rapid.IntRange(0, 9).Draw(e.rt, "legHost"))
		}
		if addr == "" {
			return ""
		}
		status := rapid.SampledFrom(shapeStatuses).Draw(e.rt, "status")
		e.addIP(ifc, addr, status)
		return fmt.Sprintf("%s: %s address on interface %d of device %d", kind, status, ifc, dev)
	case "ip-add-copy":
		// A second address object with an address some interface in the
		// scope already carries (NetBox allows it for VIP-class roles, in
		// another VRF, or with global uniqueness off).
		ifc := e.pickIface(dev, "iface")
		var pool []string
		for _, cd := range m.claimed(cfg) {
			for _, id := range shapeIfacesOf(e.w, cd.ID) {
				for _, i := range shapeIPsOf(e.w, id) {
					pool = append(pool, e.w.IPs[i].Address)
				}
			}
		}
		if ifc == 0 || len(pool) == 0 {
			return ""
		}
		e.addIP(ifc, rapid.SampledFrom(pool).Draw(e.rt, "copyOf"), "active")
		return fmt.Sprintf("ip-add-copy: duplicate of an existing address on interface %d of device %d", ifc, dev)
	case "ip-status":
		i := e.pickIP(dev, "ip")
		if i < 0 {
			return ""
		}
		status := rapid.SampledFrom(shapeStatuses).Draw(e.rt, "status")
		e.w.IPs[i].Status = &netbox.NestedValue{Value: status}
		return fmt.Sprintf("ip-status: address %d of device %d becomes %s", e.w.IPs[i].ID, dev, status)
	case "ip-role":
		i := e.pickIP(dev, "ip")
		if i < 0 {
			return ""
		}
		role := rapid.SampledFrom(shapeRoles).Draw(e.rt, "role")
		e.w.IPs[i].Role = nil
		if role != "" {
			e.w.IPs[i].Role = &netbox.NestedValue{Value: role}
		}
		return fmt.Sprintf("ip-role: address %d of device %d gets role %q", e.w.IPs[i].ID, dev, role)
	case "ip-move":
		i := e.pickIP(dev, "ip")
		ifc := e.pickIface(dev, "toIface")
		if i < 0 || ifc == 0 {
			return ""
		}
		e.w.IPs[i].AssignedObjectID = &ifc
		return fmt.Sprintf("ip-move: address %d moves to interface %d of device %d", e.w.IPs[i].ID, ifc, dev)
	case "ip-del":
		i := e.pickIP(dev, "ip")
		if i < 0 {
			return ""
		}
		id := e.w.IPs[i].ID
		e.w.IPs = slices.Delete(e.w.IPs, i, i+1)
		e.clearRefs(d, id)
		return fmt.Sprintf("ip-del: address %d of device %d deleted", id, dev)
	case "mac-clear", "mac-fresh", "mac-case", "mac-copy-same-device", "mac-copy-other-device":
		id := e.pickIface(dev, "iface")
		if id == 0 {
			return ""
		}
		ifc := shapeIface(e.w, id)
		switch kind {
		case "mac-clear":
			e.setMAC(ifc, "")
		case "mac-fresh":
			e.setMAC(ifc, e.mac())
		case "mac-case":
			cur := shapeMAC(*ifc)
			if cur == "" {
				return ""
			}
			e.setMAC(ifc, strings.ToUpper(cur))
		case "mac-copy-same-device", "mac-copy-other-device":
			var pool []string
			for _, cd := range m.claimed(cfg) {
				if (cd.ID == dev) != (kind == "mac-copy-same-device") {
					continue
				}
				for _, oid := range shapeIfacesOf(e.w, cd.ID) {
					if mac := shapeMAC(*shapeIface(e.w, oid)); mac != "" && oid != id {
						pool = append(pool, mac)
					}
				}
			}
			if len(pool) == 0 {
				return ""
			}
			e.setMAC(shapeIface(e.w, id), rapid.SampledFrom(pool).Draw(e.rt, "copyOf"))
		}
		return fmt.Sprintf("%s: interface %d of device %d", kind, id, dev)
	case "iface-add":
		typ := rapid.SampledFrom([]string{"1000base-t", "25gbase-x-sfp28", "lag", "virtual", "bridge", "other"}).Draw(e.rt, "type")
		ifc := netbox.Interface{ID: e.id(), Name: fmt.Sprintf("new%d", e.next), Enabled: true,
			MgmtOnly: rapid.Bool().Draw(e.rt, "mgmt"), Device: &netbox.NestedRef{ID: dev}, Type: &netbox.NestedValue{Value: typ}}
		if rapid.Bool().Draw(e.rt, "withMAC") {
			e.setMAC(&ifc, e.mac())
		}
		e.w.Interfaces = append(e.w.Interfaces, ifc)
		return fmt.Sprintf("iface-add: %s interface %d on device %d", typ, ifc.ID, dev)
	case "iface-del":
		id := e.pickIface(dev, "iface")
		if id == 0 {
			return ""
		}
		e.deleteIface(d, id)
		return fmt.Sprintf("iface-del: interface %d of device %d and its addresses deleted", id, dev)
	case "iface-mgmt":
		id := e.pickIface(dev, "iface")
		if id == 0 {
			return ""
		}
		ifc := shapeIface(e.w, id)
		ifc.MgmtOnly = !ifc.MgmtOnly
		return fmt.Sprintf("iface-mgmt: interface %d of device %d mgmt_only=%v", id, dev, ifc.MgmtOnly)
	case "iface-type":
		id := e.pickIface(dev, "iface")
		if id == 0 {
			return ""
		}
		typ := rapid.SampledFrom([]string{"1000base-t", "lag", "virtual", "bridge"}).Draw(e.rt, "type")
		shapeIface(e.w, id).Type = &netbox.NestedValue{Value: typ}
		return fmt.Sprintf("iface-type: interface %d of device %d becomes %s", id, dev, typ)
	case "iface-lag":
		id := e.pickIface(dev, "iface")
		parent := e.pickIface(dev, "lagParent")
		if id == 0 || parent == id {
			return ""
		}
		shapeIface(e.w, id).LAG = &netbox.NestedRef{ID: parent, Name: shapeIface(e.w, parent).Name}
		return fmt.Sprintf("iface-lag: interface %d of device %d joins %d", id, dev, parent)
	case "dev-serial-clear":
		d.Serial = ""
	case "dev-serial-set":
		d.Serial = fmt.Sprintf("NEW%07d", e.id())
	case "dev-serial-copy":
		// NetBox does not require serials to be unique.
		others := m.claimed(cfg)
		d.Serial = rapid.SampledFrom(others).Draw(e.rt, "serialOf").Serial
	case "dev-primary-clear":
		d.PrimaryIP4 = nil
	case "dev-primary-set":
		i := e.pickIP(dev, "ip")
		if i < 0 {
			return ""
		}
		d.PrimaryIP4 = &netbox.NestedRef{ID: e.w.IPs[i].ID}
	case "dev-oob-clear":
		d.OOBIP = nil
	case "dev-oob-set":
		i := e.pickIP(dev, "ip")
		if i < 0 {
			return ""
		}
		d.OOBIP = &netbox.NestedIPAddress{ID: e.w.IPs[i].ID, Address: e.w.IPs[i].Address}
	case "dev-rack-clear":
		d.Rack, d.Position = nil, nil
	case "dev-position":
		p := rapid.SampledFrom([]float64{1, 12, 12.5, 42, 0}).Draw(e.rt, "u")
		d.Position = &p
	case "dev-loc-clear":
		d.Location = nil
	case "dev-loc-move":
		locs := e.w.Locations[cfg.NetBoxSite]
		if len(locs) == 0 {
			return ""
		}
		l := rapid.SampledFrom(locs).Draw(e.rt, "toLoc")
		d.Location = &netbox.NestedRef{ID: l.ID, Slug: l.Slug, Name: l.Name}
	case "dev-type":
		var ids []int
		for id := range e.w.DeviceTypes {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		dt := e.w.DeviceTypes[rapid.SampledFrom(ids).Draw(e.rt, "dtype")]
		d.DeviceType = &netbox.NestedRef{ID: dt.ID, Slug: dt.Slug}
	case "dev-rename":
		d.Name = rapid.SampledFrom(shapeOddNames).Draw(e.rt, "name")
		// NetBox serializes a console port's far-end device by reference, so
		// the name other devices' console ports show follows the rename.
		for i := range e.w.ConsolePorts {
			for j := range e.w.ConsolePorts[i].ConnectedEndpoints {
				if ref := e.w.ConsolePorts[i].ConnectedEndpoints[j].Device; ref != nil && ref.ID == dev {
					ref.Name = d.Name
				}
			}
		}
	case "dev-del":
		for _, id := range shapeIfacesOf(e.w, dev) {
			e.deleteIface(d, id)
		}
		e.w.ConsolePorts = slices.DeleteFunc(e.w.ConsolePorts, func(p netbox.ConsolePort) bool { return p.Device != nil && p.Device.ID == dev })
		site := d.Site.Slug
		e.w.Devices[site] = slices.DeleteFunc(e.w.Devices[site], func(x netbox.Device) bool { return x.ID == dev })
	case "console-add":
		// A (possibly second) console port, cabled to a console-server port
		// on some device of the site, or left dangling.
		others := e.w.Devices[cfg.NetBoxSite]
		srv := rapid.SampledFrom(others).Draw(e.rt, "server")
		p := netbox.ConsolePort{ID: e.id(), Name: fmt.Sprintf("con%d", e.next), Device: &netbox.NestedRef{ID: dev}}
		switch rapid.IntRange(0, 4).Draw(e.rt, "cable") {
		case 0: // uncabled
		case 1: // cabled to something that is not a console server
			p.ConnectedEndpointsType, p.ConnectedEndpointsReachable = "dcim.frontport", true
			p.ConnectedEndpoints = []netbox.ConsolePortEndpoint{{ID: e.id(), Name: "1", Device: &netbox.NestedRef{ID: srv.ID, Name: srv.Name}}}
		case 2: // incomplete path
			p.ConnectedEndpointsType = "dcim.consoleserverport"
			p.ConnectedEndpoints = []netbox.ConsolePortEndpoint{{ID: e.id(), Name: "ttyS1", Device: &netbox.NestedRef{ID: srv.ID, Name: srv.Name}}}
		default:
			p.ConnectedEndpointsType, p.ConnectedEndpointsReachable = "dcim.consoleserverport", true
			p.ConnectedEndpoints = []netbox.ConsolePortEndpoint{{ID: e.id(), Name: "ttyS1", Device: &netbox.NestedRef{ID: srv.ID, Name: srv.Name}}}
		}
		e.w.ConsolePorts = append(e.w.ConsolePorts, p)
	case "console-uncable":
		changed := false
		for i := range e.w.ConsolePorts {
			if p := &e.w.ConsolePorts[i]; p.Device != nil && p.Device.ID == dev {
				p.ConnectedEndpoints, p.ConnectedEndpointsType, p.ConnectedEndpointsReachable = nil, "", false
				changed = true
			}
		}
		if !changed {
			return ""
		}
	default:
		panic("unknown edit kind " + kind)
	}
	return fmt.Sprintf("%s: device %d", kind, dev)
}

// clearRefs drops a device's primary_ip4 / oob_ip references to a deleted
// address, as NetBox does.
func (e *shapeEditor) clearRefs(d *netbox.Device, ipID int) {
	if d.PrimaryIP4 != nil && d.PrimaryIP4.ID == ipID {
		d.PrimaryIP4 = nil
	}
	if d.OOBIP != nil && d.OOBIP.ID == ipID {
		d.OOBIP = nil
	}
}

func (e *shapeEditor) deleteIface(d *netbox.Device, id int) {
	for _, ip := range e.w.IPs {
		if ip.AssignedObjectID != nil && *ip.AssignedObjectID == id {
			e.clearRefs(d, ip.ID)
		}
	}
	e.w.IPs = slices.DeleteFunc(e.w.IPs, func(ip netbox.IPAddress) bool { return ip.AssignedObjectID != nil && *ip.AssignedObjectID == id })
	e.w.Interfaces = slices.DeleteFunc(e.w.Interfaces, func(i netbox.Interface) bool { return i.ID == id })
	for i := range e.w.Interfaces {
		if l := e.w.Interfaces[i].LAG; l != nil && l.ID == id {
			e.w.Interfaces[i].LAG = nil
		}
	}
}

// shapeFootprint is everything about a device that another part of a
// snapshot may legitimately depend on: its MACs, its addresses, its name.
type shapeFootprint struct {
	macs, addrs map[string]bool
	names       map[string]bool
}

func (f *shapeFootprint) add(w *shapeworld.World, dev int) {
	if f.macs == nil {
		f.macs, f.addrs, f.names = map[string]bool{}, map[string]bool{}, map[string]bool{}
	}
	if d := shapeDevice(w, dev); d != nil {
		f.names[d.Name] = true
	}
	for _, id := range shapeIfacesOf(w, dev) {
		ifc := shapeIface(w, id)
		if mac := shapeMAC(*ifc); mac != "" {
			f.macs[mac] = true
		}
		for _, ref := range ifc.MACAddresses {
			f.macs[strings.ToLower(ref.MACAddress)] = true
		}
		for _, i := range shapeIPsOf(w, id) {
			if a, _, ok := shapeAddr(w.IPs[i].Address); ok {
				f.addrs[a.String()] = true
			}
		}
	}
}

// shapeBlastRadius checks that the difference between two snapshots of one
// config stays within one device: every other machine's facts are unchanged
// (apart from a console line that names the device as its console server),
// the segment configuration is unchanged, and reservations and relay pools
// that do not involve the device's MACs or addresses are unchanged.
func shapeBlastRadius(before, after model.SiteSnapshot, dev int, fp shapeFootprint) string {
	a, b := shapeProject(before), shapeProject(after)
	if a.DHCP != b.DHCP {
		return fmt.Sprintf("fabric DHCP config changed: %s -> %s", a.DHCP, b.DHCP)
	}
	if a.OOB != b.OOB {
		return fmt.Sprintf("OOB config changed: %s -> %s", a.OOB, b.OOB)
	}
	ownMachine := map[string]bool{}
	for _, s := range []model.SiteSnapshot{before, after} {
		for _, ms := range s.Machines {
			if ms.NetBoxDeviceID != nil && *ms.NetBoxDeviceID == dev {
				ownMachine[ms.ID.String()] = true
			}
		}
	}
	for id, fa := range a.Machines {
		if ownMachine[id] {
			continue
		}
		fb, ok := b.Machines[id]
		if !ok {
			return fmt.Sprintf("machine %s of another device disappeared", id)
		}
		if fa.Console != fb.Console {
			server, _, _ := strings.Cut(fa.Console, "|")
			server2, _, _ := strings.Cut(fb.Console, "|")
			if fp.names[server] || fp.names[server2] {
				fa.Console, fb.Console = "", ""
			}
		}
		if fmt.Sprintf("%+v", fa) != fmt.Sprintf("%+v", fb) {
			return fmt.Sprintf("machine %s of another device changed:\n before %+v\n after  %+v", id, fa, fb)
		}
	}
	for id := range b.Machines {
		if _, ok := a.Machines[id]; !ok && !ownMachine[id] {
			return fmt.Sprintf("machine %s of another device appeared", id)
		}
	}
	foreign := func(rs []string) []string {
		var out []string
		for _, r := range rs {
			mac, ip, _ := strings.Cut(r, "=")
			if !fp.macs[mac] && !fp.addrs[ip] {
				out = append(out, r)
			}
		}
		return out
	}
	if x, y := foreign(a.Reservations), foreign(b.Reservations); fmt.Sprint(x) != fmt.Sprint(y) {
		return fmt.Sprintf("reservations of other devices changed: %d -> %d entries", len(x), len(y))
	}
	foreignPools := func(ps []string) []string {
		var out []string
		for _, p := range ps {
			parts := strings.Split(p, "|")
			pfx, err := netip.ParsePrefix(parts[0])
			touched := false
			if err == nil {
				for addr := range fp.addrs {
					if a, err := netip.ParseAddr(addr); err == nil && pfx.Contains(a) {
						touched = true
					}
				}
			}
			if !touched {
				out = append(out, p)
			}
		}
		return out
	}
	if x, y := foreignPools(a.RelayPools), foreignPools(b.RelayPools); fmt.Sprint(x) != fmt.Sprint(y) {
		return fmt.Sprintf("relay pools of other links changed: %v -> %v", x, y)
	}
	return ""
}
