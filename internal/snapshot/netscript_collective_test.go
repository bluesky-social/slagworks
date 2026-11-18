package snapshot

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/bluesky-social/slagworks/internal/configscript"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// TestNetworkScripts_MatchIffTarget is the Phase 1.4 collective regression test
// and the gate for the Phase 1 re-key: it loads EVERY real network
// script from assets/network/ and asserts that each representative host matches
// exactly its intended script — and that no other script also matches it.
//
// This is a cross-script property the per-script tests can't cover: re-keying
// one script can silently make it overlap another. Crucially it asserts the
// CANDIDATE SET (every script whose rules individually match the host), not
// just configscript.Match's winner — because Match only flags a SAME-priority
// tie; an overlap at different priorities resolves silently to the higher
// script and a winner-only check would stay green. The candidate-set check is
// what actually proves "matches iff against its target".
//
// The storage analog lives in internal/storage (different resolver/inputs).

// netDevice is a minimal NetBox identity for match testing: the fields the
// network resolver reads (matchNetworkScript). rack/u are unset here (no
// shipped script keys on them yet) but the struct carries them when needed.
type netDevice struct {
	name       string
	site       string
	location   string
	deviceType string
}

func (d netDevice) device() netbox.Device {
	dev := netbox.Device{Name: d.name}
	if d.site != "" {
		dev.Site = &netbox.NestedRef{Slug: d.site}
	}
	if d.location != "" {
		dev.Location = &netbox.NestedRef{Slug: d.location}
	}
	if d.deviceType != "" {
		dev.DeviceType = &netbox.NestedRef{Slug: d.deviceType}
	}
	return dev
}

func loadNetworkScripts(t *testing.T) []configscript.Script {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", "network")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read assets/network: %v", err)
	}
	var scripts []configscript.Script
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		s, err := ParseNetworkScript(e.Name(), body)
		if err != nil {
			t.Fatalf("parse %s (real shipped script must parse under the network vocabulary): %v", e.Name(), err)
		}
		scripts = append(scripts, *s)
	}
	if len(scripts) == 0 {
		t.Fatal("no network scripts found in assets/network")
	}
	return scripts
}

// candidateNetworkScripts returns the names of EVERY script whose rules match
// dev (evaluated individually, so different-priority overlaps are caught).
func candidateNetworkScripts(t *testing.T, scripts []configscript.Script, dev netbox.Device) []string {
	t.Helper()
	var hits []string
	for i := range scripts {
		got := matchNetworkScript([]configscript.Script{scripts[i]}, dev)
		if got != nil {
			hits = append(hits, got.Name)
		}
	}
	sort.Strings(hits)
	return hits
}

func TestNetworkScripts_MatchIffTarget(t *testing.T) {
	scripts := loadNetworkScripts(t)

	// fixtures: each representative host -> the one script it should match, or
	// "" for no match. The expected names are the asset filenames.
	type fixture struct {
		desc           string
		dev            netDevice
		want           string   // "" == no match
		wantCandidates []string // nil means exactly want; overrides exercise priority
	}
	fixtures := []fixture{
		{
			desc: "pop1 L3 kworker",
			dev:  netDevice{name: "kworker-l34-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1115hs-tnr"},
			want: "100-pop1-l3-kworker",
		},
		{
			desc: "pop1 SN2700 switch (mellanox-sn2700 slug)",
			dev:  netDevice{name: "leaf-3.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2700"},
			want: "200-sn2700-switch",
		},
		{
			desc: "pop1 SN2700-CS2FO variant (leaf-4) — same family regex",
			dev:  netDevice{name: "leaf-4.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2700-cs2fo"},
			want: "200-sn2700-switch",
		},
		{
			desc: "pop3 SN2700 leaf (site-agnostic script covers all pops)",
			dev:  netDevice{name: "leaf-1.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "mellanox-sn2700"},
			want: "200-sn2700-switch",
		},
		{
			desc:           "pop2 new-cage SN2700 uses the location-specific OOB /23 override",
			dev:            netDevice{name: "leaf-3.pop2", site: "pop2", location: "pop2-01-005250", deviceType: "mellanox-sn2700"},
			want:           "300-pop2-newcage-sn2700-switch",
			wantCandidates: []string{"200-sn2700-switch", "300-pop2-newcage-sn2700-switch"},
		},
		{
			desc: "pop3 oob-gw (oob-gw-1.pop3)",
			dev:  netDevice{name: "oob-gw-1.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "dell-r640"},
			want: "150-pop3-oob-gw",
		},
		{
			desc: "pop3 router (router1.pop3, MS-01)",
			dev:  netDevice{name: "router1.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "minisforum-ms-01"},
			want: "150-pop3-router",
		},
		{
			desc: "pop3 cpu host (cpu2.pop3, RS501A)",
			dev:  netDevice{name: "cpu2.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "rs501a-e12-rs4u"},
			want: "100-pop3-cpu",
		},
		{
			desc: "pop2 old-cage hot-spare",
			dev:  netDevice{name: "hot-spare-1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "supermicro-as-1015cs-tnr"},
			want: "100-hot-spare-pop2",
		},
		{
			desc: "pop2 new-cage oob-gw",
			dev:  netDevice{name: "oob-gw-3.pop2", site: "pop2", location: "pop2-01-005250", deviceType: "dell-r640"},
			want: "150-pop2-newcage-oob-gw",
		},
		{
			desc: "pop2 old-cage oob-gw",
			dev:  netDevice{name: "oob-gw-1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "dell-r640"},
			want: "150-pop2-oldcage-oob-gw",
		},
		{
			desc: "pop1 oob-gw",
			dev:  netDevice{name: "oob-gw-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "dell-r640"},
			want: "150-pop1-oob-gw",
		},
		// --- negatives ---
		{
			desc: "pop1 legacy L2 host (algo) — no slagworks script",
			dev:  netDevice{name: "algo1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-1015cs-tnr"},
			want: "",
		},
		{
			desc: "pop1 legacy L2 host (appview) — no slagworks script",
			dev:  netDevice{name: "appview1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "supermicro-as-2125hs-tnr"},
			want: "",
		},
		{
			desc: "pop2 new-cage L3 kworker",
			dev:  netDevice{name: "kworker-l34-1.pop2", site: "pop2", location: "pop2-01-005250", deviceType: "supermicro-as-1115hs-tnr"},
			want: "125-pop2-l3-kworker",
		},
		{
			desc: "pop1 SN2410 leaf must NOT match the SN2700 family regex",
			dev:  netDevice{name: "leaf-1.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "mellanox-sn2410"},
			want: "",
		},
		{
			desc: "pop2 old-cage leaf1 is categorically outside the new-cage SN2700 override",
			dev:  netDevice{name: "leaf-1.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "mellanox-sn2410"},
			want: "",
		},
		{
			desc: "cross-pop: pop1-kworker-shaped device in pop2 must not match the pop1 script",
			dev:  netDevice{name: "kworker-l34-1.pop1", site: "pop2", location: "pop2-01-005250", deviceType: "supermicro-as-1115hs-tnr"},
			want: "",
		},
		{
			desc: "new-cage oob-gw hostname in the WRONG (old) cage must not match EITHER oob-gw script (location is new-cage's key; hostname oob-gw-3 is not [12] for old-cage)",
			dev:  netDevice{name: "oob-gw-3.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "dell-r640"},
			want: "",
		},
		{
			desc: "old-cage oob-gw hostname (oob-gw-1) in the new cage matches NOTHING: old-cage script's location pin excludes it, AND the new-cage script's hostname backstop is /oob-gw-[34]/ so oob-gw-1 fails safe there too",
			dev:  netDevice{name: "oob-gw-1.pop2", site: "pop2", location: "pop2-01-005250", deviceType: "dell-r640"},
			want: "",
		},
		{
			desc: "old-cage oob-gw must NOT match the new-cage script (different location)",
			dev:  netDevice{name: "oob-gw-2.pop2", site: "pop2", location: "pop2-01-005330", deviceType: "dell-r640"},
			want: "150-pop2-oldcage-oob-gw",
		},
		{
			desc: "pop1 oob-gw must NOT match either pop2 oob-gw script (site=pop1 scopes it; dell-r640 is shared across pops)",
			dev:  netDevice{name: "oob-gw-2.pop1", site: "pop1", location: "pop1-02-004730", deviceType: "dell-r640"},
			want: "150-pop1-oob-gw",
		},
		{
			desc: "pop2 oob-gw hostname in pop1 site must NOT match the pop1 script (hostname backstop is .pop1)",
			dev:  netDevice{name: "oob-gw-3.pop2", site: "pop1", location: "pop1-02-004730", deviceType: "dell-r640"},
			want: "",
		},
		{
			desc: "pop3 RS501A that is NOT a cpuN host must NOT match (hostname regex excludes gpu1.pop3)",
			dev:  netDevice{name: "gpu1.pop3", site: "pop3", location: "pop3-01-412290", deviceType: "rs501a-e12-rs4u"},
			want: "",
		},
		{
			desc: "pop2 RS501A-shaped cpu host must NOT match the pop3 script (site excludes)",
			dev:  netDevice{name: "cpu1.pop2", site: "pop2", location: "pop2-01-005250", deviceType: "rs501a-e12-rs4u"},
			want: "",
		},
	}

	// Coverage guard: every shipped script that any fixture targets is tracked;
	// fail if a shipped script has no positive fixture (so adding a new script
	// without a fixture fails CI). Explicit exemptions go in noPositiveFixture.
	noPositiveFixture := map[string]string{
		// (none — all shipped scripts have a positive fixture)
	}

	covered := map[string]bool{}
	for _, f := range fixtures {
		dev := f.dev.device()
		cands := candidateNetworkScripts(t, scripts, dev)

		// Candidate-set assertion: the "matches iff" core.
		wantSet := f.wantCandidates
		if wantSet == nil && f.want != "" {
			wantSet = []string{f.want}
			covered[f.want] = true
		}
		if f.want != "" {
			covered[f.want] = true
		}
		if !equalStrings(cands, wantSet) {
			t.Errorf("%s: candidate set = %v, want %v", f.desc, cands, wantSet)
		}

		// Winner assertion on top, including the no-ambiguous-tie guarantee.
		got := matchNetworkScript(scripts, dev)
		gotName := ""
		if got != nil {
			gotName = got.Name
		}
		if gotName != f.want {
			t.Errorf("%s: Match winner = %q, want %q", f.desc, gotName, f.want)
		}
	}

	// Every shipped script must have a positive fixture or an explicit exemption.
	for i := range scripts {
		name := scripts[i].Path
		if covered[name] {
			continue
		}
		if reason, ok := noPositiveFixture[name]; ok {
			t.Logf("script %q has no positive fixture: %s", name, reason)
			continue
		}
		t.Errorf("shipped network script %q has no positive fixture in this test (add one, or an explicit noPositiveFixture exemption)", name)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
