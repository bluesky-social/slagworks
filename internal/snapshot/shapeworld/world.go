// Package shapeworld is test support for the snapshot build's
// production-shape property tests (internal/snapshot/shape_*_prop_test.go).
// It is imported only by tests and by the oldsnap differential helper,
// never by a daemon.
//
// A World is a small typed NetBox: exactly the objects the snapshot
// dispatcher reads, stored under the arguments it reads them by. Two sources
// fill one: a rapid generator calibrated to the shapes surveyed in a real
// deployment, and Recorder, which captures what a real NetBox answered. One
// edit vocabulary and one set of properties then run over both.
//
// The package depends only on the netbox and model types, so the same source
// compiles against an older release for differential testing.
package shapeworld

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"sync"

	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// World holds NetBox objects keyed by the query that returns them. Lists the
// dispatcher filters by ID (interfaces, addresses, console ports) are flat
// and filtered here the way NetBox filters them; lists it asks for by slug,
// tag or role are stored per argument, so no NetBox filter semantics are
// re-implemented.
type World struct {
	Sites        map[string]netbox.Site       `json:"sites"`         // by slug
	Locations    map[string][]netbox.Location `json:"locations"`     // by site slug
	Devices      map[string][]netbox.Device   `json:"devices"`       // by site slug
	SitePrefixes map[string][]netbox.Prefix   `json:"site_prefixes"` // by SiteTagKey
	LocPrefixes  map[string][]netbox.Prefix   `json:"loc_prefixes"`  // by LocTagKey
	Ranges       map[string][]netbox.IPRange  `json:"ranges"`        // by RangeKey
	Interfaces   []netbox.Interface           `json:"interfaces"`
	IPs          []netbox.IPAddress           `json:"ips"`
	DeviceTypes  map[int]netbox.DeviceType    `json:"device_types"`
	ConsolePorts []netbox.ConsolePort         `json:"console_ports"`
}

// New returns an empty World with every map allocated.
func New() *World {
	return &World{
		Sites:        map[string]netbox.Site{},
		Locations:    map[string][]netbox.Location{},
		Devices:      map[string][]netbox.Device{},
		SitePrefixes: map[string][]netbox.Prefix{},
		LocPrefixes:  map[string][]netbox.Prefix{},
		Ranges:       map[string][]netbox.IPRange{},
		DeviceTypes:  map[int]netbox.DeviceType{},
	}
}

// SiteTagKey keys World.SitePrefixes.
func SiteTagKey(siteSlug, tag string) string { return siteSlug + "|" + tag }

// LocTagKey keys World.LocPrefixes.
func LocTagKey(locationID int, tag string) string { return strconv.Itoa(locationID) + "|" + tag }

// RangeKey keys World.Ranges.
func RangeKey(role, parentPrefix string) string { return role + "|" + parentPrefix }

// NB serves a World through the snapshot dispatcher's NetBoxReader. With
// Shuffle set every list comes back in a fresh pseudo-random order per call,
// deterministic in Seed and the call count. KeepPrefixOrder exempts the
// prefix lists, which NetBox returns in address order and whose first entry
// the snapshot build treats as the fabric DHCP prefix.
type NB struct {
	W               *World
	Shuffle         bool
	KeepPrefixOrder bool
	Seed            int64

	mu    sync.Mutex
	calls int64
}

func shuffled[T any](n *NB, in []T) []T {
	out := slices.Clone(in)
	n.mu.Lock()
	n.calls++
	calls := n.calls
	n.mu.Unlock()
	if n.Shuffle {
		r := rand.New(rand.NewSource(n.Seed + calls*7919)) //nolint:gosec // reproducible test shuffling, not a secret
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

func (n *NB) GetSiteBySlug(_ context.Context, slug string) (*netbox.Site, error) {
	s, ok := n.W.Sites[slug]
	if !ok {
		return nil, nil
	}
	return &s, nil
}

func (n *NB) GetDeviceByID(_ context.Context, id int) (*netbox.Device, error) {
	for _, devs := range n.W.Devices {
		for i := range devs {
			if devs[i].ID == id {
				d := devs[i]
				return &d, nil
			}
		}
	}
	return nil, nil
}

func (n *NB) ListDevicesForSite(_ context.Context, slug string) ([]netbox.Device, error) {
	return shuffled(n, n.W.Devices[slug]), nil
}

func (n *NB) ListLocationsForSite(_ context.Context, slug string) ([]netbox.Location, error) {
	return shuffled(n, n.W.Locations[slug]), nil
}

func idSet(ids []int) map[int]bool {
	out := make(map[int]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func (n *NB) ListInterfacesForDevices(_ context.Context, deviceIDs []int) ([]netbox.Interface, error) {
	want := idSet(deviceIDs)
	var out []netbox.Interface
	for _, i := range n.W.Interfaces {
		if i.Device != nil && want[i.Device.ID] {
			out = append(out, i)
		}
	}
	return shuffled(n, out), nil
}

func (n *NB) ListIPAddressesForInterfaces(_ context.Context, ifaceIDs []int) ([]netbox.IPAddress, error) {
	want := idSet(ifaceIDs)
	var out []netbox.IPAddress
	for _, ip := range n.W.IPs {
		if ip.AssignedObjectType == "dcim.interface" && ip.AssignedObjectID != nil && want[*ip.AssignedObjectID] {
			out = append(out, ip)
		}
	}
	return shuffled(n, out), nil
}

func (n *NB) ListSitePrefixes(_ context.Context, slug, tag string) ([]netbox.Prefix, error) {
	if n.KeepPrefixOrder {
		return slices.Clone(n.W.SitePrefixes[SiteTagKey(slug, tag)]), nil
	}
	return shuffled(n, n.W.SitePrefixes[SiteTagKey(slug, tag)]), nil
}

func (n *NB) ListLocationPrefixes(_ context.Context, locationID int, tag string) ([]netbox.Prefix, error) {
	if n.KeepPrefixOrder {
		return slices.Clone(n.W.LocPrefixes[LocTagKey(locationID, tag)]), nil
	}
	return shuffled(n, n.W.LocPrefixes[LocTagKey(locationID, tag)]), nil
}

func (n *NB) ListIPRangesByRole(_ context.Context, roleSlug, parentPrefix string) ([]netbox.IPRange, error) {
	return shuffled(n, n.W.Ranges[RangeKey(roleSlug, parentPrefix)]), nil
}

// ListDeviceTypesByIDs answers in request order, as the client's per-ID
// fetch does, and fails on an unknown ID as a 404 would.
func (n *NB) ListDeviceTypesByIDs(_ context.Context, ids []int) ([]netbox.DeviceType, error) {
	out := make([]netbox.DeviceType, 0, len(ids))
	for _, id := range ids {
		dt, ok := n.W.DeviceTypes[id]
		if !ok {
			return nil, fmt.Errorf("get device-type %d: netbox: 404 Not Found", id)
		}
		out = append(out, dt)
	}
	return out, nil
}

func (n *NB) ListConsolePortsForDevices(_ context.Context, deviceIDs []int) ([]netbox.ConsolePort, error) {
	want := idSet(deviceIDs)
	var out []netbox.ConsolePort
	for _, p := range n.W.ConsolePorts {
		if p.Device != nil && want[p.Device.ID] {
			out = append(out, p)
		}
	}
	return shuffled(n, out), nil
}

// StoreData is the Postgres side of a build: what the dispatcher's
// StoreReader returns.
type StoreData struct {
	Configs        []model.SiteConfig    `json:"configs"`
	Machines       []model.Machine       `json:"machines"`
	Images         []model.Image         `json:"images"`
	Scripts        []model.StorageScript `json:"scripts"`
	NetworkScripts []model.NetworkScript `json:"network_scripts"`
	SSHKeys        []string              `json:"ssh_keys"`
	Workers        []model.SiteWorker    `json:"workers"`
}

// Store serves a StoreData through the dispatcher's StoreReader. With
// Shuffle set every list comes back in a fresh pseudo-random order per call,
// deterministic in Seed and the call count.
type Store struct {
	D       *StoreData
	Shuffle bool
	Seed    int64

	mu    sync.Mutex
	calls int64
}

func storeShuffled[T any](s *Store, in []T) []T {
	out := slices.Clone(in)
	if s.Shuffle {
		s.mu.Lock()
		s.calls++
		r := rand.New(rand.NewSource(s.Seed + s.calls*104729)) //nolint:gosec // reproducible test shuffling, not a secret
		s.mu.Unlock()
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return out
}

func (s *Store) GetSiteConfig(slug string) (*model.SiteConfig, error) {
	for _, c := range s.D.Configs {
		if c.Slug == slug {
			return &c, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}

func (s *Store) ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error) {
	var out []model.SiteConfig
	for _, c := range s.D.Configs {
		if c.NetBoxSite == netboxSite {
			out = append(out, c)
		}
	}
	return storeShuffled(s, out), nil
}

func (s *Store) ListMachinesByDeviceIDs(deviceIDs []int) ([]model.Machine, error) {
	want := idSet(deviceIDs)
	var out []model.Machine
	for _, m := range s.D.Machines {
		if m.NetBoxDeviceID != nil && want[*m.NetBoxDeviceID] {
			out = append(out, m)
		}
	}
	return storeShuffled(s, out), nil
}

func (s *Store) ListImages() ([]model.Image, error) { return storeShuffled(s, s.D.Images), nil }

func (s *Store) ListStorageScripts() ([]model.StorageScript, error) {
	return storeShuffled(s, s.D.Scripts), nil
}

func (s *Store) ListNetworkScripts() ([]model.NetworkScript, error) {
	return storeShuffled(s, s.D.NetworkScripts), nil
}

func (s *Store) ListSSHPublicKeys() ([]string, error) { return storeShuffled(s, s.D.SSHKeys), nil }

// ListSiteWorkers returns the workers registered to a config. A release
// whose StoreReader predates the method simply never calls it.
func (s *Store) ListSiteWorkers(siteSlug string) ([]model.SiteWorker, error) {
	var out []model.SiteWorker
	for _, w := range s.D.Workers {
		if w.SiteSlug == siteSlug {
			out = append(out, w)
		}
	}
	return storeShuffled(s, out), nil
}

// BumpSiteSnapshotMeta is never reached: the tests build read-only.
func (s *Store) BumpSiteSnapshotMeta(string, string) (int64, error) {
	return 0, fmt.Errorf("shapeworld.Store is read-only")
}

// Sink discards pushes; read-only builds never push.
type Sink struct{}

func (Sink) Push(string, model.SiteSnapshot) {}
