package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// fakeNetBoxForCables models the slice of the NetBox API that
// ReconcileCables touches: device lookup by name (exact + prefix),
// interface lookup by device_id (+ optional name), cable create, and cable
// delete. It records the cables POSTed so tests can assert exactly what
// wiring was written.
type fakeNetBoxForCables struct {
	devices    []Device    // searchable by name / name__isw / site_id
	interfaces []Interface // each carries a deviceID via the helper map
	ifaceDev   map[int]int // interface ID -> device ID
	siteOf     map[int]int // device ID -> site ID

	createdCables [][2]int // [localIfaceID, remoteIfaceID] per POST
	nextCableID   int
	mutations     []string // any DELETE/PATCH seen (always a test failure)
}

func (f *fakeNetBoxForCables) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/dcim/devices/":
			f.handleDevices(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/api/dcim/interfaces/":
			f.handleInterfaces(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/dcim/cables/":
			f.handleCreateCable(t, w, r)
		case r.Method == http.MethodDelete || r.Method == http.MethodPatch:
			// Cable reconcile is additive: it must never delete or modify
			// anything.
			f.mutations = append(f.mutations, r.Method+" "+r.URL.Path)
			t.Errorf("cable reconcile must not %s: %s", r.Method, r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	})
}

func (f *fakeNetBoxForCables) handleDevices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var siteFilter int
	if s := q.Get("site_id"); s != "" {
		siteFilter = atoiPrefix(s)
	}
	name := q.Get("name")
	isw := q.Get("name__isw")
	var out []Device
	for _, d := range f.devices {
		if siteFilter > 0 && f.siteOf[d.ID] != siteFilter {
			continue
		}
		switch {
		case name != "" && d.Name == name:
			out = append(out, d)
		case isw != "" && strings.HasPrefix(strings.ToLower(d.Name), strings.ToLower(isw)):
			out = append(out, d)
		}
	}
	writeList(w, out)
}

func (f *fakeNetBoxForCables) handleInterfaces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var devFilter int
	if s := q.Get("device_id"); s != "" {
		devFilter = atoiPrefix(s)
	}
	name := q.Get("name")
	var out []Interface
	for _, i := range f.interfaces {
		if devFilter > 0 && f.ifaceDev[i.ID] != devFilter {
			continue
		}
		if name != "" && i.Name != name {
			continue
		}
		out = append(out, i)
	}
	writeList(w, out)
}

func (f *fakeNetBoxForCables) handleCreateCable(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var body struct {
		A []struct {
			ObjectID int `json:"object_id"`
		} `json:"a_terminations"`
		B []struct {
			ObjectID int `json:"object_id"`
		} `json:"b_terminations"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode cable body: %v", err)
	}
	if len(body.A) != 1 || len(body.B) != 1 {
		t.Fatalf("cable POST should have one termination per side, got a=%d b=%d", len(body.A), len(body.B))
	}
	local, remote := body.A[0].ObjectID, body.B[0].ObjectID
	f.createdCables = append(f.createdCables, [2]int{local, remote})
	f.nextCableID++
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(Cable{ID: f.nextCableID})
}

func writeList[T any](w http.ResponseWriter, items []T) {
	_ = json.NewEncoder(w).Encode(ListResponse[T]{Count: len(items), Results: items})
}

func atoiPrefix(s string) int {
	v := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		v = v*10 + int(r-'0')
	}
	return v
}

// l3 host fabric fixture: kworker eth0/eth1 -> leaf-5/leaf-6 swp25, with the
// leaf chassis MACs sitting on the leaves' management eth0 (the exact shape
// that produced the phantom cables).
func cableFixture() *fakeNetBoxForCables {
	const (
		kworkerDev = 100
		leaf5Dev   = 200
		leaf6Dev   = 201
		siteID     = 1

		kEth0, kEth1    = 1000, 1001
		l5Eth0, l5Swp25 = 2000, 2025
		l6Eth0, l6Swp25 = 3000, 3025
	)
	mgmt := func(id int, name string, mac string) Interface {
		return Interface{ID: id, Name: name, MACAddress: mac, MgmtOnly: true}
	}
	swp := func(id int, name string) Interface {
		return Interface{ID: id, Name: name}
	}
	f := &fakeNetBoxForCables{
		devices: []Device{
			{ID: kworkerDev, Name: "kworker-l56-9.pop1"},
			{ID: leaf5Dev, Name: "leaf-5.pop1"},
			{ID: leaf6Dev, Name: "leaf-6.pop1"},
		},
		interfaces: []Interface{
			swp(kEth0, "eth0"), swp(kEth1, "eth1"),
			mgmt(l5Eth0, "eth0", "02:00:00:00:05:01"), swp(l5Swp25, "swp25"),
			mgmt(l6Eth0, "eth0", "02:00:00:00:06:01"), swp(l6Swp25, "swp25"),
		},
		ifaceDev: map[int]int{
			kEth0: kworkerDev, kEth1: kworkerDev,
			l5Eth0: leaf5Dev, l5Swp25: leaf5Dev,
			l6Eth0: leaf6Dev, l6Swp25: leaf6Dev,
		},
		siteOf: map[int]int{kworkerDev: siteID, leaf5Dev: siteID, leaf6Dev: siteID},
	}
	return f
}

// TestReconcileCables_FQDNNeighborResolvesToSwp is a regression test:
// LLDP reports the leaf as an FQDN and the port as swp25; the cable must land
// on the leaf's swp25, never the leaf's mgmt eth0.
func TestReconcileCables_FQDNNeighborResolvesToSwp(t *testing.T) {
	f := cableFixture()
	c := testClient(t, f.handler(t))

	neighbors := []LLDPNeighbor{
		{LocalInterface: "eth0", ChassisName: "leaf-5.internal.pop1.example.net", ChassisMAC: "02:00:00:00:05:01", PortName: "swp25"},
		{LocalInterface: "eth1", ChassisName: "leaf-6.internal.pop1.example.net", ChassisMAC: "02:00:00:00:06:01", PortName: "swp25"},
	}
	if err := c.ReconcileCables(context.Background(), 100, 1, neighbors); err != nil {
		t.Fatalf("ReconcileCables: %v", err)
	}

	if len(f.createdCables) != 2 {
		t.Fatalf("expected 2 cables created, got %d: %v", len(f.createdCables), f.createdCables)
	}
	// eth0 -> leaf-5:swp25 (2025), eth1 -> leaf-6:swp25 (3025).
	// Must NOT be leaf eth0 (2000 / 3000).
	want := map[int]int{1000: 2025, 1001: 3025}
	for _, cab := range f.createdCables {
		local, remote := cab[0], cab[1]
		if remote == 2000 || remote == 3000 {
			t.Errorf("cable landed on leaf mgmt eth0: local=%d remote=%d", local, remote)
		}
		if want[local] != remote {
			t.Errorf("cable local=%d -> remote=%d, want %d", local, remote, want[local])
		}
	}
}

// TestReconcileCables_NoMACFallbackToMgmtPort verifies that when the LLDP
// port name does not resolve to a real port, we do NOT silently fall back to
// the chassis MAC (which resolves to the leaf's mgmt eth0). No cable is better
// than a phantom cable to the wrong port.
func TestReconcileCables_NoMACFallbackToMgmtPort(t *testing.T) {
	f := cableFixture()
	c := testClient(t, f.handler(t))

	// Port name the leaf doesn't have; chassis MAC still matches leaf-5:eth0.
	neighbors := []LLDPNeighbor{
		{LocalInterface: "eth0", ChassisName: "leaf-5.internal.pop1.example.net", ChassisMAC: "02:00:00:00:05:01", PortName: "swp99"},
	}
	if err := c.ReconcileCables(context.Background(), 100, 1, neighbors); err != nil {
		t.Fatalf("ReconcileCables: %v", err)
	}
	if len(f.createdCables) != 0 {
		t.Fatalf("expected NO cable (port unresolved, MAC fallback removed), got %v", f.createdCables)
	}
}

// TestFindDeviceByAnyName_FQDNFallback covers the name resolution directly:
// exact FQDN misses, leading-label prefix hits the short NetBox name.
func TestFindDeviceByAnyName_FQDNFallback(t *testing.T) {
	f := cableFixture()
	c := testClient(t, f.handler(t))

	dev, err := c.findDeviceByAnyName(context.Background(), "leaf-5.internal.pop1.example.net", 1)
	if err != nil {
		t.Fatalf("findDeviceByAnyName: %v", err)
	}
	if dev == nil {
		t.Fatal("expected to resolve leaf-5.pop1 from the FQDN, got nil")
	}
	if dev.Name != "leaf-5.pop1" {
		t.Errorf("resolved %q, want leaf-5.pop1", dev.Name)
	}
}

// Ensure the URL the prefix fallback builds is the one the fake expects
// (guards against a query-param rename breaking resolution silently).
func TestFindDeviceByAnyName_UsesPrefixParam(t *testing.T) {
	var sawISW bool
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if _, ok := q["name__isw"]; ok {
			sawISW = true
		}
		// Always return empty so both the exact and prefix queries fire.
		writeList(w, []Device{})
	}))
	_, _ = c.findDeviceByAnyName(context.Background(), "leaf-5.internal.pop1.example.net", 0)
	if !sawISW {
		t.Error("expected a name__isw prefix query after the exact match missed")
	}
}

// TestCableType_StringForm is the regression for the commission-blocking
// bug: NetBox serializes dcim.Cable.type as a BARE STRING ("aoc"), not
// the {value,label} object most choice fields use, and null when unset.
// A *NestedValue field failed to unmarshal the string form, so
// SyncCommissionData's cable reconcile errored on any host with a typed
// fabric cable (leaf-6's AOC uplinks), and commission_data never
// committed. Cable.Type is now a ChoiceString that tolerates all three
// wire shapes.
func TestCableType_StringForm(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bare-string", `{"id":277,"type":"aoc","status":{"value":"connected","label":"Connected"}}`, "aoc"},
		{"null-type", `{"id":42,"type":null,"status":{"value":"connected"}}`, ""},
		{"absent-type", `{"id":43,"status":{"value":"connected"}}`, ""},
		{"object-form", `{"id":44,"type":{"value":"cat6","label":"CAT6"}}`, "cat6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var c Cable
			if err := json.Unmarshal([]byte(tc.body), &c); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.name, err)
			}
			if string(c.Type) != tc.want {
				t.Fatalf("Type = %q, want %q", c.Type, tc.want)
			}
		})
	}

	// A list of mixed-shape cables (the real leaf-6 response shape: a few
	// null-type mgmt cables + many string-type "aoc" fabric cables) must
	// all parse.
	list := `[{"id":42,"type":null,"status":{"value":"connected"}},
	          {"id":277,"type":"aoc","status":{"value":"connected"}}]`
	var cables []Cable
	if err := json.Unmarshal([]byte(list), &cables); err != nil {
		t.Fatalf("unmarshal cable list: %v", err)
	}
	if len(cables) != 2 || cables[1].ID != 277 || string(cables[1].Type) != "aoc" {
		t.Fatalf("cable list parsed wrong: %+v", cables)
	}
}

// switchCommissionFixture models spine-1.pop1 at commission time
// (2026-10-24): mgmt eth0 (mgmt_only) plus the eno1 twin commission had
// created with the same MAC, cabled fabric ports, and peers on the same
// site (leaf-3, oob-gw-1, leaf-5).
func switchCommissionFixture() *fakeNetBoxForCables {
	const (
		spine, leaf3, oobgw, leaf5 = 10, 20, 30, 40
		siteID                     = 1
	)
	cabled := func(id int, name string, cable int) Interface {
		return Interface{ID: id, Name: name, Cable: &NestedRef{ID: cable}}
	}
	return &fakeNetBoxForCables{
		devices: []Device{
			{ID: spine, Name: "spine-1.pop1"},
			{ID: leaf3, Name: "leaf-3.pop1"},
			{ID: oobgw, Name: "oob-gw-1.pop1"},
			{ID: leaf5, Name: "leaf-5.pop1"},
		},
		interfaces: []Interface{
			{ID: 101, Name: "eth0", MACAddress: "94:6d:ae:52:8a:64", MgmtOnly: true},
			{ID: 102, Name: "eno1", MACAddress: "94:6D:AE:52:8A:64"}, // twin of eth0
			cabled(103, "swp1", 501), // spine:swp1 <-> leaf-3:swp31
			{ID: 104, Name: "swp2"},
			{ID: 105, Name: "swp3"},
			cabled(106, "swp4", 502),
			{ID: 107, Name: "swp5"},
			{ID: 108, Name: "swp6"},
			{ID: 109, Name: "swp7"},
			{ID: 110, Name: "ipmi0", MACAddress: "aa:bb:cc:00:00:01"},
			{ID: 111, Name: "swp9", MACAddress: "AA:BB:CC:00:00:01"}, // carries ipmi0's MAC

			{ID: 201, Name: "eth0", MgmtOnly: true},
			cabled(231, "swp31", 501),
			{ID: 232, Name: "swp32"},
			cabled(229, "swp29", 777), // cabled to some other device
			{ID: 228, Name: "swp28"},
			{ID: 227, Name: "swp27"},
			{ID: 301, Name: "eno3"},
			{ID: 401, Name: "swp1"},
		},
		ifaceDev: map[int]int{
			101: spine, 102: spine, 103: spine, 104: spine, 105: spine, 106: spine,
			107: spine, 108: spine, 109: spine, 110: spine, 111: spine,
			201: leaf3, 231: leaf3, 232: leaf3, 229: leaf3, 228: leaf3, 227: leaf3,
			301: oobgw, 401: leaf5,
		},
		siteOf: map[int]int{spine: siteID, leaf3: siteID, oobgw: siteID, leaf5: siteID},
	}
}

// TestReconcileCables_AdditiveOnly is the 2026-10-24 pop1 regression:
// commission deleted every cable on the switch and recreated cables from
// a commission-initrd LLDP view that only saw the shared OOB segment.
// Reconcile must never delete or modify a cable, and must create one only
// when both ends are uncabled.
func TestReconcileCables_AdditiveOnly(t *testing.T) {
	f := switchCommissionFixture()
	c := testClient(t, f.handler(t))

	neighbors := []LLDPNeighbor{
		// Agrees with existing cable 501: no-op.
		{LocalInterface: "swp1", ChassisName: "leaf-3.internal.pop1.example.net", PortName: "swp31"},
		// Both uncabled: the only cable that may be created.
		{LocalInterface: "swp2", ChassisName: "leaf-3.pop1", PortName: "swp32"},
		// Remote end already cabled elsewhere: leave it.
		{LocalInterface: "swp3", ChassisName: "leaf-3.pop1", PortName: "swp29"},
		// Local end already cabled (502) but LLDP names another port: leave it.
		{LocalInterface: "swp4", ChassisName: "leaf-3.pop1", PortName: "swp28"},
		// Two locals claim the same uncabled remote: ambiguous, no cable
		// for either, regardless of order.
		{LocalInterface: "swp6", ChassisName: "leaf-3.pop1", PortName: "swp27"},
		{LocalInterface: "swp7", ChassisName: "leaf-3.pop1", PortName: "swp27"},
	}
	if err := c.ReconcileCables(context.Background(), 10, 1, neighbors); err != nil {
		t.Fatalf("ReconcileCables: %v", err)
	}
	if len(f.mutations) != 0 {
		t.Errorf("cable reconcile mutated existing objects: %v", f.mutations)
	}
	if want := [][2]int{{104, 232}}; fmt.Sprint(f.createdCables) != fmt.Sprint(want) {
		t.Errorf("created cables = %v, want %v", f.createdCables, want)
	}

	// Same neighbors in reverse order (ParseLLDP's order is map order):
	// identical outcome.
	f2 := switchCommissionFixture()
	c2 := testClient(t, f2.handler(t))
	reversed := make([]LLDPNeighbor, len(neighbors))
	for i, n := range neighbors {
		reversed[len(neighbors)-1-i] = n
	}
	if err := c2.ReconcileCables(context.Background(), 10, 1, reversed); err != nil {
		t.Fatalf("ReconcileCables (reversed): %v", err)
	}
	if fmt.Sprint(f2.createdCables) != fmt.Sprint(f.createdCables) {
		t.Errorf("reversed order created %v, forward created %v", f2.createdCables, f.createdCables)
	}
}

// TestReconcileCables_SkipsMgmtAndSharedSegment pins the bogus-mgmt-cable
// half of the regression: on the unmanaged OOB segment a mgmt NIC hears
// other hosts (spine-1:eno1 <-> leaf-3:eth0, leaf-5:eno1 <->
// oob-gw-1:eno3). Neighbors on a mgmt_only local interface, on a local
// interface carrying a mgmt row's MAC, or on a local interface hearing
// several neighbors must never produce a cable, even with both ends
// uncabled and resolvable.
func TestReconcileCables_SkipsMgmtAndSharedSegment(t *testing.T) {
	f := switchCommissionFixture()
	c := testClient(t, f.handler(t))

	neighbors := []LLDPNeighbor{
		{LocalInterface: "eth0", ChassisName: "oob-gw-1.pop1", PortName: "eno3"},                    // mgmt_only local
		{LocalInterface: "eno1", ChassisName: "leaf-3.pop1", PortName: "swp27"},                     // carries eth0's MAC
		{LocalInterface: "swp9", ChassisName: "leaf-3.pop1", PortName: "swp27"},                     // carries ipmi0's MAC
		{LocalInterface: "swp5", ChassisName: "leaf-3.pop1", PortName: "swp27"},                     // shared segment:
		{LocalInterface: "swp5", ChassisName: "leaf-5.pop1", PortName: "swp1"},                      // two neighbors
		{LocalInterface: "swp7", ChassisName: "leaf-5.internal.pop1.example.net", PortName: "swp1"}, // control
	}
	if err := c.ReconcileCables(context.Background(), 10, 1, neighbors); err != nil {
		t.Fatalf("ReconcileCables: %v", err)
	}
	if len(f.mutations) != 0 {
		t.Errorf("cable reconcile mutated existing objects: %v", f.mutations)
	}
	if want := [][2]int{{109, 401}}; fmt.Sprint(f.createdCables) != fmt.Sprint(want) {
		t.Errorf("created cables = %v, want only the control cable %v", f.createdCables, want)
	}
}
