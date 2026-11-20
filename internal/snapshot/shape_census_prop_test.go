package snapshot

import (
	"fmt"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

// shapeCensusWorld is a small fixed world: site-a with one whole-site config,
// site-b with two cages, each its own config. Every config builds.
func shapeCensusWorld() (*shapeworld.World, *shapeworld.StoreData) {
	w := shapeworld.New()
	sd := &shapeworld.StoreData{}
	ip := func(id, iface int, addr string) netbox.IPAddress {
		return netbox.IPAddress{ID: id, Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &iface,
			Status: &netbox.NestedValue{Value: "active"}}
	}
	tag := func(s string) []netbox.NestedTag { return []netbox.NestedTag{{Slug: s}} }

	w.Sites["site-a"] = netbox.Site{ID: 1, Name: "Site A", Slug: "site-a"}
	w.SitePrefixes[shapeworld.SiteTagKey("site-a", shapeTagProd)] = []netbox.Prefix{
		{ID: 10, Prefix: "10.1.4.0/22", ScopeType: "dcim.site", ScopeID: 1, Tags: tag(shapeTagProd), CustomFields: shapeCF(map[string]any{"gateway_ip": "10.1.7.254"})}}
	w.SitePrefixes[shapeworld.SiteTagKey("site-a", shapeTagOOB)] = []netbox.Prefix{
		{ID: 11, Prefix: "10.1.12.0/23", ScopeType: "dcim.site", ScopeID: 1, Tags: tag(shapeTagOOB), CustomFields: shapeCF(map[string]any{"gateway_ip": "10.1.13.254"})}}
	w.Ranges[shapeworld.RangeKey(shapeRoleRange, "10.1.12.0/23")] = []netbox.IPRange{
		{ID: 12, StartAddress: "10.1.13.100/23", EndAddress: "10.1.13.149/23", CustomFields: shapeCF(map[string]any{"slagworks_oob_owner": "10.1.12.11"})}}
	w.DeviceTypes[20] = netbox.DeviceType{ID: 20, Slug: "srv-a", Model: "SRV-A"}
	siteA := &netbox.NestedRef{ID: 1, Slug: "site-a"}
	dt := &netbox.NestedRef{ID: 20, Slug: "srv-a"}
	w.Devices["site-a"] = []netbox.Device{
		{ID: 100, Name: "oobgw-1.site-a", Serial: "SN100", Site: siteA, DeviceType: dt},
		{ID: 101, Name: "node-1.site-a", Serial: "SN101", Site: siteA, DeviceType: dt},
	}
	w.Interfaces = []netbox.Interface{
		{ID: 200, Name: "eno3", MACAddress: "02:00:00:00:02:00", Device: &netbox.NestedRef{ID: 100}, Type: &netbox.NestedValue{Value: "1000base-t"}},
		{ID: 201, Name: "ipmi", MACAddress: "02:00:00:00:02:01", MgmtOnly: true, Device: &netbox.NestedRef{ID: 101}, Type: &netbox.NestedValue{Value: "1000base-t"}},
		{ID: 202, Name: "eno1", MACAddress: "02:00:00:00:02:02", Device: &netbox.NestedRef{ID: 101}, Type: &netbox.NestedValue{Value: "1000base-t"}},
	}
	w.IPs = []netbox.IPAddress{ip(300, 200, "10.1.12.11/23"), ip(301, 201, "10.1.12.20/23")}
	sd.Configs = append(sd.Configs, model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a", EphemeralPoolStart: "10.1.7.100", EphemeralPoolEnd: "10.1.7.160"})

	w.Sites["site-b"] = netbox.Site{ID: 2, Name: "Site B", Slug: "site-b"}
	w.Locations["site-b"] = []netbox.Location{
		{ID: 50, Name: "cage-1", Slug: "cage-1", Site: &netbox.NestedRef{ID: 2, Slug: "site-b"}},
		{ID: 51, Name: "cage-2", Slug: "cage-2", Site: &netbox.NestedRef{ID: 2, Slug: "site-b"}},
	}
	for i, loc := range []int{50, 51} {
		w.LocPrefixes[shapeworld.LocTagKey(loc, shapeTagProd)] = []netbox.Prefix{
			{ID: 13 + i, Prefix: []string{"10.2.4.0/22", "10.3.4.0/22"}[i], ScopeType: "dcim.location", ScopeID: loc, Tags: tag(shapeTagProd),
				CustomFields: shapeCF(map[string]any{"gateway_ip": []string{"10.2.7.254", "10.3.7.254"}[i]})}}
		slug := []string{"cage-1", "cage-2"}[i]
		sd.Configs = append(sd.Configs, model.SiteConfig{Slug: "site-b-" + slug, NetBoxSite: "site-b", NetBoxLocation: slug,
			EphemeralPoolStart: []string{"10.2.7.100", "10.3.7.100"}[i], EphemeralPoolEnd: []string{"10.2.7.160", "10.3.7.160"}[i]})
	}
	return w, sd
}

// TestShapeSiteFatalCensus pins which NetBox conditions cost a whole config
// its snapshot. A config that does not build keeps its worker on the last
// snapshot and makes every commission and deploy preflight in it fail, so
// each row here is a way one NetBox save stops a site from provisioning.
//
// "documented" rows are in docs/ (architecture.md "Builder",
// configuration.md "Site configs"). The others are stated only in code
// comments; they are listed so that the set is explicit and so that a new
// site-fatal condition has to be added here on purpose. Ambiguous OOB data
// (the rows from "OOB dynamic range without" on) costs the MACs or ranges
// involved and never the site.
func TestShapeSiteFatalCensus(t *testing.T) {
	shapeQuiet(t)
	ip := func(id, iface int, addr string) netbox.IPAddress {
		return netbox.IPAddress{ID: id, Address: addr, AssignedObjectType: "dcim.interface", AssignedObjectID: &iface,
			Status: &netbox.NestedValue{Value: "active"}}
	}
	oobKey := shapeworld.SiteTagKey("site-a", shapeTagOOB)
	rangeKey := shapeworld.RangeKey(shapeRoleRange, "10.1.12.0/23")
	rows := []struct {
		name       string
		slug       string
		documented bool
		wantFatal  bool
		edit       func(w *shapeworld.World, sd *shapeworld.StoreData)
	}{
		{name: "baseline", slug: "site-a", edit: func(*shapeworld.World, *shapeworld.StoreData) {}},

		// Site-level objects.
		{name: "no slagworks-production prefix in scope", slug: "site-a", documented: true, wantFatal: true,
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				delete(w.SitePrefixes, shapeworld.SiteTagKey("site-a", shapeTagProd))
			}},
		{name: "NetBox site gone", slug: "site-a", documented: true, wantFatal: true,
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) { delete(w.Sites, "site-a") }},
		{name: "config's location gone from the site", slug: "site-b-cage-1", documented: true, wantFatal: true,
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) { w.Locations["site-b"] = w.Locations["site-b"][1:] }},
		{name: "config's location moved under another config's", slug: "site-b-cage-1", documented: true, wantFatal: true,
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.Locations["site-b"][0].Parent = &netbox.NestedRef{ID: 51}
			}},
		{name: "second slagworks-oob prefix in scope", slug: "site-a", wantFatal: true,
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.SitePrefixes[oobKey] = append(w.SitePrefixes[oobKey], netbox.Prefix{ID: 19, Prefix: "10.1.14.0/23", ScopeType: "dcim.site", ScopeID: 1})
			}},
		{name: "OOB dynamic range without slagworks_oob_owner", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.Ranges[rangeKey] = append(w.Ranges[rangeKey], netbox.IPRange{ID: 18, StartAddress: "10.1.13.150/23", EndAddress: "10.1.13.199/23"})
			}},
		{name: "two OOB dynamic ranges with one owner", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.Ranges[rangeKey] = append(w.Ranges[rangeKey], netbox.IPRange{ID: 18, StartAddress: "10.1.13.150/23", EndAddress: "10.1.13.199/23",
					CustomFields: shapeCF(map[string]any{"slagworks_oob_owner": "10.1.12.11"})})
			}},

		// One device's objects.
		{name: "second OOB address on one interface", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.IPs = append(w.IPs, ip(310, 200, "10.1.13.254/23"))
			}},
		{name: "one MAC on two interfaces with different OOB addresses", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.Interfaces = append(w.Interfaces, netbox.Interface{ID: 210, Name: "eno3.100", MACAddress: "02:00:00:00:02:00", Device: &netbox.NestedRef{ID: 100}})
				w.IPs = append(w.IPs, ip(310, 210, "10.1.12.12/23"))
			}},
		{name: "one OOB address on two MACs", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.IPs = append(w.IPs, ip(310, 202, "10.1.12.11/23"))
			}},

		// Neighbours that must not be fatal.
		{name: "second non-OOB address on an OOB interface", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.IPs = append(w.IPs, ip(310, 200, "10.1.7.254/22"))
			}},
		{name: "second OOB address, status reserved", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				a := ip(310, 200, "10.1.12.99/23")
				a.Status = &netbox.NestedValue{Value: "reserved"}
				w.IPs = append(w.IPs, a)
			}},
		{name: "ten addresses on one interface", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				for i := range 10 {
					w.IPs = append(w.IPs, ip(320+i, 202, fmt.Sprintf("10.1.4.%d/22", 10+i)))
				}
			}},
		{name: "one MAC on twenty interfaces of twenty devices", slug: "site-a",
			edit: func(w *shapeworld.World, sd *shapeworld.StoreData) {
				for i := range 20 {
					dev := 400 + i
					w.Devices["site-a"] = append(w.Devices["site-a"], netbox.Device{ID: dev, Name: fmt.Sprintf("node-%d.site-a", dev), Serial: fmt.Sprintf("SN%d", dev), Site: &netbox.NestedRef{ID: 1, Slug: "site-a"}})
					w.Interfaces = append(w.Interfaces, netbox.Interface{ID: 500 + i, Name: "enx02ffff000001", MACAddress: shapeSharedMAC, Device: &netbox.NestedRef{ID: dev}})
				}
			}},
		{name: "device with no serial, name, type, rack, location or addresses", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.Devices["site-a"] = append(w.Devices["site-a"], netbox.Device{ID: 400, Site: &netbox.NestedRef{ID: 1, Slug: "site-a"}})
			}},
		{name: "production prefix without gateway_ip", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) {
				w.SitePrefixes[shapeworld.SiteTagKey("site-a", shapeTagProd)][0].CustomFields = nil
			}},
		{name: "no slagworks-oob prefix", slug: "site-a",
			edit: func(w *shapeworld.World, _ *shapeworld.StoreData) { delete(w.SitePrefixes, oobKey) }},
	}
	for _, row := range rows {
		w, sd := shapeCensusWorld()
		row.edit(w, sd)
		_, err := shapeBuild(w, sd, row.slug, 0)
		kind := "code comment only"
		if row.documented {
			kind = "documented"
		}
		switch {
		case err != nil && row.wantFatal:
			t.Logf("site-fatal (%s): %s", kind, row.name)
		case err != nil:
			t.Errorf("%s: fails the whole config, and is not listed as site-fatal: %v", row.name, err)
		case row.wantFatal:
			t.Errorf("%s: listed as site-fatal but the config builds; update the census", row.name)
		}
	}
}
