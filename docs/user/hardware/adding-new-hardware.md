# Adding new hardware to slagworks

Status: live (generated from code, 2026-07-03)

This guide walks through everything a new server model or vendor touches when
you bring it under slagworks management. Slagworks selects per-host behaviour
by matching NetBox identity and DMI facts to shipped scripts and by dispatching
to vendor-specific BMC tooling by manufacturer slug; a new hardware class often
requires edits in several of those places. Work through the sections in order:
each one describes what the code expects, where to make the edit, and how to
verify it.

Cross-references to sibling docs in this directory:

- `bmc-vendor-matrix.md` — vendor-by-vendor IPMI/Redfish coverage.
- `dell-r640-idrac.md` — the exhaustive trap list from onboarding one vendor.
- `ipxe-local-boot-idioms.md` — `chain file://` vs `sanboot` for local boot.
- `ncsi-shared-lan-bmc.md` — shared-LAN/NCSI BMC gotchas.

## Prerequisites: NetBox modelling

Slagworks reads its per-host facts from NetBox. Before slagworks can do
anything with a new machine, the device record must carry enough identity for
the snapshot builder to key on. `internal/snapshot/builder.go` is the pure
function that turns NetBox rows plus Postgres state into a `SiteSnapshot`; the
sections below describe exactly what it consumes.

### Device type and manufacturer

Create the device type in NetBox under the correct manufacturer. Slagworks
keys on both:

- The manufacturer slug drives BMC-tool dispatch on the site worker
  (`cmd/slagworksd/site.go` — `supermicro` routes to SMCIPMITool when the
  operator has installed it, everything else uses `bmclib`/`ipmitool`). If
  you slugify the same vendor two different ways across two devices, one
  path will silently take a different code branch.
- The device-type slug is a match key for both network and storage scripts
  (see below), and its `slagworks_kernel_params` custom field is a hard
  precondition for deploy.

Duplicate-slug footgun: NetBox allows two device types with different slugs
that describe the same physical hardware (for example, one imported from a
vendor library and another created by hand). The snapshot builder groups by
slug, so a duplicate slug for the same chassis silently breaks any script
that matches on `device_type`: half the fleet resolves to slug A and matches,
half resolves to slug B and does not. This has burned us on Dell R640,
Mellanox MSN2700, and ASUS RS501A. When onboarding, check for an existing
device type with the same model name under the same manufacturer and reuse
it rather than creating a second slug.

### `slagworks_kernel_params` custom field

The device type's `slagworks_kernel_params` custom field is verbatim kernel
cmdline appended by the agent when it installs the bootloader. It is a hard
precondition for deploy: the deploy preflight in
`internal/snapshot/preflight.go` fails op creation, the boot handler refuses
to serve a deploy boot script, and the agent's bootloader install refuses to
run, if the field is empty. There is deliberately no fallback console — a
guessed `console=` produces a host whose serial output silently misses the
BMC SOL port.

**You usually don't set this by hand: commission determines it.** When the
field is empty (the normal state for a device type commission just created
from DMI inventory), the commission boot uses a broadcast console cmdline
covering every candidate tty, the agent probes each candidate with a unique
nonce marker (`consoleprobe.Sweep` in `internal/agent/consoleprobe/probe.go`),
and global resolves which nonce actually arrived over BMC SOL in
`console_cast_events` — then writes the winning `console=` back to the
device type (`internal/commission/consolesweep.go`, WARN-logged with the
evidence; it never overwrites a non-empty field). If no nonce arrives (no
SOL, no serial hardware), the field stays empty and deploy keeps refusing
until you set a value manually or the explicit opt-out:

- Serial-console box: `console=ttyS1,115200n8`. Put `ttyS0` last if you list
  more than one so `/dev/console` binds to a device that QEMU also emits on.
- No serial console at all: the literal string `none`. This is the explicit
  opt-out and passes preflight.

The old convention of a `# slagworks:kernel_params` comment in the storage
script is retired and now rejected at upload time (`internal/storage/script.go`
`rejectKernelParams`); use the NetBox custom field instead.

### Serial and identity

Set the NetBox device `serial` field to the FRU Product Serial (Dell service
tag, Supermicro chassis serial, and so on — the same value the BMC returns
from `ipmitool fru print 0` on the "Product Serial" line). This is the
canonical chassis identity slagworks uses to verify the BMC at a given IP is
the physical box NetBox claims it is (`internal/power/bmc.go`
`ChassisSerial`). A wrong serial means the ChassisSerial gate refuses to
power-cycle the host at deploy time.

Two subtleties on Dell hardware in particular: bmclib's `common.Device.Serial`
returns the baseboard (Board Serial) on iDRAC, not the service tag. Slagworks
therefore reads FRU Product Serial directly over `ipmitool` whenever IPMI
creds are available, and falls back to bmclib only when they are not.

### Interfaces and `mgmt_only`

Every physical interface the host boots on must exist as a NetBox interface
with a `primary_mac_address`. Model the BMC interface with `mgmt_only: true`
and set the device's `oob_ip` to the IP assigned to that interface. Slagworks
uses `oob_ip` as the canonical BMC-presence signal; names vary too much across
vendors and operator habit (`ipmi`, `ipmi0`, `bmc`, `mgmt`, `lom`,
`dedicated`) to key on. A `mgmt_only` interface on a device with `oob_ip`
becomes type `ipmi`. On a BMC-less device, such as an SN2700 switch, a
`mgmt_only` host management/PXE NIC remains type `physical` and participates
in DHCP machine identity.

A BMC-less device gets console capture (`slagctl machine logs --sol`) only
through its serial console: model a console port on the device and cable it
in NetBox to a console-server port on a site worker host, named for that
host's tty (`ttyS4`). See operations.md, "Console transports".

### `primary_ip4`

Set `primary_ip4` on the NetBox device. It is the host's canonical address
for DNS and identity, and preflight refuses to schedule a deploy or
commission op without it. Legacy L2-bond hosts sometimes carry
`primary_ip4=None` even when the bond interface has an IP — set it
explicitly rather than expecting slagworks to infer it.

## Storage scripts

Storage layouts are pluggable: an operator uploads a script, and slagworks
runs it inside the deploy initrd with `SLAGWORKS_INVENTORY` pointing at the
discovered block-device list. The script must print a JSON layout on stdout
matching the shape declared in `internal/storage/layout.go`.

### The matching engine

`internal/configscript/configscript.go` is the shared rule engine. Every
storage script's top-of-file comment block declares one or more
`slagworks:match` rules plus a `slagworks:priority`. All rules AND together,
priorities break ties, and if two scripts tie at the winning priority the
snapshot build errors out rather than picking one non-deterministically.
Regex values are written between slashes (`/^AS -(1115HS|1015CS)-TNR$/`) and
are auto-anchored `^…$`; plain values are exact string comparisons.

Valid storage match fields (`internal/storage/script.go`):

- `system_manufacturer`, `system_product` — DMI/commission facts. These are
  the primary key: a layout is chosen for a chassis model.
- `hostname` — the NetBox device name. Included as a defence-in-depth
  AND-constraint because chassis models are not role-unique: for example the
  Supermicro AS-Tx TNR family is shared by hosts we do image (`kworker-*`,
  `hot-spare-*`) and hosts we must not (application servers). A model-only
  match would silently pick up either. Sourced from the snapshot builder's
  NetBox lookup, not commission-time `os.Hostname()`.

The retired `slagworks:kernel_params` header on storage scripts is now
rejected at upload; kernel params come from the NetBox device-type custom
field instead.

### Layout contract

The script writes one JSON document to stdout with two top-level keys,
`layout` (device map) and `mounts`. `internal/storage/layout.go` documents
the shape; the interesting bits:

- Each `layout` entry is either a disk (`type: "disk"`, `ptable: "gpt"`,
  `partitions: […]`) or a RAID array (`type: "raid"`, `level`, `members`).
  Set `boot: true` on a disk that carries the ESP so the layout is
  self-describing (the flag itself is advisory today).
- Partitions carry `name`, `size` (`"512M"`, `"100G"`, or `"rest"`), an
  optional `fs` (`vfat`, `ext4`, `xfs`, `swap`), and one of `bootable: true`
  (marks the ESP, GPT type EF00) or `bios_grub: true` (marks a legacy BIOS
  boot partition, GPT type EF02, for the legacy-GRUB install path). The
  latter two are mutually exclusive with `fs`.
- Mounts are keyed by mount point and reference a device or partition name
  from the layout. The planner sorts by depth so `/` mounts before
  `/boot/efi`.

The planner (`internal/storage/plan.go`) translates the JSON into an ordered
list of `sgdisk`, `mdadm`, `mkfs.*`, `blockdev --rereadpt`, `mkdir`, and
`mount` commands. Anything the planner does not know how to express is a
layout you cannot describe with a storage script; extend the planner rather
than trying to smuggle raw shell into the script.

A worked example lives in `assets/storage/200-supermicro-tnr-family` (regex
model match plus a hostname AND-constraint, RAID1 boot mirror across two
NVMe boot drives, all other NVMe left bare). Keep new scripts under
`assets/storage/` in the same numeric-priority prefix style so
`TestHardwareAllowlist` picks them up.

### Uploading

Scripts are uploaded to the global API from a file:

```
slagctl storage-script upload --name <script-name> <file>
slagctl storage-script list
slagctl storage-script delete <id>
```

The API parses the script through `storage.ParseScript` at upload time, so a
typo in a match field name, a bad regex, or a lingering `slagworks:kernel_params`
header fails loudly rather than silently never matching.

### Hardware-allowlist census

`internal/snapshot/hardware_allowlist_test.go` is a fail-closed census: it
enumerates every distinct (role, device_type) class observed in the fleet
and asserts that shipped scripts (both network and storage) match ONLY the
cohorts explicitly on the allow list. A new hardware class that a shipped
script accidentally matches will fail this test with a "DENY BREACH"
message. This is intentional maintenance friction — onboarding a new class
requires editing the census to declare either that the class is allowed
(and something must match it) or that it must not be touched. That edit is
the gate.

### Preflight

`internal/snapshot/preflight.go` refuses to create a deploy op when storage
scripts are configured but none matches the host (`storage scripts are
configured, but none matched its commission data`). A commission op does not
need a match — commission runs before we have inventory to match on.

## Network scripts

Network-config scripts are the storage-script pattern applied to host
network setup. The site worker matches on NetBox identity, ships the winning
script plus the machine's raw interface facts to the agent, and the agent
runs the script in the deploy initrd with `SLAGWORKS_NETWORK_FACTS` pointing
at the facts JSON. The script emits a bundle of files and units to be
applied under `/target`. See `internal/snapshot/netscript.go` for the
matcher and `cmd/slagworks-agent/main.go` `applyNetworkConfig` /
`applyNetBundle` for the agent-side apply.

### When you need a netscript

You do not always need one. When no network scripts are configured
site-wide, or when scripts are configured but none matches this host, the
agent writes a built-in "all `en*` interfaces, DHCP, no gateway override"
netplan (`writeDefaultNetplan`) and the host comes up on whatever DHCP is
reachable. Preflight tolerates this case only when NO scripts exist site-wide;
if any script is configured but none matches THIS host, preflight refuses to
deploy — a host that should have gotten fabric config but silently comes up
DHCP-only is a latent break.

Write a netscript when the host needs anything the default DHCP netplan
cannot express: a static address, a bond, VLANs, a routed-NIC fabric with
FRR, or extra systemd units.

### Match fields

`NetworkScriptFields` in `internal/snapshot/netscript.go` gives the
vocabulary:

- `site` — the site slug (coarsest geographic key).
- `location` — a sub-site / cage within a site. This is how one physical
  site with more than one cage gets told apart under a single NetBox site.
- `rack` — the rack name (NetBox rack names, not slugs, since NetBox does
  not slug racks in its device serializer).
- `u` — the device's U-position within the rack (finest key). Formatted with
  minimal decimal representation so whole-U `12.0` matches `u=12`.
- `device_type` — the chassis slug, for topology-family cases where
  geography does not separate co-resident families.
- `hostname` — a regex over the device name. Brittle as a sole primary key
  because a rename silently breaks it, so use it as a defence-in-depth
  AND-constraint on top of a fact key.

Match rules AND, priorities break ties. Same tie-error and regex grammar as
storage scripts.

### Emit-bundle contract

Scripts print one JSON document on stdout:

```json
{
  "files": [
    {"path": "/etc/netplan/60-slagworks.yaml", "content": "…", "mode": "0600",
     "owner": "root", "group": "root"}
  ],
  "enable_units": ["frr.service"]
}
```

- `path` must be absolute. `applyNetBundle` resolves it through
  `securejoin.SecureJoin` under `/target` and enforces containment — a
  `../` path or an `/etc` that symlinks out of the target is rejected.
- `mode` is an octal string ("0644" default; setuid/setgid/sticky bits are
  mapped through `parseBundleMode`).
- Units in `enable_units` are enabled via `systemctl --root=/target enable`
  through chroot (files are written FIRST, before any bind-mounts, so a
  buggy path cannot ride a mounted `/sys` or `/dev` out of the target).

### Failure semantics

`applyNetworkConfig` fails the deploy on: script matched but execution
returned non-zero, output that does not parse as `netBundle`, or a path that
escapes `/target`. Silently coming up with the default DHCP netplan when a
script was supposed to run is treated as a fabric break — the same
"loud-fail over silent-wrong-state" posture the storage matcher takes.

### Facts consumed by the script

The site worker exposes `/api/v1/machines/<id>/network-facts` — a JSON list
of `model.SnapshotInterface` records (`NetBoxID`, `Name`, `MAC`, `Type`,
`IP`, `LAG`, `VLAN`, `MTU`, `Gateway`). `Type` is `physical`, `lag`, or
`ipmi`. `Gateway` is the longest-prefix-match of the interface CIDR against
site prefixes' `gateway_ip` custom field. The script owns all topology
interpretation from these facts; slagworks itself is topology-agnostic. See
`assets/network/100-hot-spare-pop2` for a worked L2-bond example.

### Uploading

```
slagctl network-script upload --name <script-name> <file>
slagctl network-script list
slagctl network-script delete <id>
```

Validated at upload time against `NetworkScriptFields` the same way storage
scripts are.

## BMC support

### The credential model

Slagworks drives a BMC with exactly one credential: the device's
`kind=bmc` entry in the pop's `pop/<pop>/bmc-creds-admin` AWS Secrets
Manager secret, keyed by NetBox device serial. It must be the BMC's
slot-2 admin account. Nothing is stored in Postgres, and slagworks
provisions no account of its own. A BMC op on a device without a usable
entry is refused at creation with the secret path and serial to fix.

The only account slagworks provisions is the read-only `monitor`
(USER), stored in `pop/<pop>/bmc-creds-monitor` for ipmi_exporter.

Preflight refuses deploy or commission when the admin secret is missing
entirely (`AdminSecretPresent == false`), and deploy specifically also
requires `pop/<pop>/bmc-creds-monitor` to exist (deploy_complete writes the
per-host monitor cred there, and a missing secret used to make the handler
replay indefinitely).

### Vendor dispatch

`internal/power/power.go` defines the `Controller` interface (power on/off/
cycle/status, `SetBootDevice`, `ChassisSerial`, `Transport`, `Close`) and an
optional `BootManager` capability (`BootStatus` and `SetBootOverride` for
operator-facing boot-order reads/writes). Two BMC implementations today:

- `BMCController` (`internal/power/bmc.go`) — the default. Uses
  `bmclib` for Redfish/IPMI. AMI Aptio firmware needs cipher suite 3;
  other firmware takes suite 17 or the bmclib default. The BMC's
  chassis serial is read via `ipmitool fru print 0` whenever IPMI creds
  are available (because bmclib's ipmitool provider has no
  `InventoryGetter`, and Dell iDRAC's Redfish returns baseboard serial
  instead of the service tag). No `BootManager` implementation yet.
- `SMCController` (`internal/power/smc.go`) — Supermicro-first. Routes
  power/boot/serial through Supermicro's `SMCIPMITool`, which operators
  install themselves (see [Supermicro SMCIPMITool](supermicro-smcipmitool.md)).
  Implements both `Controller` and `BootManager`. SOL stays on ipmitool
  regardless.

The factory lives in `cmd/slagworksd/site.go` (`buildPowerController`). It
dispatches on the manufacturer slug slagworksd stamped into the snapshot
from NetBox:

```
if m.IsSupermicro() && tool.Mode != smcipmitool.ModeOff {
    if bin, ok := tool.Lookup(); ok {
        return power.NewSMCController(...)  // SMCIPMITool path
    }
    // missing: error in required mode, otherwise fall through
}
// Default: bmclib
ctrl, err := power.NewBMCController(ctx, host, user, pass, 30*time.Second, nil, opts...)
```

### Adding a new vendor

Two options depending on how well the vendor works over bmclib:

1. If bmclib's Redfish/IPMI providers work well enough for the vendor, no
   controller change is needed — the default `BMCController` path already
   covers you. Add the manufacturer slug to NetBox device types, verify the
   cipher-suite override in `BuildBMCDialOptions` matches what the firmware
   accepts, and confirm `ipmitool fru print 0` returns a usable Product
   Serial (this is what `ChassisSerial` reads for credentialed hosts).
2. If the vendor needs a different channel to its BMCs, write a new
   `Controller` implementation in `internal/power/` and dispatch to it from
   the factory on `m.Manufacturer`. It must satisfy `Controller`, may
   satisfy `BootManager`, and reports its wire path from `Transport()`.
   Prefer a pure-Go client; do not vendor proprietary vendor binaries into
   this repository.

If you add a new controller, wire the manufacturer into the census
(`TestHardwareAllowlist`) and confirm at least one script (net or storage)
matches the new class.

### BMC account reconcile

`internal/bmcreconcile/reconcile.go` provisions the `monitor` (USER) account
and disables and clears every other named account outside slots 1-2 (slot 2
is the bmc-creds-admin account and is never touched). It runs two ways:

- In-band, once per deploy, from the agent over `/dev/ipmi0`. In-band IPMI
  is ADMINISTRATOR-equivalent regardless of the BMC's network privilege
  model, so no network cred is needed; the boot handler passes only the
  admin account's name (`slagworks.bmc_admin_user`, URL-query-encoded) so
  the agent can refuse, before any write, when that account isn't at
  slot 2. Provision-once: a password is only written for an absent or
  disabled monitor slot, so the stored monitor cred stays aligned with the
  BMC across redeploys. It also re-asserts the persistent PXE boot order.
- Over the LAN, on demand, via `slagctl machine bmc-reconcile <machine>`
  (any host, including in-service hosts slagworks never deployed). The site
  worker logs in with the admin entry, checks the chassis serial, refuses
  unless the admin account is slot 2, re-asserts the monitor password
  stored in `pop/<pop>/bmc-creds-monitor` (generating one only for a device
  with none stored, or with `--rotate-monitor`), and never touches boot
  order. It then enforces the BMC's time settings
  over Redfish (`internal/bmcreconcile/time.go`): NTP on with the OOB
  prefix's `ntp_servers`, timezone UTC, and a clock more than 60s off
  stepped. A new BMC class needs the standard Manager
  `NetworkProtocol` NTP PATCH and either `DateTimeLocalOffset` or (iDRAC)
  the `Time.1.Timezone` attribute to work; the vendor family is picked from
  the Redfish manager Id (`iDRAC.Embedded.1`, `Self` for AMI, `1` for
  Supermicro). See "Time compliance" in `../operations.md`.

For deep vendor-specific traps (Redfish password writes not propagating to
IPMI, AMI session-slot exhaustion, iDRAC user roles, etc.), read
`bmc-vendor-matrix.md` and the vendor-specific docs in this directory
(currently `dell-r640-idrac.md`) before adding a new class.

## Boot and firmware quirks

### BIOS vs UEFI

Both are supported. The agent detects boot mode from firmware and picks the
bootloader path:

- UEFI: `systemd-boot` written into the ESP mounted at `/boot/efi`. Layout
  must declare a `bootable: true` (EF00) partition mounted at `/boot/efi`,
  or the agent refuses the deploy.
- Legacy BIOS: legacy GRUB (i386-pc) installed onto every disk carrying a
  `bios_grub: true` (EF02) partition. Layout must declare at least one, or
  the agent refuses. Installing onto every EF02-carrying disk keeps a RAID1
  root bootable if either member disk is lost.

Set the layout appropriately in the storage script; the agent will refuse
to deploy a layout inconsistent with the firmware mode.

### Serial console

The kernel cmdline comes from the NetBox device-type
`slagworks_kernel_params` custom field (see above), not from the storage
script. Set it once per device-type. Common patterns:

- Single serial console at ttyS1 (COM2): `console=ttyS1,115200n8`.
- Multiple consoles: list them in preference order, with `ttyS0` last so
  `/dev/console` in QEMU still binds to something visible.
- No serial console (e.g. a switch that has none, or a headless-graphics-only
  chassis): the literal string `none`.

### iPXE local-boot idioms

When a deployed host PXE-boots back to slagworks (the default post-deploy
behaviour), the site worker hands it an iPXE snippet that returns control to
firmware-boot-order. Different firmware vendors reject different snippet
shapes: some accept `chain file://` from iPXE, some need `sanboot`, some
require an `exit`. See `ipxe-local-boot-idioms.md` for the full matrix.

### Shared-LAN / NCSI BMCs

Some AST2600-based Supermicro boards can piggyback the BMC's ethernet on a
host data NIC (NCSI, "shared LAN"). This has interactions with PXE, MAC
learning, and post-deploy connectivity that are worth reading before
onboarding a chassis that uses it. See `ncsi-shared-lan-bmc.md`.

## Verification checklist

Once the pieces above are in place:

1. Commission the host: `slagctl machine commission -w <machine>`. This
   runs the discovery agent and populates `commission_data` in the machine
   row; the DMI fields your storage script matches on come from here.
2. Confirm the site snapshot has picked up the matches: `slagctl machine
   show <machine>` should list the winning storage script and network
   script (or explicitly report "no script matched" / "no scripts configured
   site-wide"). Snapshot builds also fail loudly on ambiguous ties.
3. Run `TestHardwareAllowlist` locally (`just test`) — an accidental broad
   match will fail here rather than in production.
4. Deploy: `slagctl machine deploy --destructive <machine>` (destructive is
   currently required for kworker-class deploys) and watch with
   `slagctl operation watch <op-id>`. Preflight surfaces every static
   modelling gap up front; a green preflight and a red deploy means a
   dynamic mismatch (BMC unreachable, layout constraints violated by
   inventory) that the logs will name explicitly.

If a script matches a host that must never be imaged, add the host to
`TestHardwareAllowlist` as a not-allowed class and narrow the script until
the test passes. Do not weaken the census; it is intentionally the gate.
