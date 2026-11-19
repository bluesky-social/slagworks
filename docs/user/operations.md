# Operations reference

Status: live (generated from code, 2026-07-03)

The operator verb set as it is actually implemented in
`cmd/slagctl/cmd/`. Command signatures below are the source of truth;
if a flag is missing from this doc but present in the code, prefer the
code. Run `slagctl <cmd> --help` for the machine-readable version.

All commands accept `--json` for machine-readable output and `--api
<url>` to select the API server (or set `SLAGWORKS_API`; the default is the
server saved by `slagctl auth login`).

## Argument shapes

Every command that names a machine accepts the same shapes: the
slagworks UUID, the NetBox serial, the exact hostname, or a unique
hostname substring. Non-UUID inputs resolve through global. NetBox
names are unique only within a site, so a hostname shared by machines
in two sites is ambiguous: the command refuses and lists their UUIDs,
and you pass the UUID instead. The
`operation watch` command also accepts a machine identifier and
resolves it to that machine's most recent active operation.

`slagctl machine creds <device>` is deliberately broader — it accepts
any NetBox device (server, switch, PDU) by hostname, NetBox ID, or
serial, because operators need it to look up PDU credentials for
BMC-less devices. Do not assume a `machine` subcommand always maps
one-to-one onto registered machines; `creds` is an exception documented
in `cmd/slagctl/cmd/creds.go`.

## Machine lifecycle

### `machine list`

List machines. `--state <state>` filters by machine state; valid values
are `new`, `commissioning`, `commissioned`, `deploying`, `running`,
`failed` (`internal/model/machine.go`). Columns: id, hostname, serial,
state, lock status, desired OS, age.

### `machine show <machine>`

Full machine detail. `--json` returns the underlying `Machine` row plus
the transient `hostname` field the API populates from NetBox on read.

### `machine commission <machine> [--via bmc|pdu] [--pxe-enable] [-w]`

Create a commission operation. Boots the host into the slagworks image
in commission mode; the agent collects hardware inventory and reports
back. `--via` selects the power transport (default `bmc`; `pdu`
actuates the device's switched outlets and is for BMC-less devices).
`--pxe-enable` flips `pxe_eligible=true` in the same call, folding
what would otherwise be a separate `machine pxe-enable` step.

`-w` enters the operation watcher immediately; without it the CLI
prints an op ID and exits.

### `machine deploy <machine> [flags] [-w]`

Create a deploy operation. Flags:

- `--destructive` — skip the disk safety check (required to overwrite
  disks with existing content).
- `--wipe-all-disks` — additionally signature-wipe every disk not in
  the storage layout; implies `--destructive`.
- `--recovery` — boot into an SSH-only recovery environment instead of
  running the deploy agent.
- `--via bmc|pdu` — power transport.
- `--pxe-enable` — flip `pxe_eligible=true` first.

Source: `cmd/slagctl/cmd/machine.go` (`machineDeployCmd`).

### `machine abort <machine>`

Cancel all active operations for a machine and reset its state so a
fresh operation can be issued. Used to unstick a machine wedged in
`commissioning` or `deploying`. Aborting does not power the host
off; if you need that, follow with `machine power off`.

### `machine lock <machine>` / `machine unlock <machine>`

Locking prevents any new operations from being created against the
machine (except `unlock`). Locked machines still respond to reads
and to `pxe-disable`.

### `machine pxe-enable <machine>` / `machine pxe-disable <machine>`

Flip the `pxe_eligible` boolean. This is the DHCP-server-side gate:
the site worker refuses to send a PXE boot file to a MAC belonging to
a machine with `pxe_eligible=false` (`BootEligible` in
`internal/dhcp/handler.go`). Locked machines cannot be PXE-enabled.

### `machine preflight <machine> [--op deploy|commission]`

Read-only. Runs the exact op-creation validation global would run
without creating an operation. See `internal/snapshot/preflight.go`
for the checks: NetBox `primary_ip4` set, bootable NIC with MAC,
matching network- and storage-scripts (or explicit fallback), image
row registered, per-PoP AWS BMC secrets present, kernel-params custom
field set. Exits non-zero when any gap is found.

### `machine match <machine>`

Read-only. Reports which network-config script and storage script the
owning worker would select for this host, by building the same
snapshot the boot path uses. `(none — agent fallback)` in either slot
means the agent will use its built-in default (plain DHCP netplan or
`DefaultLayout` — the latter is a modeling gap for multi-disk hosts).

## Power control

`machine power` has one subcommand per verb, each accepting the same
machine identifier and an optional `--via bmc|pdu`:

- `machine power on <machine>` — power on via BMC (or PDU with `--via
  pdu`).
- `machine power off <machine>` — power off. Interactive by default;
  pass `--yes` to skip the confirm, and note that non-TTY invocations
  refuse to run without it.
- `machine power cycle <machine>` — hard cycle.
- `machine power status <machine>` — read the current BMC-reported
  power state. This is a read, answered synchronously by the site
  worker over its WS channel; no operation row is created.
- `machine power boot-set <machine> --device pxe|disk|default
  [--persistent]` — arm the BMC's next-boot override. Supported only
  on Supermicro hosts whose site worker has SMCIPMITool installed (see
  [Supermicro SMCIPMITool](hardware/supermicro-smcipmitool.md)).
  `--device default` clears
  any armed override; `--persistent` is invalid with `default`.
- `machine power boot-status <machine>` — read the currently-armed
  next-boot override, if any. Exits non-zero when the host's BMC
  controller does not support this operation.

`--via pdu` is a hard AC actuation on shared infrastructure; the CLI
prints an explicit "Transport: PDU outlets" line to stderr before
acting, so the operator is never surprised that "cycle the switch"
meant "yank its power cords." Use it for BMC-less devices (many
switches) or when a BMC is wedged and unreachable.

Sources: `cmd/slagctl/cmd/machine.go` (`machinePowerCmd`,
`machineBootSetCmd`, `machineBootStatusCmd`).

## BMC accounts

### `machine bmc-reconcile <machine>`

Brings a BMC's IPMI user accounts and time settings into compliance, on
any host — including in-service hosts slagworks never deployed (no unlock
needed; the host OS, power and boot order are untouched). The site worker
that owns the BMC's OOB network logs in over IPMI LAN with the device's
`pop/<pop>/bmc-creds-admin` entry, checks the BMC's chassis serial
against NetBox, then:

- leaves the slot-2 admin account (the admin entry's account) alone;
- ensures exactly one `monitor` account at USER privilege and sets the
  password stored for the device in `pop/<pop>/bmc-creds-monitor` (keyed
  by NetBox serial). The stored password is authoritative: a monitor
  account whose password was changed by hand, or that was cleared, is
  brought back to it, and nothing is rotated, so the oob-gw
  `ipmi_exporter` and `sel-archiver` keep scraping. An existing account is
  tested first (`ipmitool user test`, at 16 and 20 bytes) and the password
  written only when the BMC doesn't already hold it: AMI MegaRAC refuses
  to set a password to its current value (completion code 0x19). If the
  BMC holds a different password and also refuses the stored one (for
  example because it is in MegaRAC's reuse history), the op fails saying
  so; set the password on the BMC by hand, or use `--rotate-monitor`.
  Only a device with no stored entry (a new BMC) gets a freshly generated
  password, which is stored there;
- disables and clears every other named account (a leftover
  `slagworks` or `maas` account, for example);
- brings the BMC's time settings into compliance (see "Time compliance"
  below).

It refuses, before writing anything, unless the admin entry names the
BMC's slot-2 account — otherwise it would disable the account it logged
in with. Point the entry at the slot-2 account and retry. Creation is
also refused (422) when the device has no usable admin entry, the pop's
monitor secret doesn't exist, or the device's stored monitor entry can't
be re-asserted (it names an account other than `monitor`, or has no
password). Prints the op id; follow it with
`slagctl operation watch <op-id>`. The op result lists the monitor slot,
`monitor_rotated` (false unless a password was generated),
`monitor_password_written` (false when the BMC already held the stored
password), the disabled
usernames and the time-compliance outcome; the password is never shown.
If a rotating op fails
by timing out (for example the site worker lost its connection to global
mid-run), the BMC may already hold a new `monitor` password that never
reached the secret; re-run `bmc-reconcile` to put the stored password
back (or, for a device with none stored, to generate and store one).

To rotate a monitor password deliberately, pass `--rotate-monitor`: the
op writes a freshly generated password and stores it, whatever is stored.
The oob-gw exporters read `bmc-creds-monitor` only when Ansible renders
their config, so that BMC's scrapes fail (`ipmi_up=0`, BMC-unreachable
alert) until you run
`ansible-playbook oob-gw.yaml --tags sel_archiver,ipmi_exporter`.

The admin entry is the only credential slagworks uses to drive a BMC;
there is no per-host credential in Postgres.

#### Time compliance

After the accounts, the worker talks to the BMC's Redfish API with the
same admin account and enforces:

- **NTP on, with the cage's NTP server as the only server.** The server
  is the `ntp_servers` custom field of the site's `slagworks-oob` prefix
  (the cage's OOB VIP), the same value the worker's OOB DHCP advertises
  as option 42. Stale servers in other slots (a retired `10.1.1.99`, for
  example) are cleared.
- **Timezone UTC.** On a Dell iDRAC that is the `Time.1.Timezone`
  attribute (`UTC`); on every other BMC it is the manager's
  `DateTimeLocalOffset` (`+00:00`). A non-UTC offset shifts every SEL
  timestamp the archiver keeps.
- **Clock within 60 seconds of the site worker's.** A BMC more than 60s
  off has its clock stepped: NTP is turned off, the clock is set with
  `ipmitool sel time set now` (some BMCs ignore a manual set while NTP
  is on), and NTP is turned back on. BMCs don't poll NTP right after a
  config change, so waiting for NTP alone would leave the clock wrong.
  The clock is re-read afterwards; a step that didn't take is reported.

One ASUS model reports NTP disabled a minute after it is enabled while it
keeps syncing from the DHCP-supplied server. On an AMI BMC (ASUS, Tyan)
that already lists the right server, has a correct clock and reports UTC,
that state counts as compliant and is left alone.

The op result's `bmc_reconcile` object records what the time pass did:
`ntp_servers`, `ntp_configured` (a non-compliant NTP setting was
rewritten), `timezone_set`, `clock_stepped`, and `clock_skew_seconds`
(the BMC clock minus the worker's when the op ran, positive = ahead).
`time_error` says what is still out of compliance and why, including a
skipped pass: when the BMC's IP is outside the site's `slagworks-oob`
prefix, or the prefix has no `ntp_servers`, nothing is configured (there
is no default server). Fix the cause (set `ntp_servers` in NetBox, fix
the device's `oob_ip`, check the BMC's Redfish service) and re-run
`bmc-reconcile`. A time failure does not fail the op: its pass/fail is
the account compliance, whose monitor password was already stored. Look
for `time_error` in `slagctl operation show <op-id>` and in the site
worker's `bmc time compliance incomplete` warning.

## Operations

Every mutating action against a machine (deploy, commission, power,
boot-set, bmc-reconcile) produces an `Operation` row. Operations transition through
`pending → claimed → running → completed|failed` at the operation
layer, and through a per-op-type phase spine at the execution layer.
See `internal/model/operation.go` for the status enum.

### `operation list [--all] [--machine <machine>]`

By default only active (non-terminal) operations are shown. `--all`
includes completed and failed rows. `--machine` narrows to one
machine; `--machine` accepts the same shapes as elsewhere.

Rows are ordered by last activity with the freshest at the bottom, so
under `-w` (watch) the table tails like a log.

### `operation show <op-id>`

Full operation JSON, including params, result, phase, and timing.
Credentials in params (BMC, PDU, AMT and monitor passwords, the tailnet
auth key) show as `[redacted]`, and are gone from the stored op within
a minute of it finishing; use `slagctl machine creds` for a device's
OOB credentials.

### `operation watch <op-id-or-machine>`

Watch an operation until it reaches a terminal state. When the
argument is a machine identifier, resolves to that machine's most
recent active operation.

- On a TTY: launches the three-pane deploy TUI (`cmd/slagctl/tui/`) —
  phase spine on the right, structured envelope stream on the left,
  SOL passthrough at the bottom.
- On a non-TTY: falls back to a flat line-based log stream (`--plain`
  forces this on a TTY too). Exits non-zero if the operation fails.

Flags: `--timeout <dur>` (default 30m), `--hold` (keep TUI open on
completion even under CI recording), `--no-hold` (tear down
immediately). Deploy and commission spins are the only op types that
drive the TUI; power ops fall through to the plain path.

### `machine ops <machine> [--power|--deploy]`

List operations for one machine, freshest at the bottom, same columns
as `operation list`. `--power` narrows to power-actuating ops (explicit
power on/off/cycle plus deploys and commissions, whose executors
power-cycle the host into PXE). `--deploy` narrows to deploy and
commission only. The two flags are mutually exclusive.

This subsumes what used to be `machine power history`.

## Logs — the two feeds

The single biggest trap for a new operator is that slagworks has two
independent log surfaces for one machine, and they cover different
things.

### Structured envelopes: `slagctl machine logs <machine>`

Tails the structured `slog` envelope stream over a WebSocket from
`/api/v1/logs/stream`. Envelopes flow from three sources:

- `agent` — the on-machine slagworks-agent during commission and deploy
- `site` — the site worker's slagworksd-site
- `global` — slagworksd global's server-side events

One `trace_id` correlates the full lifecycle of an op across all three
sources. By default this backfills the last window (`--since`, default
`10m`) and exits when caught up; `-f` follows live after backfill.

Flags:

- `-f, --follow` — live-tail after backfill.
- `--since <dur>` — backfill window (`5m`, `1h`, RFC3339 timestamp).
  Widen for historical operations; the default is deliberately small.
- `--trace <uuid>` — filter to one trace ID.
- `--op <uuid>` — filter to one operation ID.
- `--source agent,site,global` — comma-separated source filter.
- `--level debug|info|warn|error` — minimum level.
- `--json` — one envelope per line, JSON.

Critical: only `log.*` calls in the agent reach this feed. A
`fmt.Println` or `fmt.Printf` in the agent goes to the agent's stdout,
which only the SOL console captures — not this feed.

Source: `cmd/slagctl/cmd/logs.go`.

### SOL: `slagctl machine logs <machine> --sol`

Instead of envelopes, stream raw serial bytes off the BMC's Serial
Over LAN feed (`/api/v1/console/stream`). This is the firmware POST,
GRUB, kernel boot, and any stdout/stderr from the agent that did not
go through `log.*`.

Flags (SOL-only; combining these with envelope filters errors):

- `-f, --follow` — follow the live edge.
- `--from-start` — replay from the recorded start of the current cast
  (potentially long history).
- `--tail <dur>` — start at now minus DURATION (`10m`, `90s`; a bare
  number is seconds).
- `--raw` — write raw bytes to stdout without alt-screen/cursor
  handling.

Two facts every operator needs to know:

1. **The SOL buffer rotates.** For an operation that finished a while
   ago, the finalizing-window bytes may be gone; `--from-start` will
   show only what's still buffered. If the history you need has
   rotated away, re-run the op and capture SOL live through the
   window of interest.
2. **BMC session-slot exhaustion.** Many AMI-based BMCs (common
   Supermicro deployments) have around five IPMI session slots. Rapid
   retries after "SOL payload already active" errors make it worse.
   Wait or reset the BMC via its Redfish interface to clear stuck
   sessions.

Envelope-stream flags (`--trace`, `--op`, `--source`, `--level`,
`--json`, `--since`) explicitly error when passed with `--sol`; use
`--tail` for the SOL backfill window instead of `--since`.

#### Console transports

The site worker keeps one capture session per machine and picks the
transport from the snapshot:

| Machine | Transport | Captured by |
| --- | --- | --- |
| Intel AMT endpoint (`kind=amt` creds entry) | AMT SOL redirection | the site's active worker |
| Any other `oob_ip` (BMC) | `ipmitool sol activate` | the site's active worker |
| No `oob_ip`, console port cabled to a console server (SN2700 switches) | local serial tty, read-only | only the worker running ON that console server |

For the serial transport, NetBox is the source of truth: the device's
console port must be cabled to a console-server port whose name is the
tty on the console-server device (`oob-gw-1.pop1` port `ttyS4` means
`/dev/ttyS4` on that host); the baud is the console port's speed
(115200 when unset). An uncabled console port means no capture. The
worker whose verified hardware serial matches the console-server
device's NetBox serial opens the tty raw 8N1, with no flow control, and
never writes to it. That worker captures whether or not it holds the
VIP, and the other worker of the site never touches the line. A missing tty,
or one held by another process, fails the session; the worker logs a
warning (`castpump: repeated fast session failures`) and keeps retrying
with backoff. A switch console that prints nothing for hours is normal,
so serial sessions are exempt from the silence watchdog that reconnects
quiet BMC sessions. BMC and AMT keep precedence: a machine with an
`oob_ip` is never captured over serial.

## Nouns you manage

Slagworks exposes small CRUD trees over things that persist in
Postgres and feed the boot pipeline. Each is a `slagctl <noun>` group
with `list`, plus create/upload/delete as appropriate.

### `slagctl site-config` — per-site configuration

Sites are NetBox-owned; slagworks keeps a sparse per-site knob table.
Each site-config is the worker-identity primary key for one site
worker instance (see `cmd/slagctl/cmd/site.go`).

- `list` — every site-config across the deployment.
- `create --slug <s> [--netbox-site <s>] [--netbox-location <s>]
  --pool-start <ip> --pool-end <ip> [--promiscuous]` — create.
  `--netbox-site` defaults to `--slug` for the whole-site pattern.
  `--netbox-location` narrows to one location within a site
  (dual-cage). The ephemeral pool is the DHCP range used for PXE-
  eligible hosts. `--promiscuous` disables the MAC allowlist.
- `show <slug>` — full JSON of one config.
- `update <slug> [flags]` — partial update. `--relay-pool
  <subnet,start,end,gateway>` (repeatable) sets relayed DHCP pools —
  the flag replaces the full list; combine with `--clear-relay-pools`
  or paired `--*` / `--no-*` flags to unset booleans.
- `delete <slug>` — delete a site-config.

### `slagctl image` — PXE image row

The image row pins the immutable per-SHA URL that site workers pull
their boot artifacts from. There is exactly one image row for the
current setup; deploy and commission boot the same artifact in
different modes selected by the `slagworks.mode=` kernel command line.

- `list` — image rows.
- `create --name <n> --version <v> --sha256 <s> --url <u>
  [--arch amd64]` — register a new row explicitly.
- `follow-latest [--name noble-pxe] [--bucket-url <base>]` — resolve
  the bucket's `latest-sha.txt`, verify the sidecar `.sha256`, and
  upsert the image row to point at the immutable per-SHA URL. This is
  the normal "roll a freshly-built image to the fleet" workflow. Site
  workers auto-sync on the next reconcile tick.
- `sync --dir <path>` — download images to a local directory using the
  same code path production site workers use. Useful for hosts that
  can't reach the API directly; pass `--images-json` to feed metadata
  directly rather than fetching it from the API.

Source: `cmd/slagctl/cmd/image.go`.

### `slagctl storage-script` — disk layout scripts

Scripts that produce a partition/RAID plan against the host's
commission data. The matching engine picks the highest-priority script
whose match predicates hold; see `docs/user/storage-scripts.md` for the
authoring model.

- `list` — every script, with priority.
- `upload --name <n> <file>` — upload a new script.
- `delete <id>` — delete by ID.

### `slagctl network-script` — permanent network configs

Scripts that produce the deployed OS's netplan (or bond/FRR config)
against the host's NetBox identity. Absence of a matching script
means the agent falls back to plain-DHCP netplan.

- `list`, `upload --name <n> <file>`, `delete <id>`.

### `slagctl ssh-key` — authorized keys for the deployed OS

Global authorized_keys, applied to every deployed host.

- `list`, `add "<pubkey>" [--comment <label>]`, `remove <id>`.

### `slagctl worker register`

Mint or rotate a bearer token for a `(site_slug, worker_id)` pair.
The plaintext token is returned exactly once; hand it to the site
worker's `SLAGWORKS_TOKEN` env. Re-registering the same pair rotates.
`slagctl worker list` shows connected workers with first- and
last-seen timestamps.

## Credentials

### `slagctl machine creds <device>`

Print the OOB credentials for any NetBox device (server BMC, switch
PDU, PDU NMC) by proxying the per-PoP admin AWS secret through
global. The server's `/devices/<ident>/creds` endpoint resolves any
NetBox device by hostname, ID, or serial — not just registered
machines. Name a PDU device directly to fetch its NMC creds.

The privileged secret read happens server-side under the admin OIDC
gate; the operator needs no direct AWS access. Output is plaintext
credentials — handle accordingly. The command emits a
kind-appropriate ready-to-paste command hint (`ipmitool -I lanplus …`
for BMCs, `ssh` for PDUs) with the password kept out of the process
list.

Sources: `cmd/slagctl/cmd/creds.go`,
`internal/awssecrets/awssecrets.go`.

## Read-only introspection endpoints

`slagctl netbox bootstrap` idempotently ensures the NetBox side has
the custom fields, webhook, and event rules slagworks needs. Run once
per NetBox instance.

`slagctl netbox migrate-chassis-serial` is a one-off migration between
the legacy `chassis_serial` custom field and NetBox's built-in
`serial` field; dry-run by default (`--apply` to write).

`slagctl netbox diff <machine>` is a read-only on-call check of the
machine's persisted bindings. It compares the Slagworks machine UUID,
NetBox device ID, and serial with the live NetBox device and exits
non-zero if a binding differs or either side is missing. `--json`
prints the same report before returning the non-zero status, so it is
safe to use as a scripted check.

Hostname, site, BMC IP, and interface MACs are displayed as NetBox-only
context: NetBox is their source of truth and Slagworks does not keep an
independent copy to compare. Likewise, Slagworks lifecycle state,
commission time, and snapshot sequence are labeled as local-only
context. This provenance is intentional—a snapshot freshly derived
from NetBox is not an independent source and must not be presented as
evidence that those fields match.

`slagctl version` prints the client and server versions; useful for
confirming which build of global is answering.
