package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/netbox"
)

// netboxBootstrap ensures the NetBox-side state slagworks depends on
// (custom fields, tags, device role, webhook + event rules) exists. It is
// idempotent; rerunning is safe. Sites are not touched here: the mirror
// handles Device enrollment via webhook + reconcile, and the operator
// marks a site as slagworks-managed by creating a site_configs row
// (slagctl site-config create).
func (s *Server) netboxBootstrap(c *echo.Context) error {
	if s.netbox == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "NetBox integration not configured")
	}
	if s.externalURL == "" {
		return echo.NewHTTPError(http.StatusPreconditionFailed,
			"bootstrap requires SLAGWORKS_EXTERNAL_URL to be set so NetBox can reach the webhook endpoint")
	}
	if s.webhookSecret == "" {
		return echo.NewHTTPError(http.StatusPreconditionFailed,
			"bootstrap requires SLAGWORKS_WEBHOOK_SECRET to be set for HMAC verification of NetBox webhooks")
	}
	webhookURL := strings.TrimRight(s.externalURL, "/") + "/api/v1/hooks/netbox"

	result, err := s.netbox.Bootstrap(c.Request().Context(), netbox.BootstrapConfig{
		WebhookURL:    webhookURL,
		WebhookSecret: s.webhookSecret,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("bootstrap failed: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"custom_fields": result.CustomFields,
		"device_role":   result.DeviceRole,
		"tag":           result.Tag,
		"profiles":      result.Profiles,
		"webhook":       result.Webhook,
		"event_rules":   result.EventRules,
	})
}

// netboxMigrateChassisSerial reconciles the legacy `chassis_serial`
// custom field with the built-in `serial` field on every
// slagworks-managed device. See netbox.MigrateChassisSerial for
// per-device behavior.
//
// Body: {"dry_run": true|false}. Defaults to dry-run when absent so a
// curl with no body is observational.
func (s *Server) netboxMigrateChassisSerial(c *echo.Context) error {
	if s.netbox == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "NetBox integration not configured")
	}

	var req struct {
		DryRun *bool `json:"dry_run,omitempty"`
	}
	// Body is optional but if present must be valid AND must use the
	// expected field names. Without DisallowUnknownFields, a typo like
	// {"dryRun": false} silently parses as an empty struct and the
	// migration defaults to dry-run — invisible to the operator.
	//
	// Decode unconditionally and treat io.EOF as "empty body, fine"
	// (rather than gating on ContentLength, which is -1 for chunked
	// encoding and HTTP/2).
	dec := json.NewDecoder(c.Request().Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("invalid request body: %v", err))
	}
	dryRun := true
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}

	result, err := s.netbox.MigrateChassisSerial(c.Request().Context(), dryRun)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("migrate failed: %v", err))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"dry_run":   dryRun,
		"examined":  result.Examined,
		"skipped":   result.Skipped,
		"migrated":  result.Migrated,
		"conflicts": result.Conflicts,
	})
}
