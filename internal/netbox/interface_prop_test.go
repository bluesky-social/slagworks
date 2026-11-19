package netbox

// Model-based property tests for ReconcileInterfaces against the strict
// fakeNetBox. The oracle restates the doc-comment contract independently
// (it does not call isProtectedInterface or any other production helper):
//
//   C1 completeness: every observed host NIC ends with exactly one row of
//      its name, carrying its MAC (case-insensitive) when it has one.
//   C2 protection: protected rows (mgmt_only, lag, virtual, "lo", BMC- or
//      bond-named) keep id, name, type, mgmt_only and MAC.
//   C3 soundness: every surviving unprotected row is named after an
//      observed NIC.
//   C4 MAC identity: a NIC whose MAC is unique among NICs and held by
//      exactly one unprotected row keeps THAT row (same id), renamed to the
//      NIC's name ("Identity is the MAC address, not the name").
//   C5 no raw NetBox 400s: collisions fail loudly before NetBox rejects,
//      and only for the modeling conflicts justifiedConflict names; a
//      refused reconcile has written nothing.
//   C6 idempotence: a second identical run leaves the state unchanged.
//   C7 isolation: rows on another device are never touched.
//   C8 convergence under faults: a fault on write k followed by a clean
//      rerun reaches the same canonical state as a fault-free run
//      (SyncCommissionData: "partial completion ... corrected by the next
//      successful run").

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

const (
	propDev   = 1
	otherDev  = 2
	propTypeP = "25gbase-x-sfp28"
)

var (
	propRowNames = []string{"eth0", "eth1", "eth2", "ens1f0", "eno1", "ipmi", "bond0", "lo", "mgmt0"}
	propNICNames = []string{"eth0", "eth1", "eth2", "ens1f0", "eno1"}
	propMACs     = []string{"aa:00:00:00:00:01", "aa:00:00:00:00:02", "aa:00:00:00:00:03", "aa:00:00:00:00:04"}
	propTypes    = []string{"1000base-t", propTypeP, propTypeP, "lag", "virtual"}
)

type seedRow struct {
	Name, Type, MAC string
	Mgmt            bool
}

type ifaceWorld struct {
	Rows  []seedRow
	Other []seedRow // device 2
	NICs  []NICData
}

func oracleProtected(r fnbRow) bool {
	if r.MgmtOnly || r.Type == "lag" || r.Type == "virtual" {
		return true
	}
	n := strings.ToLower(r.Name)
	if n == "lo" {
		return true
	}
	for _, p := range []string{"ipmi", "bmc", "idrac", "ilo", "drac", "bond"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func drawMAC(t *rapid.T, label string, allowEmpty bool) string {
	opts := propMACs
	if allowEmpty {
		opts = append([]string{""}, propMACs...)
	}
	m := rapid.SampledFrom(opts).Draw(t, label)
	if rapid.Bool().Draw(t, label+"-upper") {
		m = strings.ToUpper(m)
	}
	return m
}

func genSeedRows(t *rapid.T, label string, max int) []seedRow {
	n := rapid.IntRange(0, max).Draw(t, label+"-n")
	used := map[string]bool{}
	var out []seedRow
	for i := range n {
		name := rapid.SampledFrom(propRowNames).Draw(t, fmt.Sprintf("%s-%d-name", label, i))
		if used[name] {
			continue
		}
		used[name] = true
		out = append(out, seedRow{
			Name: name,
			Type: rapid.SampledFrom(propTypes).Draw(t, fmt.Sprintf("%s-%d-type", label, i)),
			MAC:  drawMAC(t, fmt.Sprintf("%s-%d-mac", label, i), true),
			Mgmt: rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("%s-%d-mgmt", label, i)) == 0,
		})
	}
	return out
}

// genIfaceWorld draws a seeded device plus observed NICs. allowDupMAC lets
// two NICs report the same MAC (bonded/virtual NICs, which ReconcileInterfaces
// documents as happening); with it off, NIC MACs are distinct.
func genIfaceWorld(t *rapid.T, allowDupMAC bool) ifaceWorld {
	w := ifaceWorld{
		Rows:  genSeedRows(t, "row", 6),
		Other: genSeedRows(t, "other", 2),
	}
	n := rapid.IntRange(0, 4).Draw(t, "nic-n")
	used := map[string]bool{}
	usedMAC := map[string]bool{}
	for i := range n {
		name := rapid.SampledFrom(propNICNames).Draw(t, fmt.Sprintf("nic-%d-name", i))
		if used[name] {
			continue
		}
		mac := drawMAC(t, fmt.Sprintf("nic-%d-mac", i), rapid.IntRange(0, 5).Draw(t, fmt.Sprintf("nic-%d-maclessp", i)) == 0)
		if !allowDupMAC && mac != "" && usedMAC[strings.ToUpper(mac)] {
			continue
		}
		used[name] = true
		usedMAC[strings.ToUpper(mac)] = true
		w.NICs = append(w.NICs, NICData{Name: name, MAC: mac, IntfType: propTypeP})
	}
	return w
}

// seed resets f and loads the world into it.
func (w ifaceWorld) seed(f *fakeNetBox) {
	f.reset()
	for _, r := range w.Rows {
		f.addIface(propDev, r.Name, r.Type, r.Mgmt, r.MAC)
	}
	for _, r := range w.Other {
		f.addIface(otherDev, r.Name, r.Type, r.Mgmt, r.MAC)
	}
}

// sharedFake starts one fake server for the whole (outer) test.
func sharedFake(t *testing.T) *fakeNetBox {
	f := newFakeNetBox()
	f.start()
	t.Cleanup(f.close)
	return f
}

func quietClient(base string) *Client {
	c := NewClient(ClientConfig{BaseURL: base, Token: "t", Timeout: 5 * time.Second})
	c.retryBaseWait = time.Millisecond
	c.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return c
}

// justifiedConflict reports whether the oracle agrees a loud failure is the
// right answer. Reconcile may refuse, before writing anything, when
//
//   - some host NIC's name is held by a protected row, or
//   - NetBox models one MAC on two unprotected rows, an observed NIC
//     reports that MAC, and one of the two rows holds an observed NIC's
//     name: freeing the name would delete a row that is not stale.
func justifiedConflict(before []fnbRow, nics []NICData) bool {
	nicNames := map[string]bool{}
	nicMACs := map[string]bool{}
	for _, n := range nics {
		nicNames[n.Name] = true
		if n.MAC != "" {
			nicMACs[strings.ToUpper(n.MAC)] = true
		}
	}
	for _, r := range before {
		if !nicNames[r.Name] {
			continue
		}
		if oracleProtected(r) {
			return true
		}
		if r.MAC == "" || !nicMACs[r.MAC] {
			continue
		}
		for _, o := range before {
			if o.ID != r.ID && !oracleProtected(o) && o.MAC == r.MAC {
				return true
			}
		}
	}
	return false
}

func checkReconciled(before, after []fnbRow, nics []NICData) string {
	byID := map[int]fnbRow{}
	for _, r := range after {
		byID[r.ID] = r
	}
	nicNames := map[string]bool{}
	for _, n := range nics {
		nicNames[n.Name] = true
	}
	// C1
	for _, n := range nics {
		var hits []fnbRow
		for _, r := range after {
			if r.Name == n.Name {
				hits = append(hits, r)
			}
		}
		if len(hits) != 1 {
			return fmt.Sprintf("C1: NIC %s: %d rows carry its name, want 1; after=%+v", n.Name, len(hits), after)
		}
		if n.MAC != "" && !strings.EqualFold(hits[0].MAC, n.MAC) {
			return fmt.Sprintf("C1: NIC %s: row MAC %q, want %q; after=%+v", n.Name, hits[0].MAC, n.MAC, after)
		}
	}
	// C2
	for _, r := range before {
		if !oracleProtected(r) {
			continue
		}
		got, ok := byID[r.ID]
		if !ok || got != r {
			return fmt.Sprintf("C2: protected row %+v changed to %+v (present=%v)", r, got, ok)
		}
	}
	// C3
	protectedIDs := map[int]bool{}
	for _, r := range before {
		if oracleProtected(r) {
			protectedIDs[r.ID] = true
		}
	}
	for _, r := range after {
		if protectedIDs[r.ID] {
			continue
		}
		if !nicNames[r.Name] {
			return fmt.Sprintf("C3: surviving row %+v is not named after any observed NIC; after=%+v", r, after)
		}
	}
	// C4
	macCountNIC := map[string]int{}
	for _, n := range nics {
		if n.MAC != "" {
			macCountNIC[strings.ToUpper(n.MAC)]++
		}
	}
	for _, n := range nics {
		m := strings.ToUpper(n.MAC)
		if m == "" || macCountNIC[m] != 1 {
			continue
		}
		var holders []fnbRow
		for _, r := range before {
			if !oracleProtected(r) && r.MAC == m {
				holders = append(holders, r)
			}
		}
		if len(holders) != 1 {
			continue
		}
		got, ok := byID[holders[0].ID]
		if !ok || got.Name != n.Name {
			return fmt.Sprintf("C4: NIC %s (MAC %s) did not keep its MAC-matched row %+v; got %+v (present=%v); before=%+v after=%+v",
				n.Name, m, holders[0], got, ok, before, after)
		}
	}
	return ""
}

func TestPropReconcileInterfaces(t *testing.T) { reconcileProp(t, false) }

// TestPropReconcileInterfacesDupMAC lets two observed NICs share a MAC.
func TestPropReconcileInterfacesDupMAC(t *testing.T) { reconcileProp(t, true) }

func reconcileProp(t *testing.T, allowDupMAC bool) {
	f := sharedFake(t)
	c := quietClient(f.url())
	rapid.Check(t, func(t *rapid.T) {
		w := genIfaceWorld(t, allowDupMAC)
		w.seed(f)
		before := f.rows(propDev)
		otherBefore := f.rows(otherDev)

		err := c.ReconcileInterfaces(context.Background(), propDev, w.NICs, false)
		after := f.rows(propDev)
		if got := f.rows(otherDev); fmt.Sprint(got) != fmt.Sprint(otherBefore) {
			t.Fatalf("C7: device %d rows changed: %+v -> %+v", otherDev, otherBefore, got)
		}
		if len(f.rejected) > 0 {
			t.Fatalf("C5: NetBox rejected a write (%v); reconcile err=%v; before=%+v nics=%+v writes=%v",
				f.rejected, err, before, w.NICs, f.writes)
		}
		if err != nil {
			if !justifiedConflict(before, w.NICs) {
				t.Fatalf("unjustified reconcile error: %v; before=%+v nics=%+v writes=%v", err, before, w.NICs, f.writes)
			}
			if len(f.writes) > 0 {
				t.Fatalf("C5: reconcile refused (%v) after writing %v; before=%+v nics=%+v", err, f.writes, before, w.NICs)
			}
			return
		}
		if msg := checkReconciled(before, after, w.NICs); msg != "" {
			t.Fatal(msg)
		}

		// C6
		if err := c.ReconcileInterfaces(context.Background(), propDev, w.NICs, false); err != nil {
			t.Fatalf("C6: second run failed: %v", err)
		}
		if again := f.rows(propDev); fmt.Sprint(again) != fmt.Sprint(after) {
			t.Fatalf("C6: second run changed state:\n first=%+v\nsecond=%+v", after, again)
		}
	})
}

// canonical drops ids of rows created during the run so fault-free and
// faulted+rerun outcomes are comparable; surviving seeded ids are kept.
func canonical(rows []fnbRow, seeded map[int]bool) string {
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		id := "new"
		if seeded[r.ID] {
			id = fmt.Sprint(r.ID)
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%v|%s", r.Name, id, r.Type, r.MgmtOnly, r.MAC))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func TestPropReconcileInterfacesFaultConvergence(t *testing.T) {
	clean := sharedFake(t)
	faulty := sharedFake(t)
	rapid.Check(t, func(t *rapid.T) {
		w := genIfaceWorld(t, false)
		w.seed(clean)
		seeded := map[int]bool{}
		for _, r := range clean.rows(propDev) {
			seeded[r.ID] = true
		}
		before := clean.rows(propDev)
		if err := quietClient(clean.url()).ReconcileInterfaces(context.Background(), propDev, w.NICs, false); err != nil {
			if justifiedConflict(before, w.NICs) {
				return // nothing to converge to: reconcile refuses this world
			}
			t.Fatalf("unjustified reconcile error: %v; before=%+v nics=%+v", err, before, w.NICs)
		}
		want := canonical(clean.rows(propDev), seeded)
		nWrites := len(clean.writes)
		if nWrites == 0 {
			return // already converged: no write to fault
		}

		// Draw before taking the lock: rapid aborts a case by panicking
		// inside Draw, which would leave the mutex held.
		faultAt := rapid.IntRange(1, nWrites).Draw(t, "faultAt")
		faultStatus := rapid.SampledFrom([]int{http.StatusInternalServerError, http.StatusBadGateway}).Draw(t, "faultStatus")
		faultApplied := rapid.Bool().Draw(t, "faultApplied")
		w.seed(faulty)
		faulty.mu.Lock()
		faulty.faultMethods = map[string]bool{http.MethodPost: true, http.MethodPatch: true, http.MethodDelete: true}
		faulty.faultAt = faultAt
		faulty.faultStatus = faultStatus
		faulty.faultApplied = faultApplied
		faulty.mu.Unlock()
		c := quietClient(faulty.url())
		firstErr := c.ReconcileInterfaces(context.Background(), propDev, w.NICs, false)
		if faulty.faultsHit != 1 {
			t.Fatalf("C8: fault #%d of %d writes was never injected (first err %v)\nwrites=%v", faultAt, nWrites, firstErr, faulty.writes)
		}
		faulty.mu.Lock()
		faulty.faultAt = 0
		faulty.mu.Unlock()
		if err := c.ReconcileInterfaces(context.Background(), propDev, w.NICs, false); err != nil {
			t.Fatalf("C8: clean rerun after fault #%d failed: %v (first err %v)\nwrites=%v",
				faultAt, err, firstErr, faulty.writes)
		}
		if got := canonical(faulty.rows(propDev), seeded); got != want {
			t.Fatalf("C8: faulted+rerun state differs from fault-free state (first err %v)\nwant:\n%s\ngot:\n%s\nwrites=%v",
				firstErr, want, got, faulty.writes)
		}
	})
}
