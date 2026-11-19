package netbox

// fakeNetBox is a stateful, deliberately strict in-process model of the
// slice of the NetBox REST API that ReconcileInterfaces and the paginated
// client exercise. Unlike the per-test fixtures, it enforces the server-side
// constraints real NetBox enforces:
//
//   - device+name uniqueness on interfaces (400 on POST/PATCH collision)
//   - primary_mac_address must reference a MAC object assigned to the
//     interface being patched (400 otherwise)
//   - deleting an interface cascades to its MAC objects
//   - MAC addresses are normalised to upper case on write
//   - list endpoints paginate with limit/offset and an absolute next URL
//
// It also injects faults: the request numbered faultAt (1-based, counted
// over requests whose method is in faultMethods) answers faultStatus
// instead of being applied.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type fnbIface struct {
	ID          int
	Device      int
	Name        string
	Type        string
	MgmtOnly    bool
	Enabled     bool
	Description string
	Speed       *int
	PrimaryMAC  int // MAC object id; 0 = none
}

type fnbMAC struct {
	ID      int
	MAC     string
	IfaceID int
}

type fakeNetBox struct {
	mu     sync.Mutex
	ifaces map[int]*fnbIface
	macs   map[int]*fnbMAC
	nextID int

	srv      *httptest.Server
	basePath string // e.g. "" or "/netbox"

	// fault injection
	faultMethods map[string]bool
	faultAt      int
	faultStatus  int
	// faultApplied applies the faulted write before answering faultStatus
	// (a proxy timeout after the backend committed).
	faultApplied   bool
	pendingApplied bool
	counted        int

	// observability
	writes    []string // "POST /api/dcim/interfaces/ name=eth0" etc.
	rejected  []string // 400s the fake returned (uniqueness/validation)
	requests  int
	faultsHit int
}

func newFakeNetBox() *fakeNetBox {
	return &fakeNetBox{
		ifaces: map[int]*fnbIface{},
		macs:   map[int]*fnbMAC{},
		nextID: 100,
	}
}

func (f *fakeNetBox) start() {
	mux := http.NewServeMux()
	mux.HandleFunc(f.basePath+"/api/dcim/interfaces/", f.handleIfaces)
	mux.HandleFunc(f.basePath+"/api/dcim/mac-addresses/", f.handleMACs)
	f.srv = httptest.NewServer(mux)
}

func (f *fakeNetBox) close() { f.srv.Close() }

// reset clears all objects, logs and fault state but keeps the listening
// server, so one server can serve thousands of rapid cases without
// exhausting ephemeral ports.
func (f *fakeNetBox) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ifaces = map[int]*fnbIface{}
	f.macs = map[int]*fnbMAC{}
	f.nextID = 100
	f.faultMethods = nil
	f.faultAt = 0
	f.faultStatus = 0
	f.faultApplied = false
	f.pendingApplied = false
	f.counted = 0
	f.writes = nil
	f.rejected = nil
	f.requests = 0
	f.faultsHit = 0
}

func (f *fakeNetBox) url() string { return f.srv.URL + f.basePath }

// addIface seeds a row (and its MAC object when mac != "").
func (f *fakeNetBox) addIface(dev int, name, typ string, mgmt bool, mac string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := f.nextID
	f.ifaces[id] = &fnbIface{ID: id, Device: dev, Name: name, Type: typ, MgmtOnly: mgmt, Enabled: true}
	if mac != "" {
		f.nextID++
		f.macs[f.nextID] = &fnbMAC{ID: f.nextID, MAC: strings.ToUpper(mac), IfaceID: id}
		f.ifaces[id].PrimaryMAC = f.nextID
	}
	return id
}

func (f *fakeNetBox) primaryMAC(i *fnbIface) string {
	if i.PrimaryMAC == 0 {
		return ""
	}
	if m, ok := f.macs[i.PrimaryMAC]; ok {
		return m.MAC
	}
	return ""
}

func (f *fakeNetBox) render(i *fnbIface) map[string]any {
	out := map[string]any{
		"id":          i.ID,
		"device":      map[string]any{"id": i.Device, "name": fmt.Sprintf("dev-%d", i.Device)},
		"name":        i.Name,
		"type":        map[string]any{"value": i.Type, "label": i.Type},
		"enabled":     i.Enabled,
		"mgmt_only":   i.MgmtOnly,
		"description": i.Description,
		"speed":       nil,
		"lag":         nil,
		"cable":       nil,
	}
	if i.Speed != nil {
		out["speed"] = *i.Speed
	}
	var all []map[string]any
	ids := make([]int, 0)
	for id, m := range f.macs {
		if m.IfaceID == i.ID {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	for _, id := range ids {
		all = append(all, map[string]any{"id": id, "mac_address": f.macs[id].MAC})
	}
	if all == nil {
		all = []map[string]any{}
	}
	out["mac_addresses"] = all
	if pm := f.primaryMAC(i); pm != "" {
		out["mac_address"] = pm
		out["primary_mac_address"] = map[string]any{"id": i.PrimaryMAC, "mac_address": pm}
	} else {
		out["mac_address"] = nil
		out["primary_mac_address"] = nil
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maybeFault returns true (after writing the fault response) when this
// request is the scheduled fault. Caller holds f.mu.
func (f *fakeNetBox) maybeFault(w http.ResponseWriter, r *http.Request) bool {
	f.requests++
	if f.faultAt == 0 || !f.faultMethods[r.Method] {
		return false
	}
	f.counted++
	if f.counted != f.faultAt {
		return false
	}
	f.faultsHit++
	if f.faultApplied {
		f.pendingApplied = true
		return false
	}
	writeJSON(w, f.faultStatus, map[string]any{"detail": "injected fault"})
	return true
}

// swallow, when the current request is an applied fault, returns a writer
// that discards the real response and arranges for the fault status to be
// sent instead once the handler returns. Caller holds f.mu.
func (f *fakeNetBox) swallow(w http.ResponseWriter) (http.ResponseWriter, func()) {
	if !f.pendingApplied {
		return w, func() {}
	}
	f.pendingApplied = false
	status := f.faultStatus
	return httptest.NewRecorder(), func() {
		writeJSON(w, status, map[string]any{"detail": "injected fault after commit"})
	}
}

func (f *fakeNetBox) reject(w http.ResponseWriter, msg string) {
	f.rejected = append(f.rejected, msg)
	writeJSON(w, http.StatusBadRequest, map[string]any{"__all__": []string{msg}})
}

func (f *fakeNetBox) nameTaken(dev int, name string, except int) bool {
	for _, i := range f.ifaces {
		if i.Device == dev && i.Name == name && i.ID != except {
			return true
		}
	}
	return false
}

func (f *fakeNetBox) handleIfaces(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maybeFault(w, r) {
		return
	}
	w, finish := f.swallow(w)
	defer finish()
	rest := strings.TrimPrefix(r.URL.Path, f.basePath+"/api/dcim/interfaces/")
	rest = strings.TrimSuffix(rest, "/")
	switch r.Method {
	case http.MethodGet:
		if rest != "" {
			id, _ := strconv.Atoi(rest)
			i, ok := f.ifaces[id]
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
				return
			}
			writeJSON(w, http.StatusOK, f.render(i))
			return
		}
		q := r.URL.Query()
		devFilter := map[int]bool{}
		for _, d := range q["device_id"] {
			n, _ := strconv.Atoi(d)
			devFilter[n] = true
		}
		nameFilter := q.Get("name")
		var ids []int
		for id, i := range f.ifaces {
			if len(devFilter) > 0 && !devFilter[i.Device] {
				continue
			}
			if nameFilter != "" && i.Name != nameFilter {
				continue
			}
			ids = append(ids, id)
		}
		sort.Ints(ids)
		f.paginate(w, r, ids, func(id int) any { return f.render(f.ifaces[id]) })
	case http.MethodPost:
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			f.reject(w, "bad json")
			return
		}
		dev := intOf(body["device"])
		name, _ := body["name"].(string)
		if name == "" {
			f.reject(w, "name required")
			return
		}
		if f.nameTaken(dev, name, 0) {
			f.reject(w, fmt.Sprintf("Interface with this Device and Name already exists: %s", name))
			return
		}
		f.nextID++
		i := &fnbIface{ID: f.nextID, Device: dev, Name: name, Enabled: true}
		applyIfaceFields(i, body)
		f.ifaces[i.ID] = i
		f.writes = append(f.writes, fmt.Sprintf("POST iface %d name=%s", i.ID, name))
		writeJSON(w, http.StatusCreated, f.render(i))
	case http.MethodPatch:
		id, _ := strconv.Atoi(rest)
		i, ok := f.ifaces[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			f.reject(w, "bad json")
			return
		}
		if n, ok := body["name"].(string); ok && n != i.Name && f.nameTaken(i.Device, n, i.ID) {
			f.reject(w, fmt.Sprintf("Interface with this Device and Name already exists: %s", n))
			return
		}
		if v, ok := body["primary_mac_address"]; ok && v != nil {
			mid := intOf(v)
			m, ok := f.macs[mid]
			if !ok || m.IfaceID != i.ID {
				f.reject(w, "The specified MAC address is not assigned to this interface.")
				return
			}
		}
		applyIfaceFields(i, body)
		if v, ok := body["primary_mac_address"]; ok {
			if v == nil {
				i.PrimaryMAC = 0
			} else {
				i.PrimaryMAC = intOf(v)
			}
		}
		f.writes = append(f.writes, fmt.Sprintf("PATCH iface %d %v", id, keys(body)))
		writeJSON(w, http.StatusOK, f.render(i))
	case http.MethodDelete:
		id, _ := strconv.Atoi(rest)
		if _, ok := f.ifaces[id]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
			return
		}
		delete(f.ifaces, id)
		for mid, m := range f.macs {
			if m.IfaceID == id {
				delete(f.macs, mid)
			}
		}
		f.writes = append(f.writes, fmt.Sprintf("DELETE iface %d", id))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

func (f *fakeNetBox) handleMACs(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maybeFault(w, r) {
		return
	}
	w, finish := f.swallow(w)
	defer finish()
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		f.reject(w, "bad json")
		return
	}
	mac, _ := body["mac_address"].(string)
	if body["assigned_object_type"] != "dcim.interface" {
		f.reject(w, "assigned_object_type must be dcim.interface")
		return
	}
	iid := intOf(body["assigned_object_id"])
	if _, ok := f.ifaces[iid]; !ok {
		f.reject(w, "assigned object does not exist")
		return
	}
	f.nextID++
	m := &fnbMAC{ID: f.nextID, MAC: strings.ToUpper(mac), IfaceID: iid}
	f.macs[m.ID] = m
	f.writes = append(f.writes, fmt.Sprintf("POST mac %d %s -> iface %d", m.ID, m.MAC, iid))
	writeJSON(w, http.StatusCreated, map[string]any{"id": m.ID, "mac_address": m.MAC})
}

// paginate renders ids[offset:offset+limit] with an absolute next URL the
// way NetBox does (scheme://host + full path incl. any BASE_PATH).
func (f *fakeNetBox) paginate(w http.ResponseWriter, r *http.Request, ids []int, render func(int) any) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	end := min(offset+limit, len(ids))
	results := []any{}
	for _, id := range ids[min(offset, len(ids)):end] {
		results = append(results, render(id))
	}
	var next any
	if end < len(ids) {
		nq := url.Values{}
		for k, v := range q {
			nq[k] = v
		}
		nq.Set("limit", strconv.Itoa(limit))
		nq.Set("offset", strconv.Itoa(end))
		next = "http://" + r.Host + r.URL.Path + "?" + nq.Encode()
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(ids), "next": next, "previous": nil, "results": results})
}

func applyIfaceFields(i *fnbIface, body map[string]any) {
	if v, ok := body["name"].(string); ok {
		i.Name = v
	}
	if v, ok := body["type"].(string); ok {
		i.Type = v
	}
	if v, ok := body["mgmt_only"].(bool); ok {
		i.MgmtOnly = v
	}
	if v, ok := body["enabled"].(bool); ok {
		i.Enabled = v
	}
	if v, ok := body["description"].(string); ok {
		i.Description = v
	}
	if v, ok := body["speed"]; ok {
		if v == nil {
			i.Speed = nil
		} else {
			n := intOf(v)
			i.Speed = &n
		}
	}
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	case map[string]any:
		return intOf(x["id"])
	}
	return 0
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fnbRow is the canonical, comparison-friendly view of one interface.
type fnbRow struct {
	ID       int
	Name     string
	Type     string
	MgmtOnly bool
	MAC      string // primary, upper case
}

func (f *fakeNetBox) rows(dev int) []fnbRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fnbRow
	for _, i := range f.ifaces {
		if i.Device != dev {
			continue
		}
		out = append(out, fnbRow{ID: i.ID, Name: i.Name, Type: i.Type, MgmtOnly: i.MgmtOnly, MAC: f.primaryMAC(i)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}
