package netbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
)

// Read-only NetBox query helpers used by the snapshot dispatcher and
// reconciler. All use Client.List (paginated) or Client.Get (single
// object by id). Do NOT add a parallel pagination path — the shared
// client handles retries, rate limiting, and API versioning.

// ListDeviceTypesByIDs returns the DeviceType objects for the given
// IDs, in one paginated call. Used by the snapshot builder to surface
// per-device-type custom fields (slagworks_kernel_params) without
// running a separate fetch per device.
//
// NetBox doesn't support `id__in` on the device-types endpoint as of
// 4.3, so we fan out to per-id Gets via the existing client.
func (c *Client) ListDeviceTypesByIDs(ctx context.Context, ids []int) ([]DeviceType, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]DeviceType, 0, len(ids))
	for _, id := range ids {
		var dt DeviceType
		path := fmt.Sprintf("/api/dcim/device-types/%d/", id)
		if err := c.Get(ctx, path, nil, &dt); err != nil {
			return nil, fmt.Errorf("get device-type %d: %w", id, err)
		}
		out = append(out, dt)
	}
	return out, nil
}

// GetDeviceTypeByID fetches a single device type (with custom fields).
func (c *Client) GetDeviceTypeByID(ctx context.Context, id int) (*DeviceType, error) {
	var dt DeviceType
	path := fmt.Sprintf("/api/dcim/device-types/%d/", id)
	if err := c.Get(ctx, path, nil, &dt); err != nil {
		return nil, fmt.Errorf("get device-type %d: %w", id, err)
	}
	return &dt, nil
}

// SetDeviceTypeKernelParams writes the slagworks_kernel_params custom
// field on a device type. Used by the console determination sweep:
// commission discovered which tty the BMC SOL is wired to and records
// it on the TYPE (the field is per-model, shared by every device of
// that hardware). Callers enforce the only-if-empty rule — this method
// writes unconditionally.
func (c *Client) SetDeviceTypeKernelParams(ctx context.Context, id int, kernelParams string) error {
	path := fmt.Sprintf("/api/dcim/device-types/%d/", id)
	body := map[string]any{
		"custom_fields": map[string]any{"slagworks_kernel_params": kernelParams},
	}
	var updated DeviceType
	return c.Patch(ctx, path, body, &updated)
}

// ListDevicesForSite returns every Device at a site (by slug).
func (c *Client) ListDevicesForSite(ctx context.Context, siteSlug string) ([]Device, error) {
	q := url.Values{"site": []string{siteSlug}, "limit": []string{"250"}}
	var out []Device
	if err := c.List(ctx, "/api/dcim/devices/", q, &out); err != nil {
		return nil, fmt.Errorf("list devices for site %s: %w", siteSlug, err)
	}
	return out, nil
}

// multiValueChunk caps how many repeated id params go on one request
// line. The snapshot dispatcher passes every interface ID at a site
// (potentially >1000); at ~25 bytes per "assigned_object_id=NNNN&"
// param an unchunked URL would exceed common 8KB header limits. 200
// ids keeps the request line under ~5KB with headroom.
const multiValueChunk = 200

// ListDevicesByIDs fetches devices by NetBox integer ID using NetBox's
// repeated-parameter OR filter (?id=1&id=2), chunked to keep request lines
// bounded for fleet-wide machine list enrichment.
func (c *Client) ListDevicesByIDs(ctx context.Context, deviceIDs []int) ([]Device, error) {
	var out []Device
	for chunk := range slices.Chunk(deviceIDs, multiValueChunk) {
		q := url.Values{"limit": []string{"500"}}
		for _, id := range chunk {
			q.Add("id", strconv.Itoa(id))
		}
		var page []Device
		if err := c.List(ctx, "/api/dcim/devices/", q, &page); err != nil {
			return nil, fmt.Errorf("list devices: %w", err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// ListInterfacesForDevices fetches interfaces for a batch of device IDs,
// using NetBox's repeated-parameter OR filter (?device_id=1&device_id=2),
// chunked to keep URLs under header limits. Do NOT use the
// `device_id__in=1,2` comma form: NetBox 4.5.x silently ignores the
// `__in` suffix on this endpoint and returns every interface in the
// database. Repeated single-value params are the documented
// multi-value shape and verified correct against prod 4.5.4 (2026-06-10).
func (c *Client) ListInterfacesForDevices(ctx context.Context, deviceIDs []int) ([]Interface, error) {
	var out []Interface
	for chunk := range slices.Chunk(deviceIDs, multiValueChunk) {
		q := url.Values{"limit": []string{"500"}}
		for _, id := range chunk {
			q.Add("device_id", strconv.Itoa(id))
		}
		var page []Interface
		if err := c.List(ctx, "/api/dcim/interfaces/", q, &page); err != nil {
			return nil, fmt.Errorf("list interfaces: %w", err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// ListConsolePortsForDevices returns the console ports of the given
// devices, with their cable-path far end (connected_endpoints) inline.
// Same chunked repeated-parameter OR filter as ListInterfacesForDevices
// (`device_id__in` is silently ignored).
func (c *Client) ListConsolePortsForDevices(ctx context.Context, deviceIDs []int) ([]ConsolePort, error) {
	var out []ConsolePort
	for chunk := range slices.Chunk(deviceIDs, multiValueChunk) {
		q := url.Values{"limit": []string{"500"}}
		for _, id := range chunk {
			q.Add("device_id", strconv.Itoa(id))
		}
		var page []ConsolePort
		if err := c.List(ctx, "/api/dcim/console-ports/", q, &page); err != nil {
			return nil, fmt.Errorf("list console ports: %w", err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// ListIPAddressesForInterfaces returns IPAddresses assigned to the given
// Interface IDs, using the chunked repeated-parameter OR filter. Same
// `__in` footgun as ListInterfacesForDevices: `assigned_object_id__in=a,b`
// is silently ignored on /api/ipam/ip-addresses/; repeated
// `assigned_object_id=` params filter correctly (verified prod 4.5.4,
// 2026-06-10).
func (c *Client) ListIPAddressesForInterfaces(ctx context.Context, ifaceIDs []int) ([]IPAddress, error) {
	var out []IPAddress
	for chunk := range slices.Chunk(ifaceIDs, multiValueChunk) {
		q := url.Values{
			"assigned_object_type": []string{"dcim.interface"},
			"limit":                []string{"500"},
		}
		for _, id := range chunk {
			q.Add("assigned_object_id", strconv.Itoa(id))
		}
		var page []IPAddress
		if err := c.List(ctx, "/api/ipam/ip-addresses/", q, &page); err != nil {
			return nil, fmt.Errorf("list ip addresses: %w", err)
		}
		out = append(out, page...)
	}
	return out, nil
}

// GetIPAddress fetches a single IPAddress by ID. Used where only a nested
// ref (e.g. device.oob_ip, which carries id+address but no status) is in
// hand and the caller must apply the IsActive rule.
func (c *Client) GetIPAddress(ctx context.Context, id int) (*IPAddress, error) {
	var ip IPAddress
	if err := c.Get(ctx, fmt.Sprintf("/api/ipam/ip-addresses/%d/", id), nil, &ip); err != nil {
		return nil, fmt.Errorf("get ip address %d: %w", id, err)
	}
	return &ip, nil
}

// ListIPRangesByRole returns the IP ranges whose IPAM role matches the
// given slug AND that fall within the given parent prefix. Used to fetch
// the OOB DHCP dynamic pool (role slagworks-oob-dynamic, under the
// slagworks-oob prefix). The role classifies the range's functional
// purpose; the parent scopes it to the OOB subnet so an unrelated range
// with the same role at another site isn't picked up. Empty result is
// fine — pre-cutover there is no dynamic range.
func (c *Client) ListIPRangesByRole(ctx context.Context, roleSlug, parentPrefix string) ([]IPRange, error) {
	q := url.Values{
		"role":   []string{roleSlug},
		"parent": []string{parentPrefix},
		"limit":  []string{"50"},
	}
	var out []IPRange
	if err := c.List(ctx, "/api/ipam/ip-ranges/", q, &out); err != nil {
		return nil, fmt.Errorf("list ip ranges: %w", err)
	}
	return out, nil
}

// ListPowerPortsForDevice returns the power-ports (PSU feeds) for a
// single device, with connected_endpoints populated so the caller can
// trace each PSU to its PDU outlet. Single-value `device_id=` filter;
// PDU runtime resolution is always per-device (one machine's op at a
// time) so there is no batching to do.
func (c *Client) ListPowerPortsForDevice(ctx context.Context, deviceID int) ([]PowerPort, error) {
	q := url.Values{
		"device_id": []string{strconv.Itoa(deviceID)},
		"limit":     []string{"50"},
	}
	var out []PowerPort
	if err := c.List(ctx, "/api/dcim/power-ports/", q, &out); err != nil {
		return nil, fmt.Errorf("list power-ports for device %d: %w", deviceID, err)
	}
	return out, nil
}

// ListPowerOutletsForDevice returns the power-outlets for a single PDU
// device, with connected_endpoints populated so the caller can reverse-
// trace each outlet to the downstream power-port (PSU feed) and its parent
// device that the outlet cables to. Single-value `device_id=` filter;
// limit=50 gives safe headroom over the largest PDU model (24 outlets).
func (c *Client) ListPowerOutletsForDevice(ctx context.Context, deviceID int) ([]PowerOutlet, error) {
	q := url.Values{
		"device_id": []string{strconv.Itoa(deviceID)},
		"limit":     []string{"50"},
	}
	var out []PowerOutlet
	if err := c.List(ctx, "/api/dcim/power-outlets/", q, &out); err != nil {
		return nil, fmt.Errorf("list power-outlets for device %d: %w", deviceID, err)
	}
	return out, nil
}

// ListSitePrefixes returns prefixes for the given site tagged with the
// given tag (e.g. "slagworks-production" or "slagworks-oob"). Used for
// whole-site configs. NOTE: NetBox 4.x prefix scope is polymorphic, and
// `?site=` matches ONLY prefixes whose scope is that site (scope_type
// dcim.site) — it does NOT transitively include prefixes scoped to a
// location under the site. Location-scoped configs must use
// ListLocationPrefixes instead.
func (c *Client) ListSitePrefixes(ctx context.Context, siteSlug, tag string) ([]Prefix, error) {
	q := url.Values{
		"site":  []string{siteSlug},
		"tag":   []string{tag},
		"limit": []string{"50"},
	}
	var out []Prefix
	if err := c.List(ctx, "/api/ipam/prefixes/", q, &out); err != nil {
		return nil, fmt.Errorf("list prefixes: %w", err)
	}
	return out, nil
}

// ListLocationPrefixes returns prefixes scoped to the given NetBox
// location (by integer id) and tagged with the given tag. Used for
// location-scoped configs.
//
// IMPLEMENTER/OPERATOR NOTE: as of 2026-06-12 there
// are ZERO location-scoped prefixes anywhere in prod NetBox, so this
// filter is UNVERIFIED against real data. The query uses the polymorphic
// scope filter (`scope_type=dcim.location&scope_id=<id>`), the documented
// NetBox 4.x shape. Before relying on a cage worker's prefix discovery in
// prod, create a throwaway location-scoped prefix and confirm this returns
// it (the `?location=`/`?location_id=` forms were observed to 0-match an
// unrelated probe and must not be substituted without proof). The safe
// failure mode is a loud "no slagworks-production prefix" build error,
// never silent mis-scoping.
func (c *Client) ListLocationPrefixes(ctx context.Context, locationID int, tag string) ([]Prefix, error) {
	q := url.Values{
		"scope_type": []string{"dcim.location"},
		"scope_id":   []string{strconv.Itoa(locationID)},
		"tag":        []string{tag},
		"limit":      []string{"50"},
	}
	var out []Prefix
	if err := c.List(ctx, "/api/ipam/prefixes/", q, &out); err != nil {
		return nil, fmt.Errorf("list location prefixes: %w", err)
	}
	return out, nil
}

// ListLocationsForSite returns every Location at a site (by slug). The
// objects carry Parent, so the caller can build the nesting tree for
// subtree-inclusive scope matching. Sites have a handful of locations,
// so a single call with a generous limit suffices.
func (c *Client) ListLocationsForSite(ctx context.Context, siteSlug string) ([]Location, error) {
	q := url.Values{"site": []string{siteSlug}, "limit": []string{"250"}}
	var out []Location
	if err := c.List(ctx, "/api/dcim/locations/", q, &out); err != nil {
		return nil, fmt.Errorf("list locations for site %s: %w", siteSlug, err)
	}
	return out, nil
}

// GetSiteBySlug returns the Site matching slug, or nil if none. Uses a
// slug filter rather than /api/dcim/sites/<id>/ so it works without
// knowing the integer id.
func (c *Client) GetSiteBySlug(ctx context.Context, slug string) (*Site, error) {
	q := url.Values{"slug": []string{slug}, "limit": []string{"1"}}
	var out []Site
	if err := c.List(ctx, "/api/dcim/sites/", q, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// GetDeviceByID returns the Device matching the given integer id, or nil
// if NetBox returns 404. Used by the reconciler to confirm a missing
// Device is genuinely gone before soft-decommissioning (vs "Device moved
// to another site", which the other site's reconcile pass owns).
func (c *Client) GetDeviceByID(ctx context.Context, id int) (*Device, error) {
	path := fmt.Sprintf("/api/dcim/devices/%d/", id)
	var out Device
	err := c.Get(ctx, path, nil, &out)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}
