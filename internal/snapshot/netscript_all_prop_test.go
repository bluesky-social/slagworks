package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v2"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestProp_AllNetScripts_WellFormed runs a shipped script, drawn per case
// from all of them, against generic generated facts drawn from the union of the shapes in
// netscript_prop_test.go (legs, loopbacks, bonds, OOB legs, switch mgmt
// ports). Oracle: a non-zero exit carries a message and no Python
// traceback; on exit 0 the bundle is valid JSON, every path is absolute,
// every .yaml file parses as YAML, and the output is independent of facts
// order.
//
// Generic facts rarely model a host a given script accepts, so this is
// mostly the refusal half: no script crashes or exits silently on facts it
// was not written for. The per-script properties in netscript_prop_test.go
// cover what an accepted host renders to.
func TestProp_AllNetScripts_WellFormed(t *testing.T) {
	entries, err := os.ReadDir(filepath.Dir(scriptPath(t, "x")))
	if err != nil {
		t.Fatal(err)
	}
	requirePython(t)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	var ok, failed atomic.Int64
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.SampledFrom(names).Draw(t, "script")
		facts := drawGenericFacts(t)
		r := runNetScript(t, name, facts)
		if r.exit != 0 {
			if r.stderr == "" {
				t.Fatalf("non-zero exit %d with no stderr explanation", r.exit)
			}
			if strings.Contains(r.stderr, "Traceback") {
				t.Fatalf("script crashed instead of failing with a message:\n%s\nfacts=%+v", r.stderr, facts)
			}
			failed.Add(1)
			return
		}
		ok.Add(1)
		var b netBundle
		if err := json.Unmarshal([]byte(r.stdout), &b); err != nil {
			t.Fatalf("bundle JSON: %v\n%s", err, r.stdout)
		}
		for _, f := range b.Files {
			if !filepath.IsAbs(f.Path) {
				t.Fatalf("relative path %q", f.Path)
			}
			if strings.HasSuffix(f.Path, ".yaml") {
				var v any
				if err := yaml.Unmarshal([]byte(f.Content), &v); err != nil {
					t.Fatalf("%s does not parse as YAML: %v\n%s\nfacts=%+v", f.Path, err, f.Content, facts)
				}
			}
		}
		r2 := runNetScript(t, name, shuffled(t, facts))
		if r2.stdout != r.stdout {
			t.Fatalf("output depends on facts order\nfacts=%+v\n%s\n---\n%s", facts, r.stdout, r2.stdout)
		}
	})
	t.Logf("coverage: ok=%d failed=%d", ok.Load(), failed.Load())
}

func drawGenericFacts(t *rapid.T) []model.SnapshotInterface {
	pfx := rapid.SampledFrom([]string{"10.254", "10.253", "10.252"}).Draw(t, "pfx")
	names := []string{"lo", "eno1", "eno2", "ens6", "eno3", "enp1s0f0", "enp1s0f1", "bond0", "swp1", "enp3s0np17", "ipmi0", "wan0"}
	macs := []string{"7C:C2:55:BE:F6:0C", "7c:c2:55:be:f6:0d", "aa:bb:cc:00:00:01", "AA:BB:CC:00:00:02", "3c:ec:ef:de:d0:89"}
	addrs := []string{
		".31.67/32", ".28.1/31", ".28.3/31", ".29.9/31", ".29.11/31",
		".0.22/22", ".3.254/22", ".240.5/22", ".241.25/22", ".241.108/23", ".240.1/22",
	}
	n := rapid.IntRange(0, 7).Draw(t, "n")
	var out []model.SnapshotInterface
	usedName := map[string]bool{}
	for i := 0; i < n; i++ {
		name := rapid.SampledFrom(names).Draw(t, fmt.Sprintf("f%dname", i))
		if usedName[name] {
			continue // NetBox interface names are unique per device
		}
		usedName[name] = true
		f := model.SnapshotInterface{NetBoxID: i + 1, Name: name}
		f.Type = rapid.SampledFrom([]string{"physical", "physical", "physical", "lag", "ipmi"}).Draw(t, fmt.Sprintf("f%dtype", i))
		if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("f%dmacp", i)) != 0 {
			f.MAC = rapid.SampledFrom(macs).Draw(t, fmt.Sprintf("f%dmac", i))
		}
		if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("f%dipp", i)) != 0 {
			f.IP = pfx + rapid.SampledFrom(addrs).Draw(t, fmt.Sprintf("f%dip", i))
			if rapid.Bool().Draw(t, fmt.Sprintf("f%dgwp", i)) {
				f.Gateway = pfx + ".3.254"
			}
		}
		if rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("f%dlagp", i)) == 0 {
			f.LAG = "bond0"
		}
		f.MTU = rapid.SampledFrom([]int{0, 1500, 9000}).Draw(t, fmt.Sprintf("f%dmtu", i))
		f.VLAN = rapid.SampledFrom([]int{0, 1}).Draw(t, fmt.Sprintf("f%dvlan", i))
		out = append(out, f)
	}
	return out
}
