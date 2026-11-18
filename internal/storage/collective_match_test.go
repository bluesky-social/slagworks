package storage

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestStorageScripts_MatchIffTarget is the storage half of the Phase 1.4
// collective regression test, sibling to the network one in
// internal/snapshot. It loads EVERY real storage script from assets/storage/
// and asserts each representative hardware model matches exactly its intended
// script — and no other. Like the network test, it asserts the CANDIDATE SET
// (every script whose rules individually match), not just Match's winner,
// because Match only flags a SAME-priority tie; a different-priority overlap
// (e.g. a family-regex script vs an exact-model script) resolves silently to
// the higher one and a winner-only check would miss it.
//
// Fixtures use the EXACT system_product strings the real scripts match (copied
// from the script rules, not display names) — e.g. "MSN2700" not "SN2700", and
// "AS -1115HS-TNR" (note the space) which satisfies the family regex.

func loadStorageScripts(t *testing.T) []Script {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/storage -> repo root
	dir := filepath.Join(filepath.Dir(filepath.Dir(wd)), "assets", "storage")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read assets/storage: %v", err)
	}
	var scripts []Script
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		s, err := ParseScript(e.Name(), body)
		if err != nil {
			t.Fatalf("parse %s (real shipped storage script must parse): %v", e.Name(), err)
		}
		scripts = append(scripts, *s)
	}
	if len(scripts) == 0 {
		t.Fatal("no storage scripts found in assets/storage")
	}
	return scripts
}

// candidateStorageScripts returns the names of EVERY script whose rules match
// the given commission data + identity (each evaluated individually). hostname
// is the NetBox device name the snapshot builder supplies; "" means a script
// that ANDs a hostname constraint can't match.
func candidateStorageScripts(t *testing.T, scripts []Script, manufacturer, product, hostname string) []string {
	t.Helper()
	data := commissionJSON(manufacturer, product)
	identity := map[string]string{"hostname": hostname}
	var hits []string
	for i := range scripts {
		got, err := Match([]Script{scripts[i]}, data, identity)
		if err != nil {
			t.Fatalf("match %s: %v", scripts[i].Path, err)
		}
		if got != nil {
			hits = append(hits, got.Path)
		}
	}
	sort.Strings(hits)
	return hits
}

func TestStorageScripts_MatchIffTarget(t *testing.T) {
	scripts := loadStorageScripts(t)

	type fixture struct {
		desc         string
		manufacturer string
		product      string // exact system_product the script matches
		hostname     string // NetBox device name (for scripts that AND a hostname)
		want         string // "" == no match; the Match() winner
		// wantCandidates overrides the expected candidate set when it
		// differs from []string{want} — the different-priority-overlap
		// case (e.g. a family/model script AND a hostname-scoped
		// higher-priority script both individually match; Match() picks
		// the higher priority, but the candidate set legitimately has
		// both). Leave nil to default to []string{want}.
		wantCandidates []string
	}
	fixtures := []fixture{
		{
			desc:    "ASUS RS501A",
			product: "RS501A-E12-RS4U",
			want:    "100-asus-rs501a",
		},
		{
			desc:           "ASUS RS501A on a pop3 cpu hostname (preserve script, priority 150 outranks the 100 create script)",
			product:        "RS501A-E12-RS4U",
			hostname:       "cpu2.pop3",
			want:           "150-asus-rs501a-preserve",
			wantCandidates: []string{"100-asus-rs501a", "150-asus-rs501a-preserve"},
		},
		{
			desc:     "ASUS RS501A NOT on a cpu*.pop3 hostname stays on the create script (preserve's hostname rule doesn't match)",
			product:  "RS501A-E12-RS4U",
			hostname: "gpu1.pop3",
			want:     "100-asus-rs501a",
		},
		{
			desc:    "Dell PowerEdge R640",
			product: "PowerEdge R640",
			want:    "150-dell-r640",
		},
		{
			desc:     "Supermicro 1115HS TNR kworker (family regex + allowed hostname)",
			product:  "AS -1115HS-TNR",
			hostname: "kworker-l34-1.pop1",
			want:     "200-supermicro-tnr-family",
		},
		{
			desc:           "pop2 kworker preserve script outranks destructive family layout",
			product:        "AS -1115HS-TNR",
			hostname:       "kworker-l34-1.pop2",
			want:           "250-pop2-kworker-preserve-lvm",
			wantCandidates: []string{"200-supermicro-tnr-family", "250-pop2-kworker-preserve-lvm"},
		},
		{
			desc:     "Supermicro 1015CS TNR hot-spare (other regex arm + allowed hostname)",
			product:  "AS -1015CS-TNR",
			hostname: "hot-spare-1.pop2",
			want:     "200-supermicro-tnr-family",
		},
		{
			desc:    "Mellanox SN2700 (product is MSN2700, NOT SN2700)",
			product: "MSN2700",
			want:    "300-mellanox-sn2700",
		},
		{
			desc:    "Mellanox SN2700-CS2FO SKU (pop1 leaf-4; same hardware, larger disk)",
			product: "MSN2700-CS2FO",
			want:    "300-mellanox-sn2700",
		},
		{
			desc:     "Minisforum MS-01 router (SMBIOS says 'Venus Series', not MS-01; hostname-scoped)",
			product:  "Venus Series",
			hostname: "router1.pop3",
			want:     "150-minisforum-ms-01",
		},
		// --- negatives ---
		{
			desc:     "Venus Series chassis with a non-onboarded hostname must NOT match (family string; only router1.pop3 is onboarded)",
			product:  "Venus Series",
			hostname: "router2.pop3",
			want:     "",
		},
		{
			desc:     "Venus Series chassis with NO hostname resolved (unbound machine) must NOT match",
			product:  "Venus Series",
			hostname: "",
			want:     "",
		},
		{
			desc:    "unknown product → no match",
			product: "SomeOtherBox-9000",
			want:    "",
		},
		{
			desc:    "bare 'SN2700' must NOT match (the rule is anchored ^MSN2700(-CS2FO)?$)",
			product: "SN2700",
			want:    "",
		},
		{
			desc:    "other MSN2700 suffixes must NOT match (only the CS2FO SKU is vetted)",
			product: "MSN2700-BS2FO",
			want:    "",
		},
		{
			desc:    "TNR product missing the leading 'AS -' must NOT match the anchored family regex",
			product: "1115HS-TNR",
			want:    "",
		},
		// --- the defense-in-depth cases: TNR chassis, NOT-allowed hostname ---
		{
			desc:     "nova on a 1115HS TNR chassis must NOT match (shares model+role with kworker; hostname scopes it out)",
			product:  "AS -1115HS-TNR",
			hostname: "nova1.pop1",
			want:     "",
		},
		{
			desc:     "algo on a 1015CS TNR chassis must NOT match (shares model with hot-spare; hostname scopes it out)",
			product:  "AS -1015CS-TNR",
			hostname: "algo1.pop2",
			want:     "",
		},
		{
			desc:     "TNR chassis with NO hostname resolved (unbound machine) must NOT match",
			product:  "AS -1115HS-TNR",
			hostname: "",
			want:     "",
		},
	}

	noPositiveFixture := map[string]string{}

	covered := map[string]bool{}
	for _, f := range fixtures {
		cands := candidateStorageScripts(t, scripts, f.manufacturer, f.product, f.hostname)

		wantSet := f.wantCandidates
		if wantSet == nil && f.want != "" {
			wantSet = []string{f.want}
		}
		for _, name := range wantSet {
			covered[name] = true
		}
		if f.want != "" {
			covered[f.want] = true
		}
		if !equalStringsStorage(cands, wantSet) {
			t.Errorf("%s: candidate set = %v, want %v", f.desc, cands, wantSet)
		}

		got, err := Match(scripts, commissionJSON(f.manufacturer, f.product), map[string]string{"hostname": f.hostname})
		if err != nil {
			t.Fatalf("%s: Match: %v", f.desc, err)
		}
		gotName := ""
		if got != nil {
			gotName = got.Path
		}
		if gotName != f.want {
			t.Errorf("%s: Match winner = %q, want %q", f.desc, gotName, f.want)
		}
	}

	for i := range scripts {
		name := scripts[i].Path
		if covered[name] {
			continue
		}
		if reason, ok := noPositiveFixture[name]; ok {
			t.Logf("storage script %q has no positive fixture: %s", name, reason)
			continue
		}
		t.Errorf("shipped storage script %q has no positive fixture in this test (add one, or an explicit noPositiveFixture exemption)", name)
	}
}

func equalStringsStorage(a, b []string) bool {
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
