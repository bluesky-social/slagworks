//go:build integration

package netboxtest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/netboxtest"
)

// TestNetBoxWebhookDeliversRealQueuedEvents exercises the real queued
// delivery path, end to end:
//
//	real NetBox Bootstrap -> extras webhook + event rules -> NetBox RQ
//	worker -> HTTP -> real netbox.WebhookHandler -> test MirrorOps recorder
//
// It pins the current containerized NetBox (netboxTag, API 4.5) payload
// shape (top-level event/object_type/data plus update snapshots.prechange/
// postchange) so a NetBox image bump that changes the envelope is caught
// here rather than silently changing what the handler projects. It does
// NOT claim parity with a newer production NetBox (the pinned image is
// 4.5; production drifted ahead and is out of this test's scope).
func TestNetBoxWebhookDeliversRealQueuedEvents(t *testing.T) {
	env := netboxtest.NewWithWebhookWorker(t)
	ctx := context.Background()
	const secret = "test-webhook-secret"

	rec := &webhookRecorder{}
	handler := netbox.NewWebhookHandler(rec, claimingResolver{slug: "test-config"}, secret)

	// --- in-process receiver running the real WebhookHandler ---
	// The lambda only copies the raw request body for envelope assertions
	// and restores it; HMAC verification and handler dispatch stay in the
	// real WebhookHandler.
	e := echo.New()
	e.POST("/api/v1/hooks/netbox", func(c *echo.Context) error {
		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		rec.recordEnvelope(body)
		c.Request().Body = io.NopCloser(bytes.NewReader(body))
		return handler.HandleWebhook(c)
	})

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("receiver listen: %v", err)
	}
	// The worker routes to this host via env.ReceiverHost(); it reaches the
	// Go process because the listener is bound to all interfaces.
	srv := &http.Server{Handler: e}
	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			t.Logf("receiver server: %v", serr)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	receiverURL := fmt.Sprintf("http://%s:%d/api/v1/hooks/netbox", env.ReceiverHost(), port)

	// --- re-run the idempotent production bootstrap with the webhook ---
	// New() bootstrapped without a webhook; this is the same code path that
	// production runs once the operator sets SLAGWORKS_EXTERNAL_URL +
	// SLAGWORKS_WEBHOOK_SECRET. It creates the slagworks-enrollment webhook
	// (pointing at our receiver) and the three device event rules.
	bs, err := env.NetBox.Bootstrap(ctx, netbox.BootstrapConfig{
		WebhookURL:    receiverURL,
		WebhookSecret: secret,
	})
	if err != nil {
		t.Fatalf("re-bootstrap with webhook: %v", err)
	}
	if bs.Webhook != receiverURL {
		t.Fatalf("bootstrap webhook url: got %q want %q", bs.Webhook, receiverURL)
	}
	if len(bs.EventRules) != 3 {
		t.Fatalf("bootstrap event rules: got %v want exactly [create update delete]", bs.EventRules)
	}

	assertWebhookAndRules(t, env, receiverURL)

	// diag builds the timeout diagnostics: receiver state plus the NetBox
	// web and RQ worker container logs, so a missed delivery is explainable
	// (not a bare channel timeout).
	diag := func() string {
		var sb strings.Builder
		sb.WriteString("receiver state:\n" + rec.summary())
		sb.WriteString("\n--- netbox web logs ---\n" + env.NetBoxLogs())
		sb.WriteString("\n--- netbox rq worker logs ---\n" + env.WorkerLogs())
		return sb.String()
	}

	// --- server-role device: create -> update -> delete ---
	const serial = "WEBHOOK-SRV-0001"
	site := env.SeedSite("pop")
	serverDev := env.SeedDevice(site, netboxtest.SeedDeviceOpts{Serial: serial, RoleSlug: "server"})
	waitForCond(t, 30*time.Second, "one OnDeviceCreated for server device", func() bool {
		return rec.createCount() == 1
	}, diag)

	created := rec.createAt(0)
	if created.ID != serverDev.ID {
		t.Fatalf("created device id: got %d want %d", created.ID, serverDev.ID)
	}
	if created.Serial != serial {
		t.Fatalf("created serial: got %q want %q", created.Serial, serial)
	}
	if created.SiteSlug != site.Slug {
		t.Fatalf("created site slug: got %q want %q (projection must carry the NetBox site slug)", created.SiteSlug, site.Slug)
	}
	if created.ConfigSlug != "test-config" {
		t.Fatalf("created config slug: got %q want %q (claim resolver's invalidation key)", created.ConfigSlug, "test-config")
	}

	createdEnv := requireEnvelope(t, rec, "created", serverDev.ID)
	if createdEnv.Data.Serial != serial || createdEnv.Data.Site == nil || createdEnv.Data.Site.Slug != site.Slug {
		t.Fatalf("created envelope projection: serial=%q site=%+v, want %q/%q", createdEnv.Data.Serial, createdEnv.Data.Site, serial, site.Slug)
	}

	// update: patch a field slagworks itself writes (serial) so the B7
	// drift gate sees no EXTERNAL-field change.
	const serialUpdated = "WEBHOOK-SRV-0001-UPD"
	var patched netbox.Device
	if err := env.NetBox.Patch(ctx, fmt.Sprintf("/api/dcim/devices/%d/", serverDev.ID),
		map[string]any{"serial": serialUpdated}, &patched); err != nil {
		t.Fatalf("patch server device: %v", err)
	}
	waitForCond(t, 30*time.Second, "one OnDeviceUpdated for server device", func() bool {
		return rec.updateCount() == 1
	}, diag)
	if got := rec.updateAt(0); got.ID != serverDev.ID || got.Serial != serialUpdated || got.SiteSlug != site.Slug || got.ConfigSlug != "test-config" {
		t.Fatalf("updated callback: got %+v want id=%d serial=%q site=%q config=test-config",
			got, serverDev.ID, serialUpdated, site.Slug)
	}
	updatedEnv := requireEnvelope(t, rec, "updated", serverDev.ID)
	if updatedEnv.Data.Serial != serialUpdated {
		t.Fatalf("updated envelope serial: got %q want %q", updatedEnv.Data.Serial, serialUpdated)
	}
	if updatedEnv.Snapshots.PreChange.Serial != serial || updatedEnv.Snapshots.PostChange.Serial != serialUpdated {
		t.Fatalf("updated snapshot serials: got %q -> %q want %q -> %q",
			updatedEnv.Snapshots.PreChange.Serial, updatedEnv.Snapshots.PostChange.Serial, serial, serialUpdated)
	}

	// delete
	if err := env.NetBox.Delete(ctx, fmt.Sprintf("/api/dcim/devices/%d/", serverDev.ID)); err != nil {
		t.Fatalf("delete server device: %v", err)
	}
	waitForCond(t, 30*time.Second, "one OnDeviceDeleted for server device", func() bool {
		return rec.deleteCount() == 1
	}, diag)
	if got := rec.deleteAt(0); got.deviceID != serverDev.ID || got.configSlug != "test-config" {
		t.Fatalf("delete callback: got %+v want {device %d config test-config}", got, serverDev.ID)
	}
	requireEnvelope(t, rec, "deleted", serverDev.ID)

	// --- non-server device: prove the event-rule filter suppresses delivery ---
	const nonServerSerial = "WEBHOOK-NONSRV-0001"
	nonServer := env.SeedDevice(site, netboxtest.SeedDeviceOpts{Serial: nonServerSerial}) // default namespaced test role
	var nsUpdated netbox.Device
	if err := env.NetBox.Patch(ctx, fmt.Sprintf("/api/dcim/devices/%d/", nonServer.ID),
		map[string]any{"serial": "WEBHOOK-NONSRV-0001-UPD"}, &nsUpdated); err != nil {
		t.Fatalf("patch non-server device: %v", err)
	}
	// Give the worker a bounded window to (not) deliver. If the filter were
	// broken, the create/update would show up here; a real delivery arrives
	// in well under a second, so this window is generous.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if rec.createCount() > 1 || rec.updateCount() > 1 || rec.deleteCount() > 1 {
			t.Fatalf("non-server device unexpectedly dispatched\nreceiver state:\n%s", rec.summary())
		}
		if rec.hasEnvelopeForDevice(nonServer.ID) {
			t.Fatalf("non-server device (%d) reached the handler despite role!=server\nreceiver state:\n%s", nonServer.ID, rec.summary())
		}
		time.Sleep(250 * time.Millisecond)
	}

	// final counts: exactly one create, one update, one delete.
	if c, u, d := rec.counts(); c != 1 || u != 1 || d != 1 {
		t.Fatalf("event counts: created=%d updated=%d deleted=%d, want 1/1/1\n%s", c, u, d, rec.summary())
	}

}

// assertWebhookAndRules queries NetBox and asserts exactly the expected
// slagworks-enrollment webhook plus the three enabled device event rules,
// each conditioned on role.slug=server and pointing at that webhook.
func assertWebhookAndRules(t *testing.T, env *netboxtest.Env, receiverURL string) {
	t.Helper()
	ctx := context.Background()

	var webhooks []netbox.Webhook
	if err := env.NetBox.List(ctx, "/api/extras/webhooks/", url.Values{}, &webhooks); err != nil {
		t.Fatalf("list webhooks: %v", err)
	}
	var wh *netbox.Webhook
	matchingWebhooks := 0
	for i := range webhooks {
		if webhooks[i].Name == "slagworks-enrollment" {
			matchingWebhooks++
			wh = &webhooks[i]
		}
	}
	if matchingWebhooks != 1 {
		t.Fatalf("slagworks-enrollment webhooks: got %d want 1", matchingWebhooks)
	}
	if wh.PayloadURL != receiverURL {
		t.Fatalf("webhook payload_url: got %q want %q", wh.PayloadURL, receiverURL)
	}
	if wh.HTTPMethod != "POST" {
		t.Fatalf("webhook http_method: got %q want POST", wh.HTTPMethod)
	}

	var rules []netbox.EventRule
	if err := env.NetBox.List(ctx, "/api/extras/event-rules/", url.Values{}, &rules); err != nil {
		t.Fatalf("list event rules: %v", err)
	}
	want := map[string]string{
		"Slagworks device create": "object_created",
		"Slagworks device update": "object_updated",
		"Slagworks device delete": "object_deleted",
	}
	found := map[string]*netbox.EventRule{}
	matchingRules := 0
	for i := range rules {
		if _, ok := want[rules[i].Name]; ok {
			matchingRules++
			found[rules[i].Name] = &rules[i]
		}
	}
	if matchingRules != 3 || len(found) != 3 {
		t.Fatalf("expected exactly 3 slagworks event rules, found %d (%v)", matchingRules, names(found))
	}
	for name, eventType := range want {
		r := found[name]
		if r == nil {
			t.Fatalf("missing event rule %q", name)
		}
		if !r.Enabled {
			t.Errorf("event rule %q: must be enabled", name)
		}
		if len(r.ObjectTypes) != 1 || r.ObjectTypes[0] != "dcim.device" {
			t.Errorf("event rule %q: object_types %v want [dcim.device]", name, r.ObjectTypes)
		}
		if len(r.EventTypes) != 1 || r.EventTypes[0] != eventType {
			t.Errorf("event rule %q: event_types %v want [%s]", name, r.EventTypes, eventType)
		}
		if r.ActionObject == nil || r.ActionObject.ID != wh.ID {
			t.Errorf("event rule %q: action_object doesn't point at webhook %d (got %+v)", name, wh.ID, r.ActionObject)
		}
		if !conditionsRoleServer(r.Conditions) {
			t.Errorf("event rule %q: conditions %s must require role.slug=server", name, r.Conditions)
		}
	}
}

func names(m map[string]*netbox.EventRule) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// conditionsRoleServer reports whether the event-rule conditions restrict
// delivery to devices whose role slug is "server".
func conditionsRoleServer(raw json.RawMessage) bool {
	var cond struct {
		And []struct {
			Attr  string `json:"attr"`
			Value string `json:"value"`
		} `json:"and"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &cond) != nil {
		return false
	}
	for _, a := range cond.And {
		if a.Attr == "role.slug" && a.Value == "server" {
			return true
		}
	}
	return false
}

// --- envelope shape pins -------------------------------------------------
// These assert the top-level event/object_type/data and update snapshot
// shapes as produced by the pinned containerized NetBox (netboxTag). They
// are the regression tripwire for a NetBox payload bump.

type capturedEnvelope struct {
	Event      string `json:"event"`
	ObjectType string `json:"object_type"`
	Data       struct {
		ID     int    `json:"id"`
		Serial string `json:"serial"`
		Site   *struct {
			Slug string `json:"slug"`
		} `json:"site"`
	} `json:"data"`
	Snapshots struct {
		PreChange struct {
			Serial string `json:"serial"`
		} `json:"prechange"`
		PostChange struct {
			Serial string `json:"serial"`
		} `json:"postchange"`
	} `json:"snapshots"`
}

func requireEnvelope(t *testing.T, rec *webhookRecorder, event string, deviceID int) capturedEnvelope {
	t.Helper()
	raw := rec.envelopeForEvent(event)
	if raw == nil {
		t.Fatalf("no %q envelope captured", event)
	}
	var env capturedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal %q envelope: %v\nbody: %s", event, err, raw)
	}
	if env.Event != event || env.ObjectType != "dcim.device" || env.Data.ID != deviceID {
		t.Fatalf("%q envelope identity: event=%q object_type=%q id=%d, want %q/dcim.device/%d",
			event, env.Event, env.ObjectType, env.Data.ID, event, deviceID)
	}
	return env
}

// --- recorder and resolver ----------------------------------------------

// webhookRecorder is a concurrency-safe MirrorOps recorder (callbacks from
// separate RQ delivery POSTs can arrive concurrently) that also captures
// every raw request envelope for payload-shape assertions.
type webhookRecorder struct {
	mu        sync.Mutex
	creates   []mirror.Device
	updates   []mirror.Device
	deletes   []deleteCall
	envelopes []envelopeCapture
}

type deleteCall struct {
	deviceID   int
	configSlug string
}

type envelopeCapture struct {
	event string
	raw   []byte
}

func (r *webhookRecorder) OnDeviceCreated(_ context.Context, d mirror.Device) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates = append(r.creates, d)
	return nil
}
func (r *webhookRecorder) OnDeviceUpdated(_ context.Context, d mirror.Device) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, d)
	return nil
}
func (r *webhookRecorder) OnDeviceDeleted(_ context.Context, deviceID int, configSlug string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deletes = append(r.deletes, deleteCall{deviceID, configSlug})
	return nil
}
func (r *webhookRecorder) OnDeviceExternalDrift(_ context.Context, _ int, _ []string, _ string) error {
	return nil
}
func (r *webhookRecorder) OnSiteUpdated(_ context.Context, _, _ string) error {
	return nil
}

func (r *webhookRecorder) recordEnvelope(raw []byte) {
	var env struct {
		Event string `json:"event"`
	}
	event := ""
	if json.Unmarshal(raw, &env) == nil {
		event = env.Event
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envelopes = append(r.envelopes, envelopeCapture{event: event, raw: append([]byte(nil), raw...)})
}

func (r *webhookRecorder) counts() (int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.creates), len(r.updates), len(r.deletes)
}
func (r *webhookRecorder) createCount() int { c, _, _ := r.counts(); return c }
func (r *webhookRecorder) updateCount() int { _, u, _ := r.counts(); return u }
func (r *webhookRecorder) deleteCount() int { _, _, d := r.counts(); return d }
func (r *webhookRecorder) createAt(i int) mirror.Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.creates[i]
}
func (r *webhookRecorder) updateAt(i int) mirror.Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.updates[i]
}
func (r *webhookRecorder) deleteAt(i int) deleteCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.deletes[i]
}
func (r *webhookRecorder) envelopeForEvent(event string) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.envelopes {
		if e.event == event {
			return e.raw
		}
	}
	return nil
}
func (r *webhookRecorder) hasEnvelopeForDevice(deviceID int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.envelopes {
		var env struct {
			Data struct {
				ID *json.RawMessage `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(e.raw, &env) == nil && env.Data.ID != nil && string(*env.Data.ID) == fmt.Sprintf("%d", deviceID) {
			return true
		}
	}
	return false
}

// summary renders the recorder state for timeout diagnostics.
func (r *webhookRecorder) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "mirror ops: creates=%d updates=%d deletes=%d\n", len(r.creates), len(r.updates), len(r.deletes))
	for _, e := range r.envelopes {
		fmt.Fprintf(&sb, "envelope event=%q body=%s\n", e.event, e.raw)
	}
	return sb.String()
}

// claimingResolver always claims the device under a fixed config slug, so
// the webhook path dispatches with a known invalidation key.
type claimingResolver struct{ slug string }

func (r claimingResolver) ClaimingConfigs(_ context.Context, _ netbox.Device) ([]string, error) {
	return []string{r.slug}, nil
}

// waitForCond polls cond until it holds or the bounded deadline elapses,
// then fails with the provided diagnostics (NetBox web/RQ logs + receiver
// state) instead of a bare channel timeout.
func waitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool, diag func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s\n%s", what, diag())
}
