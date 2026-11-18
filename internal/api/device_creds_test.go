package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestGetDeviceCreds_AuthRequired pins the admin-auth gate on the
// device-creds endpoint. It hands out plaintext BMC/PDU credentials, so any
// call without a bearer must 401 and a non-platform token must 403. Mirrors
// TestGetMachineBMCRuntime_AuthRequired; regressing this exposes creds to
// anyone who can reach the API.
func TestGetDeviceCreds_AuthRequired(t *testing.T) {
	fv := &fakeVerifier{
		claims: map[string]*tokenverify.Claims{
			"platform-tok": {Subject: "op", Groups: []string{"platform"}},
			"eng-tok":      {Subject: "eng", Groups: []string{"engineering"}},
		},
		err: map[string]error{"bad-tok": errors.New("invalid")},
	}
	// The reject paths log via s.log; give it a discard logger so this
	// hand-built literal doesn't nil-panic (NewServer always sets one).
	s := &Server{verifier: fv, authz: tokenverify.RequireAnyGroup("platform"),
		log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e := echo.New()
	api := e.Group("/api/v1")
	read := api.Group("", s.oidcAuth)
	read.GET("/devices/:ident/creds", func(c *echo.Context) error {
		return c.NoContent(http.StatusOK)
	}, s.credentialAuth)

	cases := []struct {
		name   string
		header string
		want   int
	}{
		{"no auth header", "", http.StatusUnauthorized},
		{"non-bearer scheme", "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		{"invalid token", "Bearer bad-tok", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"valid but not platform", "Bearer eng-tok", http.StatusForbidden},
		{"valid platform member", "Bearer platform-tok", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/leaf-3.pop1/creds", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Errorf("got status %d, want %d (body=%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestDeviceCreds_CredentialGroups pins the optional credential-group gate
// on the production route table (NewServer): unset, every operator reads
// credentials; set, the caller needs an operator group AND a credential
// group, and a refusal names the setting without echoing any group. Other
// read routes ignore the setting.
func TestDeviceCreds_CredentialGroups(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	verifier := &fakeVerifier{claims: map[string]*tokenverify.Claims{
		"op-tok":        {Subject: "sub-op-only", Groups: []string{"operator-group"}},
		"both-tok":      {Subject: "sub-op-cred", Groups: []string{"operator-group", "cred-group-b"}},
		"cred-only-tok": {Subject: "sub-cred-only", Groups: []string{"cred-group-a"}},
	}}
	const password = "s3cret" // pduSecret's password
	const credsPath = "/api/v1/devices/pdu-0102-left/creds"

	newServer := func(t *testing.T, credGroups []string) (*Server, *bytes.Buffer) {
		t.Helper()
		nb := netbox.NewClient(netbox.ClientConfig{BaseURL: pduFixture().server(t).URL, Token: "t"})
		s := NewServer(Config{
			Store:            st,
			NetBox:           nb,
			BMCSecrets:       &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{"pop1": pduSecret()}},
			RequiredGroups:   []string{"operator-group"},
			CredentialGroups: credGroups,
			Verifier:         verifier,
		})
		var logs bytes.Buffer
		s.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
		return s, &logs
	}
	get := func(t *testing.T, s *Server, path, token string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.Echo().ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	t.Run("unset: an operator reads credentials", func(t *testing.T) {
		s, _ := newServer(t, nil)
		code, body := get(t, s, credsPath, "op-tok")
		if code != http.StatusOK || !strings.Contains(body, password) {
			t.Fatalf("got %d %s, want 200 with the credential", code, body)
		}
	})

	t.Run("set", func(t *testing.T) {
		s, logs := newServer(t, []string{"cred-group-a", "cred-group-b"})

		t.Run("operator without a credential group gets 403 naming the setting", func(t *testing.T) {
			logs.Reset()
			code, body := get(t, s, credsPath, "op-tok")
			if code != http.StatusForbidden {
				t.Fatalf("got %d %s, want 403", code, body)
			}
			if !strings.Contains(body, "SLAGWORKS_OIDC_CREDENTIAL_GROUPS") {
				t.Errorf("403 body does not name SLAGWORKS_OIDC_CREDENTIAL_GROUPS: %s", body)
			}
			for _, leak := range []string{password, "operator-group", "cred-group-a", "cred-group-b"} {
				if strings.Contains(body, leak) {
					t.Errorf("403 body echoes %q: %s", leak, body)
				}
			}
			logged := logs.String()
			for _, want := range []string{"level=INFO", "sub=sub-op-only", "operator-group", "cred-group-a"} {
				if !strings.Contains(logged, want) {
					t.Errorf("refusal log missing %q:\n%s", want, logged)
				}
			}
			if strings.Contains(logged, password) {
				t.Errorf("refusal log contains the credential:\n%s", logged)
			}
		})

		t.Run("operator in a credential group reads credentials", func(t *testing.T) {
			code, body := get(t, s, credsPath, "both-tok")
			if code != http.StatusOK || !strings.Contains(body, password) {
				t.Fatalf("got %d %s, want 200 with the credential", code, body)
			}
		})

		t.Run("credential group without an operator group fails the operator check", func(t *testing.T) {
			code, body := get(t, s, credsPath, "cred-only-tok")
			if code != http.StatusForbidden {
				t.Fatalf("got %d %s, want 403", code, body)
			}
			// The operator check refuses first, so the credential gate's
			// message must not appear.
			if strings.Contains(body, "SLAGWORKS_OIDC_CREDENTIAL_GROUPS") {
				t.Errorf("refused by the credential gate, want the operator check: %s", body)
			}
		})

		t.Run("another read route ignores the setting", func(t *testing.T) {
			if code, body := get(t, s, "/api/v1/images", "op-tok"); code != http.StatusOK {
				t.Fatalf("GET /images: got %d %s, want 200", code, body)
			}
		})
	})
}

// TestCredentialAuth_RequiresOperator pins that the credential gate fails
// closed when it runs without oidcAuth in front of it (a route wiring bug),
// with or without credential groups configured.
func TestCredentialAuth_RequiresOperator(t *testing.T) {
	for _, tc := range []struct {
		name  string
		authz tokenverify.Authorizer
	}{
		{"unset", nil},
		{"set", tokenverify.RequireAnyGroup("cred-group-a")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{credAuthz: tc.authz, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			e := echo.New()
			e.GET("/creds", func(c *echo.Context) error { return c.NoContent(http.StatusOK) }, s.credentialAuth)
			w := httptest.NewRecorder()
			e.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/creds", nil))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("got %d %s, want 500", w.Code, w.Body.String())
			}
		})
	}
}

// TestDeviceCreds_JSONShape pins the wire format. The slagctl client
// unmarshals directly into this struct; renaming a field without
// coordinating the client release silently zeroes it.
func TestDeviceCreds_JSONShape(t *testing.T) {
	dc := DeviceCreds{
		Device:      "pdu-0102-left",
		NetBoxID:    611,
		Serial:      "ZA2451003321",
		Pop:         "pop1",
		Kind:        "pdu",
		MgmtIP:      "10.254.240.154",
		Username:    "apc",
		Password:    "hunter2",
		ExtraConfig: map[string]any{"x": 1},
	}
	b, err := json.Marshal(dc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"device", "netbox_id", "serial", "pop", "kind", "mgmt_ip", "username", "password", "extra_config"} {
		if _, ok := got[k]; !ok {
			t.Errorf("expected field %q in JSON output: %s", k, b)
		}
	}
}

// netboxFixture is a minimal NetBox stub backing a *netbox.Client. It serves
// just the endpoints resolveDevice + deviceCredsFor touch: device list (by
// name / serial), device-by-id, interfaces, and ip-addresses.
type netboxFixture struct {
	devices    []netbox.Device // returned (filtered) by /api/dcim/devices/
	deviceByID map[int]netbox.Device
	interfaces []netbox.Interface // returned by /api/dcim/interfaces/
	ips        []netbox.IPAddress // returned by /api/ipam/ip-addresses/
}

func (f *netboxFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		// device-by-id: /api/dcim/devices/<id>/
		idPart := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/dcim/devices/"), "/")
		if idPart != "" {
			for _, d := range f.deviceByID {
				if itoa(d.ID) == idPart {
					writeJSON(w, d)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// list with client-style filtering on name= / serial=
		q := r.URL.Query()
		var out []netbox.Device
		for _, d := range f.devices {
			if n := q.Get("name"); n != "" && d.Name != n {
				continue
			}
			if sn := q.Get("serial"); sn != "" && d.Serial != sn {
				continue
			}
			out = append(out, d)
		}
		writeJSONList(w, out)
	})
	mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONList(w, f.interfaces)
	})
	mux.HandleFunc("/api/ipam/ip-addresses/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONList(w, f.ips)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONList(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": v})
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func newFixtureServer(t *testing.T, f *netboxFixture, secret *awssecrets.AdminSecret) *Server {
	t.Helper()
	srv := f.server(t)
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: srv.URL, Token: "t"})
	var reader awssecrets.Reader
	if secret != nil {
		reader = &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{"pop1": secret}}
	}
	return &Server{netbox: nb, bmcSecrets: reader}
}

func pduFixture() *netboxFixture {
	dev := netbox.Device{
		ID:     611,
		Name:   "pdu-0102-left",
		Serial: "ZA2451003321",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	return &netboxFixture{
		devices:    []netbox.Device{dev},
		deviceByID: map[int]netbox.Device{611: dev},
		interfaces: []netbox.Interface{{
			ID:       9001,
			Name:     "mgmt0",
			MgmtOnly: true,
			Device:   &netbox.NestedRef{ID: 611},
		}},
		ips: []netbox.IPAddress{{
			Address:            "10.254.240.154/24",
			AssignedObjectType: "dcim.interface",
			AssignedObjectID:   new(9001),
		}},
	}
}

//go:fix inline
func intPtr(i int) *int { return new(i) }

func pduSecret() *awssecrets.AdminSecret {
	return &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		"ZA2451003321": {Kind: awssecrets.KindPDU, Username: "apc", Password: "s3cret"},
	}}
}

// TestResolveDevice_ByName resolves a dotted device name.
func TestResolveDevice_ByName(t *testing.T) {
	s := newFixtureServer(t, pduFixture(), pduSecret())
	dev, err := s.resolveDevice(context.Background(), "pdu-0102-left")
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	if dev.ID != 611 {
		t.Errorf("got device id %d, want 611", dev.ID)
	}
}

// TestResolveDevice_ByID resolves an all-digits identifier via device-by-id.
func TestResolveDevice_ByID(t *testing.T) {
	s := newFixtureServer(t, pduFixture(), pduSecret())
	dev, err := s.resolveDevice(context.Background(), "611")
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	if dev.Name != "pdu-0102-left" {
		t.Errorf("got %q, want pdu-0102-left", dev.Name)
	}
}

// TestResolveDevice_BySerial falls through name to serial.
func TestResolveDevice_BySerial(t *testing.T) {
	s := newFixtureServer(t, pduFixture(), pduSecret())
	dev, err := s.resolveDevice(context.Background(), "ZA2451003321")
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	if dev.ID != 611 {
		t.Errorf("got device id %d, want 611", dev.ID)
	}
}

// TestResolveDevice_AllDigitSerial pins the fall-through fix: an all-digit
// identifier that is NOT a device id must still resolve as a serial. A naive
// "digits => id, else 404" short-circuit would make all-numeric serials
// unreachable.
func TestResolveDevice_AllDigitSerial(t *testing.T) {
	dev := netbox.Device{
		ID:     611,
		Name:   "weirdbox.pop1",
		Serial: "1234567", // all-digit serial, not equal to any device id
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	f := &netboxFixture{
		devices:    []netbox.Device{dev},
		deviceByID: map[int]netbox.Device{611: dev}, // id 1234567 does not exist
	}
	s := newFixtureServer(t, f, nil)
	got, err := s.resolveDevice(context.Background(), "1234567")
	if err != nil {
		t.Fatalf("resolveDevice by all-digit serial: %v", err)
	}
	if got.ID != 611 {
		t.Errorf("got device id %d, want 611", got.ID)
	}
}

// TestResolveDevice_NotFound surfaces a 404 when neither name nor serial hits.
func TestResolveDevice_NotFound(t *testing.T) {
	s := newFixtureServer(t, pduFixture(), pduSecret())
	_, err := s.resolveDevice(context.Background(), "nonesuch")
	assertHTTPStatus(t, err, http.StatusNotFound)
}

// TestDeviceCredsFor_PDU is the happy path: a PDU device resolves to its
// kind=pdu entry, mgmt IP, and is returned verbatim (no kind filter).
func TestDeviceCredsFor_PDU(t *testing.T) {
	s := newFixtureServer(t, pduFixture(), pduSecret())
	dev, err := s.resolveDevice(context.Background(), "pdu-0102-left")
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	creds, err := s.deviceCredsFor(context.Background(), dev)
	if err != nil {
		t.Fatalf("deviceCredsFor: %v", err)
	}
	if creds.Kind != awssecrets.KindPDU {
		t.Errorf("kind = %q, want pdu", creds.Kind)
	}
	if creds.Username != "apc" || creds.Password != "s3cret" {
		t.Errorf("creds = %q/%q, want apc/s3cret", creds.Username, creds.Password)
	}
	if creds.MgmtIP != "10.254.240.154" {
		t.Errorf("mgmt_ip = %q, want 10.254.240.154", creds.MgmtIP)
	}
	if creds.Pop != "pop1" || creds.Serial != "ZA2451003321" {
		t.Errorf("pop/serial = %q/%q", creds.Pop, creds.Serial)
	}
}

// TestDeviceCredsFor_SkipsNonActiveMgmtIP: a renumbered PDU keeps its
// deprecated old address on the same mgmt interface (often with the lower
// NetBox ID, so listed first) until it is cleaned up. The mgmt IP must be the
// active one, or a PDU power-cycle dials a dead address (router1.pop3 deploy,
// 2026-10-19: "dial tcp 10.252.1.104:22: no route to host").
func TestDeviceCredsFor_SkipsNonActiveMgmtIP(t *testing.T) {
	fx := pduFixture()
	fx.ips = append([]netbox.IPAddress{{
		Address:            "10.254.1.104/24",
		Status:             &netbox.NestedValue{Value: "deprecated"},
		AssignedObjectType: "dcim.interface",
		AssignedObjectID:   new(9001),
	}}, fx.ips...)
	s := newFixtureServer(t, fx, pduSecret())
	dev, err := s.resolveDevice(context.Background(), "pdu-0102-left")
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	creds, err := s.deviceCredsFor(context.Background(), dev)
	if err != nil {
		t.Fatalf("deviceCredsFor: %v", err)
	}
	if creds.MgmtIP != "10.254.240.154" {
		t.Errorf("mgmt_ip = %q, want the active 10.254.240.154", creds.MgmtIP)
	}
}

// TestDeviceCredsFor_AMT: an AMT device's endpoint is its oob_ip (the shared
// host NIC is not mgmt_only), so mgmt_ip comes from oob_ip with the mask
// stripped, not from a mgmt_only interface.
func TestDeviceCredsFor_AMT(t *testing.T) {
	dev := netbox.Device{
		ID: 812, Name: "router1.pop1", Serial: "MS01-123",
		Site:  &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
		OOBIP: &netbox.NestedIPAddress{ID: 5, Address: "10.252.240.23/22"},
	}
	s := newFixtureServer(t, &netboxFixture{devices: []netbox.Device{dev}, deviceByID: map[int]netbox.Device{812: dev}},
		&awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
			"MS01-123": {Kind: awssecrets.KindAMT, Username: "admin", Password: "amt-pw"},
		}})
	creds, err := s.deviceCredsFor(context.Background(), &dev)
	if err != nil {
		t.Fatalf("deviceCredsFor: %v", err)
	}
	if creds.Kind != awssecrets.KindAMT || creds.MgmtIP != "10.252.240.23" || creds.Username != "admin" {
		t.Errorf("creds = %+v, want kind=amt mgmt_ip=10.252.240.23 user=admin", creds)
	}
}

// TestDeviceCredsFor_NoSerial hard-fails: the secret is keyed by serial, so a
// serial-less device can't be resolved.
func TestDeviceCredsFor_NoSerial(t *testing.T) {
	f := pduFixture()
	d := f.devices[0]
	d.Serial = ""
	f.devices[0] = d
	s := newFixtureServer(t, f, pduSecret())
	_, err := s.deviceCredsFor(context.Background(), &d)
	assertHTTPStatus(t, err, http.StatusNotFound)
}

// TestDeviceCredsFor_NoEntry hard-fails when the device has a serial NetBox
// knows but the secret has no entry for it.
func TestDeviceCredsFor_NoEntry(t *testing.T) {
	empty := &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{}}
	s := newFixtureServer(t, pduFixture(), empty)
	dev, err := s.resolveDevice(context.Background(), "pdu-0102-left")
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	_, err = s.deviceCredsFor(context.Background(), dev)
	assertHTTPStatus(t, err, http.StatusNotFound)
}

func assertHTTPStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with status %d, got nil", want)
	}
	var he *echo.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
	}
	if he.Code != want {
		t.Fatalf("got status %d, want %d (%v)", he.Code, want, he.Message)
	}
}
