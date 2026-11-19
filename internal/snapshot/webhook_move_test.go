package snapshot

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// webhookMoveStore is the one store the webhook move path reads, as in
// production where the claim resolver and the mirror share *store.Store:
// the site_configs (for the claim and for the mirror's invalidation) and
// a machine lookup that finds nothing, so the drift gate has no in-flight
// op to fail. The mirror.Store methods this path never reaches are left to
// the nil embedded interface, so an unexpected call panics instead of
// silently succeeding.
type webhookMoveStore struct {
	mirror.Store
	configs []model.SiteConfig
}

func (s *webhookMoveStore) ListSiteConfigs() ([]model.SiteConfig, error) {
	return append([]model.SiteConfig(nil), s.configs...), nil
}

func (s *webhookMoveStore) ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error) {
	return (&fakeClaimConfigLister{configs: s.configs}).ListSiteConfigsByNetBoxSite(netboxSite)
}

func (s *webhookMoveStore) GetMachineByNetBoxDeviceID(int) (*model.Machine, error) {
	return nil, gorm.ErrRecordNotFound
}

// signNetBoxBody signs a webhook body the way NetBox does: hex HMAC-SHA512
// keyed with the webhook secret.
func signNetBoxBody(secret, body string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookDeviceMoveInvalidatesTheConfigItLeft drives a NetBox
// device.updated webhook that moves a device between site_config scopes
// through the components production wires together (the real
// WebhookHandler, ClaimResolver and Mirror; only NetBox, the store and the
// invalidator are fakes) and checks that the config the device LEFT is
// invalidated, not just the one it moved into.
//
// Without that invalidation the old config's pushed snapshot keeps the
// machine until the next reconcile tick (up to five minutes), and the hub,
// which routes a machine's op_delta (Params carry per-site BMC/PDU
// credentials) by the cached per-slug snapshot, keeps sending the moved
// machine's ops to the old site's worker for that long.
//
// NetBox's update snapshots name site and location by id only, as here.
func TestWebhookDeviceMoveInvalidatesTheConfigItLeft(t *testing.T) {
	dualCage := []model.SiteConfig{
		{Slug: "pop2-cage-a", NetBoxSite: "pop2", NetBoxLocation: "cage-a"},
		{Slug: "pop2-cage-b", NetBoxSite: "pop2", NetBoxLocation: "cage-b"},
	}
	dualCageLocations := []netbox.Location{
		{ID: 10, Slug: "cage-a"},
		{ID: 20, Slug: "cage-b"},
		{ID: 30, Slug: "storage"}, // claimed by no config
	}

	cases := []struct {
		name      string
		configs   []model.SiteConfig
		locations []netbox.Location
		// data is the post-change device; pre/post are the snapshots.
		data, pre, post string
		left, entered   string // entered is "" when no config claims the destination
	}{
		{
			name:      "cage-a to cage-b within one site",
			configs:   dualCage,
			locations: dualCageLocations,
			data:      `{"id":7,"name":"host-7","serial":"SN-7","site":{"id":1,"slug":"pop2"},"location":{"id":20,"slug":"cage-b"}}`,
			pre:       `{"site":1,"location":10}`,
			post:      `{"site":1,"location":20}`,
			left:      "pop2-cage-a",
			entered:   "pop2-cage-b",
		},
		{
			name:      "cage-a to a location no config claims",
			configs:   dualCage,
			locations: dualCageLocations,
			data:      `{"id":7,"name":"host-7","serial":"SN-7","site":{"id":1,"slug":"pop2"},"location":{"id":30,"slug":"storage"}}`,
			pre:       `{"site":1,"location":10}`,
			post:      `{"site":1,"location":30}`,
			left:      "pop2-cage-a",
		},
		{
			name: "pop2 to another managed site",
			configs: []model.SiteConfig{
				{Slug: "pop2", NetBoxSite: "pop2"},
				{Slug: "pop3", NetBoxSite: "pop3"},
			},
			data:    `{"id":7,"name":"host-7","serial":"SN-7","site":{"id":2,"slug":"pop3"}}`,
			pre:     `{"site":1,"location":null}`,
			post:    `{"site":2,"location":null}`,
			left:    "pop2",
			entered: "pop3",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &webhookMoveStore{configs: tc.configs}
			inv := &countingInvalidator{}
			const secret = "test-webhook-secret"
			h := netbox.NewWebhookHandler(
				mirror.New(st, inv),
				NewClaimResolver(&fakeNetBox{locations: tc.locations}, st),
				secret)

			body := `{"event":"updated","object_type":"dcim.device","data":` + tc.data +
				`,"snapshots":{"prechange":` + tc.pre + `,"postchange":` + tc.post + `}}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/netbox", strings.NewReader(body))
			req.Header.Set("X-Hook-Signature", signNetBoxBody(secret, body))
			rec := httptest.NewRecorder()
			if err := h.HandleWebhook(echo.New().NewContext(req, rec)); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("webhook delivery: err=%v status=%d", err, rec.Code)
			}

			if tc.entered != "" && inv.countFor(tc.entered) == 0 {
				t.Errorf("config the device moved into (%s) was not invalidated; invalidations=%v", tc.entered, inv.counts)
			}
			if inv.countFor(tc.left) == 0 {
				t.Errorf("config the device left (%s) was not invalidated, so its snapshot keeps the machine until the next reconcile tick; invalidations=%v",
					tc.left, inv.counts)
			}
		})
	}
}
