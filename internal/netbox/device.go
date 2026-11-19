package netbox

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// FindOrCreateManufacturer returns the manufacturer ID, creating it if needed.
//
// Lookup tries name first, then slug: NetBox enforces uniqueness on
// both, and a pre-existing row can carry a slug that differs from
// slugify(name) (hand-created or imported). Querying only by computed
// slug meant find-before-create could miss a row the POST would then
// collide with.
func (c *Client) FindOrCreateManufacturer(ctx context.Context, name string) (int, error) {
	if name == "" {
		name = "Unknown"
	}
	slug := slugify(name)
	queries := []url.Values{
		{"name": {name}},
		{"slug": {slug}},
	}

	find := func() (int, bool, error) {
		for _, q := range queries {
			var results []Manufacturer
			if err := c.List(ctx, "/api/dcim/manufacturers/", q, &results); err != nil {
				return 0, false, fmt.Errorf("finding manufacturer %q: %w", name, err)
			}
			if len(results) > 0 {
				return results[0].ID, true, nil
			}
		}
		return 0, false, nil
	}

	if id, ok, err := find(); err != nil || ok {
		return id, err
	}

	var created Manufacturer
	body := map[string]string{"name": name, "slug": slug}
	if err := c.Post(ctx, "/api/dcim/manufacturers/", body, &created); err != nil {
		// Retry on conflict: another instance may have created it concurrently
		if isConflictError(err) {
			if id, ok, ferr := find(); ferr == nil && ok {
				return id, nil
			}
		}
		return 0, fmt.Errorf("creating manufacturer %q: %w", name, err)
	}
	c.log.Info("created manufacturer", "name", name, "id", created.ID)
	return created.ID, nil
}

// FindOrCreateDeviceType returns the device type ID, creating it if needed.
//
// Lookup tries (manufacturer, model) first — the pair NetBox enforces
// uniqueness on — then (manufacturer, slug). A device type created
// outside slagworks can have any slug (the curated AS-1115HS-TNR row
// is "supermicro-as-1115hs-tnr", not slugify's "as-1115hs-tnr");
// matching on slug alone meant find-before-create missed, the POST
// 400'd on the model constraint, and the conflict-recovery re-ran the
// same missing slug query, wedging every commission of that hardware
// model in the site worker's outbox retry loop.
func (c *Client) FindOrCreateDeviceType(ctx context.Context, manufacturerID int, model string) (int, error) {
	slug := slugify(model)
	mfg := fmt.Sprint(manufacturerID)
	queries := []url.Values{
		{"manufacturer_id": {mfg}, "model": {model}},
		{"manufacturer_id": {mfg}, "slug": {slug}},
	}

	find := func() (int, bool, error) {
		for _, q := range queries {
			var results []DeviceType
			if err := c.List(ctx, "/api/dcim/device-types/", q, &results); err != nil {
				return 0, false, fmt.Errorf("finding device type %q: %w", model, err)
			}
			if len(results) > 0 {
				return results[0].ID, true, nil
			}
		}
		return 0, false, nil
	}

	if id, ok, err := find(); err != nil || ok {
		return id, err
	}

	var created DeviceType
	body := map[string]any{
		"manufacturer": manufacturerID,
		"model":        model,
		"slug":         slug,
	}
	if err := c.Post(ctx, "/api/dcim/device-types/", body, &created); err != nil {
		if isConflictError(err) {
			if id, ok, ferr := find(); ferr == nil && ok {
				return id, nil
			}
		}
		return 0, fmt.Errorf("creating device type %q: %w", model, err)
	}
	c.log.Info("created device type", "model", model, "id", created.ID)
	return created.ID, nil
}

// UpdateDevice patches a device with the given fields.
func (c *Client) UpdateDevice(ctx context.Context, deviceID int, fields map[string]any) error {
	var updated Device
	path := fmt.Sprintf("/api/dcim/devices/%d/", deviceID)
	return c.Patch(ctx, path, fields, &updated)
}

// GetDevice fetches a single device by ID.
func (c *Client) GetDevice(ctx context.Context, deviceID int) (*Device, error) {
	var d Device
	path := fmt.Sprintf("/api/dcim/devices/%d/", deviceID)
	if err := c.Get(ctx, path, nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// AmbiguousNameError reports that a device name matched more than one
// device. NetBox enforces name uniqueness only within a site (and
// tenant), so a name such as `pdu-0102-left` can exist in several sites.
type AmbiguousNameError struct {
	Name    string
	Devices []Device
}

func (e *AmbiguousNameError) Error() string {
	cands := make([]string, len(e.Devices))
	for i, d := range e.Devices {
		site := "no site"
		if d.Site != nil {
			site = "site " + d.Site.Slug
		}
		cands[i] = fmt.Sprintf("id %d (%s)", d.ID, site)
	}
	return fmt.Sprintf("NetBox device name %q matches %d devices: %s",
		e.Name, len(e.Devices), strings.Join(cands, ", "))
}

// FindDeviceByName searches for a device by exact name. When siteID > 0
// the search is scoped to that site; when siteID == 0 it runs across all
// sites. Returns nil, nil when nothing matches and an
// *AmbiguousNameError when several devices carry the name, so a caller
// never acts on one of them by accident.
func (c *Client) FindDeviceByName(ctx context.Context, name string, siteID int) (*Device, error) {
	var results []Device
	q := url.Values{"name": {name}}
	if siteID > 0 {
		q.Set("site_id", fmt.Sprint(siteID))
	}
	if err := c.List(ctx, "/api/dcim/devices/", q, &results); err != nil {
		return nil, err
	}
	switch len(results) {
	case 0:
		return nil, nil
	case 1:
		return &results[0], nil
	default:
		return nil, &AmbiguousNameError{Name: name, Devices: results}
	}
}

// FindDevicesByNameSubstring searches for devices whose name contains
// the given pattern (case-insensitive). NetBox exposes this via the
// `name__ic` filter ("ic" = insensitive contains). Used by slagctl's
// hostname-substring resolver so operators can type `slagctl machine
// deploy cpu1` and get a single match across hostnames like
// `cpu1.pop1` / `cpu1.pop3`.
//
// siteID==0 searches across all sites; siteID>0 scopes the search.
func (c *Client) FindDevicesByNameSubstring(ctx context.Context, pattern string, siteID int) ([]Device, error) {
	var results []Device
	q := url.Values{"name__ic": {pattern}}
	if siteID > 0 {
		q.Set("site_id", fmt.Sprint(siteID))
	}
	if err := c.List(ctx, "/api/dcim/devices/", q, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// GetDevicesBySerial returns all devices matching the given serial number.
func (c *Client) GetDevicesBySerial(ctx context.Context, serial string) ([]Device, error) {
	var results []Device
	q := url.Values{"serial": {serial}}
	if err := c.List(ctx, "/api/dcim/devices/", q, &results); err != nil {
		return nil, err
	}
	return results, nil
}

// FindSiteBySlug returns the NetBox site matching the given slug, or nil if not found.
func (c *Client) FindSiteBySlug(ctx context.Context, slug string) (*Site, error) {
	var results []Site
	q := url.Values{"slug": {slug}}
	if err := c.List(ctx, "/api/dcim/sites/", q, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return &results[0], nil
}

// FindDeviceRoleBySlug returns the device role ID for the given slug, or 0 if not found.
func (c *Client) FindDeviceRoleBySlug(ctx context.Context, slug string) (int, error) {
	var results []DeviceRole
	q := url.Values{"slug": {slug}}
	if err := c.List(ctx, "/api/dcim/device-roles/", q, &results); err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, fmt.Errorf("device role %q not found", slug)
	}
	return results[0].ID, nil
}

// CreateDevice creates a new device in NetBox and returns it.
func (c *Client) CreateDevice(ctx context.Context, fields map[string]any) (*Device, error) {
	var created Device
	if err := c.Post(ctx, "/api/dcim/devices/", fields, &created); err != nil {
		return nil, err
	}
	return &created, nil
}

// FindDeviceBySlagworksID searches for a device by the cf_slagworks_id custom field.
// Returns nil, nil if not found.
func (c *Client) FindDeviceBySlagworksID(ctx context.Context, slagworksID string) (*Device, error) {
	var results []Device
	params := url.Values{"cf_slagworks_id": {slagworksID}}
	if err := c.List(ctx, "/api/dcim/devices/", params, &results); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return &results[0], nil
}

// FindDeviceByMAC searches for a device by iterating NIC MAC addresses.
// Returns the first device found, or nil if no match.
func (c *Client) FindDeviceByMAC(ctx context.Context, macs []string) (*Device, error) {
	for _, mac := range macs {
		var ifaces []Interface
		params := url.Values{"mac_address": {mac}}
		if err := c.List(ctx, "/api/dcim/interfaces/", params, &ifaces); err != nil {
			continue
		}
		if len(ifaces) > 0 && ifaces[0].Device != nil {
			return c.GetDevice(ctx, ifaces[0].Device.ID)
		}
	}
	return nil, nil
}

// isConflictError returns true if the error is a 400 Bad Request,
// which NetBox returns for uniqueness constraint violations.
func isConflictError(err error) bool {
	apiErr, ok := err.(*APIError)
	return ok && apiErr.StatusCode == 400
}

// slugify creates a URL-safe slug from a name.
func slugify(name string) string {
	s := strings.ToLower(name)
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		if r == ' ' || r == '_' || r == '.' {
			return '-'
		}
		return -1
	}, s)
	// Collapse multiple hyphens
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if len(s) > 100 {
		s = s[:100]
	}
	s = strings.Trim(s, "-")
	if s == "" {
		s = "unknown"
	}
	return s
}
