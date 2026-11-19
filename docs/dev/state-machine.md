# State machine

Status: live (generated from code, 2026-07-03)

This is the canonical reference for the machine and operation state
machines as implemented today. Every state, transition, and boot
decision is cross-referenced against source with file and line. If
this document and the code disagree, the code wins and the document
is wrong.

Companion reading: `docs/dev/architecture.md` for how the pieces fit
together, and `internal/store/operations_test.go` for the machine
that actually enforces the invariants below.

## Contents

- [Machine states](#machine-states)
- [Operation types, statuses, and phases](#operation-types-statuses-and-phases)
- [State transition write sites](#state-transition-write-sites)
- [Claim semantics](#claim-semantics)
- [Boot-action decision table](#boot-action-decision-table)
- [Operation lifecycle](#operation-lifecycle)
- [Timeouts and phase-based expiration](#timeouts-and-phase-based-expiration)
- [Abort](#abort)
- [The `awaiting_phone_home` latch](#the-awaiting_phone_home-latch)
- [The upgrade latch on the site worker](#the-upgrade-latch-on-the-site-worker)
- [The `--destructive` and `--wipe-all-disks` flags](#the---destructive-and---wipe-all-disks-flags)
- [The no-auto-commission rule](#the-no-auto-commission-rule)
- [Open questions and rough edges](#open-questions-and-rough-edges)

## Machine states

`internal/model/machine.go:10` declares `MachineState` as a string.
The doc comment (`machine.go:12`) states the forward path:
`new -> commissioning -> commissioned -> deploying -> running`, with
`failed` reachable from any post-`new` transient state.

The constants (`machine.go:15`):

- `MachineStateNew            = "new"`
- `MachineStateCommissioning  = "commissioning"`
- `MachineStateCommissioned   = "commissioned"`
- `MachineStateDeploying      = "deploying"`
- `MachineStateRunning        = "running"`
- `MachineStateFailed         = "failed"`

Validation via `validMachineStates` + `Valid()` at `machine.go:24`.

Fields on the `Machine` struct (`machine.go:38`) that participate in
the state machine:

- `NetBoxSerial` (`machine.go:45`) — required; the stable rebind key
  when a device is deleted and re-created.
- `NetBoxDeviceID` (`machine.go:44`) — nullable during the small
  window between a delete and the reconciler catching up.
- `Locked` (`machine.go:54`) — the auto-lock latch. Set when a
  machine transitions to `running`, cleared when it transitions to
  `failed`. A locked machine cannot be claimed by a new
  commission or deploy op.
- `PXEEligible` (`machine.go:55`) — the operator-owned gate that
  keeps a live host from PXE-booting into an accidental reimage.
  Must be true for the boot-action decision to return anything
  other than `BootLocal`. Enabling it is refused while `Locked`
  (`SetMachinePXEEligible` returns `ErrMachineLocked`, HTTP 409);
  disabling it is always allowed.
- `CommissionData` (`machine.go:51`) — raw JSON of the last
  commission report. Empty means the machine has never been
  commissioned; this is the signal used by `FailOperation` to
  choose between reverting to `new` (initial commission failed)
  and going to `failed` (recommission failed).
- `CommissionedAt` (`machine.go:53`) — set alongside
  `CommissionData`.

There is no separate `MachinePhase` enum. Phase-shaped progress
information lives on the active `Operation`.

## Operation types, statuses, and phases

### Types

`internal/model/operation.go:10`:

- `OpDeploy          = "deploy"`
- `OpCommission      = "commission"`
- `OpPowerOn         = "power_on"`
- `OpPowerOff        = "power_off"`
- `OpPowerCycle      = "power_cycle"`
- `OpSetBootDevice   = "set_boot_device"` (Supermicro-only BMC boot
  override; used by the executor to nudge a target to PXE before a
  deploy)
- `OpBMCReconcile    = "bmc_reconcile"` (BMC IPMI-account and time
  (NTP/timezone/clock) compliance over the LAN; a time failure is
  reported, never fails the op; the worker reports through a `bmc_reconcile_report`
  observation and global finalizes the op from it)

`AllOperationTypes` (`operation.go:33`) is the enumerable list and
is asserted-against by a test that requires every op type to be
bucketed by `cache.PendingPowerOps` versus `PendingLifecycleOps`.

There is **no** `OpAbort` type. Abort is not an operation; it is a
verb on `Machine` that fails whatever ops are active. See
[Abort](#abort).

### Statuses

`operation.go:42`:

- `OpPending    = "pending"`
- `OpClaimed    = "claimed"`
- `OpRunning    = "running"`
- `OpCompleted  = "completed"`
- `OpFailed     = "failed"`

Terminal set is `{completed, failed}`; the store CAS guards
(`internal/store/store.go:559`, `587`, `785`) refuse to write over a
terminal status.

### Phases

`Operation.Phase` (`operation.go:58`) is a free-form string, not an
enum. `PhaseEnteredAt` (`operation.go:59`) is bumped only when the
phase actually changes (`internal/store/store.go:725`), which is
what makes the phone-home timeout budget work.

The canonical phase order comes from the TUI spines
(`cmd/slagctl/tui/lifecycle.go`):

Deploy spine (`lifecycle.go:47`):

```text
power_cycle -> pxe_booting -> dhcp_ack -> ipxe_dhcp -> ipxe_loaded
  -> agent_starting -> partitioning -> rsyncing_os
  -> installing_bootloader -> finalizing -> bmc_reconcile
  -> awaiting_phone_home -> completed
```

Commission spine (`lifecycle.go:67`):

```text
power_cycle -> pxe_booting -> dhcp_ack -> ipxe_dhcp -> ipxe_loaded
  -> agent_starting -> collecting_inventory -> reporting
  -> console_probe -> completed
```

Terminal: `completed` (`lifecycle.go:29`). Agent handoff phase is
`ipxe_loaded` (`lifecycle.go:40`); everything before that is emitted
by the site worker, everything after by the on-target agent.

`bmc_reconcile` and `console_probe` are conditional. They are
skipped when the agent is invoked without `--machine` and `--op` (or
`--machine=""`); the TUI treats a missing phase in the spine as
skipped.

The phases that can be written are scattered across the code base
because different subsystems emit different transitions. See [Phase
write sites](#phase-write-sites) below.

## State transition write sites

The op state machine's *decisions* — which transitions are legal, the
claim preconditions and error taxonomy, the auto-lock policy, and the
FailOperation branching — live in `internal/opstate`. The *SQL* that
reads and writes op / machine rows, plus the row locks, CAS UPDATEs, and
transaction boundaries that make each transition atomic, stays in
`internal/store/operations.go`. Store methods call opstate inside their
transactions; opstate touches no database.

Every write to `machine.state` goes through one helper.

`internal/store/operations.go` — `setMachineState(tx, machineID,
target)` applies the column set that `opstate.StateColumns(target)`
decides:

- Target `MachineStateRunning` also sets `locked = true` (auto-lock).
- Target `MachineStateFailed` also sets `locked = false` (auto-unlock;
  the `opstate.StateColumns` comment acknowledges this clears a manual
  quarantine lock).
- Any other target: state only, lock untouched.

That helper has five call sites. `PATCH /machines/:id` is not one: it
rejects a body containing `state` and writes only the `desired_os`
column, so it cannot revert a transition that commits while the request
is in flight.

| # | Function | File and line | Trigger | Preconditions | Effect on `machine.state` |
|---|---|---|---|---|---|
| 1 | `CreateMachine` | `internal/store/store.go:60` | new machine row (from `mirror.OnDeviceCreated`) | `NetBoxSerial != ""` | initial `state = new` if empty (line 67) |
| 2 | `ClaimOperation` | `internal/store/store.go:369` | worker WS RPC `claim_op` | op status is `pending` (CAS at line 430); machine not locked; state is in `claimTransition` allowed set | commission -> `commissioning`; deploy -> `deploying`; power ops leave state untouched |
| 3 | `CompleteOperationWithState` | `internal/store/store.go:576` | hub receives `op_update{status=completed}`, or a `phone_home` observation naming the deploy op | op not already terminal | sets target state; deploy passes `MachineStateRunning`; empty target means no state change (power ops, per `internal/hubhandlers/hubhandlers.go:60`) |
| 4 | `CommitCommissionResult` | `internal/store/commission_result.go` | commission observation handler | exact active commission op ID belongs to the reported machine | atomically stores inventory, completes that op, and moves its machine to `commissioned` |
| 5 | `FailOperation` | `internal/store/store.go:772` | worker `op_update{status=failed}`, `AbortMachine`, or one of the three timeout sweeps | op not already terminal | per-rule branching, see below |

### `claimTransition` allowed sources

`opstate.ClaimTransition` (`internal/opstate/opstate.go`) documents the
claim allow-lists; `opstate.CheckClaimPreconditions` enforces them plus
the lock gate at claim time:

- `OpCommission` targets `MachineStateCommissioning`; allowed
  sources are `{new, commissioned, failed, running}`.
- `OpDeploy` targets `MachineStateDeploying`; allowed sources are
  `{commissioned, failed, running}`.
- Power ops and `bmc_reconcile` return `("", nil)` from
  `opstate.ClaimTransition` and do not advance state (no lock check
  either: `bmc_reconcile` targets in-service hosts).

The API-level allow-lists mirror this and reject 400 upfront:

- Deploy (`internal/api/server.go:866`): `{Commissioned, Failed,
  Running}`.
- Commission (`server.go:942`): `{New, Commissioned, Failed,
  Running}`.

### `FailOperation` branching

`opstate.FailStateOutcome` (`internal/opstate/opstate.go`) decides the
four rules; `Store.failOperationGuarded`
(`internal/store/operations.go`) applies the returned outcome inside the
fail transaction:

1. **Op was still `pending`** (never claimed, so never advanced
   machine state) — machine state is left alone. Test:
   `TestFailOperation_PendingDoesNotTouchState` (`operations_test.go`).
2. **Op is a power op or an out-of-band BMC op (`set_boot_device`,
   `bmc_reconcile`)** — machine state (and the lock) is left alone
   (`opstate.LeavesStateOnFail`). Tests:
   `TestFailOperation_PowerOpDoesNotTouchState`,
   `TestFailOperation_SetBootDeviceDoesNotTouchState`,
   `TestFailOperation_BMCReconcileDoesNotTouchState`
   (`operations_test.go`).
3. **Op is `commission` and `machine.CommissionData` is empty**
   (initial commission failure) — machine reverts to `new`. Tests:
   `TestFailOperation_InitialCommissionRevertsToNew`,
   `TestFailOperation_RecommissionGoesToFailed` (`operations_test.go`).
4. **Otherwise** — machine goes to `failed` (default branch). Test:
   `TestFailOperation_DeployFailureGoesToFailed` (`operations_test.go`).
   The legal/illegal transition matrix itself is unit-tested in
   `internal/opstate/opstate_test.go`.

Because rule 4 flows through `setMachineState`, it also clears the
lock (`TestFailOperation_AutoUnlocksOnFailed`,
`operations_test.go:1015`).

### Phase write sites

`Operation.Phase` is written in only one place on global
(`Store.SetOperationPhase`, `internal/store/store.go:703`), called
by `hubhandlers.OpUpdate.HandleOpUpdate`
(`hubhandlers.go:132`). `SetOperationPhase` returns a
`PhaseTransition` (`store.go:690`) that the hub uses to emit exactly
one canonical log envelope per real change. It does not write a
terminal (completed/failed) op: the worker can still report a phase
after the op ended (a report in flight when the operator aborts, or
executor progress after `op_removed`), and that report changes
nothing and publishes nothing. The op keeps the phase it ended in, and
its version stays at the tombstone workers hold for it.
It reads the op row `FOR
UPDATE`, so two overlapping deliveries of the same report serialize:
the second sees the phase the first wrote and returns `Changed=false`.

The site worker maintains its own view of the phase in
`Cache.UpdateOpPhase` (`internal/worker/cache.go:1321`). The write
sites for that helper are:

- `Cache.HandleProgressReport` (`cache.go:1509`, `1525`, `1504`) —
  the intake for `POST /api/v1/report` from the on-target agent.
- `Executor.runLifecycleOp` (`internal/worker/executor.go:374`) —
  emits `power_cycle` then `pxe_booting` before waking the target.
- `dhcpCfg.OnACK` (`cmd/slagworksd/site.go:646`) — emits
  `dhcp_ack` (firmware DHCP) or `ipxe_dhcp` (iPXE second DHCP).
- `httpboot.Handler.ServeHTTP` (`internal/httpboot/handler.go:190`)
  — emits `ipxe_loaded` when the iPXE second stage fetches the boot
  script.
- Agent emits (via `POST /api/v1/report`, `reporter.go:31`):
  - Deploy: `agent_starting`, `partitioning`, `rsyncing_os`,
    `installing_bootloader`, `finalizing`, and conditionally
    `bmc_reconcile` (`cmd/slagworks-agent/main.go:273`, `312`,
    `349`, `355`, `390`, `443`).
  - Commission: `agent_starting`, `collecting_inventory`,
    `reporting`, `console_probe`
    (`cmd/slagworks-agent/commission.go:415-436`).

## Claim semantics

There are two things called "claim" in the code base.

### Site-config claim over NetBox devices

`internal/snapshot/claim.go:45` (`NewClaimResolver`) and
`ClaimingConfigs` (line 58) decide which site configs own which
NetBox device. This is location-subtree membership, not an
operation claim. It is used by the NetBox webhook and by the
reconciler to decide which site's snapshot needs invalidation on a
device event.

### Operation claim (the state-machine claim)

`internal/store/store.go:369` (`ClaimOperation`) is the atomic verb
that transitions a pending op to claimed and simultaneously
advances the machine to the corresponding running state. It is
invoked by the site worker's WebSocket RPC (`ws/messages.go:197`
`ClaimOpMsg`; hub side `ws/hub.go:962` `handleClaimOp`; client side
`internal/worker/wsconn.go:200` `ClaimOp`).

The store implementation:

- Locks the operation row `FOR UPDATE` (`store.go:377`), then locks
  the machine row `FOR UPDATE` (`store.go:396`). Lock order is
  documented at `store.go:391`: operation, then machine.
- Preconditions in order: op status is `pending`; machine state is
  in `restingStates` per `claimTransition` for the op type;
  machine is not `Locked`. Any failure returns an error the worker
  logs but does not treat as fatal.
- Race close via CAS at `store.go:430`: the transition UPDATE
  requires `WHERE status = 'pending'`. If it hits zero rows, the
  worker gets `(nil, nil)` from `ClaimOperation` — the caller
  interprets that as `errClaimLost` in
  `internal/worker/executor.go:383` and keeps its local claimed
  mark (which prevents a re-drive on the next tick).
- On success: flips op to `OpClaimed`, sets `claimed_by` and
  `claimed_at`, advances the machine via `setMachineState`, and
  emits a `claim event` log envelope (`emitClaimEvent`,
  `store.go:464`).

Under HA (two site workers on the same site config, one active and
one standby), both are connected to the hub but only the VIP holder
runs the executor and dispatches ops. The claim CAS is still the
authoritative guard against double-claim.

## Boot-action decision table

`internal/model/provider.go:7` declares the enum:

```text
BootDeploy
BootCommission
BootLocal
```

The decision is entirely site-side; the dispatcher never computes
it. The authoritative implementation is `Cache.BootActionForMAC`
(`internal/worker/cache.go:249`) and the doc comment at
`cache.go:223` reproduces the table.

| Machine state | Active operation | Operation phase | Boot action |
|---|---|---|---|
| Unknown MAC (not in the snapshot) | — | — | `BootLocal` (`cache.go:254`) |
| Any state with `!PXEEligible` | — | — | `BootLocal` (`cache.go:257`) |
| Any state | deploy op | `awaiting_phone_home` | `BootLocal` (`opState.bootActionLocked`) |
| `commissioning` | commission op already claimed | — | `BootCommission` (`cache.go:278`) |
| `deploying` | deploy op | anything except `awaiting_phone_home` | `BootDeploy` (`cache.go:285`) |
| `new` | commission op | — | `BootCommission` |
| `new` | — | — | `BootLocal` |
| `failed` | pending commission op | — | `BootCommission` (`cache.go:300`) |
| `failed` | pending deploy op | anything except `awaiting_phone_home` | `BootDeploy` (`cache.go:303`) |
| `failed` | no operation | — | `BootLocal` (`cache.go:307`) |
| `commissioned` | pending deploy op | anything except `awaiting_phone_home` | `BootDeploy` (`cache.go:309`) |
| `commissioned` | no deploy op | — | `BootLocal` (`cache.go:312`) |
| `running` | any | — | `BootLocal` (`cache.go:313`) |
| default | — | — | `BootLocal` (`cache.go:317`) |

Notes:

- The active-op selection loop (`cache.go:262`) skips terminal ops
  and power ops when picking "the active op for this machine". A
  machine can have a queued `power_cycle` alongside a pending
  deploy; the deploy still wins the boot decision.
- Commission and deploy both require an exact active operation. The boot
  script derives `machine_id`, `op_id`, and `trace_id` from that single
  binding; inventory can validate identity but cannot select another machine.
- The `awaiting_phone_home` row is the reason the post-deploy reboot
  does not loop-wipe the disk; see [The `awaiting_phone_home`
  latch](#the-awaiting_phone_home-latch). It is checked before the
  machine state because the pending-op rows let an install finish
  while the machine is still `commissioned` or `failed`.

### How the boot action reaches the target

`httpboot.Handler.ServeHTTP` (`internal/httpboot/handler.go:139`):

1. Parse the MAC from the query.
2. `action := h.provider.BootActionForMAC(mac)`.
3. Emit `ipxe_loaded` phase on the booting op.
4. Dispatch to `bootLocalScript`, `commissionBootScript`, or
   `deployBootScript`, each of which generates iPXE text that stamps
   the correct `slagworks.*` kernel command line arguments.

## Operation lifecycle

Each op flows through the same skeleton; specifics differ by type.

### Create

Operator calls `POST /api/v1/machines/:id/deploy` (or `commission`,
or `power`, or `boot`). Server-side handlers live in
`internal/api/server.go` (`deployMachine`, line 755;
`commissionMachine`, `powerMachine`, `bootMachine`). Each handler:

1. Checks the machine state is in the allowed source set for the op
   type (deploy at `server.go:866`; commission at `server.go:942`).
2. Runs the read-only snapshot preflight
   (`preflightMachine`, `internal/api/preflight.go:51`) and refuses
   with 412 if any gap is reported.
3. Resolves the runtime blob for the op via
   `resolveBMCRuntime` (`bmc_runtime.go:79`),
   `resolvePDURuntime` (`pdu_runtime.go:89`), and
   `resolveTailscaleRuntime` (`tailscale_runtime.go:68`), and
   stamps them into the op params JSON. This is what freezes the
   BMC / PDU / Tailscale identity at op-create time.
4. `store.CreateOperation` — mints the `TraceID`
   (`store.go:314`), inserts the row with `status = pending`, and
   returns.

### Preflight

`snapshot.PreflightMachine` (`internal/snapshot/preflight.go:65`)
returns a list of human-readable gap strings. Checks in order:

- Machine appears in the snapshot at all.
- `primary_ip4` is present on the boot NIC (via
  `MachineSnapshot.Interfaces`).
- Boot NIC has a MAC.
- Network script matches (all op types that involve booting).
- Storage script matches (deploy only).
- Image row exists for the requested image (deploy and commission).
- Kernel params: deploy hard-fails on an empty value (the sentinel
  string `none` is treated as "explicitly empty"); commission is
  more permissive.
- AWS admin secret present (all BMC-touching ops).
- AWS monitor secret present (deploy only, because deploy is what
  rotates monitor credentials).

The same read is available to operators at
`GET /machines/:id/preflight`.

### Op transport: the op-event stream (Tier 2 Phase 2)

Operations do NOT ride the identity snapshot. They flow on a dedicated
op-event stream (`op_delta` / `op_resync` / `op_removed`, defined in
`internal/ws/messages.go`), fed to the worker's op layer
(`internal/worker/opstate_cache.go`) independently of `ApplySnapshot`:

- **op-create** (`api/server.go` `createOperation`) publishes an
  `op_delta` for the pending row — no identity rebuild.
- **claim** (`ws/hub.go` `handleClaimOp`) publishes an `op_delta`
  carrying the claimed row + its transient state (deploying/commissioning).
- **phase transitions** (`hubhandlers`) publish an `op_delta`.
- **terminal** (completion / abort / sweep fail) publishes an
  `op_removed`; if it wrote a DURABLE `machines.state`
  (running/commissioned/failed/new) it ALSO fires one identity rebuild so
  the baseline catches up. Pending-expiry writes no state and fires none.
- **(re)connect** sends an unconditional `op_resync` (the active set
  scoped to the conn's site + the transient overlay each claimed/running
  op implies).
- **snapshot push** (`ws/hub.go` `Push`) follows each fan-out snapshot
  with every active op of the site, re-sent as an `op_delta` scoped to the
  new membership and carrying the transient state a claimed op implies.
  The mirror commits a machine row and only schedules the identity
  rebuild, so an op created on a new machine before that rebuild is
  pushed has its `op_delta` dropped by the membership filter below; the
  re-send is what delivers it. The worker drops the versions it already
  holds, so op progress it recorded locally is untouched (an `op_resync`
  here would full-replace it).

Confidentiality scoping: `Operation.Params` carries per-site credentials
(BMC/PDU passwords, tailnet auth keys), so `op_delta` and `op_resync` are
slug-scoped — the hub sends them only to workers whose cached per-slug
snapshot contains the op's machine (`Hub.machineInSlug`), preserving the
pre-cutover baseline where ops rode the per-slug snapshot. `op_removed`
fans fleet-wide, unfiltered: it carries no params (no secrets) and must
survive membership loss (a machine deleted from NetBox is in no snapshot,
but its drift-failed op must still be dropped on the worker driving it).

### Credentials in operations

The runtime blocks global stamps into `Operation.Params` at op-create
carry credentials: `bmc_runtime.password`, each
`pdu_runtime.outlets[].password`, `amt_runtime.password`,
`monitor_runtime.password`, and `tailscale_runtime.auth_key`. Usernames,
hosts, serial gates, outlet lists, flags and `extra_config` are not
credentials. Every credential field carries `secret:"true"` on its struct
in `internal/model/op_params.go`; that tag is the single definition both
mechanisms below derive from (`model.SecretParamPaths`), and
`TestOpParamsCredentialFieldsAreMarked` fails on a credential-looking
field (password/secret/key/token) anywhere under `OpParams` that lacks it.

- **Workers get the real values.** The op stream (`op_delta`, `op_resync`,
  the claim reply) carries params unredacted, scoped by slug as above.
- **Operators never do.** The API server's echo JSON serializer
  (`internal/api/op_redaction.go`) is the one choke point for operator
  responses: every `Operation` it encodes (op-create responses from
  deploy/commission/power/boot/bmc-reconcile/PDU-outlet power,
  `GET /operations`, `GET /operations/:id`) has each non-empty
  credential replaced by `[redacted]` (`model.RedactParams`); every
  other key keeps its value, so the transport an op used stays visible.
  A response that holds an `Operation` in a shape the serializer does
  not handle (a wrapper struct, a map, behind an `any`) fails the request
  instead of leaking.
- **Terminal ops drop them.** `Store.ScrubTerminalOperationCredentials`
  removes the credential keys from the params of every completed or
  failed op in one UPDATE that writes only rows still holding one. Global
  runs it before serving and at the end of every sweep tick (see the next
  section), so a terminal op keeps credentials in the `operations` table
  for at most one minute while global is up, or until global's next start
  if it is down. Nothing reads credentials from a terminal op: a retry is
  a new op that resolves credentials afresh.

Residual exposure: an active op holds its credentials in the
`operations` table (and in each worker's op layer and `worker.db`) for as
long as it runs; a pending op can sit for `PendingOpTimeout` (24h).

The worker applies deltas last-writer-wins by `operations.version`, keeps a
bounded tombstone of removed op IDs so a late lower-version delta can't
resurrect a zombie op, and — the re-wipe-window fix — clears its op-driven
machine-state overlay on an identity apply ONLY where the applied baseline
equals the overlay value. A rehydrated-from-LocalDB op set is UNTRUSTED
until the first `op_resync`: it may force `BootLocal` but never actuate.
The worker persists its op layer (`worker.db`, kv key `op_state`) after
every stream apply and after the two local writes that protect an
installed host, a deploy op reaching `awaiting_phone_home` and a
phone-home; see [the latch](#the-awaiting_phone_home-latch). Fail-closed rule: an effective transient state (deploying/commissioning)
with NO matching active op in the op layer serves `BootLocal`, never
`BootDeploy`/`BootCommission`. Protocol skew is loud: `HelloMsg.Proto` and
every hello reply carry `ws.OpStreamProto`; either side refuses an
absent/lower peer.

### Claim and drive

Once an `op_delta`/`op_resync` carrying the new pending op reaches the site
worker, the executor's pending-op wake fires (or its 5s backstop tick),
`Executor.processOps` (`internal/worker/executor.go:99`) sees it and calls
`executeOp` (line 114):

1. `Cache.MarkOpClaimed` locally in the cache first (line 120).
   This prevents the next 5-second tick re-driving the same op.
2. WS RPC `Claimer.ClaimOp(opID)`. On lost race, keep the local
   claimed mark and exit; the winning worker will drive.
3. On transport error, roll back to `OpPending` in the cache
   (line 140).
4. Dispatch to `runPowerOp` (line 151) or `runLifecycleOp` (line 288).

`runLifecycleOp` does:

- `verifyChassisSerial` gate (guards against acting on the wrong
  physical host).
- `BootManager.SetBootOverride(pxe)` on Supermicro, `SetBootDevice`
  via IPMI on other vendors, so the next boot lands on PXE.
- Emit phase `power_cycle`.
- Call the power controller's `PowerCycle` (or `PowerOn` when off).
- Emit phase `pxe_booting`.
- Mark the op `OpRunning` in the cache.
- Return. The executor does not block on the deploy; the agent
  drives phases from here on.

`runPowerOp` is straightforward: build the controller, run
`PowerOn` / `PowerOff` / `PowerCycle` / `SetBootDevice`, mark the op
completed. `runSetBootDevice` (`executor.go:259`) routes to the
`power.BootManager` capability which today is only implemented for
Supermicro.

### Complete

Path depends on the op type.

- **Deploy** completes via the phone-home service on the deployed
  host, not via the agent. The agent writes a systemd unit
  (`generatePhoneHomeUnit`, `main.go:1484`) that POSTs
  `/api/v1/phone-home?mac=...`; the httpboot handler at
  `handler.go:941` calls `provider.RecordPhoneHome`, which is
  `Cache.RecordPhoneHome` (`internal/worker/cache.go:404`).
  That sets the machine's local state to `running`, emits a
  `phone_home` observation carrying the deploy op's ID, and calls
  `Cache.RecordOpUpdate(deployOpID, "completed", nil)`. Both reach
  `Store.CompleteOperationWithState(opID, Running, ...)`, which
  auto-locks the machine: the op update through
  `hubhandlers.OpUpdate.HandleOpUpdate`, the observation through
  the observation handler (`internal/commission/handler.go`,
  `handlePhoneHome`). Whichever lands first completes the op; the
  other is a no-op. The observation is the one that must arrive:
  the installed OS phones home only until its first 2xx, op updates
  live only in the worker's memory and are never acknowledged, and
  the observation stays in the worker's SQLite outbox until global
  acks it. Without it, a worker restart, a dropped connection or a
  failed op-update handler between the 2xx and delivery would leave
  the op at `awaiting_phone_home` until `PhoneHomeTimeout` fails a
  healthy host. A `phone_home` with no op ID (the worker held no
  active deploy op) changes nothing.
- **Commission** completes via the commission report path, not the
  op-update path. The observation handler calls
  `Store.CommitCommissionResult(opID, machineID, data)`, which revalidates
  and completes that exact operation while storing inventory. The
  `completionState` branch for `OpCommission` in the hub
  (`hubhandlers.go:50`) is effectively unused in production and
  the comment there acknowledges it exists "for completeness
  against ad-hoc completions."
- **Power ops** complete via `CompleteOperationWithState` with an
  empty target state (see rule at `hubhandlers.go:60`), so
  machine state is untouched.
- **bmc_reconcile** completes (or fails) via the
  `bmc_reconcile_report` observation handler
  (`internal/commission/bmc_reconcile.go`), after a rotated monitor
  password is stored in `pop/<pop>/bmc-creds-monitor` (a re-asserted
  stored password writes nothing); empty target state.

### Fail

`Store.FailOperation` (`store.go:772`) is the funnel. Four callers:

- Worker `op_update{status=failed}` — the agent's `fatal` path
  (`runControl.fatal`, `cmd/slagworks-agent/control.go`) POSTs
  `ProgressReport{Failed: true, Phase: X, Error: msg}`; the site
  worker sets phase to `failed` (`cache.go:1504`) and queues an
  op-update with status failed. Failure surfaces immediately with
  the exact phase, not after a 30-minute timeout.
- `AbortMachine` (`store.go:952`).
- The three timeout sweepers (see next section).

The four `FailOperation` rules described above apply uniformly no
matter which caller invoked it.

## Timeouts and phase-based expiration

`internal/api/server.go:432` starts a 1-minute ticker that runs
three expiration sweeps in order. All three converge on
`FailOperation`.

| Sweep | Default | Config field | Function | Predicate |
|---|---|---|---|---|
| Pending ops | 24 hours | `PendingOpTimeout` (`server.go:71`) | `Store.ExpirePendingOperations` (`store.go:903`) | `status = 'pending' AND created_at < cutoff` |
| Claimed / running ops | 30 minutes | `OpTimeout` (`server.go:68`) | `Store.ExpireStuckOperations` (`store.go:874`) | `status IN ('claimed','running') AND claimed_at < cutoff AND phase != 'awaiting_phone_home'` |
| Phone-home | 30 minutes | `PhoneHomeTimeout` (`server.go:75`) | `Store.ExpirePhoneHomeOperations` (`store.go:933`) | `status IN ('claimed','running') AND phase = 'awaiting_phone_home' AND phase_entered_at < cutoff` |

Each sweep reads its candidates without a lock and then fails them one
at a time, so it passes its predicate to `failOperationGuarded` as the
guard: the predicate is re-checked under the op row lock, and an op
that stopped matching in between is left alone. A worker claim that
lands after the pending sweep's read keeps its op claimed, and a deploy
that enters `awaiting_phone_home` after the stuck sweep's read is
spared. `ExpireOrphanedClaims`, which fails claims whose worker died
before recording a phase, guards the same way.

The `awaiting_phone_home` phase is explicitly excluded from
`ExpireStuckOperations` (`store.go:867`) and covered separately by
`ExpirePhoneHomeOperations`. The deadline anchors are chosen so each
sweep has a stable clock:

- Pending sweep anchors on `created_at`.
- Stuck sweep anchors on `claimed_at`.
- Phone-home sweep anchors on `phase_entered_at`, which is only
  bumped by `SetOperationPhase` when the phase string actually
  changes (`store.go:725`, test
  `TestSetOperationPhase_BumpsPhaseEnteredAt` at
  `operations_test.go:882`). This is what makes the 30-minute
  phone-home budget a real budget rather than an infinitely
  renewable one.

There is no per-phase timeout (no separate budget for `partitioning`
versus `rsyncing_os`). The overall 30-minute `OpTimeout` covers the
whole claimed/running window from `claimed_at` to `awaiting_phone_home`;
`PhoneHomeTimeout` covers the window after.

Phase failure is not timeout-driven. When the agent's `fatal` runs
(`runControl.fatal`, `cmd/slagworks-agent/control.go`), it POSTs a
failure report; the site worker marks the op failed immediately. The
timeout sweeps exist only for the "agent died silently" case. An agent
that is still alive when a sweep fails its op stops at its next report,
the same way it does after an abort (see
[What an abort does on the host](#what-an-abort-does-on-the-host)).

The worker's local failure holds until global's `op_removed` for the op.
Global applies the failure as separate writes, and each phase write
publishes an `op_delta` whose row is still `claimed` before the failure
publishes `op_removed`; a reconnect's `op_resync` can also still show the
op claimed when the best-effort `op_update` never reached global. The
worker keeps such an op failed (`keepLocalFailure` in
`internal/worker/opstate_cache.go`): the host is not booted into the agent
again and reports for the op are refused as terminal. When the failure
never reached global, the op stays active there until a timeout sweep
fails it.

Each sweep fails its ops one transaction at a time and keeps going past
a per-op error, returning both the ops it failed and the joined error.
The tick (`Server.expireOperations`) publishes `op_removed` for every op
a sweep failed, and fires the identity rebuild, even when that sweep
also returned an error: those ops are terminal and no later tick selects
them again. The ops behind the error stay active and the next tick
retries them.

After the sweeps, each tick runs
`Store.ScrubTerminalOperationCredentials` (also run once before the API
starts serving); see [Credentials in operations](#credentials-in-operations).

## Abort

Route: `POST /api/v1/machines/:id/abort` (registered at
`internal/api/server.go:369`, handler `abortMachine` at
`server.go:1003`).

CLI: `slagctl machine abort <ident>`
(`cmd/slagctl/cmd/machine.go:681`).

Handler calls `store.AbortMachine(id)` (`server.go:1008`).

`Store.AbortMachine` (`store.go:952`):

1. Fetch the machine.
2. List active ops (status in `{pending, claimed, running}`).
3. For each, call `FailOperation(opID, {"error":"aborted by
   operator"})`.
4. Return the fresh machine row.

The handler publishes `op_removed` for each failed op (and fires the
identity rebuild for a durable state write) before it checks the error,
because `AbortMachine` also returns the ops it failed when a later step
errors, and a retried abort no longer finds them.

Effect on machine state follows the four `FailOperation` rules
uniformly:

- Pending op only: machine state untouched
  (`TestAbortMachine_RoutesViaFailOperation`,
  `operations_test.go:696`).
- Active claimed / running deploy: machine goes to `failed`,
  auto-unlocks.
- Active initial commission (`CommissionData` empty): machine
  reverts to `new`.
- Active recommission: machine goes to `failed`.
- Active power op, `set_boot_device`, or `bmc_reconcile`: machine state
  untouched.

There is no state precondition on `AbortMachine`. It is a no-op
that returns the current row if no ops are active.

### What an abort does on the host

Abort writes rows in global and sends nothing to the host: it does not
power the host off (the operator follows with `machine power off` for
that) and the worker takes no BMC action when an op is removed. An
agent that is still running finds out from its own reports.

Once the worker has applied the `op_removed`, `opState.validateReportBinding`
(`internal/worker/opstate_cache.go`) refuses every report for that op with
an error matching `model.ErrOperationNotActive`, and `POST /api/v1/report`
answers it with 410 Gone (`reportRejectionStatus`,
`internal/httpboot/handler.go`). The same answer covers every other way
an op ends: a timeout sweep, a failure the agent or the boot hook
reported, completion. The worker gives it only when it knows the op is
over: it holds the op with a terminal status, it holds the op's
tombstone, or the op is absent from a set that has been resynced since
the worker started. Every other rejection stays 409 Conflict, including
"operation state is not synchronized" from a worker that restarted
mid-deploy, where the op may be healthy.

The agent acts on 410 on every report path (`agentReporter.send`,
`cmd/slagworks-agent/reporter.go`): phase reports, log flushes, failure
reports and the final `deploy_complete` / `commission_report`. Its flush
loop also sends an empty report every 5 seconds as a liveness check, so
an agent in the middle of a long phase learns of an abort within that
interval rather than at its next phase. On the first 410 the agent
halts (`runControl.halt`, `cmd/slagworks-agent/control.go`): it kills
the processes it has started together with their children
(`execkit.Group.Stop`: the commands in flight, a config script, the
tailscale daemon and client of an enrollment in progress), starts
nothing further, and exits with status 3.
It does not register a boot entry it had not yet registered, does not
reconcile the BMC, does not report `deploy_complete` and does not
reboot. The boot hook treats the nonzero exit like any agent failure
and holds the host in the recovery environment (recovery sshd, or a
console shell when the image carries no keys) until an operator or the
next operation's power cycle takes it from there. Disks are left as the
interrupted step left them, so after an abort that landed once
partitioning had begun the next deploy of that machine needs
`--destructive`.

A 410 that arrives after the worker accepted the agent's final report
is the expected result of that report and is ignored: global completes
a commission as soon as it has the inventory, while the agent still has
the console probe and its reboot ahead of it.

Tests: `TestHandleProgressReport_OperationOverIsFinal` and
`TestHandleProgressReport_UnsynchronizedIsNotFinal`
(`internal/worker/report_final_test.go`),
`TestReportEndpoint_OperationOverIsGone` (`internal/httpboot/handler_test.go`),
`TestRunDeploy_StopsWhenOperationIsNoLongerActive`
(`cmd/slagworks-agent/control_test.go`) and the `TestReporter_*` tests
(`cmd/slagworks-agent/reporter_test.go`).

Two limits follow from the agent finding out by asking. An agent that
cannot reach the site worker never hears the 410; it runs on until its
final report fails, and then drops to recovery through the fatal path.
And what is not a tracked process can still finish in the moment
between the 410 and the exit: a file being written into the target, or
one of the short probes the agent runs directly (`modprobe`, a single
in-band `ipmitool` call of the BMC reconcile).

## The `awaiting_phone_home` latch

The deploy pipeline has a delicate moment: the agent has finished
writing the OS, has emitted `deploy_complete`, and is about to
reboot the target. On the first boot after that reboot the target's
firmware will do another PXE (because the boot order still has PXE
first — that is a MAAS-era invariant we have preserved). Without a
latch, that PXE would land back in `Cache.BootActionForMAC` while
the machine was still `deploying`, get `BootDeploy` again, and the
agent would wipe the disk it just wrote.

The fix is a clobber guard on the op's phase.

`Cache.HandleProgressReport` (`internal/worker/cache.go`)
recognizes the `deploy_complete` event and calls
`UpdateOpPhase(opID, "awaiting_phone_home")`.

`Cache.UpdateOpPhase` then refuses every phase write that would move
the op off `awaiting_phone_home`, except `failed`, which
`HandleProgressReport` writes just before `MarkOpFailed` on an agent
failure report. The guard lives in `opState.updatePhase`
(`internal/worker/opstate_cache.go`). The writers it stops are the
post-install firmware PXE (`pxe_booting`, `dhcp_ack`, `ipxe_dhcp`,
`ipxe_loaded`), the executor's `power_cycle` and `pxe_booting` when it
claims an op whose install finished while the op was still pending,
and an agent phase report the worker processes after
`deploy_complete`. Completion never needs a phase write: it goes
through `RecordPhoneHome`. A dropped write is not queued for global
either, so the op stays on the phone-home timeout. Tests:
`TestUpdateOpPhase_AwaitingPhoneHomeNotClobbered` and
`TestUpdateOpPhase_AwaitingPhoneHomeOnlyLeftByFailure`
(`cache_test.go`). `keepTerminalWait` holds the phase the same way
against a pending, claimed or running deploy row in an op_delta or
op_resync from global (`TestStaleEchoKeepsAwaitingPhoneHome`,
`TestStalePendingRowKeepsAwaitingPhoneHome`, `opstream_test.go`).

`Cache.BootActionForMAC` reads the phase and returns `BootLocal`
whenever the machine's active deploy op is at `awaiting_phone_home`,
whatever the machine state. The state is usually `deploying`, but a
host that PXEs on the pending-op clause while the executor's claim
keeps failing installs with the machine still `commissioned` or
`failed`: only the claim's op_delta moves it to `deploying`, and
`HandleProgressReport` accepts reports for a pending op
(`TestBootAction_AwaitingPhoneHomeOnUnclaimedDeploy`, `cache_test.go`).
The target boots off disk, its phone-home
service POSTs `/api/v1/phone-home`, `RecordPhoneHome` fires the
completion path described earlier, and the op transitions to
`completed` with `machine.state = running`.

The latch has to survive a worker restart. The phase and the completion
reach global only as op_updates, which the outbox keeps in memory, so
global can still hold the op claimed at a pre-install phase when the
install finishes during a WAN outage. `Cache.UpdateOpPhase` therefore
writes the op layer to LocalDB when it sets `awaiting_phone_home`, and
`Cache.RecordPhoneHome` writes it with the running overlay and the
`phoneHomed` pin. After a restart the first `op_resync` (built from
global's stale row) then finds the latched phase to keep
(`keepTerminalWait`) and the pin to apply, and the host's next PXE still
boots its disk. Test: `TestAwaitingPhoneHomeSurvivesRestart`
(`opstream_test.go`).

Timeout: `PhoneHomeTimeout` (30 minutes by default) anchored on
`phase_entered_at`.

## The upgrade latch on the site worker

Separate from the phone-home latch. The site worker binary can
auto-upgrade itself when the hub announces a different version.

`WSConn` (`internal/worker/wsconn.go`) tracks `upgrading atomic.Bool`.
On a `snapshot` or `no_change` message the worker calls
`checkVersion` with the hub-declared build (SHA and package version),
and if the SHA differs from `main.version` and
`SLAGWORKS_AUTO_UPGRADE=1`, it kicks `runUpgrade`. The upgrade runs
`sudo -n /usr/bin/slagworks-upgrade <git-sha> <package-version>`
(`upgradeCommand`), which apt-installs that package version from the
fleet apt repo (see `docs/dev/build-and-release.md`). A hub-declared
build with no package version (global not built by
`scripts/build-deb.sh`) never takes the latch: the worker logs that it
cannot auto-upgrade to an unpackaged build and stays put, and the
heartbeat retry skips it.

`runUpgrade` defers a cooldown release: `time.Sleep(cooldown);
w.upgrading.Store(false)`, where `upgradeRetryCooldown = 2 *
time.Minute` (`wsconn.go:523`). The point of the cooldown is that a
failed upgrade — apt failure, network hiccup, service restart loop —
must not strand the worker on the old binary while also blocking
future upgrade attempts. Test:
`TestCheckVersion_ResetsLatchAfterFailedUpgrade`
(`wsconn_test.go:383`).

## The `--destructive` and `--wipe-all-disks` flags

Slagctl surface (`cmd/slagctl/cmd/machine.go:186`, `209`, `245`):

- `--destructive` (line 257) — bypass the boot-disk safety check
  the agent runs.
- `--wipe-all-disks` (line 258) — implies destructive; signature-wipe
  every disk not in the storage layout after partitioning.
- `--recovery` (line 186) — deploy into a recovery-mode payload.
- `--pxe-enable` (line 186) — auto-activates `PXEEligible` before
  submitting the op.

The flags ride the deploy request body as JSON booleans
(`machine.go:214`). Server side, `deployMachine`
(`internal/api/server.go:755`) marshals them into `op.Params` in a
struct at `server.go:756`. The `WipeAllDisks && !Destructive`
combination returns 400 (`server.go:798`).

The httpboot deploy script decodes `op.Params` and stamps kernel
command line arguments (`internal/httpboot/handler.go:381`):

- `slagworks.destructive`
- `slagworks.wipe_all_disks`
- `slagworks.recovery`
- `slagworks.tailscale_authkey=<key>` (when a Tailscale runtime blob
  is attached)

The agent parses the boolean flags (`cmd/slagworks-agent/main.go:136`)
and consumes them:

- Skip the boot-disk safety check when `--destructive`
  (`main.go:303`).
- Wipe stale RAID / filesystem state on the target disks in the
  layout (`main.go:324`, `wipeStaleState`).
- Wipe bystander disks when `--wipe-all-disks`, gated by
  `--destructive` (`main.go:332`).
- Before wiping anything, refuse the deploy when a disk it leaves
  unwritten carries an md member superblock with an array UUID a raid
  device pins (`array_uuid`), typically the disk a redeploy moved the
  array off (`storageapply.CheckStaleArrayMembers`). At boot such a
  stale member can claim the array ahead of its real members. Under
  `--wipe-all-disks` bystander disks are not examined, because the
  bystander wipe zeroes them; retained and preserve-member disks
  always are. The agent refuses rather than zeroing the member
  itself: `--destructive` covers only the disks the layout targets,
  and only `--wipe-all-disks` authorizes writes outside the layout.

The legacy `slagworks.force` kernel argument was renamed to
`slagworks.destructive` and the tests explicitly assert the old
spelling is gone (`internal/httpboot/handler_test.go:611`, `729`).

## The no-auto-commission rule

Slagworks enforces "only operator-initiated ops produce non-`BootLocal`
actions."

**Boot-action defaults to `BootLocal` unless a pending or claimed
operation exists.** The full table is in
[Boot-action decision table](#boot-action-decision-table). Every row that
returns `BootDeploy` or `BootCommission` is backed by an explicit lifecycle
operation, including the first commission of a `new` machine.

**There is no unknown-MAC auto-registration path.** The old
`HandlerConfig.RegisterAndCommission` hook and the site-config
`auto_commission` knob were deleted; enrollment is NetBox-driven.
An unknown MAC that reaches the boot server gets `BootLocal` (or, on the
promiscuous OOB segment, no bootfile when the negative gate identifies it as
a reserved non-machine BMC, PDU, or unmanaged switch port).

## Open questions and rough edges

- **The commission branch of `hubhandlers.completionState`
  (`hubhandlers.go:50`) is effectively unused.** Commission
  completion runs through `commission.Handler` and
  `Store.CommitCommissionResult`
  in the observation path. The `OpCommission` case in
  `completionState` exists "for completeness against ad-hoc
  completions" (comment line 47) but no code path currently
  produces one.
- **`slagworks.fatal_at` and `slagworks.efivarfs` are test hooks
  that are not gated in production.** `httpboot/handler.go:391`
  and `397` will honor them if they appear in `op.Params`. A
  `SLAGWORKS_TEST=1` gate is deferred and marked in-code.
- **No per-phase timeout.** All timing lives in the three coarse
  sweeps. A partitioning that hangs for 25 minutes will not be
  detected until the overall `OpTimeout`. The agent's fatal path
  is the fast-failure mechanism; the sweep is the safety net.
