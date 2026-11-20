package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// moduleStub is a stateful model of the module endpoints ReconcileModules
// uses. It enforces the two NetBox rules that matter for module
// reconciliation: a module bay holds at most one module (a second module
// in an occupied bay is a 500 IntegrityError, as NetBox 4.5 answers), and
// deleting a bay deletes the module in it. Like NetBox, it accepts any
// number of device-level bays with the same name.
type moduleStub struct {
	mu      sync.Mutex
	next    int
	bays    map[int]string        // bay id -> name
	modules map[int]stubModuleRow // module id -> row
}

type stubModuleRow struct {
	bay    int
	serial string
}

func newModuleStub() *moduleStub {
	return &moduleStub{next: 100, bays: map[int]string{}, modules: map[int]stubModuleRow{}}
}

func (s *moduleStub) handler() http.Handler {
	page := func(w http.ResponseWriter, results []map[string]any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(results), "results": results})
	}
	pathID := func(r *http.Request, prefix string) int {
		id, _ := strconv.Atoi(strings.Trim(strings.TrimPrefix(r.URL.Path, prefix), "/"))
		return id
	}
	decode := func(r *http.Request) map[string]any {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		return body
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/manufacturers/", func(w http.ResponseWriter, r *http.Request) {
		page(w, []map[string]any{{"id": 1, "name": r.URL.Query().Get("name"), "slug": "m"}})
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
		switch r.Method {
		case http.MethodGet:
			ids := make([]int, 0, len(s.bays))
			for id := range s.bays {
				ids = append(ids, id)
			}
			sort.Ints(ids)
			out := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				out = append(out, map[string]any{"id": id, "name": s.bays[id]})
			}
			page(w, out)
		case http.MethodPost:
			name, _ := decode(r)["name"].(string)
			s.next++
			s.bays[s.next] = name
			_ = json.NewEncoder(w).Encode(map[string]any{"id": s.next, "name": name})
		case http.MethodDelete:
			id := pathID(r, "/api/dcim/module-bays/")
			delete(s.bays, id)
			for mid, row := range s.modules {
				if row.bay == id {
					delete(s.modules, mid)
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/dcim/modules/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		id := pathID(r, "/api/dcim/modules/")
		switch r.Method {
		case http.MethodGet:
			out := make([]map[string]any, 0, len(s.modules))
			for mid, row := range s.modules {
				out = append(out, map[string]any{"id": mid, "module_bay": map[string]any{"id": row.bay}, "serial": row.serial})
			}
			page(w, out)
		case http.MethodPost, http.MethodPatch:
			body := decode(r)
			bayF, _ := body["module_bay"].(float64)
			bay := int(bayF)
			for mid, row := range s.modules {
				if row.bay == bay && mid != id {
					http.Error(w, `{"error":"duplicate key value violates unique constraint \"dcim_module_module_bay_id_key\"","exception":"IntegrityError"}`, http.StatusInternalServerError)
					return
				}
			}
			if r.Method == http.MethodPost {
				s.next++
				id = s.next
			}
			row := s.modules[id]
			row.bay = bay
			if serial, ok := body["serial"].(string); ok {
				row.serial = serial
			}
			s.modules[id] = row
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		case http.MethodDelete:
			delete(s.modules, id)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

// state returns "bay name=module serial" per bay, sorted, with "<empty>"
// for a bay that holds no module.
func (s *moduleStub) state() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	serialByBay := map[int]string{}
	for _, row := range s.modules {
		serialByBay[row.bay] = row.serial
	}
	out := make([]string, 0, len(s.bays))
	for id, name := range s.bays {
		serial, ok := serialByBay[id]
		if !ok {
			serial = "<empty>"
		}
		out = append(out, name+"="+serial)
	}
	sort.Strings(out)
	return out
}

// Two PSUs whose DMI type 39 Location is the same placeholder ("Not
// Specified" on Dell R640s) used to become two modules with one bay name.
// The first sync created two same-named bays, put both modules into the
// second and failed on NetBox's one-module-per-bay constraint; the outbox
// replay then mapped both modules onto one bay and PATCHed it twice, so one
// PSU vanished and an empty bay stayed behind. Both runs must succeed and
// leave one occupied bay per PSU.
func TestSyncModules_SameLocationPSUsKeepOneBayEach(t *testing.T) {
	report := `{"psus":[
		{"location":"Not Specified","model":"PWR SPLY","serial":"PSU-A","max_watts":750},
		{"location":"Not Specified","model":"PWR SPLY","serial":"PSU-B","max_watts":750}]}`
	s, err := CommissionToSync(1, json.RawMessage(report))
	if err != nil {
		t.Fatal(err)
	}
	stub := newModuleStub()
	c := testClient(t, stub.handler())

	for run := 1; run <= 2; run++ {
		if err := c.ReconcileModules(context.Background(), 1, s.Modules); err != nil {
			t.Fatalf("run %d: ReconcileModules: %v", run, err)
		}
	}
	want := "[PSU_0=PSU-A PSU_1=PSU-B]"
	if got := fmt.Sprint(stub.state()); got != want {
		t.Errorf("bays after two runs = %s, want %s", got, want)
	}
}

// The NetBox state the failure above left behind: two device-level bays
// named "Not Specified", one holding the surviving PSU module and one
// empty. A recommission whose modules are named PSU_0 and PSU_1 must
// remove both stale bays, not one per distinct name, whichever of them the
// API lists first.
func TestReconcileModules_RemovesEveryStaleSameNamedBay(t *testing.T) {
	for _, occupied := range []int{1, 2} {
		t.Run(fmt.Sprintf("occupied bay %d", occupied), func(t *testing.T) {
			stub := newModuleStub()
			stub.bays[1] = "Not Specified"
			stub.bays[2] = "Not Specified"
			stub.modules[50] = stubModuleRow{bay: occupied, serial: "PSU-B"}
			c := testClient(t, stub.handler())

			err := c.ReconcileModules(context.Background(), 1, []ModuleData{
				{BayName: "PSU_0", ProfileName: "Power supply", Manufacturer: "Unknown", Model: "PWR SPLY 750W", Serial: "PSU-A"},
				{BayName: "PSU_1", ProfileName: "Power supply", Manufacturer: "Unknown", Model: "PWR SPLY 750W", Serial: "PSU-B"},
			})
			if err != nil {
				t.Fatalf("ReconcileModules: %v", err)
			}
			want := "[PSU_0=PSU-A PSU_1=PSU-B]"
			if got := fmt.Sprint(stub.state()); got != want {
				t.Errorf("bays after reconcile = %s, want %s", got, want)
			}
		})
	}
}

// When a still-wanted name has several existing bays, ReconcileModules
// keeps one of them for the module and removes the rest.
func TestReconcileModules_CollapsesSameNamedBaysForWantedName(t *testing.T) {
	for _, occupied := range []int{1, 2} {
		t.Run(fmt.Sprintf("occupied bay %d", occupied), func(t *testing.T) {
			stub := newModuleStub()
			stub.bays[1] = "PSU1"
			stub.bays[2] = "PSU1"
			stub.modules[50] = stubModuleRow{bay: occupied, serial: "PSU-A"}
			c := testClient(t, stub.handler())

			err := c.ReconcileModules(context.Background(), 1, []ModuleData{
				{BayName: "PSU1", ProfileName: "Power supply", Manufacturer: "Unknown", Model: "PWR SPLY 750W", Serial: "PSU-A"},
			})
			if err != nil {
				t.Fatalf("ReconcileModules: %v", err)
			}
			want := "[PSU1=PSU-A]"
			if got := fmt.Sprint(stub.state()); got != want {
				t.Errorf("bays after reconcile = %s, want %s", got, want)
			}
		})
	}
}
