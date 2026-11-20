package bmcreconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestReconcile_Prop_AdminGate: the admin credential's account must never be
// disabled or re-passworded. Reconcile refuses before any write unless the
// account named AdminUser (case-insensitive) exists exactly once, at slot 2.
// The oracle recomputes the gate from the simulated table.
func TestReconcile_Prop_AdminGate(t *testing.T) {
	sim := newBMCSim(t)
	var refused, allowed int
	rapid.Check(t, func(rt *rapid.T) {
		s := genSim(rt)
		cred := rapid.SampledFrom([]string{"ADMIN", "admin", "root", "svc"}).Draw(rt, "credName")
		if rapid.Bool().Draw(rt, "credElsewhere") {
			k := rapid.IntRange(3, len(s.Slots)).Draw(rt, "credSlot")
			s.Slots[k-1] = simSlot{
				Name: rapid.SampledFrom([]string{cred, strings.ToUpper(cred)}).Draw(rt, "credCase"),
				Priv: 4, Enabled: true, Password: simAdminPW, PwSize: 16, LAN: true,
			}
		}
		var matches []int
		for i, sl := range s.Slots {
			if strings.EqualFold(strings.TrimSpace(sl.Name), cred) {
				matches = append(matches, i+1)
			}
		}
		wantRefuse := len(matches) != 1 || matches[0] != 2
		sim.set(s)
		opt := RemoteOptions(LANTarget{Host: simLANTarget, Username: cred, Password: simAdminPW}, "", nil)
		if rapid.Bool().Draw(rt, "inband") {
			opt = Options{IPMIInterface: "open", AdminUser: cred, Desired: []DesiredUser{{Name: MonitorUser, Privilege: PrivUser}}}
		}
		opt.IPMITool = sim.bin
		_, rerr := Reconcile(context.Background(), opt)
		after := sim.snapshot()
		var gate *AdminNotSlot2Error
		isGate := errors.As(rerr, &gate)
		if wantRefuse {
			refused++
			if !isGate || len(after.Log) != 1 {
				rt.Fatalf("admin %q at slots %v: want a pre-write refusal, got err=%v log=%q", cred, matches, rerr, after.Log)
			}
		} else {
			allowed++
			if isGate {
				rt.Fatalf("admin %q correctly at slot 2 only, but refused: %v", cred, rerr)
			}
		}
		for _, m := range matches {
			if fmt.Sprint(after.Slots[m-1]) != fmt.Sprint(s.Slots[m-1]) {
				rt.Fatalf("account %q at slot %d was modified: %+v -> %+v (log %q)", cred, m, s.Slots[m-1], after.Slots[m-1], after.Log)
			}
		}
	})
	t.Logf("coverage: refused=%d allowed=%d", refused, allowed)
}
