package netbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestListDevicesForSiteFiltersBySlug(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"id": 1, "name": "host-a", "serial": "SN-A", "status": map[string]any{"value": "active"}, "site": map[string]any{"slug": "pop3"}},
				{"id": 2, "name": "host-b", "serial": "SN-B", "status": map[string]any{"value": "inventory"}, "site": map[string]any{"slug": "pop3"}},
			},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	devs, err := c.ListDevicesForSite(context.Background(), "pop3")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "site=pop3") {
		t.Errorf("expected site=pop3 filter, got %q", gotQuery)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2", len(devs))
	}
	if devs[0].Name != "host-a" || devs[0].Serial != "SN-A" {
		t.Fatalf("unexpected device: %+v", devs[0])
	}
}

// TestListInterfacesForDevicesUsesRepeatedDeviceID pins the
// repeated-parameter OR filter (?device_id=1&device_id=2). The
// `device_id__in=1,2` comma form is silently ignored by NetBox 4.5.x
// and must never come back.
func TestListInterfacesForDevicesUsesRepeatedDeviceID(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":   1,
			"results": []map[string]any{{"id": 100, "name": "eno1", "mac_address": "aa:bb:cc:dd:ee:ff", "type": map[string]any{"value": "1000base-t"}, "device": map[string]any{"id": 1}, "enabled": true}},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	ifaces, err := c.ListInterfacesForDevices(context.Background(), []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotQuery["device_id"], []string{"1", "2", "3"}; !slices.Equal(got, want) {
		t.Errorf("device_id params = %v, want %v", got, want)
	}
	if gotQuery.Has("device_id__in") {
		t.Errorf("must NOT use device_id__in (silently ignored), got %q", gotQuery.Encode())
	}
	if len(ifaces) != 1 {
		t.Fatalf("got %d ifaces, want 1", len(ifaces))
	}
}

func TestListInterfacesForDevicesEmptyReturnsNil(t *testing.T) {
	// No HTTP call should be made when ids is empty.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("did not expect HTTP call, got %s", r.URL.String())
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	got, err := c.ListInterfacesForDevices(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

// TestListDevicesByIDsUsesRepeatedID pins the batched device lookup used by
// machine-list hostname enrichment. Keep it as repeated id= params, not an
// N+1 fetch path and not id__in.
func TestListDevicesByIDsUsesRepeatedID(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"id": 10, "name": "cpu1.pop1", "serial": "SN-A"},
				{"id": 20, "name": "cpu2.pop1", "serial": "SN-B"},
			},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	devs, err := c.ListDevicesByIDs(context.Background(), []int{10, 20})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gotQuery["id"], []string{"10", "20"}; !slices.Equal(got, want) {
		t.Errorf("id params = %v, want %v", got, want)
	}
	if gotQuery.Has("id__in") {
		t.Errorf("must NOT use id__in, got %q", gotQuery.Encode())
	}
	if len(devs) != 2 || devs[0].Name != "cpu1.pop1" {
		t.Fatalf("unexpected devices: %+v", devs)
	}
}

// TestListInterfacesForDevicesChunksLargeIDSets pins URL-length
// protection: id sets larger than multiValueChunk split across
// multiple requests, results concatenated.
func TestListInterfacesForDevicesChunksLargeIDSets(t *testing.T) {
	var requests []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Query())
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":   1,
			"results": []map[string]any{{"id": len(requests), "name": "eth0", "device": map[string]any{"id": 1}}},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	ids := make([]int, multiValueChunk+1)
	for i := range ids {
		ids[i] = i + 1
	}
	ifaces, err := c.ListInterfacesForDevices(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(requests))
	}
	if got := len(requests[0]["device_id"]); got != multiValueChunk {
		t.Errorf("first chunk carried %d ids, want %d", got, multiValueChunk)
	}
	if got := len(requests[1]["device_id"]); got != 1 {
		t.Errorf("second chunk carried %d ids, want 1", got)
	}
	if len(ifaces) != 2 {
		t.Fatalf("got %d ifaces, want 2 (one per chunk)", len(ifaces))
	}
}

// TestListIPAddressesForInterfacesUsesRepeatedAssignedObjectID pins the
// repeated-parameter OR filter; same footgun as interfaces.
func TestListIPAddressesForInterfacesUsesRepeatedAssignedObjectID(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []map[string]any{}})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	_, err := c.ListIPAddressesForInterfaces(context.Background(), []int{100, 101})
	if err != nil {
		t.Fatal(err)
	}
	if got := gotQuery.Get("assigned_object_type"); got != "dcim.interface" {
		t.Errorf("assigned_object_type = %q, want dcim.interface", got)
	}
	if got, want := gotQuery["assigned_object_id"], []string{"100", "101"}; !slices.Equal(got, want) {
		t.Errorf("assigned_object_id params = %v, want %v", got, want)
	}
	if gotQuery.Has("assigned_object_id__in") {
		t.Errorf("must NOT use assigned_object_id__in (silently ignored), got %q", gotQuery.Encode())
	}
}

func TestListPowerPortsForDeviceUsesSingleValueDeviceID(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{
					"id": 61, "name": "PSU1",
					"device":                        map[string]any{"id": 580, "name": "leaf-4.pop1"},
					"connected_endpoints_reachable": true,
					"connected_endpoints": []map[string]any{
						{"id": 101, "name": "Outlet 5", "device": map[string]any{"id": 611, "name": "pdu-0102-left"}},
					},
				},
				{
					"id": 62, "name": "PSU2",
					"device":                        map[string]any{"id": 580, "name": "leaf-4.pop1"},
					"connected_endpoints_reachable": true,
					"connected_endpoints": []map[string]any{
						{"id": 125, "name": "Outlet 5", "device": map[string]any{"id": 612, "name": "pdu-0102-right"}},
					},
				},
			},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	ports, err := c.ListPowerPortsForDevice(context.Background(), 580)
	if err != nil {
		t.Fatal(err)
	}
	// Must use the SINGLE-value device_id filter, never device_id__in
	// (the plural is silently ignored on NetBox 4.5.x).
	if !strings.Contains(gotQuery, "device_id=580") {
		t.Errorf("want single-value device_id=580, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "device_id__in") {
		t.Errorf("must NOT use device_id__in (silently ignored on 4.5.x), got %q", gotQuery)
	}
	if len(ports) != 2 {
		t.Fatalf("got %d power-ports, want 2", len(ports))
	}
	if ports[0].Name != "PSU1" || len(ports[0].ConnectedEndpoints) != 1 {
		t.Fatalf("unexpected PSU1: %+v", ports[0])
	}
	if ep := ports[0].ConnectedEndpoints[0]; ep.Name != "Outlet 5" || ep.Device == nil || ep.Device.ID != 611 {
		t.Fatalf("unexpected PSU1 endpoint: %+v", ep)
	}
}

func TestListSitePrefixesFiltersByTag(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []map[string]any{}})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	_, err := c.ListSitePrefixes(context.Background(), "pop3", "slagworks-production")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "site=pop3") || !strings.Contains(gotQuery, "tag=slagworks-production") {
		t.Errorf("want site+tag filters, got %q", gotQuery)
	}
}

// A whole-site config takes its fabric and OOB prefixes from
// ListSitePrefixes, and it owns only the prefixes scoped to the site itself:
// one scoped to a location inside the site belongs to a location-scoped
// config. NetBox's ?site= filter is wider than that. It matches every prefix
// whose scope lies in the site, location-scoped ones included, in address
// order (observed on NetBox 4.6). The fake below answers the same way.
//
// Giving a location its own tagged prefixes is the first step of splitting a
// whole-site site into cages, and it must leave the whole-site config alone.
// When ListSitePrefixes passed the location's rows through, a second
// slagworks-oob prefix failed the whole-site config's snapshot build, and a
// location's slagworks-production prefix that sorted first became the
// site's fabric DHCP subnet and gateway.
func TestListSitePrefixesLeavesOutLocationScopedPrefixes(t *testing.T) {
	inSite := []Prefix{
		{ID: 2, Prefix: "10.0.2.0/24", ScopeType: "dcim.location", ScopeID: 50},
		{ID: 1, Prefix: "10.0.4.0/22", ScopeType: "dcim.site", ScopeID: 1},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		out := []Prefix{}
		if q.Get("site") == "site-a" && q.Get("tag") == "slagworks-production" {
			for _, p := range inSite {
				// NetBox also honors a scope_type filter; model it so the
				// test pins the result, not how the client asks for it.
				if st := q.Get("scope_type"); st == "" || st == p.ScopeType {
					out = append(out, p)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(out), "results": out})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	got, err := c.ListSitePrefixes(context.Background(), "site-a", "slagworks-production")
	if err != nil {
		t.Fatal(err)
	}
	var cidrs []string
	for _, p := range got {
		cidrs = append(cidrs, p.Prefix)
	}
	if !slices.Equal(cidrs, []string{"10.0.4.0/22"}) {
		t.Fatalf("ListSitePrefixes = %v, want only the site-scoped 10.0.4.0/22", cidrs)
	}
}

func TestGetSiteBySlugReturnsNilOnEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []map[string]any{}})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	site, err := c.GetSiteBySlug(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if site != nil {
		t.Fatalf("expected nil, got %+v", site)
	}
}

func TestGetSiteBySlugReturnsSite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count":   1,
			"results": []map[string]any{{"id": 42, "name": "Pop 3", "slug": "pop3"}},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	site, err := c.GetSiteBySlug(context.Background(), "pop3")
	if err != nil {
		t.Fatal(err)
	}
	if site == nil || site.ID != 42 || site.Slug != "pop3" {
		t.Fatalf("unexpected: %+v", site)
	}
}

func TestGetDeviceByIDReturnsNilOn404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail": "Not found."}`))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	dev, err := c.GetDeviceByID(context.Background(), 999)
	if err != nil {
		t.Fatalf("404 should not be an error: %v", err)
	}
	if dev != nil {
		t.Fatalf("expected nil, got %+v", dev)
	}
}

// TestFindDevicesByNameSubstringUsesNameIcFilter pins the case-
// insensitive contains filter shape (`name__ic`). NetBox supports
// this for any string field; if we ever swap to a different filter
// (e.g. `name__isw` for prefix-only), this test should fail loudly
// rather than silently change semantics for slagctl callers.
func TestFindDevicesByNameSubstringUsesNameIcFilter(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{"id": 1, "name": "cpu1.pop1", "site": map[string]any{"slug": "pop1"}},
				{"id": 2, "name": "cpu1.pop3", "site": map[string]any{"slug": "pop3"}},
			},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	devs, err := c.FindDevicesByNameSubstring(context.Background(), "cpu1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "name__ic=cpu1") {
		t.Errorf("expected name__ic=cpu1 filter, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "site_id=") {
		t.Errorf("siteID=0 must not emit a site_id filter, got %q", gotQuery)
	}
	if len(devs) != 2 {
		t.Fatalf("got %d devices, want 2", len(devs))
	}
}

func TestFindDevicesByNameSubstringScopesBySite(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []map[string]any{}})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	if _, err := c.FindDevicesByNameSubstring(context.Background(), "cpu", 7); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "site_id=7") {
		t.Errorf("expected site_id=7 in query, got %q", gotQuery)
	}
}

func TestGetDeviceByIDReturnsDevice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "name": "host-a", "serial": "SN-A", "site": map[string]any{"slug": "pop3"}})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	dev, err := c.GetDeviceByID(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if dev == nil || dev.ID != 42 || dev.Site == nil || dev.Site.Slug != "pop3" {
		t.Fatalf("unexpected: %+v", dev)
	}
}

// TestListPowerOutletsForDeviceUsesSingleValueDeviceID mirrors the power-
// ports test: verifies the single-value device_id= filter, that a cabled
// outlet's connected_endpoints deserializes the downstream power-port and
// its parent device, and that an uncabled outlet deserializes with
// ConnectedEndpointsReachable==false and nil ConnectedEndpoints.
func TestListPowerOutletsForDeviceUsesSingleValueDeviceID(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"results": []map[string]any{
				{
					// Uncabled outlet — cable:null, no connected endpoints.
					"id": 201, "name": "Outlet 1",
					"device":                        map[string]any{"id": 611, "name": "pdu-0101-left"},
					"cable":                         nil,
					"connected_endpoints":           nil,
					"connected_endpoints_reachable": false,
				},
				{
					// Cabled outlet feeding spine-2.pop1 PSU1.
					"id": 203, "name": "Outlet 3",
					"device":                        map[string]any{"id": 611, "name": "pdu-0101-left"},
					"connected_endpoints_reachable": true,
					"connected_endpoints": []map[string]any{
						{
							"id": 61, "name": "PSU1",
							"device": map[string]any{"id": 578, "name": "spine-2.pop1"},
						},
					},
				},
			},
		})
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	outlets, err := c.ListPowerOutletsForDevice(context.Background(), 611)
	if err != nil {
		t.Fatal(err)
	}
	// Must use the SINGLE-value device_id filter, never device_id__in
	// (the plural is silently ignored on NetBox 4.5.x).
	if !strings.Contains(gotQuery, "device_id=611") {
		t.Errorf("want single-value device_id=611, got %q", gotQuery)
	}
	if strings.Contains(gotQuery, "device_id__in") {
		t.Errorf("must NOT use device_id__in (silently ignored on 4.5.x), got %q", gotQuery)
	}
	if len(outlets) != 2 {
		t.Fatalf("got %d outlets, want 2", len(outlets))
	}

	// Uncabled outlet: ConnectedEndpointsReachable false, no endpoints.
	uncabled := outlets[0]
	if uncabled.Name != "Outlet 1" {
		t.Fatalf("unexpected outlet[0].Name: %q", uncabled.Name)
	}
	if uncabled.ConnectedEndpointsReachable {
		t.Errorf("uncabled outlet should have ConnectedEndpointsReachable=false")
	}
	if len(uncabled.ConnectedEndpoints) != 0 {
		t.Errorf("uncabled outlet ConnectedEndpoints should be empty, got %v", uncabled.ConnectedEndpoints)
	}

	// Cabled outlet: endpoint deserializes with downstream port name and device.
	cabled := outlets[1]
	if cabled.Name != "Outlet 3" {
		t.Fatalf("unexpected outlet[1].Name: %q", cabled.Name)
	}
	if !cabled.ConnectedEndpointsReachable {
		t.Errorf("cabled outlet should have ConnectedEndpointsReachable=true")
	}
	if len(cabled.ConnectedEndpoints) != 1 {
		t.Fatalf("expected 1 connected endpoint, got %d", len(cabled.ConnectedEndpoints))
	}
	ep := cabled.ConnectedEndpoints[0]
	if ep.Name != "PSU1" {
		t.Errorf("endpoint Name = %q, want PSU1", ep.Name)
	}
	if ep.Device == nil || ep.Device.ID != 578 || ep.Device.Name != "spine-2.pop1" {
		t.Errorf("endpoint Device = %+v, want {ID:578, Name:spine-2.pop1}", ep.Device)
	}
}

// TestListConsolePortsForDevicesParsesConnectedEndpoint pins the repeated
// device_id filter and the prod (NetBox 4.5) response shape: speed as a
// {value,label} choice, the console-server far end in connected_endpoints.
func TestListConsolePortsForDevicesParsesConnectedEndpoint(t *testing.T) {
	var gotQuery url.Values
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		_, _ = w.Write([]byte(`{"count":2,"next":null,"results":[
			{"id":1,"name":"console","device":{"id":577,"name":"spine-1.pop1"},
			 "speed":{"label":"115.2 kbps","value":115200},
			 "connected_endpoints_type":"dcim.consoleserverport",
			 "connected_endpoints_reachable":true,
			 "connected_endpoints":[{"id":1,"name":"ttyS4","device":{"id":583,"name":"oob-gw-1.pop1"}}]},
			{"id":8,"name":"console","device":{"id":584,"name":"leaf-6.pop1"},
			 "speed":null,"connected_endpoints_type":null,"connected_endpoints_reachable":null,
			 "connected_endpoints":[]}]}`))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	ports, err := c.ListConsolePortsForDevices(context.Background(), []int{577, 584})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/dcim/console-ports/" {
		t.Errorf("path = %q", gotPath)
	}
	if got, want := gotQuery["device_id"], []string{"577", "584"}; !slices.Equal(got, want) {
		t.Errorf("device_id params = %v, want %v", got, want)
	}
	if len(ports) != 2 {
		t.Fatalf("got %d ports, want 2", len(ports))
	}
	p := ports[0]
	if p.Speed == nil || p.Speed.Value != 115200 || p.ConnectedEndpointsType != "dcim.consoleserverport" ||
		!p.ConnectedEndpointsReachable || len(p.ConnectedEndpoints) != 1 || p.ConnectedEndpoints[0].Name != "ttyS4" ||
		p.ConnectedEndpoints[0].Device == nil || p.ConnectedEndpoints[0].Device.ID != 583 {
		t.Fatalf("cabled port parsed wrong: %+v", p)
	}
	if u := ports[1]; u.Speed != nil || u.ConnectedEndpointsType != "" || u.ConnectedEndpointsReachable || len(u.ConnectedEndpoints) != 0 {
		t.Fatalf("uncabled port parsed wrong: %+v", u)
	}
}
