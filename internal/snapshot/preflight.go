package snapshot

import (
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// PreflightInput is the fully-resolved view of one machine that
// PreflightMachine validates. Every field is the OUTPUT of the same
// build path the boot exercises (Build's per-machine matching, plus the
// NetBox device record), so the validator cannot pass while the real
// boot fails: it inspects exactly what the boot would consume.
//
//   - Machine: the MachineSnapshot Build produced (resolved NetworkScript,
//     StorageScript, Interfaces with MACs). Nil means the machine isn't in
//     the site snapshot at all (unbound device, or device in a different
//     site's scope) — a fatal gap on its own.
//   - Device: the live NetBox device, for fields the snapshot does not
//     carry (primary_ip4 lives on the device, not the MachineSnapshot).
//   - Images: the site snapshot's image rows; preflight asserts the row of
//     the kind the op needs (deploy/commission) resolves.
//   - HasNetworkScripts / HasStorageScripts: whether ANY script of that
//     class is configured site-wide. Build returns a nil matched-script
//     both when no scripts exist (plain-DHCP / default-layout intent, OK)
//     and when scripts exist but none matched this host (a real modeling
//     gap). These booleans disambiguate the two.
type PreflightInput struct {
	Machine *model.MachineSnapshot
	// Machines is the complete machine set from the same site snapshot as
	// Machine. Identity ownership checks use it to reject a UUID, disk, or
	// physical NIC already associated with a different machine.
	Machines          []model.MachineSnapshot
	Device            *netbox.Device
	Images            []model.Image
	HasNetworkScripts bool
	HasStorageScripts bool

	// AWS-secret presence for the device's pop, resolved by the API layer
	// (DescribeSecret) since the pure validator does no I/O. Nil means the
	// check could not run (no secrets backend wired — local-dev / NetBox-off /
	// tests that don't exercise secrets); a nil pointer SKIPS the check rather
	// than failing closed, matching the "validate only what we can" posture of
	// the rest of preflight. A non-nil false is a real gap.
	//
	//   - AdminSecretPresent: pop/<pop>/bmc-creds-admin exists. Required for any
	//     BMC-actuating op (deploy/commission both power-cycle): it's the
	//     only BMC credential and the only source for PDU creds.
	//   - MonitorSecretPresent: pop/<pop>/bmc-creds-monitor exists. Required for
	//     deploy: deploy_complete upserts the per-host monitor cred into it, and
	//     a missing secret made that handler fail-and-replay forever (the
	//     cred-drift storm). Commission writes no monitor cred, so it
	//     doesn't need this one.
	AdminSecretPresent   *bool
	MonitorSecretPresent *bool
}

// GapMachineLocked is the gap PreflightMachine reports for a locked machine.
// It is exported because it is the one gap that is about the machine's lock
// and not its model: the API words its refusal around it instead of calling
// the model incomplete.
const GapMachineLocked = "the machine is locked and excluded from lifecycle operations (a machine is locked automatically when it enters service); check that it is safe to commission or deploy it, then run `slagctl machine unlock <machine>` and retry"

// PreflightMachine validates that the NetBox/Postgres model carries
// everything the boot for opType requires, and returns one human-readable
// gap string per missing precondition. An empty slice means the op may
// proceed. Each gap names WHAT is missing and WHAT to do, so the operator
// can fix it without booting the host to discover the failure.
//
// The checks intentionally validate only what TODAY's facts pipeline can
// model. L2-bond-specific per-script fact assertions can slot in here behind
// the network-script match without restructuring the rest.
func PreflightMachine(in PreflightInput, opType model.OperationType) []string {
	var gaps []string

	if in.Machine == nil {
		// No MachineSnapshot for this machine in its site's snapshot:
		// either the machine isn't bound to a NetBox device, or the device
		// sits outside any site_config's claimed scope. Either way the boot
		// path has nothing to act on. This is terminal — the other checks
		// dereference Machine, so return immediately.
		return []string{"the machine is not present in any site snapshot (no NetBox device binding, or its device is outside every site_config's scope); bind the machine to a NetBox device in a managed site"}
	}

	// Preflight is the operator-facing answer to "would this operation be
	// accepted now?" Keep the durable lock/PXE gates visible here instead of
	// returning OK and then rejecting the immediately-following submission.
	if in.Machine.Locked {
		gaps = append(gaps, GapMachineLocked)
	}
	if opPXEBoots(opType) && !in.Machine.PXEEligible {
		gaps = append(gaps, "the machine is not PXE-eligible; run `slagctl machine pxe-enable` (or submit with --pxe-enable) before commission or deploy")
	}

	// Identity is a safety boundary, not merely modeling completeness. These
	// checks compare the stable enrollment binding, current NetBox device,
	// persisted commission identity, and sibling ownership before a deploy can
	// reach destructive disk work. Commission keeps accepting a host with no
	// prior inventory, but still refuses an ambiguous boot MAC or binding drift.
	gaps = append(gaps, preflightIdentityGaps(in, opType)...)

	// primary_ip4 (binding requirement): a host with no canonical address
	// has no FQDN/DNS/identity anchor. Legacy L2 hosts carry primary_ip4=None
	// even when the bond interface has an IP, so this is checked explicitly
	// against the device record, not inferred from interface IPs.
	if in.Device == nil || in.Device.PrimaryIP4 == nil {
		gaps = append(gaps, "the NetBox device has no primary_ip4 set; assign a primary IPv4 on the device (NetBox: Device > Edit > Primary IPv4) so the host has a canonical address for DNS/identity")
	}

	// Boot NIC: the DHCP offer + initrd NIC match both key on a modeled MAC.
	// A physical (non-mgmt) interface with a MAC must exist, or the host can
	// neither be offered a lease nor self-identify in the initrd.
	if !hasBootNIC(in.Machine.Interfaces) {
		gaps = append(gaps, "no bootable NIC is modeled with a MAC address (need at least one non-management interface carrying a MAC in NetBox); add the boot interface and its MAC so DHCP and the initrd NIC match can succeed")
	}

	// Network-config script: a matched script (or explicit plain-DHCP intent,
	// i.e. no network scripts configured at all) is required. Scripts
	// configured but none matching THIS host is a modeling gap — the host
	// would deploy with no working permanent network.
	if in.Machine.NetworkScript == nil && in.HasNetworkScripts {
		gaps = append(gaps, "no network-config script matches this host (network scripts are configured site-wide, but none matched its NetBox identity site/device_type/hostname); add a matching network script or correct the device's NetBox identity")
	}

	// Storage script: deploy pipes live inventory through the matched storage
	// script to lay out disks. A deploy with no matching script hard-fails
	// late; hoist that here. Commission collects inventory and does
	// not partition, so it does not need a storage match (a never-commissioned
	// host has no commission data for the match to even run against).
	if opType == model.OpDeploy && in.Machine.StorageScript == nil && in.HasStorageScripts {
		gaps = append(gaps, "no storage-layout script matches this host (storage scripts are configured, but none matched its commission data system_product/system_manufacturer); add a matching storage script or re-commission so the inventory is current")
	}

	// Image row: deploy and commission boot the SAME single PXE image (mode is
	// a kernel cmdline, not a separate image), so the only requirement is that
	// the one image row is registered. opPXEBoots gates this to the ops that
	// actually PXE-boot an image.
	if opPXEBoots(opType) && len(in.Images) == 0 {
		gaps = append(gaps, "no PXE image is registered (the site snapshot carries no image row); pin one with `slagctl image follow-latest`")
	}

	// Kernel params: a deploy installs a bootloader whose kernel cmdline comes
	// from the device-type's slagworks_kernel_params (NetBox custom field).
	// The agent HARD-REFUSES the deploy at installing_bootloader if
	// that field is empty (a deployed host with no console= would have no
	// serial console). That check fires deep in the deploy — after partition +
	// rsync — so hoist it here: it is a static NetBox-data precondition fully
	// knowable at op-creation time. KernelParams carries the device-type field
	// verbatim (snapshot build); empty string means unset. "none" is the valid
	// opt-out (device legitimately has no SOL, e.g. a switch) and must pass.
	if opType == model.OpDeploy && in.Machine.KernelParams == "" {
		gaps = append(gaps, "the NetBox device-type has no slagworks_kernel_params set; the deploy would hard-fail at bootloader install. Set the device-type custom field slagworks_kernel_params (e.g. \"console=ttyS1,115200n8\", or \"none\" if the device has no serial console)")
	}

	// AWS-secret preconditions. A nil pointer means the check couldn't run
	// (no secrets backend) and is skipped — only a non-nil false is a gap.
	//
	// Admin secret: every BMC-actuating op needs it (deploy + commission both
	// power-cycle the host through the BMC, which slagworks authenticates to
	// only with the device's admin entry, and PDU power draws from the same
	// secret). Without it the op fails at bmc-open.
	if opPXEBoots(opType) && in.AdminSecretPresent != nil && !*in.AdminSecretPresent {
		gaps = append(gaps, "the pop's BMC admin secret (pop/<pop>/bmc-creds-admin) does not exist in AWS Secrets Manager; slagworksd has no credential to power/boot this host's BMC. Create the pop's admin secret (layout: docs/user/configuration.md, \"Secrets in AWS Secrets Manager\") before deploying")
	}
	// Monitor secret: deploy only. The deploy_complete handler upserts the
	// per-host monitor cred into pop/<pop>/bmc-creds-monitor; a missing secret
	// makes that upsert fail and the observation replay indefinitely (the
	// cred-drift storm). Refuse up front rather than deploy into the storm.
	if opType == model.OpDeploy && in.MonitorSecretPresent != nil && !*in.MonitorSecretPresent {
		gaps = append(gaps, "the pop's BMC monitor secret (pop/<pop>/bmc-creds-monitor) does not exist in AWS Secrets Manager; the deploy_complete handler would fail to store the host's monitor credential and the observation would replay indefinitely. Bootstrap the pop's monitor secret (creating a site-config now does this automatically) before deploying")
	}

	return gaps
}

// preflightIdentityGaps validates all identity evidence available in the
// authoritative site snapshot. Live SMBIOS/BMC verification still occurs at
// boot/execution; this catches stale or cross-attributed persisted inventory
// before an operation is created.
func preflightIdentityGaps(in PreflightInput, opType model.OperationType) []string {
	m := in.Machine
	var gaps []string

	if in.Device != nil {
		if m.NetBoxDeviceID == nil || *m.NetBoxDeviceID != in.Device.ID {
			gaps = append(gaps, fmt.Sprintf("machine binding conflict: snapshot is bound to NetBox device %s but preflight resolved device %d (%q); reconcile the machine-to-device binding",
				netBoxDeviceID(m.NetBoxDeviceID), in.Device.ID, in.Device.Name))
		}
		stableSerial := model.NormalizeHardwareSerial(m.NetBoxSerial)
		deviceSerial := model.NormalizeHardwareSerial(in.Device.Serial)
		switch {
		case stableSerial == "":
			gaps = append(gaps, "the machine has no stable enrolled NetBox serial in Postgres; repair its machine-to-device binding before lifecycle work")
		case deviceSerial == "":
			gaps = append(gaps, fmt.Sprintf("NetBox device %q has no serial; set its hardware serial before lifecycle work", in.Device.Name))
		case !strings.EqualFold(stableSerial, deviceSerial) && !commissionCorroboratesSerials(*m, stableSerial, deviceSerial):
			gaps = append(gaps, fmt.Sprintf("NetBox serial conflict: machine %s was enrolled as %q but device %q currently reports %q; refusing identity drift",
				machineLabel(*m), stableSerial, in.Device.Name, deviceSerial))
		}
	}

	// A physical/non-management MAC is the boot-time identity key. Any MAC
	// shared by another machine is ambiguous for both commission and deploy.
	for _, iface := range m.Interfaces {
		mac := normalizeIdentity(iface.MAC)
		if mac == "" || iface.Type == "ipmi" {
			continue
		}
		for _, other := range in.Machines {
			if other.ID == m.ID {
				continue
			}
			for _, otherIface := range other.Interfaces {
				if otherIface.Type == "ipmi" || normalizeIdentity(otherIface.MAC) != mac {
					continue
				}
				gaps = append(gaps, fmt.Sprintf("physical NIC MAC %s is also owned by machine %s; make the NetBox MAC ownership unique before booting either host",
					iface.MAC, machineLabel(other)))
			}
		}
	}

	// Never-commissioned machines deliberately have no persisted SMBIOS or
	// disks and must remain commissionable. Deploy, however, must have a valid,
	// uniquely-owned identity and inventory before it can touch storage.
	if opType != model.OpDeploy {
		return gaps
	}

	stableSerial := model.NormalizeHardwareSerial(m.NetBoxSerial)
	systemSerial := model.NormalizeHardwareSerial(m.SystemSerial)
	chassisSerial := model.NormalizeHardwareSerial(m.ChassisSerial)
	if systemSerial == "" && chassisSerial == "" {
		gaps = append(gaps, "the last commission result has no usable SMBIOS system or chassis serial; re-commission the host before deploy")
	} else if stableSerial != "" && !matchesAnySerial(stableSerial, systemSerial, chassisSerial) {
		gaps = append(gaps, fmt.Sprintf("commission serial conflict: machine %s was enrolled as %q but its persisted inventory reports system %q and chassis %q; re-commission and verify the physical host",
			machineLabel(*m), stableSerial, m.SystemSerial, m.ChassisSerial))
	}
	if in.Device != nil {
		deviceSerial := model.NormalizeHardwareSerial(in.Device.Serial)
		if deviceSerial != "" && (systemSerial != "" || chassisSerial != "") &&
			!matchesAnySerial(deviceSerial, systemSerial, chassisSerial) {
			gaps = append(gaps, fmt.Sprintf("commission/NetBox serial conflict: device %q reports %q but persisted inventory reports system %q and chassis %q; reconcile identity before deploy",
				in.Device.Name, deviceSerial, m.SystemSerial, m.ChassisSerial))
		}
	}
	for _, other := range in.Machines {
		if other.ID == m.ID {
			continue
		}
		for _, reported := range uniqueSerials(systemSerial, chassisSerial) {
			if reported != "" && matchesAnySerial(reported, other.NetBoxSerial, other.SystemSerial, other.ChassisSerial) {
				gaps = append(gaps, fmt.Sprintf("SMBIOS serial %q from this machine's persisted inventory is already owned by machine %s; refusing cross-attributed commission inventory",
					reported, machineLabel(other)))
			}
		}
	}

	targetUUID, err := uuid.Parse(strings.TrimSpace(m.SystemUUID))
	if err != nil || targetUUID == uuid.Nil {
		gaps = append(gaps, fmt.Sprintf("the persisted SMBIOS system UUID %q is missing or invalid; re-commission the host before deploy", m.SystemUUID))
	} else {
		for _, other := range in.Machines {
			if other.ID == m.ID {
				continue
			}
			otherUUID, otherErr := uuid.Parse(strings.TrimSpace(other.SystemUUID))
			if otherErr == nil && otherUUID == targetUUID {
				gaps = append(gaps, fmt.Sprintf("SMBIOS system UUID %s is also owned by machine %s; refusing cross-attributed commission inventory",
					targetUUID, machineLabel(other)))
			}
		}
	}

	for _, disk := range m.Disks {
		for _, other := range in.Machines {
			if other.ID == m.ID {
				continue
			}
			for _, owned := range other.Disks {
				if value := duplicateIdentity(disk.Serial, owned.Serial); value != "" {
					gaps = append(gaps, fmt.Sprintf("disk serial %s is also owned by machine %s; refusing cross-attributed disk inventory",
						value, machineLabel(other)))
				}
				if value := duplicateIdentity(disk.WWN, owned.WWN); value != "" {
					gaps = append(gaps, fmt.Sprintf("disk WWN %s is also owned by machine %s; refusing cross-attributed disk inventory",
						value, machineLabel(other)))
				}
			}
		}
	}

	return gaps
}

func normalizeIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func matchesAnySerial(expected string, candidates ...string) bool {
	want := normalizeIdentity(model.NormalizeHardwareSerial(expected))
	if want == "" {
		return false
	}
	for _, candidate := range candidates {
		if got := normalizeIdentity(model.NormalizeHardwareSerial(candidate)); got != "" && got == want {
			return true
		}
	}
	return false
}

// commissionCorroboratesSerials accepts the intentional transition where a
// machine was enrolled under one usable SMBIOS serial (for example chassis)
// and current NetBox uses the other (system). Both values must independently
// appear in the same persisted commission identity; otherwise the mismatch is
// uncorroborated drift and remains a gap.
func commissionCorroboratesSerials(m model.MachineSnapshot, enrolled, current string) bool {
	return matchesAnySerial(enrolled, m.SystemSerial, m.ChassisSerial) &&
		matchesAnySerial(current, m.SystemSerial, m.ChassisSerial)
}

func uniqueSerials(values ...string) []string {
	unique := make([]string, 0, len(values))
	for _, value := range values {
		value = model.NormalizeHardwareSerial(value)
		if value == "" || matchesAnySerial(value, unique...) {
			continue
		}
		unique = append(unique, value)
	}
	return unique
}

func duplicateIdentity(a, b string) string {
	normalizedA := normalizeIdentity(model.NormalizeHardwareIdentifier(a))
	normalizedB := normalizeIdentity(model.NormalizeHardwareIdentifier(b))
	if normalizedA != "" && normalizedA == normalizedB {
		return strings.TrimSpace(a)
	}
	return ""
}

func machineLabel(m model.MachineSnapshot) string {
	if m.Hostname != "" {
		return fmt.Sprintf("%s (%s)", m.Hostname, m.ID)
	}
	return m.ID.String()
}

func netBoxDeviceID(id *int) string {
	if id == nil {
		return "<none>"
	}
	return fmt.Sprint(*id)
}

// hasBootNIC reports whether the machine has at least one non-management
// interface with a MAC address — the minimum to receive a DHCP lease and
// match in the initrd.
func hasBootNIC(ifaces []model.SnapshotInterface) bool {
	for _, i := range ifaces {
		if i.Type == "ipmi" {
			continue
		}
		if i.MAC != "" {
			return true
		}
	}
	return false
}

// opPXEBoots reports whether an op type PXE-boots the slagworks image (and so
// requires the image row to be registered). Deploy and commission both boot the
// single PXE image; other op types (power ops) don't.
func opPXEBoots(opType model.OperationType) bool {
	switch opType {
	case model.OpDeploy, model.OpCommission:
		return true
	default:
		return false
	}
}
