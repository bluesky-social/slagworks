package snapshot

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// fakeNetBox implements NetBoxReader and MirrorNetBox (Task 12).
type fakeNetBox struct {
	// Single-site helper: tests that use only one site can set `site`
	// and `GetSiteBySlug` returns it for matching queries.
	site        netbox.Site
	prefixes    []netbox.Prefix
	devices     []netbox.Device
	deviceTypes []netbox.DeviceType
	ifaces      []netbox.Interface
	ips         []netbox.IPAddress
	locations   []netbox.Location // returned by ListLocationsForSite
	// locationPrefixes is keyed by location id; returned by
	// ListLocationPrefixes. Nil falls back to `prefixes`.
	locationPrefixes map[int][]netbox.Prefix

	// oobPrefixes is returned by ListSitePrefixes when the tag is
	// "slagworks-oob"; `prefixes` is returned for any other tag (the
	// fabric tag). Lets OOB-specific tests opt in without every existing
	// test suddenly emitting OOB config.
	oobPrefixes []netbox.Prefix
	// oobRanges is returned by ListIPRangesByRole.
	oobRanges []netbox.IPRange

	// consolePorts is returned by ListConsolePortsForDevices, filtered
	// to the requested device ids.
	consolePorts []netbox.ConsolePort

	// Multi-site tests populate this map instead of `site`.
	sites map[string]netbox.Site
}

func (f *fakeNetBox) GetSiteBySlug(ctx context.Context, slug string) (*netbox.Site, error) {
	if f.sites != nil {
		if s, ok := f.sites[slug]; ok {
			return &s, nil
		}
		return nil, nil
	}
	if f.site.Slug == slug {
		s := f.site
		return &s, nil
	}
	return nil, nil
}

func (f *fakeNetBox) GetDeviceByID(ctx context.Context, id int) (*netbox.Device, error) {
	for i := range f.devices {
		if f.devices[i].ID == id {
			d := f.devices[i]
			return &d, nil
		}
	}
	return nil, nil
}

func (f *fakeNetBox) ListDevicesForSite(ctx context.Context, slug string) ([]netbox.Device, error) {
	// Return all devices whose site.slug matches, or all devices when
	// single-site fakeNetBox is in use.
	if f.sites == nil {
		return f.devices, nil
	}
	var out []netbox.Device
	for _, d := range f.devices {
		if d.Site != nil && d.Site.Slug == slug {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeNetBox) ListInterfacesForDevices(ctx context.Context, ids []int) ([]netbox.Interface, error) {
	return f.ifaces, nil
}

func (f *fakeNetBox) ListIPAddressesForInterfaces(ctx context.Context, ids []int) ([]netbox.IPAddress, error) {
	return f.ips, nil
}

func (f *fakeNetBox) ListConsolePortsForDevices(ctx context.Context, ids []int) ([]netbox.ConsolePort, error) {
	want := make(map[int]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []netbox.ConsolePort
	for _, p := range f.consolePorts {
		if p.Device != nil && want[p.Device.ID] {
			out = append(out, p)
		}
	}
	return out, nil
}

func (f *fakeNetBox) ListSitePrefixes(ctx context.Context, slug, tag string) ([]netbox.Prefix, error) {
	if tag == "slagworks-oob" {
		return f.oobPrefixes, nil
	}
	return f.prefixes, nil
}

func (f *fakeNetBox) ListIPRangesByRole(ctx context.Context, roleSlug, parentPrefix string) ([]netbox.IPRange, error) {
	return f.oobRanges, nil
}

func (f *fakeNetBox) ListLocationPrefixes(ctx context.Context, locationID int, tag string) ([]netbox.Prefix, error) {
	// Tag-aware like ListSitePrefixes: the OOB tag returns oobPrefixes so a
	// location-scoped test doesn't build OOB config from fabric prefixes.
	if tag == "slagworks-oob" {
		return f.oobPrefixes, nil
	}
	if f.locationPrefixes != nil {
		return f.locationPrefixes[locationID], nil
	}
	return f.prefixes, nil
}

func (f *fakeNetBox) ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error) {
	return f.locations, nil
}

func (f *fakeNetBox) ListDeviceTypesByIDs(ctx context.Context, ids []int) ([]netbox.DeviceType, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	want := make(map[int]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]netbox.DeviceType, 0, len(ids))
	for _, dt := range f.deviceTypes {
		if want[dt.ID] {
			out = append(out, dt)
		}
	}
	return out, nil
}

// fakeStore implements StoreReader.
type fakeStore struct {
	mu             sync.Mutex
	snapshotSeq    int64 // models the site_snapshot_seq sequence; outlives configs rows
	configs        map[string]*model.SiteConfig
	machines       []model.Machine
	images         []model.Image
	scripts        []model.StorageScript
	networkScripts []model.NetworkScript
	sshKeys        []string
}

func newFakeStore(t *testing.T, cfgs ...model.SiteConfig) *fakeStore {
	t.Helper()
	s := &fakeStore{configs: make(map[string]*model.SiteConfig, len(cfgs))}
	for i := range cfgs {
		c := cfgs[i]
		s.configs[c.Slug] = &c
	}
	return s
}

func (f *fakeStore) GetSiteConfig(slug string) (*model.SiteConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.configs[slug]
	if !ok {
		return nil, errNotFound(slug)
	}
	cc := *c
	return &cc, nil
}

// ListSiteConfigsByNetBoxSite returns the matching configs in map order,
// which varies from call to call like Postgres's unordered rows.
func (f *fakeStore) ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.SiteConfig
	for _, c := range f.configs {
		if c.NetBoxSite == netboxSite {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *fakeStore) ListMachinesByDeviceIDs(ids []int) ([]model.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	want := make(map[int]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var out []model.Machine
	for _, m := range f.machines {
		if m.NetBoxDeviceID != nil && want[*m.NetBoxDeviceID] {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeStore) ListImages() ([]model.Image, error)                 { return f.images, nil }
func (f *fakeStore) ListStorageScripts() ([]model.StorageScript, error) { return f.scripts, nil }
func (f *fakeStore) ListNetworkScripts() ([]model.NetworkScript, error) { return f.networkScripts, nil }
func (f *fakeStore) ListSSHPublicKeys() ([]string, error)               { return f.sshKeys, nil }

func (f *fakeStore) BumpSiteSnapshotMeta(slug, hash string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.configs[slug]
	f.snapshotSeq++
	c.LastSnapshotSeq = f.snapshotSeq
	c.LastSnapshotHash = hash
	return c.LastSnapshotSeq, nil
}

type errNotFound string

func (e errNotFound) Error() string { return "not found: " + string(e) }

// countingSink records every Push call.
type countingSink struct {
	mu     sync.Mutex
	pushes []model.SiteSnapshot
}

func (c *countingSink) Push(slug string, snap model.SiteSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pushes = append(c.pushes, snap)
}

func (c *countingSink) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pushes)
}

func (c *countingSink) Last() model.SiteSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pushes[len(c.pushes)-1]
}

// gatewayJSON builds a custom_fields json.RawMessage with gateway_ip.
func gatewayJSON(gw string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"gateway_ip": gw})
	return b
}
