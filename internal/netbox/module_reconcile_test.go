package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// moduleFieldsFixture is a stub of the NetBox module endpoints
// ReconcileModules uses. It stores each module's serial and
// firmware_version with NetBox's PATCH semantics: a top-level field the
// body omits keeps its stored value, and custom_fields merges key by key
// into the stored set. A field the reconcile does not send therefore
// survives, as it would against a real NetBox.
type moduleFieldsFixture struct {
	mu      sync.Mutex
	next    int
	bays    map[int]string           // bay id -> name
	modules map[int]*moduleFieldsRow // module id -> stored row
}

type moduleFieldsRow struct {
	bay      int
	serial   string
	firmware any // nil (NetBox null) or string
}

func (f *moduleFieldsFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var body map[string]any
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("%s %s: bad body: %v", r.Method, r.URL.Path, err)
			}
		}
		q := r.URL.Query()
		switch p := r.URL.Path; {
		case p == "/api/dcim/manufacturers/" && r.Method == http.MethodGet:
			writeList(w, []map[string]any{{"id": 1, "name": q.Get("name"), "slug": "unknown"}})
		case p == "/api/dcim/module-type-profiles/" && r.Method == http.MethodGet:
			writeList(w, []map[string]any{{"id": 1, "name": q.Get("name")}})
		case p == "/api/dcim/module-types/" && r.Method == http.MethodGet:
			writeList(w, []map[string]any{{"id": 1, "model": q.Get("model")}})
		case p == "/api/dcim/module-bays/" && r.Method == http.MethodGet:
			rows := []map[string]any{}
			for id, name := range f.bays {
				rows = append(rows, map[string]any{"id": id, "name": name})
			}
			writeList(w, rows)
		case p == "/api/dcim/module-bays/" && r.Method == http.MethodPost:
			f.next++
			name, _ := body["name"].(string)
			f.bays[f.next] = name
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.next, "name": name})
		case p == "/api/dcim/modules/" && r.Method == http.MethodGet:
			rows := []map[string]any{}
			for id, m := range f.modules {
				rows = append(rows, map[string]any{"id": id, "module_bay": map[string]any{"id": m.bay}})
			}
			writeList(w, rows)
		case p == "/api/dcim/modules/" && r.Method == http.MethodPost:
			f.next++
			row := &moduleFieldsRow{}
			f.modules[f.next] = row
			row.apply(body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.next})
		case strings.HasPrefix(p, "/api/dcim/modules/") && r.Method == http.MethodPatch:
			id, _ := strconv.Atoi(strings.Trim(strings.TrimPrefix(p, "/api/dcim/modules/"), "/"))
			row, ok := f.modules[id]
			if !ok {
				t.Errorf("PATCH of unknown module %d", id)
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			row.apply(body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	})
}

// apply stores the fields body carries and leaves the others unchanged.
func (m *moduleFieldsRow) apply(body map[string]any) {
	if bay, ok := body["module_bay"].(float64); ok {
		m.bay = int(bay)
	}
	if serial, ok := body["serial"].(string); ok {
		m.serial = serial
	}
	if cf, ok := body["custom_fields"].(map[string]any); ok {
		if fw, ok := cf["firmware_version"]; ok {
			m.firmware = fw
		}
	}
}

// state lists each stored module as its bay name, serial and
// firmware_version, sorted.
func (f *moduleFieldsFixture) state() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.modules))
	for _, m := range f.modules {
		out = append(out, fmt.Sprintf("%s serial=%q firmware_version=%v", f.bays[m.bay], m.serial, m.firmware))
	}
	slices.Sort(out)
	return out
}

// A disk or PSU replaced in the same bay by a part whose serial or
// firmware version reads empty (a PSU whose DMI serial is a placeholder,
// a disk lsblk lists without a serial, a failed smartctl firmware read)
// must not keep the replaced part's serial and firmware_version. The
// re-commission PATCHes the module already in the bay, and NetBox keeps
// any field a PATCH omits, so the empty reading has to be written.
func TestReconcileModules_ReplacedPartWithEmptySerialClearsOldValues(t *testing.T) {
	f := &moduleFieldsFixture{next: 100, bays: map[int]string{}, modules: map[int]*moduleFieldsRow{}}
	c := testClient(t, f.handler(t))
	ctx := context.Background()

	disk := ModuleData{BayName: "NVMe_0", ProfileName: "Hard disk", Manufacturer: "Unknown",
		Model: "NVME 3576GB", Serial: "TEST-DISK-OLD", FWVersion: "TEST-FW-OLD"}
	psu := ModuleData{BayName: "PSU1", ProfileName: "Power supply", Manufacturer: "Unknown",
		Model: "Power Supply 1600W", Serial: "TEST-PSU-OLD"}

	if err := c.ReconcileModules(ctx, 1, []ModuleData{disk, psu}); err != nil {
		t.Fatalf("first commission: ReconcileModules: %v", err)
	}
	wantFirst := []string{
		`NVMe_0 serial="TEST-DISK-OLD" firmware_version=TEST-FW-OLD`,
		`PSU1 serial="TEST-PSU-OLD" firmware_version=<nil>`,
	}
	if got := f.state(); !slices.Equal(got, wantFirst) {
		t.Fatalf("after first commission modules = %q, want %q", got, wantFirst)
	}

	// Both parts are swapped for ones whose serial and firmware read empty.
	disk.Serial, disk.FWVersion = "", ""
	psu.Serial = ""
	if err := c.ReconcileModules(ctx, 1, []ModuleData{disk, psu}); err != nil {
		t.Fatalf("re-commission: ReconcileModules: %v", err)
	}
	want := []string{
		`NVMe_0 serial="" firmware_version=<nil>`,
		`PSU1 serial="" firmware_version=<nil>`,
	}
	if got := f.state(); !slices.Equal(got, want) {
		t.Errorf("after re-commission modules = %q, want %q (the replaced parts' serial and firmware_version must be cleared)", got, want)
	}
}
