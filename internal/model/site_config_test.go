package model

import (
	"strings"
	"testing"
)

func TestSiteConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     SiteConfig
		wantErr bool
	}{
		{"valid", SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}, false},
		{"empty slug", SiteConfig{Slug: ""}, true},
		{"missing netbox_site", SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}, true},
		{"location scoped valid", SiteConfig{Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "pop2-01-005330", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"}, false},
		{"bad pool order", SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.200", EphemeralPoolEnd: "10.0.0.100"}, true},
		{"non-ip pool", SiteConfig{Slug: "pop3", NetBoxSite: "pop3", EphemeralPoolStart: "not-an-ip", EphemeralPoolEnd: "10.0.0.200"}, true},
		// IPv6 is not supported (OOB networks are IPv4). Accepting IPv6
		// addresses would silently bypass the start <= end check because
		// the naive uint32 comparison only works for v4.
		{"ipv6 pool", SiteConfig{Slug: "pop3", EphemeralPoolStart: "fd00::1", EphemeralPoolEnd: "fd00::ff"}, true},
		{"mixed pool", SiteConfig{Slug: "pop3", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "fd00::1"}, true},
		{"valid relay pool", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.100", RangeEnd: "10.254.27.150", Gateway: "10.254.27.1"}}}, false},
		{"relay pool bad subnet", SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "not-a-cidr", RangeStart: "10.254.27.100", RangeEnd: "10.254.27.150", Gateway: "10.254.27.1"}}}, true},
		{"relay pool range outside subnet", SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.28.100", RangeEnd: "10.254.28.150", Gateway: "10.254.27.1"}}}, true},
		{"relay pool gateway outside subnet", SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.100", RangeEnd: "10.254.27.150", Gateway: "10.254.26.1"}}}, true},
		{"relay pool reversed range", SiteConfig{Slug: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.150", RangeEnd: "10.254.27.100", Gateway: "10.254.27.1"}}}, true},
		// The gateway is the router (leaf VRRP VIP) handed to relayed
		// clients. The DHCP handler allocates upward from range_start and
		// its ARP probe cannot see a relayed subnet, so a gateway inside the
		// range is offered to a client as that client's own address.
		{"relay pool gateway at range start", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.1", RangeEnd: "10.254.27.150", Gateway: "10.254.27.1"}}}, true},
		{"relay pool gateway mid range", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.100", RangeEnd: "10.254.27.150", Gateway: "10.254.27.120"}}}, true},
		{"relay pool gateway at range end", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.100", RangeEnd: "10.254.27.150", Gateway: "10.254.27.150"}}}, true},
		// A subnet of /30 or wider has a network and a broadcast address,
		// and neither is a usable host address to lease.
		{"relay pool range includes network address", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.0", RangeEnd: "10.254.27.50", Gateway: "10.254.27.254"}}}, true},
		{"relay pool range includes broadcast address", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.200", RangeEnd: "10.254.27.255", Gateway: "10.254.27.1"}}}, true},
		{"relay pool range includes network address of a /16", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.0.0/16", RangeStart: "10.254.0.0", RangeEnd: "10.254.0.50", Gateway: "10.254.255.254"}}}, true},
		{"relay pool range includes broadcast address of a /22", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.24.0/22", RangeStart: "10.254.27.200", RangeEnd: "10.254.27.255", Gateway: "10.254.24.1"}}}, true},
		{"relay pool range includes broadcast address of a /30", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/30", RangeStart: "10.254.27.2", RangeEnd: "10.254.27.3", Gateway: "10.254.27.1"}}}, true},
		// Host addresses on either side of the network and broadcast
		// addresses stay valid; .255 is a host address inside a /22.
		{"relay pool range spans every host but the gateway", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/24", RangeStart: "10.254.27.2", RangeEnd: "10.254.27.254", Gateway: "10.254.27.1"}}}, false},
		{"relay pool range ends at a non-broadcast .255", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.24.0/22", RangeStart: "10.254.25.200", RangeEnd: "10.254.25.255", Gateway: "10.254.24.1"}}}, false},
		{"relay pool /30 host range", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.254.27.0/30", RangeStart: "10.254.27.2", RangeEnd: "10.254.27.2", Gateway: "10.254.27.1"}}}, false},
		// A /31 (RFC 3021) has no network or broadcast address: both ends
		// are hosts, as in the per-leg pools the snapshot builder generates.
		{"relay pool /31 lower end", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.253.28.52/31", RangeStart: "10.253.28.52", RangeEnd: "10.253.28.52", Gateway: "10.253.28.53"}}}, false},
		{"relay pool /31 upper end", SiteConfig{Slug: "pop1", NetBoxSite: "pop1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			RelayPools: []RelayPool{{Subnet: "10.253.28.52/31", RangeStart: "10.253.28.53", RangeEnd: "10.253.28.53", Gateway: "10.253.28.52"}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("got %v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

// A relay pool feeds a DHCPv4 scope, so its subnet must be an IPv4 CIDR.
// IPv4-mapped IPv6 notation is the dangerous case: net.ParseCIDR returns a
// 16-byte mask for it, the IPv4 range and gateway still pass Contains, and
// the DHCP handler truncates that mask to its first four bytes, telling
// clients their netmask is 255.255.255.255. Plain IPv6 would also fail the
// range containment check; the error must name the subnet family instead.
func TestRelayPoolValidateRejectsNonIPv4Subnet(t *testing.T) {
	for _, subnet := range []string{
		"::ffff:10.254.27.0/120",
		"::ffff:0:0/96",
		"2001:db8::/64",
	} {
		t.Run(subnet, func(t *testing.T) {
			p := RelayPool{
				Subnet:     subnet,
				RangeStart: "10.254.27.100",
				RangeEnd:   "10.254.27.150",
				Gateway:    "10.254.27.1",
			}
			err := p.Validate()
			if err == nil {
				t.Fatalf("Validate accepted non-IPv4 subnet %q", subnet)
			}
			if !strings.Contains(err.Error(), "must be an IPv4 CIDR") {
				t.Fatalf("Validate rejected %q for the wrong reason: %v", subnet, err)
			}
		})
	}
}
