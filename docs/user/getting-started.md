# Getting started

Status: live (generated from code, 2026-07-03)

This walks a new operator through the happy path: from a
freshly-installed control plane to a deployed host. It assumes you have
already stood up the slagworks global service and at least one site
worker per the installation and configuration docs, and that your
NetBox instance is populated with devices representing the hardware
you intend to manage. If any of that is missing, start there:

- Installation: `docs/user/installation.md`
- Configuration: `docs/user/configuration.md`
- Adding a site worker: `docs/user/adding-a-site-worker.md`

## The moving parts, briefly

Slagworks has two long-running processes and a CLI:

- `slagworksd global` — the API server. Owns Postgres, mirrors NetBox
  identity, holds the operation queue, and hands site workers
  ready-to-consume snapshots. Operators talk to it via `slagctl`.
- `slagworksd site` — the site worker, one process per gateway host.
  Each worker syncs images and connects out to global over a WebSocket.
  A site's gateways typically run two in HA: only the worker holding
  the site VIP serves DHCP/TFTP/HTTP boot, actuates BMCs and PDUs, and
  drives ops; the other stays connected as a warm standby.
- `slagctl` — the operator CLI. Talks only to global. Every non-read
  action is expressed as an operation the site worker executes.

The one non-obvious thing to internalize now: NetBox is the source of
truth for identity. Slagworks does not itself have a "add a machine"
command. Machines appear because their NetBox devices fall inside a
site-config's claim, and vanish (as `Machine` rows) when the NetBox
device is deleted.

## Authenticate `slagctl`

Operator auth to global is OIDC via SSO. First-time setup:

```
slagctl auth login --api https://slagworks.example.com
```

This runs the interactive loopback flow (browser opens, code returns to
localhost) and caches an access token that is refreshed silently when it
expires, including part-way through a long `operation watch`, or when the
server refuses it early (a 401). Use `--device` for the device-code flow
on hosts without a local browser. `slagctl auth whoami` shows the cached
identity; `slagctl auth logout` clears it.

The login asks the server which identity provider to use and saves the
server as the default, so later commands need no `--api`. `--api` or
`SLAGWORKS_API` selects a different server for one command.

Source: `cmd/slagctl/cmd/auth.go`, `cmd/slagctl/cmd/root.go`.

## NetBox as source of truth: enrollment

A `Machine` row in slagworks is created for a NetBox device when three
things line up:

1. The NetBox device has a non-empty `serial` (`serial` is the stable
   key slagworks re-binds against, so it is required).
2. Some `site_config` in slagworks claims the device — either because
   its NetBox site slug matches the config's `netbox_site`, or, for a
   location-scoped config, both `netbox_site` and `netbox_location`
   match.
3. The device is reachable through slagworks' scope predicates (see
   `internal/snapshot/scope.go`).

When those hold, the mirror creates the row on either a NetBox webhook
event or the periodic reconcile scan, and the machine starts in state
`new` with `pxe_eligible=false`. See `internal/mirror/mirror.go` and
the webhook handler in `internal/netbox/webhook.go`.

The practical consequence: to enroll a batch of hosts, put them in
NetBox with correct serials, ensure they sit under a site (or location)
that a slagworks site-config claims, and let the reconciler pick them
up. There is no explicit enrollment CLI.

### Creating the first site-config

If no `site_config` yet claims the devices you want to manage, create
one. The site-config is the per-site (or per-location) identity object —
not a per-worker object: every worker serving that scope (including an
HA pair) registers against the same site-config slug:

```
slagctl site-config create \
    --slug <site-slug> \
    --netbox-site <netbox-site-slug> \
    --pool-start <first-ephemeral-ip> \
    --pool-end   <last-ephemeral-ip>
```

`--slug` is the site-config primary key (the slug workers register
against). If you omit `--netbox-site` it defaults to `--slug`, which is
the common case where one site-config claims one whole NetBox site. Add
`--netbox-location <slug>` when you want to narrow scope to one location
within the site (the dual-cage pattern; two workers, one NetBox site,
distinct locations).

The ephemeral pool is the range from which the DHCP/TFTP boot server
hands leases to hosts booting via `pxe_eligible=true`. It must sit
inside the OOB subnet the site worker is bound to but should not
overlap production reservations: a reserved host takes its address back
from any dynamic client holding it, and that client is NAKed at its next
renew and moves to another address. Relay pools (`--relay-pool`) are added
via `site-config update` for hosts that PXE across an L3 relay.

Source: `cmd/slagctl/cmd/site.go`, `internal/model/site_config.go`.

### Verifying enrollment

`slagctl machine list` shows every enrolled machine, keyed by NetBox
serial and hostname, with state, desired OS, lock status, and age.
`--state new` filters to hosts that have never been commissioned.

If a NetBox device you expect to see is missing, the most common
causes are:

- Empty `serial` on the NetBox device (the mirror skips it).
- The device is not in a site or location claimed by any site-config.
- The reconciler has not yet run (webhooks are best-effort; the
  reconciler is authoritative).

`slagctl machine show <arg>` accepts a UUID, NetBox serial, exact
hostname, or a unique hostname substring. Same shape everywhere in the
CLI.

## First commission

Commissioning boots the host into the slagworks image in `commission`
mode, which collects hardware inventory (system product/manufacturer,
disks, NICs) and reports it back. That inventory is what storage- and
network-script matchers key on, so most hosts must be commissioned
before they can be deployed.

Before firing anything, ask slagworks whether the host is fully
modeled:

```
slagctl machine preflight <machine> --op commission
```

Preflight is a read; it creates no operation. It runs the exact
op-creation validation global would run (see
`internal/snapshot/preflight.go`), catching missing `primary_ip4`,
missing bootable NIC MAC, unmatched network script, unregistered
image, and missing per-PoP AWS BMC secrets. Each gap names both what
is missing and what to do.

Then commission:

```
slagctl machine pxe-enable <machine>
slagctl machine commission <machine> -w
```

`pxe-enable` flips `pxe_eligible=true`; without it the DHCP handler
refuses to serve a boot file to this host's MAC (`BootEligible` gate,
`internal/dhcp/handler.go`). `--pxe-enable` on `commission` folds the
two into one command. `-w` (watch) enters the TUI-based operation
watcher immediately.

The site worker claims the operation, walks the BMC into a PXE boot,
and streams progress through the phase spine
`power_cycle → pxe_booting → dhcp_ack → ipxe_dhcp → ipxe_loaded →
agent_starting → collecting_inventory → reporting → console_probe →
completed`. See `cmd/slagctl/tui/lifecycle.go` for the canonical
commission spine.

### Reading commission results

After the operation completes, `slagctl machine show <machine>` prints
the machine detail including `commissioned_at` and — via `--json` —
the raw `commission_data` blob. That blob is the source of truth for
storage-script matching: the layout engine looks at
`system_product` / `system_manufacturer` and picks the highest-priority
storage script whose match predicates hold.

`slagctl machine match <machine>` (also a read) reports the network-
and storage-scripts the owning worker would select for this host,
proving the match without firing a deploy. `(none — agent fallback)`
in either slot means the agent will fall back to its built-in
plain-DHCP netplan or default storage layout, which is fine for
single-disk hosts but usually a modeling gap on real fleet hardware.

Sources: `cmd/slagctl/cmd/machine.go` (`machineMatchCmd`),
`internal/snapshot/builder.go`.

## First deploy

A deploy PXE-boots the same image in `deploy` mode. The agent
partitions per the matched storage script, rsyncs a squashfs OS to
disk, installs a bootloader whose kernel command line comes from the
NetBox device-type custom field `slagworks_kernel_params`, arranges
tailscale enrollment, and reboots into the on-disk OS. "Deployed"
means: the host is reachable off the boot image and is running its
first-boot on the disk.

The critical flags:

- `--destructive` — skip the disk safety check. Required whenever the
  target has existing data or partitions. Slagworks refuses to
  overwrite disks with content unless this is set. It is also required
  when the layout builds or preserves a RAID array and an old array on
  a disk outside the layout is assembled at the same `/dev/mdN`: the
  deploy stops arrays outside the layout only with this flag, and
  otherwise refuses before changing anything. The old array's disks are
  not written.
- `--wipe-all-disks` — additionally signature-wipe every disk NOT in
  the storage layout (clears stale mdadm/DDF containers and old
  filesystem signatures that would otherwise confuse later reboots).
  Implies `--destructive`.
- `--recovery` — boot into an SSH-only recovery environment instead of
  running the deploy agent. Useful for triage.
- `--via pdu` — actuate the power cycle via the device's switched PDU
  outlets instead of the BMC. For BMC-less hosts (many switches) or a
  wedged BMC. See `cmd/slagctl/cmd/machine.go` for the exact per-verb
  behavior.

Typical first deploy:

```
slagctl machine preflight <machine> --op deploy
slagctl machine deploy <machine> --destructive -w
```

`-w` again enters the TUI. Without it, the CLI prints an operation
ID and returns; follow it later with:

```
slagctl operation watch <op-id-or-machine>
```

The watch argument accepts the same shapes as `machine`: a
machine identifier resolves to that machine's most recent active
operation. On a TTY you get the three-pane TUI (phase spine +
structured envelopes + SOL passthrough); on a non-TTY you get a
flat log stream and a non-zero exit if the operation failed.

The deploy phase spine is
`power_cycle → pxe_booting → dhcp_ack → ipxe_dhcp → ipxe_loaded →
agent_starting → partitioning → rsyncing_os → installing_bootloader →
finalizing → bmc_reconcile → awaiting_phone_home → completed`
(`cmd/slagctl/tui/lifecycle.go`).

### What "deployed" means

`awaiting_phone_home` sits at the end of the spine for a reason:
slagworks' terminal success condition is that the just-deployed host
booted off local disk and phoned home over its production network.
The site worker installs a boot-local iPXE idiom that the firmware
follows after the deploy reboot, so the disk boot happens without
another PXE round trip. If the host's production networking hasn't
converged yet (dependent switch config elsewhere in your fleet),
the operation stays in `awaiting_phone_home` — this is correct;
the deploy itself succeeded, and the phone-home will land once the
network is up.

## Where to go next

- `docs/user/operations.md` — every operator verb, with flag detail.
- `docs/user/troubleshooting.md` — common failure modes and how to
  diagnose them, phase by phase.
- `docs/user/storage-scripts.md` — how storage-script matching and
  templating work, for authoring layouts against your hardware.
- `docs/user/hardware/` — vendor-specific BMC and boot notes
  (Supermicro, Dell iDRAC, common iPXE local-boot idioms).
- `docs/archeology/commissioning-flow-2026-03.md` — deeper walk through the commission
  path if you need to debug it.
