package snapshot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

// Tests over a recorded deployment (see shape_record_prop_test.go for how to
// record one). They log counts and shapes only, never a name, address or
// serial from the recording, so their output can be quoted in a report.

func shapeRecorded(t *testing.T) (*shapeworld.World, *shapeworld.StoreData) {
	t.Helper()
	dir := os.Getenv(shapeDirEnv)
	if dir == "" {
		t.Skipf("set %s to a recorded deployment", shapeDirEnv)
	}
	return shapeLoadWorld(t, dir), shapeLoadStore(t, dir)
}

func shapeQuiet(t *testing.T) {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(old) })
}

// shapeBuild builds one config through the real dispatcher. A non-zero
// shuffleSeed reorders every NetBox and store list except the prefix lists
// (NetBox returns those in address order); shapeBuildAnyOrder reorders those
// too.
func shapeBuild(w *shapeworld.World, sd *shapeworld.StoreData, slug string, shuffleSeed int64) (model.SiteSnapshot, error) {
	nb := &shapeworld.NB{W: w, Shuffle: shuffleSeed != 0, KeepPrefixOrder: true, Seed: shuffleSeed}
	st := &shapeworld.Store{D: sd, Shuffle: shuffleSeed != 0, Seed: shuffleSeed}
	return NewDispatcher(nb, st, shapeworld.Sink{}).BuildSnapshot(context.Background(), slug)
}

func shapeBuildAnyOrder(w *shapeworld.World, sd *shapeworld.StoreData, slug string, shuffleSeed int64) (model.SiteSnapshot, error) {
	nb := &shapeworld.NB{W: w, Shuffle: true, Seed: shuffleSeed}
	st := &shapeworld.Store{D: sd, Shuffle: true, Seed: shuffleSeed}
	return NewDispatcher(nb, st, shapeworld.Sink{}).BuildSnapshot(context.Background(), slug)
}

// TestShapeRecordedBuilds: every recorded site_config builds, and builds to
// the same snapshot whatever order NetBox and the store list devices,
// interfaces, addresses, locations, ranges, console ports and machines in.
func TestShapeRecordedBuilds(t *testing.T) {
	w, sd := shapeRecorded(t)
	shapeQuiet(t)
	shapeRecordedOrder(t, w, sd, shapeBuild)
}

// TestShapeRecordedPrefixOrder reorders the prefix lists as well.
func TestShapeRecordedPrefixOrder(t *testing.T) {
	w, sd := shapeRecorded(t)
	shapeQuiet(t)
	shapeRecordedOrder(t, w, sd, shapeBuildAnyOrder)
}

func shapeRecordedOrder(t *testing.T, w *shapeworld.World, sd *shapeworld.StoreData,
	build func(*shapeworld.World, *shapeworld.StoreData, string, int64) (model.SiteSnapshot, error)) {
	t.Helper()
	for i, cfg := range sd.Configs {
		base, err := shapeBuild(w, sd, cfg.Slug, 0)
		if err != nil {
			t.Errorf("recorded config #%d does not build: %v", i, err)
			continue
		}
		want := propCanon(base)
		stable := true
		for seed := int64(1); seed <= 50 && stable; seed++ {
			got, err := build(w, sd, cfg.Slug, seed)
			if err != nil {
				t.Errorf("recorded config #%d: builds in NetBox order, fails with lists reordered (seed %d): %v", i, seed, err)
				stable = false
			} else if propCanon(got) != want {
				t.Errorf("recorded config #%d: snapshot depends on list order (seed %d): %s", i, seed, shapeFirstDiff(base, got))
				stable = false
			}
		}
		t.Logf("config #%d: %d machines, %d reservations, %d relay pools; stable over 50 list orders: %v",
			i, len(base.Machines), len(base.Reservations), len(base.DHCP.RelayPools), stable)
	}
}

// shapeGapKinds classifies a preflight gap by the check that produced it,
// keyed on a fragment of the message that carries no deployment data.
var shapeGapKinds = []struct{ kind, fragment string }{
	{"locked", "the machine is locked"},
	{"not-pxe-eligible", "not PXE-eligible"},
	{"binding-conflict", "machine binding conflict"},
	{"no-enrolled-serial", "no stable enrolled NetBox serial"},
	{"device-no-serial", "has no serial; set its hardware serial"},
	{"netbox-serial-conflict", "NetBox serial conflict"},
	{"shared-nic-mac", "physical NIC MAC"},
	{"no-smbios-serial", "no usable SMBIOS system or chassis serial"},
	{"commission-serial-conflict", "commission serial conflict"},
	{"commission-netbox-serial-conflict", "commission/NetBox serial conflict"},
	{"smbios-serial-shared", "from this machine's persisted inventory is already owned"},
	{"smbios-uuid-invalid", "is missing or invalid"},
	{"smbios-uuid-shared", "SMBIOS system UUID"},
	{"disk-serial-shared", "disk serial"},
	{"disk-wwn-shared", "disk WWN"},
	{"no-primary-ip4", "no primary_ip4"},
	{"no-boot-nic", "no bootable NIC"},
	{"no-network-script", "no network-config script"},
	{"no-storage-script", "no storage-layout script"},
	{"no-image", "no PXE image"},
	{"no-kernel-params", "no slagworks_kernel_params"},
}

func shapeGapKind(gap string) string {
	for _, k := range shapeGapKinds {
		if strings.Contains(gap, k.fragment) {
			return k.kind
		}
	}
	return "other"
}

// TestShapeRecordedPreflightCensus runs the op-creation preflight for every
// recorded machine, for commission and for deploy, and counts which checks
// refuse how many machines. A refusal that fires for a large share of a real
// fleet is either a real fleet-wide data problem or a check stricter than
// the fleet's legitimate shapes; the census is how to tell which checks to
// look at.
func TestShapeRecordedPreflightCensus(t *testing.T) {
	w, sd := shapeRecorded(t)
	shapeQuiet(t)
	devByID := map[int]netbox.Device{}
	for _, devs := range w.Devices {
		for _, d := range devs {
			devByID[d.ID] = d
		}
	}
	for i, cfg := range sd.Configs {
		snap, err := shapeBuild(w, sd, cfg.Slug, 0)
		if err != nil {
			t.Errorf("recorded config #%d does not build: %v", i, err)
			continue
		}
		// How many machines model each non-ipmi MAC.
		owners := map[string]map[string]bool{}
		for _, m := range snap.Machines {
			for _, ifc := range m.Interfaces {
				if ifc.MAC == "" || ifc.Type == "ipmi" {
					continue
				}
				if owners[ifc.MAC] == nil {
					owners[ifc.MAC] = map[string]bool{}
				}
				owners[ifc.MAC][m.ID.String()] = true
			}
		}
		for _, op := range []model.OperationType{model.OpCommission, model.OpDeploy} {
			kinds := map[string]int{}
			refused, onlyShared, sharedWithUnique := 0, 0, 0
			for mi := range snap.Machines {
				ms := &snap.Machines[mi]
				var dev *netbox.Device
				if ms.NetBoxDeviceID != nil {
					if d, ok := devByID[*ms.NetBoxDeviceID]; ok {
						dev = &d
					}
				}
				gaps := PreflightMachine(PreflightInput{
					Machine: ms, Machines: snap.Machines, Device: dev, Images: snap.Images,
					HasNetworkScripts: len(sd.NetworkScripts) > 0, HasStorageScripts: len(sd.Scripts) > 0,
				}, op)
				if len(gaps) == 0 {
					continue
				}
				refused++
				seen := map[string]bool{}
				for _, g := range gaps {
					seen[shapeGapKind(g)] = true
				}
				for k := range seen {
					kinds[k]++
				}
				if seen["shared-nic-mac"] {
					// PXE eligibility and the lock are operator switches,
					// not model gaps: leave them out when asking whether the
					// shared MAC is the only thing in the way.
					only := true
					for k := range seen {
						if k != "shared-nic-mac" && k != "not-pxe-eligible" && k != "locked" {
							only = false
						}
					}
					if only {
						onlyShared++
					}
					for _, ifc := range ms.Interfaces {
						if ifc.MAC != "" && ifc.Type == "physical" && len(owners[ifc.MAC]) == 1 {
							sharedWithUnique++
							break
						}
					}
				}
			}
			var parts []string
			for k, n := range kinds {
				parts = append(parts, fmt.Sprintf("%s=%d", k, n))
			}
			sort.Strings(parts)
			t.Logf("config #%d %s: %d of %d machines refused; machines per check: %s", i, op, refused, len(snap.Machines), strings.Join(parts, " "))
			if kinds["shared-nic-mac"] > 0 {
				t.Logf("config #%d %s: shared-nic-mac refuses %d machines; for %d it is the only model gap; %d of them also model a physical NIC MAC no other machine has",
					i, op, kinds["shared-nic-mac"], onlyShared, sharedWithUnique)
			}
		}
		shared := 0
		maxOwners := 0
		for _, o := range owners {
			if len(o) > 1 {
				shared++
				maxOwners = max(maxOwners, len(o))
			}
		}
		t.Logf("config #%d: %d non-ipmi MACs modelled on more than one machine (largest group: %d machines)", i, shared, maxOwners)
	}
}

// shapeFirstDiff names the first field two snapshots disagree on, without
// printing either value.
func shapeFirstDiff(a, b model.SiteSnapshot) string {
	if a.DHCP.Subnet != b.DHCP.Subnet || a.DHCP.Gateway != b.DHCP.Gateway {
		return "dhcp subnet/gateway"
	}
	if fmt.Sprint(a.DHCP.DNS) != fmt.Sprint(b.DHCP.DNS) || a.DHCP.LeaseSeconds != b.DHCP.LeaseSeconds {
		return "dhcp dns/lease"
	}
	if fmt.Sprint(a.DHCP.RelayPools) != fmt.Sprint(b.DHCP.RelayPools) {
		return "dhcp relay pools"
	}
	if fmt.Sprint(a.OOB) != fmt.Sprint(b.OOB) {
		return "oob config"
	}
	if len(a.Reservations) != len(b.Reservations) {
		return "reservation count"
	}
	if len(a.Machines) != len(b.Machines) {
		return "machine count"
	}
	return "machine facts or reservation contents"
}
