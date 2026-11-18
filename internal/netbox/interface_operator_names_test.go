package netbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// sn2700Rows is a pop1 SN2700 as operators model it: swpN fabric ports,
// mgmt eth0 (mgmt_only, holds the OOB IP), bond_sp* lags, lo, and a
// MAC-less swp8 (template row).
func sn2700Rows() []map[string]any {
	mgmt := ifaceRow(427, "eth0", "1000base-t", "0C:42:A1:07:C4:92", nil)
	mgmt["mgmt_only"] = true
	return []map[string]any{
		mgmt,
		ifaceRow(424, "bond_sp1", "lag", "20:4D:52:23:00:11", nil),
		ifaceRow(428, "lo", "virtual", "", nil),
		ifaceRow(1259, "swp1", "100gbase-cr4", "1C:34:DA:82:65:FD", nil),
		ifaceRow(1260, "swp2", "100gbase-x-qsfp28", "1C:34:DA:82:65:FF", nil),
		ifaceRow(1254, "swp8", "100gbase-x-qsfp28", "", nil),
		ifaceRow(1270, "swp30", "100gbase-x-qsfp28", "1C:34:DA:82:66:11", nil), // not seen this commission
	}
}

// sn2700CommissionNICs is what the commission initrd reports for that
// switch: kernel names enp3s0npN and eno1 for the mgmt NIC.
func sn2700CommissionNICs() []NICData {
	return []NICData{
		{Name: "eno1", MAC: "0c:42:a1:07:c4:92", Speed: 1_000_000, IntfType: "1000base-t"},
		{Name: "enp3s0np1", MAC: "1c:34:da:82:65:fd", Speed: 100_000_000, IntfType: "100gbase-cr4"},
		{Name: "enp3s0np2", MAC: "1c:34:da:82:65:ff", Speed: 100_000_000, IntfType: "100gbase-x-qsfp28"},
		{Name: "enp3s0np8", MAC: "1c:34:da:82:65:f3", Speed: 100_000_000, IntfType: "100gbase-x-qsfp28"},
	}
}

// TestReconcileInterfaces_OperatorNamesKeepsSwitchRows pins the
// 2026-10-23/24 pop1 regression: commissioning an SN2700 renamed every
// swpN row to its initrd kernel name (enp3s0npN) and created an eno1
// twin of the mgmt eth0 row. On an operator-named device a MAC match
// updates non-name fields only; nothing is renamed, created, or deleted.
func TestReconcileInterfaces_OperatorNamesKeepsSwitchRows(t *testing.T) {
	f := newReconcileFixture(sn2700Rows())
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if err := c.ReconcileInterfaces(context.Background(), 579, sn2700CommissionNICs(), true); err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}

	if len(f.posts) != 0 {
		t.Errorf("expected no interface creations (no eno1 twin, no enp3s0np8 row), got %v", f.posts)
	}
	if len(f.deletes) != 0 {
		t.Errorf("expected no deletions on an operator-named device, got %v", f.deletes)
	}
	if len(f.macs) != 0 {
		t.Errorf("expected no MAC-object writes, got %v", f.macs)
	}
	for _, id := range []int{1259, 1260} {
		ps := f.patches[id]
		if len(ps) != 1 {
			t.Fatalf("row %d: expected one non-name PATCH, got %v", id, ps)
		}
		if name, ok := ps[0]["name"]; ok {
			t.Errorf("row %d: PATCH renames to %v; switch names are operator-owned", id, name)
		}
		if ps[0]["speed"] != float64(100_000_000) {
			t.Errorf("row %d: PATCH speed = %v, want non-name fields still updated", id, ps[0]["speed"])
		}
	}
	for _, id := range []int{427, 424, 428, 1254, 1270} {
		if ps := f.patches[id]; len(ps) != 0 {
			t.Errorf("row %d: expected untouched, got patches %v", id, ps)
		}
	}
}

// TestReconcileInterfaces_ServerStillRenamesSameShape pins that the
// operator-names mode is opt-in: the identical input on a server keeps
// today's MAC-first rename, create, and stale-sweep behavior.
func TestReconcileInterfaces_ServerStillRenamesSameShape(t *testing.T) {
	f := newReconcileFixture(sn2700Rows())
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if err := c.ReconcileInterfaces(context.Background(), 579, sn2700CommissionNICs(), false); err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if ps := f.patches[1259]; len(ps) == 0 || ps[0]["name"] != "enp3s0np1" {
		t.Errorf("row 1259: expected rename PATCH to enp3s0np1, got %v", ps)
	}
	names := map[any]bool{}
	for _, p := range f.posts {
		names[p["name"]] = true
	}
	if !names["eno1"] || !names["enp3s0np8"] {
		t.Errorf("expected eno1 (shared-LOM semantics) and enp3s0np8 created, got %v", f.posts)
	}
	deleted := map[int]bool{}
	for _, id := range f.deletes {
		deleted[id] = true
	}
	if !deleted[1254] || !deleted[1270] {
		t.Errorf("expected unclaimed swp8/swp30 swept, got deletes %v", f.deletes)
	}
}

// TestSyncCommissionData_SwitchRoleKeepsInterfaceNames pins the wiring
// from the NetBox device role to the operator-names mode: role leaf keeps
// swp1, role server renames it.
func TestSyncCommissionData_SwitchRoleKeepsInterfaceNames(t *testing.T) {
	for _, tc := range []struct {
		role       string
		wantRename bool
	}{
		{"leaf", false},
		{"spine", false},
		{"server", true},
		{"", true},
	} {
		t.Run("role="+tc.role, func(t *testing.T) {
			var (
				mu      sync.Mutex
				patches = map[int][]map[string]any{}
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.URL.Path == "/api/dcim/devices/42/" && r.Method == http.MethodPatch:
					_, _ = w.Write([]byte(`{"id": 42}`))
				case r.URL.Path == "/api/dcim/interfaces/" && r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "results": []map[string]any{
						ifaceRow(1259, "swp1", "100gbase-cr4", "1C:34:DA:82:65:FD", nil),
					}})
				case strings.HasPrefix(r.URL.Path, "/api/dcim/interfaces/") && r.Method == http.MethodPatch:
					id, _ := strconv.Atoi(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/dcim/interfaces/"), "/"))
					body, _ := io.ReadAll(r.Body)
					var parsed map[string]any
					_ = json.Unmarshal(body, &parsed)
					patches[id] = append(patches[id], parsed)
					_, _ = w.Write([]byte(`{}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

			err := c.SyncCommissionData(context.Background(), CommissionSync{
				DeviceID:   42,
				DeviceRole: tc.role,
				NICs:       []NICData{{Name: "enp3s0np1", MAC: "1c:34:da:82:65:fd", IntfType: "100gbase-cr4"}},
			})
			if err != nil {
				t.Fatalf("SyncCommissionData: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			ps := patches[1259]
			if len(ps) != 1 {
				t.Fatalf("expected one PATCH on swp1, got %v", ps)
			}
			_, renamed := ps[0]["name"]
			if renamed != tc.wantRename {
				t.Errorf("PATCH %v: renamed=%v, want %v", ps[0], renamed, tc.wantRename)
			}
		})
	}
}
