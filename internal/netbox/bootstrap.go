package netbox

import (
	"context"
	"fmt"
	"net/url"
)

// BootstrapConfig holds the configuration needed for bootstrapping.
type BootstrapConfig struct {
	WebhookURL    string
	WebhookSecret string
}

// BootstrapResult reports what was created or verified during bootstrap.
type BootstrapResult struct {
	CustomFields []string
	DeviceRole   string
	Tag          string
	Webhook      string
	EventRules   []string
	Profiles     []string
}

// Bootstrap ensures all required NetBox objects exist for slagworks integration.
// It is idempotent: objects that already exist are left unchanged.
func (c *Client) Bootstrap(ctx context.Context, cfg BootstrapConfig) (*BootstrapResult, error) {
	if err := c.CheckVersion(ctx, 4, 3); err != nil {
		return nil, err
	}

	result := &BootstrapResult{}

	// Custom fields
	fields := []struct {
		name        string
		fieldType   string
		objectTypes []string
		label       string
		description string
	}{
		{"slagworks_id", "text", []string{"dcim.device"}, "Slagworks ID", ""},
		{"board_vendor", "text", []string{"dcim.device"}, "Board Vendor", ""},
		{"board_product", "text", []string{"dcim.device"}, "Board Product", ""},
		{"board_serial", "text", []string{"dcim.device"}, "Board Serial", ""},
		{"chassis_type", "text", []string{"dcim.device"}, "Chassis Type", ""},
		// chassis_serial is intentionally NOT a custom field. The DMI
		// chassis serial number (Type 3) populates NetBox's built-in
		// `serial` field on the Device. Most server hardware reports
		// the same value for system serial (DMI Type 1) and chassis
		// serial; the chassis value is the authoritative one (NetBox
		// docs describe `serial` as the chassis serial number).
		{"firmware_version", "text", []string{"dcim.module"}, "Firmware Version", ""},
		{"gateway_ip", "text", []string{"ipam.prefix"}, "Gateway IP", ""},
		{
			"slagworks_kernel_params", "text", []string{"dcim.devicetype"},
			"Slagworks Kernel Params",
			`Linux kernel cmdline (e.g. "console=ttyS1,115200n8"). "none" = no SOL. Empty refuses deploy.`,
		},
	}

	for _, f := range fields {
		if err := c.ensureCustomField(ctx, f.name, f.fieldType, f.objectTypes, f.label, f.description); err != nil {
			return nil, fmt.Errorf("custom field %q: %w", f.name, err)
		}
		result.CustomFields = append(result.CustomFields, f.name)
	}

	// Device role: "server"
	if err := c.ensureDeviceRole(ctx, "server", "server"); err != nil {
		return nil, fmt.Errorf("device role: %w", err)
	}
	result.DeviceRole = "server"

	// Tag: "slagworks-managed" — applied to Devices slagworks manages.
	if err := c.ensureTag(ctx, "slagworks-managed", "slagworks-managed"); err != nil {
		return nil, fmt.Errorf("tag: %w", err)
	}
	result.Tag = "slagworks-managed"

	// Tag: "slagworks-production" — applied to the Prefix that defines
	// the production subnet for a site (consumed by the snapshot builder
	// for DHCP config).
	if err := c.ensureTag(ctx, "slagworks-production", "slagworks-production"); err != nil {
		return nil, fmt.Errorf("tag slagworks-production: %w", err)
	}

	// Verify required module type profiles exist
	requiredProfiles := []string{"CPU", "GPU", "Hard disk", "Memory", "Expansion card"}
	for _, name := range requiredProfiles {
		if err := c.verifyModuleTypeProfile(ctx, name); err != nil {
			return nil, fmt.Errorf("module type profile %q: %w", name, err)
		}
		result.Profiles = append(result.Profiles, name)
	}

	// Webhook and event rules
	if cfg.WebhookURL != "" {
		webhookID, err := c.ensureWebhook(ctx, cfg.WebhookURL, cfg.WebhookSecret)
		if err != nil {
			return nil, fmt.Errorf("webhook: %w", err)
		}
		result.Webhook = cfg.WebhookURL

		rules := []struct {
			name   string
			events []string
		}{
			{"Slagworks device create", []string{"object_created"}},
			{"Slagworks device update", []string{"object_updated"}},
			{"Slagworks device delete", []string{"object_deleted"}},
		}

		for _, r := range rules {
			if err := c.ensureEventRule(ctx, r.name, webhookID, r.events); err != nil {
				return nil, fmt.Errorf("event rule %q: %w", r.name, err)
			}
			result.EventRules = append(result.EventRules, r.name)
		}
	}

	return result, nil
}

func (c *Client) ensureCustomField(ctx context.Context, name, fieldType string, objectTypes []string, label, description string) error {
	var fields []CustomField
	q := url.Values{"name": {name}}
	if err := c.List(ctx, "/api/extras/custom-fields/", q, &fields); err != nil {
		return err
	}

	if len(fields) > 0 {
		c.log.Info("custom field exists", "name", name)
		return nil
	}

	body := map[string]any{
		"name":          name,
		"type":          fieldType,
		"object_types":  objectTypes,
		"label":         label,
		"description":   description,
		"required":      false,
		"search_weight": 1000,
		"ui_visible":    "always",
		"ui_editable":   "yes",
	}

	var created CustomField
	if err := c.Post(ctx, "/api/extras/custom-fields/", body, &created); err != nil {
		return err
	}
	c.log.Info("created custom field", "name", name, "id", created.ID)
	return nil
}

func (c *Client) ensureDeviceRole(ctx context.Context, name, slug string) error {
	var roles []DeviceRole
	q := url.Values{"slug": {slug}}
	if err := c.List(ctx, "/api/dcim/device-roles/", q, &roles); err != nil {
		return err
	}

	if len(roles) > 0 {
		c.log.Info("device role exists", "slug", slug)
		return nil
	}

	body := map[string]any{
		"name": name,
		"slug": slug,
	}
	var created DeviceRole
	if err := c.Post(ctx, "/api/dcim/device-roles/", body, &created); err != nil {
		return err
	}
	c.log.Info("created device role", "slug", slug, "id", created.ID)
	return nil
}

func (c *Client) ensureTag(ctx context.Context, name, slug string) error {
	var tags []Tag
	q := url.Values{"slug": {slug}}
	if err := c.List(ctx, "/api/extras/tags/", q, &tags); err != nil {
		return err
	}

	if len(tags) > 0 {
		c.log.Info("tag exists", "slug", slug)
		return nil
	}

	body := map[string]any{
		"name": name,
		"slug": slug,
	}
	var created Tag
	if err := c.Post(ctx, "/api/extras/tags/", body, &created); err != nil {
		return err
	}
	c.log.Info("created tag", "slug", slug, "id", created.ID)
	return nil
}

func (c *Client) verifyModuleTypeProfile(ctx context.Context, name string) error {
	var profiles []ModuleTypeProfile
	q := url.Values{"name": {name}}
	if err := c.List(ctx, "/api/dcim/module-type-profiles/", q, &profiles); err != nil {
		return err
	}

	if len(profiles) == 0 {
		return fmt.Errorf("required profile %q not found in NetBox (expected default profile from NetBox 4.3+)", name)
	}
	c.log.Info("module type profile verified", "name", name)
	return nil
}

func (c *Client) ensureWebhook(ctx context.Context, payloadURL, secret string) (int, error) {
	var webhooks []Webhook
	q := url.Values{"name": {"slagworks-enrollment"}}
	if err := c.List(ctx, "/api/extras/webhooks/", q, &webhooks); err != nil {
		return 0, err
	}

	body := map[string]any{
		"name":              "slagworks-enrollment",
		"payload_url":       payloadURL,
		"http_method":       "POST",
		"http_content_type": "application/json",
		"secret":            secret,
		"ssl_verification":  true,
	}

	if len(webhooks) > 0 {
		// Update existing webhook to match config.
		var updated Webhook
		path := fmt.Sprintf("/api/extras/webhooks/%d/", webhooks[0].ID)
		if err := c.Patch(ctx, path, body, &updated); err != nil {
			return 0, err
		}
		c.log.Info("updated webhook", "id", updated.ID)
		return updated.ID, nil
	}

	var created Webhook
	if err := c.Post(ctx, "/api/extras/webhooks/", body, &created); err != nil {
		return 0, err
	}
	c.log.Info("created webhook", "id", created.ID)
	return created.ID, nil
}

func (c *Client) ensureEventRule(ctx context.Context, name string, webhookID int, events []string) error {
	var rules []EventRule
	q := url.Values{"name": {name}}
	if err := c.List(ctx, "/api/extras/event-rules/", q, &rules); err != nil {
		return err
	}

	body := map[string]any{
		"name":               name,
		"object_types":       []string{"dcim.device"},
		"event_types":        events,
		"enabled":            true,
		"action_type":        "webhook",
		"action_object_type": "extras.webhook",
		"action_object_id":   webhookID,
		"conditions": map[string]any{
			"and": []map[string]any{
				{"attr": "role.slug", "value": "server"},
			},
		},
	}

	if len(rules) > 0 {
		var updated EventRule
		path := fmt.Sprintf("/api/extras/event-rules/%d/", rules[0].ID)
		if err := c.Patch(ctx, path, body, &updated); err != nil {
			return err
		}
		c.log.Info("updated event rule", "name", name, "id", updated.ID)
		return nil
	}

	var created EventRule
	if err := c.Post(ctx, "/api/extras/event-rules/", body, &created); err != nil {
		return err
	}
	c.log.Info("created event rule", "name", name, "id", created.ID)
	return nil
}
