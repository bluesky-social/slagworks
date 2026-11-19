package storage

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// deployInitrdModules returns the kernel modules the deploy initrd's hook
// bakes in: the word list of its `for m in ...; do manual_add_modules "$m"`
// loop.
func deployInitrdModules(t *testing.T) map[string]bool {
	t.Helper()
	const hookPath = "../../assets/initramfs/hooks/slagworks"
	hook, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read the deploy initramfs hook: %v", err)
	}
	loop := regexp.MustCompile(`(?s)for m in (.*?); do\s+manual_add_modules "\$m"`).FindSubmatch(hook)
	if loop == nil {
		t.Fatalf("no `for m in ...; do manual_add_modules \"$m\"` loop in %s; update this parser if the hook changed shape", hookPath)
	}
	baked := map[string]bool{}
	for _, m := range strings.Fields(strings.ReplaceAll(string(loop[1]), `\`, " ")) {
		baked[m] = true
	}
	if !baked["overlay"] {
		t.Fatalf("parsed module list %v from %s lacks overlay, which the hook has always baked; the parser is wrong", baked, hookPath)
	}
	return baked
}

// TestDeployInitrdBakesEveryAcceptedRAIDPersonality ties the planner to the
// boot image. A RAID level validateRAID accepts whose md personality module
// the deploy initrd leaves out passes planning and then fails in the deploy
// at `mdadm --create` ("RUN_ARRAY failed: Invalid argument"), after the
// member disks have been repartitioned. RAID 5 and 6 shipped that way: the
// hook baked raid0, raid1 and raid10 but not raid456.
func TestDeployInitrdBakesEveryAcceptedRAIDPersonality(t *testing.T) {
	baked := deployInitrdModules(t)

	// Probe validateRAID directly rather than reading raidLevels, so a
	// level accepted through any other path is still held to the hook.
	layout := Layout{Devices: map[string]Device{}}
	var members []string
	for _, d := range []string{"vda", "vdb", "vdc", "vdd", "vde", "vdf"} {
		layout.Devices[d] = Device{Type: "disk"}
		members = append(members, d)
	}
	accepted := 0
	for level := -10; level <= 64; level++ {
		if validateRAID("md", Device{Type: "raid", Level: level, Members: members}, layout) != nil {
			continue
		}
		accepted++
		module, ok := RAIDModule(level)
		if !ok {
			t.Errorf("validateRAID accepts RAID level %d but RAIDModule does not name its md personality module", level)
			continue
		}
		if !baked[module] {
			t.Errorf("validateRAID accepts RAID level %d, but its md personality module %q is not baked into the deploy initrd "+
				"by assets/initramfs/hooks/slagworks: mdadm --create would fail RUN_ARRAY after repartitioning the disks",
				level, module)
		}
	}
	if accepted == 0 {
		t.Fatal("validateRAID accepted no RAID level at all; the probe layout is wrong")
	}
}
