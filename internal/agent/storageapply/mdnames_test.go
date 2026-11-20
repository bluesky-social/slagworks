//go:build !integration

package storageapply

import (
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// bystanderMD0Case is the host the storage property suite found: one clean
// target disk, two clean bare disks the layout builds a RAID 0 over (at
// /dev/md0), and a fourth disk outside the layout whose old array the deploy
// environment auto-assembled as /dev/md0.
func bystanderMD0Case(destructive bool) hostCase {
	const size = 480103981056
	return hostCase{
		Disks: []hostDiskSpec{
			{Name: "sda", Bytes: size, Sector: 512, Tran: "sata", Role: "target", Prior: "clean"},
			{Name: "sdb", Bytes: size, Sector: 512, Tran: "sata", Role: "bare", Prior: "clean"},
			{Name: "sdc", Bytes: size, Sector: 512, Tran: "sata", Role: "bare", Prior: "clean"},
			{Name: "sdd", Bytes: size, Sector: 512, Tran: "sata", Role: "bystander", Prior: "md-whole", ActiveAs: "md0"},
		},
		Layout: storage.Layout{
			Devices: map[string]storage.Device{
				"sda": {Type: "disk", PTable: "gpt", Partitions: []storage.Partition{
					{Name: "sda1", Size: "512M", FS: "vfat", Bootable: true},
					{Name: "sda2", Size: "rest", FS: "ext4"},
				}},
				"sdb":    {Type: "disk"},
				"sdc":    {Type: "disk"},
				"mddata": {Type: "raid", Level: 0, Members: []string{"sdb", "sdc"}, FS: "xfs"},
			},
			Mounts: map[string]storage.Mount{
				"/":         {Device: "sda2"},
				"/boot/efi": {Device: "sda1"},
				"/srv":      {Device: "mddata"},
			},
		},
		Destructive: destructive,
	}
}

// A deploy without --destructive whose layout creates an array at a
// /dev/mdN that an auto-assembled array from outside the layout holds must
// refuse before it changes anything: it may not stop that array, and
// mdadm --create cannot take a name in use. It used to partition the clean
// disks and then fail at mdadm --create.
func TestDeploy_BystanderArrayHoldsPlannedMDName_RefusedBeforeAnyWrite(t *testing.T) {
	hc := bystanderMD0Case(false)
	h := hc.build()
	stage, err := deploy(quietCtx(h), h, hc)
	if err == nil {
		t.Fatalf("deploy completed (stage %q); want a refusal: /dev/md0 is held by an array outside the layout", stage)
	}
	if len(h.writes) != 0 {
		t.Errorf("deploy wrote to %v before stopping at %q (%v); want no disk changed", keys(h.writes), stage, err)
	}
	if stage != "md names" {
		t.Errorf("deploy stopped at %q (%v); want the md name check to refuse it", stage, err)
	}
	for _, c := range h.calls {
		if strings.HasPrefix(c, "mdadm --stop") {
			t.Errorf("deploy ran %q; a deploy without --destructive must not stop an array outside the layout", c)
		}
	}
	if a, ok := h.arrays["md0"]; !ok || len(a.members) != 1 || a.members[0] != "sdd" {
		t.Errorf("the bystander array at md0 changed: %+v", h.arrays)
	}
	for _, want := range []string{`"mddata"`, "/dev/md0", "sdd", "--destructive", "nothing has been changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

// The remedy the refusal names works: --destructive stops every assembled
// array, so the same host deploys, and the bystander disk is not written.
func TestDeploy_BystanderArrayHoldsPlannedMDName_DestructiveCompletes(t *testing.T) {
	hc := bystanderMD0Case(true)
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

func TestCheckMDNamesFree(t *testing.T) {
	raidLayout := bystanderMD0Case(false).Layout
	tests := []struct {
		name    string
		layout  storage.Layout
		arrays  map[string]*hArray
		wantErr []string // nil: the check passes
	}{
		{name: "no arrays assembled", layout: raidLayout},
		{name: "array on a name the plan does not use", layout: raidLayout,
			arrays: map[string]*hArray{"md127": {name: "md127", members: []string{"sdd"}}}},
		{name: "array on the planned name", layout: raidLayout,
			arrays:  map[string]*hArray{"md0": {name: "md0", members: []string{"sdd", "sde"}}},
			wantErr: []string{`raid "mddata"`, "/dev/md0", "sdd[0] sde[1]"}},
		{name: "layout with no raid", layout: storage.Layout{Devices: map[string]storage.Device{"sda": {Type: "disk"}}},
			arrays: map[string]*hArray{"md0": {name: "md0", members: []string{"sdd"}}}},
		{name: "second planned name taken", layout: storage.Layout{Devices: map[string]storage.Device{
			"sda": {Type: "disk"}, "sdb": {Type: "disk"}, "sdc": {Type: "disk"}, "sde": {Type: "disk"},
			"mda": {Type: "raid", Level: 0, Members: []string{"sda", "sdb"}},
			"mdb": {Type: "raid", Level: 0, Members: []string{"sdc", "sde"}},
		}},
			arrays:  map[string]*hArray{"md1": {name: "md1", members: []string{"sdd"}}},
			wantErr: []string{`raid "mdb"`, "/dev/md1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHost()
			for n, a := range tt.arrays {
				h.arrays[n] = a
			}
			err := CheckMDNamesFree(quietCtx(h), tt.layout)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("CheckMDNamesFree: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckMDNamesFree passed, want a refusal")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not mention %q: %v", want, err)
				}
			}
			for _, c := range h.calls {
				if c != "cat /proc/mdstat" {
					t.Errorf("the check ran %q; it may only read /proc/mdstat", c)
				}
			}
		})
	}
}

// /proc/mdstat does not exist until the md driver is loaded, which on a host
// with no arrays is not until the first mdadm --create: a mdstat confirmed
// absent means no array holds a name.
func TestCheckMDNamesFree_NoMdstatMeansNoArrays(t *testing.T) {
	r := newScriptRunner()
	r.outputErr["cat /proc/mdstat"] = exitErr{1}
	r.output["test ! -e /proc/mdstat"] = ""
	if err := CheckMDNamesFree(scriptCtx(r), bystanderMD0Case(false).Layout); err != nil {
		t.Fatalf("CheckMDNamesFree with no /proc/mdstat: %v, want nil", err)
	}
}

// A mdstat that exists but cannot be read leaves the nodes' state unknown;
// the check refuses rather than let the plan partition and then find out.
func TestCheckMDNamesFree_UnreadableMdstatRefuses(t *testing.T) {
	r := newScriptRunner()
	r.outputErr["cat /proc/mdstat"] = exitErr{1}
	r.outputErr["test ! -e /proc/mdstat"] = exitErr{1}
	err := CheckMDNamesFree(scriptCtx(r), bystanderMD0Case(false).Layout)
	if err == nil {
		t.Fatal("CheckMDNamesFree passed although /proc/mdstat exists and could not be read")
	}
	for _, want := range []string{"/proc/mdstat", "refusing to partition", "--destructive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}
