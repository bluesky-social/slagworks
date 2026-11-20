package snapshot

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// Property test for PreflightMachine as a validator (construct a valid
// object, inject one targeted defect, check the auditor notices; and check
// it stays quiet on valid objects).
//
// The generator builds a fully modeled, clean machine plus a handful of
// clean siblings, then applies BENIGN variations that must not create a gap
// (letter case, surrounding whitespace, sibling order, siblings carrying
// firmware placeholder identifiers, siblings sharing a MAC only on an ipmi
// interface, the machine listed among its own siblings), and optionally ONE
// defect drawn from the documented rule list (docs/dev/architecture.md,
// "Preflight"; PreflightMachine's doc comments). The oracle is the defect
// table below, not the validator's code:
//
//   - no defect            -> zero gaps, for commission and for deploy
//   - defect d on op type o -> at least one gap, and one of them mentions
//     d's keyword, iff the rule applies to o; otherwise zero gaps
//   - the gap set does not depend on sibling / interface / disk order

type propPFDefect struct {
	name       string
	keyword    string
	deploy     bool // rule applies to deploy
	commission bool // rule applies to commission
	apply      func(in *PreflightInput, sib *model.MachineSnapshot)
}

func propPtr[T any](v T) *T { return &v }

var propPFDefects = []propPFDefect{
	{"locked", "locked", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Machine.Locked = true }},
	{"not-pxe-eligible", "PXE-eligible", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Machine.PXEEligible = false }},
	{"no-primary-ip4", "primary_ip4", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Device.PrimaryIP4 = nil }},
	{"no-boot-mac", "bootable NIC", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) {
		for i := range in.Machine.Interfaces {
			if in.Machine.Interfaces[i].Type != "ipmi" {
				in.Machine.Interfaces[i].MAC = ""
			}
		}
	}},
	{"no-network-script-match", "network-config script", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) {
		in.Machine.NetworkScript, in.HasNetworkScripts = nil, true
	}},
	{"no-storage-script-match", "storage-layout script", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) {
		in.Machine.StorageScript, in.HasStorageScripts = nil, true
	}},
	{"no-image", "PXE image", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Images = nil }},
	{"no-kernel-params", "slagworks_kernel_params", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Machine.KernelParams = "" }},
	{"no-admin-secret", "bmc-creds-admin", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.AdminSecretPresent = propPtr(false) }},
	{"no-monitor-secret", "bmc-creds-monitor", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) { in.MonitorSecretPresent = propPtr(false) }},
	{"binding-other-device", "binding conflict", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) {
		in.Machine.NetBoxDeviceID = propPtr(in.Device.ID + 1)
	}},
	{"binding-none", "binding conflict", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Machine.NetBoxDeviceID = nil }},
	{"no-enrolled-serial", "no stable enrolled NetBox serial", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Machine.NetBoxSerial = "N/A" }},
	{"device-serial-cleared", "has no serial", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Device.Serial = " " }},
	{"device-serial-drift", "serial conflict", true, true, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Device.Serial = "OTHER-CHASSIS" }},
	{"mac-owned-by-sibling", "also owned by machine", true, true, func(in *PreflightInput, sib *model.MachineSnapshot) {
		sib.Interfaces = append(sib.Interfaces, model.SnapshotInterface{Name: "ethX", Type: "physical", MAC: strings.ToUpper(in.Machine.Interfaces[0].MAC)})
	}},
	{"no-smbios-serials", "no usable SMBIOS", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) {
		// Undo the chassis-enrolled variation: without commission identity
		// that enrollment is uncorroborated drift, a second defect.
		in.Machine.NetBoxSerial = "SN0001"
		in.Machine.SystemSerial, in.Machine.ChassisSerial = "To Be Filled By O.E.M.", "0000000000"
	}},
	{"commission-serial-drift", "serial conflict", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) {
		in.Machine.NetBoxSerial = "SN0001"
		in.Machine.SystemSerial, in.Machine.ChassisSerial = "SOMEONE-ELSE", ""
	}},
	{"smbios-serial-owned-by-sibling", "already owned by machine", true, false, func(in *PreflightInput, sib *model.MachineSnapshot) {
		sib.ChassisSerial = " " + strings.ToLower(in.Machine.SystemSerial)
	}},
	{"uuid-missing", "system UUID", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) { in.Machine.SystemUUID = "" }},
	{"uuid-nil", "system UUID", true, false, func(in *PreflightInput, _ *model.MachineSnapshot) {
		in.Machine.SystemUUID = "00000000-0000-0000-0000-000000000000"
	}},
	{"uuid-owned-by-sibling", "also owned by machine", true, false, func(in *PreflightInput, sib *model.MachineSnapshot) {
		sib.SystemUUID = strings.ToUpper(in.Machine.SystemUUID)
	}},
	{"disk-serial-owned-by-sibling", "disk serial", true, false, func(in *PreflightInput, sib *model.MachineSnapshot) {
		sib.Disks = append(sib.Disks, model.SnapshotDiskIdentity{Serial: strings.ToLower(in.Machine.Disks[0].Serial) + " "})
	}},
	{"disk-wwn-owned-by-sibling", "disk WWN", true, false, func(in *PreflightInput, sib *model.MachineSnapshot) {
		sib.Disks = append(sib.Disks, model.SnapshotDiskIdentity{WWN: strings.ToUpper(in.Machine.Disks[0].WWN)})
	}},
}

func propPFMachine(n int) model.MachineSnapshot {
	return model.MachineSnapshot{
		ID:             uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n)),
		NetBoxDeviceID: propPtr(100 + n),
		NetBoxSerial:   fmt.Sprintf("SN%04d", n),
		Hostname:       fmt.Sprintf("h%d.site", n),
		State:          model.MachineStateCommissioned,
		PXEEligible:    true,
		SystemSerial:   fmt.Sprintf("SN%04d", n),
		ChassisSerial:  fmt.Sprintf("CH%04d", n),
		SystemUUID:     fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", n),
		KernelParams:   "console=ttyS1,115200n8",
		Interfaces: []model.SnapshotInterface{
			{NetBoxID: 10*n + 1, Name: "eth0", Type: "physical", MAC: fmt.Sprintf("aa:bb:cc:00:%02x:01", n)},
			{NetBoxID: 10*n + 2, Name: "eth1", Type: "physical", MAC: fmt.Sprintf("aa:bb:cc:00:%02x:02", n)},
			{NetBoxID: 10*n + 3, Name: "ipmi0", Type: "ipmi", MAC: fmt.Sprintf("aa:bb:cc:00:%02x:03", n)},
		},
		Disks: []model.SnapshotDiskIdentity{
			{Serial: fmt.Sprintf("DISK%04dA", n), WWN: fmt.Sprintf("0x5000c500%04da", n)},
			{Serial: fmt.Sprintf("DISK%04dB", n), WWN: fmt.Sprintf("0x5000c500%04db", n)},
		},
		StorageScript: &model.SnapshotStorageScript{Name: "s", Content: "c"},
		NetworkScript: &model.SnapshotNetworkScript{Name: "n", Content: "c"},
	}
}

func propPFGapSet(gaps []string) []string {
	out := append([]string(nil), gaps...)
	sort.Strings(out)
	return out
}

func TestPropPreflightDefectInjection(t *testing.T) {
	hits := map[string]int{}
	clean := 0
	rapid.Check(t, func(rt *rapid.T) {
		target := propPFMachine(1)
		nSib := rapid.IntRange(1, 4).Draw(rt, "nSib")
		sibs := make([]model.MachineSnapshot, nSib)
		for i := range sibs {
			sibs[i] = propPFMachine(i + 2)
		}
		in := PreflightInput{
			Machine: &target,
			Device: &netbox.Device{ID: 101, Name: "h1.site", Serial: target.NetBoxSerial,
				PrimaryIP4: &netbox.NestedRef{ID: 7}},
			Images:            []model.Image{{Name: "img"}},
			HasNetworkScripts: rapid.Bool().Draw(rt, "hasNet"),
			HasStorageScripts: rapid.Bool().Draw(rt, "hasStor"),
		}
		if rapid.Bool().Draw(rt, "secretsWired") {
			in.AdminSecretPresent, in.MonitorSecretPresent = propPtr(true), propPtr(true)
		}

		// Benign variations.
		for _, v := range rapid.SliceOfNDistinct(rapid.IntRange(0, 9), 0, 6, rapid.ID[int]).Draw(rt, "benign") {
			sib := &sibs[rapid.IntRange(0, nSib-1).Draw(rt, "benignSib")]
			switch v {
			case 0: // enrolled under the chassis serial, NetBox now says system serial: corroborated by commission
				target.NetBoxSerial = target.ChassisSerial
			case 1: // case and whitespace noise on the device serial
				in.Device.Serial = " " + strings.ToLower(in.Device.Serial) + " "
			case 2: // siblings with unprogrammed firmware identity: not ownership
				sib.SystemSerial, sib.ChassisSerial, sib.SystemUUID = "Default string", "To Be Filled By O.E.M.", ""
			case 3: // a sibling BMC sharing a MAC with our NIC is not a boot-identity clash
				sib.Interfaces = append(sib.Interfaces, model.SnapshotInterface{Name: "bmc", Type: "ipmi", MAC: target.Interfaces[0].MAC})
			case 4: // bond: our own two NICs share a MAC
				target.Interfaces[1].MAC = target.Interfaces[0].MAC
			case 5: // never-commissioned sibling
				sib.SystemSerial, sib.ChassisSerial, sib.SystemUUID, sib.Disks = "", "", "", nil
			case 6: // sibling disks with placeholder identity
				sib.Disks = append(sib.Disks, model.SnapshotDiskIdentity{Serial: "0000000000", WWN: "0x0000000000000000"})
			case 7: // "none" is the documented no-console opt-out
				target.KernelParams = "none"
			case 8: // device-level whitespace in SMBIOS uuid
				target.SystemUUID = " " + strings.ToUpper(target.SystemUUID) + " "
			case 9: // one of our disks has only a WWN
				target.Disks[1].Serial = ""
			}
		}

		var defect *propPFDefect
		if rapid.IntRange(0, 4).Draw(rt, "inject") > 0 {
			defect = &propPFDefects[rapid.IntRange(0, len(propPFDefects)-1).Draw(rt, "defect")]
			defect.apply(&in, &sibs[rapid.IntRange(0, nSib-1).Draw(rt, "defectSib")])
		}

		// The machine is normally listed among its own snapshot's machines.
		in.Machines = append([]model.MachineSnapshot{}, sibs...)
		if rapid.Bool().Draw(rt, "selfListed") {
			in.Machines = append(in.Machines, target)
		}

		for _, op := range []model.OperationType{model.OpDeploy, model.OpCommission} {
			gaps := PreflightMachine(in, op)
			applies := defect != nil && ((op == model.OpDeploy && defect.deploy) || (op == model.OpCommission && defect.commission))
			switch {
			case !applies && len(gaps) > 0:
				name := "none"
				if defect != nil {
					name = defect.name + " (does not apply to " + string(op) + ")"
				}
				rt.Fatalf("%s preflight reported gaps on a valid machine (defect: %s):\n  %s", op, name, strings.Join(gaps, "\n  "))
			case applies:
				found := false
				for _, g := range gaps {
					found = found || strings.Contains(g, defect.keyword)
				}
				if !found {
					rt.Fatalf("%s preflight missed defect %s (want a gap mentioning %q); gaps:\n  %s", op, defect.name, defect.keyword, strings.Join(gaps, "\n  "))
				}
				hits[defect.name]++
			default:
				clean++
			}

			// Order independence of the gap set.
			shuf := in
			m2 := target
			shuf.Machine = &m2
			r := rand.New(rand.NewSource(rapid.Int64Range(0, 1<<20).Draw(rt, "shuffle")))
			shuf.Machines = append([]model.MachineSnapshot(nil), in.Machines...)
			r.Shuffle(len(shuf.Machines), func(i, j int) { shuf.Machines[i], shuf.Machines[j] = shuf.Machines[j], shuf.Machines[i] })
			m2.Interfaces = append([]model.SnapshotInterface(nil), target.Interfaces...)
			r.Shuffle(len(m2.Interfaces), func(i, j int) { m2.Interfaces[i], m2.Interfaces[j] = m2.Interfaces[j], m2.Interfaces[i] })
			m2.Disks = append([]model.SnapshotDiskIdentity(nil), target.Disks...)
			r.Shuffle(len(m2.Disks), func(i, j int) { m2.Disks[i], m2.Disks[j] = m2.Disks[j], m2.Disks[i] })
			if a, b := propPFGapSet(gaps), propPFGapSet(PreflightMachine(shuf, op)); strings.Join(a, "|") != strings.Join(b, "|") {
				rt.Fatalf("%s preflight gap set depends on input order:\n  %v\n  %v", op, a, b)
			}
		}

		// A machine absent from every snapshot is one terminal gap.
		if g := PreflightMachine(PreflightInput{Device: in.Device, Images: in.Images}, model.OpDeploy); len(g) != 1 {
			rt.Fatalf("nil Machine must yield exactly one terminal gap, got %v", g)
		}
	})
	names := make([]string, 0, len(hits))
	for _, d := range propPFDefects {
		names = append(names, fmt.Sprintf("%s=%d", d.name, hits[d.name]))
	}
	t.Logf("coverage: clean_checks=%d defects_detected: %s", clean, strings.Join(names, " "))
}
