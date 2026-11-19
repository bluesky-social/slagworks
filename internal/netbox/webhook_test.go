package netbox

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/mirror"
)

// fakeMirror records the device events the webhook handler dispatches.
// A non-nil err is returned from every mirror call (after recording).
type fakeMirror struct {
	creates []mirror.Device
	updates []mirror.Device
	moves   []int // device ids passed to OnDeviceMoved
	deletes []deleteCall
	drifts  []driftCall
	err     error
}

type deleteCall struct {
	deviceID   int
	configSlug string
}

func (f *fakeMirror) OnDeviceCreated(_ context.Context, d mirror.Device) error {
	f.creates = append(f.creates, d)
	return f.err
}
func (f *fakeMirror) OnDeviceUpdated(_ context.Context, d mirror.Device) error {
	f.updates = append(f.updates, d)
	return f.err
}
func (f *fakeMirror) OnDeviceMoved(_ context.Context, deviceID int) error {
	f.moves = append(f.moves, deviceID)
	return f.err
}
func (f *fakeMirror) OnDeviceDeleted(_ context.Context, deviceID int, configSlug string) error {
	f.deletes = append(f.deletes, deleteCall{deviceID, configSlug})
	return f.err
}
func (f *fakeMirror) OnSiteUpdated(_ context.Context, _, _ string) error { return f.err }

type driftCall struct {
	deviceID   int
	fields     []string
	configSlug string
}

func (f *fakeMirror) OnDeviceExternalDrift(_ context.Context, deviceID int, fields []string, configSlug string) error {
	f.drifts = append(f.drifts, driftCall{deviceID, fields, configSlug})
	return f.err
}

// fakeResolver maps a device id to the claiming config slug(s). Models a
// location-scoped config: an in-scope device resolves to its config slug
// (which differs from the NetBox site slug), an out-of-scope device
// resolves to nothing. A non-nil err fails every resolution.
type fakeResolver struct {
	byID map[int][]string
	err  error
}

func (r *fakeResolver) ClaimingConfigs(_ context.Context, d Device) ([]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.byID[d.ID], nil
}

// testSecret is the webhook secret the handler-level tests configure;
// postWebhook signs with it the way NetBox does.
const testSecret = "test-webhook-secret"

// postWebhook delivers body signed with testSecret.
func postWebhook(t *testing.T, h *WebhookHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postWebhookWithSig(t, h, body, signBody(testSecret, body))
}

// TestWebhookEnrollsClaimedLocationScopedDevice is the webhook-path
// regression for the location-scoping bug: a device whose NetBox site is
// managed by a location-scoped config (slug != netbox_site) must enroll
// via the webhook, stamped with the CONFIG slug (the invalidation key),
// not the netbox site slug.
func TestWebhookEnrollsClaimedLocationScopedDevice(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{
		777: {"pop2-old-cage"}, // device 777 is claimed by the old-cage config
	}}
	h := NewWebhookHandler(mir, res, testSecret)

	// NetBox device.created webhook for a pop2 host in the old cage.
	body := `{"event":"created","object_type":"dcim.device","data":{"id":777,"name":"spare-1","serial":"S500002X0000002","site":{"slug":"pop2"},"location":{"id":21,"slug":"pop2-01-005330"}}}`
	w := postWebhook(t, h, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if len(mir.creates) != 1 {
		t.Fatalf("expected 1 OnDeviceCreated, got %d", len(mir.creates))
	}
	got := mir.creates[0]
	if got.ID != 777 || got.Serial != "S500002X0000002" {
		t.Fatalf("unexpected device: %+v", got)
	}
	if got.ConfigSlug != "pop2-old-cage" {
		t.Fatalf("ConfigSlug: got %q want pop2-old-cage (the invalidation key must be the config slug, not the site slug)", got.ConfigSlug)
	}
	if got.SiteSlug != "pop2" {
		t.Fatalf("SiteSlug: got %q want pop2", got.SiteSlug)
	}
}

// TestWebhookRejectsUnclaimedLocation is the negative case: a device in a
// location of the same managed NetBox site that NO config claims (the new
// cage) must NOT enroll, even though netbox_site=pop2 is managed.
func TestWebhookRejectsUnclaimedLocation(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{
		// device 888 resolves to no claiming config.
	}}
	h := NewWebhookHandler(mir, res, testSecret)

	body := `{"event":"created","object_type":"dcim.device","data":{"id":888,"name":"new-cage-host","serial":"SN-NEW","site":{"slug":"pop2"},"location":{"id":99,"slug":"pop2-01-005250"}}}`
	w := postWebhook(t, h, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (NetBox must not retry an unclaimable device)", w.Code)
	}
	if len(mir.creates) != 0 {
		t.Fatalf("unclaimed-location device must not enroll, got %d creates", len(mir.creates))
	}
}

// TestWebhookDeleteKeysInvalidationOnConfigSlug confirms the delete path
// passes the claiming config slug (for the invalidation), and still
// dispatches the delete by device id.
func TestWebhookDeleteKeysInvalidationOnConfigSlug(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
	h := NewWebhookHandler(mir, res, testSecret)

	body := `{"event":"deleted","object_type":"dcim.device","data":{"id":777,"site":{"slug":"pop2"},"location":{"id":21,"slug":"pop2-01-005330"}}}`
	w := postWebhook(t, h, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if len(mir.deletes) != 1 {
		t.Fatalf("expected 1 OnDeviceDeleted, got %d", len(mir.deletes))
	}
	if mir.deletes[0].deviceID != 777 || mir.deletes[0].configSlug != "pop2-old-cage" {
		t.Fatalf("delete call: %+v want {777 pop2-old-cage}", mir.deletes[0])
	}
}

// TestWebhookProcessingErrorsAck200 pins the B9 contract: NetBox never
// retries a failed webhook delivery, so every processing failure must
// be acked with 200 (the 5-min reconcile is the recovery path). A 5xx
// would only lose the event with extra steps.
func TestWebhookProcessingErrorsAck200(t *testing.T) {
	deviceBody := `{"event":"created","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}}}`
	deleteBody := `{"event":"deleted","object_type":"dcim.device","data":{"id":777,"site":{"slug":"pop2"}}}`
	moveBody := `{"event":"updated","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"site-b"}},` +
		`"snapshots":{"prechange":{"site":1,"location":21},"postchange":{"site":1,"location":99}}}`

	cases := []struct {
		name string
		h    *WebhookHandler
		body string
	}{
		{
			name: "resolver failure",
			h:    NewWebhookHandler(&fakeMirror{}, &fakeResolver{err: errors.New("netbox down")}, testSecret),
			body: deviceBody,
		},
		{
			name: "mirror create failure",
			h: NewWebhookHandler(
				&fakeMirror{err: errors.New("pg down")},
				&fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}, testSecret),
			body: deviceBody,
		},
		{
			name: "mirror delete failure",
			h: NewWebhookHandler(
				&fakeMirror{err: errors.New("pg down")},
				&fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}, testSecret),
			body: deleteBody,
		},
		{
			name: "mirror move failure",
			h: NewWebhookHandler(
				&fakeMirror{err: errors.New("pg down")},
				&fakeResolver{byID: map[int][]string{777: {"site-b-old-cage"}}}, testSecret),
			body: moveBody,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postWebhook(t, tc.h, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status: got %d want 200 (NetBox never retries; 5xx loses the event)", w.Code)
			}
		})
	}
}

// postWebhookWithSig delivers body with an explicit X-Hook-Signature
// header. An empty sig leaves the header unset (the handler then rejects
// because verifyHMAC returns false for an empty signature).
func postWebhookWithSig(t *testing.T, h *WebhookHandler, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/netbox", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Hook-Signature", sig)
	}
	w := httptest.NewRecorder()
	c := e.NewContext(req, w)
	if err := h.HandleWebhook(c); err != nil {
		// Echo handlers can return an *echo.HTTPError instead of writing;
		// surface it into the recorder so tests assert on one place.
		if he, ok := err.(*echo.HTTPError); ok {
			w.WriteHeader(he.Code)
			_, _ = w.WriteString(he.Message)
		} else {
			t.Fatalf("handler error: %v", err)
		}
	}
	return w
}

// signBody replicates the production NetBox HMAC: a SHA-512 HMAC of the
// exact request body hex-encoded with the webhook secret. Used to mint the
// valid control signature and to prove the tamper test's 403 is a real
// signature mismatch (not a disabled check).
func signBody(secret, body string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookTamperedSignature403 pins the current contract: a request
// whose X-Hook-Signature does not match the secret is rejected with 403
// and NEVER dispatched. This is the real queued path — NetBox signs its
// webhook POSTs with the HMAC the Bootstrap call configured — so a bad
// signature must fail closed before the mirror sees the event.
func TestWebhookTamperedSignature403(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
	h := NewWebhookHandler(mir, res, "valid-secret")

	body := `{"event":"created","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}}}`
	// Signature computed with a DIFFERENT secret than the handler holds.
	w := postWebhookWithSig(t, h, body, signBody("attacker-secret", body))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403 (bad HMAC must not dispatch)", w.Code)
	}
	if len(mir.creates) != 0 {
		t.Fatalf("tampered webhook must not dispatch, got %d creates", len(mir.creates))
	}
}

// TestWebhookNoSecretRejectsEveryDelivery pins the fail-closed contract:
// with no SLAGWORKS_WEBHOOK_SECRET configured the receiver rejects every
// delivery, signed or not, with a 503 naming the setting, and the mirror
// never sees the event. A signature computed with an empty key is
// rejected too: an empty secret is not a key anyone can be trusted with.
func TestWebhookNoSecretRejectsEveryDelivery(t *testing.T) {
	createBody := `{"event":"created","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}}}`
	deleteBody := `{"event":"deleted","object_type":"dcim.device","data":{"id":777,"site":{"slug":"pop2"}}}`
	cases := []struct {
		name, body, sig string
	}{
		{"unsigned create", createBody, ""},
		{"unsigned delete", deleteBody, ""},
		{"create signed with an empty key", createBody, signBody("", createBody)},
		{"delete signed with some key", deleteBody, signBody("guessed", deleteBody)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mir := &fakeMirror{}
			res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
			h := NewWebhookHandler(mir, res, "")

			w := postWebhookWithSig(t, h, tc.body, tc.sig)

			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status: got %d want 503 (no secret must reject every delivery)", w.Code)
			}
			if !strings.Contains(w.Body.String(), "SLAGWORKS_WEBHOOK_SECRET") {
				t.Errorf("body should name the setting to configure: %q", w.Body.String())
			}
			if len(mir.creates)+len(mir.updates)+len(mir.moves)+len(mir.deletes)+len(mir.drifts) != 0 {
				t.Fatalf("mirror changed with no secret configured: %+v", mir)
			}
		})
	}
}

// TestWebhookNoSecretRejectsBeforeReadingBody pins that the disabled
// check comes first: a body that cannot be read still gets the 503 that
// names the setting, not a 400.
func TestWebhookNoSecretRejectsBeforeReadingBody(t *testing.T) {
	h := NewWebhookHandler(&fakeMirror{}, &fakeResolver{}, "")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/netbox", iotest.ErrReader(errors.New("connection reset")))
	err := h.HandleWebhook(echo.New().NewContext(req, httptest.NewRecorder()))
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %v, want a 503 HTTPError", err)
	}
}

// TestWebhookUnsignedWithSecret403 is the configured-secret control: an
// unsigned delivery is a bad signature (403), not a disabled receiver.
func TestWebhookUnsignedWithSecret403(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
	h := NewWebhookHandler(mir, res, testSecret)

	body := `{"event":"created","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}}}`
	w := postWebhookWithSig(t, h, body, "")

	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", w.Code)
	}
	if len(mir.creates) != 0 {
		t.Fatalf("unsigned webhook must not dispatch, got %d creates", len(mir.creates))
	}
}

// TestWebhookMalformedJSON400 pins that a well-signed but unparseable body
// is rejected with 400 and never dispatched. The signature is computed
// over the actual (malformed) body with the correct secret, so this
// isolates the JSON-parse rejection from the HMAC gate.
func TestWebhookMalformedJSON400(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
	h := NewWebhookHandler(mir, res, "valid-secret")

	malformed := `{"event":"created","object_type":"dcim.device","data":{"id":777,`
	w := postWebhookWithSig(t, h, malformed, signBody("valid-secret", malformed))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400 (malformed payload must not dispatch)", w.Code)
	}
	if len(mir.creates) != 0 {
		t.Fatalf("malformed webhook must not dispatch, got %d creates", len(mir.creates))
	}
}

// TestWebhookValidSignatureDispatches is the control that proves signBody
// reproduces NetBox's real signature: with the correct secret the event is
// accepted (200) and dispatched, so the 403/400 tests above are exercising
// real signature verification rather than a disabled check.
func TestWebhookValidSignatureDispatches(t *testing.T) {
	mir := &fakeMirror{}
	res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
	h := NewWebhookHandler(mir, res, "valid-secret")

	body := `{"event":"created","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}}}`
	w := postWebhookWithSig(t, h, body, signBody("valid-secret", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (valid HMAC must dispatch)", w.Code)
	}
	if len(mir.creates) != 1 {
		t.Fatalf("expected 1 create, got %d", len(mir.creates))
	}
	if mir.creates[0].ID != 777 {
		t.Fatalf("dispatched device id: got %d want 777", mir.creates[0].ID)
	}
}

// TestWebhookExternalDriftGate pins the B7 detection layer: an
// `updated` event whose prechange/postchange diff shows an EXTERNAL
// topology field changed (site/location/rack/device_type/role) fires
// OnDeviceExternalDrift with the changed fields; changes to fields
// slagworks writes itself (serial, status, custom_fields...) do not.
//
// A site or location change also fires OnDeviceMoved, because the device
// may have left the config that claimed it before (the resolver names
// only the config that claims it now). That holds when the device lands
// outside every claim, where the handler mirrors nothing else.
func TestWebhookExternalDriftGate(t *testing.T) {
	mkBody := func(pre, post string) string {
		return `{"event":"updated","object_type":"dcim.device",
			"data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}},
			"snapshots":{"prechange":` + pre + `,"postchange":` + post + `}}`
	}
	cases := []struct {
		name       string
		body       string
		unclaimed  bool // the resolver claims device 777 for no config
		wantFields []string
		wantMoved  bool
	}{
		{
			name:       "location moved",
			body:       mkBody(`{"location":{"id":21},"serial":"SN1"}`, `{"location":{"id":99},"serial":"SN1"}`),
			wantFields: []string{"location"},
			wantMoved:  true,
		},
		{
			name:       "location moved out of every claim",
			body:       mkBody(`{"site":1,"location":21}`, `{"site":1,"location":99}`),
			unclaimed:  true,
			wantFields: []string{"location"},
			wantMoved:  true,
		},
		{
			name:       "site and device_type moved",
			body:       mkBody(`{"site":{"slug":"pop2"},"device_type":{"id":1}}`, `{"site":{"slug":"pop3"},"device_type":{"id":2}}`),
			wantFields: []string{"site", "device_type"},
			wantMoved:  true,
		},
		{
			name:       "rack moved within the same location",
			body:       mkBody(`{"site":1,"location":21,"rack":5}`, `{"site":1,"location":21,"rack":6}`),
			wantFields: []string{"rack"},
		},
		{
			name: "self-written fields exempt",
			body: mkBody(
				`{"serial":"OLD","status":{"value":"planned"},"custom_fields":{"a":1},"location":{"id":21}}`,
				`{"serial":"NEW","status":{"value":"active"},"custom_fields":{"a":2},"location":{"id":21}}`),
			wantFields: nil,
		},
		{
			name:       "no snapshots at all",
			body:       `{"event":"updated","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"pop2"}}}`,
			wantFields: nil,
		},
		{
			name:       "key-order difference is not drift",
			body:       mkBody(`{"location":{"id":21,"slug":"x"}}`, `{"location":{"slug":"x","id":21}}`),
			wantFields: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mir := &fakeMirror{}
			res := &fakeResolver{byID: map[int][]string{777: {"pop2-old-cage"}}}
			wantSlug := res.byID[777][0]
			if tc.unclaimed {
				res = &fakeResolver{byID: map[int][]string{}}
				wantSlug = ""
			}
			h := NewWebhookHandler(mir, res, testSecret)
			w := postWebhook(t, h, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			switch {
			case tc.wantMoved && (len(mir.moves) != 1 || mir.moves[0] != 777):
				t.Fatalf("OnDeviceMoved calls: got %v, want [777]", mir.moves)
			case !tc.wantMoved && len(mir.moves) != 0:
				t.Fatalf("unexpected OnDeviceMoved calls: %v", mir.moves)
			}
			if tc.wantFields == nil {
				if len(mir.drifts) != 0 {
					t.Fatalf("unexpected drift call: %+v", mir.drifts)
				}
				return
			}
			if len(mir.drifts) != 1 {
				t.Fatalf("expected 1 drift call, got %d", len(mir.drifts))
			}
			got := mir.drifts[0]
			if got.deviceID != 777 || got.configSlug != wantSlug {
				t.Fatalf("drift call: %+v", got)
			}
			if len(got.fields) != len(tc.wantFields) {
				t.Fatalf("fields: got %v want %v", got.fields, tc.wantFields)
			}
			for i := range tc.wantFields {
				if got.fields[i] != tc.wantFields[i] {
					t.Fatalf("fields: got %v want %v", got.fields, tc.wantFields)
				}
			}
		})
	}
}

// TestWebhookMoveInvalidatesWhenClaimResolveFails: invalidating the config
// a moved device left needs no claim, so a failed claim resolution (NetBox
// unreachable for the location tree) does not skip it, though the rest of
// the event is still left to reconcile.
func TestWebhookMoveInvalidatesWhenClaimResolveFails(t *testing.T) {
	mir := &fakeMirror{}
	h := NewWebhookHandler(mir, &fakeResolver{err: errors.New("netbox down")}, testSecret)

	body := `{"event":"updated","object_type":"dcim.device","data":{"id":777,"name":"h1","serial":"SN1","site":{"slug":"site-b"}},` +
		`"snapshots":{"prechange":{"site":1,"location":21},"postchange":{"site":1,"location":99}}}`
	w := postWebhook(t, h, body)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	if len(mir.moves) != 1 || mir.moves[0] != 777 {
		t.Fatalf("OnDeviceMoved calls: got %v, want [777]", mir.moves)
	}
	if len(mir.updates)+len(mir.drifts) != 0 {
		t.Fatalf("claim-dependent calls ran without a claim: updates=%v drifts=%v", mir.updates, mir.drifts)
	}
}
