// Package nbfake is a strict, stateful, in-process model of the slice of the
// NetBox 4.6 REST API that commission write-back exercises: devices, device
// types, tags, interfaces, MAC address objects, cables, manufacturers,
// module-type profiles, module types, module bays and modules.
//
// It is a test helper. Unlike the per-test httptest fixtures it enforces the
// server-side constraints a real NetBox enforces, so a write sequence the
// real server would refuse is refused here too:
//
//   - interface device+name uniqueness; interface type must be a NetBox
//     choice; name <= 64, description <= 200
//   - primary_mac_address must reference a MAC object assigned to that
//     interface; MAC objects must be EUI-48
//   - device asset_tag is globally unique; serial <= 50; unknown custom
//     fields and unknown tags are 400s; status must be a NetBox choice
//   - manufacturer name and slug unique; module type (manufacturer, model)
//     unique, model required; attribute_data is validated against the
//     profile schema (required keys, JSON types, enums) only with
//     ValidateAttributeData
//   - module bay position <= 30, name <= 64 (names are NOT unique per
//     device: NetBox's constraint includes the nullable parent module, so
//     a device can hold two same-named bays)
//   - a module's bay must exist on the module's device and be unoccupied;
//     module status must be a NetBox choice; serial <= 50
//   - a cable's terminations must exist, be distinct, be uncabled, and not
//     be virtual or LAG interfaces
//   - deleting an interface deletes its MAC objects and its cable; deleting
//     a module bay deletes its module
//   - list endpoints paginate with limit/offset and an absolute next URL
//
// Fault injection: setting FaultAt to N arms one fault on the Nth write
// (POST/PATCH/DELETE) from then on. That write answers FaultStatus, either
// without being applied or, with FaultApplied, after being applied (a proxy
// timeout after the backend committed).
package nbfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Device is a dcim.device row.
type Device struct {
	ID           int
	Name         string
	Serial       string
	AssetTag     *string
	Status       string
	SiteID       int
	RoleSlug     string
	DeviceTypeID int
	Tags         []string // slugs
	CustomFields map[string]any
}

// DeviceType is a dcim.device-type row.
type DeviceType struct {
	ID           int
	Model        string
	CustomFields map[string]any
}

// Iface is a dcim.interface row.
type Iface struct {
	ID          int
	Device      int
	Name        string
	Type        string
	MgmtOnly    bool
	Enabled     bool
	Description string
	Speed       *int
	PrimaryMAC  int // MAC object id; 0 = none
	LAG         int // parent lag interface id; 0 = none
	Cable       int // cable id; 0 = none
}

// MAC is a dcim.mac-address row.
type MAC struct {
	ID      int
	MAC     string
	IfaceID int
}

// Cable is a dcim.cable row between two interfaces.
type Cable struct {
	ID   int
	A, B int // interface ids
}

// Manufacturer is a dcim.manufacturer row.
type Manufacturer struct {
	ID   int
	Name string
	Slug string
}

// Profile is a dcim.module-type-profile row with a reduced JSON schema.
type Profile struct {
	ID       int
	Name     string
	Props    map[string]PropSchema
	Required []string
}

// PropSchema is one property of a profile schema.
type PropSchema struct {
	Type string   // "integer", "number", "string", "boolean"
	Enum []string // string enum, when non-nil
}

// ModuleType is a dcim.module-type row.
type ModuleType struct {
	ID            int
	Manufacturer  int
	Model         string
	Profile       int
	PartNumber    string
	AttributeData map[string]any
}

// Bay is a dcim.module-bay row.
type Bay struct {
	ID       int
	Device   int
	Name     string
	Position string
}

// Module is a dcim.module row.
type Module struct {
	ID           int
	Device       int
	Bay          int
	ModuleType   int
	Status       string
	Serial       string
	CustomFields map[string]any
}

// Server is the fake NetBox.
type Server struct {
	mu   sync.Mutex
	next int

	Devices       map[int]*Device
	DeviceTypes   map[int]*DeviceType
	TagSlugs      map[string]bool
	Ifaces        map[int]*Iface
	MACs          map[int]*MAC
	Cables        map[int]*Cable
	Manufacturers map[int]*Manufacturer
	Profiles      map[int]*Profile
	ModuleTypes   map[int]*ModuleType
	Bays          map[int]*Bay
	Modules       map[int]*Module

	DeviceCFs     map[string]bool
	DeviceTypeCFs map[string]bool
	ModuleCFs     map[string]bool

	// PageSize is the default page size when a request sends no limit.
	PageSize int
	// MaxPageSize clamps any requested limit (NetBox's MAX_PAGE_SIZE); 0
	// leaves it unclamped.
	MaxPageSize int

	// ValidateAttributeData checks a module type's attribute_data against
	// its profile schema on create. Off by default: a NetBox 4.5 container
	// accepted a Memory module type with class "LPDDR5", outside the
	// shipped profile's enum, so real NetBox does not enforce it there.
	ValidateAttributeData bool

	// Fault injection. FaultAt counts down over writes and fires at zero,
	// so it is relative to the moment it was set.
	FaultAt      int
	FaultStatus  int
	FaultApplied bool
	FaultsHit    int

	// Observability.
	Writes   []string
	Rejected []string
	Requests int

	srv *httptest.Server
}

// New returns an empty fake with NetBox's built-in module-type profiles
// (schemas as shipped by NetBox 4.6) and slagworks' custom fields defined.
func New() *Server {
	s := &Server{PageSize: 50}
	s.Reset()
	return s
}

// Reset drops every object and log, keeping the listener.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = 1000
	s.Devices = map[int]*Device{}
	s.DeviceTypes = map[int]*DeviceType{}
	s.TagSlugs = map[string]bool{"slagworks-managed": true}
	s.Ifaces = map[int]*Iface{}
	s.MACs = map[int]*MAC{}
	s.Cables = map[int]*Cable{}
	s.Manufacturers = map[int]*Manufacturer{}
	s.Profiles = map[int]*Profile{}
	s.ModuleTypes = map[int]*ModuleType{}
	s.Bays = map[int]*Bay{}
	s.Modules = map[int]*Module{}
	s.DeviceCFs = map[string]bool{"slagworks_id": true, "board_vendor": true, "board_product": true, "chassis_type": true, "board_serial": true}
	s.DeviceTypeCFs = map[string]bool{"slagworks_kernel_params": true}
	s.ModuleCFs = map[string]bool{"firmware_version": true}
	s.FaultAt, s.FaultStatus, s.FaultApplied = 0, 0, false
	s.FaultsHit = 0
	s.Writes, s.Rejected, s.Requests = nil, nil, 0
	for _, p := range builtinProfiles() {
		s.next++
		p.ID = s.next
		s.Profiles[p.ID] = p
	}
}

func builtinProfiles() []*Profile {
	return []*Profile{
		{Name: "CPU", Props: map[string]PropSchema{"architecture": {Type: "string"}, "cores": {Type: "integer"}, "speed": {Type: "number"}}},
		{Name: "Expansion card", Props: map[string]PropSchema{"bandwidth": {Type: "integer"}, "connector_type": {Type: "string"}}},
		{Name: "Fan", Props: map[string]PropSchema{"rpm": {Type: "integer"}}},
		{Name: "GPU", Props: map[string]PropSchema{"gpu": {Type: "string"}, "interface": {Type: "string", Enum: []string{"PCIe 4.0", "PCIe 4.0 x8", "PCIe 4.0 x16", "PCIe 5.0 x16"}}, "memory": {Type: "integer"}}, Required: []string{"memory"}},
		{Name: "Hard disk", Props: map[string]PropSchema{"size": {Type: "integer"}, "speed": {Type: "integer"}, "type": {Type: "string", Enum: []string{"HD", "SSD", "NVME"}}}, Required: []string{"size"}},
		{Name: "Memory", Props: map[string]PropSchema{"class": {Type: "string", Enum: []string{"DDR3", "DDR4", "DDR5"}}, "data_rate": {Type: "integer"}, "ecc": {Type: "boolean"}, "size": {Type: "integer"}}, Required: []string{"class", "size"}},
		{Name: "Power supply", Props: map[string]PropSchema{"hot_swappable": {Type: "boolean"}, "input_current": {Type: "string", Enum: []string{"AC", "DC"}}, "input_voltage": {Type: "integer"}, "wattage": {Type: "integer"}}, Required: []string{"input_current", "input_voltage"}},
	}
}

// Start begins serving.
func (s *Server) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/devices/", s.wrap(s.handleDevices))
	mux.HandleFunc("/api/dcim/device-types/", s.wrap(s.handleDeviceTypes))
	mux.HandleFunc("/api/dcim/interfaces/", s.wrap(s.handleIfaces))
	mux.HandleFunc("/api/dcim/mac-addresses/", s.wrap(s.handleMACs))
	mux.HandleFunc("/api/dcim/cables/", s.wrap(s.handleCables))
	mux.HandleFunc("/api/dcim/manufacturers/", s.wrap(s.handleManufacturers))
	mux.HandleFunc("/api/dcim/module-type-profiles/", s.wrap(s.handleProfiles))
	mux.HandleFunc("/api/dcim/module-types/", s.wrap(s.handleModuleTypes))
	mux.HandleFunc("/api/dcim/module-bays/", s.wrap(s.handleBays))
	mux.HandleFunc("/api/dcim/modules/", s.wrap(s.handleModules))
	s.srv = httptest.NewServer(mux)
}

// Close stops serving.
func (s *Server) Close() { s.srv.Close() }

// URL is the base URL a netbox.Client should use.
func (s *Server) URL() string { return s.srv.URL }

// Lock and Unlock let a test read or seed the maps directly.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// NextID allocates an id. Caller holds the lock.
func (s *Server) NextID() int { s.next++; return s.next }

// ---- seeding helpers (take the lock) ----

// AddDevice seeds a device and returns its id.
func (s *Server) AddDevice(d Device) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	d.ID = s.NextID()
	if d.CustomFields == nil {
		d.CustomFields = map[string]any{}
	}
	if d.Status == "" {
		d.Status = "active"
	}
	for _, t := range d.Tags {
		s.TagSlugs[t] = true
	}
	cp := d
	s.Devices[d.ID] = &cp
	return d.ID
}

// AddDeviceType seeds a device type.
func (s *Server) AddDeviceType(model string, cf map[string]any) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.NextID()
	if cf == nil {
		cf = map[string]any{}
	}
	s.DeviceTypes[id] = &DeviceType{ID: id, Model: model, CustomFields: cf}
	return id
}

// AddIface seeds an interface (and a primary MAC object when mac != "").
func (s *Server) AddIface(dev int, name, typ string, mgmt bool, mac string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.NextID()
	s.Ifaces[id] = &Iface{ID: id, Device: dev, Name: name, Type: typ, MgmtOnly: mgmt, Enabled: true}
	if mac != "" {
		mid := s.NextID()
		s.MACs[mid] = &MAC{ID: mid, MAC: strings.ToUpper(mac), IfaceID: id}
		s.Ifaces[id].PrimaryMAC = mid
	}
	return id
}

// AddCable seeds a cable between two interfaces.
func (s *Server) AddCable(a, b int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.NextID()
	s.Cables[id] = &Cable{ID: id, A: a, B: b}
	s.Ifaces[a].Cable = id
	s.Ifaces[b].Cable = id
	return id
}

// ---- request plumbing ----

type handler func(w http.ResponseWriter, r *http.Request, rest string, body map[string]any)

func (s *Server) wrap(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.Requests++
		var body map[string]any
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				s.reject(w, "malformed JSON body")
				return
			}
		}
		if r.Method != http.MethodGet {
			armed := s.FaultAt > 0
			if armed {
				s.FaultAt--
			}
			if armed && s.FaultAt == 0 {
				s.FaultsHit++
				if !s.FaultApplied {
					writeJSON(w, s.FaultStatus, map[string]any{"detail": "injected fault"})
					return
				}
				rec := httptest.NewRecorder()
				h(rec, r, restOf(r.URL.Path), body)
				writeJSON(w, s.FaultStatus, map[string]any{"detail": "injected fault after commit"})
				return
			}
		}
		h(w, r, restOf(r.URL.Path), body)
	}
}

// restOf returns the path segment after /api/dcim/<resource>/.
func restOf(p string) string {
	p = strings.TrimPrefix(p, "/api/dcim/")
	_, rest, _ := strings.Cut(p, "/")
	return strings.TrimSuffix(rest, "/")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// integrityPrefix marks a rejection real NetBox answers as a 500
// IntegrityError (a database constraint its validators do not check)
// rather than a 400.
const integrityPrefix = "IntegrityError: "

func (s *Server) reject(w http.ResponseWriter, msg string) {
	s.Rejected = append(s.Rejected, msg)
	if rest, ok := strings.CutPrefix(msg, integrityPrefix); ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": rest, "exception": "IntegrityError"})
		return
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"__all__": []string{msg}})
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"detail": "No object matches the given query."})
}

func (s *Server) logWrite(format string, args ...any) {
	s.Writes = append(s.Writes, fmt.Sprintf(format, args...))
}

func (s *Server) paginate(w http.ResponseWriter, r *http.Request, ids []int, render func(int) any) {
	sort.Ints(ids)
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = s.PageSize
	}
	if s.MaxPageSize > 0 && limit > s.MaxPageSize {
		// NetBox clamps limit to MAX_PAGE_SIZE.
		limit = s.MaxPageSize
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

func intsOf(vals []string) map[int]bool {
	out := map[int]bool{}
	for _, v := range vals {
		n, _ := strconv.Atoi(v)
		out[n] = true
	}
	return out
}

func ref(id int, name string) any {
	if id == 0 {
		return nil
	}
	return map[string]any{"id": id, "name": name}
}

// ---- devices ----

var deviceStatuses = map[string]bool{"offline": true, "active": true, "planned": true, "staged": true, "failed": true, "inventory": true, "decommissioning": true}

func (s *Server) renderDevice(d *Device) map[string]any {
	tags := []any{}
	for _, t := range d.Tags {
		tags = append(tags, map[string]any{"slug": t, "name": t})
	}
	cf := map[string]any{}
	for k := range s.DeviceCFs {
		cf[k] = d.CustomFields[k]
	}
	var at any
	if d.AssetTag != nil {
		at = *d.AssetTag
	}
	out := map[string]any{
		"id":            d.ID,
		"name":          d.Name,
		"serial":        d.Serial,
		"asset_tag":     at,
		"status":        map[string]any{"value": d.Status, "label": d.Status},
		"site":          map[string]any{"id": d.SiteID, "name": fmt.Sprintf("site-%d", d.SiteID), "slug": fmt.Sprintf("site-%d", d.SiteID)},
		"device_type":   ref(d.DeviceTypeID, ""),
		"role":          map[string]any{"id": 1, "name": d.RoleSlug, "slug": d.RoleSlug},
		"tags":          tags,
		"custom_fields": cf,
	}
	return out
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	switch r.Method {
	case http.MethodGet:
		if rest != "" {
			d, ok := s.Devices[intOf(rest)]
			if !ok {
				notFound(w)
				return
			}
			writeJSON(w, http.StatusOK, s.renderDevice(d))
			return
		}
		q := r.URL.Query()
		var ids []int
		for id, d := range s.Devices {
			if v := q.Get("name"); v != "" && d.Name != v {
				continue
			}
			if v := q.Get("name__isw"); v != "" && !strings.HasPrefix(strings.ToLower(d.Name), strings.ToLower(v)) {
				continue
			}
			if v := q.Get("site_id"); v != "" && strconv.Itoa(d.SiteID) != v {
				continue
			}
			if v := q.Get("serial"); v != "" && !strings.EqualFold(d.Serial, v) {
				continue
			}
			if v := q.Get("cf_slagworks_id"); v != "" && d.CustomFields["slagworks_id"] != v {
				continue
			}
			ids = append(ids, id)
		}
		s.paginate(w, r, ids, func(id int) any { return s.renderDevice(s.Devices[id]) })
	case http.MethodPatch:
		d, ok := s.Devices[intOf(rest)]
		if !ok {
			notFound(w)
			return
		}
		next := *d
		next.CustomFields = map[string]any{}
		for k, v := range d.CustomFields {
			next.CustomFields[k] = v
		}
		for k, v := range body {
			switch k {
			case "serial":
				sv, _ := v.(string)
				if v == nil {
					sv = ""
				}
				if len(sv) > 50 {
					s.reject(w, "serial: Ensure this field has no more than 50 characters.")
					return
				}
				next.Serial = sv
			case "asset_tag":
				if v == nil {
					next.AssetTag = nil
					continue
				}
				sv, _ := v.(string)
				if len(sv) > 50 {
					s.reject(w, "asset_tag: Ensure this field has no more than 50 characters.")
					return
				}
				for _, o := range s.Devices {
					if o.ID != d.ID && o.AssetTag != nil && *o.AssetTag == sv {
						s.reject(w, "asset_tag: Device with this Asset tag already exists.")
						return
					}
				}
				next.AssetTag = &sv
			case "name":
				sv, _ := v.(string)
				if len(sv) > 64 {
					s.reject(w, "name: Ensure this field has no more than 64 characters.")
					return
				}
				for _, o := range s.Devices {
					if o.ID != d.ID && o.SiteID == d.SiteID && sv != "" && strings.EqualFold(o.Name, sv) {
						s.reject(w, "Device name must be unique per site.")
						return
					}
				}
				next.Name = sv
			case "status":
				sv, _ := v.(string)
				if !deviceStatuses[sv] {
					s.reject(w, fmt.Sprintf("status: %q is not a valid choice.", sv))
					return
				}
				next.Status = sv
			case "device_type":
				id := intOf(v)
				if _, ok := s.DeviceTypes[id]; !ok {
					s.reject(w, "device_type: Invalid pk - object does not exist.")
					return
				}
				next.DeviceTypeID = id
			case "tags":
				list, _ := v.([]any)
				var slugs []string
				for _, t := range list {
					m, _ := t.(map[string]any)
					slug, _ := m["slug"].(string)
					if !s.TagSlugs[slug] {
						s.reject(w, fmt.Sprintf("tags: Related object not found using the provided attributes: {'slug': '%s'}", slug))
						return
					}
					slugs = append(slugs, slug)
				}
				next.Tags = slugs
			case "custom_fields":
				m, _ := v.(map[string]any)
				for ck, cv := range m {
					if !s.DeviceCFs[ck] {
						s.reject(w, fmt.Sprintf("custom_fields: Unknown field name '%s' in custom field data.", ck))
						return
					}
					next.CustomFields[ck] = cv
				}
			default:
				// Fields this fake does not model are accepted and ignored.
			}
		}
		*d = next
		s.logWrite("PATCH device %d %v", d.ID, sortedKeys(body))
		writeJSON(w, http.StatusOK, s.renderDevice(d))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

// ---- device types ----

func (s *Server) renderDeviceType(dt *DeviceType) map[string]any {
	cf := map[string]any{}
	for k := range s.DeviceTypeCFs {
		cf[k] = dt.CustomFields[k]
	}
	return map[string]any{"id": dt.ID, "model": dt.Model, "slug": strings.ToLower(dt.Model), "custom_fields": cf}
}

func (s *Server) handleDeviceTypes(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	dt, ok := s.DeviceTypes[intOf(rest)]
	switch r.Method {
	case http.MethodGet:
		if !ok {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, s.renderDeviceType(dt))
	case http.MethodPatch:
		if !ok {
			notFound(w)
			return
		}
		m, _ := body["custom_fields"].(map[string]any)
		for ck := range m {
			if !s.DeviceTypeCFs[ck] {
				s.reject(w, fmt.Sprintf("custom_fields: Unknown field name '%s' in custom field data.", ck))
				return
			}
		}
		for ck, cv := range m {
			dt.CustomFields[ck] = cv
		}
		s.logWrite("PATCH device-type %d", dt.ID)
		writeJSON(w, http.StatusOK, s.renderDeviceType(dt))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

// ---- interfaces ----

var ifaceTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, t := range InterfaceTypes {
		m[t] = true
	}
	return m
}()

func (s *Server) primaryMAC(i *Iface) string {
	if m, ok := s.MACs[i.PrimaryMAC]; ok {
		return m.MAC
	}
	return ""
}

func (s *Server) renderIface(i *Iface) map[string]any {
	out := map[string]any{
		"id":          i.ID,
		"device":      map[string]any{"id": i.Device, "name": s.deviceName(i.Device)},
		"name":        i.Name,
		"type":        map[string]any{"value": i.Type, "label": i.Type},
		"enabled":     i.Enabled,
		"mgmt_only":   i.MgmtOnly,
		"description": i.Description,
		"speed":       nil,
		"lag":         ref(i.LAG, ""),
		"cable":       ref(i.Cable, ""),
	}
	if i.Cable != 0 {
		out["cable"] = map[string]any{"id": i.Cable}
	}
	if i.Speed != nil {
		out["speed"] = *i.Speed
	}
	var ids []int
	for id, m := range s.MACs {
		if m.IfaceID == i.ID {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	all := []any{}
	for _, id := range ids {
		all = append(all, map[string]any{"id": id, "mac_address": s.MACs[id].MAC})
	}
	out["mac_addresses"] = all
	if pm := s.primaryMAC(i); pm != "" {
		out["mac_address"] = pm
		out["primary_mac_address"] = map[string]any{"id": i.PrimaryMAC, "mac_address": pm}
	} else {
		out["mac_address"] = nil
		out["primary_mac_address"] = nil
	}
	return out
}

func (s *Server) deviceName(id int) string {
	if d, ok := s.Devices[id]; ok {
		return d.Name
	}
	return ""
}

func (s *Server) ifaceNameTaken(dev int, name string, except int) bool {
	for _, i := range s.Ifaces {
		if i.Device == dev && i.Name == name && i.ID != except {
			return true
		}
	}
	return false
}

// applyIfaceBody validates and applies an interface write; returns a
// rejection message or "".
func (s *Server) applyIfaceBody(i *Iface, body map[string]any) string {
	if v, ok := body["name"]; ok {
		n, _ := v.(string)
		if n == "" {
			return "name: This field may not be blank."
		}
		if len(n) > 64 {
			return "name: Ensure this field has no more than 64 characters."
		}
		if s.ifaceNameTaken(i.Device, n, i.ID) {
			return fmt.Sprintf("Interface with this Device and Name already exists: %s", n)
		}
	}
	if v, ok := body["type"]; ok {
		t, _ := v.(string)
		if !ifaceTypes[t] {
			return fmt.Sprintf("type: %q is not a valid choice.", t)
		}
	}
	if v, ok := body["description"]; ok {
		d, _ := v.(string)
		if len(d) > 200 {
			return "description: Ensure this field has no more than 200 characters."
		}
	}
	if v, ok := body["speed"]; ok && v != nil {
		f, isNum := v.(float64)
		if !isNum || f < 0 || f != float64(int64(f)) {
			return "speed: A valid integer is required."
		}
	}
	if v, ok := body["primary_mac_address"]; ok && v != nil {
		m, exists := s.MACs[intOf(v)]
		if !exists || m.IfaceID != i.ID {
			return "primary_mac_address: The specified MAC address is not assigned to this interface."
		}
	}
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
	if v, ok := body["primary_mac_address"]; ok {
		i.PrimaryMAC = intOf(v)
	}
	return ""
}

func (s *Server) deleteIface(id int) {
	i := s.Ifaces[id]
	if i.Cable != 0 {
		s.deleteCable(i.Cable)
	}
	for mid, m := range s.MACs {
		if m.IfaceID == id {
			delete(s.MACs, mid)
		}
	}
	for _, o := range s.Ifaces {
		if o.LAG == id {
			o.LAG = 0
		}
	}
	delete(s.Ifaces, id)
}

func (s *Server) deleteCable(id int) {
	c, ok := s.Cables[id]
	if !ok {
		return
	}
	if a, ok := s.Ifaces[c.A]; ok {
		a.Cable = 0
	}
	if b, ok := s.Ifaces[c.B]; ok {
		b.Cable = 0
	}
	delete(s.Cables, id)
	s.logWrite("cascade-delete cable %d", id)
}

func (s *Server) handleIfaces(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	switch r.Method {
	case http.MethodGet:
		if rest != "" {
			i, ok := s.Ifaces[intOf(rest)]
			if !ok {
				notFound(w)
				return
			}
			writeJSON(w, http.StatusOK, s.renderIface(i))
			return
		}
		q := r.URL.Query()
		devs := intsOf(q["device_id"])
		var ids []int
		for id, i := range s.Ifaces {
			if len(devs) > 0 && !devs[i.Device] {
				continue
			}
			if v := q.Get("name"); v != "" && i.Name != v {
				continue
			}
			if v := q.Get("mac_address"); v != "" {
				found := false
				for _, m := range s.MACs {
					if m.IfaceID == id && strings.EqualFold(m.MAC, v) {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			ids = append(ids, id)
		}
		s.paginate(w, r, ids, func(id int) any { return s.renderIface(s.Ifaces[id]) })
	case http.MethodPost:
		dev := intOf(body["device"])
		if _, ok := s.Devices[dev]; !ok {
			s.reject(w, "device: Invalid pk - object does not exist.")
			return
		}
		if _, ok := body["name"]; !ok {
			s.reject(w, "name: This field is required.")
			return
		}
		if _, ok := body["type"]; !ok {
			s.reject(w, "type: This field is required.")
			return
		}
		i := &Iface{Device: dev, Enabled: true}
		if msg := s.applyIfaceBody(i, body); msg != "" {
			s.reject(w, msg)
			return
		}
		i.ID = s.NextID()
		s.Ifaces[i.ID] = i
		s.logWrite("POST iface %d dev=%d name=%s", i.ID, dev, i.Name)
		writeJSON(w, http.StatusCreated, s.renderIface(i))
	case http.MethodPatch:
		i, ok := s.Ifaces[intOf(rest)]
		if !ok {
			notFound(w)
			return
		}
		next := *i
		if msg := s.applyIfaceBody(&next, body); msg != "" {
			s.reject(w, msg)
			return
		}
		if next.Name != i.Name {
			s.logWrite("RENAME iface %d dev=%d %s->%s", i.ID, i.Device, i.Name, next.Name)
		}
		*i = next
		s.logWrite("PATCH iface %d dev=%d %v", i.ID, i.Device, sortedKeys(body))
		writeJSON(w, http.StatusOK, s.renderIface(i))
	case http.MethodDelete:
		id := intOf(rest)
		i, ok := s.Ifaces[id]
		if !ok {
			notFound(w)
			return
		}
		s.logWrite("DELETE iface %d dev=%d name=%s", id, i.Device, i.Name)
		s.deleteIface(id)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

// ---- MAC addresses ----

var eui48 = regexp.MustCompile(`^(?i)([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$|^(?i)[0-9a-f]{12}$|^(?i)([0-9a-f]{4}\.){2}[0-9a-f]{4}$`)

func (s *Server) handleMACs(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	mac, _ := body["mac_address"].(string)
	if !eui48.MatchString(mac) {
		s.reject(w, fmt.Sprintf("mac_address: Invalid MAC address format: %s", mac))
		return
	}
	if body["assigned_object_type"] != "dcim.interface" {
		s.reject(w, "assigned_object_type must be dcim.interface")
		return
	}
	iid := intOf(body["assigned_object_id"])
	if _, ok := s.Ifaces[iid]; !ok {
		s.reject(w, "assigned_object_id: Related object not found.")
		return
	}
	norm := strings.ToUpper(strings.NewReplacer("-", ":", ".", "").Replace(mac))
	if len(norm) == 12 {
		var b strings.Builder
		for k := 0; k < 12; k += 2 {
			if k > 0 {
				b.WriteByte(':')
			}
			b.WriteString(norm[k : k+2])
		}
		norm = b.String()
	}
	m := &MAC{ID: s.NextID(), MAC: norm, IfaceID: iid}
	s.MACs[m.ID] = m
	s.logWrite("POST mac %d %s -> iface %d", m.ID, m.MAC, iid)
	writeJSON(w, http.StatusCreated, map[string]any{"id": m.ID, "mac_address": m.MAC})
}

// ---- cables ----

func (s *Server) handleCables(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	term := func(key string) (*Iface, string) {
		list, _ := body[key].([]any)
		if len(list) != 1 {
			return nil, key + ": exactly one termination expected"
		}
		m, _ := list[0].(map[string]any)
		if m["object_type"] != "dcim.interface" {
			return nil, key + ": unsupported termination type"
		}
		i, ok := s.Ifaces[intOf(m["object_id"])]
		if !ok {
			return nil, key + ": termination object does not exist"
		}
		if i.Cable != 0 {
			return nil, fmt.Sprintf("Duplicate termination found for dcim.interface:%d: already cabled (cable %d)", i.ID, i.Cable)
		}
		if i.Type == "virtual" || i.Type == "lag" || i.Type == "bridge" {
			return nil, fmt.Sprintf("Cables cannot be terminated to %s interfaces", i.Type)
		}
		return i, ""
	}
	a, msg := term("a_terminations")
	if msg != "" {
		s.reject(w, msg)
		return
	}
	b, msg := term("b_terminations")
	if msg != "" {
		s.reject(w, msg)
		return
	}
	if a.ID == b.ID {
		s.reject(w, "A and B terminations cannot connect to the same object.")
		return
	}
	c := &Cable{ID: s.NextID(), A: a.ID, B: b.ID}
	s.Cables[c.ID] = c
	a.Cable, b.Cable = c.ID, c.ID
	s.logWrite("POST cable %d %d<->%d", c.ID, a.ID, b.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID,
		"a_terminations": []any{map[string]any{"object_type": "dcim.interface", "object_id": a.ID}},
		"b_terminations": []any{map[string]any{"object_type": "dcim.interface", "object_id": b.ID}},
		"status":         map[string]any{"value": "connected"}})
}

// ---- manufacturers ----

var slugRE = regexp.MustCompile(`^[-a-zA-Z0-9_]+$`)

func (s *Server) handleManufacturers(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	render := func(m *Manufacturer) any { return map[string]any{"id": m.ID, "name": m.Name, "slug": m.Slug} }
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		var ids []int
		for id, m := range s.Manufacturers {
			if v := q.Get("name"); v != "" && m.Name != v {
				continue
			}
			if v := q.Get("slug"); v != "" && m.Slug != v {
				continue
			}
			ids = append(ids, id)
		}
		s.paginate(w, r, ids, func(id int) any { return render(s.Manufacturers[id]) })
	case http.MethodPost:
		name, _ := body["name"].(string)
		slug, _ := body["slug"].(string)
		if name == "" || len(name) > 100 {
			s.reject(w, "name: must be 1-100 characters")
			return
		}
		if slug == "" || len(slug) > 100 || !slugRE.MatchString(slug) {
			s.reject(w, "slug: Enter a valid slug")
			return
		}
		for _, m := range s.Manufacturers {
			if strings.EqualFold(m.Name, name) {
				s.reject(w, "name: manufacturer with this name already exists.")
				return
			}
			if m.Slug == slug {
				s.reject(w, "slug: manufacturer with this slug already exists.")
				return
			}
		}
		m := &Manufacturer{ID: s.NextID(), Name: name, Slug: slug}
		s.Manufacturers[m.ID] = m
		s.logWrite("POST manufacturer %d %q", m.ID, name)
		writeJSON(w, http.StatusCreated, render(m))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

// ---- module type profiles ----

func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	q := r.URL.Query()
	var ids []int
	for id, p := range s.Profiles {
		if v := q.Get("name"); v != "" && p.Name != v {
			continue
		}
		ids = append(ids, id)
	}
	s.paginate(w, r, ids, func(id int) any { return map[string]any{"id": id, "name": s.Profiles[id].Name} })
}

// ---- module types ----

func validateAttrs(p *Profile, data map[string]any) string {
	if data == nil {
		// jsonschema.validate(None, schema) passes: properties/required
		// apply only to objects.
		return ""
	}
	for _, k := range p.Required {
		if _, ok := data[k]; !ok {
			return fmt.Sprintf("Invalid schema: '%s' is a required property", k)
		}
	}
	for k, v := range data {
		ps, ok := p.Props[k]
		if !ok {
			continue // no additionalProperties:false in the built-in schemas
		}
		switch ps.Type {
		case "integer":
			f, isNum := v.(float64)
			if !isNum || f != float64(int64(f)) {
				return fmt.Sprintf("Invalid schema: %v is not of type 'integer'", v)
			}
		case "number":
			if _, isNum := v.(float64); !isNum {
				return fmt.Sprintf("Invalid schema: %v is not of type 'number'", v)
			}
		case "string":
			sv, isStr := v.(string)
			if !isStr {
				return fmt.Sprintf("Invalid schema: %v is not of type 'string'", v)
			}
			if ps.Enum != nil {
				found := false
				for _, e := range ps.Enum {
					if e == sv {
						found = true
					}
				}
				if !found {
					return fmt.Sprintf("Invalid schema: '%s' is not one of %v", sv, ps.Enum)
				}
			}
		case "boolean":
			if _, isB := v.(bool); !isB {
				return fmt.Sprintf("Invalid schema: %v is not of type 'boolean'", v)
			}
		}
	}
	return ""
}

func (s *Server) renderModuleType(mt *ModuleType) any {
	out := map[string]any{
		"id":             mt.ID,
		"manufacturer":   ref(mt.Manufacturer, ""),
		"model":          mt.Model,
		"profile":        ref(mt.Profile, ""),
		"part_number":    mt.PartNumber,
		"attribute_data": mt.AttributeData,
	}
	return out
}

func (s *Server) handleModuleTypes(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		mfgs := intsOf(q["manufacturer_id"])
		var ids []int
		for id, mt := range s.ModuleTypes {
			if len(mfgs) > 0 && !mfgs[mt.Manufacturer] {
				continue
			}
			if v := q.Get("model"); v != "" && mt.Model != v {
				continue
			}
			ids = append(ids, id)
		}
		s.paginate(w, r, ids, func(id int) any { return s.renderModuleType(s.ModuleTypes[id]) })
	case http.MethodPost:
		mfg := intOf(body["manufacturer"])
		if _, ok := s.Manufacturers[mfg]; !ok {
			s.reject(w, "manufacturer: Invalid pk - object does not exist.")
			return
		}
		model, _ := body["model"].(string)
		if strings.TrimSpace(model) == "" {
			s.reject(w, "model: This field may not be blank.")
			return
		}
		if len(model) > 100 {
			s.reject(w, "model: Ensure this field has no more than 100 characters.")
			return
		}
		pn, _ := body["part_number"].(string)
		if len(pn) > 50 {
			s.reject(w, "part_number: Ensure this field has no more than 50 characters.")
			return
		}
		for _, mt := range s.ModuleTypes {
			if mt.Manufacturer == mfg && mt.Model == model {
				s.reject(w, "Module type with this Manufacturer and Model already exists.")
				return
			}
		}
		var attrs map[string]any
		if v, ok := body["attribute_data"]; ok && v != nil {
			attrs, _ = v.(map[string]any)
		}
		prof := intOf(body["profile"])
		if prof != 0 {
			p, ok := s.Profiles[prof]
			if !ok {
				s.reject(w, "profile: Invalid pk - object does not exist.")
				return
			}
			if s.ValidateAttributeData {
				if msg := validateAttrs(p, attrs); msg != "" {
					s.reject(w, msg)
					return
				}
			}
		} else {
			attrs = nil
		}
		mt := &ModuleType{ID: s.NextID(), Manufacturer: mfg, Model: model, Profile: prof, PartNumber: pn, AttributeData: attrs}
		s.ModuleTypes[mt.ID] = mt
		s.logWrite("POST module-type %d %q", mt.ID, model)
		writeJSON(w, http.StatusCreated, s.renderModuleType(mt))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

// ---- module bays ----

func (s *Server) handleBays(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	render := func(b *Bay) any {
		var installed any
		for _, m := range s.Modules {
			if m.Bay == b.ID {
				installed = map[string]any{"id": m.ID}
			}
		}
		return map[string]any{"id": b.ID, "name": b.Name, "position": b.Position,
			"device": map[string]any{"id": b.Device, "name": s.deviceName(b.Device)}, "installed_module": installed}
	}
	switch r.Method {
	case http.MethodGet:
		devs := intsOf(r.URL.Query()["device_id"])
		var ids []int
		for id, b := range s.Bays {
			if len(devs) > 0 && !devs[b.Device] {
				continue
			}
			ids = append(ids, id)
		}
		s.paginate(w, r, ids, func(id int) any { return render(s.Bays[id]) })
	case http.MethodPost:
		dev := intOf(body["device"])
		if _, ok := s.Devices[dev]; !ok {
			s.reject(w, "device: Invalid pk - object does not exist.")
			return
		}
		name, _ := body["name"].(string)
		pos, _ := body["position"].(string)
		if name == "" || len(name) > 64 {
			s.reject(w, "name: must be 1-64 characters")
			return
		}
		if len(pos) > 30 {
			s.reject(w, "position: Ensure this field has no more than 30 characters.")
			return
		}
		b := &Bay{ID: s.NextID(), Device: dev, Name: name, Position: pos}
		s.Bays[b.ID] = b
		s.logWrite("POST bay %d dev=%d name=%q", b.ID, dev, name)
		writeJSON(w, http.StatusCreated, render(b))
	case http.MethodDelete:
		id := intOf(rest)
		b, ok := s.Bays[id]
		if !ok {
			notFound(w)
			return
		}
		for mid, m := range s.Modules {
			if m.Bay == id {
				delete(s.Modules, mid)
				s.logWrite("cascade-delete module %d", mid)
			}
		}
		delete(s.Bays, id)
		s.logWrite("DELETE bay %d dev=%d name=%q", id, b.Device, b.Name)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

// ---- modules ----

var moduleStatuses = map[string]bool{"offline": true, "active": true, "planned": true, "staged": true, "failed": true, "decommissioning": true}

func (s *Server) renderModule(m *Module) any {
	cf := map[string]any{}
	for k := range s.ModuleCFs {
		cf[k] = m.CustomFields[k]
	}
	return map[string]any{"id": m.ID, "device": ref(m.Device, s.deviceName(m.Device)),
		"module_bay": ref(m.Bay, ""), "module_type": ref(m.ModuleType, ""), "serial": m.Serial,
		"status": map[string]any{"value": m.Status, "label": m.Status}, "custom_fields": cf}
}

func (s *Server) applyModuleBody(m *Module, body map[string]any, create bool) string {
	if v, ok := body["device"]; ok {
		m.Device = intOf(v)
	}
	if _, ok := s.Devices[m.Device]; !ok {
		return "device: Invalid pk - object does not exist."
	}
	if v, ok := body["module_bay"]; ok {
		bid := intOf(v)
		b, exists := s.Bays[bid]
		if !exists {
			return "module_bay: Invalid pk - object does not exist."
		}
		if b.Device != m.Device {
			return fmt.Sprintf("Module must be installed within a module bay belonging to the assigned device (%d).", m.Device)
		}
		for _, o := range s.Modules {
			if o.Bay == bid && o.ID != m.ID {
				// Real NetBox (4.5) surfaces this one as a 500
				// IntegrityError on dcim_module_module_bay_id_key, not a
				// validation 400; see handleModules.
				return integrityPrefix + "duplicate key value violates unique constraint \"dcim_module_module_bay_id_key\""
			}
		}
		m.Bay = bid
	} else if create {
		return "module_bay: This field is required."
	}
	if v, ok := body["module_type"]; ok {
		tid := intOf(v)
		if _, exists := s.ModuleTypes[tid]; !exists {
			return "module_type: Invalid pk - object does not exist."
		}
		m.ModuleType = tid
	} else if create {
		return "module_type: This field is required."
	}
	if v, ok := body["status"]; ok {
		st, _ := v.(string)
		if !moduleStatuses[st] {
			return fmt.Sprintf("status: %q is not a valid choice.", st)
		}
		m.Status = st
	}
	if v, ok := body["serial"]; ok {
		sv, _ := v.(string)
		if len(sv) > 50 {
			return "serial: Ensure this field has no more than 50 characters."
		}
		m.Serial = sv
	}
	if v, ok := body["custom_fields"]; ok {
		cm, _ := v.(map[string]any)
		for ck, cv := range cm {
			if !s.ModuleCFs[ck] {
				return fmt.Sprintf("custom_fields: Unknown field name '%s' in custom field data.", ck)
			}
			m.CustomFields[ck] = cv
		}
	}
	return ""
}

func (s *Server) handleModules(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	switch r.Method {
	case http.MethodGet:
		devs := intsOf(r.URL.Query()["device_id"])
		var ids []int
		for id, m := range s.Modules {
			if len(devs) > 0 && !devs[m.Device] {
				continue
			}
			ids = append(ids, id)
		}
		s.paginate(w, r, ids, func(id int) any { return s.renderModule(s.Modules[id]) })
	case http.MethodPost:
		m := &Module{Status: "active", CustomFields: map[string]any{}}
		if msg := s.applyModuleBody(m, body, true); msg != "" {
			s.reject(w, msg)
			return
		}
		m.ID = s.NextID()
		s.Modules[m.ID] = m
		s.logWrite("POST module %d dev=%d bay=%d", m.ID, m.Device, m.Bay)
		writeJSON(w, http.StatusCreated, s.renderModule(m))
	case http.MethodPatch:
		cur, ok := s.Modules[intOf(rest)]
		if !ok {
			notFound(w)
			return
		}
		next := *cur
		next.CustomFields = map[string]any{}
		for k, v := range cur.CustomFields {
			next.CustomFields[k] = v
		}
		if msg := s.applyModuleBody(&next, body, false); msg != "" {
			s.reject(w, msg)
			return
		}
		*cur = next
		s.logWrite("PATCH module %d dev=%d %v", cur.ID, cur.Device, sortedKeys(body))
		writeJSON(w, http.StatusOK, s.renderModule(cur))
	case http.MethodDelete:
		id := intOf(rest)
		m, ok := s.Modules[id]
		if !ok {
			notFound(w)
			return
		}
		delete(s.Modules, id)
		s.logWrite("DELETE module %d dev=%d", id, m.Device)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil)
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
