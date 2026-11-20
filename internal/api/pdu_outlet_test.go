package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// outletFixture holds the full set of NetBox objects a PDU-outlet scenario
// requires. The embedded netboxFixture serves devices, interfaces, and IPs;
// this type adds power-outlets and power-ports.
type outletFixture struct {
	netboxFixture
	// powerOutletsByDeviceID maps PDU device ID -> list of power-outlets.
	powerOutletsByDeviceID map[int][]netbox.PowerOutlet
	// powerPortsByDeviceID maps downstream device ID -> list of power-ports.
	powerPortsByDeviceID map[int][]netbox.PowerPort
}

// server builds a test HTTP server serving all the NetBox endpoints the
// outlet resolution logic touches.
func (f *outletFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// Devices: reuse the parent fixture's handler logic inline.
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
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

	// Power-outlets: filtered by device_id query param.
	mux.HandleFunc("/api/dcim/power-outlets/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		devID := q.Get("device_id")
		var out []netbox.PowerOutlet
		for id, outlets := range f.powerOutletsByDeviceID {
			if itoa(id) == devID {
				out = append(out, outlets...)
			}
		}
		writeJSONList(w, out)
	})

	// Power-ports: filtered by device_id query param.
	mux.HandleFunc("/api/dcim/power-ports/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		devID := q.Get("device_id")
		var out []netbox.PowerPort
		for id, ports := range f.powerPortsByDeviceID {
			if itoa(id) == devID {
				out = append(out, ports...)
			}
		}
		writeJSONList(w, out)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// outletFixtureServer builds an api Server backed by the extended NetBox
// fixture, an optional secret, and a testpg store (required for machine
// lookup and op creation). preflightKey may be nil — a fixed test key is
// used in that case.
func outletFixtureServer(
	t *testing.T,
	f *outletFixture,
	secret *awssecrets.AdminSecret,
	key []byte,
) (*Server, *store.Store) {
	t.Helper()
	if key == nil {
		key = []byte("test-preflight-key-32-bytes-fixed")
	}
	nbSrv := f.server(t)
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: nbSrv.URL, Token: "t"})
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	var reader awssecrets.Reader
	if secret != nil {
		reader = &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{"pop1": secret}}
	}
	s := &Server{
		netbox:       nb,
		bmcSecrets:   reader,
		store:        st,
		preflightKey: key,
		log:          slog.Default(),
		opPublisher:  nil, // no-op in tests
		echo:         echo.New(),
	}
	s.echo.GET("/api/v1/pdus/:ident/outlet/:n/power/resolve", s.resolvePDUOutletPower)
	s.echo.POST("/api/v1/pdus/:ident/outlet/:n/power", s.powerPDUOutlet)
	return s, st
}

// --- fixtures ---

// pduDeviceID / downstreamDeviceID are the NetBox IDs used in all outlet tests.
const (
	pduNBID        = 611
	downstreamNBID = 712
	pduBDeviceID   = 613 // second PDU for redundant-feed scenario
)

// singleFeedFixture builds a scenario where:
//   - pdu-0102-left (id 611, serial ZA2451003321) has Outlet 5
//   - Outlet 5 cables to kworker-1.pop1 (id 712, serial SN-KWORKER1) PSU1
//   - kworker-1 has exactly ONE feed (sole)
func singleFeedFixture() *outletFixture {
	pdu := netbox.Device{
		ID:     pduNBID,
		Name:   "pdu-0102-left",
		Serial: "ZA2451003321",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	downstream := netbox.Device{
		ID:     downstreamNBID,
		Name:   "kworker-1.pop1",
		Serial: "SN-KWORKER1",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	return &outletFixture{
		netboxFixture: netboxFixture{
			devices:    []netbox.Device{pdu, downstream},
			deviceByID: map[int]netbox.Device{pduNBID: pdu, downstreamNBID: downstream},
			interfaces: []netbox.Interface{{
				ID:       9001,
				Name:     "mgmt0",
				MgmtOnly: true,
				Device:   &netbox.NestedRef{ID: pduNBID},
			}},
			ips: []netbox.IPAddress{{
				Address:            "10.254.240.154/24",
				AssignedObjectType: "dcim.interface",
				AssignedObjectID:   new(9001),
			}},
		},
		powerOutletsByDeviceID: map[int][]netbox.PowerOutlet{
			pduNBID: {
				{
					ID:   5001,
					Name: "Outlet 5",
					Device: &netbox.NestedRef{
						ID:   pduNBID,
						Name: "pdu-0102-left",
					},
					ConnectedEndpoints: []netbox.PowerPort{{
						ID:   6001,
						Name: "PSU1",
						Device: &netbox.NestedRef{
							ID:   downstreamNBID,
							Name: "kworker-1.pop1",
						},
					}},
					ConnectedEndpointsReachable: true,
				},
				// Outlet 7 is unconnected.
				{
					ID:                          5002,
					Name:                        "Outlet 7",
					Device:                      &netbox.NestedRef{ID: pduNBID},
					ConnectedEndpoints:          nil,
					ConnectedEndpointsReachable: false,
				},
			},
		},
		powerPortsByDeviceID: map[int][]netbox.PowerPort{
			// kworker-1 has exactly one feed, connecting back to pdu-0102-left.
			downstreamNBID: {
				{
					ID:   6001,
					Name: "PSU1",
					Device: &netbox.NestedRef{
						ID:   downstreamNBID,
						Name: "kworker-1.pop1",
					},
					ConnectedEndpoints: []netbox.PowerOutlet{{
						ID:     5001,
						Name:   "Outlet 5",
						Device: &netbox.NestedRef{ID: pduNBID, Name: "pdu-0102-left"},
					}},
					ConnectedEndpointsReachable: true,
				},
			},
		},
	}
}

// dualFeedFixture is the same as singleFeedFixture but kworker-1 has a second
// feed on a distinct PDU (pdu-0102-right, id 613). Classification: redundant_unverified.
func dualFeedFixture() *outletFixture {
	f := singleFeedFixture()
	pduB := netbox.Device{
		ID:     pduBDeviceID,
		Name:   "pdu-0102-right",
		Serial: "ZB2451003322",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	f.devices = append(f.devices, pduB)
	f.deviceByID[pduBDeviceID] = pduB
	// Add a second feed on pdu-0102-right.
	f.powerPortsByDeviceID[downstreamNBID] = append(
		f.powerPortsByDeviceID[downstreamNBID],
		netbox.PowerPort{
			ID:   6002,
			Name: "PSU2",
			Device: &netbox.NestedRef{
				ID:   downstreamNBID,
				Name: "kworker-1.pop1",
			},
			ConnectedEndpoints: []netbox.PowerOutlet{{
				ID:     5010,
				Name:   "Outlet 3",
				Device: &netbox.NestedRef{ID: pduBDeviceID, Name: "pdu-0102-right"},
			}},
			ConnectedEndpointsReachable: true,
		},
	)
	return f
}

// nonPDUFixture has a device whose secret entry says kind=bmc (not a PDU).
func nonPDUFixture() *outletFixture {
	dev := netbox.Device{
		ID:     pduNBID,
		Name:   "server-1.pop1",
		Serial: "ZA2451003321",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	return &outletFixture{
		netboxFixture: netboxFixture{
			devices:    []netbox.Device{dev},
			deviceByID: map[int]netbox.Device{pduNBID: dev},
			interfaces: []netbox.Interface{{
				ID:       9001,
				Name:     "mgmt0",
				MgmtOnly: true,
				Device:   &netbox.NestedRef{ID: pduNBID},
			}},
			ips: []netbox.IPAddress{{
				Address:            "10.1.2.3/24",
				AssignedObjectType: "dcim.interface",
				AssignedObjectID:   new(9001),
			}},
		},
		powerOutletsByDeviceID: map[int][]netbox.PowerOutlet{},
		powerPortsByDeviceID:   map[int][]netbox.PowerPort{},
	}
}

// bmcSecret returns a secret with kind=bmc for the pdu serial (used to trigger the kind-check failure).
func bmcSecret() *awssecrets.AdminSecret {
	return &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		"ZA2451003321": {Kind: awssecrets.KindBMC, Username: "ADMIN", Password: "pw"},
	}}
}

// seedPDUMachine inserts a Machine row linked to the PDU's NetBox device ID.
func seedPDUMachine(t *testing.T, st *store.Store, netboxDeviceID int) {
	t.Helper()
	devID := netboxDeviceID
	m := &model.Machine{
		ID:             seedUUID(t),
		NetBoxSerial:   "ZA2451003321",
		NetBoxDeviceID: &devID,
		State:          model.MachineStateNew,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatalf("seed PDU machine: %v", err)
	}
}

func seedUUID(t *testing.T) [16]byte {
	t.Helper()
	id := [16]byte{}
	b := []byte(t.Name())
	copy(id[:], b)
	return id
}

func getJSON(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// --- preflight token unit tests ---

// TestMintVerifyPreflightToken_RoundTrip is the happy path: mint then verify.
func TestMintVerifyPreflightToken_RoundTrip(t *testing.T) {
	key := []byte("test-key-32-bytes-exactly-padded!")
	claims := preflightClaims{
		Operator:         "op@example.com",
		Verb:             "cycle",
		PDUDeviceID:      611,
		Outlet:           5,
		DownstreamSerial: "SN-KWORKER1",
		Classification:   "sole",
		ExpiresAt:        time.Now().Add(5 * time.Minute).Unix(),
	}
	token, err := mintPreflightToken(key, claims)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := verifyPreflightToken(key, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Operator != claims.Operator || got.Verb != claims.Verb ||
		got.PDUDeviceID != claims.PDUDeviceID || got.Outlet != claims.Outlet ||
		got.DownstreamSerial != claims.DownstreamSerial || got.Classification != claims.Classification {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, claims)
	}
}

// TestVerifyPreflightToken_Expired rejects a token whose exp is in the past.
func TestVerifyPreflightToken_Expired(t *testing.T) {
	key := []byte("test-key-32-bytes-exactly-padded!")
	claims := preflightClaims{
		ExpiresAt: time.Now().Add(-1 * time.Second).Unix(), // already expired
	}
	token, _ := mintPreflightToken(key, claims)
	if _, err := verifyPreflightToken(key, token); err == nil {
		t.Fatal("expected error for expired token")
	}
}

// TestVerifyPreflightToken_WrongKey rejects a token signed with a different key.
func TestVerifyPreflightToken_WrongKey(t *testing.T) {
	key1 := []byte("key-one-32-bytes-exactly-padded!!")
	key2 := []byte("key-two-32-bytes-exactly-padded!!")
	claims := preflightClaims{ExpiresAt: time.Now().Add(5 * time.Minute).Unix()}
	token, _ := mintPreflightToken(key1, claims)
	if _, err := verifyPreflightToken(key2, token); err == nil {
		t.Fatal("expected error for wrong key")
	}
}

// TestVerifyPreflightToken_Malformed rejects tampered/garbage tokens.
func TestVerifyPreflightToken_Malformed(t *testing.T) {
	key := []byte("test-key-32-bytes-exactly-padded!")
	bad := []string{
		"",
		"nodot",
		"tooshort.x",
		"abcdef." + strings.Repeat("0", 64), // valid length but bad hex claims
	}
	for _, tok := range bad {
		if _, err := verifyPreflightToken(key, tok); err == nil {
			t.Errorf("expected error for token %q", tok)
		}
	}
}

// --- resolve endpoint tests ---

// TestResolvePDUOutletPower_Connected_SoleFeed is the happy path: a connected
// outlet with one feed. Returns connected=true, classification="sole", and a
// non-empty preflight token.
func TestResolvePDUOutletPower_Connected_SoleFeed(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r OutletResolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !r.Connected {
		t.Error("expected connected=true")
	}
	if r.Classification != "sole" {
		t.Errorf("classification = %q, want sole", r.Classification)
	}
	if r.DownstreamDevice != "kworker-1.pop1" {
		t.Errorf("downstream_device = %q, want kworker-1.pop1", r.DownstreamDevice)
	}
	if r.DownstreamSerial != "SN-KWORKER1" {
		t.Errorf("downstream_serial = %q, want SN-KWORKER1", r.DownstreamSerial)
	}
	if r.PreflightToken == "" {
		t.Error("expected non-empty preflight_token for connected outlet")
	}
	if r.Warning == "" {
		t.Error("expected non-empty warning")
	}
}

// TestResolvePDUOutletPower_Connected_RedundantFeed: a downstream with two
// distinct-PDU feeds returns classification="redundant_unverified".
func TestResolvePDUOutletPower_Connected_RedundantFeed(t *testing.T) {
	f := dualFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r OutletResolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Classification != "redundant_unverified" {
		t.Errorf("classification = %q, want redundant_unverified", r.Classification)
	}
	if r.PreflightToken == "" {
		t.Error("expected preflight_token for connected redundant outlet")
	}
}

// TestResolvePDUOutletPower_Unconnected: an unconnected outlet returns
// connected=false, classification="unconnected", and no preflight token.
func TestResolvePDUOutletPower_Unconnected(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/7/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r OutletResolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Connected {
		t.Error("expected connected=false for unconnected outlet")
	}
	if r.Classification != "unconnected" {
		t.Errorf("classification = %q, want unconnected", r.Classification)
	}
	if r.PreflightToken != "" {
		t.Error("expected no preflight_token for unconnected outlet")
	}
}

// TestResolvePDUOutletPower_NonPDU: resolving an ident whose secret entry
// says kind=bmc returns 400.
func TestResolvePDUOutletPower_NonPDU(t *testing.T) {
	f := nonPDUFixture()
	s, st := outletFixtureServer(t, f, bmcSecret(), nil)
	// No machine row needed — kind check fires first.
	_ = st

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/server-1.pop1/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestResolvePDUOutletPower_OutletNotFound: outlet index that doesn't exist
// on the PDU returns 404.
func TestResolvePDUOutletPower_OutletNotFound(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/99/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestResolvePDUOutletPower_DownstreamNoSerial: a connected outlet whose
// downstream device has no serial in NetBox is refused with 422. The P4
// interlock requires the downstream serial to identify what would be cut —
// a missing serial is treated as unresolvable (fail-closed), not as a soft
// warning.
func TestResolvePDUOutletPower_DownstreamNoSerial(t *testing.T) {
	f := singleFeedFixture()
	// Clear the downstream device's serial.
	downstream := f.deviceByID[downstreamNBID]
	downstream.Serial = ""
	f.deviceByID[downstreamNBID] = downstream
	for i, d := range f.devices {
		if d.ID == downstreamNBID {
			f.devices[i].Serial = ""
		}
	}
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (downstream has no serial)", resp.StatusCode)
	}
}

// TestResolvePDUOutletPower_NoSecretStore: global runs without a secret store
// when the AWS SDK configuration fails to load. Resolution must then still
// work from NetBox alone (mgmt IP, serial, topology) instead of reading the
// nil store; it used to panic, which echo surfaced as an opaque 500.
func TestResolvePDUOutletPower_NoSecretStore(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, nil, nil) // nil secret: no secret store
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r OutletResolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.PDUMgmtIP != "10.254.240.154" {
		t.Errorf("pdu_mgmt_ip = %q, want 10.254.240.154 (from NetBox)", r.PDUMgmtIP)
	}
	if r.PDUSerial != "ZA2451003321" {
		t.Errorf("pdu_serial = %q, want ZA2451003321", r.PDUSerial)
	}
	if !r.Connected || r.Classification != "sole" || r.DownstreamSerial != "SN-KWORKER1" {
		t.Errorf("resolution = connected %v, classification %q, downstream_serial %q; want true, sole, SN-KWORKER1",
			r.Connected, r.Classification, r.DownstreamSerial)
	}
	if r.PreflightToken == "" {
		t.Error("expected non-empty preflight_token for connected outlet")
	}
}

// TestResolvePDUOutletPower_MissingVerb: missing verb query param returns 400.
func TestResolvePDUOutletPower_MissingVerb(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// --- POST endpoint tests ---

// roundtrip performs a GET resolve then a POST power and returns the created
// op. Helper to keep individual test bodies concise.
func roundtrip(
	t *testing.T,
	srv *httptest.Server,
	pduIdent string,
	outlet int,
	verb string,
) (*model.Operation, int) {
	t.Helper()
	// GET resolve.
	resolveResp := getJSON(t, srv,
		"/api/v1/pdus/"+pduIdent+"/outlet/"+itoa(outlet)+"/power/resolve?verb="+verb)
	defer resolveResp.Body.Close()
	if resolveResp.StatusCode != http.StatusOK {
		t.Logf("resolve status = %d", resolveResp.StatusCode)
		return nil, resolveResp.StatusCode
	}
	var r OutletResolution
	if err := json.NewDecoder(resolveResp.Body).Decode(&r); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}

	// POST power.
	body, _ := json.Marshal(map[string]string{
		"verb":            verb,
		"preflight_token": r.PreflightToken,
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/pdus/"+pduIdent+"/outlet/"+itoa(outlet)+"/power",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	powerResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST power: %v", err)
	}
	defer powerResp.Body.Close()
	if powerResp.StatusCode != http.StatusCreated {
		return nil, powerResp.StatusCode
	}
	var op model.Operation
	if err := json.NewDecoder(powerResp.Body).Decode(&op); err != nil {
		t.Fatalf("decode op: %v", err)
	}
	return &op, powerResp.StatusCode
}

// TestPowerPDUOutlet_HappyPath_AllVerbs: cycle/on/off each create the
// correctly-typed op anchored on the PDU machine, with a single-outlet
// PDURuntime and the downstream serial stamped.
func TestPowerPDUOutlet_HappyPath_AllVerbs(t *testing.T) {
	cases := []struct {
		verb   string
		opType model.OperationType
	}{
		{"cycle", model.OpPowerCycle},
		{"on", model.OpPowerOn},
		{"off", model.OpPowerOff},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			f := singleFeedFixture()
			s, st := outletFixtureServer(t, f, pduSecret(), nil)
			seedPDUMachine(t, st, pduNBID)

			// Fetch the PDU machine ID to verify the op anchors on it.
			pduDevID := pduNBID
			pduMachine, err := st.GetMachineByNetBoxDeviceID(pduDevID)
			if err != nil {
				t.Fatalf("get PDU machine: %v", err)
			}

			srv := httptest.NewServer(s.echo)
			defer srv.Close()

			op, status := roundtrip(t, srv, "pdu-0102-left", 5, tc.verb)
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want 201", status)
			}
			if op.Type != tc.opType {
				t.Errorf("op.Type = %q, want %q", op.Type, tc.opType)
			}
			if op.MachineID != pduMachine.ID {
				t.Errorf("op.MachineID = %v, want %v (PDU machine)", op.MachineID, pduMachine.ID)
			}
			if op.Status != model.OpPending {
				t.Errorf("op.Status = %q, want pending", op.Status)
			}

			// Verify the persisted params: single-outlet PDURuntime + downstream serial.
			got, err := st.GetOperation(op.ID)
			if err != nil {
				t.Fatalf("get op: %v", err)
			}
			params, err := got.DecodeParams()
			if err != nil {
				t.Fatalf("decode params: %v", err)
			}
			if params.PDURuntime == nil {
				t.Fatal("op.Params missing pdu_runtime")
			}
			if len(params.PDURuntime.Outlets) != 1 {
				t.Errorf("pdu_runtime.outlets len = %d, want 1", len(params.PDURuntime.Outlets))
			}
			if params.PDURuntime.Outlets[0].Outlet != 5 {
				t.Errorf("outlet index = %d, want 5", params.PDURuntime.Outlets[0].Outlet)
			}
			if params.PDURuntime.DownstreamSerial != "SN-KWORKER1" {
				t.Errorf("downstream_serial = %q, want SN-KWORKER1", params.PDURuntime.DownstreamSerial)
			}
			// P4b: the outlet endpoints stamp the explicit interlock marker.
			if !params.PDURuntime.Outlet {
				t.Error("pdu_runtime.outlet = false, want true (P4b interlock applicability marker)")
			}
			// No --allow-self was passed, so the override must be off.
			if params.PDURuntime.AllowSelf {
				t.Error("pdu_runtime.allow_self = true, want false (no override requested)")
			}
		})
	}
}

// TestPowerPDUOutlet_NoSecretStore: without a secret store the POST re-resolves
// through the same path as the GET, so it must not panic either. It creates
// the op with the PDU's NetBox mgmt IP and serial and no credential, the same
// credential-less envelope `--via pdu` builds in this mode.
func TestPowerPDUOutlet_NoSecretStore(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, nil, nil) // nil secret: no secret store
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	op, status := roundtrip(t, srv, "pdu-0102-left", 5, "cycle")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	params, err := got.DecodeParams()
	if err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.PDURuntime == nil || len(params.PDURuntime.Outlets) != 1 {
		t.Fatalf("pdu_runtime = %+v, want exactly one outlet", params.PDURuntime)
	}
	o := params.PDURuntime.Outlets[0]
	want := model.PDUOutlet{PDUHost: "10.254.240.154", Outlet: 5, Serial: "ZA2451003321"}
	if o != want {
		t.Errorf("outlet = %+v, want %+v (NetBox mgmt IP and serial, no credential)", o, want)
	}
}

// seedWorkerWithSerial registers a site worker and records its verified
// hardware serial, so ListSiteWorkersBySerial resolves it as a serving worker
// (the P4b creation-time INV-1 / self-feed source).
func seedWorkerWithSerial(t *testing.T, st *store.Store, slug, workerID, serial string) {
	t.Helper()
	if _, _, err := st.RegisterSiteWorker(slug, workerID, workerID+".test"); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	if _, err := st.TouchSiteWorker(slug, workerID, "", &serial); err != nil {
		t.Fatalf("set worker serial: %v", err)
	}
}

// TestPowerPDUOutlet_INV1_OffOnServingWorker_RefusedAtCreation: an `off` whose
// downstream device is a registered serving worker is refused at op creation
// (INV-1: no serving-worker-off path).
func TestPowerPDUOutlet_INV1_OffOnServingWorker_RefusedAtCreation(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)
	// The downstream device (kworker-1, serial SN-KWORKER1) IS a serving worker.
	seedWorkerWithSerial(t, st, "pop1", "kworker-1", "SN-KWORKER1")

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	_, status := roundtrip(t, srv, "pdu-0102-left", 5, "off")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (INV-1: off on serving worker)", status)
	}
}

// TestPowerPDUOutlet_INV1_OffOnServingWorker_SerialCase_RefusedAtCreation: the
// worker's row holds its SMBIOS spelling, the downstream serial is NetBox's.
// The hello gate matched them case-insensitively, so a case-only difference
// is still the same serving worker and the off is refused.
func TestPowerPDUOutlet_INV1_OffOnServingWorker_SerialCase_RefusedAtCreation(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)
	// NetBox has SN-KWORKER1; the worker's board reports it in lower case.
	seedWorkerWithSerial(t, st, "pop1", "kworker-1", "sn-kworker1")

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	_, status := roundtrip(t, srv, "pdu-0102-left", 5, "off")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (INV-1: off on serving worker whose serial differs only in case)", status)
	}
}

// gwNBID is the NetBox ID of the second device in twoDeviceOutletFixture.
const gwNBID = 713

// twoDeviceOutletFixture is singleFeedFixture with Outlet 5's cable fanned
// out to a second device (a Y-cable: NetBox allows several terminations on
// one cable end). Outlet 5 lists kworker-1.pop1 PSU1 first, then
// oob-gw-1.pop1 (id 713, serial SN-GW1) PSU1. Cutting Outlet 5 cuts both.
func twoDeviceOutletFixture() *outletFixture {
	f := singleFeedFixture()
	gw := netbox.Device{
		ID:     gwNBID,
		Name:   "oob-gw-1.pop1",
		Serial: "SN-GW1",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop1", Slug: "pop1"},
	}
	f.devices = append(f.devices, gw)
	f.deviceByID[gwNBID] = gw
	outlet5 := &f.powerOutletsByDeviceID[pduNBID][0]
	outlet5.ConnectedEndpoints = append(outlet5.ConnectedEndpoints, netbox.PowerPort{
		ID: 6009, Name: "PSU1", Device: &netbox.NestedRef{ID: gwNBID, Name: "oob-gw-1.pop1"},
	})
	f.powerPortsByDeviceID[gwNBID] = []netbox.PowerPort{{
		ID: 6009, Name: "PSU1", Device: &netbox.NestedRef{ID: gwNBID, Name: "oob-gw-1.pop1"},
		ConnectedEndpoints:          []netbox.PowerOutlet{{ID: 5001, Name: "Outlet 5", Device: &netbox.NestedRef{ID: pduNBID, Name: "pdu-0102-left"}}},
		ConnectedEndpointsReachable: true,
	}}
	return f
}

// TestPowerPDUOutlet_OutletFeedingTwoDevicesIsRefused: an outlet whose cable
// fans out to two devices cuts both. Here the second is a serving site
// worker, whose feed `off` must never cut (INV-1). The warning, the preflight
// token and the interlock each identify one downstream device, so the
// request must be refused rather than resolved from the first endpoint.
func TestPowerPDUOutlet_OutletFeedingTwoDevicesIsRefused(t *testing.T) {
	key := []byte("test-preflight-key-32-bytes-fixed")
	newServer := func(t *testing.T) (*httptest.Server, *store.Store) {
		s, st := outletFixtureServer(t, twoDeviceOutletFixture(), pduSecret(), key)
		seedPDUMachine(t, st, pduNBID)
		seedWorkerWithSerial(t, st, "pop1", "oob-gw-1", "SN-GW1") // oob-gw-1 is a serving site worker
		srv := httptest.NewServer(s.echo)
		t.Cleanup(srv.Close)
		return srv, st
	}
	assertNoOps := func(t *testing.T, st *store.Store) {
		t.Helper()
		ops, err := st.ListOperations(nil)
		if err != nil {
			t.Fatalf("list ops: %v", err)
		}
		if len(ops) != 0 {
			t.Errorf("%d operation(s) created, want none", len(ops))
		}
	}

	t.Run("resolve", func(t *testing.T) {
		srv, _ := newServer(t)
		resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=off")
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d (%s); want 422", resp.StatusCode, msg)
		}
		for _, dev := range []string{"kworker-1.pop1", "oob-gw-1.pop1"} {
			if !strings.Contains(string(msg), dev) {
				t.Errorf("refusal %s does not name %q", msg, dev)
			}
		}
	})

	t.Run("resolve then post", func(t *testing.T) {
		srv, st := newServer(t)
		op, status := roundtrip(t, srv, "pdu-0102-left", 5, "off")
		if op != nil || status != http.StatusUnprocessableEntity {
			t.Errorf("`off` on an outlet that feeds kworker-1 and the serving worker oob-gw-1: status %d, created=%v; want 422 and no operation", status, op != nil)
		}
		assertNoOps(t, st)
	})

	// The POST re-resolves the outlet, so a token minted while Outlet 5 fed
	// kworker-1 alone (before the second device was cabled) is refused too.
	t.Run("token minted before the fan-out", func(t *testing.T) {
		srv, st := newServer(t)
		token, err := mintPreflightToken(key, preflightClaims{
			Verb:             "off",
			PDUDeviceID:      pduNBID,
			Outlet:           5,
			DownstreamSerial: "SN-KWORKER1",
			Classification:   "sole",
			ExpiresAt:        time.Now().Add(5 * time.Minute).Unix(),
		})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		body, _ := json.Marshal(map[string]string{"verb": "off", "preflight_token": token})
		req, _ := http.NewRequest(http.MethodPost,
			srv.URL+"/api/v1/pdus/pdu-0102-left/outlet/5/power", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST power: %v", err)
		}
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d (%s); want 422", resp.StatusCode, msg)
		}
		assertNoOps(t, st)
	})
}

// TestResolvePDUOutletPower_SecondEndpointHasNoDevice: every endpoint of the
// outlet's cable must name a parent device, not just the first. A port with
// none means the outlet cuts something NetBox cannot identify, so the outlet
// resolves as unconnected (fail-closed) and gets no token.
func TestResolvePDUOutletPower_SecondEndpointHasNoDevice(t *testing.T) {
	f := singleFeedFixture()
	outlet5 := &f.powerOutletsByDeviceID[pduNBID][0]
	outlet5.ConnectedEndpoints = append(outlet5.ConnectedEndpoints, netbox.PowerPort{ID: 6010, Name: "orphan-PSU"})
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r OutletResolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Connected || r.Classification != "unconnected" || r.PreflightToken != "" {
		t.Errorf("connected %v, classification %q, token issued %v; want false, unconnected, no token",
			r.Connected, r.Classification, r.PreflightToken != "")
	}
	if !strings.Contains(r.Warning, "orphan-PSU") {
		t.Errorf("warning %q does not name the port with no device", r.Warning)
	}
}

// TestResolvePDUOutletPower_OutletFeedingTwoPortsOfOneDevice: a cable that
// fans out to two power-ports of the SAME device still identifies exactly
// one downstream device, so it resolves normally and reports both ports.
func TestResolvePDUOutletPower_OutletFeedingTwoPortsOfOneDevice(t *testing.T) {
	f := singleFeedFixture()
	kworker := &netbox.NestedRef{ID: downstreamNBID, Name: "kworker-1.pop1"}
	outlet5 := &f.powerOutletsByDeviceID[pduNBID][0]
	outlet5.ConnectedEndpoints = append(outlet5.ConnectedEndpoints,
		netbox.PowerPort{ID: 6002, Name: "PSU2", Device: kworker})
	f.powerPortsByDeviceID[downstreamNBID] = append(f.powerPortsByDeviceID[downstreamNBID], netbox.PowerPort{
		ID: 6002, Name: "PSU2", Device: kworker,
		ConnectedEndpoints:          []netbox.PowerOutlet{{ID: 5001, Name: "Outlet 5", Device: &netbox.NestedRef{ID: pduNBID, Name: "pdu-0102-left"}}},
		ConnectedEndpointsReachable: true,
	})
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var r OutletResolution
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.DownstreamDevice != "kworker-1.pop1" || r.DownstreamSerial != "SN-KWORKER1" {
		t.Errorf("downstream = %q/%q, want kworker-1.pop1/SN-KWORKER1", r.DownstreamDevice, r.DownstreamSerial)
	}
	if r.DownstreamPort != "PSU1, PSU2" {
		t.Errorf("downstream_port = %q, want %q", r.DownstreamPort, "PSU1, PSU2")
	}
	// Both PSUs hang off one outlet of one PDU: cutting it drops the device.
	if r.Classification != "sole" {
		t.Errorf("classification = %q, want sole", r.Classification)
	}
	if r.PreflightToken == "" {
		t.Error("expected a preflight_token")
	}
}

// TestPowerPDUOutlet_SelfFeedCycle_SelfIsNotAPartnerAcrossSerialCase: the only
// connected worker is the downstream device itself, reporting NetBox's serial
// in a different case. It is not an eligible partner, so a non-AllowSelf
// cycle is refused at creation.
func TestPowerPDUOutlet_SelfFeedCycle_SelfIsNotAPartnerAcrossSerialCase(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)
	seedWorkerWithSerial(t, st, "pop1", "kworker-1", "sn-kworker1")
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	s.powerStatusRPC = fakeOutletRPC{serials: map[string][]string{"pop1": {"sn-kworker1"}}}

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	_, status := roundtrip(t, srv, "pdu-0102-left", 5, "cycle")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (the only connected worker is the downstream itself)", status)
	}
}

// TestPowerPDUOutlet_SelfFeedCycle_NoEligiblePartner_RefusedAtCreation: a
// non-AllowSelf cycle whose downstream is a serving worker is refused at
// creation when no eligible partner is connected (powerStatusRPC nil ⇒ no
// observable partner).
func TestPowerPDUOutlet_SelfFeedCycle_NoEligiblePartner_RefusedAtCreation(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)
	seedWorkerWithSerial(t, st, "pop1", "kworker-1", "SN-KWORKER1")

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	_, status := roundtrip(t, srv, "pdu-0102-left", 5, "cycle")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (no eligible partner for self-feed cycle)", status)
	}
}

// fakeOutletRPC is a PowerStatusRPC stub for the op-creation-time
// eligible-partner check. Only ConnectedWorkerSerials is exercised here.
type fakeOutletRPC struct{ serials map[string][]string }

func (f fakeOutletRPC) PowerStatus(context.Context, string, uuid.UUID, json.RawMessage, json.RawMessage) (ws.PowerStatusResponseMsg, error) {
	return ws.PowerStatusResponseMsg{}, nil
}
func (f fakeOutletRPC) BootStatus(context.Context, string, uuid.UUID, json.RawMessage) (ws.BootStatusResponseMsg, error) {
	return ws.BootStatusResponseMsg{}, nil
}
func (f fakeOutletRPC) ConnectedWorkerSerials(slug string) []string { return f.serials[slug] }
func (f fakeOutletRPC) DHCPLeases(context.Context, string) []ws.WorkerDHCPLeases {
	return nil
}

// TestPowerPDUOutlet_SelfFeedCycle_EligiblePartnerConnected_Created: a
// non-AllowSelf self-feed cycle IS created when a connected, claim-eligible
// non-self partner exists (verified serial != "" and != downstream).
func TestPowerPDUOutlet_SelfFeedCycle_EligiblePartnerConnected_Created(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)
	seedWorkerWithSerial(t, st, "pop1", "kworker-1", "SN-KWORKER1")
	// A site_config must exist so routeSlugForDevice resolves the PDU's owning
	// slug (the PDU device is in netbox_site pop1).
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: "pop1", NetBoxSite: "pop1",
		EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250",
	}); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	// A connected partner with a distinct verified serial.
	s.powerStatusRPC = fakeOutletRPC{serials: map[string][]string{"pop1": {"SN-PARTNER"}}}

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	op, status := roundtrip(t, srv, "pdu-0102-left", 5, "cycle")
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (eligible partner connected)", status)
	}
	if op == nil || op.Type != model.OpPowerCycle {
		t.Fatalf("expected a power_cycle op, got %+v", op)
	}
}

// TestPowerPDUOutlet_AllowSelf_Plumbed: a self-feed cycle with allow_self=true
// bypasses the eligible-partner check and creates the op with AllowSelf stamped
// into the PDURuntime.
func TestPowerPDUOutlet_AllowSelf_Plumbed(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)
	seedWorkerWithSerial(t, st, "pop1", "kworker-1", "SN-KWORKER1")

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	// GET resolve (verb=cycle) for the token.
	resolveResp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resolveResp.Body.Close()
	var r OutletResolution
	if err := json.NewDecoder(resolveResp.Body).Decode(&r); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	// POST with allow_self=true.
	body, _ := json.Marshal(map[string]any{
		"verb":            "cycle",
		"preflight_token": r.PreflightToken,
		"allow_self":      true,
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/pdus/pdu-0102-left/outlet/5/power", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (allow_self bypasses partner check)", resp.StatusCode)
	}
	var op model.Operation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		t.Fatalf("decode op: %v", err)
	}
	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatalf("get op: %v", err)
	}
	params, err := got.DecodeParams()
	if err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if params.PDURuntime == nil || !params.PDURuntime.AllowSelf {
		t.Error("pdu_runtime.allow_self = false, want true (plumbed from body)")
	}
	if !params.PDURuntime.Outlet {
		t.Error("pdu_runtime.outlet = false, want true")
	}
}

// TestPowerPDUOutlet_TokenMismatch_WrongVerb: a token issued for "cycle" is
// rejected when POST body says "on" (verb mismatch → 409).
func TestPowerPDUOutlet_TokenMismatch_WrongVerb(t *testing.T) {
	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), nil)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	// GET resolve with verb=cycle.
	resolveResp := getJSON(t, srv, "/api/v1/pdus/pdu-0102-left/outlet/5/power/resolve?verb=cycle")
	defer resolveResp.Body.Close()
	var r OutletResolution
	json.NewDecoder(resolveResp.Body).Decode(&r)

	// POST with a different verb.
	body, _ := json.Marshal(map[string]string{
		"verb":            "on", // mismatch: token says "cycle"
		"preflight_token": r.PreflightToken,
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/pdus/pdu-0102-left/outlet/5/power",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST power (verb mismatch): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 (verb mismatch)", resp.StatusCode)
	}
}

// TestPowerPDUOutlet_TokenExpired: an expired token returns 409.
func TestPowerPDUOutlet_TokenExpired(t *testing.T) {
	key := []byte("test-preflight-key-32-bytes-fixed")
	// Mint a token that is already expired.
	claims := preflightClaims{
		Operator:         "",
		Verb:             "cycle",
		PDUDeviceID:      pduNBID,
		Outlet:           5,
		DownstreamSerial: "SN-KWORKER1",
		Classification:   "sole",
		ExpiresAt:        time.Now().Add(-1 * time.Second).Unix(),
	}
	token, _ := mintPreflightToken(key, claims)

	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), key)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"verb":            "cycle",
		"preflight_token": token,
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/pdus/pdu-0102-left/outlet/5/power",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST power (expired token): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 (expired token)", resp.StatusCode)
	}
}

// TestPowerPDUOutlet_UnconnectedOutlet: POSTing for an unconnected outlet
// (outlet 7) is refused with 422 even with a valid token (the token
// itself can't be minted for unconnected outlets, so we also test the
// guard that fires when an operator somehow bypasses the resolve step).
func TestPowerPDUOutlet_UnconnectedOutlet(t *testing.T) {
	key := []byte("test-preflight-key-32-bytes-fixed")
	// Mint a token for the unconnected outlet manually
	// (mimicking a future unsafe-override path or a bypassed client).
	claims := preflightClaims{
		Operator:         "",
		Verb:             "cycle",
		PDUDeviceID:      pduNBID,
		Outlet:           7,
		DownstreamSerial: "", // unconnected has no downstream serial
		Classification:   "unconnected",
		ExpiresAt:        time.Now().Add(5 * time.Minute).Unix(),
	}
	token, _ := mintPreflightToken(key, claims)

	f := singleFeedFixture()
	s, st := outletFixtureServer(t, f, pduSecret(), key)
	seedPDUMachine(t, st, pduNBID)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	body, _ := json.Marshal(map[string]string{
		"verb":            "cycle",
		"preflight_token": token,
	})
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/api/v1/pdus/pdu-0102-left/outlet/7/power",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST power (unconnected): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422 (unconnected outlet)", resp.StatusCode)
	}
}

// TestPowerPDUOutlet_NoNetBox: both endpoints return 503 without NetBox.
func TestPowerPDUOutlet_NoNetBox(t *testing.T) {
	s := &Server{preflightKey: []byte("k")}
	e := echo.New()
	e.GET("/api/v1/pdus/:ident/outlet/:n/power/resolve", s.resolvePDUOutletPower)
	e.POST("/api/v1/pdus/:ident/outlet/:n/power", s.powerPDUOutlet)

	for _, tc := range []struct {
		method, path string
		body         string
	}{
		{http.MethodGet, "/api/v1/pdus/foo/outlet/1/power/resolve?verb=cycle", ""},
		{http.MethodPost, "/api/v1/pdus/foo/outlet/1/power", `{"verb":"cycle","preflight_token":"x"}`},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			var body *bytes.Reader
			if tc.body != "" {
				body = bytes.NewReader([]byte(tc.body))
			} else {
				body = bytes.NewReader(nil)
			}
			req, _ := http.NewRequest(tc.method, tc.path, body)
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			e.ServeHTTP(w, req)
			if w.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", w.Code)
			}
		})
	}
}

// TestOutletResolution_JSONShape pins the wire format.
func TestOutletResolution_JSONShape(t *testing.T) {
	r := OutletResolution{
		PDUName:          "pdu-0102-left",
		PDUMgmtIP:        "10.254.240.154",
		PDUSerial:        "ZA2451003321",
		Outlet:           5,
		Connected:        true,
		DownstreamDevice: "kworker-1.pop1",
		DownstreamSerial: "SN-KWORKER1",
		DownstreamPort:   "PSU1",
		Classification:   "sole",
		Warning:          "may power off",
		PreflightToken:   "tok",
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	json.Unmarshal(b, &got)
	for _, k := range []string{
		"pdu_name", "pdu_mgmt_ip", "pdu_serial",
		"outlet", "connected",
		"downstream_device", "downstream_serial", "downstream_port",
		"classification", "warning", "preflight_token",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing JSON field %q in: %s", k, b)
		}
	}
}
