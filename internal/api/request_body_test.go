package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// Request bodies on the operator API are JSON (docs/dev/api.md). These tests
// pin that a body the server cannot read as JSON is refused, not treated as
// an empty body: an operation created from an empty body silently drops
// every flag the caller sent (a requested recovery boot becomes a real
// deploy, a PDU power-cycle actuates the BMC). They go through NewServer
// because the production echo wiring, including its request binder, is
// what decides how a body is read.

// bodyTestServer is NewServer with cfg plus one admin token, "op-tok".
func bodyTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.RequiredGroups = []string{"platform"}
	cfg.Verifier = &fakeVerifier{claims: map[string]*tokenverify.Claims{
		"op-tok": {Subject: "op", Groups: []string{"platform"}},
	}}
	s := NewServer(cfg)
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s
}

// sendBody sends body (none when empty) with Content-Type ctype (none when
// empty) as the admin operator.
func sendBody(s *Server, method, path, body, ctype string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer op-tok")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	s.Echo().ServeHTTP(w, req)
	return w
}

// lifecycleServer is a store-only server holding one commissioned machine and
// one SSH key, so deploy and commission both reach op creation.
func lifecycleServer(t *testing.T) (*Server, *store.Store, uuid.UUID) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	if err := st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxSerial: "SN-BODY", State: model.MachineStateCommissioned, PXEEligible: true,
	}); err != nil {
		t.Fatal(err)
	}
	return bodyTestServer(t, Config{Store: st}), st, id
}

func wantRefused(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	if w.Code != http.StatusBadRequest && w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("%s = %d, want 400 or 415; response: %s", what, w.Code, strings.TrimSpace(w.Body.String()))
	}
}

func wantNoOps(t *testing.T, st *store.Store, id uuid.UUID) {
	t.Helper()
	ops, err := st.ListOperations(&id)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		t.Errorf("a refused request created a %s operation with params %s", op.Type, op.Params)
	}
}

// TestLifecycleCreate_UnparseableBodyIsRefused: a deploy or commission body
// the server cannot read as JSON must be refused with no operation created.
func TestLifecycleCreate_UnparseableBodyIsRefused(t *testing.T) {
	cases := []struct {
		name, verb, body, ctype string
	}{
		{"deploy, JSON syntax error", "deploy", `{"recovery": true,}`, "application/json"},
		{"deploy, wrong JSON type", "deploy", `{"recovery": "yes"}`, "application/json"},
		// encoding/json keeps decoding after a type error, so destructive
		// would survive while recovery is dropped: a requested rescue boot
		// would become a real destructive deploy.
		{"deploy, destructive kept while mistyped recovery is dropped", "deploy", `{"destructive": true, "recovery": "yes"}`, "application/json"},
		{"deploy, not a JSON content type", "deploy", `{"recovery": true}`, "text/plain"},
		{"deploy, no content type", "deploy", `{"recovery": true}`, ""},
		// What `curl -d '{"recovery": true}'` sends. A form body binds no
		// field of a JSON-tagged struct and reports no error.
		{"deploy, JSON sent as a form (curl -d default)", "deploy", `{"recovery": true}`, "application/x-www-form-urlencoded"},
		{"commission, truncated body", "commission", `{"via": "pdu"`, "application/json"},
		{"commission, JSON sent as a form", "commission", `{"via": "pdu"}`, "application/x-www-form-urlencoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, st, id := lifecycleServer(t)
			w := sendBody(s, http.MethodPost, "/api/v1/machines/"+id.String()+"/"+tc.verb, tc.body, tc.ctype)
			wantRefused(t, w, tc.verb+" with body "+tc.body+" ("+tc.ctype+")")
			wantNoOps(t, st, id)
		})
	}
}

// TestLifecycleCreate_AcceptedBodies pins what the refusal must keep: deploy
// and commission take an optional body (slagctl sends none when no flag is
// set), and a JSON body's flags reach the operation.
func TestLifecycleCreate_AcceptedBodies(t *testing.T) {
	cases := []struct {
		name, verb, body, ctype string
		wantRecovery            bool
	}{
		{"deploy, no body", "deploy", "", "", false},
		{"commission, no body", "commission", "", "", false},
		{"deploy, JSON", "deploy", `{"recovery": true}`, "application/json", true},
		{"deploy, JSON with charset", "deploy", `{"recovery": true}`, "application/json; charset=utf-8", true},
		{"commission, empty JSON object", "commission", `{}`, "application/json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, st, id := lifecycleServer(t)
			w := sendBody(s, http.MethodPost, "/api/v1/machines/"+id.String()+"/"+tc.verb, tc.body, tc.ctype)
			if w.Code != http.StatusCreated {
				t.Fatalf("%s = %d, want 201; response: %s", tc.name, w.Code, strings.TrimSpace(w.Body.String()))
			}
			ops, err := st.ListOperations(&id)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 1 {
				t.Fatalf("got %d operations, want 1", len(ops))
			}
			p, err := ops[0].DecodeParams()
			if err != nil {
				t.Fatal(err)
			}
			if p.Recovery != tc.wantRecovery {
				t.Errorf("op params recovery = %v, want %v (params %s)", p.Recovery, tc.wantRecovery, ops[0].Params)
			}
		})
	}
}

// TestBMCReconcile_FormBodyIsRefused: bmc-reconcile checks its bind error,
// but a JSON body sent as a form used to bind as an empty body without one,
// so the reconcile ran without the rotation the caller asked for.
func TestBMCReconcile_FormBodyIsRefused(t *testing.T) {
	a, id := adminOnlyServer(t, bmcAdminSecret(awssecrets.KindBMC))
	s := bodyTestServer(t, Config{Store: a.store, NetBox: a.netbox, BMCSecrets: a.bmcSecrets})
	path := "/api/v1/machines/" + id.String() + "/bmc-reconcile"

	w := sendBody(s, http.MethodPost, path, `{"rotate_monitor": true}`, "application/x-www-form-urlencoded")
	wantRefused(t, w, "bmc-reconcile with a form body")
	wantNoOps(t, a.store, id)

	// The same body as JSON is the control: it creates a rotating reconcile.
	w = sendBody(s, http.MethodPost, path, `{"rotate_monitor": true}`, "application/json")
	if w.Code != http.StatusCreated {
		t.Fatalf("bmc-reconcile with a JSON body = %d, want 201; response: %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var op model.Operation
	if err := json.Unmarshal(w.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	if p, err := op.DecodeParams(); err != nil || !p.RotateMonitor {
		t.Errorf("JSON bmc-reconcile params = %s (err %v), want rotate_monitor", op.Params, err)
	}
}

// TestPatch_FormBodyIsRefused: the PATCH handlers bind into a map, so a form
// body bound its whole JSON text as one unknown key and the PATCH answered
// 200 having applied nothing.
func TestPatch_FormBodyIsRefused(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{ID: id, NetBoxSerial: "SN-PATCH", State: model.MachineStateNew}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop-a", NetBoxSite: "pop-a", EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}, nil); err != nil {
		t.Fatal(err)
	}
	s := bodyTestServer(t, Config{Store: st})
	const form = "application/x-www-form-urlencoded"

	w := sendBody(s, http.MethodPatch, "/api/v1/machines/"+id.String(), `{"desired_os": "os-b"}`, form)
	wantRefused(t, w, "PATCH machine with a form body")
	w = sendBody(s, http.MethodPatch, "/api/v1/site-configs/pop-a", `{"promiscuous": true}`, form)
	wantRefused(t, w, "PATCH site-config with a form body")
}
