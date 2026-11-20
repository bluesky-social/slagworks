# Troubleshooting

Status: live (generated from code, 2026-07-03)

Common failure modes, in the order they tend to show up during a
deploy or commission, and how to diagnose each one. When something is
wrong, start at the phase where the operation stalled or failed and
work from there. The deploy phase spine (as defined in
`cmd/slagctl/tui/lifecycle.go`) is:

```
power_cycle → pxe_booting → dhcp_ack → ipxe_dhcp → ipxe_loaded →
agent_starting → partitioning → rsyncing_os → installing_bootloader →
finalizing → bmc_reconcile → awaiting_phone_home → completed
```

The commission spine shares the boot prefix and then diverges at
`collecting_inventory → reporting → console_probe → completed`.

## Two log feeds, choose the right one

Almost every triage step below picks between the same two feeds:

- `slagctl machine logs <machine>` — the structured envelope stream.
  Site-worker events, global events, and any agent code path that
  called `log.*`. Backfills the last 10 minutes by default; widen with
  `--since`. Add `-f` to follow live.
- `slagctl machine logs <machine> --sol` — raw serial off the BMC.
  Firmware POST, GRUB, kernel boot messages, and any agent output
  that went to stdout/stderr rather than `log.*`. `--from-start`
  replays what's still buffered; the SOL buffer rotates, so long-ago
  operations may have lost their finalizing window.

If a phase failed but the envelope stream has nothing useful, check
SOL. If SOL shows the host booting fine but nothing arrives in the
envelope stream, check whether the agent got network at all (DHCP
gates below).

## Preflight first

Before treating a failure as mysterious, run:

```
slagctl machine preflight <machine> --op deploy
```

Preflight (`internal/snapshot/preflight.go`) checks the static
NetBox/AWS preconditions the op needs and enumerates every gap in
plain English. This catches:

- No `primary_ip4` on the NetBox device.
- No bootable NIC with a MAC.
- Storage- or network-scripts configured site-wide, but none match
  this host's NetBox identity or commission data.
- No PXE image row registered (see "image not syncing" below).
- Empty `slagworks_kernel_params` custom field on the device-type
  (would hard-fail at bootloader install).
- Per-PoP AWS BMC admin or monitor secret missing.

If any gap turns up, fix it before diving deeper — several deploy
failures that look like BMC or boot problems are really unmet
preconditions.

`slagctl machine match <machine>` reports which scripts the host
actually resolves to, which is the fastest way to catch "the script
exists but doesn't match this host."

## Deploy fails at a specific phase

Every terminal failure names the phase it hit (visible in the TUI
header, in `operation show`, and in the `phase=<name>` prefix on
structured envelopes). The phases and what each means:

### `power_cycle`

The site worker could not power-cycle the host. Almost always a BMC
credential or network problem. See the BMC section below.

### `pxe_booting`

The host received power but never sent a DHCP request. Firmware boot
order may not be putting the target NIC first; BIOS/UEFI PXE may be
disabled; the NIC may not have a driver in the boot NIC's option ROM
for that firmware mode. Watch `--sol` to see whether PXE is even
attempted.

### `dhcp_ack`

The host sent a DISCOVER but was never offered a lease. See "machine
won't PXE" below for the gate cascade.

### `ipxe_dhcp` / `ipxe_loaded`

The host got a lease and pulled iPXE, but iPXE never chained through
to the slagworks HTTP boot script, or fetched it and never returned.
Structured envelopes from the site worker's HTTP boot handler
(`internal/httpboot/handler.go`) show the request; if no request
lands, check that the ephemeral pool's next-server and boot file
point back at the site worker and that the HTTP endpoint is
reachable from the host's subnet.

### `agent_starting`

The kernel booted, but the on-machine slagworks-agent never made its
first phone-home. Read `--sol` for panics, kernel command-line typos,
missing kernel modules for the boot NIC, or an initrd that refused
to bring the network up. If the site worker sees no envelope from
this machine ID after `ipxe_loaded`, the agent process either did
not start or could not reach the site worker.

### `partitioning`

The storage script matched, but disk layout failed. Envelopes carry
the exact `exec` call and its exit code (the `exec` message expands
into the argv and exit status; see the human renderer in
`cmd/slagctl/cmd/logs.go`). Typical causes: stale mdadm/DDF
containers from a previous OS (use `--wipe-all-disks` on the retry),
disks with unexpected controllers, or a script whose match ran but
whose template output referenced a device that isn't present.

### `rsyncing_os`

Partitioning succeeded, but the OS rsync from the site worker's HTTP
endpoint failed. Usually network or disk-fill; check `--sol` for I/O
errors and check the site worker's disk for the image being served.

### `installing_bootloader`

The rsync landed, but bootloader install refused. The most common
cause is an empty `slagworks_kernel_params` custom field on the
NetBox device-type — preflight catches this ahead of time.
Set the field to a valid `console=…` line, or `"none"` if the device
genuinely has no serial console.

### `finalizing`

Everything on-disk succeeded, but the tail work (fstab, hostname,
tailscale enrollment, first-boot hooks) hit an error. Read the
envelope stream for the specific handler that failed.

### `bmc_reconcile`

The deploy succeeded, but the agent's in-band BMC account reconcile
(provisioning the `monitor` account, disabling stray accounts) failed.
Non-fatal on the deploy path itself; the machine will boot into its
deployed OS regardless. Re-run it out of band with
`slagctl machine bmc-reconcile <machine>`.

A refusal logged as "the bmc-creds-admin account is not the slot-2
account" (with `admin_user` / `admin_slot` / `slot2_user`) means the
device's admin entry names an account outside slot 2 — reconcile wrote
nothing, because disabling stray accounts would have disabled it. Point
the entry at the slot-2 account. A "slot-2 admin gate skipped" warning
means the deploy had no admin entry to name, so the reconcile ran
ungated.

### `awaiting_phone_home`

This is not a failure — it is the terminal wait state. The host
booted off local disk (the site worker armed a boot-local iPXE idiom
during `finalizing`, so no PXE round-trip happens), and slagworks is
waiting for it to phone home over its production network. If the
host's production network hasn't converged yet (upstream switch
config not applied elsewhere in your fleet), the operation stays
here. That's correct.

## Machine won't PXE

Symptom: `dhcp_ack` phase never arrives, or the site worker's DHCP
metrics show DISCOVER-without-OFFER for the host's MAC.

The site worker's DHCP handler (`internal/dhcp/handler.go`) applies a
cascade of predicates:

1. `KnownMACs` — is this MAC registered to a machine in the site's
   snapshot? Rejected as `unknown_mac`. Fixes: verify the device
   exists in NetBox with the correct MAC on a non-management
   interface; verify the device is in scope for the site's config;
   verify the snapshot has rebuilt since you added it (invalidation
   is best-effort; the reconciler is authoritative). `--promiscuous`
   on the site-config disables this filter, at the cost of serving
   any random MAC that lands on the segment.

2. `ReserveIP` — for MACs that expect a static reservation (mgmt/BMC
   IPs), the handler serves the reserved IP and no PXE boot file.
   This is intentional: a MAC intended as a BMC or reservation is
   not a boot candidate.

3. `BootEligible` — is `pxe_eligible=true` on the machine? If not,
   the handler serves a lease with **no** boot file. Fix with
   `slagctl machine pxe-enable <machine>`. Locked machines can't be
   PXE-enabled.

For hosts across an L3 relay, verify the site-config has a matching
`--relay-pool` (subnet, start, end, gateway) — a request from a
subnet with no relay pool falls off the end of the cascade.

## BMC unreachable or credentials wrong

Symptoms: `power_cycle` fails, `machine power status` returns an
error, `machine power on` times out.

Slagworks uses exactly one credential to drive a BMC: the device's
entry in `pop/<pop>/bmc-creds-admin` (AWS Secrets Manager), keyed by
the device's NetBox serial, `kind=bmc`. It is the BMC's slot-2 admin
account. Slagworks stores no BMC password in Postgres and provisions
no account of its own. Preflight refuses any BMC-actuating op if the
secret does not exist, and every BMC op is refused at creation if the
device has no usable entry — the error names the secret path and
serial to fix.

The only other BMC credential is the read-only **monitor** account
(`pop/<pop>/bmc-creds-monitor`, also keyed by NetBox serial, with
`last_known_hostname` / `last_known_ip` fields), which slagworks
writes for external monitoring (ipmi_exporter) and never uses itself.
It is set by the deploy agent's `bmc_reconcile` phase and by
`slagctl machine bmc-reconcile`. Preflight refuses `deploy` if this
secret does not exist (a missing upsert target used to cause the
handler to replay indefinitely).

To bring a BMC's accounts into compliance (the stored monitor password
re-asserted, or generated for a BMC with none stored; stray accounts
such as a leftover `slagworks` or `maas` disabled) on
any host, including in-service hosts slagworks never deployed, run
`slagctl machine bmc-reconcile <machine>` and watch the op. It refuses
unless the admin entry names the BMC's slot-2 account; if it does not,
point the entry at the slot-2 account first.

`slagctl machine creds <device>` prints the credential the server
would use, resolving the device against NetBox (any device — BMCs,
PDUs, switches) rather than against the machine table.

Common causes of BMC failures beyond credentials:

- BMC session-slot exhaustion on AMI-based BMCs (common on
  Supermicro): only ~5 IPMI slots. Wait or reset the BMC via Redfish.
- IPMI channel password not propagated from a Redfish password
  change: some BMCs treat the two as separate credential stores.
- BMC IP unreachable from the site worker's OOB interface — verify
  routing and that the BMC is on the segment the site worker
  attaches to.

For BMC-less hosts (many switches), `--via pdu` on power and
deploy/commission commands actuates the device's switched PDU
outlets directly. Slagworks prints an explicit "Transport: PDU
outlets" line to stderr before acting — this is a hard AC actuation
on shared infrastructure, and an outlet-index error can de-power a
different device on the same strip.

## Image not syncing

Symptoms: a fresh build isn't reflected in what the host boots, or
`slagctl image list` shows a stale SHA.

Slagworks pins exactly one image row per environment. Site workers
sync whenever they apply a site snapshot, by comparing the row's
`sha256` against the version directory the image's link in the images
directory names (`noble-pxe -> noble-pxe@<first 16 hex digits>`,
`internal/imagepull/`), and retry a failed download on a backoff
(5 seconds, doubling to 5 minutes). To roll a new image:

1. Publishing a build advances `latest-sha.txt` in the image bucket
   ([images.md](images.md#publishing-an-image)). That pointer, by
   itself, has no effect on running workers.
2. `slagctl image follow-latest` resolves `latest-sha.txt`, verifies
   the sidecar `.sha256`, and upserts the image row to the immutable
   per-SHA URL. This is the load-bearing step.
3. Global pushes a snapshot carrying the new row to every site
   worker, and each syncs when it applies it.

Diagnosis order when a new image "isn't on the workers":

- `slagctl image list` — does the row's SHA256 column (the first 12
  characters; `--json` prints the full digest) match the SHA you
  expect? If not, `follow-latest` hasn't been run yet.
- Site-worker-side: does `readlink <images_dir>/noble-pxe` name the
  version `slagctl image list` pins? If not, the worker hasn't
  synced yet — check the site worker's structured logs for image
  sync events. `image sync failed; retrying` is a failed download the
  worker retries by itself; `image sync failed` with a `hint` (a
  digest mismatch, a tarball that does not extract, a refused name, a
  failed write) is not retried, so fix the row or the host.
  Re-running `follow-latest` on an unchanged row pushes nothing.
- The image row and the link match, but the boot doesn't behave
  like the new image? That's an agent question, not an image
  question — see next section.

## Agent version mismatch

Slagworks ships two independent artifacts:

- **The boot image** (kernel, initrd, root squashfs) — pulled by site
  workers via the image row above.
- **The slagworks-agent binary** — the agent logic itself.

The boot image contains the deployed OS and every tool the agent
execs on the target (`mkfs`, `rsync`, `ping`, `tailscale`, `mdadm`,
…), while the agent itself is downloaded by the initrd from the
serving site worker's local filesystem at boot time. The two are on
separate release paths.

Consequences:

- If a change touches `cmd/slagworks-agent/**`, it ships in the
  agent, and until the serving site worker's `slagworks` package is
  upgraded, deploys of that host use the old agent.
- If a change touches the boot image (kernel, initrd, in-image tool
  set), it ships in the image; `slagctl image follow-latest` picks
  it up.
- If a change touches global-only code (matching, snapshot
  building, the API), it takes effect as soon as global is
  redeployed; neither the image nor the agent changes.

Diagnosis: "my agent fix didn't take" — check the serving site
worker's installed slagworks package version, not the image SHA.
`follow-latest` never moves the agent binary. In an HA cage where
two workers serve, both must be upgraded; a stale partner serves the
old agent on operations it happens to claim.

## Stuck operations

An operation that will not leave `pending`, `claimed`, or `running`
after a reasonable wait — say, a `power_cycle` that never advances
to `pxe_booting`, or a deploy stuck in `agent_starting` for 20
minutes:

1. Read the operation with `operation show <id>`; note the phase and
   `claimed_by`.
2. Check the claiming site worker is connected — `slagctl worker
   list` shows last-seen timestamps.
3. Tail the envelope stream with `--op <id> -f` to see live activity.
4. If truly wedged, `slagctl machine abort <machine>` cancels active
   operations for the machine and resets its state. This does not
   power the host off; follow with `machine power off` if needed. An
   agent still running on the host stops within a few seconds of the
   abort and waits in the recovery environment (serial console:
   `agent exited with code 3`).

## SOL session-slot exhaustion

On AMI-based BMCs (common Supermicro deployments), the BMC has a
small number of IPMI session slots (typically five). Symptoms:

- "SOL payload already active on another session" errors from
  `--sol` streams.
- Frame-header EOFs and short reads.
- `power status` failures where the earlier read succeeded.

Do not rapid-retry. Options:

- Wait for existing sessions to time out (minutes).
- Reset the BMC via Redfish to clear all sessions.
- Confirm that no other operator or monitoring tool is holding a
  session against the same BMC.

The SOL feed itself is best-effort on these BMCs. For operations
whose SOL history has rotated off the buffer, the reliable fix is
to re-run the operation while capturing SOL live (`--sol -f`).
