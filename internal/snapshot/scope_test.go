package snapshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Location tree used across these tests:
//
//	cage-a (id 10)            cage-b (id 20)
//	  └─ row-a1 (id 11)
//	       └─ rack-a1a (id 12)
func testLocations() []netbox.Location {
	return []netbox.Location{
		{ID: 10, Slug: "cage-a", Site: &netbox.NestedRef{Slug: "pop2"}},
		{ID: 11, Slug: "row-a1", Site: &netbox.NestedRef{Slug: "pop2"}, Parent: &netbox.NestedRef{ID: 10}},
		{ID: 12, Slug: "rack-a1a", Site: &netbox.NestedRef{Slug: "pop2"}, Parent: &netbox.NestedRef{ID: 11}},
		{ID: 20, Slug: "cage-b", Site: &netbox.NestedRef{Slug: "pop2"}},
	}
}

// reparentedLocations is testLocations after a NetBox operator moves cage-a
// under cage-b within the same site. Every location stays in the site, so a
// config on either cage still resolves, but cage-b's subtree now holds all
// of cage-a's.
func reparentedLocations() []netbox.Location {
	locs := testLocations()
	locs[0].Parent = &netbox.NestedRef{ID: 20} // cage-a
	return locs
}

func dev(id int, locID int) netbox.Device {
	d := netbox.Device{ID: id, Site: &netbox.NestedRef{Slug: "pop2"}}
	if locID != 0 {
		d.Location = &netbox.NestedRef{ID: locID}
	}
	return d
}

func TestResolveScopeSubtree(t *testing.T) {
	cfg := model.SiteConfig{Slug: "pop2-old", NetBoxSite: "pop2", NetBoxLocation: "cage-a"}
	s, err := resolveScope(cfg, "pop2", testLocations())
	if err != nil {
		t.Fatal(err)
	}
	// cage-a's subtree is {10, 11, 12}; cage-b (20) is excluded.
	for _, id := range []int{10, 11, 12} {
		if !s.SubtreeIDs[id] {
			t.Errorf("expected %d in cage-a subtree", id)
		}
	}
	if s.SubtreeIDs[20] {
		t.Error("cage-b (20) must not be in cage-a subtree")
	}
}

func TestResolveScopeWholeSite(t *testing.T) {
	cfg := model.SiteConfig{Slug: "pop1", NetBoxSite: "pop1"}
	s, err := resolveScope(cfg, "pop1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.WholeSite() {
		t.Fatal("config with no location must be whole-site")
	}
}

func TestResolveScopeDriftError(t *testing.T) {
	// Location named by the config isn't in the site's location set
	// (gone, or re-parented to another site). Must error loudly, not
	// scope to nothing.
	cfg := model.SiteConfig{Slug: "pop2-old", NetBoxSite: "pop2", NetBoxLocation: "ghost-cage"}
	_, err := resolveScope(cfg, "pop2", testLocations())
	if err == nil {
		t.Fatal("expected stale-config error for a location not in the site")
	}
	if !strings.Contains(err.Error(), "stale site_config") {
		t.Fatalf("error should name the stale config: %v", err)
	}
}

func TestClaimsDevice(t *testing.T) {
	locScoped, _ := resolveScope(
		model.SiteConfig{Slug: "a", NetBoxSite: "pop2", NetBoxLocation: "cage-a"}, "pop2", testLocations())
	wholeSite, _ := resolveScope(
		model.SiteConfig{Slug: "w", NetBoxSite: "pop2"}, "pop2", testLocations())

	cases := []struct {
		name   string
		scope  Scope
		device netbox.Device
		want   bool
	}{
		{"loc-scoped claims own location", locScoped, dev(1, 10), true},
		{"loc-scoped claims descendant", locScoped, dev(2, 12), true},
		{"loc-scoped rejects sibling cage", locScoped, dev(3, 20), false},
		{"loc-scoped rejects no-location device", locScoped, dev(4, 0), false},
		{"whole-site claims any device", wholeSite, dev(5, 20), true},
		{"whole-site claims no-location device", wholeSite, dev(6, 0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.scope.ClaimsDevice(c.device); got != c.want {
				t.Fatalf("ClaimsDevice = %v, want %v", got, c.want)
			}
		})
	}
}

func TestClaimsPrefix(t *testing.T) {
	const siteID = 4
	locScoped, _ := resolveScope(
		model.SiteConfig{Slug: "a", NetBoxSite: "pop2", NetBoxLocation: "cage-a"}, "pop2", testLocations())
	wholeSite, _ := resolveScope(
		model.SiteConfig{Slug: "w", NetBoxSite: "pop2"}, "pop2", testLocations())

	siteScoped := netbox.Prefix{Prefix: "10.0.0.0/24", ScopeType: "dcim.site", ScopeID: siteID}
	cageAScoped := netbox.Prefix{Prefix: "10.1.0.0/24", ScopeType: "dcim.location", ScopeID: 10}
	cageBScoped := netbox.Prefix{Prefix: "10.2.0.0/24", ScopeType: "dcim.location", ScopeID: 20}

	if !wholeSite.ClaimsPrefix(siteScoped, siteID) {
		t.Error("whole-site must claim site-scoped prefix")
	}
	if wholeSite.ClaimsPrefix(cageAScoped, siteID) {
		t.Error("whole-site must NOT claim a location-scoped prefix")
	}
	if !locScoped.ClaimsPrefix(cageAScoped, siteID) {
		t.Error("loc-scoped must claim its own location's prefix")
	}
	if locScoped.ClaimsPrefix(cageBScoped, siteID) {
		t.Error("loc-scoped must NOT claim a sibling cage's prefix")
	}
	if locScoped.ClaimsPrefix(siteScoped, siteID) {
		t.Error("loc-scoped must NOT claim a site-scoped prefix (ruling 6: unclaimed)")
	}
}

func TestCheckOverlap(t *testing.T) {
	locs := testLocations()
	cageA := model.SiteConfig{Slug: "pop2-a", NetBoxSite: "pop2", NetBoxLocation: "cage-a"}
	cageB := model.SiteConfig{Slug: "pop2-b", NetBoxSite: "pop2", NetBoxLocation: "cage-b"}
	rackA1a := model.SiteConfig{Slug: "pop2-rack", NetBoxSite: "pop2", NetBoxLocation: "rack-a1a"}
	wholeSite := model.SiteConfig{Slug: "pop2-all", NetBoxSite: "pop2"}
	otherSite := model.SiteConfig{Slug: "pop3", NetBoxSite: "pop3"}

	t.Run("disjoint cages do not overlap", func(t *testing.T) {
		if err := CheckOverlap(cageB, []model.SiteConfig{cageA}, locs); err != nil {
			t.Fatalf("disjoint cages should not overlap: %v", err)
		}
	})
	t.Run("ancestor/descendant overlap rejected", func(t *testing.T) {
		// rack-a1a is nested under cage-a; subtree matching makes both
		// claim rack-a1a's devices, so this must be rejected even though
		// the slugs differ.
		if err := CheckOverlap(rackA1a, []model.SiteConfig{cageA}, locs); err == nil {
			t.Fatal("a child-location config must overlap its ancestor-location config")
		}
	})
	t.Run("whole-site overlaps any location config", func(t *testing.T) {
		if err := CheckOverlap(wholeSite, []model.SiteConfig{cageA}, locs); err == nil {
			t.Fatal("whole-site config must overlap an existing location-scoped config")
		}
		if err := CheckOverlap(cageA, []model.SiteConfig{wholeSite}, locs); err == nil {
			t.Fatal("location-scoped config must overlap an existing whole-site config")
		}
	})
	t.Run("different netbox site never overlaps", func(t *testing.T) {
		if err := CheckOverlap(otherSite, []model.SiteConfig{wholeSite}, locs); err != nil {
			t.Fatalf("configs on different sites must not overlap: %v", err)
		}
	})
	t.Run("update is idempotent (skips own row by slug)", func(t *testing.T) {
		if err := CheckOverlap(cageA, []model.SiteConfig{cageA, cageB}, locs); err != nil {
			t.Fatalf("re-saving a config must not overlap itself: %v", err)
		}
	})
}

// fakeConfigStore implements ConfigOwnerStore over an in-memory slice.
type fakeConfigStore struct {
	configs []model.SiteConfig
	err     error
}

func (f fakeConfigStore) ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []model.SiteConfig
	for _, c := range f.configs {
		if c.NetBoxSite == netboxSite {
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeLocNetBox implements ConfigOwnerNetBox, returning a fixed location
// tree and recording whether ListLocationsForSite was called (to assert the
// whole-site fast path skips the fetch).
type fakeLocNetBox struct {
	locations []netbox.Location
	called    bool
	err       error
}

func (f *fakeLocNetBox) ListLocationsForSite(_ context.Context, _ string) ([]netbox.Location, error) {
	f.called = true
	if f.err != nil {
		return nil, f.err
	}
	return f.locations, nil
}

// TestResolveOwningConfigSlug pins that worker RPC routing must map a
// device to the CONFIG slug that claims it (the WS-hub key), not the
// device's NetBox site slug. The canonical break is a location-scoped site
// whose config slug (pop2-old-cage) differs from netbox_site (pop2).
func TestResolveOwningConfigSlug(t *testing.T) {
	oldCage := model.SiteConfig{Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-a"}
	newCage := model.SiteConfig{Slug: "pop2-new-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-b"}
	wholeSite := model.SiteConfig{Slug: "pop1", NetBoxSite: "pop1"}

	t.Run("location-scoped: slug differs from netbox_site", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage, newCage}}
		nb := &fakeLocNetBox{locations: testLocations()}
		// dev(1, 10) is in cage-a -> claimed by pop2-old-cage.
		slug, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(1, 10))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if slug != "pop2-old-cage" {
			t.Fatalf("got slug %q, want pop2-old-cage (NOT the netbox site pop2)", slug)
		}
	})

	t.Run("location-scoped: descendant claimed by ancestor config", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage, newCage}}
		nb := &fakeLocNetBox{locations: testLocations()}
		// dev(2, 12) is in rack-a1a, nested under cage-a -> pop2-old-cage.
		slug, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(2, 12))
		if err != nil || slug != "pop2-old-cage" {
			t.Fatalf("got (%q, %v), want (pop2-old-cage, nil)", slug, err)
		}
	})

	t.Run("location-scoped: sibling cage routes to the other worker", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage, newCage}}
		nb := &fakeLocNetBox{locations: testLocations()}
		slug, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(3, 20))
		if err != nil || slug != "pop2-new-cage" {
			t.Fatalf("got (%q, %v), want (pop2-new-cage, nil)", slug, err)
		}
	})

	t.Run("location-scoped: unclaimed location -> ErrNoOwningConfig", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage}}
		nb := &fakeLocNetBox{locations: testLocations()}
		// dev in cage-b, which only pop2-new-cage would claim — absent here.
		if _, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(9, 20)); err != ErrNoOwningConfig {
			t.Fatalf("want ErrNoOwningConfig, got %v", err)
		}
	})

	t.Run("location-scoped: device with no location -> ErrNoOwningConfig", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage}}
		nb := &fakeLocNetBox{locations: testLocations()}
		if _, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(9, 0)); err != ErrNoOwningConfig {
			t.Fatalf("want ErrNoOwningConfig, got %v", err)
		}
	})

	t.Run("whole-site: config slug == netbox_site, no location fetch", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{wholeSite}}
		nb := &fakeLocNetBox{} // no locations; must not be consulted
		d := netbox.Device{ID: 1, Site: &netbox.NestedRef{Slug: "pop1"}}
		slug, err := ResolveOwningConfigSlug(context.Background(), nb, st, d)
		if err != nil || slug != "pop1" {
			t.Fatalf("got (%q, %v), want (pop1, nil)", slug, err)
		}
		if nb.called {
			t.Fatal("whole-site config must not trigger a location-tree fetch")
		}
	})

	t.Run("no config for the device's site -> ErrNoOwningConfig", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage}}
		nb := &fakeLocNetBox{locations: testLocations()}
		d := netbox.Device{ID: 1, Site: &netbox.NestedRef{Slug: "pop3"}}
		if _, err := ResolveOwningConfigSlug(context.Background(), nb, st, d); err != ErrNoOwningConfig {
			t.Fatalf("want ErrNoOwningConfig, got %v", err)
		}
	})

	t.Run("no-location device skips the location fetch (no false failure on NetBox blip)", func(t *testing.T) {
		// A location-scoped config is present, so pre-fix the resolver would
		// fetch the location tree even for a no-location device that can never
		// be claimed by it. Make the fetch error: a correct resolver doesn't
		// call it (device has no location) and returns ErrNoOwningConfig.
		st := fakeConfigStore{configs: []model.SiteConfig{oldCage}}
		nb := &fakeLocNetBox{err: fmt.Errorf("netbox 503")}
		if _, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(9, 0)); err != ErrNoOwningConfig {
			t.Fatalf("want ErrNoOwningConfig, got %v", err)
		}
		if nb.called {
			t.Fatal("must not fetch the location tree for a no-location device")
		}
	})

	t.Run("device with no site -> error", func(t *testing.T) {
		st := fakeConfigStore{configs: []model.SiteConfig{wholeSite}}
		nb := &fakeLocNetBox{}
		if _, err := ResolveOwningConfigSlug(context.Background(), nb, st, netbox.Device{ID: 1}); err == nil {
			t.Fatal("want error for device with no NetBox site")
		}
	})
}

// TestResolveOwningConfigSlugAmbiguousAfterLocationReparent: CheckOverlap
// admits configs on two sibling cages, then a NetBox operator moves cage-a
// under cage-b within the site. Nothing re-runs CheckOverlap and both
// locations still resolve, so both configs now claim cage-a's devices.
// Routing must refuse to pick one instead of returning whichever config the
// store lists first: Postgres returns the rows in no fixed order, and the
// order changes as rows are updated. A device only one config claims still
// routes.
func TestResolveOwningConfigSlugAmbiguousAfterLocationReparent(t *testing.T) {
	oldCage := model.SiteConfig{Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-a"}
	newCage := model.SiteConfig{Slug: "pop2-new-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-b"}
	if err := CheckOverlap(newCage, []model.SiteConfig{oldCage}, testLocations()); err != nil {
		t.Fatalf("setup: configs on sibling cages must be admitted: %v", err)
	}
	nb := &fakeLocNetBox{locations: reparentedLocations()}

	for _, order := range [][]model.SiteConfig{{oldCage, newCage}, {newCage, oldCage}} {
		st := fakeConfigStore{configs: order}
		listed := order[0].Slug + ", " + order[1].Slug

		slug, err := ResolveOwningConfigSlug(context.Background(), nb, st, dev(1, 10))
		if !errors.Is(err, ErrAmbiguousOwningConfig) {
			t.Fatalf("store lists [%s]: the cage-a device is claimed by both configs, want ErrAmbiguousOwningConfig, got (%q, %v)",
				listed, slug, err)
		}
		if !strings.Contains(err.Error(), "pop2-new-cage, pop2-old-cage") {
			t.Fatalf("store lists [%s]: the error must name both claiming configs: %v", listed, err)
		}

		slug, err = ResolveOwningConfigSlug(context.Background(), nb, st, dev(3, 20))
		if err != nil || slug != "pop2-new-cage" {
			t.Fatalf("store lists [%s]: the cage-b device is claimed by pop2-new-cage alone, want (pop2-new-cage, nil), got (%q, %v)",
				listed, slug, err)
		}
	}
}

// TestInvariantDispatcherScopesAndResolvesSiteIdentity covers invariant 3:
// a location-scoped config's snapshot sees only its subtree's devices,
// but SiteInfo resolves from the NetBox site (pop2), not the config slug.
func TestInvariantDispatcherScopesToLocation(t *testing.T) {
	cfg := model.SiteConfig{
		Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-a",
		EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}
	fs := newFakeStore(t, cfg)
	// Store has machines for BOTH devices. The dispatcher passes only the
	// scoped device IDs to ListMachinesByDeviceIDs, so only cage-a's
	// machine should surface in the snapshot.
	id1, id2 := 1, 2
	fs.machines = []model.Machine{
		{ID: uuid.New(), NetBoxDeviceID: &id1, NetBoxSerial: "SN-A", State: model.MachineStateNew},
		{ID: uuid.New(), NetBoxDeviceID: &id2, NetBoxSerial: "SN-B", State: model.MachineStateNew},
	}
	nb := &fakeNetBox{
		sites:     map[string]netbox.Site{"pop2": {Slug: "pop2", Name: "pop2"}},
		locations: testLocations(),
		// One device in cage-a (claimed), one in cage-b (other cage).
		devices: []netbox.Device{
			{ID: 1, Name: "in-cage-a", Serial: "SN-A", Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 10}},
			{ID: 2, Name: "in-cage-b", Serial: "SN-B", Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 20}},
		},
		locationPrefixes: map[int][]netbox.Prefix{
			10: {{Prefix: "10.0.0.0/24", ScopeType: "dcim.location", ScopeID: 10, CustomFields: gatewayJSON("10.0.0.1")}},
		},
	}
	d := NewDispatcher(nb, fs, &countingSink{})
	snap, err := d.rebuild(context.Background(), "pop2-old-cage", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Invariant 3a: SiteInfo from the NetBox site.
	if snap.Site.Slug != "pop2" {
		t.Errorf("SiteInfo.Slug = %q, want pop2 (resolved from netbox_site)", snap.Site.Slug)
	}
	// Invariant 3b: only cage-a's device is in the snapshot.
	if len(snap.Machines) != 1 {
		t.Fatalf("expected 1 machine (cage-a only), got %d", len(snap.Machines))
	}
}

// TestDispatcherFetchesDescendantLocationPrefixes: a prefix scoped to a
// CHILD location of the config's location must be discovered (the subtree
// is the claim, not just the root location). Regression test for the
// roast HIGH finding (fetch must match Scope.ClaimsPrefix).
func TestDispatcherFetchesDescendantLocationPrefixes(t *testing.T) {
	cfg := model.SiteConfig{
		Slug: "pop2-cage-a", NetBoxSite: "pop2", NetBoxLocation: "cage-a",
		EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}
	fs := newFakeStore(t, cfg)
	id := 1
	fs.machines = []model.Machine{
		{ID: uuid.New(), NetBoxDeviceID: &id, NetBoxSerial: "SN-A", State: model.MachineStateNew},
	}
	nb := &fakeNetBox{
		sites:     map[string]netbox.Site{"pop2": {Slug: "pop2", Name: "pop2"}},
		locations: testLocations(), // cage-a(10) > row-a1(11) > rack-a1a(12)
		devices: []netbox.Device{
			{ID: 1, Name: "host", Serial: "SN-A", Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 10}},
		},
		// The production prefix is scoped to rack-a1a (12), a DESCENDANT of
		// the config's cage-a (10), NOT to cage-a itself. A root-only query
		// would miss it; the subtree query must find it.
		locationPrefixes: map[int][]netbox.Prefix{
			12: {{Prefix: "10.0.0.0/24", ScopeType: "dcim.location", ScopeID: 12, CustomFields: gatewayJSON("10.0.0.1")}},
		},
	}
	d := NewDispatcher(nb, fs, &countingSink{})
	if _, err := d.rebuild(context.Background(), "pop2-cage-a", 1); err != nil {
		t.Fatalf("rebuild should find the descendant-scoped prefix, got: %v", err)
	}
}

// TestDispatcherLocationScopedPrimaryPrefixIsStable: when a location-scoped
// config's subtree has slagworks-production prefixes at more than one
// location, every rebuild over unchanged NetBox data must pick the same
// primary prefix (Build serves Inputs.Prefixes[0] as the fabric DHCP subnet
// and gateway), and must push only once. The primary is the prefix scoped to
// the config's own location, the one site-config create requires; when that
// location has none, it is the lowest-id descendant's.
func TestDispatcherLocationScopedPrimaryPrefixIsStable(t *testing.T) {
	// cage-a deliberately has a HIGHER id than its children, so sorting the
	// subtree ids alone would not put the config's own location first.
	locations := []netbox.Location{
		{ID: 30, Slug: "cage-a"},
		{ID: 11, Slug: "row-a1", Parent: &netbox.NestedRef{ID: 30}},
		{ID: 12, Slug: "row-a2", Parent: &netbox.NestedRef{ID: 30}},
	}
	prefix := func(id, locID int, cidr, gw string) netbox.Prefix {
		return netbox.Prefix{ID: id, Prefix: cidr, ScopeType: "dcim.location", ScopeID: locID, CustomFields: gatewayJSON(gw)}
	}
	cases := []struct {
		name     string
		prefixes map[int][]netbox.Prefix
		want     string
	}{
		{
			name: "own location has a prefix",
			prefixes: map[int][]netbox.Prefix{
				30: {prefix(1, 30, "10.1.0.0/24", "10.1.0.1")},
				11: {prefix(2, 11, "10.2.0.0/24", "10.2.0.1")},
				12: {prefix(3, 12, "10.3.0.0/24", "10.3.0.1")},
			},
			want: "10.1.0.0/24 gw 10.1.0.1",
		},
		{
			name: "only descendants have prefixes",
			prefixes: map[int][]netbox.Prefix{
				11: {prefix(2, 11, "10.2.0.0/24", "10.2.0.1")},
				12: {prefix(3, 12, "10.3.0.0/24", "10.3.0.1")},
			},
			want: "10.2.0.0/24 gw 10.2.0.1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := model.SiteConfig{
				Slug: "pop2-cage-a", NetBoxSite: "pop2", NetBoxLocation: "cage-a",
				EphemeralPoolStart: "10.1.0.100", EphemeralPoolEnd: "10.1.0.200",
			}
			nb := &fakeNetBox{
				sites:            map[string]netbox.Site{"pop2": {ID: 1, Slug: "pop2", Name: "pop2"}},
				locations:        locations,
				locationPrefixes: c.prefixes,
			}
			sink := &countingSink{}
			d := NewDispatcher(nb, newFakeStore(t, cfg), sink)
			seen := map[string]int{}
			const builds = 200
			for i := range builds {
				snap, err := d.rebuild(context.Background(), "pop2-cage-a", int64(i+1))
				if err != nil {
					t.Fatal(err)
				}
				seen[snap.DHCP.Subnet+" gw "+snap.DHCP.Gateway]++
			}
			if len(seen) != 1 || seen[c.want] != builds {
				t.Fatalf("%d rebuilds over unchanged NetBox data served %d different primary DHCP prefixes, want only %s: %v",
					builds, len(seen), c.want, seen)
			}
			if got := sink.Count(); got != 1 {
				t.Fatalf("%d rebuilds over unchanged NetBox data pushed %d snapshots, want 1", builds, got)
			}
		})
	}
}

// TestInvariantDispatcherDriftErrors: a location-scoped config whose
// location vanished from the site must fail the rebuild loudly.
func TestInvariantDispatcherDriftErrors(t *testing.T) {
	cfg := model.SiteConfig{
		Slug: "pop2-ghost", NetBoxSite: "pop2", NetBoxLocation: "ghost-cage",
		EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}
	fs := newFakeStore(t, cfg)
	nb := &fakeNetBox{
		sites:     map[string]netbox.Site{"pop2": {Slug: "pop2"}},
		locations: testLocations(), // ghost-cage absent
	}
	d := NewDispatcher(nb, fs, &countingSink{})
	_, err := d.rebuild(context.Background(), "pop2-ghost", 1)
	if err == nil || !strings.Contains(err.Error(), "stale site_config") {
		t.Fatalf("expected stale-config drift error, got %v", err)
	}
}

// TestDispatcherRefusesConfigsThatOverlapAfterLocationReparent: CheckOverlap
// admitted configs on two sibling cages, then NetBox moved cage-a under
// cage-b within the site. Neither config is stale, but both now claim
// cage-a's device, and building both snapshots would hand that machine to
// two workers. Each rebuild must fail and push nothing, so both workers keep
// their last snapshot until the operator repairs the tree or narrows a
// config. A sibling that is merely stale claims nothing, so it must not stop
// a healthy config from building.
func TestDispatcherRefusesConfigsThatOverlapAfterLocationReparent(t *testing.T) {
	oldCage := model.SiteConfig{
		Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-a",
		EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}
	newCage := model.SiteConfig{
		Slug: "pop2-new-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-b",
		EphemeralPoolStart: "10.0.1.100", EphemeralPoolEnd: "10.0.1.200",
	}
	setup := func(locations []netbox.Location) (*Dispatcher, *countingSink) {
		fs := newFakeStore(t, oldCage, newCage)
		id := 1
		fs.machines = []model.Machine{
			{ID: uuid.New(), NetBoxDeviceID: &id, NetBoxSerial: "SN-A", State: model.MachineStateNew},
		}
		nb := &fakeNetBox{
			sites:     map[string]netbox.Site{"pop2": {Slug: "pop2", Name: "pop2"}},
			locations: locations,
			devices: []netbox.Device{
				{ID: 1, Name: "in-cage-a", Serial: "SN-A", Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 10}},
			},
			locationPrefixes: map[int][]netbox.Prefix{
				10: {{Prefix: "10.0.0.0/24", ScopeType: "dcim.location", ScopeID: 10, CustomFields: gatewayJSON("10.0.0.1")}},
				20: {{Prefix: "10.0.1.0/24", ScopeType: "dcim.location", ScopeID: 20, CustomFields: gatewayJSON("10.0.1.1")}},
			},
		}
		sink := &countingSink{}
		return NewDispatcher(nb, fs, sink), sink
	}

	t.Run("sibling cages build", func(t *testing.T) {
		d, _ := setup(testLocations())
		for _, slug := range []string{oldCage.Slug, newCage.Slug} {
			if _, err := d.rebuild(context.Background(), slug, 1); err != nil {
				t.Fatalf("setup: %s must build while the cages are siblings: %v", slug, err)
			}
		}
	})

	t.Run("nested cages are refused", func(t *testing.T) {
		d, sink := setup(reparentedLocations())
		for _, slug := range []string{oldCage.Slug, newCage.Slug} {
			snap, err := d.rebuild(context.Background(), slug, 1)
			if err == nil {
				t.Fatalf("%s built a snapshot with %d machine(s) although cage-a now sits inside cage-b and both configs claim its device",
					slug, len(snap.Machines))
			}
			if !strings.Contains(err.Error(), "pop2-old-cage") || !strings.Contains(err.Error(), "pop2-new-cage") ||
				!strings.Contains(err.Error(), "overlap") {
				t.Fatalf("%s: the error must name both configs and the overlap: %v", slug, err)
			}
		}
		if got := sink.Count(); got != 0 {
			t.Fatalf("refused rebuilds pushed %d snapshot(s), want 0", got)
		}
	})

	t.Run("stale sibling does not block", func(t *testing.T) {
		d, _ := setup([]netbox.Location{{ID: 20, Slug: "cage-b"}}) // cage-a deleted
		if _, err := d.rebuild(context.Background(), newCage.Slug, 1); err != nil {
			t.Fatalf("%s must build while its only sibling is stale: %v", newCage.Slug, err)
		}
		if _, err := d.rebuild(context.Background(), oldCage.Slug, 1); err == nil || !strings.Contains(err.Error(), "stale site_config") {
			t.Fatalf("%s: want the stale-config error, got %v", oldCage.Slug, err)
		}
	})
}
