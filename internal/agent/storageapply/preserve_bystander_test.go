//go:build !integration

package storageapply

import (
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// preserveBystanderCase is a deploy that keeps a data array: one clean
// target disk, the preserved array's two members (which the deploy
// environment auto-assembled as /dev/md125), and a fifth disk outside the
// layout whose old array the deploy environment assembled as bystanderMD.
func preserveBystanderCase(bystanderMD string, destructive bool) hostCase {
	const size = 480103981056
	return hostCase{
		Disks: []hostDiskSpec{
			{Name: "sda", Bytes: size, Sector: 512, Tran: "sata", Role: "target", Prior: "clean"},
			{Name: "nvme1n1", Bytes: size, Sector: 512, Tran: "nvme", Role: "preserve", Prior: "preserve-whole", ActiveAs: "md125"},
			{Name: "nvme2n1", Bytes: size, Sector: 512, Tran: "nvme", Role: "preserve", Prior: "preserve-whole", ActiveAs: "md125"},
			{Name: "sdd", Bytes: size, Sector: 512, Tran: "sata", Role: "bystander", Prior: "md-whole", ActiveAs: bystanderMD},
		},
		Layout: storage.Layout{
			Devices: map[string]storage.Device{
				"sda": {Type: "disk", PTable: "gpt", Partitions: []storage.Partition{
					{Name: "sda1", Size: "512M", FS: "vfat", Bootable: true},
					{Name: "sda2", Size: "rest", FS: "ext4"},
				}},
				"mdkeep": {Type: "preserve", Members: []string{"nvme1n1", "nvme2n1"}, FS: "xfs", ArrayUUID: preserveUUID},
			},
			Mounts: map[string]storage.Mount{
				"/":         {Device: "sda2"},
				"/boot/efi": {Device: "sda1"},
				"/data":     {Device: "mdkeep"},
			},
		},
		Destructive: destructive,
	}
}

// A deploy without --destructive that keeps a preserved array stops the
// array the deploy environment assembled from that array's members, and no
// other: an array on a disk outside the layout stays assembled as found. It
// used to run a global `mdadm --stop --scan` (twice), which stopped the
// bystander array too.
func TestDeploy_PreserveLeavesBystanderArrayAssembled(t *testing.T) {
	hc := preserveBystanderCase("md127", false)
	h := hc.build()
	stage, err := deploy(quietCtx(h), h, hc)
	if stage != "done" {
		t.Fatalf("deploy stopped at %q: %v", stage, err)
	}
	if a, ok := h.arrays["md127"]; !ok || len(a.members) != 1 || a.members[0] != "sdd" {
		t.Errorf("the bystander array md127 on sdd was stopped or changed: arrays %+v", h.arrays)
	}
	for _, c := range h.calls {
		if c == "mdadm --stop --scan" || c == "mdadm --stop /dev/md127" {
			t.Errorf("deploy without --destructive ran %q, which stops the bystander array", c)
		}
	}
	if h.writes["sdd"] {
		t.Errorf("deploy wrote to sdd, a disk outside the layout")
	}
	if _, ok := h.arrays["md125"]; ok {
		t.Errorf("the auto-assembly md125 of the preserved members is still assembled; it should have been stopped and the array assembled at its planned node")
	}
	if a, ok := h.arrays["md0"]; !ok || a.uuid != preserveUUID || !h.mounted["md0"] {
		t.Errorf("the preserved array is not assembled and mounted at /dev/md0: arrays %+v mounted %v", h.arrays, h.mounted)
	}
}

// When the bystander array holds the /dev/mdN the layout assembles the
// preserved array at, a deploy without --destructive cannot complete
// without stopping it, which it may not do. It must refuse before it changes
// anything, as for a raid device's node.
func TestDeploy_PreserveBystanderArrayHoldsPlannedMDName_RefusedBeforeAnyChange(t *testing.T) {
	hc := preserveBystanderCase("md0", false)
	h := hc.build()
	stage, err := deploy(quietCtx(h), h, hc)
	if err == nil {
		t.Fatalf("deploy completed (stage %q); want a refusal: /dev/md0 is held by an array outside the layout", stage)
	}
	if stage != "md names" {
		t.Errorf("deploy stopped at %q (%v); want the md name check to refuse it", stage, err)
	}
	if len(h.writes) != 0 {
		t.Errorf("deploy wrote to %v before stopping at %q", keys(h.writes), stage)
	}
	for _, c := range h.calls {
		if strings.HasPrefix(c, "mdadm") {
			t.Errorf("deploy ran %q; the refusal must come before any mdadm command", c)
		}
	}
	if a, ok := h.arrays["md0"]; !ok || len(a.members) != 1 || a.members[0] != "sdd" {
		t.Errorf("the bystander array at md0 changed: %+v", h.arrays)
	}
	for _, want := range []string{`preserve "mdkeep"`, "/dev/md0", "sdd[0]", "--destructive", "nothing has been changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// The remedy the refusal names works: --destructive stops every assembled
// array, so the same host deploys, and the bystander disk is not written.
func TestDeploy_PreserveBystanderArrayHoldsPlannedMDName_DestructiveCompletes(t *testing.T) {
	hc := preserveBystanderCase("md0", true)
	h := hc.build()
	stage, err := deploy(quietCtx(h), h, hc)
	if stage != "done" {
		t.Fatalf("--destructive deploy stopped at %q: %v", stage, err)
	}
	if h.writes["sdd"] {
		t.Errorf("--destructive deploy wrote to sdd, a disk outside the layout")
	}
	if got := h.disks["sdd"].whole.mdUUID(); got != oldArrayUUID("md0") {
		t.Errorf("sdd's md superblock = %q, want the old array's %q", got, oldArrayUUID("md0"))
	}
}
