package snapshot

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// fullyModeledInput returns a PreflightInput that passes every deploy
// check: primary_ip4 set, a boot NIC with a MAC, a matched network and
// storage script, and a deploy image row. Individual tests knock out one
// field to assert the corresponding gap.
func fullyModeledInput() PreflightInput {
	machine := &model.MachineSnapshot{
		ID:             uuid.MustParse("00000000-0000-0000-0000-0000000000aa"),
		NetBoxDeviceID: new(42),
		NetBoxSerial:   "SN-HOST-A",
		Hostname:       "host-a",
		PXEEligible:    true,
		SystemSerial:   "SN-HOST-A",
		ChassisSerial:  "CHASSIS-HOST-A",
		SystemUUID:     "550e8400-e29b-41d4-a716-446655440000",
		Disks: []model.SnapshotDiskIdentity{
			{Serial: "DISK-A", WWN: "0x5000a"},
		},
		Interfaces: []model.SnapshotInterface{
			{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01", Type: "physical"},
			{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:ff", Type: "ipmi"},
		},
		NetworkScript: &model.SnapshotNetworkScript{Name: "pop1-l3", Content: "#!/bin/sh\n"},
		StorageScript: &model.SnapshotStorageScript{Name: "default", Content: "#!/bin/sh\n"},
		KernelParams:  "console=ttyS1,115200n8",
	}
	return PreflightInput{
		Machine:  machine,
		Machines: []model.MachineSnapshot{*machine},
		Device: &netbox.Device{
			ID:         42,
			Name:       "host-a",
			Serial:     "SN-HOST-A",
			PrimaryIP4: &netbox.NestedRef{ID: 7, Name: "10.0.0.20/24"},
		},
		Images: []model.Image{
			{Name: "noble-pxe"},
		},
		HasNetworkScripts:    true,
		HasStorageScripts:    true,
		AdminSecretPresent:   new(true),
		MonitorSecretPresent: new(true),
	}
}

//go:fix inline

func gapsContain(t *testing.T, gaps []string, substr string) {
	t.Helper()
	for _, g := range gaps {
		if strings.Contains(g, substr) {
			return
		}
	}
	t.Fatalf("expected a gap containing %q, got %v", substr, gaps)
}

func TestPreflight_FullyModeledDeployPasses(t *testing.T) {
	gaps := PreflightMachine(fullyModeledInput(), model.OpDeploy)
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps for a fully-modeled deploy, got %v", gaps)
	}
}

func TestPreflight_FullyModeledCommissionPasses(t *testing.T) {
	gaps := PreflightMachine(fullyModeledInput(), model.OpCommission)
	if len(gaps) != 0 {
		t.Fatalf("expected no gaps for a fully-modeled commission, got %v", gaps)
	}
}

func TestPreflight_LockedMachineRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.Locked = true
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "machine is locked")
}

func TestPreflight_PXEDisabledMachineRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.PXEEligible = false
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "not PXE-eligible")
}

func TestPreflight_MachineDeviceBindingConflictRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.NetBoxDeviceID = new(99)
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "machine binding conflict")
}

func TestPreflight_StableSerialNetBoxConflictRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Device.Serial = "SN-DIFFERENT"
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "NetBox serial conflict")
}

func TestPreflight_SystemChassisSerialTransitionRequiresCommissionCorroboration(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.NetBoxSerial = "CHASSIS-HOST-A"
	in.Device.Serial = "SN-HOST-A"
	for _, op := range []model.OperationType{model.OpCommission, model.OpDeploy} {
		for _, gap := range PreflightMachine(in, op) {
			if strings.Contains(gap, "NetBox serial conflict") {
				t.Fatalf("commission inventory corroborates both serials for %s, got %v", op, gap)
			}
		}
	}

	in.Machine.ChassisSerial = "CHASSIS-DIFFERENT"
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "NetBox serial conflict")
}

func TestPreflight_PlaceholderStableAndDeviceSerialRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.NetBoxSerial = "Default string"
	in.Device.Serial = "default string"
	gapsContain(t, PreflightMachine(in, model.OpCommission), "no stable enrolled NetBox serial")
}

func TestPreflight_CommissionSerialConflictRejectedOnDeploy(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.SystemSerial = "SN-WRONG"
	in.Machine.ChassisSerial = "CHASSIS-WRONG"
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "commission serial conflict")
	gapsContain(t, gaps, "commission/NetBox serial conflict")
}

func TestPreflight_MissingOrInvalidSystemUUIDRejectedOnDeploy(t *testing.T) {
	for _, systemUUID := range []string{"", "not-a-uuid", uuid.Nil.String()} {
		in := fullyModeledInput()
		in.Machine.SystemUUID = systemUUID
		gapsContain(t, PreflightMachine(in, model.OpDeploy), "system UUID")
	}
}

func TestPreflight_DuplicateSystemUUIDRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:         uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname:   "host-b",
		SystemUUID: in.Machine.SystemUUID,
	})
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "also owned by machine host-b")
}

func TestPreflight_ForeignSMBIOSSerialRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.ChassisSerial = "SN-HOST-B"
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:           uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname:     "host-b",
		NetBoxSerial: "sn-host-b",
	})
	gapsContain(t, PreflightMachine(in, model.OpDeploy), "already owned by machine host-b")
}

func TestPreflight_EqualSystemAndChassisSerialReportsOwnershipOnce(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.SystemSerial = "SN-HOST-A"
	in.Machine.ChassisSerial = "sn-host-a"
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:           uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname:     "host-b",
		NetBoxSerial: "SN-HOST-A",
	})
	count := 0
	for _, gap := range PreflightMachine(in, model.OpDeploy) {
		if strings.Contains(gap, "already owned by machine host-b") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("equal system/chassis serial should produce one ownership gap, got %d", count)
	}
}

func TestPreflight_PlaceholderSerialsDoNotCreateForeignOwnership(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.ChassisSerial = "Default string"
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:            uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname:      "host-b",
		SystemSerial:  "default string",
		ChassisSerial: "0000000000",
	})
	for _, gap := range PreflightMachine(in, model.OpDeploy) {
		if strings.Contains(gap, "already owned by machine host-b") {
			t.Fatalf("placeholder SMBIOS serial must not establish ownership, got %v", gap)
		}
	}
}

func TestPreflight_DuplicateDiskIdentityRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:       uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname: "host-b",
		Disks: []model.SnapshotDiskIdentity{
			{Serial: "disk-a", WWN: "0X5000A"},
		},
	})
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "disk serial DISK-A")
	gapsContain(t, gaps, "disk WWN 0x5000a")
}

func TestPreflight_PlaceholderDiskIdentityDoesNotCreateOwnership(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.Disks = []model.SnapshotDiskIdentity{{Serial: "0", WWN: "0x0000000000000000"}}
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:       uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname: "host-b",
		Disks:    []model.SnapshotDiskIdentity{{Serial: "0", WWN: "0x0000000000000000"}},
	})
	for _, gap := range PreflightMachine(in, model.OpDeploy) {
		if strings.Contains(gap, "disk serial") || strings.Contains(gap, "disk WWN") {
			t.Fatalf("placeholder disk identity must not establish ownership, got %v", gap)
		}
	}
}

func TestPreflight_DuplicatePhysicalMACRejectedForCommissionAndDeploy(t *testing.T) {
	for _, op := range []model.OperationType{model.OpCommission, model.OpDeploy} {
		in := fullyModeledInput()
		in.Machines = append(in.Machines, model.MachineSnapshot{
			ID:       uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
			Hostname: "host-b",
			Interfaces: []model.SnapshotInterface{
				{Name: "eno2", MAC: "AA:BB:CC:DD:EE:01", Type: "physical"},
			},
		})
		gapsContain(t, PreflightMachine(in, op), "physical NIC MAC")
	}
}

func TestPreflight_SharedManagementMACIgnored(t *testing.T) {
	in := fullyModeledInput()
	in.Machines = append(in.Machines, model.MachineSnapshot{
		ID:       uuid.MustParse("00000000-0000-0000-0000-0000000000bb"),
		Hostname: "host-b",
		Interfaces: []model.SnapshotInterface{
			{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:ff", Type: "ipmi"},
		},
	})
	for _, gap := range PreflightMachine(in, model.OpDeploy) {
		if strings.Contains(gap, "physical NIC MAC") {
			t.Fatalf("management MAC must not participate in host identity, got %v", gap)
		}
	}
}

func TestPreflight_NeverCommissionedMachineCanCommission(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.SystemSerial = ""
	in.Machine.ChassisSerial = ""
	in.Machine.SystemUUID = ""
	in.Machine.Disks = nil
	for _, gap := range PreflightMachine(in, model.OpCommission) {
		if strings.Contains(gap, "commission") || strings.Contains(gap, "system UUID") {
			t.Fatalf("first commission must not require persisted inventory, got %v", gap)
		}
	}
}

func TestPreflight_MissingPrimaryIP4Rejected(t *testing.T) {
	in := fullyModeledInput()
	in.Device.PrimaryIP4 = nil
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "primary_ip4")
}

func TestPreflight_NilDeviceRejectsPrimaryIP4(t *testing.T) {
	in := fullyModeledInput()
	in.Device = nil
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "primary_ip4")
}

func TestPreflight_NoBootNICRejected(t *testing.T) {
	in := fullyModeledInput()
	// Only a mgmt interface remains — no bootable NIC.
	in.Machine.Interfaces = []model.SnapshotInterface{
		{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:ff", Type: "ipmi"},
	}
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "bootable NIC")
}

func TestPreflight_PhysicalNICWithoutMACRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.Interfaces = []model.SnapshotInterface{
		{Name: "eno1", MAC: "", Type: "physical"},
	}
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "bootable NIC")
}

func TestPreflight_NetworkScriptsConfiguredButNoneMatchRejected(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.NetworkScript = nil // none matched
	in.HasNetworkScripts = true    // but scripts exist
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "no network-config script matches")
}

func TestPreflight_NoNetworkScriptsIsPlainDHCPIntent(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.NetworkScript = nil // none matched
	in.HasNetworkScripts = false   // and none configured -> plain DHCP, OK
	gaps := PreflightMachine(in, model.OpDeploy)
	for _, g := range gaps {
		if strings.Contains(g, "network-config script") {
			t.Fatalf("plain-DHCP intent must not produce a network-script gap, got %v", gaps)
		}
	}
}

func TestPreflight_NoStorageScriptMatchRejectedOnDeploy(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.StorageScript = nil
	in.HasStorageScripts = true
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "no storage-layout script matches")
}

func TestPreflight_StorageScriptNotRequiredOnCommission(t *testing.T) {
	in := fullyModeledInput()
	in.Machine.StorageScript = nil // commission has no commission data yet
	in.HasStorageScripts = true
	gaps := PreflightMachine(in, model.OpCommission)
	for _, g := range gaps {
		if strings.Contains(g, "storage-layout script") {
			t.Fatalf("commission must not require a storage script, got %v", gaps)
		}
	}
}

func TestPreflight_MissingImageRejected(t *testing.T) {
	// deploy and commission boot the SAME single image; the only requirement is
	// that the one image row exists. No row -> both ops are rejected.
	for _, op := range []model.OperationType{model.OpDeploy, model.OpCommission} {
		in := fullyModeledInput()
		in.Images = nil
		gaps := PreflightMachine(in, op)
		gapsContain(t, gaps, "no PXE image is registered")
	}
}

func TestPreflight_SingleImageSatisfiesBothOps(t *testing.T) {
	// One image row present -> neither deploy nor commission reports an image gap.
	for _, op := range []model.OperationType{model.OpDeploy, model.OpCommission} {
		in := fullyModeledInput()
		in.Images = []model.Image{{Name: "noble-pxe"}}
		for _, g := range PreflightMachine(in, op) {
			if strings.Contains(g, "PXE image") {
				t.Fatalf("op %s should not report an image gap with one image present, got %v", op, g)
			}
		}
	}
}

func TestPreflight_MissingKernelParamsRejectedForDeploy(t *testing.T) {
	// Empty slagworks_kernel_params makes the agent hard-refuse at
	// installing_bootloader; preflight must hoist that to op-create.
	in := fullyModeledInput()
	in.Machine.KernelParams = ""
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "slagworks_kernel_params")
}

func TestPreflight_KernelParamsNotRequiredForCommission(t *testing.T) {
	// Commission does not install a bootloader, so empty kernel_params is fine
	// (the console probe just skips). Guards against the deploy-only check
	// over-firing on commission — exactly the path a never-deployed host takes.
	in := fullyModeledInput()
	in.Machine.KernelParams = ""
	for _, g := range PreflightMachine(in, model.OpCommission) {
		if strings.Contains(g, "slagworks_kernel_params") {
			t.Fatalf("commission must not require kernel_params, got %v", g)
		}
	}
}

func TestPreflight_KernelParamsNoneIsValid(t *testing.T) {
	// "none" is the explicit opt-out for a device with no serial console (e.g.
	// a switch); it must NOT be treated as missing.
	in := fullyModeledInput()
	in.Machine.KernelParams = "none"
	for _, g := range PreflightMachine(in, model.OpDeploy) {
		if strings.Contains(g, "slagworks_kernel_params") {
			t.Fatalf("kernel_params=none is a valid opt-out, got gap %v", g)
		}
	}
}

func TestPreflight_NilMachineIsTerminalGap(t *testing.T) {
	in := fullyModeledInput()
	in.Machine = nil
	gaps := PreflightMachine(in, model.OpDeploy)
	if len(gaps) != 1 {
		t.Fatalf("nil machine should yield exactly one terminal gap, got %v", gaps)
	}
	gapsContain(t, gaps, "not present in any site snapshot")
}

func TestPreflight_MultipleGapsAllReported(t *testing.T) {
	in := fullyModeledInput()
	in.Device.PrimaryIP4 = nil
	in.Machine.Interfaces = nil
	in.Images = nil
	gaps := PreflightMachine(in, model.OpDeploy)
	if len(gaps) < 3 {
		t.Fatalf("expected at least 3 gaps (primary_ip4, boot NIC, image), got %v", gaps)
	}
}

func TestPreflight_MissingAdminSecretRejectedOnDeploy(t *testing.T) {
	in := fullyModeledInput()
	in.AdminSecretPresent = new(false)
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "bmc-creds-admin")
}

func TestPreflight_MissingAdminSecretRejectedOnCommission(t *testing.T) {
	// Commission power-cycles the host too, so it also needs the admin cred.
	in := fullyModeledInput()
	in.AdminSecretPresent = new(false)
	gaps := PreflightMachine(in, model.OpCommission)
	gapsContain(t, gaps, "bmc-creds-admin")
}

func TestPreflight_MissingMonitorSecretRejectedOnDeploy(t *testing.T) {
	in := fullyModeledInput()
	in.MonitorSecretPresent = new(false)
	gaps := PreflightMachine(in, model.OpDeploy)
	gapsContain(t, gaps, "bmc-creds-monitor")
}

func TestPreflight_MissingMonitorSecretNotRequiredForCommission(t *testing.T) {
	// Commission writes no monitor cred, so a missing monitor secret must NOT
	// block it.
	in := fullyModeledInput()
	in.MonitorSecretPresent = new(false)
	for _, g := range PreflightMachine(in, model.OpCommission) {
		if strings.Contains(g, "bmc-creds-monitor") {
			t.Fatalf("commission must not require the monitor secret, got gap %v", g)
		}
	}
}

func TestPreflight_NilSecretPointersSkipTheCheck(t *testing.T) {
	// No secrets backend wired (nil pointers) => the secret checks are skipped,
	// not failed. Models local-dev / NetBox-off.
	in := fullyModeledInput()
	in.AdminSecretPresent = nil
	in.MonitorSecretPresent = nil
	gaps := PreflightMachine(in, model.OpDeploy)
	for _, g := range gaps {
		if strings.Contains(g, "bmc-creds-") {
			t.Fatalf("nil secret pointers must skip the check, got gap %v", g)
		}
	}
}
