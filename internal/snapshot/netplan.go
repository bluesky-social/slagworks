package snapshot

import (
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// This file holds slagworks' own DHCP-service config for the L3 host
// fabric: the per-host /31 relay pools the site worker uses to answer
// PXE DISCOVERs relayed by the leaves. This is "serve the host's boot",
// not "configure the host" — host network config (netplan + FRR) moved to
// pluggable network-config scripts (docs/dev/extensibility.md),
// but relay-pool generation stays in Go because the host isn't up yet at
// PXE time and the shape is detected generically (any /31 on a device
// interface → a single-address pool), not burned in to pop1.

// leafSideOf31 computes the other address of a /31 pair: the leaf side
// for a host-side address. Computed generically by flipping the last bit
// so it doesn't depend on the host-odd/leaf-even parity convention.
// Returns "" if the input isn't a parseable /31 IPv4 CIDR.
func leafSideOf31(hostCIDR string) string {
	ip, ipnet, err := net.ParseCIDR(hostCIDR)
	if err != nil {
		return ""
	}
	if ones, bits := ipnet.Mask.Size(); ones != 31 || bits != 32 {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	peer := make(net.IP, 4)
	copy(peer, v4)
	peer[3] ^= 1
	return peer.String()
}

// l3LegRelayPools generates one single-address relay pool per host /31
// leg, from the NetBox interface + IP records. The leaf's dhcp-helper
// relays PXE traffic with giaddr = the leaf side of the ingress /31;
// matching that giaddr to a pool whose subnet is the /31 itself yields
// exactly one offerable address: the host side. Gateway = the leaf side
// (it IS the host's next hop on that link).
//
// The records cover every scoped device, not only hosts, so a leaf
// modeled in the same site contributes the leaf end of a /31 alongside
// the host end. Filtering by parity would couple us to the layout
// formula, so each /31 instead produces one pool keyed by its own subnet,
// and both ends collapse into it. When only one end is present it is
// taken as the host side. When both are present the records do not say
// which is the host, so the higher address is kept: the host under the
// usual host-odd layout, and the same answer whichever record NetBox
// listed first.
//
// The result is sorted by subnet. NetBox's list order shifts when
// unrelated objects change, and the snapshot content hash covers these
// pools, so an order-dependent result would push a new snapshot for data
// that did not change. The worker derives both ends of any /31 pool from
// giaddr (dhcp.relayPoolForRequest), so the host-side choice changes what
// the snapshot records, never a DHCP answer.
func l3LegRelayPools(ifaces []netbox.Interface, ips []netbox.IPAddress) []model.RelayPool {
	ifaceByID := make(map[int]*netbox.Interface, len(ifaces))
	for i := range ifaces {
		ifaceByID[ifaces[i].ID] = &ifaces[i]
	}
	hostByLink := make(map[netip.Prefix]netip.Addr)
	for _, ip := range ips {
		if !strings.HasSuffix(ip.Address, "/31") || ip.AssignedObjectID == nil || !ip.IsActive() {
			continue
		}
		iface, ok := ifaceByID[*ip.AssignedObjectID]
		if !ok || classifyInterface(*iface) != "physical" {
			continue
		}
		p, err := netip.ParsePrefix(ip.Address)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		link := p.Masked()
		if prev, ok := hostByLink[link]; !ok || p.Addr().Compare(prev) > 0 {
			hostByLink[link] = p.Addr()
		}
	}
	links := slices.SortedFunc(maps.Keys(hostByLink), func(a, b netip.Prefix) int {
		return a.Addr().Compare(b.Addr())
	})
	pools := make([]model.RelayPool, 0, len(links))
	for _, link := range links {
		host := hostByLink[link]
		pools = append(pools, model.RelayPool{
			Subnet:     link.String(),
			RangeStart: host.String(),
			RangeEnd:   host.String(),
			Gateway:    leafSideOf31(netip.PrefixFrom(host, 31).String()),
		})
	}
	return pools
}
