# Extensibility

Status: live (generated from code, 2026-07-03)

This document is for engineers who need to add a new hardware vendor,
a new operation type, a new commission field, or otherwise extend one
of slagworks' seams. Each section names the contract, the
registration point, and the test expectation.

The seams are:

- **The `configscript` engine** — matches a per-host script (storage
  layout, host network config) by header directives.
- **Storage layout** — the shape a storage script emits, and the
  planner that turns it into shell commands.
- **Netscript** — the network-config script contract, the interface
  facts the agent injects, and the file+unit bundle the script emits.
- **Power / BMC vendor dispatch** — `power.Controller` and its
  optional `power.BootManager` capability, plus the PDU transport.
- **Commission inventory** — the agent's collectors, the wire
  envelope, and the NetBox sync.
- **Operation types** — the enum, the executor dispatch, and the
  routing invariant.
- **Boot actions** — the iPXE scripts the boot handler emits and the
  cmdline the agent parses.

## The configscript engine

Package: `internal/configscript`. One file, ~190 lines.

Both storage scripts and host network-config scripts share a single
match engine. Callers supply the field vocabulary and a per-host
resolver; the engine parses headers, evaluates rules, and returns the
highest-priority match.

### Header grammar

Only top-of-file comment lines are scanned. Scanning stops at the
first non-comment line (`configscript.go:51-62`). Shebangs are
skipped. Two directives:

- `# slagworks:match <field>=<value>` — one match rule, ANDed with
  the others.
- `# slagworks:priority <int>` — tiebreaker on ties among matching
  scripts, higher wins (`configscript.go:95-100`).

`<value>` is either a literal string (exact equality against the
resolver's result) or a `/regex/` (anchored `^(?:...)$`,
`configscript.go:86-93`). A `slagworks:match` against a field not in
the caller's `validFields` map is a parse error, so a typo fails at
upload time rather than silently never matching
(`configscript.go:83-85`).

Any whitespace may separate a directive from its argument. A header
line whose first word starts with `slagworks:` in any case is a
directive, and any directive other than the two above
(`slagworks:mtach`, `slagworks:match:`, `Slagworks:match`, a bare
`slagworks:`) is a parse error (`configscript.go:101-102`). Treating
it as prose would drop the rule or priority without a word, which
widens the script's match or lets a lower-priority script win. Prose
that mentions slagworks without that colon prefix is unaffected.

### Selection

`configscript.Match(scripts, resolve)` sorts by priority descending,
walks in order, and:

- Returns the first script whose rules all evaluate to true.
- If two scripts tie at the winning priority AND both match, returns
  an error rather than picking one non-deterministically
  (`configscript.go:123-161`). Priority is the tiebreaker; a tie is a
  configuration bug the operator must resolve (raise one script's
  priority, or tighten a rule).
- A script with zero rules never matches.

### The two callers

- **Storage** — `internal/storage/script.go` re-exports `Script` and
  provides a matcher whose vocabulary is `{system_manufacturer,
  system_product, hostname}`. The first two are DMI facts from
  commission; `hostname` is a defense-in-depth identity constraint
  from the NetBox device name (`storage/script.go:33-37`,
  `storage/script.go:105-123`).
- **Netscript** — `internal/snapshot/netscript.go` provides a
  matcher whose vocabulary is `{site, location, rack, u,
  device_type, hostname}` — a NetBox-identity hierarchy. The
  resolver reads directly off the `netbox.Device` record already in
  hand (`netscript.go:41-48`, `netscript.go:81-126`).

### Adding a new script kind

Two things a new match kind must provide:

1. A `validFields` map (typed as `map[string]struct{}`) enumerating
   the allowed match fields. The engine rejects any header referencing
   a field not in this set.
2. A resolver `func(field string) string` that returns the per-host
   value for each field.

Both callers today additionally:

- Register a CRUD surface on the global API server
  (`/api/v1/storage-scripts`, `/api/v1/network-scripts`,
  `server.go:352-377`).
- Have global perform the match at snapshot-build time and attach the
  chosen script to the `MachineSnapshot` (see
  `internal/snapshot/builder.go` `matchStorageScript` and
  `matchNetworkScript`). The site worker never runs the match.
- Serve the chosen script bytes back to the agent from
  `internal/httpboot` at a machine-scoped path.

### Test expectation

`internal/configscript/configscript_test.go` covers the parser, the
regex-vs-literal split, priority ordering, and the tied-priority
error. Callers add table-driven tests over the field vocabulary and
resolver behaviour (see `internal/storage/script_test.go`,
`internal/storage/collective_match_test.go`, and
`internal/snapshot/netscript_test.go` for the patterns).

## Storage layout contract

Packages: `internal/storage` (the layout types, validation, and
planner) and `cmd/slagworks-agent/main.go` (the executor).

### What the script emits

A storage script is any executable whose stdout is a JSON document
matching `storage.Layout` (`internal/storage/layout.go:5-8`). The
agent invokes the script with `SLAGWORKS_INVENTORY` pointing at a
temp file containing the live inventory JSON collected on the target
just before deploy (see `cmd/slagworks-agent/script_exec.go:9-72`).

`Layout` has two maps:

- `layout` — named `Device` entries, each `type: "disk" | "raid"`.
  Disks carry `partitions[]`; raids carry `level`, `members[]`, and
  an optional filesystem for the array itself.
- `mounts` — a map from mount point (`/`, `/boot/efi`, ...) to a
  `Device` reference by name. Mount points must be clean absolute
  paths with no whitespace, control characters or backslashes
  (`validateMountPoint` in `storage/validate.go`), because the
  planner and the fstab writer use them verbatim.

Partitions declare `name`, `size` (`"512M"`, `"100G"`, `"rest"`),
optional `fs` (`vfat`, `ext4`, `xfs`, `swap`), and two boolean
partition-type flags: `bootable` (EFI System Partition, GPT type
EF00) and `bios_grub` (BIOS boot partition, GPT type EF02, mutually
exclusive with `fs`/`bootable`) (`storage/layout.go:22-33`,
`storage/plan.go:294-307`).

Header directives on the script:

- The configscript `slagworks:match` / `slagworks:priority` header,
  matched on the DMI + hostname vocabulary above.
- `slagworks:kernel_params` is retired and rejected at parse time
  (`storage/script.go:59-73`). Per-hardware kernel params come from
  the NetBox `device_type.custom_fields.slagworks_kernel_params`
  field, threaded through `MachineSnapshot.KernelParams` and served
  by the boot handler's `/api/v1/machines/:id/kernel-params`
  endpoint.

### The executor

`storage.Plan(layout, inventory)` is pure: it validates the layout
against a `[]BlockDevice` inventory (referenced disks must exist),
assigns `/dev/mdN` names to RAID arrays in sorted order, and returns
an ordered `[]Command` (`storage/plan.go:28-95`). Phases: partition,
create RAID, format, mount (deepest last).

The agent runs each command via `runStorageCmd`
(`cmd/slagworks-agent/main.go:552-`), which retries `blockdev
--rereadpt` briefly to work around a udev race
(`storage/plan.go:186-198`).

`storage.BIOSGrubDisks(layout)` returns the sorted `/dev/`-paths of
every disk carrying an EF02 partition; the legacy bootloader path
uses this to install GRUB onto each so a RAID1 root survives
single-disk loss (`storage/plan.go:127-143`).

### Adding a new fs or partition kind

- New filesystem: extend `mkfsCommand` (`storage/plan.go:309-322`).
- New partition-type code: extend `partitionTypeCode`
  (`storage/plan.go:294-307`), and add a bool to `Partition` if the
  layout schema needs to express it.
- New device kind (beyond `disk`/`raid`): extend `classifyDevices`
  (`storage/plan.go:145-158`), add a plan function, and route it in
  `Plan`.

### Test expectation

`internal/storage/plan_test.go` and `validate.go` cover the schema
and the emitted command sequence. `dell_r640_scripts_test.go` and
`storage_layout_test.go` in `cmd/slagworks-agent/` exercise
end-to-end script emission against representative inputs.

## Netscript contract

Packages: `internal/snapshot/netscript.go` (match + selection),
`internal/snapshot/netplan.go` (relay pools from NetBox), and
`internal/agent/netapply` (execution: `RenderNetworkConfig` runs the
script before any disk is touched, `ApplyNetworkConfig` writes the
rendered bundle into the target in the finalizing phase).

### Match

Same as storage, over the identity vocabulary described above. Global
matches per machine at snapshot-build time and attaches the chosen
script to `MachineSnapshot.NetworkScript`
(`model/snapshot.go:234-` and `netscript.go:81-126`).

### What the script emits

A network-config script runs in the deploy initrd with
`SLAGWORKS_NETWORK_FACTS` pointing at a temp file containing the JSON
array of the machine's NetBox interface facts
(`cmd/slagworks-agent/main.go:1760-1774`,
`cmd/slagworks-agent/script_exec.go:14-16`). The script does all
topology interpretation itself — the agent does not parse `/31`
addresses or FRR config.

The script's stdout is a JSON `netBundle`
(`cmd/slagworks-agent/main.go:1708-1722`):

```json
{
  "files": [{"path": "...", "content": "...", "mode": "0644", "owner": "...", "group": "..."}],
  "enable_units": ["...service"]
}
```

Paths must not escape `/target`. `mode` is octal (`"0755"`), default
`0644`. `applyNetBundle` writes each file, chmods it, and enables the
requested systemd units in the target root.

### The shared helper library (netlib)

The network scripts are standalone Python executables the agent
downloads one at a time, so they cannot share code by importing a repo
module at runtime. The shared plumbing they all need — facts I/O
(`load_facts`), the oob-gw VIP resolver (`vip_nameserver`), the
CIDR/private-IP/BMC-name predicates, the emit-bundle dict shape
(`bundle`/`file_entry`/`emit`), and the netplan YAML fragments
(`loopback_lines`, `oob_leg_lines`, `bond_member_lines`, `bond_lines`)
— lives in `internal/agent/netlib/slagworks_netlib.py`, embedded into
the agent binary via `internal/agent/netlib` (`//go:embed`).
`execkit.RunConfigScript` writes it as `slagworks_netlib.py` into the
same temp directory as the script it execs, so the script's own
directory (Python's `sys.path[0]`) resolves `import slagworks_netlib`
with no `PYTHONPATH` — the co-location is the whole mechanism. A new
network script gets the helpers for free by importing the module; the
storage seam passes no sidecars (its scripts have no companion lib). A
script's topology interpretation and its hard-fail error messages stay
in the script (host-family-specific, test-asserted); netlib owns only
the shared mechanics.

Failure semantics (`netapply.RenderNetworkConfig` /
`ApplyNetworkConfig`):

- HTTP 204 (no script matched) or empty machineID: write the built-in
  all-`en*`-DHCP netplan and continue.
- Script matched but exec fails, JSON is invalid, or a file has a
  relative path or a non-octal mode: FAIL the deploy loudly, before
  the boot-disk safety check or any disk is touched (the script is a
  pure function of the facts, so a NetBox model it rejects costs a
  failed deploy, not a wiped host). The alternative — a host that
  should get L3 netplan + FRR silently coming up DHCP-only — is a
  latent fabric break.
- Writing the bundle into `/target` (path containment against the
  installed tree, `chown`, unit enable) can only fail in the
  finalizing phase, after the install.

stderr is the script's diagnostics channel, for both script kinds
(`execkit.RunConfigScript`): it streams to the serial console, each
line of its last 8 KiB is logged as a Warn envelope
(`<kind> script stderr: ...`, visible in `slagctl machine logs`)
whether or not the script succeeds, and on failure that tail is
appended to the error the operation fails with. So a hard-fail message
should say what is wrong in the NetBox model and what to change.

### Where the site worker serves them

- `GET /api/v1/machines/:id/network-script` → 200 with the script
  body, or 204 (`httpboot/handler.go:729-747`).
- `GET /api/v1/machines/:id/network-facts` → 200 JSON array of
  `SnapshotInterface` (`httpboot/handler.go:749-766`).

### Relay-pool generation

`internal/snapshot/netplan.go` generates a `[]RelayPool` from the
NetBox interfaces of DHCP-served hosts: any `/31` on a physical
interface becomes a single-address relay pool. Only host-side `/31`s
show up (leaves have no machine row), and the pool subnet is the
`/31` itself so duplicates collapse by construction
(`netplan.go:55-93`).

### Test expectation

`internal/snapshot/netscript_test.go`,
`netscript_collective_test.go`, and `netplan_test.go` cover the
match, the relay-pool derivation, and the collective (multi-machine)
selection invariants. Agent-side apply is covered by
`cmd/slagworks-agent/network_config_test.go`.

## Power and BMC vendor dispatch

Package: `internal/power`. Wired in `cmd/slagworksd/site.go`
`powerControllerFactory`.

### The interfaces

`power.Controller` is required (`internal/power/power.go:74-96`):

- `PowerOn / PowerOff / PowerCycle`
- `PowerStatus` returns `State` (`on` / `off` / `unknown`)
- `SetBootDevice(ctx, BootDevice)` — one-time next-boot override
  used by the deploy path. `BootDefault` is a no-op.
- `ChassisSerial(ctx)` returns the FRU Product Serial the site
  worker uses to verify the BMC at this IP is the box NetBox thinks
  it is. `("", nil)` means the controller has no chassis-serial
  concept (a PDU); the worker treats that as "skip the check". Any
  other empty return is fail-closed.
- `Transport()` names the wire path (`redfish`, `ipmi`,
  `smcipmitool`, `pdu`, or `bmc` before the first successful call).
- `Close(ctx)`.

`power.BootManager` is optional (`power.go:60-72`). A controller that
can read AND set persistent boot-device overrides implements it;
callers type-assert (`bm, ok := ctrl.(power.BootManager)`) and
surface "not supported on this host" when the assertion fails. The
`OpSetBootDevice` executor path requires it (see below). Only
`SMCController` implements it today.

### The three controllers today

- `BMCController` — `internal/power/bmc.go`. Wraps `bmc-toolbox/bmclib`.
  Provider-agnostic (Redfish, IPMI, vendor HTTP). Its
  `ChassisSerial` reads the FRU Product Serial via `ipmitool fru
  print 0` whenever creds are available (bmclib's ipmitool provider
  has no `InventoryGetter`, and Dell iDRAC's Redfish reports the
  baseboard serial instead of the service tag)
  (`bmc.go:217-309`).
- `SMCController` — `internal/power/smc.go`. Vendor-first path for
  Supermicro hosts, driving the operator-installed `SMCIPMITool` via
  `internal/power/smcipmitool.Runner`. Implements `BootManager`. SOL
  is deliberately NOT served here — console capture stays on
  ipmitool.
- `PDUController` — `internal/power/pdu.go`. Multi-outlet
  orchestration for switched PDUs. Uses APC NMC CLI over SSH for
  actuation (via `pdu_transport.go`'s `sshRedfishConn`, which
  matters because the APC NMC's `cryptlib` SSH server rejects Go's
  default banner — a longer `ClientVersion` is the entire fix) and
  Redfish for status reads. `PowerCycle` uses the firmware-atomic
  `olDlyReboot` so the controller cannot strand a device that
  carries its own control path (`pdu.go:186-226`).

### Dispatch

`cmd/slagworksd/site.go` `powerControllerFactory`
(`site.go:1113-1207`) picks per operation:

1. If the op envelope carries a `pdu_runtime` (operator ran
   `--via pdu`), build a `PDUController` over the outlet specs.
2. Else, if `MachineSnapshot.IsSupermicro()` and SMCIPMITool resolves
   (`SLAGWORKS_SMCIPMITOOL_BIN`, default `SMCIPMITool` on `$PATH`),
   build an `SMCController`. When the tool is missing, `auto` mode
   falls through to step 3 and `required` mode returns an error;
   `off` skips this step.
3. Else, build a `BMCController` with the bmclib options from
   `power.BuildBMCDialOptions` (adds the AMI cipher-3 override and
   qemutest host:port handling).

The NetBox manufacturer slug is stamped into the snapshot
(`MachineSnapshot.Manufacturer`).

The worker reports its SMCIPMITool status (`installed`, `missing`,
`missing-required`, `disabled`) in its hello; global holds it per connection and
`GET /workers` returns it with the site's Supermicro machine count, so
`slagctl worker list` can tell operators where to install it. Status
reads of a Supermicro host served by the fallback carry a `hint`.

### Adding a new vendor

Case A — the new hardware works with bmclib: nothing to add. The
generic path already handles it. Add coverage in
`bmcruntime_test.go` and — if the vendor lands with quirky auth or
cipher requirements — in `power.BuildBMCDialOptions`.

Case B — a vendor-specific controller:

1. Add a subpackage under `internal/power/` (e.g.
   `internal/power/dellracadm/`) holding the protocol client, parsers,
   and command builders, kept free of process execution where possible
   so it unit-tests without hardware.
2. Add a new controller type (`internal/power/racadm.go`) implementing
   `power.Controller` and, if the vendor supports it,
   `power.BootManager`. Both are asserted at the bottom of the file
   with `var _ Controller = (*RacadmController)(nil)` — this is the
   compile-time check.
3. Extend the factory in `cmd/slagworksd/site.go` to select the new
   controller based on `MachineSnapshot.Manufacturer` (whatever
   NetBox slugifies that vendor to).
4. Prefer pure-Go clients over shelling out to vendor binaries. If a
   controller does need an external tool, do not vendor proprietary
   binaries into this repository; document the operator-installed
   dependency instead.

### PDU transport

`pduConn` (`internal/power/pdu.go:32-39`) abstracts one PDU
connection: `cli` for actuation, `outletState` for reads,
`close`. Production factory is `newSSHRedfishConn`
(`pdu_transport.go:41-57`). Tests inject a fake via
`WithPDUConnFactory` (`pdu.go:69-75`). Adding a new PDU vendor means
implementing a new `pduConn` and gating the factory choice — the rest
of the multi-outlet orchestration is transport-agnostic.

### Test expectation

`bmc_test.go`, `smc_test.go`, and `pdu_test.go` each cover their
controller with a fake underneath. The factory itself is exercised by
`internal/api/bmc_runtime_test.go` and
`internal/worker/executor_test.go`. Any new controller must produce a
`Transport()` value distinct from the existing ones so operator-facing
power-status output preserves provenance.

## Commission inventory

Package: `internal/commission` (server-side handler),
`internal/agent/inventory` (agent-side collectors + wire types), and
`internal/netbox/sync.go` (NetBox reconciler). Orchestration/reporting
lives in `cmd/slagworks-agent/commission.go`.

### The collectors

`internal/agent/inventory` runs a fixed list of collectors against the
target hardware. Each is a `stepRecorder.record` entry tagging a
category (`dmidecode_system`, `lscpu`, `dmidecode_memory`,
`lsblk_disks`, `ip_nics`, `lspci`, `lldp_neighbors`, `bmc_ipmitool`,
`dmidecode_psus`, `numa`, `lsusb`) with a `stepVerdict` (`collect.go`).

The `CommissionResult` envelope carries the collected fields
(`types.go`). Adding a new field:

1. Add the Go struct (`type FooInfo struct { ... }`) and a slice or
   pointer field on `CommissionResult` in `types.go`.
2. Write a `collectFoo(r agent.Runner) []FooInfo` (or pointer) that
   shells `dmidecode` / reads `/sys` / etc. The `agent.Runner` seam is
   the injected fake in tests (`fixtureRunner`) and `execkit.Exec` in
   production.
3. Add a `sr.record("foo", ...)` step in `Collect` (and the sibling
   `CollectGraceful`) with a `verdictForSlice` / bespoke verdict
   function.
4. Add table-driven tests in `internal/agent/inventory`.

The collectors gracefully skip when tools are unavailable; the fallback
mirrors sysfs (`collectSystemGraceful`, `system.go`).

### The wire

The agent POSTs an operation-bound `commission_report` envelope to
`/api/v1/report` on the serving worker. The worker verifies the exact
machine/operation binding and discovered serial, UUID, NIC, and disk
identity before routing it through the WebSocket hub. `internal/commission.
Handler.HandleObservation` receives it, revalidates the exact operation,
atomically persists the blob and completes that operation, and calls
`netbox.Client.SyncCommissionData` to reconcile the collected fields
onto the NetBox device (`netbox/sync.go:112-`). The ack back to the
worker is issued only after BOTH writes succeed; a failure anywhere
leaves the worker's outbox entry unacked and the next reconnect
replays it.

### Landing a field in NetBox

`netbox.CommissionSync` (`netbox/sync.go:64-110`) is the flat struct
`SyncCommissionData` operates on. Adding a new field:

1. Add it to `CommissionSync` and populate it from the raw JSON in
   `CommissionToSync` (`sync.go:199-`).
2. If the field maps to a NetBox custom field, add it to the
   `custom_fields` map in `SyncCommissionData`
   (`sync.go:127-142`). Custom fields must be created in NetBox
   (via the netbox-bootstrap migration in
   `internal/api/netbox_admin.go`, or manually) before the sync will
   succeed.
3. If the field derives from an existing collector, extraction is
   local; otherwise add a collector as above.

### Test expectation

`internal/commission/handler_test.go` covers the observation router
including the ack-after-both-writes invariant.
`internal/netbox/sync_test.go` and
`internal/netboxtest/...` cover NetBox reconciliation against a live
NetBox in Docker.

## Operation types

Package: `internal/model` (the enum) and `internal/worker/executor.go`
(the dispatch).

### The enum and its invariant

`model.OperationType` and `model.AllOperationTypes`
(`model/operation.go:10-40`) enumerate the shipped types:
`deploy, commission, power_on, power_off, power_cycle,
set_boot_device`. The invariant, enforced by a guard test in the
`worker` package: `model.AllOperationTypes` MUST equal the union of
`Cache.PendingPowerOps()` and `Cache.PendingLifecycleOps()`. An op
of a type routed by neither getter is orphaned forever.

### Executor dispatch

`Executor.executeOp` (`worker/executor.go:114-148`) claims the op via
the global, then splits on `op.Type`:

- `OpCommission, OpDeploy` → `runLifecycleOp` (the boot-prefix path:
  set BootPXE, power on/cycle, wait for agent).
- Everything else → `runPowerOp`.

`runPowerOp` (`worker/executor.go:151-251`) further switches:

- `OpPowerOn / OpPowerOff / OpPowerCycle` → the corresponding
  `Controller` method.
- `OpSetBootDevice` → `runSetBootDevice`, which requires
  `power.BootManager` and fails "not supported" if the controller
  doesn't implement it (`executor.go:253-`).

### Adding a new op type

1. Add the constant to `model/operation.go` and to
   `AllOperationTypes` in the same edit. The guard test refuses to
   ship an entry that isn't routed.
2. Decide whether it is a lifecycle op or a power/settings op. Route
   it in `PendingPowerOps` or `PendingLifecycleOps`
   (`internal/worker/cache.go`).
3. Add a case to `runPowerOp` (or a new `runXOp` from
   `executeOp`). If the op needs a new capability on the controller,
   add it as an OPTIONAL interface next to `BootManager` and
   type-assert; do not extend `Controller` (that path is why
   `BootManager` is optional).
4. Add REST + slagctl surfaces if operators need to fire it directly.
   Existing patterns: `POST /machines/:id/power`,
   `POST /machines/:id/boot`.
5. Test expectations: guard test in `worker` package, unit tests for
   the executor branch, and a golden test on the params-JSON shape
   the API accepts.

## Boot actions

Package: `internal/httpboot`.

### The decision surface

`Handler.ServeHTTP` (`httpboot/handler.go:139-220`) reads the
requesting MAC, asks the `MachineProvider` for the current
`BootAction`, and emits an iPXE script. Actions:

- `BootLocal` — hand back to firmware. Legacy BIOS gets `exit`;
  UEFI gets `sanboot --no-describe --drive 0x80 || 0x81 || exit 1`.
  The BIOS-vs-UEFI split is derived from the iPXE `${platform}` chain
  param (`bootLocalScript`, `handler.go:227-270`).
- `BootDeploy` — `kernel + initrd` boot with a long cmdline
  carrying `boot=slagworks`, `ip=dhcp`, `BOOTIF=`, `slagworks.server`,
  `slagworks.image`, and machine/op/trace/params IDs
  (`deployBootScript`, `handler.go:355-465`).
- `BootCommission` — same shape, `slagworks.mode=commission`
  appended (`commissionBootScript`, `handler.go:321-353`).
- Kernel-only fallback and Phase-0 chainload markers exist for the
  early-boot / one-shot cases (`kernelBootScript`,
  `chainloadCompleteScript`).

Console params come from `consoleParams` (`handler.go:290-302`),
which reads the per-machine value the provider sourced from
`NetBox.device_type.custom_fields.slagworks_kernel_params`. A literal
`"none"` opts the host out of any `console=`; an unset value falls
back to the historical `tty0 + ttyS1@115200`.

### The image row and mode split

`resolveImageName` is deliberately kind-less: deploy and commission
boot the byte-identical artifact from one Image row (`noble-pxe`)
and the mode is a cmdline, not a separate image (`handler.go:467-475`).

### The server surface

`Server.buildMux` (`handler.go:841-1050`) is the complete route
table for the site worker's PXE / boot HTTP endpoint:

- `/boot/ipxe` — the entry the DHCP OFFER points at.
- `/api/v1/report`, `/api/v1/inventory`, `/api/v1/phone-home` — the
  agent's callbacks.
- `/api/v1/machines/:id/network-script`, `network-facts`,
  `kernel-params`, `hostname`, `storage-script` — per-machine config
  served to the agent in the deploy initrd.
- `/logs/ingest` — structured-log sink (envelopes bounce out to
  global over the WS).
- `/files/images/...` — the image tarball tree, ServeFile'd from
  disk.
- `/files/ipxe/...` — the vendored iPXE binaries, served over HTTP
  as a UEFI HTTP-boot alternative to TFTP.
- `/files/bin/slagworks-agent` — the running site worker's OWN
  `slagworks-agent` binary, served to the booting host at deploy
  time. This is why an agent bugfix requires an apt upgrade on the
  serving worker, not just `slagctl image follow-latest`.

### Adding a new boot action or altering script generation

1. Add the action constant on the `MachineProvider.BootAction`
   surface (`internal/model`), and populate it from the appropriate
   op state in the provider implementation
   (`internal/model/inmem_provider.go` and its production sibling).
2. Add a `*BootScript` method on `Handler` producing the iPXE
   payload. Preserve `boot=slagworks`, `ip=dhcp`, and `BOOTIF=` on
   any kernel-boot path, or the stock Ubuntu `/init` will refuse to
   source the slagworks BOOT hook.
3. Route the new action in `Handler.ServeHTTP`'s decision
   `switch` (`handler.go:204-215`).
4. If the action introduces a new cmdline knob the agent reads,
   parse it in `assets/initramfs/scripts/slagworks` (the boot hook)
   or in the agent's cmdline reader, matching the existing
   `slagworks.foo=` convention.

### Test expectation

`internal/httpboot/handler_test.go` is the golden test for every
emitted script: BIOS vs UEFI local, commission cmdline, deploy
cmdline with every extra flag, refusal to serve deploy without an
`slagworks.expect-uuid`, and 204 semantics on the per-machine
endpoints. Any new action or new cmdline flag lands with a case
here.

## Unsure and surprising

- `MachineProvider` (in `internal/model/provider.go`) is the
  interface every boot / snapshot code path depends on. New provider
  methods land there first; both the in-memory implementation and
  the production `Cache` implementation must satisfy it, so a
  method-add always shows up as a compile break in the site worker
  wiring.
- The vendor-first BMC dispatch keys on `Manufacturer == "supermicro"`
  exactly. A vendor whose NetBox slug doesn't match that literal
  will silently take the bmclib fallback path; if a Supermicro row
  ever gets a different slug, the same fallback happens and the tool
  is skipped. The single-string check is intentional (adding a
  vendor is a code change, not a data change) but easy to miss.
