# slagctl command reference

Status: generated — do not edit. Regenerate with `just gen-cli-docs`.

Operator CLI for the slagworks bare-metal provisioning system

## Global flags

```text
      --api slagctl auth login   API server URL (default: $SLAGWORKS_API, then the server saved by slagctl auth login)
      --json                     output as JSON
```

### slagctl auth

manage SSO authentication (OIDC)

```text
slagctl auth
```

#### slagctl auth login

sign in to a slagworks server via SSO

Runs the interactive OIDC login and caches the resulting token.

The server named by --api (or SLAGWORKS_API, or the one saved by the last
login) reports which identity provider and client to use, and a successful
login saves that server as the default for later commands.

By default this uses the loopback flow: a browser opens, you authenticate with
the identity provider, and the code is captured on a local port. Use --device for the
device-authorization flow (prints a URL + code to enter on another device) when
no local browser is available.

```text
slagctl auth login [flags]
```

Flags:

```text
      --device   use the device-authorization flow instead of loopback
```

#### slagctl auth logout

delete the cached SSO token

```text
slagctl auth logout
```

#### slagctl auth whoami

show the identity in the cached SSO token

```text
slagctl auth whoami
```

### slagctl dhcp

inspect site-worker DHCP state

```text
slagctl dhcp
```

#### slagctl dhcp history

show DHCP lease history for a site, from the global ledger

Reads the global lease-history ledger (Phase 3): every lease
allocate/renew/delete a site worker has ever reported, folded into
open (currently held) and closed (superseded or deleted) rows. Unlike
"dhcp leases" this never touches a live worker — it answers from
Postgres even when every site worker is offline.

```text
slagctl dhcp history <slug> [flags]
```

Flags:

```text
      --ip string    filter by IP address
      --limit int    max rows to return (server default 100, cap 1000)
      --mac string   filter by MAC address
  -w, --watch        refresh every 3s
```

#### slagctl dhcp leases

show DHCP leases currently held by a site's worker(s)

Fans out to every connected site worker for <slug> and shows the
leases each currently holds. An HA cage's two workers each own their
own dynamic slice, so both matter — a worker that errors or times out
is reported as a warning, not a fatal error, so the other worker's
leases still print.

```text
slagctl dhcp leases <slug> [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

### slagctl doctor

Check binary version, credentials, and connectivity

```text
slagctl doctor [flags]
```

Flags:

```text
      --json   Emit results as JSON array instead of table
```

### slagctl image

manage images

```text
slagctl image
```

#### slagctl image create

register an image

```text
slagctl image create [flags]
```

Flags:

```text
      --arch string      architecture (default "amd64")
      --name string      image name
      --sha256 string    SHA256 checksum
      --url string       download URL
      --version string   version string
```

#### slagctl image delete

delete an image row

Deletes an image row by name or uuid. Site workers sync whatever image
rows exist; deleting a row pushes a snapshot to every site, and workers
stop tracking and booting that image when they apply it. Any
already-downloaded file stays on disk under the worker's local image
cache (nothing prunes it).

```text
slagctl image delete <name-or-id> [flags]
```

Flags:

```text
  -y, --yes   skip confirmation prompt
```

#### slagctl image follow-latest

register/update the PXE image row pointing at the current latest <sha>.tar.gz

Resolves <bucket>/<name>/latest-sha.txt to the current
git SHA, fetches the matching <sha>.tar.gz.sha256 sidecar to learn
the content sha256, and creates-or-updates the single Image row to
pin URL=<...>/<sha>.tar.gz with SHA256=<content-sha>.

There is ONE image: deploy and commission boot the same artifact in
different modes (the slagworks.mode= kernel cmdline), so there is no
--kind. The row always points at the immutable per-SHA URL; the worker
never fetches latest-sha.txt directly. To pick up a new build, re-run
this command (or run it on a cron).

```text
slagctl image follow-latest [flags]
```

Flags:

```text
      --arch string         architecture for the row (default "amd64")
      --bucket-url string   base URL of the image bucket (default: the bucket of the image row already registered under --name)
      --name string         image row name / S3 prefix (default "noble-pxe")
      --version string      version string for the row (default "24.04")
```

#### slagctl image list

list images

```text
slagctl image list [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

#### slagctl image sync

download images to a local directory (same code path as production site workers)

Downloads any missing or outdated images to the local directory.
This is the same code production site workers use (SyncImages).

Image metadata comes from the global API (default) or from --images-json
(for hosts that can't reach the API directly).

Initrds are stock Ubuntu initrds with the slagworks BOOT hook baked in at
build time (boot=slagworks sources it); there is no download-time repack.

```text
slagctl image sync [flags]
```

Flags:

```text
      --dir string           local directory for images (required)
      --images-json string   image metadata as JSON (alternative to API fetch)
```

### slagctl machine

manage machines

```text
slagctl machine
```

#### slagctl machine abort

cancel active operations and reset machine state

```text
slagctl machine abort <machine>
```

#### slagctl machine bmc-reconcile

bring a machine's BMC user accounts and time settings into compliance over the LAN

Create a bmc_reconcile operation. The site worker that owns the BMC's
OOB network logs in over IPMI LAN with the device's pop/<pop>/bmc-creds-admin
entry (keyed by NetBox serial), checks the BMC's chassis serial against
NetBox, then:

  - leaves the slot-2 admin account untouched,
  - ensures exactly one "monitor" account at USER privilege holding the
    password stored for the device in pop/<pop>/bmc-creds-monitor (keyed
    by NetBox serial): the account is tested first and the password is
    written only when the BMC does not already hold it, so a BMC whose
    monitor drifted converges back to the store without rotating
    anything. Only when nothing is stored for the device (a new BMC), or
    with --rotate-monitor, is the password freshly generated and stored
    there; the op result's monitor_rotated and monitor_password_written
    say which happened,
  - disables and clears every other named account,
  - over Redfish with the same account: enables NTP with the ntp_servers
    of the BMC's slagworks-oob prefix as the only servers, sets the BMC
    timezone to UTC, and steps the BMC clock to the site worker's when it
    is more than 60s off.

Boot order and power are never touched, and the host OS is unaffected, so
this is safe on in-service hosts (no unlock needed). The op is refused
unless the admin entry names the BMC's slot-2 account. A time failure
(or a prefix without ntp_servers) is reported in the op result's
time_error and does not fail the op. Follow it with
"slagctl operation watch <op-id>".

```text
slagctl machine bmc-reconcile <machine> [flags]
```

Flags:

```text
      --rotate-monitor   write a freshly generated monitor password and store it in pop/<pop>/bmc-creds-monitor, instead of re-asserting the stored one. The oob-gw ipmi_exporter and sel-archiver keep the old password, so this BMC's scrapes fail (ipmi_up=0) until "ansible-playbook oob-gw.yaml --tags sel_archiver,ipmi_exporter" re-renders their creds
```

#### slagctl machine commission

trigger commission for a machine

```text
slagctl machine commission <machine> [flags]
```

Flags:

```text
      --pxe-enable machine pxe-enable   set pxe_eligible=true first (skips the separate machine pxe-enable step)
      --timeout duration                maximum wait time (with -w) (default 30m0s)
      --via string                      power transport: "bmc", "pdu" (power-cycle via the device's switched PDU outlets; for BMC-less switches), or "amt" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc
  -w, --watch                           watch the operation until it completes
```

#### slagctl machine creds

print OOB credentials for a NetBox device

Look up a NetBox device by hostname, id, or serial and print its
out-of-band credentials from the pop's bmc-creds-admin AWS secret.

Works for any device modeled in NetBox — registered as a slagworks
machine or not — and for all five credential kinds the secret carries
(bmc, amt, pdu, serial, serial-server). The secret read is proxied through
slagworksd global under the admin auth gate, so the operator needs no
direct access to the AWS secret.

A server that sets SLAGWORKS_OIDC_CREDENTIAL_GROUPS returns credentials
only to operators who are also in one of those groups; anyone else gets
a 403 naming that setting. Ask a slagworks administrator for membership,
then run "slagctl auth login" for a token that carries the group.

To get the credentials for the PDUs feeding a BMC-less switch, name the
PDU devices directly (e.g. "slagctl machine creds pdu-0102-left").

Output is plaintext credentials — handle accordingly.

```text
slagctl machine creds <device>
```

#### slagctl machine deploy

trigger deploy for a machine

```text
slagctl machine deploy <machine> [flags]
```

Flags:

```text
      --destructive                     skip disk safety check (wipe existing data)
      --pxe-enable machine pxe-enable   set pxe_eligible=true first (skips the separate machine pxe-enable step)
      --recovery                        boot into recovery sshd instead of running the deploy agent
      --timeout duration                maximum wait time (with -w) (default 30m0s)
      --via string                      power transport: "bmc", "pdu" (power-cycle via the device's switched PDU outlets; for BMC-less switches), or "amt" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc
  -w, --watch                           watch the operation until it completes
      --wipe-all-disks                  also signature-wipe every disk NOT in the storage layout (clears stale md superblocks/filesystems; implies --destructive)
```

#### slagctl machine list

list machines

```text
slagctl machine list [flags]
```

Flags:

```text
      --state string   filter by state
  -w, --watch          refresh every 3s
```

#### slagctl machine lock

lock a machine to prevent new operations

```text
slagctl machine lock <machine>
```

#### slagctl machine logs

tail a machine's structured log envelopes (or --sol for the BMC serial console)

Tail logs for a single machine.

Default mode opens a WebSocket to /api/v1/logs/stream on the global API
and tails structured log envelopes scoped to this machine. Envelopes
flow from agents (deploy/commission), site workers, and global itself;
one trace_id correlates the full lifecycle of an op. Deploy logs are a
subset of this stream. By default it backfills the last --since window
(10m if unset) and exits; pass -f to follow live after backfill.

With --sol it instead streams the machine's BMC SOL (serial console)
byte feed from /api/v1/console/stream — raw serial output rather than
structured envelopes. --from-start replays history, --tail backfills a
duration, -f follows the live edge.

Examples:
  slagctl machine logs kworker-l34-1.pop1            # last 10m, exit
  slagctl machine logs kworker-l34-1.pop1 --since 1h
  slagctl machine logs kworker-l34-1.pop1 -f         # live tail after backfill
  slagctl machine logs kworker-l34-1.pop1 --op 8b2e3f --source agent
  slagctl machine logs kworker-l34-1.pop1 --sol -f   # live BMC serial console
  slagctl machine logs kworker-l34-1.pop1 --sol --from-start

```text
slagctl machine logs <machine> [flags]
```

Flags:

```text
  -f, --follow          follow live after backfill (default: exit when caught up)
      --from-start      (--sol) replay from cast_started_at (potentially long history)
      --level string    minimum level (debug|info|warn|error)
      --op string       operation UUID to filter by
      --raw             (--sol) write raw bytes to stdout without alt-screen/cursor handling
      --since string    backfill window (default 10m; e.g. 5m, 1h, RFC3339 timestamp)
      --sol             stream the machine's BMC SOL serial console instead of log envelopes
      --source string   comma-separated sources (agent,site,global)
      --tail string     (--sol) start at now-DURATION (e.g. 10m, 90s; a bare number = seconds); empty = live edge or --from-start
      --trace string    trace UUID to filter by
```

#### slagctl machine match

show which config scripts (network + storage) a host resolves to (read-only, creates no op)

Reports the network-config script and storage script the owning worker would select for this host, by building the same snapshot the boot path uses and reading the authoritative match off it (so it can't drift from a real deploy). Answers "what would this host get?" before firing one.

A script shown as "(none)" means no script of that kind matched — the agent falls back to its built-in DHCP netplan (network) or default layout (storage). NOTE this proves the MATCH only, not that the script will render correct config from the host's NetBox facts.

```text
slagctl machine match <machine>
```

#### slagctl machine ops

list operations run against a machine

List the operations recorded against a machine, newest activity at the
bottom. Columns match "operation list".

By default every op is shown. --power narrows to power-actuating ops
(explicit power on/off/cycle plus deploys and commissions, whose
executors power-cycle the host into PXE); --deploy narrows to deploy
and commission lifecycle ops. The two flags are mutually exclusive.

```text
slagctl machine ops <machine> [flags]
```

Flags:

```text
      --deploy   show only deploy/commission lifecycle ops
      --power    show only power-actuating ops (power_on/off/cycle + deploy/commission)
  -w, --watch    refresh every 3s
```

#### slagctl machine power

control machine power state

```text
slagctl machine power
```

##### slagctl machine power boot-set

set the BMC's next-boot override (Supermicro only)

```text
slagctl machine power boot-set <machine> [flags]
```

Flags:

```text
      --device string   boot device: pxe, disk, or default (clear)
      --persistent      apply to all future boots (default: one-time)
```

##### slagctl machine power boot-status

show the BMC's current next-boot override (Supermicro only)

```text
slagctl machine power boot-status <machine>
```

##### slagctl machine power cycle

power-cycle a machine

```text
slagctl machine power cycle <machine> [flags]
```

Flags:

```text
      --via string   power transport: "bmc", "pdu" (actuate the device's switched PDU outlets; for BMC-less switches or a wedged BMC), or "amt" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc
```

##### slagctl machine power off

power off a machine

```text
slagctl machine power off <machine> [flags]
```

Flags:

```text
      --via string   power transport: "bmc", "pdu" (actuate the device's switched PDU outlets; for BMC-less switches or a wedged BMC), or "amt" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc
      --yes          skip confirmation prompt
```

##### slagctl machine power on

power on a machine

```text
slagctl machine power on <machine> [flags]
```

Flags:

```text
      --via string   power transport: "bmc", "pdu" (actuate the device's switched PDU outlets; for BMC-less switches or a wedged BMC), or "amt" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc
```

##### slagctl machine power status

show current BMC power state

```text
slagctl machine power status <machine>
```

#### slagctl machine preflight

check a host's NetBox model for the config a deploy/commission boot needs (read-only, creates no op)

Runs the same op-creation validation global performs (the real snapshot-build matching path) without creating an operation, so you can confirm a host is fully modeled before firing a deploy.

Validates: primary_ip4 set, a bootable NIC with a MAC, a matching network-config script (or plain-DHCP intent), a matching storage script (deploy only), and a registered image of the right kind.

```text
slagctl machine preflight <machine> [flags]
```

Flags:

```text
      --op string   operation to validate for: deploy or commission (default "deploy")
```

#### slagctl machine pxe-disable

disable PXE boot for a machine

Sets pxe_eligible=false, preventing the machine from being PXE booted.

```text
slagctl machine pxe-disable <machine>
```

#### slagctl machine pxe-enable

enable PXE boot for a machine

Sets pxe_eligible=true, allowing the machine to be PXE booted for
commission/deploy operations.

Renamed from "activate" 2026-05-19 to make the verb scoped to PXE
rather than ambiguous machine state. PXE-enable is blocked for
locked machines.

```text
slagctl machine pxe-enable <machine>
```

#### slagctl machine show

show machine details

```text
slagctl machine show <machine>
```

#### slagctl machine unlock

unlock a machine to allow new operations

```text
slagctl machine unlock <machine>
```

### slagctl netbox

NetBox integration commands

```text
slagctl netbox
```

#### slagctl netbox bootstrap

ensure required NetBox objects exist (custom fields, webhook, event rules)

```text
slagctl netbox bootstrap
```

#### slagctl netbox diff

compare a machine's persisted Slagworks bindings with live NetBox

Compares the independently persisted machine UUID, NetBox device ID, and
serial. Hostname, site, BMC IP, MACs, lifecycle state, commission time, and
snapshot sequence are included as one-sided context because those values do
not exist independently on both sides.

Exits non-zero for a mismatched binding or a missing side, after printing the
complete human or JSON report.

```text
slagctl netbox diff <machine>
```

#### slagctl netbox migrate-chassis-serial

move chassis_serial custom field values to the built-in serial field (dry-run by default)

Walks every slagworks-managed device and reconciles the legacy
chassis_serial custom field with the built-in serial field:

  - cf empty / missing                                     -> skip
  - cf set to a DMI placeholder, built-in empty            -> clear cf only
  - cf set, built-in serial empty                          -> write cf -> serial, clear cf
  - cf set, built-in serial == cf                          -> clear cf only
  - cf set, built-in serial != cf, both non-empty          -> conflict, leave untouched

Conflicts are reported for operator triage. Idempotent.

Run order: this should be invoked AFTER the agent rollout that stops
writing chassis_serial. Running it earlier means a re-commission can
re-populate the cf after migration clears it, and divergence between
new chassis serials and old cf values will surface as conflicts on
the next migration run.

```text
slagctl netbox migrate-chassis-serial [flags]
```

Flags:

```text
      --apply   write changes (default is dry-run)
```

### slagctl network-script

manage host network-config scripts

```text
slagctl network-script
```

#### slagctl network-script delete

delete a network-config script by ID

```text
slagctl network-script delete <id>
```

#### slagctl network-script history

list every version of a network-config script

```text
slagctl network-script history <name>
```

#### slagctl network-script list

list network-config scripts

```text
slagctl network-script list [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

#### slagctl network-script update

mint a new version of a network-config script's content, by name

```text
slagctl network-script update <file> [flags]
```

Flags:

```text
      --name string   script name to update (required)
  -y, --yes           skip confirmation prompt
```

#### slagctl network-script upload

upload a network-config script from a file

```text
slagctl network-script upload <file> [flags]
```

Flags:

```text
      --name string   script name (required)
```

### slagctl operation

manage operations

```text
slagctl operation
```

#### slagctl operation list

list operations

List operations.

By default only active (non-terminal) operations are shown — pending,
claimed, or running. Pass --all to include completed and failed ops.

Rows are ordered by last activity with the freshest at the BOTTOM, so
under -w the most-recently-changed op tails the table like a log.

```text
slagctl operation list [flags]
```

Flags:

```text
      --all              include completed/failed ops (default: active only)
      --machine string   filter by machine ID
  -w, --watch            refresh every 3s
```

#### slagctl operation show

show operation details

```text
slagctl operation show [id]
```

#### slagctl operation watch

watch an operation until it completes

Watch an operation until it reaches a terminal state.

The argument is either:

  - an operation UUID (watch that specific op)
  - a machine UUID, NetBox serial, hostname, or unique hostname
    substring (watch the latest active op for that machine)

Machine resolution lets on-call type
"slagctl operation watch cpu1.pop1" without copying op IDs.

```text
slagctl operation watch <op-id|machine> [flags]
```

Flags:

```text
      --hold               hold the TUI open until q even when non-interactive (e.g. recording under CI)
      --no-hold            tear down immediately on completion instead of holding for q (default: interactive runs hold until q)
      --plain              stream log entries to stdout without TUI
      --timeout duration   maximum wait time (default 30m0s)
```

### slagctl power

PDU-centric power control

```text
slagctl power
```

#### slagctl power outlet

control a PDU outlet by address

```text
slagctl power outlet
```

##### slagctl power outlet cycle

power-cycle a PDU outlet

```text
slagctl power outlet cycle <pdu-name> <outlet> [flags]
```

Flags:

```text
      --allow-self   allow cycling an outlet that feeds the serving worker itself (single-worker site; the worker briefly drops itself)
      --yes          skip confirmation prompt
```

##### slagctl power outlet off

turn a PDU outlet off (strands device until an explicit 'on')

```text
slagctl power outlet off <pdu-name> <outlet> [flags]
```

Flags:

```text
      --yes   skip confirmation prompt
```

##### slagctl power outlet on

turn a PDU outlet on

```text
slagctl power outlet on <pdu-name> <outlet> [flags]
```

Flags:

```text
      --allow-self   allow cycling an outlet that feeds the serving worker itself (single-worker site; the worker briefly drops itself)
      --yes          skip confirmation prompt
```

### slagctl site-config

manage slagworks per-site config

```text
slagctl site-config
```

#### slagctl site-config create

create a site config

```text
slagctl site-config create [flags]
```

Flags:

```text
      --netbox-location string   NetBox location slug to narrow scope to (optional; empty = whole site)
      --netbox-site string       NetBox site slug to mirror (default: --slug)
      --pool-end string          last IP in ephemeral (PXE) pool
      --pool-start string        first IP in ephemeral (PXE) pool
      --promiscuous              serve DHCP to unknown MACs
      --slug string              worker identity (PK); defaults netbox-site to this if --netbox-site omitted
```

#### slagctl site-config delete

delete a site config

```text
slagctl site-config delete [slug]
```

#### slagctl site-config list

list all site configs

```text
slagctl site-config list [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

#### slagctl site-config show

show site config

```text
slagctl site-config show [slug]
```

#### slagctl site-config update

update a site config

```text
slagctl site-config update [slug] [flags]
```

Flags:

```text
      --clear-location           widen scope back to the whole NetBox site
      --clear-relay-pools        remove all relay pools
      --netbox-location string   narrow scope to this NetBox location slug
      --netbox-site string       change the mirrored NetBox site slug
      --no-promiscuous           disable promiscuous DHCP
      --pool-end string          last IP in ephemeral (PXE) pool
      --pool-start string        first IP in ephemeral (PXE) pool
      --promiscuous              enable promiscuous DHCP
      --relay-pool stringArray   relayed DHCP pool as subnet,start,end,gateway (e.g. 10.254.27.0/24,10.254.27.100,10.254.27.150,10.254.27.1); repeatable, REPLACES the full list
```

### slagctl ssh-key

manage global SSH authorized keys

```text
slagctl ssh-key
```

#### slagctl ssh-key add

add an SSH public key

Add an SSH public key. Pass the full key as a single argument (quote it).

```text
slagctl ssh-key add <public-key> [flags]
```

Flags:

```text
      --comment string   optional label for the key
```

#### slagctl ssh-key list

list SSH keys

```text
slagctl ssh-key list [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

#### slagctl ssh-key remove

remove an SSH key by ID

```text
slagctl ssh-key remove <id>
```

### slagctl storage-script

manage storage scripts

```text
slagctl storage-script
```

#### slagctl storage-script delete

delete a storage script by ID

```text
slagctl storage-script delete <id>
```

#### slagctl storage-script history

list every version of a storage script

```text
slagctl storage-script history <name>
```

#### slagctl storage-script list

list storage scripts

```text
slagctl storage-script list [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

#### slagctl storage-script update

mint a new version of a storage script's content, by name

```text
slagctl storage-script update <file> [flags]
```

Flags:

```text
      --name string   script name to update (required)
  -y, --yes           skip confirmation prompt
```

#### slagctl storage-script upload

upload a storage script from a file

```text
slagctl storage-script upload <file> [flags]
```

Flags:

```text
      --name string   script name (required)
```

### slagctl version

Print client and server (global API) versions

```text
slagctl version
```

### slagctl worker

manage site workers

```text
slagctl worker
```

#### slagctl worker delete

delete a site worker registration

Deletes a site worker's registration row, by uuid, worker-id, or fqdn.
Refused if the worker is currently connected, or has an in-flight
operation claimed. This is the teardown path for a retired site worker
(e.g. decommissioned scaffolding) — the row is what authenticates the
worker's WS connection to global.

```text
slagctl worker delete <ident> [flags]
```

Flags:

```text
  -y, --yes   skip confirmation prompt
```

#### slagctl worker list

list connected site workers

```text
slagctl worker list [flags]
```

Flags:

```text
  -w, --watch   refresh every 3s
```

#### slagctl worker register

mint a bearer token for a site worker

Mints (or rotates) a bearer token for a (site_slug, worker_id) pair.
The plaintext token is returned once; hand it to the site worker via
SLAGWORKS_TOKEN. Re-registering the same pair rotates the token.

A worker connected with the token receives the site's device credentials
with every operation it is sent, so a server that sets
SLAGWORKS_OIDC_CREDENTIAL_GROUPS mints tokens only for operators who are
also in one of those groups, as for "slagctl machine creds"; anyone else
gets a 403 naming that setting.

```text
slagctl worker register [flags]
```

Flags:

```text
      --fqdn string        worker host FQDN (optional)
      --slug string        site-config slug (see 'slagctl site-config list'; equals the NetBox site slug only for whole-site configs)
      --worker-id string   stable worker identifier
```

