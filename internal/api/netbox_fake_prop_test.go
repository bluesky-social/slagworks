package api

// propNetBox is an in-memory NetBox for the property harness: the endpoints
// the API server reads, with NetBox's filter semantics for the query
// parameters the client sends. It is mutable between requests so a history
// can re-cable an outlet or rename a device mid-run.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/netbox"
)

type propNetBox struct {
	mu sync.Mutex
	// devices is in NetBox's response order.
	devices    []netbox.Device
	interfaces []netbox.Interface
	ips        []netbox.IPAddress
	outlets    map[int][]netbox.PowerOutlet // PDU device id -> outlets
	ports      map[int][]netbox.PowerPort   // device id -> power-ports
	sites      []netbox.Site
	// requests counts the requests served, by path.
	requests map[string]int
}

func newPropNetBox() *propNetBox {
	return &propNetBox{
		outlets:  map[int][]netbox.PowerOutlet{},
		ports:    map[int][]netbox.PowerPort{},
		requests: map[string]int{},
	}
}

func (f *propNetBox) client(t *testing.T) *netbox.Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return netbox.NewClient(netbox.ClientConfig{BaseURL: srv.URL, Token: "netbox-api-token"})
}

func (f *propNetBox) device(id int) *netbox.Device {
	for i := range f.devices {
		if f.devices[i].ID == id {
			return &f.devices[i]
		}
	}
	return nil
}

func (f *propNetBox) list(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": v})
}

func (f *propNetBox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests[r.URL.Path]++
	q := r.URL.Query()
	detail := func(prefix string) (int, bool) {
		rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/")
		if rest == "" {
			return 0, false
		}
		id, err := strconv.Atoi(rest)
		if err != nil {
			return -1, true
		}
		return id, true
	}
	in := func(key string, id int) bool { // repeated-parameter OR filter
		vals, ok := q[key]
		if !ok {
			return true
		}
		for _, v := range vals {
			if v == strconv.Itoa(id) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/dcim/devices/"):
		if id, ok := detail("/api/dcim/devices/"); ok {
			if d := f.device(id); d != nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(d)
				return
			}
			http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
			return
		}
		out := []netbox.Device{}
		for _, d := range f.devices {
			if n, ok := q["name"]; ok && d.Name != n[0] {
				continue
			}
			if n, ok := q["name__ic"]; ok && !strings.Contains(strings.ToLower(d.Name), strings.ToLower(n[0])) {
				continue
			}
			if s, ok := q["serial"]; ok && d.Serial != s[0] {
				continue
			}
			if s, ok := q["site"]; ok && (d.Site == nil || d.Site.Slug != s[0]) {
				continue
			}
			if _, ok := q["cf_slagworks_id"]; ok {
				continue // no device carries the custom field in this fake
			}
			if !in("id", d.ID) {
				continue
			}
			out = append(out, d)
		}
		f.list(w, out)
	case strings.HasPrefix(r.URL.Path, "/api/dcim/interfaces/"):
		out := []netbox.Interface{}
		for _, i := range f.interfaces {
			if i.Device != nil && !in("device_id", i.Device.ID) {
				continue
			}
			out = append(out, i)
		}
		f.list(w, out)
	case strings.HasPrefix(r.URL.Path, "/api/ipam/ip-addresses/"):
		if id, ok := detail("/api/ipam/ip-addresses/"); ok {
			for _, ip := range f.ips {
				if ip.ID == id {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(ip)
					return
				}
			}
			http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
			return
		}
		out := []netbox.IPAddress{}
		for _, ip := range f.ips {
			if ip.AssignedObjectID != nil && !in("assigned_object_id", *ip.AssignedObjectID) {
				continue
			}
			out = append(out, ip)
		}
		f.list(w, out)
	case strings.HasPrefix(r.URL.Path, "/api/dcim/power-outlets/"):
		id, _ := strconv.Atoi(q.Get("device_id"))
		out := f.outlets[id]
		if out == nil {
			out = []netbox.PowerOutlet{}
		}
		f.list(w, out)
	case strings.HasPrefix(r.URL.Path, "/api/dcim/power-ports/"):
		id, _ := strconv.Atoi(q.Get("device_id"))
		out := f.ports[id]
		if out == nil {
			out = []netbox.PowerPort{}
		}
		f.list(w, out)
	case strings.HasPrefix(r.URL.Path, "/api/dcim/sites/"):
		out := []netbox.Site{}
		for _, s := range f.sites {
			if slug, ok := q["slug"]; ok && s.Slug != slug[0] {
				continue
			}
			out = append(out, s)
		}
		f.list(w, out)
	default:
		// Every other collection the server may read is empty.
		f.list(w, []any{})
	}
}
