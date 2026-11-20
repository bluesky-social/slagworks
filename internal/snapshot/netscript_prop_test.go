package snapshot

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v2"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Property-based coverage for the shipped network-config scripts, run
// through the real python3 interpreter with generated facts (the same
// SLAGWORKS_NETWORK_FACTS contract the agent uses). Facts are generated as
// model.SnapshotInterface (the exact wire shape Build emits) from small
// pools so legs, loopbacks, bond members, missing MACs and stray interfaces
// collide.
//
// Oracles (independent of the scripts' code):
//   - the script exits 0 exactly when the reference model says the host is
//     fully modeled, and non-zero otherwise (no silent fallback);
//   - on success the netplan parses as YAML, and every address, MAC and
//     member the model expects appears exactly where expected and nothing
//     else does (ethernets/bonds key sets, match-by-MAC of the right
//     interface, addresses, routes, MTU);
//   - FRR: every BGP neighbor's remote-as is the leaf ASN for that leg's
//     own /31 subnet;
//   - permuting the facts array does not change the output bytes.
//
// Every case costs an interpreter start or two, so the properties that take
// a script name draw it per case instead of running the full check count
// against each script; -rapid.checks scales the depth up for a local run.

func scriptPath(t tb, name string) string {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", "network", name)
}

// tb is the slice of testing.TB / *rapid.T the script runner needs.
type tb interface {
	Fatalf(format string, args ...any)
}

// requirePython skips a script property where the interpreter is absent
// (it is present on CI and dev laptops).
func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

type scriptRun struct {
	stdout, stderr string
	exit           int
}

func runNetScript(t tb, script string, facts []model.SnapshotInterface) scriptRun {
	if facts == nil {
		facts = []model.SnapshotInterface{}
	}
	b, err := json.Marshal(facts)
	if err != nil {
		t.Fatalf("marshal facts: %v", err)
	}
	f, err := os.CreateTemp("", "facts-*.json")
	if err != nil {
		t.Fatalf("tempfile: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		t.Fatalf("write facts: %v", err)
	}
	f.Close()
	wd, _ := os.Getwd()
	cmd := exec.Command("python3", scriptPath(t, script))
	cmd.Env = append(os.Environ(),
		"SLAGWORKS_NETWORK_FACTS="+f.Name(),
		"PYTHONPATH="+filepath.Join(filepath.Dir(filepath.Dir(wd)), "internal", "agent", "netlib"))
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	r := scriptRun{}
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run %s: %v", script, err)
		}
		r.exit = ee.ExitCode()
	}
	r.stdout, r.stderr = out.String(), errb.String()
	return r
}

func parseBundle(t *rapid.T, r scriptRun) (netBundle, map[any]any) {
	var b netBundle
	if err := json.Unmarshal([]byte(r.stdout), &b); err != nil {
		t.Fatalf("bundle JSON: %v\n%s", err, r.stdout)
	}
	var np map[any]any
	for _, f := range b.Files {
		if f.Path == "/etc/netplan/99-slagworks.yaml" {
			if err := yaml.Unmarshal([]byte(f.Content), &np); err != nil {
				t.Fatalf("netplan does not parse as YAML: %v\n%s", err, f.Content)
			}
		}
	}
	if np == nil {
		t.Fatalf("no netplan in bundle")
	}
	return b, np
}

func ymap(t *rapid.T, v any, path string) map[any]any {
	m, ok := v.(map[any]any)
	if !ok {
		t.Fatalf("%s: want mapping, got %T (%v)", path, v, v)
	}
	return m
}

func keysOf(m map[any]any) []string {
	var out []string
	for k := range m {
		out = append(out, fmt.Sprint(k))
	}
	sort.Strings(out)
	return out
}

func shuffled(t *rapid.T, facts []model.SnapshotInterface) []model.SnapshotInterface {
	out := append([]model.SnapshotInterface(nil), facts...)
	for i := len(out) - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("shuf%d", i))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---------------- L2 bond (hot-spare) ----------------

var cvBondOK, cvBondFail, cvBondMembers atomic.Int64

func TestProp_HotSpareBondScript(t *testing.T) {
	requirePython(t)
	rapid.Check(t, func(t *rapid.T) {
		var facts []model.SnapshotInterface
		id := 1
		bondNames := []string{"bond0", "bond1"}
		nBond := rapid.SampledFrom([]int{0, 1, 1, 1, 1, 1, 2}).Draw(t, "nBond")
		for i := 0; i < nBond; i++ {
			f := model.SnapshotInterface{NetBoxID: id, Name: bondNames[i], Type: "lag"}
			id++
			if rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("b%dnoip", i)) != 0 {
				f.IP = rapid.SampledFrom([]string{"10.253.0.22/22", "10.253.0.22/22", "10.253.1.9/24", "2001:db8::22/64", "not-a-cidr"}).Draw(t, fmt.Sprintf("b%dip", i))
			}
			if rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("b%dnogw", i)) != 0 {
				f.Gateway = rapid.SampledFrom([]string{"10.253.3.254", "10.253.1.1"}).Draw(t, fmt.Sprintf("b%dgw", i))
			}
			f.MTU = rapid.SampledFrom([]int{0, 1500, 9000}).Draw(t, fmt.Sprintf("b%dmtu", i))
			f.VLAN = rapid.SampledFrom([]int{0, 1, 100}).Draw(t, fmt.Sprintf("b%dvlan", i))
			facts = append(facts, f)
		}
		nPhys := rapid.SampledFrom([]int{0, 1, 2, 2, 2, 3, 4}).Draw(t, "nPhys")
		physNames := []string{"enp129s0f0np0", "enp129s0f1np1", "eno1", "eno2"}
		macs := []string{"7C:C2:55:42:FC:30", "7c:c2:55:42:fc:31", "aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02"}
		for i := 0; i < nPhys; i++ {
			f := model.SnapshotInterface{NetBoxID: id, Name: physNames[i], Type: "physical"}
			id++
			if rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("p%dnomac", i)) != 0 {
				f.MAC = macs[i]
			}
			f.LAG = rapid.SampledFrom([]string{"", "bond0", "bond0", "bond1"}).Draw(t, fmt.Sprintf("p%dlag", i))
			f.MTU = rapid.SampledFrom([]int{0, 1500, 9000}).Draw(t, fmt.Sprintf("p%dmtu", i))
			facts = append(facts, f)
		}
		if rapid.Bool().Draw(t, "ipmi") {
			facts = append(facts, model.SnapshotInterface{NetBoxID: id, Name: "ipmi0", MAC: "3c:ec:ef:de:d0:89", Type: "ipmi", IP: "10.253.241.108/23"})
		}

		// Reference model.
		var bonds []model.SnapshotInterface
		for _, f := range facts {
			if f.Type == "lag" && f.IP != "" {
				bonds = append(bonds, f)
			}
		}
		wantOK := len(bonds) == 1
		var bond model.SnapshotInterface
		type mem struct{ name, mac string }
		var members []mem
		if wantOK {
			bond = bonds[0]
			if _, err := netip.ParsePrefix(bond.IP); err != nil {
				wantOK = false
			}
			if bond.Gateway == "" {
				wantOK = false
			}
			for _, f := range facts {
				if f.LAG == bond.Name && f.MAC != "" {
					members = append(members, mem{f.Name, strings.ToLower(f.MAC)})
				}
			}
			if len(members) == 0 {
				wantOK = false
			}
		}
		r := runNetScript(t, "100-hot-spare-pop2", facts)
		if (r.exit == 0) != wantOK {
			t.Fatalf("exit=%d, model expects ok=%v\nfacts=%+v\nstderr=%s", r.exit, wantOK, facts, r.stderr)
		}
		if !wantOK {
			cvBondFail.Add(1)
			return
		}
		cvBondOK.Add(1)
		cvBondMembers.Add(int64(len(members)))
		b, np := parseBundle(t, r)
		if len(b.EnableUnits) != 0 || len(b.Files) != 1 {
			t.Fatalf("unexpected bundle shape %+v", b)
		}
		netw := ymap(t, np["network"], "network")
		eth := ymap(t, netw["ethernets"], "ethernets")
		var wantNames []string
		for _, m := range members {
			wantNames = append(wantNames, m.name)
		}
		sort.Strings(wantNames)
		if got := keysOf(eth); !equalStrings(got, wantNames) {
			t.Fatalf("ethernets %v, want members %v", got, wantNames)
		}
		mtu := bond.MTU
		if mtu == 0 {
			mtu = 1500
		}
		for _, m := range members {
			e := ymap(t, eth[m.name], "ethernets."+m.name)
			match := ymap(t, e["match"], "match")
			if fmt.Sprint(match["macaddress"]) != m.mac || fmt.Sprint(e["set-name"]) != m.name || e["mtu"] != mtu {
				t.Fatalf("member %s stanza %v, want mac %s mtu %d", m.name, e, m.mac, mtu)
			}
		}
		bondsY := ymap(t, netw["bonds"], "bonds")
		if got := keysOf(bondsY); !equalStrings(got, []string{bond.Name}) {
			t.Fatalf("bonds %v, want [%s]", got, bond.Name)
		}
		by := ymap(t, bondsY[bond.Name], "bond")
		var ifs []string
		for _, x := range by["interfaces"].([]any) {
			ifs = append(ifs, fmt.Sprint(x))
		}
		if !equalStrings(ifs, wantNames) {
			t.Fatalf("bond interfaces %v, want %v", ifs, wantNames)
		}
		addrs := by["addresses"].([]any)
		if len(addrs) != 1 || fmt.Sprint(addrs[0]) != bond.IP {
			t.Fatalf("bond addresses %v, want [%s]", addrs, bond.IP)
		}
		routes := by["routes"].([]any)
		rt := ymap(t, routes[0], "route")
		if len(routes) != 1 || fmt.Sprint(rt["via"]) != bond.Gateway || fmt.Sprint(rt["to"]) != "default" {
			t.Fatalf("bond routes %v, want default via %s", routes, bond.Gateway)
		}
		// Order independence.
		r2 := runNetScript(t, "100-hot-spare-pop2", shuffled(t, facts))
		if r2.stdout != r.stdout || r2.exit != r.exit {
			t.Fatalf("output depends on facts order:\n%s\n---\n%s", r.stdout, r2.stdout)
		}
	})
	t.Logf("coverage: ok=%d fail=%d members=%d", cvBondOK.Load(), cvBondFail.Load(), cvBondMembers.Load())
}

// ---------------- L3 kworker ----------------

var l3Pairs = map[string]map[string][2]int{
	"100-pop1-l3-kworker": {"10.254.28": {65023, 65024}, "10.254.29": {65025, 65026}},
	"125-pop2-l3-kworker": {"10.253.28": {65023, 65024}, "10.253.29": {65025, 65026}},
}

var neighborASRE = regexp.MustCompile(`(?m)^ neighbor (\S+) remote-as (\d+)$`)

var cvL3OK, cvL3Fail, cvL3CrossPair atomic.Int64

func TestProp_L3KworkerScript(t *testing.T) {
	requirePython(t)
	rapid.Check(t, func(t *rapid.T) {
		script := rapid.SampledFrom([]string{"100-pop1-l3-kworker", "125-pop2-l3-kworker"}).Draw(t, "script")
		pairs := l3Pairs[script]
		var pfx string
		for k := range pairs {
			pfx = k[:strings.LastIndexByte(k, '.')] // "10.254"
		}
		var facts []model.SnapshotInterface
		id := 1
		// Loopback.
		switch rapid.IntRange(0, 9).Draw(t, "loKind") {
		case 0:
		case 1:
			facts = append(facts, model.SnapshotInterface{NetBoxID: id, Name: "lo", Type: "physical", IP: pfx + ".31.67/31"})
		default:
			facts = append(facts, model.SnapshotInterface{NetBoxID: id, Name: "lo", Type: "physical", IP: pfx + ".31.67/32"})
		}
		id++
		// Legs: host-side odd addresses in the two known pairs' /24s (and an
		// unknown /24). Most legs share one /24; now and then one strays
		// into another pair's.
		subnets := []string{pfx + ".28", pfx + ".29", pfx + ".30"}
		home := rapid.SampledFrom([]string{pfx + ".28", pfx + ".29", pfx + ".28", pfx + ".29", pfx + ".30"}).Draw(t, "home")
		nLeg := rapid.SampledFrom([]int{0, 1, 2, 2, 2, 2, 3, 3}).Draw(t, "nLeg")
		used := map[string]bool{}
		macs := []string{"7C:C2:55:BE:F6:0C", "7c:c2:55:be:f6:0d", "7c:c2:55:be:f6:0e"}
		for i := 0; i < nLeg; i++ {
			sn := home
			if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("leg%dcross", i)) == 0 {
				sn = rapid.SampledFrom(subnets).Draw(t, fmt.Sprintf("leg%dsn", i))
			}
			host := rapid.SampledFrom([]int{1, 3, 9, 11, 63}).Draw(t, fmt.Sprintf("leg%dhost", i))
			ip := fmt.Sprintf("%s.%d/31", sn, host)
			if used[ip] {
				continue
			}
			used[ip] = true
			f := model.SnapshotInterface{NetBoxID: id, Name: fmt.Sprintf("enp%ds0", i+1), Type: "physical", IP: ip}
			id++
			if rapid.IntRange(0, 12).Draw(t, fmt.Sprintf("leg%dnomac", i)) != 0 {
				f.MAC = macs[i]
			}
			if rapid.IntRange(0, 12).Draw(t, fmt.Sprintf("leg%dipmi", i)) == 0 {
				f.Type = "ipmi"
			}
			facts = append(facts, f)
		}
		if rapid.Bool().Draw(t, "oob") {
			facts = append(facts, model.SnapshotInterface{NetBoxID: id, Name: "ipmi0", MAC: "90:5a:08:15:6c:f2", Type: "ipmi", IP: pfx + ".241.25/22"})
		}

		// Reference model.
		type leg struct {
			mac  string
			host netip.Addr
			sn   string
		}
		var legs []leg
		lo := ""
		for _, f := range facts {
			if f.Name == "lo" && strings.HasSuffix(f.IP, "/32") {
				lo = f.IP
				continue
			}
			p, err := netip.ParsePrefix(f.IP)
			if f.Type != "physical" || err != nil || p.Bits() != 31 || f.MAC == "" {
				continue
			}
			a := p.Addr().As4()
			legs = append(legs, leg{strings.ToLower(f.MAC), p.Addr(), fmt.Sprintf("%d.%d.%d", a[0], a[1], a[2])})
		}
		sort.Slice(legs, func(i, j int) bool { return legs[i].host.Less(legs[j].host) })
		wantOK := len(legs) >= 2 && lo != ""
		if wantOK {
			for _, l := range legs[:2] {
				if _, ok := pairs[l.sn]; !ok {
					wantOK = false
				}
			}
			if legs[0].sn != legs[1].sn {
				cvL3CrossPair.Add(1)
				// The two BGP legs must land on ONE leaf pair: a host whose
				// legs sit in two pairs' subnets is a modeling error the
				// script must refuse, like an unknown subnet.
				wantOK = false
			}
		}
		r := runNetScript(t, script, facts)
		if (r.exit == 0) != wantOK {
			t.Fatalf("exit=%d, model expects ok=%v\nfacts=%+v\nstderr=%s\nstdout=%.600s", r.exit, wantOK, facts, r.stderr, r.stdout)
		}
		if !wantOK {
			cvL3Fail.Add(1)
			return
		}
		cvL3OK.Add(1)
		b, np := parseBundle(t, r)
		eth := ymap(t, ymap(t, np["network"], "network")["ethernets"], "ethernets")
		// lo + one stanza per leg.
		if len(eth) != 1+len(legs) {
			t.Fatalf("ethernets %v, want lo + %d legs", keysOf(eth), len(legs))
		}
		loY := ymap(t, eth["lo"], "lo")
		if a := loY["addresses"].([]any); len(a) != 2 || fmt.Sprint(a[1]) != lo {
			t.Fatalf("lo addresses %v, want [127.0.0.1/8 %s]", a, lo)
		}
		for i, l := range legs {
			e := ymap(t, eth[fmt.Sprintf("leg%d", i)], "leg")
			if fmt.Sprint(ymap(t, e["match"], "match")["macaddress"]) != l.mac {
				t.Fatalf("leg%d mac %v, want %s", i, e["match"], l.mac)
			}
			a := e["addresses"].([]any)
			if len(a) != 1 || fmt.Sprint(a[0]) != l.host.String()+"/31" {
				t.Fatalf("leg%d addresses %v, want %s/31", i, a, l.host)
			}
		}
		var frr string
		for _, f := range b.Files {
			if f.Path == "/etc/frr/frr.conf" {
				frr = f.Content
			}
		}
		ms := neighborASRE.FindAllStringSubmatch(frr, -1)
		if len(ms) != 2 {
			t.Fatalf("want 2 BGP neighbors, got %v", ms)
		}
		for i, m := range ms {
			l := legs[i]
			a := l.host.As4()
			a[3] ^= 1
			if m[1] != netip.AddrFrom4(a).String() {
				t.Fatalf("neighbor %d = %s, want leaf side of %s", i, m[1], l.host)
			}
			want := pairs[l.sn][i]
			if m[2] != fmt.Sprint(want) {
				t.Fatalf("neighbor %s remote-as %s, want %d (leg %s in pair %s)", m[1], m[2], want, l.host, l.sn)
			}
		}
		r2 := runNetScript(t, script, shuffled(t, facts))
		if r2.stdout != r.stdout {
			t.Fatalf("output depends on facts order")
		}
	})
	t.Logf("coverage: ok=%d fail=%d crossPairLegs=%d", cvL3OK.Load(), cvL3Fail.Load(), cvL3CrossPair.Load())
}
