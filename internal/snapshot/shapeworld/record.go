package shapeworld

import (
	"context"

	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Reader is the snapshot dispatcher's NetBoxReader, restated here so this
// package does not import the package under test.
type Reader interface {
	GetSiteBySlug(ctx context.Context, slug string) (*netbox.Site, error)
	GetDeviceByID(ctx context.Context, id int) (*netbox.Device, error)
	ListDevicesForSite(ctx context.Context, slug string) ([]netbox.Device, error)
	ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error)
	ListInterfacesForDevices(ctx context.Context, deviceIDs []int) ([]netbox.Interface, error)
	ListIPAddressesForInterfaces(ctx context.Context, ifaceIDs []int) ([]netbox.IPAddress, error)
	ListSitePrefixes(ctx context.Context, slug, tag string) ([]netbox.Prefix, error)
	ListLocationPrefixes(ctx context.Context, locationID int, tag string) ([]netbox.Prefix, error)
	ListIPRangesByRole(ctx context.Context, roleSlug, parentPrefix string) ([]netbox.IPRange, error)
	ListDeviceTypesByIDs(ctx context.Context, ids []int) ([]netbox.DeviceType, error)
	ListConsolePortsForDevices(ctx context.Context, deviceIDs []int) ([]netbox.ConsolePort, error)
}

// Recorder passes every read through to Inner and files the answer in W, so
// that a later build over W.NB() sees what Inner answered. Objects fetched by
// ID are merged by ID; lists fetched by slug, tag or role replace the stored
// list for that argument.
type Recorder struct {
	Inner Reader
	W     *World
}

func (r *Recorder) GetSiteBySlug(ctx context.Context, slug string) (*netbox.Site, error) {
	s, err := r.Inner.GetSiteBySlug(ctx, slug)
	if err == nil && s != nil {
		r.W.Sites[slug] = *s
	}
	return s, err
}

func (r *Recorder) GetDeviceByID(ctx context.Context, id int) (*netbox.Device, error) {
	return r.Inner.GetDeviceByID(ctx, id)
}

func (r *Recorder) ListDevicesForSite(ctx context.Context, slug string) ([]netbox.Device, error) {
	out, err := r.Inner.ListDevicesForSite(ctx, slug)
	if err == nil {
		r.W.Devices[slug] = out
	}
	return out, err
}

func (r *Recorder) ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error) {
	out, err := r.Inner.ListLocationsForSite(ctx, slug)
	if err == nil {
		r.W.Locations[slug] = out
	}
	return out, err
}

func (r *Recorder) ListInterfacesForDevices(ctx context.Context, deviceIDs []int) ([]netbox.Interface, error) {
	out, err := r.Inner.ListInterfacesForDevices(ctx, deviceIDs)
	if err == nil {
		have := make(map[int]bool, len(r.W.Interfaces))
		for _, i := range r.W.Interfaces {
			have[i.ID] = true
		}
		for _, i := range out {
			if !have[i.ID] {
				r.W.Interfaces = append(r.W.Interfaces, i)
			}
		}
	}
	return out, err
}

func (r *Recorder) ListIPAddressesForInterfaces(ctx context.Context, ifaceIDs []int) ([]netbox.IPAddress, error) {
	out, err := r.Inner.ListIPAddressesForInterfaces(ctx, ifaceIDs)
	if err == nil {
		have := make(map[int]bool, len(r.W.IPs))
		for _, ip := range r.W.IPs {
			have[ip.ID] = true
		}
		for _, ip := range out {
			if !have[ip.ID] {
				r.W.IPs = append(r.W.IPs, ip)
			}
		}
	}
	return out, err
}

func (r *Recorder) ListSitePrefixes(ctx context.Context, slug, tag string) ([]netbox.Prefix, error) {
	out, err := r.Inner.ListSitePrefixes(ctx, slug, tag)
	if err == nil {
		r.W.SitePrefixes[SiteTagKey(slug, tag)] = out
	}
	return out, err
}

func (r *Recorder) ListLocationPrefixes(ctx context.Context, locationID int, tag string) ([]netbox.Prefix, error) {
	out, err := r.Inner.ListLocationPrefixes(ctx, locationID, tag)
	if err == nil {
		r.W.LocPrefixes[LocTagKey(locationID, tag)] = out
	}
	return out, err
}

func (r *Recorder) ListIPRangesByRole(ctx context.Context, roleSlug, parentPrefix string) ([]netbox.IPRange, error) {
	out, err := r.Inner.ListIPRangesByRole(ctx, roleSlug, parentPrefix)
	if err == nil {
		r.W.Ranges[RangeKey(roleSlug, parentPrefix)] = out
	}
	return out, err
}

func (r *Recorder) ListDeviceTypesByIDs(ctx context.Context, ids []int) ([]netbox.DeviceType, error) {
	out, err := r.Inner.ListDeviceTypesByIDs(ctx, ids)
	if err == nil {
		for _, dt := range out {
			r.W.DeviceTypes[dt.ID] = dt
		}
	}
	return out, err
}

func (r *Recorder) ListConsolePortsForDevices(ctx context.Context, deviceIDs []int) ([]netbox.ConsolePort, error) {
	out, err := r.Inner.ListConsolePortsForDevices(ctx, deviceIDs)
	if err == nil {
		have := make(map[int]bool, len(r.W.ConsolePorts))
		for _, p := range r.W.ConsolePorts {
			have[p.ID] = true
		}
		for _, p := range out {
			if !have[p.ID] {
				r.W.ConsolePorts = append(r.W.ConsolePorts, p)
			}
		}
	}
	return out, err
}
