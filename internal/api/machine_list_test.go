package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

func TestEnrichMachineHostnamesFromNetBox(t *testing.T) {
	devID := 42
	withHost := model.Machine{
		ID:             uuid.New(),
		NetBoxDeviceID: &devID,
		NetBoxSerial:   "SN-HOST",
		State:          model.MachineStateRunning,
	}
	withoutHost := model.Machine{
		ID:           uuid.New(),
		NetBoxSerial: "SN-NO-HOST",
		State:        model.MachineStateNew,
	}
	machines := []model.Machine{withHost, withoutHost}

	var gotIDs []string
	nb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dcim/devices/" {
			t.Fatalf("unexpected NetBox path %s", r.URL.Path)
		}
		gotIDs = r.URL.Query()["id"]
		writeJSONList(w, []netbox.Device{{ID: devID, Name: "cpu42.pop1", Serial: "SN-HOST"}})
	}))
	t.Cleanup(nb.Close)

	s := &Server{
		netbox: netbox.NewClient(netbox.ClientConfig{BaseURL: nb.URL, Token: "t"}),
	}
	if err := s.enrichMachineHostnames(t.Context(), machines); err != nil {
		t.Fatal(err)
	}
	if len(gotIDs) != 1 || gotIDs[0] != "42" {
		t.Fatalf("NetBox id query = %v, want [42]", gotIDs)
	}

	hostnameBySerial := map[string]string{}
	for _, m := range machines {
		hostnameBySerial[m.NetBoxSerial] = m.Hostname
	}
	if got := hostnameBySerial["SN-HOST"]; got != "cpu42.pop1" {
		t.Errorf("hostname for bound machine = %q, want cpu42.pop1", got)
	}
	if got := hostnameBySerial["SN-NO-HOST"]; got != "" {
		t.Errorf("hostname for unbound machine = %q, want empty", got)
	}
}

func TestEnrichMachineHostnamesFailsOnStaleNetBoxBinding(t *testing.T) {
	devID := 99
	machines := []model.Machine{{
		ID:             uuid.New(),
		NetBoxDeviceID: &devID,
		NetBoxSerial:   "SN-STALE",
		State:          model.MachineStateRunning,
	}}

	nb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONList(w, []netbox.Device{})
	}))
	t.Cleanup(nb.Close)

	s := &Server{netbox: netbox.NewClient(netbox.ClientConfig{BaseURL: nb.URL, Token: "t"})}
	err := s.enrichMachineHostnames(t.Context(), machines)
	if err == nil {
		t.Fatal("expected stale NetBox binding to fail")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("error should identify missing device id, got %v", err)
	}
}
