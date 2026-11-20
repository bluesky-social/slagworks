package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

const (
	fabricPoolStart = "10.0.7.100"
	fabricPoolEnd   = "10.0.7.160"
	// What a snapshot serves when 10.0.4.0/22, the prefix holding the
	// pool, is the DHCP prefix.
	fabricPoolPrefixDHCP = "subnet 10.0.4.0/22 gateway 10.0.7.254 dns [10.0.9.10] lease 600"
)

// dhcpPrefixCF is a production prefix's custom fields: the gateway, DNS
// servers and lease time the fabric DHCP server hands out when the prefix is
// the DHCP prefix.
func dhcpPrefixCF(gw, dns string, lease int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"gateway_ip":%q,"dns_servers":%q,"dhcp_lease_seconds":%d}`, gw, dns, lease))
}

func dhcpPrefixAt(id int, cidr, scopeType string, scopeID int, cf json.RawMessage) netbox.Prefix {
	return netbox.Prefix{ID: id, Prefix: cidr, ScopeType: scopeType, ScopeID: scopeID, CustomFields: cf}
}

// fabricDHCP renders the fields of a snapshot's DHCP config that come from
// the DHCP prefix.
func fabricDHCP(d model.DHCPConfig) string {
	return fmt.Sprintf("subnet %s gateway %s dns %v lease %d", d.Subnet, d.Gateway, d.DNS, d.LeaseSeconds)
}

// TestBuildFabricDHCPPrefixIsTheOneHoldingTheEphemeralPool: the fabric DHCP
// subnet, gateway, DNS servers and lease time come from the
// slagworks-production prefix the config's ephemeral pool lies in, whatever
// order NetBox lists the scope's production prefixes in.
//
// Tagging a prefix slagworks-production is also how a host interface in it
// gets a gateway fact (docs/user/hardware/dell-r640-idrac.md), so a site or
// location can carry more than one. NetBox lists prefixes by address, so
// Build used to serve the first one tagged that sorted below the DHCP prefix
// as the site's DHCP subnet and gateway while the pool stayed where it was:
// the worker then handed out pool addresses with the other prefix's mask and
// router, and if that prefix had no gateway_ip its pool config stopped
// parsing.
func TestBuildFabricDHCPPrefixIsTheOneHoldingTheEphemeralPool(t *testing.T) {
	const siteID, ownLoc, childLoc, otherLoc = 1, 10, 11, 20
	site := func(id int, cidr string, cf json.RawMessage) netbox.Prefix {
		return dhcpPrefixAt(id, cidr, "dcim.site", siteID, cf)
	}
	loc := func(id int, cidr string, locID int, cf json.RawMessage) netbox.Prefix {
		return dhcpPrefixAt(id, cidr, "dcim.location", locID, cf)
	}
	poolCF := dhcpPrefixCF("10.0.7.254", "10.0.9.10", 600)
	// A second production prefix, numerically lower, tagged so that hosts
	// in it resolve a gateway.
	lowerCF := dhcpPrefixCF("10.0.2.1", "10.0.2.53", 60)

	wholeSite := model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a", EphemeralPoolStart: fabricPoolStart, EphemeralPoolEnd: fabricPoolEnd}
	cage := wholeSite
	cage.Slug, cage.NetBoxLocation = "site-a-cage", "cage-a"

	cases := []struct {
		name     string
		cfg      model.SiteConfig
		prefixes []netbox.Prefix // in the order the dispatcher passes them
		want     string
	}{
		{
			name:     "whole site, pool prefix listed first",
			cfg:      wholeSite,
			prefixes: []netbox.Prefix{site(1, "10.0.4.0/22", poolCF), site(2, "10.0.2.0/24", lowerCF)},
			want:     fabricPoolPrefixDHCP,
		},
		{
			name:     "whole site, lower prefix listed first as NetBox lists by address",
			cfg:      wholeSite,
			prefixes: []netbox.Prefix{site(2, "10.0.2.0/24", lowerCF), site(1, "10.0.4.0/22", poolCF)},
			want:     fabricPoolPrefixDHCP,
		},
		{
			// A site-wide supernet tagged for its gateway also holds the
			// pool; the most specific prefix holding it wins.
			name:     "whole site, an enclosing prefix also holds the pool",
			cfg:      wholeSite,
			prefixes: []netbox.Prefix{site(3, "10.0.0.0/16", dhcpPrefixCF("10.0.0.1", "", 0)), site(2, "10.0.2.0/24", lowerCF), site(1, "10.0.4.0/22", poolCF)},
			want:     fabricPoolPrefixDHCP,
		},
		{
			// NetBox's ?site= filter also returns prefixes scoped to a
			// location under the site, in the same address order.
			name:     "whole site, a location-scoped prefix listed first",
			cfg:      wholeSite,
			prefixes: []netbox.Prefix{loc(2, "10.0.2.0/24", otherLoc, lowerCF), site(1, "10.0.4.0/22", poolCF)},
			want:     fabricPoolPrefixDHCP,
		},
		{
			name:     "location, own location's lower prefix listed first",
			cfg:      cage,
			prefixes: []netbox.Prefix{loc(2, "10.0.2.0/24", ownLoc, lowerCF), loc(1, "10.0.4.0/22", ownLoc, poolCF)},
			want:     fabricPoolPrefixDHCP,
		},
		{
			// The config's own location supplies the DHCP prefix even when
			// a child location's more specific prefix also holds the pool.
			name: "location, a child location's prefix also holds the pool",
			cfg:  cage,
			prefixes: []netbox.Prefix{
				loc(2, "10.0.2.0/24", ownLoc, lowerCF), loc(1, "10.0.4.0/22", ownLoc, poolCF),
				loc(3, "10.0.7.0/24", childLoc, dhcpPrefixCF("10.0.7.1", "10.0.7.53", 30)),
			},
			want: fabricPoolPrefixDHCP,
		},
		{
			// An inconsistent config (the pool is in no production
			// prefix) keeps the first one listed, NetBox's lowest address,
			// and logs a warning.
			name:     "pool in no prefix",
			cfg:      wholeSite,
			prefixes: []netbox.Prefix{site(2, "10.0.2.0/24", lowerCF), site(4, "10.0.3.0/24", dhcpPrefixCF("10.0.3.1", "", 0))},
			want:     "subnet 10.0.2.0/24 gateway 10.0.2.1 dns [10.0.2.53] lease 60",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap, err := Build(Inputs{Site: netbox.Site{ID: siteID, Slug: "site-a"}, Config: c.cfg, Prefixes: c.prefixes})
			if err != nil {
				t.Fatal(err)
			}
			if got := fabricDHCP(snap.DHCP); got != c.want {
				t.Errorf("fabric DHCP is %s, want %s (ephemeral pool %s-%s)", got, c.want, fabricPoolStart, fabricPoolEnd)
			}
		})
	}
}

// TestDispatcherFabricDHCPPrefixIsTheOneHoldingTheEphemeralPool: the same
// through the dispatcher, with the fake NetBox listing each scope's
// production prefixes by address as NetBox does: the lower prefix first.
func TestDispatcherFabricDHCPPrefixIsTheOneHoldingTheEphemeralPool(t *testing.T) {
	lower := func(scopeType string, scopeID int) netbox.Prefix {
		return dhcpPrefixAt(2, "10.0.2.0/24", scopeType, scopeID, dhcpPrefixCF("10.0.2.1", "10.0.2.53", 60))
	}
	pool := func(scopeType string, scopeID int) netbox.Prefix {
		return dhcpPrefixAt(1, "10.0.4.0/22", scopeType, scopeID, dhcpPrefixCF("10.0.7.254", "10.0.9.10", 600))
	}
	sites := map[string]netbox.Site{"site-a": {ID: 1, Slug: "site-a", Name: "Site A"}}
	cases := []struct {
		name string
		cfg  model.SiteConfig
		nb   *fakeNetBox
	}{
		{
			name: "whole site",
			cfg:  model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a", EphemeralPoolStart: fabricPoolStart, EphemeralPoolEnd: fabricPoolEnd},
			nb:   &fakeNetBox{sites: sites, prefixes: []netbox.Prefix{lower("dcim.site", 1), pool("dcim.site", 1)}},
		},
		{
			name: "location",
			cfg: model.SiteConfig{Slug: "site-a-cage", NetBoxSite: "site-a", NetBoxLocation: "cage-a",
				EphemeralPoolStart: fabricPoolStart, EphemeralPoolEnd: fabricPoolEnd},
			nb: &fakeNetBox{
				sites:            sites,
				locations:        []netbox.Location{{ID: 10, Slug: "cage-a"}},
				locationPrefixes: map[int][]netbox.Prefix{10: {lower("dcim.location", 10), pool("dcim.location", 10)}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snap, err := NewDispatcher(c.nb, newFakeStore(t, c.cfg), &countingSink{}).BuildSnapshot(context.Background(), c.cfg.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if got := fabricDHCP(snap.DHCP); got != fabricPoolPrefixDHCP {
				t.Errorf("fabric DHCP is %s, want %s (the prefix holding the ephemeral pool %s-%s)", got, fabricPoolPrefixDHCP, fabricPoolStart, fabricPoolEnd)
			}
		})
	}
}
