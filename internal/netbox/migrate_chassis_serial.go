package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MigrateChassisSerialResult is the per-device outcome of one
// MigrateChassisSerial pass.
type MigrateChassisSerialResult struct {
	// Total devices examined.
	Examined int
	// Devices where chassis_serial cf was empty / absent — no work needed.
	Skipped int
	// Devices where the cf and built-in serial agreed (or the built-in
	// was empty); the cf was cleared.
	Migrated []MigratedDevice
	// Devices where chassis_serial cf disagreed with the built-in serial
	// AND both were non-empty. Operator triage required; left untouched.
	Conflicts []ConflictingDevice
}

// MigratedDevice records a device whose chassis_serial cf was applied
// to the built-in serial field.
type MigratedDevice struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	OldSerial string `json:"old_serial"`
	NewSerial string `json:"new_serial"`
	ClearedCF bool   `json:"cleared_cf"`
}

// ConflictingDevice records a device where chassis_serial cf and the
// built-in serial both contain non-empty, distinct values. The
// migration leaves these alone for operator review.
type ConflictingDevice struct {
	ID              int    `json:"id"`
	Name            string `json:"name"`
	BuiltInSerial   string `json:"built_in_serial"`
	ChassisCFSerial string `json:"chassis_cf_serial"`
}

// MigrateChassisSerial walks every Device tagged "slagworks-managed"
// and reconciles the legacy `chassis_serial` custom field with the
// built-in `serial` field. Per-device behavior:
//
//   - cf empty / missing                                  → skip
//   - cf set, built-in serial empty                       → write cf → serial, clear cf
//   - cf set, built-in serial == cf                       → clear cf only (already correct)
//   - cf set, built-in serial != cf, both non-empty       → conflict; leave untouched
//
// The migration is idempotent: rerunning yields the same result minus
// previously-migrated rows.
//
// Pass dryRun=true to inspect what would happen without writing.
func (c *Client) MigrateChassisSerial(ctx context.Context, dryRun bool) (*MigrateChassisSerialResult, error) {
	q := url.Values{
		"tag":   []string{"slagworks-managed"},
		"limit": []string{"500"},
	}
	var devices []Device
	if err := c.List(ctx, "/api/dcim/devices/", q, &devices); err != nil {
		return nil, fmt.Errorf("list slagworks-managed devices: %w", err)
	}

	result := &MigrateChassisSerialResult{Examined: len(devices)}

	for _, d := range devices {
		cfChassis := readStringCF(d.CustomFields, "chassis_serial")
		if cfChassis == "" {
			result.Skipped++
			continue
		}
		builtIn := strings.TrimSpace(d.Serial)
		cfChassis = strings.TrimSpace(cfChassis)

		// Don't adopt DMI placeholders ("Default string", "To Be Filled
		// By O.E.M.", etc.) into the built-in serial field — those alias
		// across hardware and would break the rebind/identity key.
		// Treat a junk cf value the same as an unset one: clear it if
		// the built-in is also junk/empty, conflict-report it otherwise.
		cfChassisIsJunk := isJunkDMIValue(cfChassis)

		switch {
		case builtIn == "" && cfChassisIsJunk:
			// Both effectively empty; just clear the placeholder cf.
			if !dryRun {
				if err := c.patchDeviceClearChassisCF(ctx, d.ID); err != nil {
					return nil, fmt.Errorf("clear cf on device %d (%s): %w", d.ID, d.Name, err)
				}
			}
			result.Migrated = append(result.Migrated, MigratedDevice{
				ID: d.ID, Name: d.Name,
				OldSerial: "", NewSerial: "", ClearedCF: true,
			})
		case builtIn == "":
			// Adopt the cf value.
			if !dryRun {
				if err := c.patchDeviceSerialAndClearCF(ctx, d.ID, cfChassis); err != nil {
					return nil, fmt.Errorf("patch device %d (%s): %w", d.ID, d.Name, err)
				}
			}
			result.Migrated = append(result.Migrated, MigratedDevice{
				ID: d.ID, Name: d.Name,
				OldSerial: "", NewSerial: cfChassis, ClearedCF: true,
			})
		case builtIn == cfChassis:
			// Already correct; just clear the redundant cf.
			if !dryRun {
				if err := c.patchDeviceClearChassisCF(ctx, d.ID); err != nil {
					return nil, fmt.Errorf("clear cf on device %d (%s): %w", d.ID, d.Name, err)
				}
			}
			result.Migrated = append(result.Migrated, MigratedDevice{
				ID: d.ID, Name: d.Name,
				OldSerial: builtIn, NewSerial: builtIn, ClearedCF: true,
			})
		default:
			result.Conflicts = append(result.Conflicts, ConflictingDevice{
				ID: d.ID, Name: d.Name,
				BuiltInSerial: builtIn, ChassisCFSerial: cfChassis,
			})
		}
	}

	return result, nil
}

func (c *Client) patchDeviceSerialAndClearCF(ctx context.Context, id int, newSerial string) error {
	body := map[string]any{
		"serial": newSerial,
		"custom_fields": map[string]any{
			"chassis_serial": nil,
		},
	}
	var out json.RawMessage
	return c.Patch(ctx, fmt.Sprintf("/api/dcim/devices/%d/", id), body, &out)
}

func (c *Client) patchDeviceClearChassisCF(ctx context.Context, id int) error {
	body := map[string]any{
		"custom_fields": map[string]any{
			"chassis_serial": nil,
		},
	}
	var out json.RawMessage
	return c.Patch(ctx, fmt.Sprintf("/api/dcim/devices/%d/", id), body, &out)
}

// readStringCF extracts a string value from a Device's CustomFields
// blob without unmarshaling the whole shape into a typed struct.
// Returns "" if the key is absent or non-string.
func readStringCF(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}
