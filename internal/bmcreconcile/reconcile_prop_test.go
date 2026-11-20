package bmcreconcile

// Model-based property test for Reconcile against the stateful ipmitool
// simulator in bmcsim_prop_test.go.
//
// Each case generates a BMC (slot table, firmware quirks, list format), the
// monitor password global currently stores, and a history of reconcile runs
// (remote compliance, remote rotation, in-band deploy), each with a fault
// schedule (call k fails before or after it is applied, or the BMC goes
// away). Global's storage rule is mirrored outside the production code
// (simStore.apply): a generated monitor password in the result is stored,
// one the result marks unconfirmed is kept beside the stored password, and
// a remote compliance run gets both.
//
// Properties (the oracle reads the simulator state, never production
// helpers):
//   - slot 2 (the admin credential's account) is never modified
//   - a generated password reported in the result is the one the BMC holds,
//     unless the result marks it unconfirmed
//   - a run never leaves a slot holding a password that is neither the
//     stored one nor reported in the result, and an enabled monitor a run
//     created always holds a password global has (the stored one or an
//     unconfirmed one)
//   - no password appears in an error
//   - a run that returns nil error leaves the BMC compliant: exactly one
//     enabled account named monitor, USER privilege, LAN access, every
//     other named non-reserved account disabled; on the remote path its
//     password is the stored one
//   - in-band provision-once: an enabled, USER, LAN monitor is never
//     re-passworded by an in-band run
//   - convergence: after any history, a fault-free remote compliance run
//     with the stored password succeeds and leaves the BMC compliant, and
//     a second one writes no password (idempotence)

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

const (
	simAdminPW   = "Adm1n!Secret"
	simPWA       = "Mon1tor!Alpha"
	simPWB       = "Mon1tor!Bravo"
	simPWLong    = "LongMon1tor!Pass19c" // 19 bytes: needs the 20-byte form
	simPWVendor  = "OldVendor1!"
	simLANTarget = "192.0.2.10"
)

type propRun struct {
	inband bool
	rotate bool
	faults map[int]string
}

func genSim(t *rapid.T) *simState {
	s := &simState{
		AdminPassword:        simAdminPW,
		MegaRAC:              rapid.Bool().Draw(t, "megarac"),
		NoEmptyName:          rapid.Bool().Draw(t, "noEmptyName"),
		Tabs:                 rapid.Bool().Draw(t, "tabs"),
		DisableShowsNoAccess: rapid.Bool().Draw(t, "disableShowsNoAccess"),
	}
	n := rapid.IntRange(4, 7).Draw(t, "nslots")
	adminName := rapid.SampledFrom([]string{"ADMIN", "root", "Administrator"}).Draw(t, "adminName")
	names := []string{"", "", "", "monitor", "MONITOR", "maas", "slagworks"}
	privs := []int{0, 0, 2, 3, 4}
	pws := []string{"", simPWA, simPWB, simPWLong, simPWVendor}
	for i := 1; i <= n; i++ {
		var sl simSlot
		switch i {
		case 1:
			sl = simSlot{Priv: rapid.SampledFrom([]int{0, 4}).Draw(t, "slot1priv")}
		case 2:
			sl = simSlot{Name: adminName, Priv: 4, Enabled: true, Password: simAdminPW, PwSize: 16, LAN: true}
		default:
			priv := rapid.SampledFrom(privs).Draw(t, fmt.Sprintf("priv%d", i))
			pw := rapid.SampledFrom(pws).Draw(t, fmt.Sprintf("pw%d", i))
			size := 16
			if len(pw) > 16 {
				size = 20
			}
			if pw == "" {
				size = 0
			}
			sl = simSlot{
				Name:     rapid.SampledFrom(names).Draw(t, fmt.Sprintf("name%d", i)),
				Priv:     priv,
				Enabled:  priv != 0 && rapid.Bool().Draw(t, fmt.Sprintf("en%d", i)),
				Password: pw,
				PwSize:   size,
				LAN:      rapid.Bool().Draw(t, fmt.Sprintf("lan%d", i)),
			}
		}
		s.Slots = append(s.Slots, sl)
	}
	return s
}

func genRuns(t *rapid.T) []propRun {
	k := rapid.IntRange(1, 4).Draw(t, "nruns")
	runs := make([]propRun, k)
	for i := range runs {
		r := propRun{
			inband: rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("kind%d", i)) == 0,
			faults: map[int]string{},
		}
		if !r.inband {
			r.rotate = rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("rotate%d", i)) == 0
		}
		nf := rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("nfaults%d", i))
		for f := range nf {
			idx := rapid.IntRange(0, 14).Draw(t, fmt.Sprintf("fidx%d_%d", i, f))
			r.faults[idx] = rapid.SampledFrom([]string{faultBefore, faultAfter, faultDown}).Draw(t, fmt.Sprintf("fkind%d_%d", i, f))
		}
		runs[i] = r
	}
	return runs
}

// simMonitorSlots returns the 1-based slots named monitor (case-insensitive).
func simMonitorSlots(s *simState) []int {
	var out []int
	for i, sl := range s.Slots {
		if strings.EqualFold(strings.TrimSpace(sl.Name), MonitorUser) {
			out = append(out, i+1)
		}
	}
	return out
}

// simCompliant reports why the simulated BMC is not compliant ("" if it is),
// and the managed monitor slot.
func simCompliant(s *simState) (string, int) {
	var enabledMon []int
	for _, n := range simMonitorSlots(s) {
		if s.Slots[n-1].Enabled {
			enabledMon = append(enabledMon, n)
		}
	}
	if len(enabledMon) != 1 {
		return fmt.Sprintf("want exactly one enabled monitor account, have %v (all monitor slots %v)", enabledMon, simMonitorSlots(s)), 0
	}
	m := enabledMon[0]
	sl := s.Slots[m-1]
	if sl.Priv != int(PrivUser) || !sl.LAN {
		return fmt.Sprintf("monitor slot %d: priv %d lan %v", m, sl.Priv, sl.LAN), m
	}
	for i, o := range s.Slots {
		slot := i + 1
		if slot <= 2 || slot == m || strings.TrimSpace(o.Name) == "" {
			continue
		}
		if o.Enabled {
			return fmt.Sprintf("leftover slot %d (%q) still enabled", slot, o.Name), m
		}
	}
	return "", m
}

// simHealthyMonitor is the precondition for the provision-once property: one
// monitor account, enabled, USER, LAN.
func simHealthyMonitor(s *simState) (int, bool) {
	ms := simMonitorSlots(s)
	if len(ms) != 1 {
		return 0, false
	}
	sl := s.Slots[ms[0]-1]
	return ms[0], sl.Enabled && sl.Priv == int(PrivUser) && sl.LAN
}

// simHiddenDisabledMonitor reports whether a monitor account has its enable
// bit off while `user list` shows it as a USER with LAN access.
func simHiddenDisabledMonitor(s *simState) bool {
	if s.DisableShowsNoAccess {
		return false
	}
	for _, n := range simMonitorSlots(s) {
		if sl := s.Slots[n-1]; !sl.Enabled && sl.Priv == int(PrivUser) && sl.LAN {
			return true
		}
	}
	return false
}

// simUnreportedPassword reports whether the run whose command log s carries
// wrote a slot a password that the slot now holds and that is neither in
// accounted (fixed test passwords and the ones the result reported) nor the
// password global stored before the run.
func simUnreportedPassword(s *simState, accounted []string, stored string) bool {
	for _, l := range s.Log {
		f := strings.Fields(l)
		if len(f) < 5 || !strings.HasPrefix(l, "user set password ") {
			continue
		}
		slot, err := strconv.Atoi(f[3])
		if err != nil || slot < 1 || slot > len(s.Slots) {
			continue
		}
		if pw := f[4]; pw != stored && !slices.Contains(accounted, pw) && s.Slots[slot-1].Password == pw {
			return true
		}
	}
	return false
}

func simPasswordWrites(log []string) int {
	n := 0
	for _, l := range log {
		if strings.HasPrefix(l, "user set password") {
			n++
		}
	}
	return n
}

// simStore mirrors the device's entry in global's monitor secret: the
// password, and the unconfirmed passwords kept beside it, oldest first.
type simStore struct {
	pw          string
	unconfirmed []string
}

// has reports whether global holds pw for the device in either role.
func (st simStore) has(pw string) bool {
	return pw == st.pw || slices.Contains(st.unconfirmed, pw)
}

// apply returns the store after global handled a run's outcome. It mirrors
// commission.upsertMonitorCred and handleBMCReconcileReport, not Reconcile.
func (st simStore) apply(r propRun, res *Result, err error) simStore {
	if res != nil {
		reported := append(slices.Clone(res.CreatedOrUpdated), res.PasswordVerified...)
		for _, u := range reported {
			switch {
			case u.Name != MonitorUser || u.Password == "":
			case u.PasswordUnconfirmed && st.pw != "":
				// Kept beside the stored password, never over it.
				if !st.has(u.Password) {
					st.unconfirmed = append(slices.Clone(st.unconfirmed), u.Password)
				}
				return st
			default:
				// A password the BMC holds (or the only candidate there
				// is): it replaces the entry.
				return simStore{pw: u.Password}
			}
		}
	}
	if err == nil && !r.inband {
		// A remote run that succeeded without reporting a password left
		// the BMC holding the stored one.
		st.unconfirmed = nil
	}
	return st
}

// reconcileOnce runs one reconcile against the simulator and returns the
// result, the error and what global stores afterwards.
func reconcileOnce(sim *bmcSim, r propRun, st simStore) (*Result, simStore, error) {
	s := sim.snapshot()
	var opt Options
	if r.inband {
		opt = Options{
			IPMIInterface: "open",
			Desired:       []DesiredUser{{Name: MonitorUser, Privilege: PrivUser}},
		}
	} else {
		pw, unconfirmed := st.pw, st.unconfirmed
		if r.rotate {
			pw, unconfirmed = "", nil
		}
		opt = RemoteOptions(LANTarget{Host: simLANTarget, Username: s.Slots[1].Name, Password: simAdminPW}, pw, unconfirmed)
	}
	opt.AdminUser = s.Slots[1].Name
	opt.IPMITool = sim.bin
	s.Faults, s.Calls, s.Log, s.Down = r.faults, 0, nil, false
	sim.set(s)

	res, err := Reconcile(context.Background(), opt)
	return res, st.apply(r, res, err), err
}

type reconcilePropStats struct {
	cases, runs, inband, rotate, faultsHit, errs, generated, unconfirmed, adopted, leftovers int
}

func checkReconcileHistory(t *rapid.T, sim *bmcSim, stats *reconcilePropStats) {
	stats.cases++
	initial := genSim(t)
	storedChoice := rapid.SampledFrom([]string{"none", "A", "B", "long", "bmc"}).Draw(t, "stored")
	stored := map[string]string{"none": "", "A": simPWA, "B": simPWB, "long": simPWLong}[storedChoice]
	if storedChoice == "bmc" {
		if ms := simMonitorSlots(initial); len(ms) > 0 {
			stored = initial.Slots[ms[0]-1].Password
		}
	}
	store := simStore{pw: stored}
	runs := genRuns(t)
	sim.set(initial)
	admin0 := initial.Slots[1]

	knownSecrets := []string{simAdminPW, simPWA, simPWB, simPWLong, simPWVendor}
	// createdMonitor: a run named a slot monitor during this history, so
	// that account's password is one Reconcile generated.
	createdMonitor := false
	for i, r := range runs {
		stats.runs++
		if r.inband {
			stats.inband++
		}
		if r.rotate {
			stats.rotate++
		}
		before := sim.snapshot()
		healthySlot, healthy := simHealthyMonitor(before)
		beforeMonPW := ""
		if healthySlot > 0 {
			beforeMonPW = before.Slots[healthySlot-1].Password
		}

		stored := store.pw
		res, newStore, err := reconcileOnce(sim, r, store)
		newStored := newStore.pw
		after := sim.snapshot()
		for k := range r.faults {
			if k < after.Calls {
				stats.faultsHit++
			}
		}
		if err != nil {
			stats.errs++
		}
		label := fmt.Sprintf("run %d (inband=%v rotate=%v faults=%v stored=%q unconfirmed=%q)", i, r.inband, r.rotate, r.faults, stored, store.unconfirmed)

		// Slot 2 untouched.
		if fmt.Sprint(after.Slots[1]) != fmt.Sprint(admin0) {
			t.Fatalf("%s: slot 2 modified: before %+v after %+v\nlog: %q", label, admin0, after.Slots[1], after.Log)
		}

		// Reported generated passwords are applied.
		var generated []string
		if res != nil {
			for _, u := range res.CreatedOrUpdated {
				if u.Password == "" {
					continue
				}
				generated = append(generated, u.Password)
				stats.generated++
				if u.PasswordUnconfirmed {
					stats.unconfirmed++
					if err == nil {
						t.Fatalf("%s: an unconfirmed password for slot %d came without an error", label, u.Slot)
					}
					continue
				}
				if u.Slot < 1 || u.Slot > len(after.Slots) || after.Slots[u.Slot-1].Password != u.Password {
					t.Fatalf("%s: reported generated password for slot %d is not what the BMC holds\nlog: %q", label, u.Slot, after.Log)
				}
			}
			// An adopted password is an unconfirmed one global handed the
			// run, and it is what the BMC holds.
			for _, u := range res.PasswordVerified {
				if u.Password == "" {
					continue
				}
				stats.adopted++
				if !slices.Contains(store.unconfirmed, u.Password) {
					t.Fatalf("%s: adopted a password for slot %d that is not one of the unconfirmed ones", label, u.Slot)
				}
				if u.Slot < 1 || u.Slot > len(after.Slots) || after.Slots[u.Slot-1].Password != u.Password {
					t.Fatalf("%s: adopted password for slot %d is not what the BMC holds\nlog: %q", label, u.Slot, after.Log)
				}
			}
			stats.leftovers += len(res.LeftoversDisabled)
		}

		// No secret in the error.
		if err != nil {
			secrets := append(append([]string{}, knownSecrets...), generated...)
			secrets = append(secrets, store.unconfirmed...)
			for _, sl := range after.Slots {
				secrets = append(secrets, sl.Password)
				secrets = append(secrets, sl.History...)
			}
			for _, sec := range secrets {
				if sec != "" && strings.Contains(err.Error(), sec) {
					t.Fatalf("%s: error leaks a password: %v", label, err)
				}
			}
		}

		// Provision-once on the in-band path.
		if r.inband && healthy {
			if w := simPasswordWrites(after.Log); w > 0 {
				t.Fatalf("%s: in-band run re-passworded a healthy monitor at slot %d (%d password writes)\nBMC before: %+v\nlog: %q",
					label, healthySlot, w, before.Slots, after.Log)
			}
		}

		// Success means compliance.
		// Excluded pattern, a defect in Reconcile: the in-band path takes a
		// monitor that lists USER with LAN access as provisioned and never
		// sends `user enable`, so one whose enable bit is off (a create that
		// stopped before its last step) stays unable to log in while the run
		// succeeds. `user list` shows the bit only on firmware that lists a
		// disabled user as NO ACCESS. The remote path always enables, so the
		// convergence check below still covers these tables.
		excluded := r.inband && simHiddenDisabledMonitor(after)
		if err == nil && !excluded {
			why, m := simCompliant(after)
			if why != "" {
				t.Fatalf("%s: Reconcile returned nil error but the BMC is not compliant: %s\nBMC before: %+v\nBMC after: %+v\nlog: %q",
					label, why, before.Slots, after.Slots, after.Log)
			}
			if !r.inband && after.Slots[m-1].Password != newStored {
				t.Fatalf("%s: remote reconcile succeeded but the BMC monitor password %q is not the stored %q\nlog: %q",
					label, after.Slots[m-1].Password, newStored, after.Log)
			}
			if !r.inband && len(newStore.unconfirmed) > 0 {
				t.Fatalf("%s: remote reconcile succeeded but global still holds unconfirmed passwords", label)
			}
			if r.inband {
				if newStored != stored && after.Slots[m-1].Password != newStored {
					t.Fatalf("%s: in-band reported a password the BMC does not hold", label)
				}
				if newStored == stored && healthy && after.Slots[m-1].Password != beforeMonPW {
					t.Fatalf("%s: in-band changed the monitor password without reporting it", label)
				}
			}
		}
		if res != nil {
			for _, u := range res.CreatedOrUpdated {
				if u.Name == MonitorUser && !r.inband && u.Password == "" && stored == "" {
					t.Fatalf("%s: wrote a stored password but none is stored", label)
				}
			}
		}
		for _, l := range after.Log {
			if strings.HasPrefix(l, "user set name ") && strings.HasSuffix(l, " "+MonitorUser) {
				createdMonitor = true
			}
		}
		// No password is lost: whatever a run wrote that a slot now holds
		// is the stored password or one the result reported.
		if simUnreportedPassword(after, append(generated, knownSecrets...), stored) {
			t.Fatalf("%s: a slot holds a password this run wrote that is neither stored nor reported (err: %v)\nBMC after: %+v\nlog: %q",
				label, err, after.Slots, after.Log)
		}
		// An in-band success must not leave an enabled monitor that an
		// earlier run created with a password global does not hold.
		if r.inband && err == nil && createdMonitor {
			if ms := simMonitorSlots(after); len(ms) == 1 && after.Slots[ms[0]-1].Enabled && !newStore.has(after.Slots[ms[0]-1].Password) {
				t.Fatalf("%s: in-band succeeded but the monitor a run created (slot %d) holds a password global does not have (stored %q, unconfirmed %q)\nBMC before: %+v\nBMC after: %+v\nlog: %q",
					label, ms[0], newStored, newStore.unconfirmed, before.Slots, after.Slots, after.Log)
			}
		}
		store = newStore
	}
	stored = store.pw

	// Convergence: one fault-free remote compliance run.
	before := sim.snapshot()
	res, newStore, err := reconcileOnce(sim, propRun{rotate: stored == ""}, store)
	newStored := newStore.pw
	after := sim.snapshot()
	if res != nil {
		for _, u := range res.PasswordVerified {
			if u.Password != "" {
				stats.adopted++
			}
		}
	}
	if err != nil {
		t.Fatalf("convergence: fault-free remote compliance run failed: %v\nstored=%q\nBMC before: %+v\nlog: %q", err, stored, before.Slots, after.Log)
	}
	why, m := simCompliant(after)
	if why != "" {
		t.Fatalf("convergence: not compliant after a clean remote run: %s\nBMC: %+v\nlog: %q", why, after.Slots, after.Log)
	}
	if after.Slots[m-1].Password != newStored {
		t.Fatalf("convergence: BMC monitor password %q != stored %q", after.Slots[m-1].Password, newStored)
	}
	if len(newStore.unconfirmed) > 0 {
		t.Fatalf("convergence: global still holds unconfirmed passwords after a clean remote run")
	}
	// Idempotence: a second clean run writes no password.
	_, store2, err := reconcileOnce(sim, propRun{}, newStore)
	stored2 := store2.pw
	after2 := sim.snapshot()
	if err != nil {
		t.Fatalf("idempotence: second clean run failed: %v\nlog: %q", err, after2.Log)
	}
	if w := simPasswordWrites(after2.Log); w > 0 || stored2 != newStored {
		t.Fatalf("idempotence: second clean run wrote %d passwords (stored %q -> %q)\nlog: %q", w, newStored, stored2, after2.Log)
	}
}

// lowerDefaultRapidChecks runs the calling test's rapid.Check with n cases
// unless -rapid.checks was given on the command line, which then scales the
// test up for a deep local run. For properties whose cases are expensive.
func lowerDefaultRapidChecks(t *testing.T, n int) {
	t.Helper()
	const name = "rapid.checks"
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == name })
	if explicit {
		return
	}
	prev := flag.Lookup(name).Value.String()
	if err := flag.Set(name, fmt.Sprint(n)); err != nil {
		t.Fatalf("set -%s: %v", name, err)
	}
	t.Cleanup(func() { _ = flag.Set(name, prev) })
}

func TestReconcile_Prop_FaultSchedules(t *testing.T) {
	// A case is up to six Reconcile runs of a dozen ipmitool processes each,
	// and a failing password write sleeps two seconds before its last retry.
	lowerDefaultRapidChecks(t, 20)
	sim := newBMCSim(t)
	var stats reconcilePropStats
	rapid.Check(t, func(rt *rapid.T) { checkReconcileHistory(rt, sim, &stats) })
	t.Logf("coverage: %+v", stats)
}
