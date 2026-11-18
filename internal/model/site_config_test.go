package model

import "testing"

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
