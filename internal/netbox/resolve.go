package netbox

import (
	"context"
	"fmt"
)

// ResolveRequest contains the identifiers to match against NetBox devices,
// plus creation fields used if no match is found.
type ResolveRequest struct {
	SlagworksID  string   // machine UUID custom field (if known)
	Serial       string   // from CommissionResult.System.SerialNumber
	MACs         []string // from CommissionResult.NICs[*].MAC
	Hostname     string   // device name for creation
	SiteID       int      // NetBox site ID for creation
	Manufacturer string   // from CommissionResult.System.Manufacturer; falls back to "Unknown"
	ProductName  string   // from CommissionResult.System.ProductName; used as device type model
}

// ResolveDevice finds or creates a NetBox device using the resolution chain:
//  1. slagworks_id custom field
//  2. serial number
//  3. MAC address on any interface
//  4. create new device
//
// Returns (device, created, error). created is true only when a new device
// was created in step 4.
func (c *Client) ResolveDevice(ctx context.Context, req ResolveRequest) (*Device, bool, error) {
	if req.SlagworksID != "" {
		d, err := c.FindDeviceBySlagworksID(ctx, req.SlagworksID)
		if err != nil {
			return nil, false, fmt.Errorf("lookup by slagworks_id: %w", err)
		}
		if d != nil {
			return d, false, nil
		}
	}

	if req.Serial != "" {
		devices, err := c.GetDevicesBySerial(ctx, req.Serial)
		if err != nil {
			return nil, false, fmt.Errorf("lookup by serial: %w", err)
		}
		if len(devices) > 0 {
			return &devices[0], false, nil
		}
	}

	if len(req.MACs) > 0 {
		d, err := c.FindDeviceByMAC(ctx, req.MACs)
		if err != nil {
			return nil, false, fmt.Errorf("lookup by MAC: %w", err)
		}
		if d != nil {
			return d, false, nil
		}
	}

	mfgName := req.Manufacturer
	if mfgName == "" {
		mfgName = "Unknown"
	}
	dtModel := req.ProductName
	if dtModel == "" {
		dtModel = "Unknown"
	}

	mfgID, err := c.FindOrCreateManufacturer(ctx, mfgName)
	if err != nil {
		return nil, false, fmt.Errorf("find/create manufacturer: %w", err)
	}
	dtID, err := c.FindOrCreateDeviceType(ctx, mfgID, dtModel)
	if err != nil {
		return nil, false, fmt.Errorf("find/create device type: %w", err)
	}
	roleID, err := c.FindDeviceRoleBySlug(ctx, "server")
	if err != nil {
		return nil, false, fmt.Errorf("find device role: %w", err)
	}

	fields := map[string]any{
		"name":        req.Hostname,
		"site":        req.SiteID,
		"device_type": dtID,
		"role":        roleID,
	}
	d, err := c.CreateDevice(ctx, fields)
	if err != nil {
		return nil, false, fmt.Errorf("create device: %w", err)
	}
	return d, true, nil
}
