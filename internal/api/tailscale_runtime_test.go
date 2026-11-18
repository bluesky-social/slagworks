package api

import (
	"encoding/json"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestTailnetHostname covers the <short>-<pop> derivation that must match
// ansible's tailscale_hostname (group_vars/all) so a re-deploy reuses the
// MagicDNS name instead of spawning a duplicate node.
func TestTailnetHostname(t *testing.T) {
	cases := []struct {
		devName string
		pop     string
		want    string
	}{
		// NetBox names these hosts <short>.<pop>; strip the .pop suffix.
		{"oob-gw-3.pop2", "pop2", "oob-gw-3-pop2"},
		{"kworker-l34-1.pop1", "pop1", "kworker-l34-1-pop1"},
		// No .pop suffix on the name: use it as the short part as-is.
		{"oob-gw-3", "pop2", "oob-gw-3-pop2"},
		// Suffix only matches the exact ".<pop>" tail, not a mid-name match.
		{"pop2-thing.pop2", "pop2", "pop2-thing-pop2"},
	}
	for _, c := range cases {
		if got := tailnetHostname(c.devName, c.pop); got != c.want {
			t.Errorf("tailnetHostname(%q, %q) = %q, want %q", c.devName, c.pop, got, c.want)
		}
	}
}

// TestTailscaleRuntime_ComposesWithSiblings: an OpParams carrying deploy
// flags + a bmc_runtime block gains tailscale_runtime alongside — the
// blocks compose in one struct, and the marshalled wire shape carries
// every key. This is the shape resolveTailscaleRuntime produces when it
// stamps params.TailscaleRuntime after the BMC resolver ran.
func TestTailscaleRuntime_ComposesWithSiblings(t *testing.T) {
	params := model.OpParams{
		Destructive:      true,
		BMCRuntime:       &model.BMCRuntime{Username: "u", ExtraConfig: map[string]any{}},
		TailscaleRuntime: &model.TailscaleRuntime{AuthKey: "tskey-xyz", Hostname: "h", Tags: []string{"tag:pop1"}},
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("marshalled not an object: %v", err)
	}
	for _, k := range []string{"destructive", "bmc_runtime", "tailscale_runtime"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("marshalled params dropped %q: %s", k, raw)
		}
	}
	// Spot-check the tailscale block round-trips the key.
	var got model.TailscaleRuntime
	if err := json.Unmarshal(obj["tailscale_runtime"], &got); err != nil {
		t.Fatalf("tailscale_runtime decode: %v", err)
	}
	if got.AuthKey != "tskey-xyz" || got.Hostname != "h" {
		t.Errorf("tailscale_runtime = %+v, want key=tskey-xyz host=h", got)
	}
}

// TestTailscaleRuntime_AbsentOmitsKey: an OpParams with no tailscale
// enrollment (nil pointer) omits the tailscale_runtime key entirely, so
// the boot handler's absent-block => skip-enrollment path holds.
func TestTailscaleRuntime_AbsentOmitsKey(t *testing.T) {
	raw, err := json.Marshal(model.OpParams{Destructive: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["tailscale_runtime"]; ok {
		t.Errorf("absent tailscale enrollment should omit the key: %s", raw)
	}
}
