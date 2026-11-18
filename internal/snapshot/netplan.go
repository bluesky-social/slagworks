package snapshot

import (
	"net"
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
// Only host-side /31s produce pools; the leaf-side addresses live on leaf
// devices, which have no Machine row and never reach this path's callers.
// Filtering by parity here would couple us to the layout formula, so each
// /31 instead produces a pool keyed by its own subnet — duplicates
// collapse by construction because both sides share the subnet and the
// host side is derived, not read.
func l3LegRelayPools(ifaces []netbox.Interface, ips []netbox.IPAddress) []model.RelayPool {
	ifaceByID := make(map[int]*netbox.Interface, len(ifaces))
	for i := range ifaces {
		ifaceByID[ifaces[i].ID] = &ifaces[i]
	}
	seen := make(map[string]bool)
	var pools []model.RelayPool
	for _, ip := range ips {
		if !strings.HasSuffix(ip.Address, "/31") || ip.AssignedObjectID == nil || !ip.IsActive() {
			continue
		}
		iface, ok := ifaceByID[*ip.AssignedObjectID]
		if !ok || classifyInterface(*iface) != "physical" {
			continue
		}
		hostAddr, _, err := net.ParseCIDR(ip.Address)
		if err != nil {
			continue
		}
		leafSide := leafSideOf31(ip.Address)
		if leafSide == "" {
			continue
		}
		// Normalize to the /31 network for the pool subnet.
		_, ipnet, _ := net.ParseCIDR(ip.Address)
		subnet := ipnet.String()
		if seen[subnet] {
			continue
		}
		seen[subnet] = true
		pools = append(pools, model.RelayPool{
			Subnet:     subnet,
			RangeStart: hostAddr.String(),
			RangeEnd:   hostAddr.String(),
			Gateway:    leafSide,
		})
	}
	return pools
}
