package snapshot

import (
	"context"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// fakeClaimConfigLister implements claimConfigLister: returns the configs
// whose NetBoxSite matches.
type fakeClaimConfigLister struct {
	configs []model.SiteConfig
}

func (f *fakeClaimConfigLister) ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error) {
	var out []model.SiteConfig
	for _, c := range f.configs {
		if c.NetBoxSite == netboxSite {
			out = append(out, c)
		}
	}
	return out, nil
}

func TestClaimingConfigsLocationScoped(t *testing.T) {
	// Two location-scoped configs over one NetBox site (dual cage), plus
	// the location tree they resolve against.
	st := &fakeClaimConfigLister{configs: []model.SiteConfig{
		{Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-a"},
		{Slug: "pop2-new-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-b"},
	}}
	nb := &fakeNetBox{locations: testLocations()} // cage-a=10 (subtree 10,11,12), cage-b=20
	r := NewClaimResolver(nb, st)

	tests := []struct {
		name string
		dev  netbox.Device
		want []string
	}{
		{
			name: "device in cage-a subtree claimed by old-cage config",
			dev:  dev(1, 12), // rack-a1a, a descendant of cage-a
			want: []string{"pop2-old-cage"},
		},
		{
			name: "device at cage-a root claimed by old-cage config",
			dev:  dev(2, 10),
			want: []string{"pop2-old-cage"},
		},
		{
			name: "device in cage-b claimed by new-cage config",
			dev:  dev(3, 20),
			want: []string{"pop2-new-cage"},
		},
		{
			name: "device with no location claimed by neither (location-scoped configs)",
			dev:  dev(4, 0),
			want: nil,
		},
		{
			name: "device in an unclaimed location of the managed site claimed by neither",
			dev:  netbox.Device{ID: 5, Site: &netbox.NestedRef{Slug: "pop2"}, Location: &netbox.NestedRef{ID: 99}},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ClaimingConfigs(context.Background(), tc.dev)
			if err != nil {
				t.Fatal(err)
			}
			if !sameStrings(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaimingConfigsUnmanagedSite(t *testing.T) {
	st := &fakeClaimConfigLister{configs: []model.SiteConfig{
		{Slug: "pop2-old-cage", NetBoxSite: "pop2", NetBoxLocation: "cage-a"},
	}}
	nb := &fakeNetBox{locations: testLocations()}
	r := NewClaimResolver(nb, st)

	// Device in a site no config mirrors → empty, no NetBox location fetch
	// even attempted (no candidate configs).
	got, err := r.ClaimingConfigs(context.Background(),
		netbox.Device{ID: 1, Site: &netbox.NestedRef{Slug: "pop9"}, Location: &netbox.NestedRef{ID: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unmanaged site should claim nothing, got %v", got)
	}

	// Device with no site set → empty.
	got, err = r.ClaimingConfigs(context.Background(), netbox.Device{ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("siteless device should claim nothing, got %v", got)
	}
}

func TestClaimingConfigsWholeSite(t *testing.T) {
	// A whole-site config claims every device in its NetBox site,
	// regardless of location.
	st := &fakeClaimConfigLister{configs: []model.SiteConfig{
		{Slug: "pop3", NetBoxSite: "pop3"}, // no NetBoxLocation = whole-site
	}}
	nb := &fakeNetBox{} // whole-site needs no location tree
	r := NewClaimResolver(nb, st)

	got, err := r.ClaimingConfigs(context.Background(),
		netbox.Device{ID: 1, Site: &netbox.NestedRef{Slug: "pop3"}, Location: &netbox.NestedRef{ID: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(got, []string{"pop3"}) {
		t.Fatalf("whole-site config should claim the device, got %v", got)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
