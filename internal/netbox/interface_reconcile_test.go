package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// reconcileFixture stands up a stub NetBox interfaces API over a
// mutable in-memory row set and records every write so tests can
// assert exactly what ReconcileInterfaces did.
type reconcileFixture struct {
	rows    map[int]map[string]any // interface id -> row
	patches map[int][]map[string]any
	posts   []map[string]any
	deletes []int
	macs    []map[string]any // POST /api/dcim/mac-addresses/ bodies
	nextID  int
}

func newReconcileFixture(rows []map[string]any) *reconcileFixture {
	f := &reconcileFixture{
		rows:    map[int]map[string]any{},
		patches: map[int][]map[string]any{},
		nextID:  1000,
	}
	for _, r := range rows {
		f.rows[r["id"].(int)] = r
	}
	return f
}

func (f *reconcileFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dcim/interfaces/", func(w http.ResponseWriter, r *http.Request) {
		idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/dcim/interfaces/"), "/")
		switch {
		case r.Method == http.MethodGet:
			results := make([]map[string]any, 0, len(f.rows))
			for _, row := range f.rows {
				results = append(results, row)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"count": len(results), "results": results})
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			f.nextID++
			parsed["id"] = f.nextID
			f.posts = append(f.posts, parsed)
			f.rows[f.nextID] = parsed
			_ = json.NewEncoder(w).Encode(map[string]any{"id": f.nextID, "name": parsed["name"]})
		case r.Method == http.MethodPatch:
			id, _ := strconv.Atoi(idStr)
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			f.patches[id] = append(f.patches[id], parsed)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete:
			id, _ := strconv.Atoi(idStr)
			f.deletes = append(f.deletes, id)
			delete(f.rows, id)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/api/dcim/mac-addresses/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		f.macs = append(f.macs, parsed)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 9000 + len(f.macs)})
	})
	return httptest.NewServer(mux)
}

// row builders for readability.
func ifaceRow(id int, name, typ, mac string, lag any) map[string]any {
	r := map[string]any{
		"id":      id,
		"name":    name,
		"type":    map[string]any{"value": typ},
		"enabled": true,
	}
	if mac != "" {
		r["mac_address"] = mac
	}
	if lag != nil {
		r["lag"] = lag
	}
	return r
}

// TestReconcileInterfaces_MACMatchRenamesPreservingLAG pins the
// kworker-l56-1 regression: a bond member row named ens2f0np0 with
// lag=bond0, observed by commission as eth0 (same MAC), must be
// RENAMED in place — one PATCH on the existing row, no POST, no
// DELETE — so the lag link survives.
func TestReconcileInterfaces_MACMatchRenamesPreservingLAG(t *testing.T) {
	bondRef := map[string]any{"id": 10, "name": "bond0"}
	f := newReconcileFixture([]map[string]any{
		ifaceRow(10, "bond0", "lag", "", nil),
		ifaceRow(11, "ens2f0np0", "25gbase-x-sfp28", "7C:C2:55:BE:F6:0C", bondRef),
		ifaceRow(12, "ens2f1np1", "25gbase-x-sfp28", "7C:C2:55:BE:F6:0D", bondRef),
		ifaceRow(13, "ipmi0", "1000base-t", "90:5A:08:15:6C:AA", nil),
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 809, []NICData{
		{Name: "eth0", MAC: "7c:c2:55:be:f6:0c", IntfType: "25gbase-x-sfp28"},
		{Name: "eth1", MAC: "7c:c2:55:be:f6:0d", IntfType: "25gbase-x-sfp28"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}

	if len(f.posts) != 0 {
		t.Errorf("expected no interface creations, got %d: %v", len(f.posts), f.posts)
	}
	if len(f.deletes) != 0 {
		t.Errorf("expected no deletions, got %v", f.deletes)
	}
	for id, want := range map[int]string{11: "eth0", 12: "eth1"} {
		ps := f.patches[id]
		if len(ps) == 0 {
			t.Fatalf("row %d: expected a rename PATCH, got none", id)
		}
		if got := ps[0]["name"]; got != want {
			t.Errorf("row %d: PATCH name = %v, want %s", id, got, want)
		}
		// The PATCH body must not touch lag — absence preserves the link.
		if _, hasLag := ps[0]["lag"]; hasLag {
			t.Errorf("row %d: PATCH body sets lag (must leave membership untouched)", id)
		}
	}
	// MACs already match — no MAC-object churn.
	if len(f.macs) != 0 {
		t.Errorf("expected no MAC-object creations, got %v", f.macs)
	}
}

// TestReconcileInterfaces_PlaceholderEvictedOnRename pins the
// rename-collision path: a MAC-less placeholder row already holding
// the target name ("eth0") is deleted so the MAC-matched row can take
// the name, and the placeholder is not re-deleted in the stale sweep.
func TestReconcileInterfaces_PlaceholderEvictedOnRename(t *testing.T) {
	bondRef := map[string]any{"id": 10, "name": "bond0"}
	f := newReconcileFixture([]map[string]any{
		ifaceRow(10, "bond0", "lag", "", nil),
		ifaceRow(11, "ens2f0np0", "25gbase-x-sfp28", "7C:C2:55:BE:91:BC", bondRef),
		ifaceRow(14, "eth0", "1000base-t", "", nil), // MAC-less placeholder squatting the name
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 817, []NICData{
		{Name: "eth0", MAC: "7c:c2:55:be:91:bc", IntfType: "25gbase-x-sfp28"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if want := []int{14}; len(f.deletes) != 1 || f.deletes[0] != want[0] {
		t.Errorf("deletes = %v, want %v (placeholder only, once)", f.deletes, want)
	}
	if ps := f.patches[11]; len(ps) == 0 || ps[0]["name"] != "eth0" {
		t.Errorf("row 11: expected rename PATCH to eth0, got %v", ps)
	}
}

// TestReconcileInterfaces_NameFallbackForMACless pins that NICs
// without a MAC still match by name (no spurious create+delete).
func TestReconcileInterfaces_NameFallbackForMACless(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(20, "eth0", "1000base-t", "", nil),
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "eth0", IntfType: "1000base-t"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if len(f.posts) != 0 || len(f.deletes) != 0 {
		t.Errorf("expected pure update; posts=%v deletes=%v", f.posts, f.deletes)
	}
	if len(f.patches[20]) == 0 {
		t.Error("expected PATCH on existing row 20")
	}
}

// TestReconcileInterfaces_DuplicateMACFallsBackToName pins the
// roast-found double-claim flaw: when two NICs report the same MAC
// (bonded/virtual NICs), the second must fall back to adopting its
// name-matched row via PATCH — not POST a duplicate name into
// NetBox's device+name uniqueness constraint.
func TestReconcileInterfaces_DuplicateMACFallsBackToName(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(40, "eth0", "25gbase-x-sfp28", "AA:BB:CC:DD:EE:01", nil),
		ifaceRow(41, "eth1", "25gbase-x-sfp28", "", nil), // MAC-less row for the dup-MAC NIC
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", IntfType: "25gbase-x-sfp28"},
		{Name: "eth1", MAC: "aa:bb:cc:dd:ee:01", IntfType: "25gbase-x-sfp28"}, // same MAC
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if len(f.posts) != 0 {
		t.Errorf("expected no creations (eth1 must adopt row 41 by name), got %v", f.posts)
	}
	if len(f.patches[41]) == 0 {
		t.Error("expected PATCH on row 41 (name fallback for dup-MAC NIC)")
	}
	if len(f.deletes) != 0 {
		t.Errorf("expected no deletions, got %v", f.deletes)
	}
}

// TestReconcileInterfaces_StaleSweepKeepsProtected pins the deletion
// guard: unclaimed data NICs are deleted, while bonds, mgmt_only rows,
// and BMC-named rows survive.
func TestReconcileInterfaces_StaleSweepKeepsProtected(t *testing.T) {
	mgmt := ifaceRow(31, "mgmt0", "1000base-t", "", nil)
	mgmt["mgmt_only"] = true
	f := newReconcileFixture([]map[string]any{
		ifaceRow(30, "bond0", "lag", "", nil),
		mgmt,
		ifaceRow(32, "ipmi0", "1000base-t", "", nil),
		ifaceRow(33, "enp99s0", "10gbase-x-sfpp", "AA:BB:CC:00:00:33", nil), // gone from hw
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if err := c.ReconcileInterfaces(context.Background(), 1, nil, false); err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if want := fmt.Sprint([]int{33}); fmt.Sprint(f.deletes) != want {
		t.Errorf("deletes = %v, want %v", f.deletes, want)
	}
}

// TestReconcileInterfaces_StaleSweepKeepsLo pins the cpu2.pop3
// regression (2026-07-23): an unclaimed "lo" row of type virtual — the
// operator-modeled identity loopback the L3 netscripts hard-require —
// must survive the stale sweep. Commission's ip_nics inventory only
// walks physical NICs, so "lo" is never observed and would otherwise
// look stale.
func TestReconcileInterfaces_StaleSweepKeepsLo(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(50, "lo", "virtual", "", nil),
		ifaceRow(51, "enp99s0", "10gbase-x-sfpp", "AA:BB:CC:00:00:51", nil), // gone from hw
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if err := c.ReconcileInterfaces(context.Background(), 1, nil, false); err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if want := fmt.Sprint([]int{51}); fmt.Sprint(f.deletes) != want {
		t.Errorf("deletes = %v, want %v (lo must survive)", f.deletes, want)
	}
}

// TestReconcileInterfaces_StaleSweepKeepsAnyVirtual pins the broader
// guard: an unclaimed virtual-type row survives regardless of name,
// because commission's inventory can never observe a virtual NIC by
// construction (it only walks physical hardware) — an unclaimed
// virtual row is always operator intent, never staleness.
func TestReconcileInterfaces_StaleSweepKeepsAnyVirtual(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(60, "dummy0", "virtual", "", nil),
		ifaceRow(61, "enp99s0", "10gbase-x-sfpp", "AA:BB:CC:00:00:61", nil), // gone from hw
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	if err := c.ReconcileInterfaces(context.Background(), 1, nil, false); err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if want := fmt.Sprint([]int{61}); fmt.Sprint(f.deletes) != want {
		t.Errorf("deletes = %v, want %v (virtual dummy0 must survive)", f.deletes, want)
	}
}

// TestReconcileInterfaces_ProtectedLAGSurvivesMACCollision pins the
// protected-row MATCH-safety half: a modeled lag row (Linux bond) sharing its
// member NIC's MAC must never be claimed and PATCHed by the observed-NIC
// match loop. Protected rows are invisible to byMAC entirely, so the
// observed NIC falls through to a fresh POST instead of clobbering the
// bond's type/name/mgmt_only via PATCH.
func TestReconcileInterfaces_ProtectedLAGSurvivesMACCollision(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(70, "bond0", "lag", "AA:BB:CC:DD:EE:70", nil),
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "eth0", MAC: "aa:bb:cc:dd:ee:70", IntfType: "25gbase-x-sfp28"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if ps := f.patches[70]; len(ps) != 0 {
		t.Errorf("row 70 (protected lag): expected zero patches, got %v", ps)
	}
	if len(f.posts) != 1 || f.posts[0]["name"] != "eth0" {
		t.Errorf("expected exactly one POST named eth0, got %v", f.posts)
	}
}

// TestReconcileInterfaces_ProtectedBMCSurvivesMACCollision pins the
// protected-row MATCH-safety half for shared-LOM hardware: a mgmt_only BMC row
// sharing a MAC with an observed host NIC must never be claimed and
// PATCHed by the match loop.
func TestReconcileInterfaces_ProtectedBMCSurvivesMACCollision(t *testing.T) {
	bmc := ifaceRow(71, "ipmi0", "1000base-t", "AA:BB:CC:DD:EE:71", nil)
	bmc["mgmt_only"] = true
	f := newReconcileFixture([]map[string]any{bmc})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "eno1", MAC: "aa:bb:cc:dd:ee:71", IntfType: "1000base-t"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if ps := f.patches[71]; len(ps) != 0 {
		t.Errorf("row 71 (protected BMC): expected zero patches, got %v", ps)
	}
	if len(f.posts) != 1 || f.posts[0]["name"] != "eno1" {
		t.Errorf("expected exactly one POST named eno1, got %v", f.posts)
	}
}

// TestReconcileInterfaces_MgmtOnlyRowIsIdempotent pins a roast-found
// regression in the initial protected-row fix: SyncCommissionData promises
// idempotency and calls ReconcileInterfaces every commission run with a
// synthetic mgmt_only "ipmi" NICData carrying the BMC MAC. Once that row
// exists (created by an earlier reconcile), it is protected and excluded
// from the match maps, so a second identical reconcile must not error —
// it must recognize the collision is the row's own prior creation (same
// name, same MAC) and skip without touching it.
func TestReconcileInterfaces_MgmtOnlyRowIsIdempotent(t *testing.T) {
	bmc := ifaceRow(90, "ipmi", "1000base-t", "AA:BB:CC:DD:EE:90", nil)
	bmc["mgmt_only"] = true
	f := newReconcileFixture([]map[string]any{bmc})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:90", IntfType: "1000base-t", MgmtOnly: true},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: expected idempotent no-op, got error: %v", err)
	}
	if len(f.posts) != 0 {
		t.Errorf("expected no creations, got %v", f.posts)
	}
	if ps := f.patches[90]; len(ps) != 0 {
		t.Errorf("row 90 (protected mgmt_only): expected zero patches, got %v", ps)
	}
	if len(f.deletes) != 0 {
		t.Errorf("expected no deletions, got %v", f.deletes)
	}
}

// TestReconcileInterfaces_MgmtOnlyIdempotentRegardlessOfProcessingOrder
// pins a roast-found regression: on shared-LOM hardware, the protected
// BMC row and an unprotected host interface can carry the same MAC.
// SyncCommissionData always appends the synthetic "ipmi" NICData before
// host NICs (see sync.go), so the mgmt NIC is processed FIRST here — if
// the idempotency check only ran after ordinary byMAC/byName resolution,
// byMAC would resolve the shared MAC to the unprotected host row (since
// protected rows are excluded from byMAC) and attempt an errant rename,
// making the outcome depend on NIC order. The check must be
// order-independent: the mgmt NIC must skip cleanly, and the
// host NIC (processed second, same MAC) must still be reconciled
// normally.
func TestReconcileInterfaces_MgmtOnlyIdempotentRegardlessOfProcessingOrder(t *testing.T) {
	bmc := ifaceRow(95, "ipmi", "1000base-t", "AA:BB:CC:DD:EE:95", nil)
	bmc["mgmt_only"] = true
	f := newReconcileFixture([]map[string]any{
		bmc,
		ifaceRow(96, "eno1", "1000base-t", "AA:BB:CC:DD:EE:95", nil), // shared-LOM: same MAC as BMC
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:95", IntfType: "1000base-t", MgmtOnly: true}, // mgmt NIC first, as SyncCommissionData orders it
		{Name: "eno1", MAC: "aa:bb:cc:dd:ee:95", IntfType: "1000base-t"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	if ps := f.patches[95]; len(ps) != 0 {
		t.Errorf("row 95 (protected mgmt_only): expected zero patches, got %v", ps)
	}
	if len(f.patches[96]) == 0 {
		t.Error("row 96 (host NIC): expected a PATCH (normal reconcile of the shared-MAC host NIC)")
	}
	if len(f.posts) != 0 {
		t.Errorf("expected no creations, got %v", f.posts)
	}
	if len(f.deletes) != 0 {
		t.Errorf("expected no deletions, got %v", f.deletes)
	}
}

// TestReconcileInterfaces_MgmtOnlyFirstRunNeverClaimsHostRowByMAC pins a
// roast-found gap in the mgmt-only idempotency fix: on the FIRST-EVER
// reconcile of shared-LOM hardware, no protected BMC row exists yet, so
// the idempotency check (which only recognizes an existing protected
// row) cannot fire. SyncCommissionData still processes the synthetic
// "ipmi" NICData before the host NIC, sharing the host's MAC. Without a
// mgmt_only guard on ordinary byMAC/byName resolution, the mgmt NIC
// would MAC-match the UNPROTECTED host row and rename it to "ipmi" with
// mgmt_only=true, corrupting the host interface (stripping whatever
// cables/IPs it carried) and leaving the host NIC to fall through to a
// bare POST for a new "eno1" row. A mgmt_only NIC must never claim an
// existing unprotected row by ordinary matching; on first run it must
// always create its own new row.
func TestReconcileInterfaces_MgmtOnlyFirstRunNeverClaimsHostRowByMAC(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(97, "eno1", "1000base-t", "AA:BB:CC:DD:EE:97", nil), // unprotected host row, no BMC row yet
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:97", IntfType: "1000base-t", MgmtOnly: true}, // mgmt NIC first, as SyncCommissionData orders it
		{Name: "eno1", MAC: "aa:bb:cc:dd:ee:97", IntfType: "1000base-t"},
	}, false)
	if err != nil {
		t.Fatalf("ReconcileInterfaces: %v", err)
	}
	// Row 97 is legitimately reconciled by the HOST NIC (matched by MAC,
	// same name, ordinary PATCH) — that's correct and expected. What
	// must never happen is the row being renamed to "ipmi" or marked
	// mgmt_only, which would mean the MGMT NIC claimed it instead.
	for _, p := range f.patches[97] {
		if p["name"] == "ipmi" || p["mgmt_only"] == true {
			t.Errorf("row 97 (unprotected host): claimed/renamed by the mgmt NIC, got patch %v", p)
		}
	}
	if len(f.deletes) != 0 {
		t.Errorf("expected no deletions, got %v", f.deletes)
	}
	names := map[string]bool{}
	for _, p := range f.posts {
		names[fmt.Sprint(p["name"])] = true
	}
	if !names["ipmi"] {
		t.Errorf("expected a new interface created for the mgmt NIC (\"ipmi\"), got posts=%v", f.posts)
	}
	if len(f.posts) != 1 {
		t.Errorf("expected exactly one creation (host row 97 reconciles via PATCH, not POST), got %v", f.posts)
	}
}

// TestReconcileInterfaces_NonMgmtNameAndMACCollisionStillErrors pins the
// scope of the mgmt-only idempotency exception: a non-mgmt observed NIC
// whose name AND MAC both happen to equal a protected row's (e.g. a
// bond's name/MAC squatting on a kernel-reported name) must still error
// loudly. Only a MgmtOnly-on-both-sides match is a legitimate rerun of
// the synthetic BMC NICData; this is a genuine modeling conflict.
func TestReconcileInterfaces_NonMgmtNameAndMACCollisionStillErrors(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(91, "bond0", "lag", "AA:BB:CC:DD:EE:91", nil),
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "bond0", MAC: "aa:bb:cc:dd:ee:91", IntfType: "25gbase-x-sfp28"}, // MgmtOnly: false
	}, false)
	if err == nil {
		t.Fatal("expected a loud error on non-mgmt name+MAC collision, got nil")
	}
	if !strings.Contains(err.Error(), "bond0") {
		t.Errorf("error = %v, want it to mention the colliding name %q", err, "bond0")
	}
	if ps := f.patches[91]; len(ps) != 0 {
		t.Errorf("row 91 (protected): expected zero patches, got %v", ps)
	}
	if len(f.posts) != 0 {
		t.Errorf("expected no creations, got %v", f.posts)
	}
}

// TestReconcileInterfaces_ProtectedNameCollisionOnRenameIsLoudError pins
// a roast-found gap in the initial protected-row fix: a MAC-matched
// unprotected row being renamed can also collide with a protected row's
// name (not just the no-match/create path). Row 81 (unprotected, will
// MAC-match and want to rename to "bond0") must not be PATCHed, and
// protected row 80 ("bond0", lag) must not be deleted or touched — the
// rename must fail loudly instead of falling through to NetBox's
// device+name uniqueness 400.
func TestReconcileInterfaces_ProtectedNameCollisionOnRenameIsLoudError(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(80, "bond0", "lag", "", nil),
		ifaceRow(81, "ens2f0np0", "25gbase-x-sfp28", "AA:BB:CC:DD:EE:81", nil),
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "bond0", MAC: "aa:bb:cc:dd:ee:81", IntfType: "25gbase-x-sfp28"},
	}, false)
	if err == nil {
		t.Fatal("expected a loud error on protected name collision during rename, got nil")
	}
	if !strings.Contains(err.Error(), "bond0") {
		t.Errorf("error = %v, want it to mention the colliding name %q", err, "bond0")
	}
	if ps := f.patches[81]; len(ps) != 0 {
		t.Errorf("row 81: expected zero patches (rename must not proceed), got %v", ps)
	}
	if ps := f.patches[80]; len(ps) != 0 {
		t.Errorf("row 80 (protected): expected zero patches, got %v", ps)
	}
	for _, id := range f.deletes {
		if id == 80 {
			t.Error("row 80 (protected) must not be deleted")
		}
	}
}

// TestReconcileInterfaces_ProtectedNameCollisionIsLoudError pins the
// protected-row fallback: when an observed NIC's name collides with a
// protected row's name and there is no unprotected candidate, NetBox
// would reject a POST with a device+name uniqueness 400 (the protected
// row still holds the name in NetBox's DB even though it's excluded from
// our maps). ReconcileInterfaces must detect this before POSTing and
// return a loud, descriptive error instead of surfacing a raw 400 or
// silently skipping the NIC or deleting the protected row.
func TestReconcileInterfaces_ProtectedNameCollisionIsLoudError(t *testing.T) {
	f := newReconcileFixture([]map[string]any{
		ifaceRow(72, "bond0", "lag", "", nil),
	})
	srv := f.server(t)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.ReconcileInterfaces(context.Background(), 1, []NICData{
		{Name: "bond0", MAC: "aa:bb:cc:dd:ee:99", IntfType: "1000base-t"},
	}, false)
	if err == nil {
		t.Fatal("expected a loud error on protected name collision, got nil")
	}
	if !strings.Contains(err.Error(), "bond0") {
		t.Errorf("error = %v, want it to mention the colliding name %q", err, "bond0")
	}
	if ps := f.patches[72]; len(ps) != 0 {
		t.Errorf("row 72 (protected): expected zero patches, got %v", ps)
	}
	for _, id := range f.deletes {
		if id == 72 {
			t.Error("row 72 (protected) must not be deleted")
		}
	}
}
