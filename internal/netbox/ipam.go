package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// AllocateIPResult holds the allocated IP details from NetBox IPAM.
type AllocateIPResult struct {
	ID      int    // NetBox IPAddress ID (for later deletion)
	Address string // e.g., "10.10.1.42/24"
}

// AllocateIP allocates an available IP from a prefix in NetBox IPAM.
// The IP is created as "active" and assigned to the given interface.
func (c *Client) AllocateIP(ctx context.Context, prefixID, interfaceID int) (*AllocateIPResult, error) {
	body := map[string]any{
		"status":               "active",
		"assigned_object_type": "dcim.interface",
		"assigned_object_id":   interfaceID,
	}

	var created IPAddress
	path := fmt.Sprintf("/api/ipam/prefixes/%d/available-ips/", prefixID)
	if err := c.Post(ctx, path, body, &created); err != nil {
		return nil, fmt.Errorf("allocating IP from prefix %d: %w", prefixID, err)
	}

	c.log.Info("allocated IP", "address", created.Address, "prefix_id", prefixID, "id", created.ID)
	return &AllocateIPResult{
		ID:      created.ID,
		Address: created.Address,
	}, nil
}

// ReleaseIP deletes an IPAddress from NetBox IPAM.
func (c *Client) ReleaseIP(ctx context.Context, ipAddressID int) error {
	path := fmt.Sprintf("/api/ipam/ip-addresses/%d/", ipAddressID)
	if err := c.Delete(ctx, path); err != nil {
		return fmt.Errorf("releasing IP %d: %w", ipAddressID, err)
	}
	c.log.Info("released IP", "id", ipAddressID)
	return nil
}

// SetDevicePrimaryIP sets the primary_ip4 on a device.
func (c *Client) SetDevicePrimaryIP(ctx context.Context, deviceID, ipAddressID int) error {
	return c.UpdateDevice(ctx, deviceID, map[string]any{
		"primary_ip4": ipAddressID,
	})
}

// PrefixInfo holds essential details about a NetBox prefix.
type PrefixInfo struct {
	ID        int
	Prefix    string
	SiteID    int
	GatewayIP string // from custom_fields.gateway_ip
}

// FindPrefixesByTag finds all prefixes with the given tag, optionally filtered by site.
func (c *Client) FindPrefixesByTag(ctx context.Context, tag string, siteID *int) ([]PrefixInfo, error) {
	q := url.Values{"tag": {tag}}
	if siteID != nil {
		q.Set("site_id", fmt.Sprint(*siteID))
	}

	var prefixes []Prefix
	if err := c.List(ctx, "/api/ipam/prefixes/", q, &prefixes); err != nil {
		return nil, fmt.Errorf("finding prefixes with tag %q: %w", tag, err)
	}

	var results []PrefixInfo
	for _, p := range prefixes {
		info := PrefixInfo{
			ID:     p.ID,
			Prefix: p.Prefix,
		}
		if p.Site != nil {
			info.SiteID = p.Site.ID
		}

		// Extract gateway_ip from custom fields
		if len(p.CustomFields) > 0 {
			var cf map[string]any
			if err := json.Unmarshal(p.CustomFields, &cf); err == nil {
				if gw, ok := cf["gateway_ip"].(string); ok {
					info.GatewayIP = gw
				}
			}
		}

		results = append(results, info)
	}

	return results, nil
}

// EnsureIPRange creates an IPRange with mark_populated if it doesn't exist.
// Used to reserve DHCP pool ranges so NetBox doesn't allocate from them.
func (c *Client) EnsureIPRange(ctx context.Context, startAddr, endAddr string, description string) error {
	// Check if range already exists
	var ranges []IPRange
	q := url.Values{"start_address": {startAddr}, "end_address": {endAddr}}
	if err := c.List(ctx, "/api/ipam/ip-ranges/", q, &ranges); err != nil {
		return err
	}
	if len(ranges) > 0 {
		return nil
	}

	body := map[string]any{
		"start_address": startAddr,
		"end_address":   endAddr,
		"mark_utilized": true,
		"description":   description,
	}
	var created IPRange
	if err := c.Post(ctx, "/api/ipam/ip-ranges/", body, &created); err != nil {
		return fmt.Errorf("creating IP range: %w", err)
	}
	c.log.Info("created IP range", "start", startAddr, "end", endAddr)
	return nil
}
