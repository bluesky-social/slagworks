//go:build integration

package qemutest_test

// Storage property suite, part 6: the histories that reach a defect the
// system under test still has. runPropCase skips a case that
// propKnownDefect matches, so the properties stay as strict as the contract
// and the rest of each set keeps running. When a defect is fixed, delete its
// branch here and its entries in TestStoragePropKnownDefects.

import (
	"sort"
	"testing"
)

// propKnownDefect returns a one-line description of the unfixed defect the
// case would fail on, or "" when it reaches none. It follows, step by step,
// which disks carry an md member superblock and which carry a bootloader,
// from the layouts and expected outcomes alone.
func propKnownDefect(c propCase) string {
	// Per disk, whether it carries an md member superblock and whether it
	// carries a bootloader for c.Firmware: propYes, propMaybe or propNo.
	md, boot := map[string]string{}, map[string]string{}
	for _, d := range c.Disks {
		md[d.ID], boot[d.ID] = propNo, propNo
	}
	bootable := func(dl propDiskLayout) bool {
		for _, p := range dl.Parts {
			if (c.Firmware == "uefi" && p.ESP) || (c.Firmware == "bios" && p.BIOSGrub) {
				return true
			}
		}
		return false
	}
	for _, st := range c.Steps {
		// What this step's deploy writes, and what it leaves on those disks.
		written := map[string]bool{}
		member := map[string]bool{}
		boots := map[string]bool{}
		kept := map[string]bool{} // exempt from --wipe-all-disks
		for _, dl := range st.Layout.Disks {
			written[dl.Disk] = true
			boots[dl.Disk] = bootable(dl)
		}
		for _, a := range st.Layout.Arrays {
			for _, m := range a.Members {
				if a.Type == "preserve" {
					kept[m.Disk] = true
					continue
				}
				written[m.Disk] = true
				member[m.Disk] = true
			}
		}
		for _, id := range st.Layout.Retain {
			kept[id] = true
		}
		// A disk the deploy leaves as it found it.
		stale := func(id string) bool {
			return !written[id] && (!st.WipeAll || kept[id])
		}

		if propExpects(st.Expect, "ok") {
			// A layout without md arrays leaves the image's mdadm.conf, which
			// has no "AUTO -all", in the installed system.
			if len(st.Layout.Arrays) == 0 {
				for _, id := range st.Attach {
					if stale(id) && md[id] == propYes {
						return "a layout without md arrays leaves auto-assembly on, so the installed system " +
							"assembles the old array on " + id + ", a disk outside the layout"
					}
				}
			}
			// The boot-local script boots the first ESP that has a
			// bootloader, and a BIOS machine its first disk. Either is the
			// old install when its disk comes before the new boot disk. A
			// UEFI deploy's first boot is spared by the one-shot BootNext.
			if c.Firmware == "bios" || st.Reboot {
				for _, id := range st.Attach {
					if written[id] && boots[id] {
						break
					}
					if stale(id) && boot[id] == propYes {
						return "boot-local boots the first bootable disk, not the deployed one, so the machine " +
							"comes up in the old install on " + id + ", a disk outside the layout"
					}
				}
			}
		}

		// Carry the disks forward. A refused deploy writes nothing. One that
		// was cut short, or whose outcome is open, leaves what it would have
		// changed unknown, and an unknown never matches: bios-scsi-kill, for
		// one, redeploys over an interrupted mirror and passes.
		ran := false
		for _, e := range []string{"ok", "killed", "fatal_at"} {
			if propExpects(st.Expect, e) && !propStoppedEarly(st, e) {
				ran = true
			}
		}
		if !ran {
			continue
		}
		certain := st.Expect == "ok" || st.Expect == "fatal_at"
		set := func(state map[string]string, id, to string) {
			if !certain {
				// Neither the old content nor the new is assured: a
				// --destructive deploy cut short may have got no further
				// than wiping the disk.
				to = propMaybe
			}
			state[id] = to
		}
		for id := range written {
			to := propNo
			if member[id] {
				to = propYes
			}
			set(md, id, to)
			to = propNo
			if boots[id] {
				to = propYes
				if st.Expect != "ok" {
					// Stopped before, or while, the bootloader went on.
					to = propMaybe
				}
			}
			set(boot, id, to)
		}
		if st.WipeAll {
			for _, id := range st.Attach {
				if !written[id] && !kept[id] {
					set(md, id, propNo)
					set(boot, id, propNo)
				}
			}
		}
	}
	return ""
}

// TestStoragePropKnownDefects needs no VM. It holds propKnownDefect to the
// histories that VM runs have shown to fail on a known defect and to pass:
// a match that is too wide would quietly skip working cases, one too narrow
// would leave a set red. No history that runs by default may be skipped.
func TestStoragePropKnownDefects(t *testing.T) {
	pinned := propPinnedCases()
	var got []string
	for name, c := range pinned {
		if propKnownDefect(c) != "" {
			got = append(got, name)
		}
	}
	sort.Strings(got)
	want := []string{
		"bios-raid1-single",
		"bios-root-moves-off-old-install",
		"uefi-prescan-bystander-md0-destructive",
		"uefi-raid1-then-single",
		"uefi-root-moves-off-old-install",
	}
	if !propSameSet(got, want) {
		t.Errorf("pinned histories matched as reaching a known defect: %v, want %v", got, want)
	}
	for test, names := range propDefaultRuns {
		for _, name := range names {
			if why := propKnownDefect(pinned[name]); why != "" {
				t.Errorf("%s plays %s by default, which would be skipped: %s", test, name, why)
			}
		}
	}
	for _, seed := range propGenDefaultSeeds {
		if why := propKnownDefect(propGenCase(seed)); why != "" {
			t.Errorf("default generated seed %d would be skipped: %s", seed, why)
		}
	}
	for _, seed := range []int64{2005, 2044, 2077, 2120, 2153} {
		if propKnownDefect(propGenCase(seed)) == "" {
			t.Errorf("generated seed %d fails on a known defect on VMs but is not matched", seed)
		}
	}
	for _, seed := range []int64{
		2, 3, 7, 13, 23, 34, 66, 78, 1000, 1001, 1003,
		2002, 2009, 2012, 2017, 2021, 2034, 2035, 2045, 2058, 2181, 2184,
	} {
		if why := propKnownDefect(propGenCase(seed)); why != "" {
			t.Errorf("generated seed %d passes on VMs but is matched: %s", seed, why)
		}
	}
}
