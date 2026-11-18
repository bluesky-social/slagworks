package main

import (
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
)

// oobBind is the host-local OOB DHCP bind: the interface whose address
// falls inside the OOB subnet, and that address (the serverIP slagworks
// advertises as next-server / iPXE-URL host on the OOB segment). Derived
// from NetBox (the OOB subnet) ∩ the host's own interfaces — no per-host
// env config.
type oobBind struct {
	iface    string
	serverIP net.IP
}

// deriveOOBBind finds the local interface whose IPv4 address is inside the
// OOB subnet. That interface is where the OOB DHCP socket binds, and its
// address is the OOB serverIP. Returns ok=false when no local interface is
// on the OOB subnet (the common case on a host that isn't an oob-gw, or
// before the OOB NIC is addressed) — the caller then skips OOB binding,
// which is the correct degenerate behavior.
//
// ifaceAddrs is injected (defaults to net.InterfaceAddrs-equivalent via
// listLocalIface) so the matching logic is unit-testable without real NICs.
func deriveOOBBind(oobSubnet string, ifaces []localIface) (oobBind, bool, error) {
	_, subnet, err := net.ParseCIDR(oobSubnet)
	if err != nil {
		return oobBind{}, false, fmt.Errorf("parse OOB subnet %q: %w", oobSubnet, err)
	}
	for _, ifc := range ifaces {
		for _, a := range ifc.addrs {
			ip := a
			if v4 := ip.To4(); v4 != nil && subnet.Contains(v4) {
				return oobBind{iface: ifc.name, serverIP: v4}, true, nil
			}
		}
	}
	return oobBind{}, false, nil
}

// localIface is the minimal interface view deriveOOBBind needs: a name and
// its IPv4 addresses. Pulled out so tests can construct fixtures.
type localIface struct {
	name  string
	addrs []net.IP
}

// listLocalIfaces enumerates the host's interfaces and their IPv4 addresses
// for deriveOOBBind. Skips down interfaces and loopback.
func listLocalIfaces() ([]localIface, error) {
	netIfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	out := make([]localIface, 0, len(netIfaces))
	for _, ni := range netIfaces {
		if ni.Flags&net.FlagUp == 0 || ni.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ni.Addrs()
		if err != nil {
			continue
		}
		li := localIface{name: ni.Name}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok {
				if v4 := ipnet.IP.To4(); v4 != nil {
					li.addrs = append(li.addrs, v4)
				}
			}
		}
		if len(li.addrs) > 0 {
			out = append(out, li)
		}
	}
	return out, nil
}

// oobIfaceParams builds the dhcp.IfaceParams for the OOB segment from the
// snapshot's OOBConfig and the derived serverIP. The OOB interface is
// promiscuous (the fleet is mostly non-machine devices), serves the full
// reservation set (via the shared ReserveIP), and offers from the dynamic
// pool to unknowns. ARPProbe is bound to the OOB interface so dynamic-
// allocation collision probes egress the right NIC. Returns ok=false when
// the OOBConfig is incomplete (no subnet/gateway) — the caller skips OOB.
func oobIfaceParams(oob model.OOBConfig, serverIP net.IP, iface string, arpTimeout time.Duration, logger *slog.Logger) (dhcp.IfaceParams, bool, error) {
	if oob.Subnet == "" || oob.Gateway == "" {
		return dhcp.IfaceParams{}, false, nil
	}
	_, subnet, err := net.ParseCIDR(oob.Subnet)
	if err != nil {
		return dhcp.IfaceParams{}, false, fmt.Errorf("parse OOB subnet %q: %w", oob.Subnet, err)
	}
	gw := net.ParseIP(oob.Gateway)
	if gw == nil {
		return dhcp.IfaceParams{}, false, fmt.Errorf("invalid OOB gateway %q", oob.Gateway)
	}

	p := dhcp.IfaceParams{
		ServerIP: serverIP,
		DNS:      parseIPs(oob.DNS),
		NTP:      parseIPs(oob.NTP),
		PoolConfig: dhcp.PoolConfig{
			Subnet:      *subnet,
			Gateway:     gw,
			Promiscuous: true,
		},
	}
	if oob.LeaseSeconds > 0 {
		p.LeaseDuration = time.Duration(oob.LeaseSeconds) * time.Second
	}

	// Split-pool: this gateway's dynamic slice is the one whose Owner equals
	// our serverIP. We originate fresh leases only from that slice (set as
	// RangeStart/End). The FULL set of dynamic slices is also carried so the
	// handler can tell "a peer's slice" (NAK on a broadcast request) from
	// "outside the OOB subnet entirely". A gateway with no matching slice
	// serves reservations only (valid — e.g. a 3rd gateway with no pool).
	for _, s := range oob.DynamicSlices {
		ds := dhcp.DynamicSlice{Start: net.ParseIP(s.Start), End: net.ParseIP(s.End)}
		if ds.Start == nil || ds.End == nil {
			return dhcp.IfaceParams{}, false, fmt.Errorf("invalid OOB dynamic slice %q-%q", s.Start, s.End)
		}
		p.DynamicSlices = append(p.DynamicSlices, ds)
		if s.Owner == serverIP.String() {
			p.RangeStart = ds.Start
			p.RangeEnd = ds.End
		}
	}

	// ARP prober bound to the OOB interface, mirroring the fabric leg's
	// prober. Skipped (nil) when we can't resolve the interface — the
	// allocator then doesn't probe, which is safe (it just can't detect a
	// squatter).
	if ni, err := net.InterfaceByName(iface); err == nil {
		p.ARPProbe = dhcp.NewARPProber(ni, arpTimeout, logger)
	}
	return p, true, nil
}

// oobCollidesWithFabric reports whether the derived OOB bind shares an
// interface OR a serverIP with the fabric leg. The multi-interface model
// requires them distinct: the fabric uses the handler's base config and the
// OOB uses per-interface params keyed by interface, so the same interface
// can't carry both, and TFTP/HTTP can't double-bind one serverIP. True =>
// the caller must refuse the OOB bind (a misconfiguration; in the real
// topology fabric=leaf4 P2P and OOB=eno3 never coincide).
func oobCollidesWithFabric(bind oobBind, fabricIface, fabricServerIP string) bool {
	return bind.iface == fabricIface || bind.serverIP.String() == fabricServerIP
}

// parseIPs maps a slice of dotted-quad strings to net.IP, dropping any that
// don't parse. Used for the OOB DNS/NTP option lists.
func parseIPs(ss []string) []net.IP {
	if len(ss) == 0 {
		return nil
	}
	out := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		if ip := net.ParseIP(s); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
