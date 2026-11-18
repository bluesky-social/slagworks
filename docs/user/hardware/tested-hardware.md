# Tested hardware

The hardware slagworks is known to run on, taken from evidence in this
repository: the storage scripts in `assets/storage/`, the vendor-specific
code in `internal/power` and `internal/bmcreconcile`, and the hardware
notes under `docs/`. **Anything not listed here is untested.** That
includes other models in the same vendor families: a different chassis
needs its own storage script, and its BMC may behave differently.

| Vendor / model | Role | BMC transport(s) | Storage script | Notes |
|---|---|---|---|---|
| ASUS RS501A-E12-RS4U | Compute node | IPMI and Redfish (bmclib); IPMI LAN user reconcile; Redfish time settings | `100-asus-rs501a`, `150-asus-rs501a-preserve` | AMI MegaRAC BMC. Small IPMI session pool. SOL on `ttyS1`. Boot RAID1 on the two smaller NVMe drives, RAID0 data array on the Kioxia NVMe drives; the preserve variant reassembles the data array instead of recreating it and is limited by hostname. |
| Supermicro AS -1115HS-TNR (H13SSH board) | Kubernetes worker, hot spare | SMCIPMITool when installed, else IPMI and Redfish (bmclib); IPMI LAN user reconcile; Redfish time settings | `200-supermicro-tnr-family`, `250-pop2-kworker-preserve-lvm` | AMI MegaRAC SP-X BMC with few IPMI session slots, which is why SMCIPMITool is preferred. Its IPMI rejects passwords that fail a complexity check with completion code `0xC8`, so generated BMC passwords mix four character classes. Both storage scripts are limited by hostname: the same chassis is used for roles slagworks does not image. |
| Supermicro AS -1015CS-TNR | Kubernetes worker, hot spare | As AS -1115HS-TNR | `200-supermicro-tnr-family`, `250-pop2-kworker-preserve-lvm` | Same layout and hostname limits as the AS -1115HS-TNR. Assumed to share its BMC behavior; not separately characterized. |
| Dell PowerEdge R640 | OOB gateway (runs `slagworksd site`), imaged by slagworks | IPMI LAN and Redfish (iDRAC); `ipmitool fru print` for the chassis serial when the BMC user has no Redfish role | `150-dell-r640` | IPMI over LAN is off on a fresh iDRAC and must be enabled. iDRAC timezone is set through its own attribute. SOL on `ttyS1`. Boot RAID1 across two SAS SSDs behind an LSI SAS3008 HBA. See [Dell R640 / iDRAC](dell-r640-idrac.md). |
| Minisforum MS-01 (SMBIOS product `Venus Series`) | Router | Intel AMT over WS-Man/TLS (power, one-time PXE); AMT redirection for the console | `150-minisforum-ms-01` | No BMC; the admin secret entry has `kind: "amt"`. AMT can only arm a one-time PXE boot (no disk or persistent override) and exposes no chassis serial. Boot RAID1 on the two NVMe drives; the script is limited by hostname because `Venus Series` is a family string. |
| Mellanox SN2700 (`MSN2700`, `MSN2700-CS2FO`) | Leaf/spine switch running Ubuntu and FRR | None: power through its switched PDU outlets (`--via pdu`); console from a port cabled to the site worker | `300-mellanox-sn2700` | Firmware boots legacy BIOS, so the script makes a `bios_grub` partition and the agent installs GRUB for BIOS. Single small disk. Two PSUs on two PDUs. |
| APC APDU9965, NMC firmware 2.5.3.1 | Switched PDU | NMC CLI over SSH (actuation), Redfish (outlet status) | n/a | The NMC's SSH server rejects Go's default client banner; slagworks sends an OpenSSH-style one. Concurrent SSH sessions are limited. See [PDU power control](../../dev/pdu-power-control.md). |

Sources, per row:

- ASUS RS501A: `assets/storage/100-asus-rs501a`,
  `assets/storage/150-asus-rs501a-preserve`,
  [BMC matrix](bmc-vendor-matrix.md), `internal/bmcreconcile/time.go`
  (AMI family).
- Supermicro TNR: `assets/storage/200-supermicro-tnr-family`,
  `assets/storage/250-pop2-kworker-preserve-lvm`,
  [BMC matrix](bmc-vendor-matrix.md), `internal/power/smc.go`,
  `internal/bmcreconcile/reconcile.go` (`generatePassword`),
  `internal/bmcreconcile/time.go`,
  [Supermicro SMCIPMITool](supermicro-smcipmitool.md).
- Dell R640: `assets/storage/150-dell-r640`, `internal/power/bmc.go`
  (`fruSerialViaIPMItool`), `internal/bmcreconcile/time.go` (iDRAC
  family), [Dell R640 / iDRAC](dell-r640-idrac.md).
- Minisforum MS-01: `assets/storage/150-minisforum-ms-01`,
  `internal/power/amt.go`, `internal/amt/redir`,
  `internal/qemutest/ms01_router_script_test.go` (the AMT-capable
  I226-LM port).
- Mellanox SN2700: `assets/storage/300-mellanox-sn2700`,
  `internal/power/pdu.go`, `docs/dev/pdu-power-control.md`.
- APC PDU: `internal/power/pdu_transport.go`,
  `docs/dev/pdu-power-control.md`.

## Not tested

- Supermicro AS -2125HS-TNR appears in the [BMC matrix](bmc-vendor-matrix.md)
  as part of the same fleet but has no storage script and no recorded
  slagworks run.
- `internal/bmcreconcile/time.go` names Tyan alongside ASUS as an AMI
  MegaRAC BMC family. No Tyan model has a storage script or other evidence
  of a run.
- The generic BMC path (bmclib over Redfish and IPMI) is not tied to a
  vendor, but no hardware beyond the rows above is known to have been
  driven by it.

To bring up something new, see [Adding new hardware](adding-new-hardware.md).
