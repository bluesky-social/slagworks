package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

func TestBuildNetBoxDiffComparesOnlyIndependentBindings(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	deviceID := 42
	commissioned := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	machine := &model.Machine{
		ID:             id,
		NetBoxDeviceID: &deviceID,
		NetBoxSerial:   "SN-42",
		State:          model.MachineStateRunning,
		CommissionedAt: &commissioned,
	}
	device := &netbox.Device{
		ID:           42,
		Name:         "cpu1.pop1",
		Serial:       "SN-42",
		Site:         &netbox.NestedRef{Slug: "pop1"},
		OOBIP:        &netbox.NestedIPAddress{Address: "10.254.243.137/22"},
		Status:       &netbox.NestedValue{Value: "active"},
		CustomFields: json.RawMessage(`{"slagworks_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`),
	}
	ifaces := []netbox.Interface{
		{MACAddress: "CC:DD:EE:FF:00:11"},
		{MACAddress: "aa:bb:cc:dd:ee:ff"},
		{MACAddress: "AA:BB:CC:DD:EE:FF"},
	}

	diff := buildNetBoxDiff(id, machine, device, ifaces, "1247", "")
	if !diff.Matched {
		t.Fatal("matching persisted bindings reported drift")
	}

	fields := fieldsByName(diff.Fields)
	for _, name := range []string{"slagworks_id", "netbox_device_id", "serial"} {
		field := fields[name]
		if !field.Comparable || field.Match == nil || !*field.Match {
			t.Errorf("%s = %+v, want comparable match", name, field)
		}
	}
	for _, name := range []string{
		"hostname", "site", "bmc_ip", "macs", "slagworks_state",
		"netbox_status", "commissioned_at", "snapshot_seq",
	} {
		field := fields[name]
		if field.Comparable || field.Match != nil {
			t.Errorf("%s = %+v, want one-sided context", name, field)
		}
	}
	if got := fields["hostname"].Slagworks; got != diffNetBoxSSoT {
		t.Fatalf("hostname Slagworks value = %q, want explicit NetBox provenance", got)
	}
	if got := fields["macs"].NetBox; got != "aa:bb:cc:dd:ee:ff, cc:dd:ee:ff:00:11" {
		t.Fatalf("MAC set = %q, want normalized sorted set", got)
	}
	if got := fields["bmc_ip"].NetBox; got != "10.254.243.137" {
		t.Fatalf("BMC IP = %q, want CIDR mask stripped", got)
	}
}

func TestBuildNetBoxDiffReportsEveryBindingMismatch(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	deviceID := 42
	machine := &model.Machine{ID: id, NetBoxDeviceID: &deviceID, NetBoxSerial: "SN-OLD"}
	device := &netbox.Device{
		ID:           99,
		Serial:       "SN-NEW",
		CustomFields: json.RawMessage(`{"slagworks_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`),
	}

	diff := buildNetBoxDiff(id, machine, device, nil, diffUnavailable, "")
	if diff.Matched {
		t.Fatal("binding drift reported as matched")
	}
	fields := fieldsByName(diff.Fields)
	for _, name := range []string{"slagworks_id", "netbox_device_id", "serial"} {
		if fields[name].Match == nil || *fields[name].Match {
			t.Errorf("%s = %+v, want mismatch", name, fields[name])
		}
	}
}

func TestBuildNetBoxDiffMissingSidesAreStructuredAndNonMatching(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	device := &netbox.Device{
		ID:           42,
		Serial:       "SN-42",
		CustomFields: json.RawMessage(`{"slagworks_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`),
	}

	missingSlagworks := buildNetBoxDiff(id, nil, device, nil, diffUnavailable, MissingSlagworks)
	if missingSlagworks.Matched || missingSlagworks.Missing != MissingSlagworks || missingSlagworks.Message == "" {
		t.Fatalf("missing Slagworks result = %+v", missingSlagworks)
	}

	deviceID := 42
	machine := &model.Machine{ID: id, NetBoxDeviceID: &deviceID, NetBoxSerial: "SN-42"}
	missingNetBox := buildNetBoxDiff(id, machine, nil, nil, diffUnavailable, MissingNetBox)
	if missingNetBox.Matched || missingNetBox.Missing != MissingNetBox || missingNetBox.Message == "" {
		t.Fatalf("missing NetBox result = %+v", missingNetBox)
	}
}

func TestBuildNetBoxDiffMissingSlagworksCustomFieldIsDrift(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	deviceID := 42
	machine := &model.Machine{ID: id, NetBoxDeviceID: &deviceID, NetBoxSerial: "SN-42"}
	device := &netbox.Device{ID: 42, Serial: "SN-42", CustomFields: json.RawMessage(`{}`)}

	diff := buildNetBoxDiff(id, machine, device, nil, diffUnavailable, "")
	field := fieldsByName(diff.Fields)["slagworks_id"]
	if diff.Matched || field.Match == nil || *field.Match || field.NetBox != diffNone {
		t.Fatalf("unstamped slagworks_id result = %+v, diff matched=%v", field, diff.Matched)
	}
}

func TestNetBoxDiffMachineResolvesNetBoxByCustomField(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	st := store.New(testpg.DB(t, store.Migrate))
	staleDeviceID := 42
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxDeviceID: &staleDeviceID, NetBoxSerial: "SN-OLD",
	}); err != nil {
		t.Fatal(err)
	}

	nb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dcim/devices/":
			if got := r.URL.Query().Get("cf_slagworks_id"); got != id.String() {
				t.Fatalf("cf_slagworks_id = %q, want %s", got, id)
			}
			writeJSONList(w, []netbox.Device{{
				ID: 99, Serial: "SN-NEW",
				CustomFields: json.RawMessage(`{"slagworks_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`),
			}})
		case "/api/dcim/interfaces/":
			if got := r.URL.Query().Get("device_id"); got != "99" {
				t.Fatalf("interface device_id = %q, want 99", got)
			}
			writeJSONList(w, []netbox.Interface{})
		default:
			t.Fatalf("unexpected NetBox request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(nb.Close)

	s := &Server{
		store:  st,
		netbox: netbox.NewClient(netbox.ClientConfig{BaseURL: nb.URL, Token: "test"}),
	}
	e := newEchoForNetBoxDiff(s)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/"+id.String()+"/netbox-diff", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var diff NetBoxDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Matched || diff.Missing != "" {
		t.Fatalf("diff = %+v, want binding drift with both sides present", diff)
	}
	fields := fieldsByName(diff.Fields)
	if fields["slagworks_id"].Match == nil || !*fields["slagworks_id"].Match {
		t.Fatalf("slagworks_id should identify the NetBox side: %+v", fields["slagworks_id"])
	}
	for _, name := range []string{"netbox_device_id", "serial"} {
		if fields[name].Match == nil || *fields[name].Match {
			t.Errorf("%s = %+v, want stale-binding mismatch", name, fields[name])
		}
	}
}

func TestNetBoxDiffMachineReportsMissingSlagworksSide(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	st := store.New(testpg.DB(t, store.Migrate))
	nb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dcim/devices/":
			writeJSONList(w, []netbox.Device{{
				ID: 42, Serial: "SN-42",
				CustomFields: json.RawMessage(`{"slagworks_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}`),
			}})
		case "/api/dcim/interfaces/":
			writeJSONList(w, []netbox.Interface{})
		default:
			t.Fatalf("unexpected NetBox request %s", r.URL.Path)
		}
	}))
	t.Cleanup(nb.Close)

	s := &Server{store: st, netbox: netbox.NewClient(netbox.ClientConfig{BaseURL: nb.URL, Token: "test"})}
	e := newEchoForNetBoxDiff(s)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/"+id.String()+"/netbox-diff", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var diff NetBoxDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Matched || diff.Missing != MissingSlagworks {
		t.Fatalf("diff = %+v, want structured missing-Slagworks result", diff)
	}
}

func TestNetBoxDiffMachineReportsMissingNetBoxSide(t *testing.T) {
	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	st := store.New(testpg.DB(t, store.Migrate))
	deviceID := 42
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxDeviceID: &deviceID, NetBoxSerial: "SN-42",
	}); err != nil {
		t.Fatal(err)
	}
	nb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dcim/devices/":
			writeJSONList(w, []netbox.Device{})
		case "/api/dcim/devices/42/":
			http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
		default:
			t.Fatalf("unexpected NetBox request %s", r.URL.Path)
		}
	}))
	t.Cleanup(nb.Close)

	s := &Server{store: st, netbox: netbox.NewClient(netbox.ClientConfig{BaseURL: nb.URL, Token: "test"})}
	e := newEchoForNetBoxDiff(s)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/"+id.String()+"/netbox-diff", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var diff NetBoxDiff
	if err := json.Unmarshal(rec.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.Matched || diff.Missing != MissingNetBox {
		t.Fatalf("diff = %+v, want structured missing-NetBox result", diff)
	}
}

func newEchoForNetBoxDiff(s *Server) http.Handler {
	e := echo.New()
	e.GET("/api/v1/machines/:id/netbox-diff", s.netboxDiffMachine)
	return e
}

func fieldsByName(fields []DiffField) map[string]DiffField {
	out := make(map[string]DiffField, len(fields))
	for _, field := range fields {
		out[field.Name] = field
	}
	return out
}
