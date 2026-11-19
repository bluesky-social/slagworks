# Architecture

Status: live (generated from code, 2026-07-03)

Slagworks is a bare-metal provisioning system: it discovers physical
hosts through NetBox, boots them over PXE, drives them through
commissioning and deploy, and hands the finished OS back to the
operator. Everything below describes the system as built after the
April-June 2026 rewrite (NetBox as source of truth, WebSocket snapshot
push, structured log envelopes, single boot image). Earlier design
docs under `docs/archeology/` describe a different world; do not trust
them as a guide to the code.

## Contents

- [Binaries](#binaries)
- [Global versus site split](#global-versus-site-split)
- [NetBox as source of truth](#netbox-as-source-of-truth)
- [The snapshot pipeline](#the-snapshot-pipeline)
- [WebSocket transport](#websocket-transport)
- [Site worker internals](#site-worker-internals)
- [The on-target agent](#the-on-target-agent)
- [API surface](#api-surface)
- [Secrets](#secrets)
- [Logging pipeline](#logging-pipeline)
- [Boot handlers](#boot-handlers)
- [Storage-script and network-script pipeline](#storage-script-and-network-script-pipeline)
- [Package map](#package-map)
- [Module boundaries a contributor must respect](#module-boundaries-a-contributor-must-respect)

## Binaries

Three Go binaries; two of them are the same executable running
different subcommands.

### `slagworksd`

Entry point `cmd/slagworksd/main.go:15`. A cobra root with three
subcommands: `global`, `site`, `version`. The build embeds the
short git SHA via `-X main.version=<sha>`; that string is what a site
worker sends on its WebSocket hello and what gates the site-worker
auto-upgrade. `scripts/build-deb.sh` also embeds the deb package
version via `-X main.packageVersion=<deb-version>`; global sends it
with its SHA so an upgrading worker installs exactly that package.

- `slagworksd global` — the central API and hub. Wired in
  `cmd/slagworksd/global.go:61` (`buildGlobal`). Postgres store,
  NetBox client, snapshot dispatcher, snapshot reconciler, WebSocket
  hub, log store, console-cast store, AWS Secrets reader, optional
  Tailscale minter, and the operator HTTP API all live here.
  OIDC is required: `global.go:62` refuses to start without
  `--oidc-issuer` and `--oidc-audience`.
- `slagworksd site` — the site worker, run on the out-of-band gateway
  in each cage. Configured entirely by environment (`SLAGWORKS_API_URL`,
  `SLAGWORKS_TOKEN`, `SLAGWORKS_SERVER_IP`, `SLAGWORKS_IFACE`, and a
  handful of optionals). Startup at `cmd/slagworksd/site.go:212`
  (`runSite`); the OOB DHCP/TFTP/HTTP listeners are gated by
  `SLAGWORKS_OOB_DHCP_ENABLED`. A subcommand `slagworksd site
  worker-id` inspects the persistent worker UUID stored in the local
  SQLite database.

### `slagworks-agent`

The initrd-time agent. `cmd/slagworks-agent/main.go` (about 91 KB and
still monolithic). Flags parsed at `main.go:132`:

```text
-server=host:port  -image=noble  -mode=deploy|commission|stdout|inventory
-destructive       -wipe-all-disks
-machine=<uuid>    -op=<uuid>    -trace=<uuid>    -expect-uuid=<uuid>
```

The iPXE boot script emitted by the site worker sets those values via
kernel command line. The agent also consumes a few string-form kernel
args: `slagworks.tailscale_authkey`,
`slagworks.tailscale_hostname` (`cmd/slagworks-agent/tailscale.go:35`),
`slagworks.no_reboot` (`main.go:492`), and the boolean bootline flags
`slagworks.destructive`, `slagworks.wipe_all_disks`,
`slagworks.recovery`.

Modes (dispatched from `main.go` around line 165):

- `stdout` — dump inventory JSON to stdout, no server.
- `inventory` — POST inventory to `/api/v1/inventory` and exit.
- `commission` — `runCommission` (`commission.go:406`). Emits phases
  `agent_starting`, `collecting_inventory`, `reporting`, and
  (conditionally) `console_probe` back through `POST /api/v1/report`,
  then sends the full inventory as an operation-bound
  `commission_report` through that same endpoint and reboots.
- `deploy` (default) — the long path in `deploy.go` `runDeploy`.
  Verifies identity, resolves the storage layout, renders the network
  configuration (runs the network script; both scripts run before any
  disk is touched, so a model gap fails the deploy with the host as
  found), plans and executes wipe / partition / mkfs, extracts the
  image, installs a bootloader, configures RAID, writes fstab, seeds
  cloud-init, writes the rendered network configuration into the
  target, writes the hostname, installs SSH keys,
  enrolls in Tailscale, writes the phone-home unit, reconciles the
  BMC, reports `deploy_complete`, and reboots.

All agent-to-site traffic is plain HTTP against the site worker's
HTTP-boot server (see [Boot handlers](#boot-handlers)). The final
phone-home is done by a systemd unit written by the agent
(`generatePhoneHomeUnit`, `main.go:1484`), not by the agent itself.

### `slagctl`

The operator CLI. Entry `cmd/slagctl/main.go:16`; root wiring in
`cmd/slagctl/cmd/root.go`. Every non-`auth` subcommand runs through a
`PersistentPreRunE` (`root.go:50`) that calls
`clientauth.Bearer` to obtain an OIDC access token, then builds a
`slclient.Client` with that bearer. Token cache lives at
`~/.config/slagctl/token.json` (`vendor/.../clientauth.go:139`).

Command groups (`root.go:82`): `auth`, `site`, `machine`, `operation`,
`image`, `storage-script`, `network-script`, `ssh-key`, `worker`,
`netbox`, `version`. The interactive verbs (`console`, `logs`,
`machine detail`, `watch`, `ops`, `creds`) live alongside the group
files. The three-pane TUI drives itself out of `cmd/slagctl/tui/` and
is entered by `cmd/slagctl/cmd/tui_run.go`.

OIDC configuration is compiled in: issuer and public client id are
constants at `cmd/slagctl/cmd/authconfig.go`, with `SLAGWORKS_OIDC_*`
environment overrides for testing. `slagctl auth login` supports
loopback PKCE (default) and `--device` for headless flows.

## Global versus site split

The daemon binary is one executable; the split is entirely in the
subcommand.

Global-only components (constructed in `buildGlobal`):

- Postgres store (`internal/store`) with goose migrations.
- NetBox client and webhook receiver.
- Snapshot subsystem: `snapshot.NewInvalidator`,
  `snapshot.NewClaimResolver`, `snapshot.NewDispatcher`,
  `snapshot.NewReconciler`, plus the identity mirror (`internal/mirror`)
  and the commission observation handler (`internal/commission`).
- WebSocket hub (`ws.NewHub`) — accepts site-worker connections at
  `/api/v1/ws/site`.
- Log store (`internal/logstore`) and per-process log ring
  (`internal/logring`) for the `/api/v1/logs/stream` live tail.
- Console-cast pipeline (`hubhandlers.CastBatch`/`CastResume`).
- AWS Secrets reader with monitor-secret bootstrap.
- Tailscale key minter (optional; requires OAuth client env).
- The operator HTTP API (`internal/api.Server`) with OIDC middleware.

Site-only components (constructed in `runSite`):

- Local SQLite database (`internal/worker.LocalDB`) at
  `/var/lib/slagworks/worker.db`.
- In-memory materialization of the pushed snapshot
  (`internal/worker.Cache`).
- The WebSocket client (`internal/worker.WSConn`).
- Site-facing listeners: `internal/dhcp` handler and server,
  `internal/tftp` server per bound interface, `internal/httpboot`
  server per bound interface. Optional OOB bind is derived from
  `SLAGWORKS_OOB_DHCP_ENABLED`.
- The op executor (`internal/worker.Executor`) — runs only on the
  worker that holds the site VIP.
- Cast manager and pump for BMC SOL streaming
  (`internal/worker.CastManager`, `castpump.go`).
- Health server on `SLAGWORKS_HEALTH_ADDR`.
- VIP monitor (`internal/worker/ha.go`).

The site worker connects up over WebSocket. URL is built in
`internal/worker/wsconn.go:443` (`buildURL`): it takes `GlobalURL`
(`SLAGWORKS_API_URL`), rewrites `http[s]://` to `ws[s]://`, and
appends `/api/v1/ws/site?token=<SLAGWORKS_TOKEN>`. The token is
opaque; the hub looks it up in `site_workers.token_hash` (via
`store.HashWorkerToken`) and derives the site slug from the row, so a
worker cannot claim a site it does not own. On connect the worker
sends a `HelloMsg{WorkerID, FQDN, LastSeqNum, Version}`
(`ws/messages.go:185`); the hub responds with a snapshot or a
`no_change` depending on the sequence-number comparison.

### HA between site workers

`internal/worker/ha.go:11` defines a `VIPMonitor` that polls
`net.InterfaceAddrs` every 2 seconds looking for `SLAGWORKS_VIP` and
emits acquire/release edges on a channel. There is no direct
site-worker to site-worker coordination. Both HA workers stay
connected to the hub, both receive snapshot pushes into their local
`Cache`, but only the VIP holder runs `startSiteServices` (the DHCP,
TFTP, HTTP-boot, cast manager, executor). The other worker is a warm
standby. The VIP itself is owned by keepalived — see the comment at
`cmd/slagworksd/site.go:1057`.

Both active and standby wire `SetPowerControllerFactory` on their
`WSConn` (`site.go:286`) so either can answer power-status RPCs from
the hub. Operation claim collisions are settled at the store level:
`store.ClaimOperation` (`internal/store/store.go:369`) is atomic under
a row lock, and the loser gets `ClaimOpResponse{AlreadyClaimed:true}`
back over the RPC (`ws/messages.go:213`).

## NetBox as source of truth

NetBox owns physical identity (device rows, interfaces, IP addresses,
cables, prefixes, IP ranges, locations). Slagworks reads that identity
into a snapshot and writes back the subset it observes at commission
time. See also `docs/archeology/netbox-integration-2026-03.md` for the operator-facing
side of what NetBox needs to look like.

The relevant packages sit at slightly overlapping names; three
distinct concepts share the word "reconcile":

- **`internal/netbox`** — the NetBox HTTP client
  (`client.go`), typed objects (`types.go`), read-only queries
  (`query.go`), and the write-back paths for interfaces
  (`interface.go` `ReconcileInterfaces`) and cables
  (`cable.go` `ReconcileCables`). This layer talks to NetBox only.
- **`internal/mirror`** — the single write path into Postgres
  identity tables (today: `machines`). Called from two sides: the
  NetBox webhook (fast path) and the periodic reconciler (safety
  net). `mirror.OnDeviceCreated`, `OnDeviceUpdated`, `OnDeviceDeleted`.
  Enforces `ErrSerialRequired` (`mirror.go:53`) and `ErrSlugImmutable`
  (`mirror.go:58`), and on a re-create with the same serial rebinds
  the existing row via `SetMachineNetBoxDeviceID`.
- **`internal/snapshot.Reconciler`** — a 5-minute ticker
  (`reconcile.go:39`) that walks every `site_config`, lists its NetBox
  devices, and calls `mirror.OnDevice*` for each. This is the safety
  net for missed webhooks. It also maintains a per-slug stale-since
  map (`markStale`/`clearStale`) that feeds `/readyz`.

### Webhook fast path

`internal/netbox/webhook.go:48` implements `WebhookHandler`, mounted
by the API at `POST /api/v1/hooks/netbox` (`internal/api/server.go:392`).
It verifies HMAC-SHA512 signatures via `X-Hook-Signature`
(`webhook.go:218`) using the shared `webhook-secret`; with no secret
configured it rejects every delivery with 503 and global logs at startup
that the endpoint is disabled. Handled models:

- `device`: `created` / `updated` / `deleted` — resolves the owning
  `site_config` via `snapshot.ClaimResolver` and calls the matching
  `mirror.OnDevice*` verb.
- `site`: only `updated` — enforces slug immutability and returns 409
  on a rename of a managed site.

Anything else is 200'd silently; the reconciler catches up. Comment at
`webhook.go:43` explicitly says NetBox does not retry, so the
reconciler is the recovery mechanism.

### What slagworks owns versus what NetBox owns

`internal/netbox/sync.go:122` (`SyncCommissionData`) is the outbound
direction. On a commission report, slagworks PATCHes the Device with
the fields it discovered:

- Top-level: `serial`, `asset_tag`, `name` (hostname), `status`
  (`active` on deploy), `device_type`.
- Tags: adds `slagworks-managed`.
- Custom fields: `slagworks_id`, `board_vendor`, `board_product`,
  `chassis_type`, `board_serial`.

Everything else on the Device (site, location, rack, `primary_ip4`,
device role, U-position, `device_type.custom_fields.slagworks_kernel_params`)
is operator-owned and read-only from slagworks' side.

Interfaces and cables are more permissive: `ReconcileInterfaces` will
create, patch (including rename), and delete NetBox interfaces to match
the NICs the agent reports, but with a protected-set
(`isProtectedInterface`) that leaves management, LAG, and virtual
interfaces alone. On switches (device role `leaf`, `spine`,
`oob-switch`) interface names and the row set are operator-owned: the
commission initrd's kernel names (`enp3s0npN`, `eno1`) are not the
operational `swpN`/`eth0`, so reconcile only patches non-name fields of
MAC-matched rows and never renames, creates, or deletes.
Cables are created from LLDP neighbours, additively: a cable is added
only between two uncabled interfaces, never on a management interface
or one hearing several neighbours (the shared OOB segment), and an
existing cable is never deleted or changed.

## The snapshot pipeline

The snapshot is the beating heart of slagworks. It is a single
per-site object that carries every fact a site worker needs to answer
DHCP, TFTP, HTTP-boot, and agent-facing endpoints. Everything else in
the site is a side effect of the snapshot.

Types are in `internal/model/snapshot.go`:

- `SiteSnapshot` (`snapshot.go:19`) — `SeqNum`, `Site` (`SiteInfo`),
  `DHCP` (`DHCPConfig`), `Machines`, `Images`, `SSHKeys`, `OOB`
  (`OOBConfig`), `Reservations`. Operations are deliberately NOT
  carried in the snapshot; they ride the dedicated op-event stream (see
  [WebSocket transport](#websocket-transport)).
- `MachineSnapshot` (`snapshot.go:171`) — one row per machine claimed
  by the site config. Carries the boot decision inputs (`State`,
  `Locked`, `PXEEligible`), the boot payloads (`Interfaces`, `BMCIP`,
  `SystemUUID`, `KernelParams`, `StorageScript`, `NetworkScript`),
  and everything the agent needs at deploy time.
- `SnapshotInterface` (`snapshot.go:126`) — `Type` is one of
  `physical`, `ipmi`, `lag`; only physical NICs are matched by DHCP.
  A `mgmt_only` interface is `ipmi` only when the device has a canonical
  NetBox `oob_ip`; on a BMC-less device it remains a physical host/PXE NIC.
- `ContentHash()` (`snapshot.go:280`) — SeqNum stripped, slices sorted
  in copy, sha256. This is the dispatcher's push-decision key.

### Builder

`internal/snapshot/builder.go:66` is `Build(in Inputs) (SiteSnapshot,
error)`. It is pure — no I/O — and fails loudly if `Prefixes` is
empty (`builder.go:68`). Its inputs (`Inputs`, `builder.go:31`) are
NetBox reads (site, config, prefixes, OOB prefixes and ranges,
devices, device-types, interfaces, IPs), Postgres reads (machines,
images, storage scripts, network scripts, SSH keys), and
an optional `BMCAdminSecret` from AWS Secrets for admin-cred
prestamping.

Notable helpers inside `Build`:

- `l3LegRelayPools` (`builder.go:91`) — assembles per-leg relay pools
  for L3 fabrics from prefixes tagged with `oob-relay`.
- `newPrefixGatewayResolver` (`builder.go:137`) — resolves prefix
  gateways for interfaces.
- `parseNetworkScripts` (`builder.go:158`) plus
  `matchNetworkScript` — the per-machine network-script picker.
- `matchStorageScript` (`builder.go:185`) — delegates to
  `storage.Match`; the winning script's `Content` lands in
  `MachineSnapshot.StorageScript`.
- `extractKernelParams` (`builder.go:199`) — pulls
  `device_type.custom_fields.slagworks_kernel_params` and stamps it
  on the machine snapshot.

### Dispatcher

`internal/snapshot/dispatcher.go:59` is where "make a fresh snapshot
and push it" lives. Interfaces at the top (`dispatcher.go:19`):

- `NetBoxReader` — every read the builder needs; the real
  implementation is `internal/netbox.Client`, tests use
  `snapshot/fakes_test.go`.
- `StoreReader` — every Postgres read the builder needs; real
  implementation is `internal/store.Store`.
- `PushSink` — the target of push. In production this is
  `ws.Hub.Push`; the test dispatcher can be pointed at a fake.
- `BMCSecretsReader` — optional admin-cred reader.

`Rebuild(ctx, slug, gen)` (`dispatcher.go:99`) does the whole cycle:
gather all reads, call `Build`, compute `ContentHash`, compare with
`cfg.LastSnapshotHash`, and either skip (metric label
`skipped_no_change`) or bump `site_configs.last_snapshot_seq` and
push (`success`). The per-scope filtering lives in
`internal/snapshot/scope.go`: a device is included in a site's
snapshot only if `Scope.ClaimsDevice(d)` returns true (whole-site
configs claim everything under the NetBox site; location-scoped
configs claim only the location subtree — `scope.go:126`).

The read-only variant `BuildSnapshot` (`dispatcher.go:157`) is what
the API's preflight and match endpoints call — it takes the same
read path so the answer matches what a real boot would see.

### Invalidator

`internal/snapshot/invalidator.go:22` is the debounced trigger. Every
site slug has a dirty generation counter and a 500ms debounce
window; `Invalidate(slug)` bumps the generation and, if a rebuild is
not already in flight, schedules one. If the generation advances
mid-rebuild the loop retries (`runRebuild`, `invalidator.go:161`).
The `Invalidator` is called by:

- The NetBox webhook (via `mirror` writes that fan out through the
  event pubsub in `internal/store/op_events.go`).
- `hubhandlers.OpUpdate.SetInvalidator` (`hubhandlers.go:102`) — on
  a real phase transition or a terminal status change of any
  operation. Wired from `global.go:191`.
- Direct manual invalidation from admin endpoints.

### Preflight, reconciler, hardware census

- `preflight.go` (`snapshot/preflight.go:65`) is `PreflightMachine`.
  Given the same read-shaped input a boot would see, it returns a
  list of human-readable gaps: machine not in snapshot, no
  `primary_ip4`, boot NIC without MAC, no network-script match, no
  storage-script match (deploy only), no image row, no kernel params
  (deploy hard-fails on empty; the sentinel value `none` is
  allowed), AWS admin/monitor secret missing. Called by both the
  operator API (`GET /machines/:id/preflight`) and internally on op
  create.
- `reconcile.go` (`snapshot/reconcile.go:39`) is the 5-minute
  safety-net ticker described above.
- `hardware_allowlist_test.go` is CI-only. `TestHardwareAllowlist`
  enumerates every `(role, device_type, hostname)` class expected in
  the fleet and asserts that shipped storage and network scripts
  match only those classes and nothing else. There is no runtime
  allowlist file; this is a compile-time invariant.

## WebSocket transport

Every site-to-global message rides one WebSocket connection. Types
are declared in `internal/ws/messages.go`; the transport lives in
`internal/ws`.

Envelope (`messages.go:15`): `Type`, `RequestID`, `Data`.
`MarshalEnvelope` / `ParseEnvelope` at the bottom of the file.

Message types (constants at `messages.go:470`):

Site to global:

- `hello` (`HelloMsg`, line 250) — first message on connect.
- `heartbeat` (`HeartbeatMsg`, 112).
- `observation` (`ObservationMsg`, 45) — commission reports,
  inventory reports, deploy-complete envelopes.
- `op_update` (`OpUpdateMsg`, 52) — phase/status transitions
  observed by the worker.
- `log_batch` (`LogBatchMsg`, 65) — batched slog envelopes from both
  the site worker and the on-target agent.
- `cast_batch` / `cast_resume_request` (`CastBatchMsg`, 88;
  `CastResumeRequestMsg`, 96) — SOL/asciinema frames.
- `claim_op` (`ClaimOpMsg`, 264) — RPC to atomically claim a
  pending op.

Global to site:

- `snapshot` (`SnapshotMsg`, 295) — full site snapshot.
- `no_change` (`NoChangeMsg`, 314) — hello came in already-current.
- `op_delta` / `op_resync` / `op_removed` (`OpDeltaMsg`, line 434 /
  `OpResyncMsg`, 450 / `OpRemovedMsg`, 462) — the op-event stream.
  Operations left `SiteSnapshot` (Tier 2 Phase 2); they flow as per-op
  `op_delta`s, a full `op_resync` on every (re)connect, and
  `op_removed` for terminal ops.
- `power_status_request` / `boot_status_request` (`131`, `150`) —
  synchronous RPCs invoked by the API for operator reads.

There is a generic `rpc_response` envelope (line 319) with a
`Terminal` flag, and the slagctl-side stream types
(`log_stream_subscribe` and friends, 265-322) for the two live-tail
endpoints. `source=sol` envelopes on the log stream are rejected by
`hubhandlers.LogBatch.HandleLogBatch` (see below) — SOL bytes ride the
cast pipeline, and the `SourceSOL` constant no longer exists.

### Hub

`internal/ws/hub.go` defines a hub that:

- Authenticates and accepts worker connections (`ServeHTTP`, `hub.go:323`).
- Registers each connection under its `SiteSlug` derived from the
  token (`register`, 373).
- Fans snapshot pushes out to every connected worker for the slug
  (`Push`, 301) — one goroutine per worker, per-connection write
  order preserved via `workerConn.writeMu`.
- On hello, sends a snapshot or `no_change`. Coalesces concurrent
  hello-triggered rebuilds through a
  `singleflight.Group` (`helloRebuild`, 226).
- Dispatches inbound envelopes per type (`dispatch`, 701) to
  handlers implemented in `internal/hubhandlers`:
  `handleObservation` (789), `handleOpUpdate` (837),
  `handleLogBatch` (858), `handleCastBatch` (879),
  `handleCastResume` (901), `handleClaimOp` (962).
- Provides `PowerStatus` / `BootStatus` synchronous RPCs (453, 503)
  used by the operator API; the response is routed to the calling
  goroutine by `deliverRPCReply` (551).

The concrete handler implementations wired in `buildGlobal` live in
`internal/hubhandlers`:

- `OpUpdate` (`hubhandlers.go:76`) — writes `SetOperationPhase`,
  `CompleteOperationWithState`, or `FailOperation` to Postgres, and
  fires the snapshot invalidator on any real transition
  (`SetInvalidator`, line 102). Comment at line 80 explains why: before
  this wiring, phase changes landed in Postgres but the worker
  cache stayed frozen.
- `LogBatch` (`hubhandlers.go:210`) — inserts into `log_envelopes`
  via `logstore.Store.InsertBatch`, re-emits each envelope as JSONL
  on stdout (for the fleet log-shipper), and pushes into the
  global `logring.Ring` for `/logs/stream` subscribers. Rejects
  `source=sol` (`hubhandlers.go:243`) — SOL rides the cast pipeline.
- `CastBatch` / `CastResume` (`console_cast.go`) — persist frames
  into `console_cast_events` grouped by machine; answer resume
  requests with the current byte offset.

### Postgres listener

`internal/ws/listener.go` wraps Postgres LISTEN/NOTIFY. It runs a
`listenLoop` that reads notifications from a set of channels and
dispatches them to a `NotifyHandler`. This is how database-side
changes (from another `slagworksd` global replica or from goose
migrations that emit change events) become invalidations without a
polling loop.

## Site worker internals

### WebSocket client — `internal/worker/wsconn.go`

`WSConn` (line 31) manages the single upstream connection.

- `Run` (274) drives the connect / read / drain loops with backoff.
- Snapshot receive path: `applySnapshot` (461) calls
  `cache.ApplySnapshot`, persists the sync cursor
  (`localDB.SetSyncCursor`), and then `checkVersion` (495). If the
  hub's declared SHA differs from `main.version`,
  `SLAGWORKS_AUTO_UPGRADE=1`, and the hub declared a package version,
  `runUpgrade` (540) is fired with that SHA and package version.
  Upgrade and its 2-minute cooldown latch are described in the
  state-machine doc.
- Outbound drain loops: `drainLoop` (579) for observations and op
  updates, `logRingDrainLoop` (601), `castDrainLoop` (701),
  `heartbeatLoop` (670).
- RPC surface: `handlePowerStatus` (760) and `handleBootStatus`
  (840) are called on the receive path when the hub asks for a
  synchronous power or boot read; the controller is built by the
  injected `PowerControllerFactory` (`cmd/slagworksd/site.go:1123`).
- `ClaimOp` client (200) — sends `claim_op`, waits for a
  `ClaimOpResponse`. Returns `errClaimLost` if the response marks
  the op already claimed, else the fresh `Operation` row.

### Cache — `internal/worker/cache.go`

`Cache` (line 32) is an in-memory materialization of the pushed
snapshot. `ApplySnapshot(snap)` (line 464) rebuilds every map atomically
under the cache lock:

- `machines` keyed by ID, `macIndex` keyed by MAC (physical only; BMC/IPMI
  MACs are excluded from machine identity).
- `ipByMAC` (physical only) for DHCP reservations.
- `oobReservations`, `images`, `sshKeys`, `dhcpCfg`, `oobCfg`,
  `siteInfo`. The `ops` set is NOT rebuilt here — it is fed by the
  op-event stream (`op_delta` / `op_resync` / `op_removed`), not the
  snapshot (see [WebSocket transport](#websocket-transport)).

The cache rejects an out-of-order snapshot (SeqNum equal or less than
the current). It runs `imagepull.SyncImages` synchronously under
`imageSyncMu` on every apply (`cache.go:578`) — that is what pulls
images to disk if the snapshot points at a new SHA. `OnSnapshotApplied`
callbacks fire outside the lock; today the cast manager and DHCP
handler subscribe.

Public boot-time reads (called from the DHCP handler, the HTTP-boot
handler, and the executor):

- `MachineByMAC` / `MachineByID` — the single machine-snapshot
  accessors. Per-machine boot config (SystemUUID, StorageScript,
  KernelParams, NetworkScript, Interfaces, FQDN) is read directly off
  the returned `*MachineSnapshot`; the old per-field `*ForMAC` /
  `*ForMachine` accessor pairs were collapsed into these two
  (fable-review Tier 4 #7).
- `BootActionForMAC` — the boot decision table. Full
  documentation lives in the state-machine doc.
- `ReservedIPForMAC`, `IsOOBManagedNonMachine`, `MachineIDForMAC`.
- `GetDHCPConfig`, `GetOOBConfig`, `GetSSHKeys`, `GetSiteInfo`.

Observation intake (called from the HTTP-boot handler when the agent
POSTs `/api/v1/report` or `/api/v1/inventory` or the phone-home
service POSTs `/api/v1/phone-home`):

- `RecordDeployComplete` (382), `RecordPhoneHome` (404),
  `RecordCommissionReport` (434), `RecordInventoryReport` (446).
- `HandleProgressReport` — the entry point for `/api/v1/report`; sets
  op phase, or marks the op failed on `report.Failed`.
- `UpdateOpPhase` (1321) — the phase-write helper with the
  `awaiting_phone_home` clobber guard (documented in the
  state-machine doc).

Outbox handling: every observation and op update the cache accepts
is appended to a durable outbox row on the local SQLite (via
`LocalDB.AppendOutbox`) and queued for the WebSocket drain
(`cache.go:334 appendObservation`, `685 DrainOutbox`, `805 RequeueOutbox`,
`824 AckObservation`, `843 ReloadOutboxFromDisk`). Retry backoff is in
`armRetryTimerLocked` (724) and `RequeueRejected` (772). This is why
a site worker restart does not lose commission reports.

### Executor — `internal/worker/executor.go`

The executor (`Executor`, line 47) is the drive loop for pending
operations. `Run` (85) ticks every 5 seconds and calls `processOps`
(99): first `PendingPowerOps` from the cache, then
`PendingLifecycleOps`. Per-op:

- `MarkOpClaimed` locally in the cache first (line 120) to prevent
  the next tick re-driving.
- WS `claimOp` RPC (`Claimer` interface, implemented by
  `WSConn.ClaimOp`).
- On lost race: keep the local claimed mark, do nothing more.
- On transport failure: roll back to `OpPending` in the cache.
- On success: dispatch to `runLifecycleOp` (288) for deploy /
  commission, or `runPowerOp` (151) for power ops.

`runLifecycleOp` sets the BMC PXE boot device, power-cycles (or powers
on if off), emits phases `power_cycle` and `pxe_booting`, and marks
the op running. The agent's phone-home is what eventually completes
the op — the executor exits early and does not block on the deploy.

Power ops decode the runtime blob out of `op.Params`. The
`PowerControllerFactory` (`cmd/slagworksd/site.go:1123`) picks:

- `power/smcipmitool` on Supermicro (`Manufacturer == "supermicro"`)
  when the operator has installed SMCIPMITool on the worker.
  `SLAGWORKS_SMCIPMITOOL` picks the behavior when it's missing: `auto`
  falls back to bmclib, `required` is a hard error, `off` never uses it.
- `power/bmc` (generic bmclib/Redfish/IPMI) otherwise.
- `power/pdu*` when the op params carry a `pdu_runtime` blob (this
  is explicit, not derived from topology).

### Cast manager and pump

`CastManager` (`castmanager.go:30`) keeps one persistent
`ipmitool sol activate` PTY child per active machine, driven by
`Reconcile` on every `OnSnapshotApplied`. `castpump.go` implements
the pump that turns the child's stdout into `CastFrame` structs and
hands them to `WSConn.PushCastFrame`; frames flow up through
`TypeCastBatch` to `hubhandlers.CastBatch` and land in
`console_cast_events`.

### Local database — `internal/worker/localdb.go`

GORM over SQLite at `/var/lib/slagworks/worker.db` (unless
`SLAGWORKS_DATA_DIR` overrides). Tables:

- `kv` — worker identity (persistent UUID), sync cursor
  (last-applied `SeqNum`), and a per-boot instance id used to
  invalidate stale claims.
- `outbox` — pending observations and op updates.
- `cached_machines` — a compact snapshot fallback used on restart
  before the first snapshot lands.
- `dhcp_leases` — persisted DHCP leases so the pool survives
  restart; `NewLeaseStore` (`localdb.go:467`) bridges to the DHCP
  package.
- `current_deploy_ops` — one row per machine currently in a deploy
  cycle; consulted when serving `/boot/ipxe` for a MAC that comes
  in during a race.

### Image sync — `internal/imagepull`

`SyncImages(log, imagesDir, images)` (`imagepull.go:41`) iterates the
snapshot's images map. For each image it compares the on-disk
`<imagesDir>/<name>/.sha256` marker with the snapshot's declared
`sha256`; on mismatch it downloads the tarball from the URL in the
image row and calls `extractTarGz` (`imagepull.go:173`). Idempotent
and safe under concurrent snapshot applies (see
`imageSyncMu` in the cache).

## The on-target agent

Covered above under [Binaries](#binaries). Additional facts that
matter to contributors:

- The initrd baked into every image already contains the slagworks
  overlay `/init`. The agent binary is not baked in — the initrd
  fetches it at boot from the serving site worker's
  `GET /files/bin/slagworks-agent`. Practical consequence: an agent
  fix ships with the site worker's apt package, not with the boot
  image. `docs/dev/build-and-release.md` covers the rollout mechanics.
- Every agent-to-site path is plain HTTP over the fabric segment
  the target booted on. Endpoints (all handled in
  `internal/httpboot/handler.go`):

  ```text
  POST /api/v1/report                            (progress, phase, failure, deploy_complete, commission_report)
  POST /api/v1/inventory
  POST /logs/ingest                              (log envelopes)
  POST /api/v1/phone-home?mac=...                (from the installed OS)
  GET  /boot/ipxe?mac=...
  GET  /boot/ssh-keys
  GET  /files/bin/slagworks-agent
  GET  /files/bin/systemd-bootx64.efi
  GET  /files/images/<image>/vmlinuz
  GET  /files/images/<image>/initrd-local
  GET  /api/v1/machines/<id>/storage-script
  GET  /api/v1/machines/<id>/network-script
  GET  /api/v1/machines/<id>/network-facts
  GET  /api/v1/machines/<id>/kernel-params
  GET  /api/v1/machines/<id>/hostname
  ```

- `internal/bmcreconcile/` is a self-contained package the agent runs
  against `/dev/ipmi0` (in-band) to provision the `monitor` (USER) BMC
  account and disable every other non-reserved account (slot 2, the
  bmc-creds-admin account, is never touched). The deploy boot script
  carries the admin account's name (`slagworks.bmc_admin_user`, from the
  op's `bmc_runtime` or the snapshot), and the agent refuses before any
  write unless that account is at slot 2 — otherwise the leftover pass
  would disable slagworks' only BMC credential. It is gated on
  `reporter != nil`, which is true only when both `--machine` and
  `--op` are set. Running the agent manually skips BMC reconcile. The
  same package runs over the LAN from the site worker for the
  `bmc_reconcile` op (see below).
- The `deploy_complete` envelope carries a newly provisioned monitor
  password back to global; the observation handler
  (`internal/commission/handler.go`) upserts it into
  `pop/<pop>/bmc-creds-monitor`, keyed by NetBox serial.

## API surface

`internal/api/server.go` (about 1800 lines) hosts every operator API
route plus the machine-facing WebSocket mount. Routes are registered
in `registerRoutes` (`server.go:311`).

Unauthenticated:

- `GET /healthz`, `GET /readyz` — `RegisterHealthz`
  (`healthz_readiness.go:28`). `/readyz` checks
  `Readiness.FirstReconcileComplete` and whether any site has a
  stale-since older than the threshold.
- `GET /metrics` — Prometheus.
- `GET /api/v1/client-config` — the OIDC issuer and public client ID
  `slagctl auth login` reads before it has a token.
- `GET /api/v1/ws/site` — the hub mount. Auth is `?token=`, checked
  by the hub itself.
- `POST /api/v1/hooks/netbox` — the NetBox webhook. HMAC-signed.

OIDC-protected under `/api/v1` (middleware in `internal/api/oidcauth.go`):

- Reads: `GET /machines`, `GET /machines/:id`,
  `GET /machines/:id/power`, `GET /machines/:id/boot`,
  `GET /machines/:id/preflight`, `GET /machines/:id/match`,
  `GET /machines/:id/console-probe`,
  `GET /devices/:ident/creds`, `GET /operations`,
  `GET /operations/:id`, `GET /images`, `GET /ssh-keys`,
  `GET /storage-scripts`, `GET /network-scripts`,
  `GET /site-configs`, `GET /site-configs/:slug`,
  `GET /workers`.
- Streams (WebSocket upgrades, token in the `Authorization` header):
  `GET /logs/stream`, `GET /console/stream`. They accept an upgrade
  with no `Origin` header or a same-origin one and refuse any other
  origin (`acceptOperatorStream` in `logs_stream.go`).
- Machine writes: `PATCH /machines/:id`;
  `POST /machines/:id/deploy` / `commission` / `lock` / `unlock` /
  `activate` / `deactivate` / `power` / `boot` / `bmc-reconcile` /
  `abort`.
- Catalog writes: `POST /images`, `PUT /images/:name`,
  `POST` / `DELETE /ssh-keys`,
  `POST` / `DELETE /storage-scripts`,
  `POST` / `DELETE /network-scripts`,
  `POST /site-configs`, `PATCH` / `DELETE /site-configs/:slug`,
  `POST /site-workers`.
- Admin: `POST /admin/netbox/bootstrap`,
  `POST /admin/netbox/migrate-chassis-serial`.

The OIDC middleware (`oidcauth.go:44`) pulls `Authorization: Bearer
<token>`, calls the injected `Verifier` (built from
`--oidc-issuer` / `--oidc-jwks-url` / `--oidc-audience`), then runs an
`Authz` callback that checks the token's group claims against
`--oidc-required-groups`. 401 on missing or invalid, 403 on
authenticated-but-wrong-group. Test wiring injects
`Config.Verifier` and `Config.Authz` directly.

Notable support files:

- `preflight.go` — `preflightMachine` builds the same
  `SiteSnapshot` a real boot would consume via
  `snapshotBuilder.BuildSnapshot`, then calls
  `snapshot.PreflightMachine` on the pre-resolved view. Also
  checks AWS Secrets presence via
  `awssecrets.AdminSecretExists` / `MonitorSecretExists`.
- `match.go` — the `/machines/:id/match` reader; reuses the same
  read-only build.
- `bmc_runtime.go` / `pdu_runtime.go` / `tailscale_runtime.go` —
  build the `BMCRuntime`, `PDURuntime`, and `TailscaleRuntime`
  payloads that ride inside `op.Params`. `resolveBMCRuntime` takes
  the creds and `extra_config` only from the device's serial-keyed
  `kind=bmc` entry in `pop/<pop>/bmc-creds-admin`; with no usable
  entry the op is refused at creation, naming the entry to fix.
- `device_creds.go` — the `/devices/:ident/creds` endpoint,
  the single privileged surface for fetching BMC and PDU credentials.
- `console_stream.go`, `console_snapshot.go`, `console_probe.go` —
  the SOL live-tail and asciinema replay surface. The stream refuses a
  machine id that names no machine (404) before it creates a cast row.
- `logs_stream.go` — the structured-log live-tail.
- `netbox_admin.go` — the two admin bootstrap paths.

The operation timeout sweeper (`server.go:432`) runs on a 1-minute
ticker and fans out into `ExpirePendingOperations`,
`ExpireStuckOperations`, and `ExpirePhoneHomeOperations`, then strips
credentials from terminal ops' stored params
(`ScrubTerminalOperationCredentials`, also run once at startup). Every
JSON response goes through `operatorJSONSerializer`
(`op_redaction.go`), which redacts the credentials in any operation's
params. Full detail in the state-machine doc ("Credentials in
operations").

## Secrets

`internal/awssecrets` reads from AWS Secrets Manager in the region
supplied at construction. Two secret families per pop:

- `pop/<pop>/bmc-creds-admin` — the pool of admin credentials
  slagworks uses to reach every BMC and PDU in the pop, keyed by
  NetBox serial. The `kind=bmc` entry (the BMC's slot-2 account) is
  the only BMC credential slagworks uses; nothing is stored in
  Postgres. Read-only from slagworks' perspective; operators rotate.
- `pop/<pop>/bmc-creds-monitor` — the per-host read-only `monitor`
  credentials slagworks generates during BMC reconcile, keyed by
  NetBox serial with `last_known_hostname` / `last_known_ip`.
  Slagworks upserts one entry per host on `deploy_complete` and
  `bmc_reconcile_report`.

The remote `bmc_reconcile` op (`slagctl machine bmc-reconcile`) is
how a BMC is brought into compliance outside a deploy. Global resolves
the admin runtime like a power op (and refuses if the monitor secret
is missing), and the same way stamps the device's stored monitor
password (`monitor_runtime`) onto the op when the monitor secret holds
an entry for its serial; the site worker that owns the OOB network runs
the chassis-serial gate, then `ipmitool -I lanplus -H <bmc> -U <admin>
-C <cipher, default 17> -E` (password in `IPMI_PASSWORD`). Reconcile
refuses before any write unless the admin account is slot 2, re-asserts
the stored monitor password (an existing account is tested with
`user test` first and written only when the BMC doesn't hold it, since
MegaRAC refuses to set a password to its current value; it rotates to a
generated one only when nothing is stored or the op has
`rotate_monitor`), disables
every other account, and never writes boot order. Rotation is never a
side effect of compliance because the oob-gw exporters read the monitor
secret only when Ansible re-renders their config. A generated password
reaches global only in a `bmc_reconcile_report` observation; global
stores it in the monitor secret (a re-asserted one writes nothing) and
finalizes the op with non-secret facts (monitor slot, `monitor_rotated`,
`monitor_password_written`, disabled usernames, the time pass outcome). A failed `bmc_reconcile`
never moves machine state.

After the accounts the worker runs the time pass
(`bmcreconcile.ReconcileTime`) with the same admin credential: over
Redfish it enables NTP with the `ntp_servers` of the site's
`slagworks-oob` prefix (only when that prefix contains the BMC's IP;
otherwise the pass is skipped and reported, with no default server), sets
the timezone to UTC, and when the BMC clock is more than 60s from the
worker's, disables NTP, runs `ipmitool sel time set now` and re-enables
NTP. Time failures ride the report's `time_error` and never fail the op,
whose pass/fail stays the account compliance.

Public API:

- `NewClient(ctx, Config)` (`awssecrets.go:120`).
- `GetAdmin(ctx, pop)` (152) → `*AdminSecret`.
- `AdminSecretExists(ctx, pop)` (198),
  `MonitorSecretExists(ctx, pop)` (210) — used by preflight.
- `AdminSecret.Lookup(serial)` (238) → `*AdminHost`.
- `Client.UpsertMonitorEntry(ctx, pop, serial, entry)`
  (`monitor.go:76`) — called from the commission handler.
- `Client.EnsureMonitorSecret(ctx, pop)` (`monitor.go:173`) — creates
  the initial `_meta`-only stub when a `site_config` is created.

Global-owned application credentials (database URL, NetBox token,
webhook secret, Tailscale OAuth client) are read from a separate
secret managed by the deployment environment (not by this code);
`buildGlobal` accepts them as flags or environment.

Fakes for tests: `awssecrets.FakeReader` (279) and
`FakeMonitorWriter` (288).

## Logging pipeline

Every log line in slagworks — global, site, agent, and (via SOL)
serial — flows through a single envelope shape and one persistent
store.

### Envelope

`internal/slogkit/envelope.go`:

- `Source` (line 20) — `SourceGlobal`, `SourceSite`, `SourceAgent`.
  (`source=sol` was the older SOL path; ingest rejects it loudly so a
  stale worker binary fails visibly. SOL rides the cast pipeline.)
- `Envelope` (line 42) — `TS`, `Level`, `Source`, `Host`, `Msg`,
  `MachineID`, `MachineName`, `OpID`, `Phase`, `TraceID`,
  `ReceivedAt`, `Extra` (map). `MarshalJSON` elides zero UUIDs.

The `slogkit.Handler` (`handler.go:76`) is the slog handler wired
into every binary. It hoists first-class attributes (`machine_id`,
`op_id`, `trace_id`, `phase`, `machine_name`) out of the `Extra` map
into top-level fields for cheap indexing (`hoist`, line 172).

### Rings and store

- **Site ring** (`internal/logring`) — bounded in-process ring,
  100 MiB / 1 hour on the site worker (see
  `cmd/slagworksd/site.go:234`). Drop-oldest on pressure; a `dropped`
  counter is exposed on `Stats`.
- **Global ring** — the same `logring.Ring` type but sized 16 MiB /
  1 hour (`global.go:219`). This is the fan-out point for
  `/api/v1/logs/stream` subscribers.
- **Postgres** — `internal/logstore` writes `log_envelopes` rows in
  `InsertBatch` and queries them in `Query` with a
  `QueryFilter`. Retention is `DeleteOlderThan`.

### End-to-end flow

Agent produces a slog line:

1. `slogkit.Handler` writes to stderr and pushes into an
   `agentlog.Client` sink.
2. `agentlog.Client.Run` batches envelopes and
   POSTs to `http://<site>/logs/ingest`.
3. The site's `httpboot.Handler.handleLogsIngest` unmarshals and
   pushes into the site's `logring.Ring`.

Site produces a slog line: directly into the same `logring.Ring`
via the site slog handler's sink.

From the site ring:

1. `WSConn.logRingDrainLoop` (`worker/wsconn.go:601`) drains the ring
   and calls `sendLogBatch` (691) with `TypeLogBatch`.
2. Hub dispatches to `hubhandlers.LogBatch.HandleLogBatch`.
3. Handler inserts into `log_envelopes` and pushes into the global
   `logring.Ring`, and re-emits JSONL on stdout for the fleet
   log-shipper.

`slagctl logs` and `slagctl operation watch` open a WebSocket to
`GET /api/v1/logs/stream`, get a seed backfill from
`logstore.Query`, and live-tail from the global ring.

Trace IDs: every operation is minted with a `TraceID` on creation
(`store.go:314`), and the agent stamps it on every envelope via the
`-trace=<uuid>` flag. That is what makes `slagctl operation watch`
show a coherent stream across global, site, and agent sources.

## Boot handlers

The DHCP-TFTP-HTTPBoot stack on the site worker is what turns the
snapshot into an actual boot. All three listeners are wired in
`cmd/slagworksd/site.go:startSiteServices` (line 665).

### DHCP — `internal/dhcp`

`dhcp.Handler` (`handler.go:192`) is a full DHCPv4 server. Its
configuration (`Config`, 57) carries:

- `KnownMACs(mac)` — is this MAC in the snapshot at all?
  (`site.go:581`: `cache.GetMachineByMAC(mac) != nil`).
- `ReserveIP(mac)` — the static IP reservation for a MAC, populated
  from both machine primary IPs and OOB `Reservations`.
- `MachineIDForMAC(mac)` — for envelope tagging.
- `BootEligible(mac)` — the negative gate for OOB-managed
  non-machines. `!cache.IsOOBManagedNonMachine(mac)` — a MAC that
  is a reservation for a BMC, PDU, or unmanaged switch interface is
  known, but must not get a bootfile. Unknown MACs are still
  eligible on a promiscuous pool; the httpboot layer decides what
  to serve them.
- `OnACK(mac, opID)` — records DHCP phases (`dhcp_ack`, `ipxe_dhcp`)
  on the machine's booting op for the TUI (`site.go:646`).

The handler supports the multi-interface split-pool case
(`IfaceParams`, line 135), relay pools for L3 fabrics
(`RelaySubnet`, 19), and promiscuous mode which flows through to
TFTP (`AnyPromiscuous`, 363; used at `site.go:763`).

### TFTP — `internal/tftp`

Thin server (`Config`, `tftp/server.go:15`). Per-interface
instance, one per bound server IP. `KnownIPs` is the DHCP lease gate
so a machine that never got a lease cannot pull the iPXE binary.
`OnFetch` (`site.go`) records the TFTP fetch as an op event when a
booting op is tracked.

### HTTPBoot — `internal/httpboot`

The bulk of the on-fabric surface. `handler.go:139` is
`ServeHTTP` for `/boot/ipxe`:

1. Parse `mac` from the query.
2. Call `provider.BootActionForMAC(mac)` — the interface is
   `model.MachineProvider`, satisfied by `worker.Cache`. Machine
   registration is NetBox-driven; the handler never mints a new
   machine row from a boot request.
3. Emit phase `ipxe_loaded` on the booting op.
4. Dispatch to `bootLocalScript`, `commissionBootScript`, or
   `deployBootScript`. Each generates iPXE text with the correct
   `slagworks.*` kernel command-line arguments (the deploy script
   stamps `slagworks.machine`, `slagworks.op`, `slagworks.trace`,
   `slagworks.expect-uuid`, `slagworks.tailscale_authkey`, and the
   boolean flags).

The rest of the handler implements every agent-facing endpoint
listed under [The on-target agent](#the-on-target-agent).

## Storage-script and network-script pipeline

Both are strings of shell script matched to a host by NetBox
identity, executed by the agent with a JSON environment variable, and
whose stdout is a JSON document consumed by later agent logic.
Matching runs on global inside `snapshot.Build`; the site worker is a
passive carrier.

### Shared engine

`internal/configscript`:

- `Script` (`configscript.go:19`) — `Path`, `Priority`, `Rules
  []MatchRule`, `Content`.
- `Parse(path, content, validFields)` (line 38) — reads header
  directives `# slagworks:match <field> <regex>` and
  `# slagworks:priority <int>` from the leading comment block.
  Rejects unknown fields.
- `Match(scripts, resolve)` (line 106) — resolves each field via a
  callback and picks the highest-priority script whose rules all
  match.

### Storage — `internal/storage`

- `Layout` / `Device` / `Partition` / `Mount` (`layout.go`).
- `Plan(layout, inventory) []Command` (`plan.go:30`).
- `DefaultLayout(diskName)` (`default.go:5`) — the fallback the
  agent uses when the layout resolver hard-fails; today the agent
  refuses to boot on empty (documented in `main.go`).
- `Match(scripts, commissionData, identity)` (`script.go:105`) —
  vocabulary is `system_manufacturer`, `system_product`,
  `chassis_serial`, plus the NetBox identity fields.
- `ParseScript` refuses `slagworks:kernel_params` in script headers
  (`rejectKernelParams`, `script.go:59`) — kernel params live on
  `device_type.custom_fields`, not scripts.

### Network — `internal/snapshot/netscript.go`

- Vocabulary: `site`, `location`, `rack`, `u`, `device_type`,
  `hostname` (`NetworkScriptFields`, line 41).
- `matchNetworkScript` used by `Build` to select the per-machine
  script.

### End-to-end for storage

1. `snapshot.Build` calls `matchStorageScript` per machine and
   stamps `MachineSnapshot.StorageScript` with the selected script's
   name and content.
2. Snapshot rides over the WebSocket. `Cache.ApplySnapshot` stores it.
3. The httpboot handler answers
   `GET /api/v1/machines/<id>/storage-script` from
   `cache.StorageScriptForMachine`.
4. Agent (`main.go:759 resolveStorageLayout`) fetches the script,
   execs `runConfigScript("storage", scriptContent,
   "SLAGWORKS_INVENTORY", commData)` (line 795), and parses the
   resulting `storage.Layout` JSON. `storage.Plan(layout,
   inventory)` turns that into `[]Command` for wipe / partition /
   mkfs / mount.

Network scripts follow the same pipeline with
`SLAGWORKS_NETWORK_FACTS` as the env variable and the network
configuration applied against the target chroot before reboot
(`main.go:1740`).

Kernel params are the third rail: not a script, just the
`device_type.custom_fields.slagworks_kernel_params` value stamped
into `MachineSnapshot.KernelParams` at `builder.go:199`, served at
`/api/v1/machines/<id>/kernel-params`, and consumed by the agent's
`fetchKernelParams` when it stamps the bootloader config.

## Package map

Every directory under `cmd/` and `internal/`, one line each.

Commands:

- `cmd/slagworksd` — the daemon (`global`, `site`, `version`).
- `cmd/slagctl` — the operator CLI.
- `cmd/slagworks-agent` — the on-target commission/deploy agent.
- `cmd/qemutest-serialrelay` — QEMU test helper: bridges the VM's serial
  port to the simulated BMC's SOL inside the test network namespace.
- `cmd/qemutest-tftpd` — QEMU test helper: TFTP server run inside the
  test network namespace.

Packages:

- `agent` — Shared context for the agent's subsystems, with
  subpackages `bootloader`, `consoleprobe`, `execkit`, `inventory`,
  `netapply`, `netlib`, `storageapply` and `target`.
- `agentlog` — HTTP-batching slog sink used by the on-target agent
  to POST log envelopes to the site worker's `/logs/ingest`.
- `amt` — `amt/redir`: Intel AMT redirection-protocol client for
  serial-over-LAN.
- `api` — Global HTTP API (`Server`). All operator REST plus the
  `/api/v1/ws/site` mount and the two live-tail endpoints.
- `awssecrets` — AWS Secrets Manager client and monitor-secret
  bootstrap for `pop/<pop>/bmc-creds-{admin,monitor}`.
- `bmcreconcile` — IPMI user-table reconciler (monitor account,
  leftover disable), run in-band by the deploy agent and over the LAN
  by the site worker for the `bmc_reconcile` op.
- `commission` — Observation handler for `commission_report`,
  `deploy_complete`, `bmc_reconcile_report`, and `dhcp_lease` events.
  Writes commission data back to NetBox, stores the BMC monitor
  credential in AWS, finalizes `bmc_reconcile` ops, and records the
  DHCP lease ledger.
- `configscript` — Shared config-script parser and matcher used by
  both the storage and network script pipelines.
- `dhcp` — DHCPv4 handler and server, multi-interface, split-pool,
  relay-aware, lease-persistent.
- `dockermirror` — Test helper that rewrites Docker Hub image paths
  through an optional pull-through cache for `dockertest`-based suites.
- `envutil` — Environment-variable-or-default helper shared by the
  command binaries.
- `healthserver` — Site-side `/healthz` / `/readyz` / `/metrics`
  binder.
- `httpboot` — Site-side HTTP-boot server. iPXE scripts, image
  assets, agent binary, every agent-facing endpoint.
- `hubhandlers` — Concrete implementations of the
  `ws.OpUpdateHandler`, `LogBatchHandler`, `CastBatchHandler`, and
  `CastResumeHandler` interfaces wired in `buildGlobal`.
- `imagepull` — `SyncImages` fetches missing image tarballs to
  disk on the site worker and writes `.sha256` markers.
- `logring` — Bounded in-process log ring with byte and age budgets
  and drop-oldest under pressure.
- `logstore` — Postgres `log_envelopes` write, query, and retention.
- `mirror` — Single write path into Postgres identity tables
  (machines), driven by both the webhook and the reconciler.
- `model` — Shared data types: `Machine`, `Operation`, `Image`,
  `SiteConfig`, `SiteSnapshot`, `SnapshotInterface`,
  `MachineSnapshot`, `OOBConfig`, `Reservation`, `Observation`,
  `ProgressReport`, `BMCCreds`, and the `MachineProvider` interface.
- `netbox` — NetBox HTTP client, typed request builders,
  `SyncCommissionData` (write-back), `WebhookHandler`, and the
  one-shot `MigrateChassisSerial`.
- `netboxtest` — Per-test integration environment: a NetBox container
  plus a slagworks Postgres, via `dockertest`.
- `opsmetrics` — Operation-level Prometheus metrics (phase durations,
  outcomes, expiry sweeps, worker package versions).
- `opstate` — Operation state-machine decisions: legal transitions,
  claim preconditions, auto-lock policy, failure branching.
- `power` — Power-controller abstraction. Implementations: `BMCController`
  (bmclib, Redfish and IPMI), `SMCController` (Supermicro, via the
  operator-installed SMCIPMITool; subpackage `smcipmitool`),
  `AMTController` (Intel AMT over WS-Man), `PDUController` (APC NMC:
  SSH actuation, Redfish status). Interfaces: `Controller`, `BootManager`.
- `qemutest` — QEMU full-stack integration suite: fake global hub,
  in-process site worker, PXE-booting VM, simulated BMC.
- `rdsiam` — RDS IAM-token login for global's Postgres connection.
- `sdnotify` — systemd `sd_notify(READY=1)` shim.
- `slogkit` — Shared slog envelope shape and handler.
- `snapshot` — The snapshot pipeline: `Build`, `Dispatcher`,
  `Registry` (invalidation), `Reconciler`, `ClaimResolver`, `Scope`,
  `PreflightMachine`, network-script matching.
- `storage` — Storage-layout DSL: `Layout`, `Plan`, `Match`,
  `DefaultLayout`, plus the script grammar wrappers.
- `store` — Postgres store (GORM), one file per table family
  (`machines.go`, `operations.go`, `scripts.go`, `site_configs.go`, ...).
  Migrations under `store/migrations/`.
- `tailscale` — One-shot tag-scoped auth key minter using the
  Tailscale OAuth client.
- `testpg` — Ephemeral Postgres harness for integration tests.
- `tftp` — Site-side TFTP server, one per bound interface.
- `worker` — Every site-worker component: `Cache`, `WSConn`,
  `LocalDB`, `Executor`, `CastManager`, `CastPump`, `VIPMonitor`,
  `BMCRuntime` / `PDURuntime` / `AMTRuntime` decoders, SOL-noise filter.
- `ws` — WebSocket transport: `Hub` (server), `Conn` wrapper,
  `Envelope` and typed messages, the claim-time outlet interlock, and a
  Postgres LISTEN/NOTIFY `Listener` that neither daemon starts.

## Module boundaries a contributor must respect

- **Snapshots are the only wire format.** A site worker cannot query
  Postgres, cannot call NetBox, cannot read AWS Secrets directly. If
  a fact needs to reach a site worker, add it to `SiteSnapshot`.
- **The mirror is the only writer to `machines` identity fields.**
  Never PATCH machines directly from a handler; go through
  `mirror.OnDevice*`, which fires the correct invariants and emits
  the correct events.
- **NetBox writes are limited.** `SyncCommissionData`
  (`internal/netbox/sync.go:129`) enumerates every field slagworks
  writes back. If a new observation needs to land in NetBox, add it
  there and update the doc.
- **Ops carry their runtime in `Params`, and the runtime is
  resolved on op creation, not on op execute.** That is what
  guarantees the executor sees the same BMC / PDU / Tailscale
  identity the operator saw at preflight time.
  `bmc_runtime.go`, `pdu_runtime.go`, and `tailscale_runtime.go`
  are the shape of those payloads.
- **`slogkit` is the only log envelope shape.** Do not invent a
  parallel schema. If a new hoisted field is needed, add it to
  `envelope.go` and `hoist()`.
- **Global-only vs site-only wiring.** If you find yourself adding
  a package that has to run on both sides, look at what state it
  actually needs; usually the answer is a snapshot field, not a
  new binary boundary.

## Open questions and rough edges

- `slagworks.fatal_at` and `slagworks.efivarfs` are test-only kernel
  arguments but are not gated by an env variable in production
  (`httpboot/handler.go:391`). A stray operator or a leaky test
  fixture could exercise them; the fix (a `SLAGWORKS_TEST=1` gate)
  is deferred and marked in-code.
