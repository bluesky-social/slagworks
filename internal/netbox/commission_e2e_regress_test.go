//go:build commissionfindings

package netbox

// Deterministic regressions for the commission write-back findings of the
// commission e2e property harness (commission_e2e_prop_test.go). Each
// test is standalone: explicit inputs, an inline stub of the NetBox
// endpoints it touches, and an explicit expected outcome. The real-NetBox
// twins are in internal/netboxtest/commission_writeback_integration_test.go.
// They fail on main until the fixes land, so they build only with
// -tags commissionfindings.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Two PSUs whose DMI type 39 Location is the same placeholder: the shape
// five Dell R640 hosts ("Not Specified") and one mini PC ("Default
// string") report in production. ReconcileModules keys a module by its
// bay name, so two modules with one bay name cannot both be stored.
func TestCommissionToSync_SameLocationPSUsGetDistinctBays(t *testing.T) {
	report := `{"psus":[
		{"location":"Not Specified","model":"PWR SPLY,750W","serial":"PSU-A","max_watts":750},
		{"location":"Not Specified","model":"PWR SPLY,750W","serial":"PSU-B","max_watts":750}]}`
	s, err := CommissionToSync(1, json.RawMessage(report))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range s.Modules {
		if prev, dup := seen[m.BayName]; dup {
			t.Fatalf("modules %s and %s share bay name %q; want one bay per reported PSU", prev, m.Serial, m.BayName)
		}
		seen[m.BayName] = m.Serial
	}
	if len(seen) != 2 {
		t.Fatalf("got %d PSU bays, want 2", len(seen))
	}
}

// moduleStub serves the module endpoints ReconcileModules uses, with
// NetBox's module-bay occupancy rule (a second module in one bay is a 500
// IntegrityError, as NetBox 4.5 answers) and the bay -> module cascade.
type moduleStub struct {
	mu      sync.Mutex
	next    int
	bays    map[int]string // id -> name
	modules map[int]int    // module id -> bay id
	patches map[int][]map[string]any
}

func (s *moduleStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	page := func(w http.ResponseWriter, results []map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(results), "results": results})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/manufacturers/", func(w http.ResponseWriter, r *http.Request) {
		page(w, []map[string]any{{"id": 1, "name": "Unknown", "slug": "unknown"}})
	})
	mux.HandleFunc("/api/dcim/module-type-profiles/", func(w http.ResponseWriter, r *http.Request) {
		page(w, []map[string]any{{"id": 1, "name": r.URL.Query().Get("name")}})
	})
	mux.HandleFunc("/api/dcim/module-types/", func(w http.ResponseWriter, r *http.Request) {
		page(w, []map[string]any{{"id": 7, "model": r.URL.Query().Get("model")}})
	})
	mux.HandleFunc("/api/dcim/module-bays/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id, _ := strconv.Atoi(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/dcim/module-bays/"), "/"))
		switch r.Method {
		case http.MethodGet:
			var ids []int
			for id := range s.bays {
				ids = append(ids, id)
			}
			sort.Ints(ids)
			var out []map[string]any
			for _, id := range ids {
				out = append(out, map[string]any{"id": id, "name": s.bays[id]})
			}
			page(w, out)
		case http.MethodPost:
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			s.next++
			s.bays[s.next] = body["name"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": s.next, "name": body["name"]})
		case http.MethodDelete:
			delete(s.bays, id)
			for mid, bay := range s.modules {
				if bay == id {
					delete(s.modules, mid)
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/dcim/modules/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id, _ := strconv.Atoi(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/dcim/modules/"), "/"))
		switch r.Method {
		case http.MethodGet:
			var out []map[string]any
			for mid, bay := range s.modules {
				out = append(out, map[string]any{"id": mid, "module_bay": map[string]any{"id": bay}})
			}
			page(w, out)
		case http.MethodPost, http.MethodPatch:
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			bay := int(body["module_bay"].(float64))
			for mid, b := range s.modules {
				if b == bay && mid != id {
					http.Error(w, `{"error":"duplicate key value violates unique constraint \"dcim_module_module_bay_id_key\"","exception":"IntegrityError"}`, http.StatusInternalServerError)
					return
				}
			}
			if r.Method == http.MethodPost {
				s.next++
				id = s.next
			} else {
				if s.patches == nil {
					s.patches = map[int][]map[string]any{}
				}
				s.patches[id] = append(s.patches[id], body)
			}
			s.modules[id] = bay
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case http.MethodDelete:
			delete(s.modules, id)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return httptest.NewServer(mux)
}

// The NetBox state the first-run failure above left on five production
// hosts: two bays named "Not Specified", one holding the PSU module and
// one empty. A recommission whose report names the PSU bays distinctly
// must leave one occupied bay per PSU and no orphan bay. Today the stale
// sweep walks a name-keyed map, so one of two same-named stale bays is
// never deleted.
func TestReconcileModules_RemovesEveryStaleSameNamedBay(t *testing.T) {
	stub := &moduleStub{next: 100, bays: map[int]string{1: "Not Specified", 2: "Not Specified"}, modules: map[int]int{50: 2}}
	srv := stub.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileModules(context.Background(), 1, []ModuleData{
		{BayName: "PSU_0", ProfileName: "Power supply", Manufacturer: "Unknown", Model: "PWR SPLY 750W", Serial: "PSU-A"},
		{BayName: "PSU_1", ProfileName: "Power supply", Manufacturer: "Unknown", Model: "PWR SPLY 750W", Serial: "PSU-B"},
	})
	if err != nil {
		t.Fatalf("ReconcileModules: %v", err)
	}
	occupied := map[int]bool{}
	for _, bay := range stub.modules {
		occupied[bay] = true
	}
	var names []string
	for id, name := range stub.bays {
		names = append(names, name)
		if !occupied[id] {
			t.Errorf("bay %d %q is empty after reconcile; want no orphan bays", id, name)
		}
	}
	sort.Strings(names)
	if fmt.Sprint(names) != "[PSU_0 PSU_1]" {
		t.Errorf("bays after reconcile = %v, want [PSU_0 PSU_1]", names)
	}
}

// A device's asset_tag is unique across NetBox. DMI chassis asset tags
// are firmware-provided: a placeholder outside the junk list ("Chassis
// Asset Tag" is AMI's default) or a board moved between chassis reports a
// tag another device already holds. That one datum must cost the
// asset_tag field, not the whole commission of the second host.
func TestSyncCommissionData_AssetTagHeldByAnotherDeviceDegrades(t *testing.T) {
	var patches []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/7/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "name": "host2", "tags": []any{map[string]any{"slug": "slagworks-managed"}}})
		case http.MethodPatch:
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			patches = append(patches, body)
			if at, ok := body["asset_tag"]; ok && at != nil {
				http.Error(w, `{"asset_tag":["device with this asset tag already exists."]}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
		}
	})
	mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []any{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	s, err := CommissionToSync(7, json.RawMessage(`{"system":{"serial_number":"S-B","asset_tag":"Chassis Asset Tag","board_vendor":"Vendor"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SyncCommissionData(context.Background(), *s); err != nil {
		t.Fatalf("SyncCommissionData failed on an asset tag another device holds: %v", err)
	}
	last := patches[len(patches)-1]
	if last["serial"] != "S-B" {
		t.Errorf("device PATCH that landed carried serial %v, want S-B (the rest of the device fields must still be written)", last["serial"])
	}
}

// The synthetic BMC NIC ("ipmi", mgmt_only) must reconcile onto the
// device's BMC row whenever one exists, not only when that row already
// carries the reported MAC under the name "ipmi":
//
//   - a MAC-less mgmt_only "ipmi" row is what a commission leaves behind
//     when its MAC-object POST fails after the interface POST (and what an
//     operator models before the MAC is known); today every replay then
//     fails with "modeling conflict, resolve in NetBox", forever;
//   - a mgmt_only row carrying the BMC MAC under the operator's name
//     ("ipmi0", the shape of all 163 production BMC rows) gets a second
//     mgmt_only "ipmi" row with the same MAC beside it.
func TestReconcileInterfaces_BMCNICAdoptsMACLessIPMIRow(t *testing.T) {
	bmc := ifaceRow(90, "ipmi", "1000base-t", "", nil)
	bmc["mgmt_only"] = true
	f := newReconcileFixture([]map[string]any{bmc})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "ipmi", MAC: "d0:50:99:aa:bb:cc", IntfType: "1000base-t", MgmtOnly: true},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces with a MAC-less ipmi row: %v", err)
	}
	if len(f.posts) != 0 {
		t.Errorf("interface POSTs = %v, want none (row 90 is the BMC row)", f.posts)
	}
	if len(f.macs) != 1 || f.macs[0]["assigned_object_id"] != float64(90) {
		t.Errorf("MAC-object POSTs = %v, want one assigned to row 90", f.macs)
	}
}

func TestReconcileInterfaces_BMCNICRecognisesOperatorIPMI0Row(t *testing.T) {
	bmc := ifaceRow(91, "ipmi0", "1000base-t", "D0:50:99:AA:BB:01", nil)
	bmc["mgmt_only"] = true
	f := newReconcileFixture([]map[string]any{bmc})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "ipmi", MAC: "d0:50:99:aa:bb:01", IntfType: "1000base-t", MgmtOnly: true},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if len(f.posts) != 0 {
		t.Errorf("interface POSTs = %v, want none: ipmi0 already is the BMC row with that MAC", f.posts)
	}
	if ps := f.patches[91]; len(ps) != 0 {
		t.Errorf("row 91 (operator BMC row) PATCHes = %v, want none", ps)
	}
}

// A replaced disk, DIMM or PSU whose serial (or firmware version) the
// agent cannot read must not keep the serial of the part it replaced: the
// device fields are written authoritatively for the same reason
// (SyncCommissionData clears what inventory no longer reports).
func TestReconcileModules_ClearsSerialAndFirmwareOfReplacedPart(t *testing.T) {
	stub := &moduleStub{next: 100, bays: map[int]string{1: "NVMe_0"}, modules: map[int]int{50: 1}}
	srv := stub.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileModules(context.Background(), 1, []ModuleData{
		{BayName: "NVMe_0", ProfileName: "Hard disk", Manufacturer: "Unknown", Model: "NVME 3576GB"},
	})
	if err != nil {
		t.Fatalf("ReconcileModules: %v", err)
	}
	ps := stub.patches[50]
	if len(ps) != 1 {
		t.Fatalf("module 50 PATCHes = %v, want exactly one", ps)
	}
	if serial, ok := ps[0]["serial"]; !ok || serial != "" {
		t.Errorf("module 50 PATCH serial = %v (present=%v), want \"\" to clear the replaced part's serial", serial, ok)
	}
	cf, _ := ps[0]["custom_fields"].(map[string]any)
	if fw, ok := cf["firmware_version"]; !ok || fw != nil {
		t.Errorf("module 50 PATCH custom_fields = %v, want firmware_version: null", ps[0]["custom_fields"])
	}
}
