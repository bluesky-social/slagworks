# Glossary

Status: live
Canonical definitions for terms used across slagworks code, docs, and CLI.
If a term isn't here, it shouldn't be in the codebase without adding it.

---

## Nouns

### Machine

Slagworks' primary entity. Represents a single provisionable host — bare metal
server, network switch, or cloud instance. Stored in Postgres (central) and
SQLite (worker cache). Uniquely identified by UUID; MAC is globally unique,
hostname is unique within a site. Initial state is `new` (enrolled but not
yet commissioned).

**Not "server", "host", "node", or "device"** in slagworks context. "Device"
is reserved for the corresponding NetBox object.

### Device

A NetBox object. One slagworks Machine corresponds to one NetBox Device.
Slagworks reads Device metadata (site, role, rack) and writes back state
and hardware inventory. Never used as a synonym for Machine in slagworks code.

### Site

A physical point of presence (POP). Scoping boundary for hostnames, workers,
DHCP, and sync. Every Machine belongs to exactly one Site.

"POP" is acceptable in conversation and ops context but code and docs use
"Site" as the canonical term.

### Worker

A `slagworksd site` instance running on an OOB gateway at a site. Manages
DHCP, TFTP, HTTP boot, power control, and deploy/commission execution for
all Machines at its Site. Syncs state with central API. Multiple workers
per site in active/standby via Keepalived VIP.

Not a first-class database entity — workers are identified by a `worker_id`
string in operation claims and reports. Stateless and replaceable.

### Agent

The static binary (`slagworks-agent`) that runs *on the target machine itself*,
not on the worker. Collects hardware inventory (commission mode) or reports
deploy completion (deploy mode). Communicates results back via HTTP POST.

**Not the worker.** The agent runs inside the machine being provisioned; the
worker runs on the infrastructure managing that machine.

### Operation

A task queue entry representing work to be done on a Machine. Current types:
`deploy`, `commission`, `power_on`, `power_off`, `power_cycle`. Planned:
`adopt` (not yet in code). Has a lifecycle:
`pending` -> `claimed` -> `running` -> `completed` | `failed`.

### CommissionResult

The typed Go struct output by the agent after hardware collection. Contains
System, CPUs, Memory, Disks, NICs, PCIDevices, LLDP, BMC, and Cloud metadata.
Defined in `cmd/slagworks-agent/commission.go` (package main).
`internal/netbox/sync.go` has a parallel mirror struct for deserialization.

### CommissionData

The stored form of a CommissionResult. Lives as `json.RawMessage` on
`Machine.commission_data`. Same payload, different name to distinguish
"agent output" from "stored state."

### Observation

A hardware/lifecycle event reported by the worker to central. Types:
`commission_report` (with CommissionData payload), `deploy_complete`,
`phone_home`. Queued in the worker's local outbox, batched in ReportRequests.

### Overlay

The CPIO archive (`assets/overlay/overlay.cpio`) injected into the initrd
during PXE boot. Replaces `/init` with custom logic that mounts overlayfs,
sets up networking, downloads the agent, and hands off to the appropriate
boot mode.

### BMC

Baseboard Management Controller. The out-of-band hardware management interface
(Redfish/IPMI). Slagworks stores BMC credentials on the Machine and uses them
for power control. BMC IPs are allocated from NetBox IPAM at enrollment time.

### VIP

Virtual IP managed by Keepalived (VRRP) for worker high availability. The
active worker owns the VIP; the standby monitors for it. Stored on
`Site.vip`. See `internal/worker/ha.go`.

### Module (NetBox)

NetBox 4.3+ inventory object. Slagworks writes Modules to NetBox during
commission reconciliation (CPUs, memory, disks, GPUs, NICs, BIOS, BMC).
Not a slagworks internal model — exists only in NetBox.

### Interface (NetBox)

NetBox network interface object. Slagworks creates/updates Interfaces from
agent-discovered NIC data during commission reconciliation.

### Cable (NetBox)

NetBox cable termination. Slagworks creates Cables from LLDP neighbor data
discovered during commissioning — connecting machine NICs to switch ports.

---

## Binaries and CLI Tools

### slagworksd

The main daemon. A single binary with subcommands:

- `slagworksd global` — central API mode. Manages the global machine
  inventory, orchestrates operations, talks to NetBox, serves the sync/report
  endpoints that site workers poll. Backed by Postgres.
- `slagworksd site` — site worker mode. Runs on OOB gateways.
  Manages DHCP, TFTP, HTTP boot, power control, and executes deploy/commission
  operations. Syncs state with central API. Backed by local SQLite cache.
  Multiple instances per site in active/standby (see HA below).

**HA**: Multiple `slagworksd site` instances run per site on separate OOB
gateways. Keepalived manages a VIP via VRRP. The active instance owns the VIP
and runs all services; standby instances sync state but don't serve DHCP/TFTP.
Failover is automatic — the worker monitors whether the VIP is on a local
interface. No distributed consensus, no API dependency for failover.

### slagworks-agent

Static binary that runs on the target machine (not the worker). Two modes:
commission (collect hardware inventory) and deploy (install OS, report
completion). Downloaded by the overlay init during PXE boot.

### slagctl

Operator CLI tool. Runs on dev laptops. Always a thin API client — every
action goes through the global API, never directly to machines or
site workers. Subcommands include:

- `slagctl machine {list,show,create,deploy,commission,power,lock,unlock,abort,...}` — machine lifecycle
- `slagctl site {list,show,create,delete}` — site management
- `slagctl operation {list,show,watch}` — operation tracking
- `slagctl image {list,create}` — image registry
- `slagctl storage-script {list,upload,delete}` — storage script management
- `slagctl netbox {bootstrap,import}` — NetBox integration
- `slagctl worker list` — connected worker status
- `slagctl admin token generate` — admin token management

---

## Verbs and Lifecycle States

### Enroll

Register a new Machine in slagworks. Triggered by a NetBox webhook when an
operator creates a Device (or by the periodic reconciler as a safety net).
Enrollment allocates a BMC IP from NetBox IPAM (if BMC MAC is known) and
creates the Machine in `new` state.

Not "register", "onboard", or "import".

### Commission

Collect hardware inventory from a Machine. The agent boots in a minimal
environment, discovers hardware (DMI, NICs, disks, CPUs, memory, PCI, LLDP,
BMC), and reports a CommissionResult. Central stores the data and reconciles
it to NetBox.

Machine states: `new` -> `commissioning` -> `ready`

Operation type: `commission`

### Deploy

Install an OS onto a Machine's disk. The agent boots into a PXE environment,
partitions/formats the disk (via storage scripts or default layout), rsyncs
the live root filesystem to disk, installs the bootloader, and seeds
cloud-init. The agent reports completion before rebooting.

Machine states: `ready` -> `deploying` -> `deployed` -> `verified`

Operation type: `deploy`

**"Deploy" is overloaded** — it's a verb ("deploy this machine"), an operation
type (`OpDeploy`), a boot action (`BootDeploy`), and part of state names
(`deploying`, `deployed`). Context disambiguates; this is intentional, not
sloppy.

### Verify

Confirm that a deployed Machine's installed OS is running. Triggered by a
phone-home callback from a first-boot systemd service on the target machine.

Machine state: `deployed` -> `verified`

Event: `phone_home`

### Adopt

Import an existing running machine into slagworks without reimaging it.
Planned: `slagctl adopt` would SSH into the machine, run the agent in
collect-only mode, and POST the CommissionResult to central. The machine
would go straight to `verified` state. Not yet implemented.

Not "import" — that implies bulk data loading. Adopt is per-machine and
involves active collection.

### Provision

Umbrella term for the full lifecycle: enroll -> commission -> deploy -> verify.
**Not** a specific operation type or API action. Use in high-level docs and
conversation only, not in code.

### Decommission

Retire a machine. Operator sets Device.status to "decommissioning" in NetBox.
Slagworks respects this status and will not attempt operations on the machine.
Not currently an automated slagworks action.

### Sync

Bidirectional state synchronization between central API and worker, over
the persistent WebSocket.

- **Push down**: global pushes a full `SiteSnapshot` to the worker whenever
  site state changes (debounced). Snapshots carry a per-site `SeqNum`;
  within a connection workers apply strictly by SeqNum to prevent stale
  overwrites. The first snapshot on each connection is global's current
  view and replaces whatever the worker held, even at a lower SeqNum.
- **Push up**: the worker sends observations, operation updates, and log
  envelopes upstream on the same socket, draining its durable local outbox.

### Report

Worker pushes observations and operation status updates to central. A Report
contains a batch of Observations and OperationUpdates. The reporter drains
the local outbox on each cycle.

### Claim

Worker atomically locks a pending Operation to prevent duplicate execution.
Exactly one worker wins the claim; others get a nil response. Prevents race
conditions in multi-worker (HA) setups.

Operation status: `pending` -> `claimed`

### Expire

Central API garbage-collects Operations that have been claimed but not
completed within the timeout (default 30 minutes). Marks them as `failed`.
Prevents stuck operations from blocking the machine indefinitely.

### Phone Home

Post-boot callback from the installed OS to the worker. A first-boot systemd
oneshot service POSTs to `/api/v1/phone-home?mac=X`. Triggers the
`deployed` -> `verified` state transition.

---

## Machine State Diagram

```
                enroll
                  |
                  v
     .-----.  commission  .---------------.  complete  .-------.   deploy   .----------.
     | new | ──────────> | commissioning  | ────────> | ready | ────────> | deploying |
     '-----'             '---------------'            '-------'           '-----------'
        |                      |                         ^                      |
        |                  (failure)                     |               deploy_complete
        |                      |                    (re-commission)             |
        |                      v                         |                     v
        |                 .--------.                     |               .----------.
        |                 | failed | ────────────────────'               | deployed |
        |                 '--------'                                    '----------'
        |                      ^                                             |
        |                      |── (deploy failure) ─────────────────────────'
        |                                                               phone_home
        |                                                                    |
        |                                                                    v
        |                                                              .----------.
        '────────── adopt (planned, not yet implemented) ────────────> | verified |
                                                                       '----------'
```

Additional states not in the main lifecycle: `unknown` (pre-enrollment), `unmanaged` (excluded from PXE). Both boot locally.

---

## Anti-Glossary: Terms to Avoid

| Don't say | Say instead | Why |
|-----------|------------|-----|
| server, host, node | Machine | Consistent naming; "host" is ambiguous with worker host |
| device (in slagworks code) | Machine | "Device" is reserved for the NetBox object |
| box | Machine | Informal; not greppable |
| POP (in code) | Site | "POP" is fine in conversation, "Site" in code |
| task, job | Operation | Operations have a specific lifecycle and type system |
| inventory (for hardware data) | CommissionResult / CommissionData | "Inventory" collides with NetBox InventoryItem (deprecated) |
| image (for deploy action) | deploy | "Image" is a noun (OS image); deploying installs an image |
| provision (as a specific action) | commission or deploy | Provision is the umbrella; be specific about which step |
| slagworks-worker | slagworksd site | Old binary name; now a subcommand of slagworksd |
| slagworks-api | slagworksd global | Old binary name; now a subcommand of slagworksd |
