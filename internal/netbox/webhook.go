package netbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/mirror"
)

// MirrorOps is the subset of internal/mirror.Mirror the webhook
// handler invokes. Declared as an interface so tests can supply a fake
// without spinning up the real store.
type MirrorOps interface {
	OnDeviceCreated(ctx context.Context, d mirror.Device) error
	OnDeviceUpdated(ctx context.Context, d mirror.Device) error
	OnDeviceMoved(ctx context.Context, deviceID int) error
	OnDeviceDeleted(ctx context.Context, deviceID int, configSlug string) error
	OnDeviceExternalDrift(ctx context.Context, deviceID int, fields []string, configSlug string) error
	OnSiteUpdated(ctx context.Context, oldSlug, newSlug string) error
}

// externalDriftFields are the NetBox device fields that (a) slagworks
// NEVER writes itself and (b) feed op-critical decisions — the scope
// a machine is claimed under, the scripts it matches, the identity it
// deploys as. A mid-operation change to any of these means the op is
// running against invalidated data (B7): the drift handler fails the
// in-flight ops. Fields slagworks's own commission/deploy sync writes
// (serial, name, status, asset_tag, custom_fields, tags, primary_ip4)
// are deliberately absent so self-writes never trip this.
var externalDriftFields = []string{"site", "location", "rack", "device_type", "role"}

// movesClaim reports whether drift touches a field the site_config claim
// is evaluated on (site and location; see snapshot.Scope.ClaimsDevice),
// i.e. whether the device may have left the scope of a config that claimed
// it before the change.
func movesClaim(drift []string) bool {
	return slices.Contains(drift, "site") || slices.Contains(drift, "location")
}

// diffExternalDrift compares the webhook's prechange/postchange
// snapshots on the external-drift field set, returning the fields
// whose values differ. Missing snapshots (NetBox omits them for some
// event types) diff as empty — no drift detected, reconcile catches
// anything real.
func diffExternalDrift(pre, post json.RawMessage) []string {
	if len(pre) == 0 || len(post) == 0 {
		return nil
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(pre, &before); err != nil {
		return nil
	}
	if err := json.Unmarshal(post, &after); err != nil {
		return nil
	}
	var changed []string
	for _, f := range externalDriftFields {
		if !bytes.Equal(canonicalJSON(before[f]), canonicalJSON(after[f])) {
			changed = append(changed, f)
		}
	}
	return changed
}

// canonicalJSON re-marshals a raw value so semantically-equal
// encodings (key order, whitespace) compare equal; unparseable or
// absent values canonicalize to nil.
func canonicalJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if string(out) == "null" {
		return nil
	}
	return out
}

// ClaimResolver answers which site_config(s) claim a device (site +
// location-subtree). The webhook path, unlike the reconciler, hands the
// mirror a raw device with no pre-scoping, so it resolves the claim here
// before dispatching. Satisfied by *snapshot.ClaimResolver; declared as
// an interface in this package so internal/netbox does not import
// internal/snapshot (the dependency points snapshot -> netbox only).
type ClaimResolver interface {
	ClaimingConfigs(ctx context.Context, d Device) ([]string, error)
}

// WebhookHandler receives NetBox event webhooks and delegates to the
// mirror. It verifies the HMAC signature on incoming requests, parses
// the payload, and dispatches based on event type.
//
// NetBox doesn't retry failed webhooks (see the SSoT spec), so a 5xx
// response only loses the event with extra steps. Every well-formed
// request is therefore acked with 200 regardless of downstream
// success; processing failures are logged at ERROR and the 5-min
// reconcile loop is the recovery path. The only non-200s are the 503
// while no secret is configured, request defects (bad signature,
// malformed payload) and the deliberate 409 on a slug rename.
type WebhookHandler struct {
	mir      MirrorOps
	resolver ClaimResolver
	secret   string
	log      *slog.Logger
}

func NewWebhookHandler(mir MirrorOps, resolver ClaimResolver, secret string) *WebhookHandler {
	return &WebhookHandler{mir: mir, resolver: resolver, secret: secret, log: slog.Default()}
}

func (h *WebhookHandler) SetLogger(log *slog.Logger) { h.log = log }

// webhookDisabledMessage is the 503 body for every delivery while global
// runs without a webhook secret.
const webhookDisabledMessage = "NetBox webhook receiver is disabled: SLAGWORKS_WEBHOOK_SECRET is not set on slagworksd global. " +
	"Set it to the secret configured on this NetBox webhook and restart global; " +
	"until then device changes reach slagworks only through the 5-minute reconcile."

// webhookEnvelope is the outer shape NetBox sends.
type webhookEnvelope struct {
	Event      string          `json:"event"`       // "created" | "updated" | "deleted"
	ObjectType string          `json:"object_type"` // "dcim.device" | "dcim.site" | ...
	Timestamp  string          `json:"timestamp"`
	Username   string          `json:"username"`
	RequestID  string          `json:"request_id"`
	Data       json.RawMessage `json:"data"`
	Snapshots  struct {
		PreChange  json.RawMessage `json:"prechange"`
		PostChange json.RawMessage `json:"postchange"`
	} `json:"snapshots"`
}

// HandleWebhook is the Echo handler registered at POST /api/v1/hooks/netbox.
func (h *WebhookHandler) HandleWebhook(c *echo.Context) error {
	// Fail closed, before reading the body: without a secret no delivery
	// can be authenticated, so none is processed (an HMAC keyed with ""
	// is computable by anyone).
	if h.secret == "" {
		h.log.Warn("netbox webhook rejected: SLAGWORKS_WEBHOOK_SECRET is not set", "remote", c.RealIP())
		return echo.NewHTTPError(http.StatusServiceUnavailable, webhookDisabledMessage)
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "read body")
	}

	sig := c.Request().Header.Get("X-Hook-Signature")
	if !verifyHMAC(body, sig, h.secret) {
		h.log.Warn("netbox webhook bad signature", "remote", c.RealIP())
		return echo.NewHTTPError(http.StatusForbidden, "bad signature")
	}

	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "parse envelope")
	}

	ctx := c.Request().Context()
	switch env.ObjectType {
	case "dcim.device":
		return h.handleDevice(ctx, c, env)
	case "dcim.site":
		return h.handleSite(ctx, c, env)
	default:
		// Other object types (interfaces, IP addresses, prefixes) are
		// picked up by the 5-min reconcile. Ack so NetBox doesn't retry.
		h.log.Debug("netbox webhook for unhandled object type; ignoring",
			"object_type", env.ObjectType, "event", env.Event)
		return c.NoContent(http.StatusOK)
	}
}

// mirrorDevice projects a netbox.Device onto the slim mirror.Device the
// mirror API takes, stamping the claiming config slug as the invalidation
// key. configSlug is "" for the delete path (the row is keyed by device id;
// the empty key is a deliberate no-op invalidate).
func mirrorDevice(d Device, configSlug string) mirror.Device {
	out := mirror.Device{ID: d.ID, Name: d.Name, Serial: d.Serial, ConfigSlug: configSlug}
	if d.Site != nil {
		out.SiteSlug = d.Site.Slug
	}
	return out
}

func (h *WebhookHandler) handleDevice(ctx context.Context, c *echo.Context, env webhookEnvelope) error {
	// Parse the full NetBox Device (incl. site + location) so the claim
	// resolver can evaluate location-subtree membership — the webhook path
	// has no pre-scoping, unlike the reconciler.
	var dev Device
	if err := json.Unmarshal(env.Data, &dev); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "parse device")
	}

	// The external topology fields this update changed (none for other
	// events).
	var drift []string
	if env.Event == "updated" {
		drift = diffExternalDrift(env.Snapshots.PreChange, env.Snapshots.PostChange)
	}

	// A site/location change can take the device out of the config that
	// claimed it before. The claim resolved below names only the config
	// that claims it now (or none), so OnDeviceMoved invalidates the config
	// it left (every config: see its doc). That needs no claim, so it runs
	// first, even when resolving the claim fails: otherwise the old
	// config's snapshot, and the hub's op_delta routing that trusts it,
	// keep the machine until reconcile.
	if movesClaim(drift) {
		if err := h.mir.OnDeviceMoved(ctx, dev.ID); err != nil {
			h.log.Error("invalidating site configs after a device move failed; the config the device left keeps it until reconcile",
				"device_id", dev.ID, "fields", drift, "err", err)
		}
	}

	// Resolve which config(s) claim this device. Empty = the device's site
	// is unmanaged, or its location is outside every config's claim (e.g. a
	// pop2 host in a cage no config owns). Reject quietly with 200 so NetBox
	// doesn't retry a condition it can't fix.
	claimed, err := h.resolver.ClaimingConfigs(ctx, dev)
	if err != nil {
		// NetBox never retries a failed webhook delivery, so a 500 here
		// just loses the event with extra steps. Ack with 200, log at
		// ERROR, and let the 5-min reconcile pick up the missed change.
		h.log.Error("webhook claim resolve failed; event dropped until reconcile",
			"device_id", dev.ID, "event", env.Event, "err", err)
		return c.NoContent(http.StatusOK)
	}

	switch env.Event {
	case "deleted":
		// Delete keys the row by device id regardless of claim; pass the
		// first claiming slug (if any) purely for the invalidation key.
		configSlug := ""
		if len(claimed) > 0 {
			configSlug = claimed[0]
		}
		if err := h.mir.OnDeviceDeleted(ctx, dev.ID, configSlug); err != nil {
			// 200 despite the error: NetBox never retries, so failing the
			// delivery only loses the event. The reconcile sweep confirms
			// gone-from-NetBox devices and finishes the job.
			h.log.Error("mirror OnDeviceDeleted failed; reconcile sweep will retry",
				"device_id", dev.ID, "err", err)
		}
		return c.NoContent(http.StatusOK)
	case "created", "updated":
		// B7 drift gate: an EXTERNAL change to a topology field
		// (site/location/rack/device_type/role — fields slagworks never
		// writes) invalidates any in-flight op on the machine. Checked
		// before the claim gate: a site/location change can move the
		// device OUT of every claimed scope, and that unclaiming is
		// itself the drift. The mirror no-ops when the machine is idle
		// or unknown.
		if len(drift) > 0 {
			slug := ""
			if len(claimed) > 0 {
				slug = claimed[0]
			}
			if err := h.mir.OnDeviceExternalDrift(ctx, dev.ID, drift, slug); err != nil {
				h.log.Error("drift-gate check failed; event still mirrored, reconcile backstops",
					"device_id", dev.ID, "fields", drift, "err", err)
			}
		}
		if len(claimed) == 0 {
			site := ""
			if dev.Site != nil {
				site = dev.Site.Slug
			}
			h.log.Info("device at unclaimed site/location; skipping",
				"site", site, "device_id", dev.ID)
			return c.NoContent(http.StatusOK)
		}
		for _, slug := range claimed {
			md := mirrorDevice(dev, slug)
			var opErr error
			if env.Event == "created" {
				opErr = h.mir.OnDeviceCreated(ctx, md)
			} else {
				opErr = h.mir.OnDeviceUpdated(ctx, md)
			}
			if opErr != nil {
				// Always 200: NetBox never retries, so a 500 only loses the
				// event. Serial-required is operator-fixable (WARN); other
				// errors are transient (ERROR) and the 5-min reconcile
				// re-mirrors the device.
				if errors.Is(opErr, mirror.ErrSerialRequired) {
					h.log.Warn("mirror device event", "event", env.Event,
						"device_id", dev.ID, "config", slug, "err", opErr)
				} else {
					h.log.Error("mirror device event failed; event dropped until reconcile",
						"event", env.Event, "device_id", dev.ID, "config", slug, "err", opErr)
				}
				return c.NoContent(http.StatusOK)
			}
		}
	}
	return c.NoContent(http.StatusOK)
}

func (h *WebhookHandler) handleSite(ctx context.Context, c *echo.Context, env webhookEnvelope) error {
	if env.Event != "updated" {
		// Site create/delete is operator-driven and manual; Plan 1 doesn't
		// mirror it. A reconcile tick surfaces stale slugs.
		return c.NoContent(http.StatusOK)
	}
	var pre, post struct {
		Slug string `json:"slug"`
	}
	if len(env.Snapshots.PreChange) > 0 {
		_ = json.Unmarshal(env.Snapshots.PreChange, &pre)
	}
	if len(env.Snapshots.PostChange) > 0 {
		_ = json.Unmarshal(env.Snapshots.PostChange, &post)
	}
	if pre.Slug == "" || post.Slug == "" {
		return c.NoContent(http.StatusOK)
	}
	if err := h.mir.OnSiteUpdated(ctx, pre.Slug, post.Slug); err != nil {
		// Slug-rename rejection is operator-facing; surface as 409 in the
		// response so someone notices, AND log loudly. NetBox won't retry
		// a 4xx.
		if errors.Is(err, mirror.ErrSlugImmutable) {
			h.log.Error("netbox site slug rename rejected",
				"old", pre.Slug, "new", post.Slug)
			return echo.NewHTTPError(http.StatusConflict, err.Error())
		}
		// 200 on transient errors — NetBox never retries a 500; the
		// reconcile tick surfaces stale slugs.
		h.log.Error("mirror OnSiteUpdated failed; event dropped until reconcile",
			"old", pre.Slug, "new", post.Slug, "err", err)
	}
	return c.NoContent(http.StatusOK)
}

func verifyHMAC(body []byte, sig, secret string) bool {
	if sig == "" {
		return false
	}
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(expected))
}
