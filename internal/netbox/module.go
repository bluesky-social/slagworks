package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// ModuleData represents a hardware component from commission data.
type ModuleData struct {
	BayName       string          // stable identity key (e.g., "DIMM_A1", "CPU_0")
	ProfileName   string          // e.g., "Memory", "CPU", "Hard disk", "GPU", "Expansion card"
	Manufacturer  string          // resolved to Manufacturer ID
	Model         string          // ModuleType.model (includes distinguishing attrs like capacity)
	PartNumber    string          // optional
	Serial        string          // Module.serial (may be empty for CPUs, BIOS, BMC)
	FWVersion     string          // Module.custom_fields["firmware_version"]
	AttributeData json.RawMessage // profile-specific typed attributes
}

// ReconcileModules performs the 3-step module reconciliation:
// 1. Find-or-create ModuleTypes
// 2. Reconcile ModuleBays by name
// 3. Reconcile Modules (create new, update existing, delete stale)
//
// Each module's BayName must be unique within modules (CommissionToSync
// guarantees it): a bay holds one module, so two modules with one name
// cannot both be stored. Every existing bay that does not hold a wanted
// module is deleted, including extra bays that share a wanted name.
//
// A module's serial and firmware_version are authoritative, like the
// device fields in SyncCommissionData: an empty reading clears the stored
// value, so re-commission after a part swap leaves no stale serial behind.
func (c *Client) ReconcileModules(ctx context.Context, deviceID int, modules []ModuleData) error {
	// Step 1: Resolve all ModuleTypes
	typeIDs := make(map[string]int) // key: profileName + "/" + model
	for _, m := range modules {
		key := m.ProfileName + "/" + m.Model
		if _, ok := typeIDs[key]; ok {
			continue
		}
		id, err := c.findOrCreateModuleType(ctx, m)
		if err != nil {
			return fmt.Errorf("module type %q: %w", m.Model, err)
		}
		typeIDs[key] = id
	}

	// Step 2: Reconcile ModuleBays
	existingBays, err := c.listModuleBays(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("listing module bays: %w", err)
	}
	bayByName := make(map[string]ModuleBay, len(existingBays))
	for _, b := range existingBays {
		bayByName[b.Name] = b
	}

	bayIDs := make(map[string]int) // bayName -> bayID

	// Create new bays first
	for _, m := range modules {
		if existing, ok := bayByName[m.BayName]; ok {
			bayIDs[m.BayName] = existing.ID
			continue
		}
		var created ModuleBay
		body := map[string]any{
			"device":   deviceID,
			"name":     m.BayName,
			"position": m.BayName,
		}
		if err := c.Post(ctx, "/api/dcim/module-bays/", body, &created); err != nil {
			return fmt.Errorf("creating module bay %q: %w", m.BayName, err)
		}
		c.log.Info("created module bay", "name", m.BayName, "device_id", deviceID)
		bayIDs[m.BayName] = created.ID
	}

	// Step 3: Reconcile Modules
	existingModules, err := c.listModules(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("listing modules: %w", err)
	}
	moduleByBay := make(map[int]Module, len(existingModules)) // module_bay.id -> Module
	for _, mod := range existingModules {
		if mod.ModuleBay != nil {
			moduleByBay[mod.ModuleBay.ID] = mod
		}
	}

	seenModuleBays := make(map[int]bool)

	// Create/update modules
	for _, m := range modules {
		bayID := bayIDs[m.BayName]
		seenModuleBays[bayID] = true
		typeKey := m.ProfileName + "/" + m.Model
		moduleTypeID := typeIDs[typeKey]

		// serial and firmware_version are written even when empty: this
		// body also PATCHes the module already in the bay, and NetBox
		// keeps any field a PATCH omits, so skipping an empty reading
		// would leave a replaced part's values on the part in its bay.
		body := map[string]any{
			"device":      deviceID,
			"module_bay":  bayID,
			"module_type": moduleTypeID,
			"status":      "active",
			"serial":      m.Serial,
			"custom_fields": map[string]any{
				"firmware_version": emptyToNil(m.FWVersion),
			},
		}

		if existing, ok := moduleByBay[bayID]; ok {
			// Update in place
			path := fmt.Sprintf("/api/dcim/modules/%d/", existing.ID)
			var updated Module
			if err := c.Patch(ctx, path, body, &updated); err != nil {
				return fmt.Errorf("updating module in bay %q: %w", m.BayName, err)
			}
		} else {
			// Create new
			var created Module
			if err := c.Post(ctx, "/api/dcim/modules/", body, &created); err != nil {
				return fmt.Errorf("creating module in bay %q: %w", m.BayName, err)
			}
			c.log.Debug("created module", "bay", m.BayName, "type", m.Model)
		}
	}

	// Delete stale modules, then stale bays
	for bayID, mod := range moduleByBay {
		if seenModuleBays[bayID] {
			continue
		}
		path := fmt.Sprintf("/api/dcim/modules/%d/", mod.ID)
		if err := c.Delete(ctx, path); err != nil {
			c.log.Warn("failed to delete stale module", "id", mod.ID, "err", err)
		}
	}
	// Walk the listed bays, not bayByName: NetBox allows several
	// device-level bays with one name, and bayByName holds only one of them.
	for _, bay := range existingBays {
		if id, ok := bayIDs[bay.Name]; ok && id == bay.ID {
			continue
		}
		path := fmt.Sprintf("/api/dcim/module-bays/%d/", bay.ID)
		if err := c.Delete(ctx, path); err != nil {
			c.log.Warn("failed to delete stale module bay", "name", bay.Name, "id", bay.ID, "err", err)
		} else {
			c.log.Info("deleted stale module bay", "name", bay.Name, "id", bay.ID)
		}
	}

	return nil
}

func (c *Client) findOrCreateModuleType(ctx context.Context, m ModuleData) (int, error) {
	mfgID, err := c.FindOrCreateManufacturer(ctx, m.Manufacturer)
	if err != nil {
		return 0, err
	}

	profileID, err := c.resolveProfileID(ctx, m.ProfileName)
	if err != nil {
		return 0, err
	}

	// Search by manufacturer + model
	var results []ModuleType
	q := url.Values{
		"manufacturer_id": {fmt.Sprint(mfgID)},
		"model":           {m.Model},
	}
	if err := c.List(ctx, "/api/dcim/module-types/", q, &results); err != nil {
		return 0, err
	}
	if len(results) > 0 {
		return results[0].ID, nil
	}

	body := map[string]any{
		"manufacturer": mfgID,
		"model":        m.Model,
		"profile":      profileID,
	}
	if m.PartNumber != "" {
		body["part_number"] = m.PartNumber
	}
	if len(m.AttributeData) > 0 {
		body["attribute_data"] = json.RawMessage(m.AttributeData)
	}

	var created ModuleType
	if err := c.Post(ctx, "/api/dcim/module-types/", body, &created); err != nil {
		return 0, fmt.Errorf("creating module type %q: %w", m.Model, err)
	}
	c.log.Info("created module type", "model", m.Model, "manufacturer", m.Manufacturer)
	return created.ID, nil
}

func (c *Client) resolveProfileID(ctx context.Context, name string) (int, error) {
	var results []ModuleTypeProfile
	q := url.Values{"name": {name}}
	if err := c.List(ctx, "/api/dcim/module-type-profiles/", q, &results); err != nil {
		return 0, err
	}
	if len(results) == 0 {
		return 0, fmt.Errorf("module type profile %q not found", name)
	}
	return results[0].ID, nil
}

func (c *Client) listModuleBays(ctx context.Context, deviceID int) ([]ModuleBay, error) {
	var results []ModuleBay
	q := url.Values{"device_id": {fmt.Sprint(deviceID)}}
	if err := c.List(ctx, "/api/dcim/module-bays/", q, &results); err != nil {
		return nil, err
	}
	return results, nil
}

func (c *Client) listModules(ctx context.Context, deviceID int) ([]Module, error) {
	var results []Module
	q := url.Values{"device_id": {fmt.Sprint(deviceID)}}
	if err := c.List(ctx, "/api/dcim/modules/", q, &results); err != nil {
		return nil, err
	}
	return results, nil
}
