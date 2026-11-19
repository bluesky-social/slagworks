# PDU-based power control

Status: live — implemented (internal/power pdu*.go); design record with rationale. Contains deployment-specific validation evidence (real PDU IPs/serials) — sanitize before any OSS publish.
Date: 2026-06-02
("Implement PDU power control via pdu-mqtt" — the "pdu-mqtt" framing is dead; there
is no broker, the PDU speaks Redfish/SSH on the wire and the site worker is
L3-adjacent to it).

## Goal

Today slagworks can power-control a machine only if it has a BMC. The
`power.Controller` interface has two implementations: `BMCController`
(`internal/power/bmc.go`, working, Redfish/IPMI via bmclib) and `PDUController`
(`internal/power/pdu.go`, a stub whose every method returns "not yet implemented").
The site-worker controller factory (`cmd/slagworksd/site.go` `bmcControllerFactory`)
hard-fails when `m.BMCIP == ""`, and the executor (`internal/worker/executor.go`)
bails the same way before it ever reaches the factory.

That leaves an entire class of hardware unmanageable: **whitebox switches with no
BMC**, whose only remote power control is the switched PDU outlets their PSUs plug
into. This is not hypothetical — it motivated the whole effort: `leaf-3.pop1`
(NetBox device 579) was hung, and a dual-feed PDU power-cycle was the only way to
recover it without smart hands. We validated the recovery primitive live on
2026-06-02 (see "What we proved" below).

Concretely, after this work:

- A power op (`OpPowerOn` / `OpPowerOff` / `OpPowerCycle`) on a BMC-less,
  PDU-cabled device actuates the correct outlet(s) on the correct PDU(s) instead
  of failing with "no BMC configured".
- `PowerStatus` returns a real `on`/`off`/`unknown` from the PDU's own
  outlet-state reporting (Redfish).
- Transport selection (BMC vs PDU) is automatic from NetBox topology, but the
  resolved transport and exact outlets are **surfaced to the operator before
  actuation** and an explicit `--via` override exists (§A).
- Switches go through the full slagworks commission/deploy lifecycle (confirmed
  operator intent 2026-06-02), not just bare power control — so PDU power
  integrates with `runLifecycleOp`, not only `runPowerOp` (§Interface-completeness).

The single most important design decision is the **multi-feed atomic cycle**
(§D): a device with redundant PSUs has TWO outlets on TWO PDUs and does not lose
power when one outlet is cut — and we must never strand ourselves by composing a
cycle from discrete off/on.

## What we proved (live, 2026-06-02)

Validated against real pop1 hardware before committing the design:

- **The hardware:** `leaf-3`/`leaf-4.pop1` are whitebox Mellanox SN2700s running
  Ubuntu 24.04 (not Cumulus/SONiC). Each has two PSUs on two PDUs:
  `pdu-0102-left` (NetBox 611, `10.254.240.154`, serial `ZA2451003321`) and
  `pdu-0102-right` (NetBox 612, `10.254.240.155`, serial `ZA2451003335`). Both are
  APC **APDU9965**, NMC firmware **v2.5.3.1**.
- **The join is exact:** NetBox `device.serial` == Redfish `RackPDU/0.SerialNumber`
  == the AWS secret key (`ZA2451003321`). Resolving a device → its outlets → the
  PDU cred is unambiguous, keyed on serial end-to-end.
- **Actuation is SSH-only on this firmware.** Confirmed three ways: the NMC manual
  (`990-6302F-001`) documents Redfish outlet control as discrete `SwitchedState`
  On/Off ONLY — *and only on the standard `/redfish/v1/PowerEquipment/RackPDUs`
  tree present on firmware 3.4.x+*. Our PDUs answer on the OEM `DCIMPower` tree;
  `/redfish/v1/PowerEquipment` 404s. The working platform `pdumgmt` tool does all
  outlet actuation over SSH CLI and uses Redfish for reads only. So Redfish gives
  us status, not actuation, on the hardware we actually have.
- **The atomic cycle works.** `olDlyReboot <outlet>` (SSH CLI) is the firmware's
  single-command delayed cycle: it turns the outlet off, holds for the configured
  `RebootDuration`, then turns it back on — all lodged in firmware before any
  power is cut. We set `olRbootTime <outlet> 60`, fired `olDlyReboot` on **both**
  of leaf-3's feeds back-to-back (3.4 s dispatch skew), and confirmed via
  `olStatus` that **both outlets were `Off` simultaneously** for ~56 s, then
  auto-restored to `On`. Single-feed test on leaf-4 separately confirmed the cut
  is visible from the switch OS (PSU `vin` → 0 V) and that one feed alone keeps
  the switch up.

Ground-truth details (Redfish tree walk, exact CLI output shapes, SSH transport
quirks) are captured in `~/src/scratch/pdu-control/redfish-ground-truth.md`.

## Approach

### A. Transport selection vs. actuation consent

There are two separate concerns the original design conflated under "selection is
automatic":

1. **Capability discovery** — "what *can* power this device?" — is automatic from
   NetBox. slagworks already knows whether a device has a BMC IP and/or PDU
   outlets; there's no reason to make the operator restate facts it has.
2. **Actuation consent / visibility** — "what is about to physically happen?" —
   must NOT be hidden. A BMC reset and a PDU outlet cut have different physics: a
   BMC reset is OS-signalled and single-machine; a PDU cut is a hard AC yank on
   **shared infrastructure** (an outlet-index error de-powers a *different* device
   on the same strip).

**Decision: selection is automatic by precedence policy, but the resolved
transport and exact outlets are echoed before actuation, and `--via bmc|pdu`
overrides.**

Precedence policy (stated as policy, with rationale — not as a detection trick):

```text
default precedence when resolving a machine's power transport:
    BMC present  -> BMCController        (graceful, OS-signalled, single-device
                                          blast radius, can SetBootDevice)
    else PDU outlets present -> PDUController
    else         -> error "no power path"
operator override: --via {bmc,pdu} forces the transport when both are modeled.
```

*I chose BMC-precedence-with-override over a bare field-presence rule because once
servers also get PDU outlets modeled (the stated long-term direction), "BMC wins"
silently makes PDU unreachable on every server via auto-selection — which is the
right default (prefer the gentler, narrower-radius actuator) but a trap if read as
"PDU support." The override is not a nice-to-have: for a server whose BMC is
**wedged**, `--via pdu` is the *primary* and only way to hard-cycle it at the PDU.
That is precisely the recovery case PDU control on a BMC-equipped box exists for.*

Near-term scope: the only PDU-auto-selected devices are the **BMC-less Mellanox
leaf/spine switches**. Servers keep using BMC. So "PDU-managed device count" ≈
switch count, not host count (§Numbers).

A consequence that reinforces §C/§D: because `--via pdu` on a wedged-BMC server is
a real path, `PowerStatus` for a PDU controller **cannot** cross-check against the
BMC (the BMC may be dead — that's why we're at the PDU). PDU status must stand
alone, from the PDU's own reporting.

Selection lives in the site-worker controller factory, renamed
`bmcControllerFactory` → `powerControllerFactory`. The `m.BMCIP == ""` hard-fail
and the matching executor hard-fails are replaced by the precedence branch; the
executor stays transport-agnostic and just asks the factory for a controller.

### B. Data flow: a `pdu_runtime` op envelope, NOT the snapshot

The original design added `PDUOutlets []PDUOutlet` to `MachineSnapshot`. **Struck**
— that denormalizes NetBox-owned topology into every snapshot (staleness + creds
riding every push) for data only needed when a power op is actually in flight.

**Decision: resolve a machine's outlets + creds at op-creation time on global, and
stamp them into a `pdu_runtime` block in `op.Params`, exactly parallel to the
existing `bmc_runtime` envelope (`internal/api/bmc_runtime.go`).**

```go
// op.Params["pdu_runtime"] — built by resolvePDURuntime at op-creation, mirroring
// resolveBMCRuntime. Travels only with power/lifecycle ops, never in the
// steady-state snapshot.
type PDURuntime struct {
    Outlets []PDUOutlet `json:"outlets"`
}

type PDUOutlet struct {
    PDUHost  string `json:"pdu_host"`  // PDU OOB mgmt IP, e.g. "10.254.240.154"
    Outlet   int    `json:"outlet"`    // 1-based index (NetBox "Outlet 5" -> 5)
    Serial   string `json:"serial"`    // PDU NetBox serial == secret key (diag + cred join)
    Username string `json:"username,omitempty"`
    Password string `json:"password,omitempty"`
}
```

*I chose the op-envelope over the snapshot because it mirrors the BMC cred path the
project already converged on (`bmc_runtime`), keeps creds out of every snapshot
push, eliminates the staleness window (outlets resolved fresh from NetBox per op),
and is exactly where slagctl reads "here is what is about to be flipped" for the §A
consent echo. The snapshot would only win if the site worker needed outlet data
absent an op (e.g. autonomous PSU polling) — which the operator-triggered-only
model explicitly does not.* This also means the snapshot schema is unchanged and
`ContentHash` needs no new sort key.

**Resolution (`resolvePDURuntime`, new, next to `resolveBMCRuntime`):** for the
machine's NetBox device, `GET /api/dcim/power-ports/?device_id=<N>` returns
power-ports with `connected_endpoints` populated — each endpoint is a power-outlet
carrying its parent PDU device + outlet name. For each: the PDU device's serial is
the cred-secret key (§E); its `mgmt_only` interface IP is the actuation host (reuse
`netboxBMCIPForDevice` shape, `bmc_runtime.go:298`); the outlet name parses to the
index. **Watch the NetBox 4.5.4 `device_id__in` footgun** (`bmc_runtime.go:285`):
the plural `__in` filters are silently ignored — use single-value `device_id=`.

Outlet-name parse is **strict and fail-closed**: anchored `^Outlet (\d+)$`, error on
any non-match. Verified safe — all 24 outlet names on device-type 29 (APDU9965) and
both live instances are uniformly `"Outlet N"` (space, no zero-pad). A sloppy
trailing-digit grab is exactly how you flip the wrong outlet; we don't do that.

### C. Transport: hybrid — SSH CLI actuates, Redfish reads status

**Decision: actuation (on/off/cycle) is the APC NMC CLI over SSH; status reads are
Redfish. This is forced by the hardware (see "What we proved") and mirrors the
working `pdumgmt` tool's split.**

The original design's "Redfish-only" and "atomic-cycle-only" were both load-bearing
operator requirements, and they are **mutually exclusive on this firmware**: the
NMC's Redfish exposes no atomic outlet cycle (only discrete On/Off, and not even
that on our OEM `DCIMPower` tree). The atomic cycle (`olDlyReboot`) is SSH-CLI-only.
When transport-aesthetics collide with a safety property (§D lock-out), safety wins
— so SSH stays, confined to actuation.

Actuation verbs (SSH CLI, validated live):

- `olDlyReboot <outlet>` — the atomic delayed cycle (off → hold `RebootDuration` →
  on). The cycle is lodged in firmware on command receipt; losing the SSH session
  afterward does not interrupt it. **This is the only cycle primitive we use.**
- `olOff <outlet>` / `olOn <outlet>` — discrete off / on, for the deliberate
  terminal-state ops `PowerOff` / `PowerOn` (§D). Never composed into a cycle.
- `olRbootTime <outlet> <sec>` — set the per-outlet off-window (1–7200 s). The
  controller sets this to a configured value (default **60 s**) immediately before
  each `olDlyReboot`, so the cycle is self-contained and not dependent on PDU
  config drift.

Status read (Redfish, validated live):

- `GET /redfish/v1/DCIMPower/default/RackPDU/0/Outlets/<N>` → `Oem.SwitchedState`
  (`"On"`/`"Off"`). Note: vendor-OEM field on the OEM tree, **not** the DMTF
  standard `PowerState` — name it explicitly. Basic auth `apc:<pw>`, `curl -k`
  (self-signed, same posture as BMC Redfish).

*I chose Redfish for status over the CLI `olStatus` because Redfish returns
structured JSON with honest HTTP status codes and no cred-in-argv exposure, whereas
`olStatus` is human-readable text we'd screen-scrape over a pty. We already speak
Redfish to these PDUs for reads. Actuation can't use Redfish (no atomic cycle on
this firmware), so it stays on the CLI — accepting that we run two transports, each
on the surface it's actually good at.* SNMP is **not** used (it was only ever a
status-read fallback; Redfish covers status cleanly).

**SSH transport: native Go `crypto/ssh`, NOT a shell-out.** Actuation uses
`golang.org/x/crypto/ssh` directly from the site worker — no `ssh`/`sshpass`
subprocess, no external binary dependency, password fully in-process.

The one non-obvious requirement (root-caused live, writeup at
`~/src/scratch/pdu-control/ssh-rootcause.md`): **the APC NMC's SSH server is
cryptlib, which rejects Go's default client banner `SSH-2.0-Go`** — cryptlib's
peer-ID parser enforces a minimum software-version-token length, and `Go`
(2 chars) is too short, so it disconnects with `SSH_DISCONNECT_PROTOCOL_ERROR`
right after KEXINIT. This is NOT an algorithm/KEX/Terrapin issue (the negotiated
suite is standard and OpenSSH works fine); it is solely the banner. Setting
`ClientConfig.ClientVersion = "SSH-2.0-OpenSSH_8.9"` (any longer token works;
we mimic OpenSSH) is the entire fix. Validated live: native crypto/ssh with
that banner completes handshake + auth + `olStatus 5` → `E000` against the real
NMC. *An earlier draft of this design assumed crypto/ssh couldn't talk to the
NMC and specified a `sshpass` shell-out; that was a misdiagnosis — the banner
gate, not an unfixable handshake. There is no shell-out and no `sshpass`/`ssh`
`.deb` dependency.*

Other transport specifics (validated):

- Pin cipher `aes256-ctr` (the NMC offers it); KEX/MAC/host-key left at x/crypto
  defaults (the server's `ecdh-sha2-nistp256` / `hmac-sha2-256` /
  `ecdsa-sha2-nistp256` are all in the default sets).
- One command per session — the NMC's interactive CLI mangles multi-line input;
  a single `session.CombinedOutput(cmd)` is clean and needs no pty.
- **No exit-status from cryptlib.** The NMC closes the exec channel without an
  SSH `exit-status` message, so `CombinedOutput` returns a benign "exited without
  exit status" error even on success. **Ignore the exec error; parse the APC
  result code in the output** (`E000` = success, `E1xx` = error). That code, not
  the session exit, is the real signal.
- Host key not verified (`ssh.InsecureIgnoreHostKey`): a PDU is a replaceable
  appliance on the trusted OOB segment, and key-pinning would break on swap —
  consistent with the project's "OOB-net unauth is OK for v0" stance.
- ctx cancellation honored via a watchdog goroutine that closes the client conn
  (crypto/ssh sessions take no context).

**Reach: direct, no gateway hop.** The site worker runs on the oob-gw host, which
is L3-adjacent to the PDUs, so it dials the PDU IP directly — same posture as
`BMCController` reaching a BMC IP. We do NOT replicate `pdumgmt`'s `ssh root@<gw>`
outer hop (an artifact of that tool running off-network). And unlike the original
plan, the actuation path no longer shells out — it uses native Go transports just
like `BMCController` (bmclib), so there's no process-spawn cost and no external
binary to package.

### D. Multi-feed semantics and the atomic-cycle reversal — the critical decision

A redundant-PSU device (leaf-3) has TWO outlets on TWO PDUs and **does not lose
power when one outlet is cut**. The original design defined `PowerCycle` as "off
ALL → dwell → on ALL", composed by slagworks from discrete `olOff`/`olOn`. **That is
reversed.** Composing a cycle ourselves is dangerous:

*The two-phase off-then-on has a window where the target is unpowered between two
separate instructions. If the target is (or sits behind) the OOB top-of-rack switch
that carries our own control path to the PDU, the "off" kills our path and the "on"
can never be delivered — the device is stranded dark until smart hands arrive. The
catastrophe is intrinsic to the two-phase shape; even a perfectly-executed off→on
strands us. The firmware's `olDlyReboot` is **one** instruction that lodges the
entire off-delay-on cycle in the PDU before any power is cut. We lose our session
during the off-window but don't need it — the PDU completes autonomously. Atomicity
IS the safety property.*

**This eliminates the original design's worst failure mode entirely.** The old
off-all/on-all model had a "worker crashes between off and on → box hard-down for up
to the 30-min op-timeout" window. With `olDlyReboot`, once the command is accepted
(`E000`), the cycle completes regardless of what happens to slagworks. There is no
hard-down-on-crash window.

Definitions (the `Controller` interface keeps three real methods; `PowerCycle` is
NOT composed from `PowerOff`+`PowerOn` — interface contract, §Interface-completeness):

**PowerCycle — atomic firmware cycle on ALL feeds.** For each outlet: `olRbootTime
<n> 60` then `olDlyReboot <n>`, dispatched to all feeds back-to-back. On a
single-feed device this is one atomic cycle. On a dual-feed device, the two atomic
cycles overlap: with a 60 s `RebootDuration` and sub-10 s dispatch skew between the
two PDUs, both feeds are off simultaneously for ~50+ s → a genuine cold restart.
*Validated live on leaf-3: 3.4 s skew, both outlets `Off` together for ~56 s.* The
long duration is what makes dispatch skew irrelevant — the load-bearing invariant
is **dispatch skew ≪ RebootDuration**.

**PowerCycle MUST block until the box is back before reporting success.**
`olDlyReboot` returns `E000` *immediately* (the cycle is async in firmware); the
outlets stay `Off` for the full `RebootDuration` after that. We confirmed this live:
the command returned instantly, `olStatus` read `Off*` at +5 s, and power didn't
restore until +67 s. So a `PowerCycle()` that returns when the SSH command returns
is lying — the executor would proceed to post-op `PowerStatus` (reads `StateOff`)
and lifecycle steps (wait-for-boot) while the box is still hard-down. **The wait
must first get past the off-window, then confirm recovery — polling for `On`
immediately would match the *pre-cut* `On` and falsely return at once.** This is a
real race: for the first few seconds after `olDlyReboot` is accepted, the firmware
hasn't dropped the outlet yet, so it still reads `On` (we saw `Off*` only at +5 s
live). So after dispatching `olDlyReboot` to all feeds the controller **(1) waits
for every outlet to read `Off` (or, as a floor, sleeps the configured
`RebootDuration`), THEN (2) polls Redfish `Oem.SwitchedState` until every outlet
reads `On`** (or a deadline expires), then returns. Step 1 ensures we observe the
trailing recovery `On`, not the leading pre-cut `On`. The poll deadline must cover
the *latest* feed's window — max dispatch skew + `RebootDuration` + margin — and
sits well inside the executor's `OpTimeout` (30 min). *Note this is the one place the atomic design re-introduces a block that
the old composed off→dwell→on model had for free: by moving the timing into
firmware we removed slagworks' natural dwell, so the completion-wait has to be
explicit. Polling Redfish (not a fixed sleep) is chosen so completion means "the
box is actually powered again," not merely "enough time elapsed" — a flaky feed
that never comes back surfaces as a timeout, not a false success. The poll is
read-only Redfish (cheap), not an SSH loop, so it doesn't contend for NMC SSH
slots.*

**PowerOff — `olOff` ALL feeds; the genuinely dangerous op.** This is the one shape
that cannot be made atomic-safe (its purpose is to stay off). For a device carrying
our own control path (an OOB ToR switch), PowerOff *can* strand us. It therefore
gets the heaviest consent gate (§A) and should never be the auto-selected action
for an OOB-infra device.

**PowerOn — `olOn` ALL feeds.** Best-effort across all outlets (continue on error):
stopping early would defeat redundancy by leaving a reachable feed un-powered. Op is
`failed` if any feed failed to come on, but the box is maximally likely up.

**PowerStatus — Redfish `Oem.SwitchedState` aggregate.** Read each outlet; fold:

| Per-outlet reads | Aggregate |
|---|---|
| all `On` | `StateOn` |
| all `Off` | `StateOff` |
| mixed (all readable) | `StateOn` + WARN |
| any `On`, others errored | `StateOn` + WARN (box is definitely powered) |
| no `On` confirmed, ≥1 errored | `StateUnknown` + error |

*Any confirmed-live feed pins `StateOn`: a dual-fed box is powered as long as one
feed is live, and reporting `Off`/`Unknown` while a feed is confirmed up is a lie
that could send an operator to touch live hardware. `StateUnknown` is reserved for
"no live feed confirmed AND a read errored" — we can't rule out a feed we couldn't
reach.*

**Failure handling (per CLAUDE.md "trigger/preconditions/failure/rollback"):**

- **Trigger:** an `OpPowerOn/Off/Cycle` claimed by the executor, routed to a PDU
  controller.
- **Preconditions:** ≥1 outlet descriptor, each with host/index≥1/creds. No
  external binaries needed (native crypto/ssh + net/http).
- **PowerCycle partial failure (dual-feed):** outlets dispatched sequentially. If
  feed A accepts `olDlyReboot` (`E000`) but feed B's SSH fails, feed A cycles and
  feed B stays up → **the box stays powered** (B holds it); it just isn't the cold
  restart requested. Mark the op `failed` (operator wanted a cycle and didn't get
  one) but the box is safe — there is no hard-down window, unlike the old composed
  model. Operator retries. *This safety is the entire payoff of the atomic reversal.*
- **PowerOff partial failure: roll back to fully-on.** If `olOff` cuts feed A but
  feed B's `olOff` fails, the box is left running on one PSU — degraded, redundancy
  silently lost. The controller **issues `olOn` to every feed it sent `olOff` to,
  including the one whose `olOff` failed**, restoring the device to its known-good
  pre-op state, then reports the op failed. The failing feed is included because
  a failed `olOff` may still have cut it: the SSH session can drop, hit the
  command timeout, or return output with no result code after the NMC has
  executed the command, and the transport reports each of those as an error.
  `olOn` only powers toward up, so sending it to a feed whose `olOff` never ran
  leaves that feed on. The same applies to a single-feed device: a failed `olOff`
  gets an `olOn`.
  *I chose rollback-to-pre-op-state over leave-half-off (the rest of this design's
  no-rollback stance) specifically for PowerOff, because a FAILED op should return
  to the baseline the operator can reason about, not strand the box in a degraded
  half-state. "Asked for off, got half-off, retry from clean redundancy" beats
  "asked for off, got half-off, now reason about which feed is live." This is the
  one rollback in the design, and it's safe to make: the rollback action is `olOn`
  (powering toward up, never toward dark), so even if the rollback `olOn` itself
  fails the box only ends up more-powered, never stranded. (Contrast PowerCycle,
  where the firmware completes the cycle autonomously — there is nothing to roll
  back; and PowerOn, which is already best-effort toward up.) If the rollback `olOn`
  also fails, report both errors; the box is then in whatever partial-feed (or, on a
  single feed, off) state the failure left it, which a subsequent `PowerStatus`
  reports. When the failing feed's PDU is simply unreachable, its rollback `olOn`
  fails too, so the op reports "rollback failed" even though that feed was never
  cut; that is the accepted cost of not guessing whether a failed `olOff` ran.
- **PowerOn partial failure:** best-effort all feeds, then report aggregate.
- **Rollback:** the *only* rollback is PowerOff's redundancy-restore above
  (`olOn` the feeds it may have cut). PowerCycle and PowerOn do NOT roll back: PowerCycle
  has no intermediate state to revert from (the firmware completes the cycle
  autonomously — a failed dispatch leaves the box either up on the other feed or
  mid-firmware-cycle), and PowerOn is already best-effort toward up. The reason
  PowerOff is the exception is direction: its rollback (`olOn`) powers *toward up*,
  which is always safe, whereas a PowerCycle/PowerOn "rollback" would have to power
  *toward dark* to undo, which can strand. A post-op `PowerStatus` reports the true
  state in every case.
- **PDU SSH-slot contention:** an APC NMC caps concurrent SSH sessions (~1–4). One
  PDU feeds many devices. Within a single site worker, ops already execute serially
  (`executor.go` `processOps` is a sequential loop), so one worker never opens
  concurrent sessions to one PDU. *This is a load-bearing invariant: if op
  execution is ever parallelized, PDU actuation needs an explicit per-PDU-IP
  semaphore added at the same time.* Cross-worker (HA) contention is a real but
  low-probability gap at v1's operator-driven op volume; batch/rack-level power is
  out of scope (§Out of Scope) — when it's built, it needs cross-worker
  serialization per PDU, not before.

### E. Credentials — done

**This is already implemented and deployed this session.** PDU creds live in the
same single per-pop secret as BMC creds — no separate PDU secret, no new mechanism.

- The secret `pop/<pop>/bmc-creds-admin` (name kept to avoid an IAM rescope; it is
  a misnomer — it holds both kinds) is **serial-keyed** (operator decision
  2026-05-28) and carries `kind: "bmc"` and `kind: "pdu"` entries. PDU entries are
  `{kind:"pdu", username:"apc", password:...}` keyed by the PDU's NetBox serial.
  All three pops' secrets were rebuilt serial-keyed and pushed (pop1 replaced in
  place; pop2/pop3 created) on 2026-06-02.
- `internal/awssecrets` was rewritten to match: `AdminSecret.Lookup(serial)`
  (was hostname), `AdminHost.Kind` exposed, `GetAdmin` (was `GetBMCAdmin`).
  Call sites in `builder.go` and `bmc_runtime.go` now key by `device.Serial`.
  Build/vet/tests green.
- So `resolvePDURuntime` (§B) resolves each outlet's PDU serial, looks it up in the
  already-loaded admin secret, filters on `Kind == "pdu"`, and stamps
  username/password — both fields or neither, as for BMC creds. The admin secret is
  the sole source, exactly as it is for BMCs.

### Interface-completeness: SetBootDevice, ChassisSerial, the atomicity contract

The `power.Controller` interface requires all methods. Critically:

- **`PowerCycle` MUST map to the single firmware-atomic `olDlyReboot` (per feed),
  NEVER to `PowerOff()` then `PowerOn()`.** This is an interface contract, not an
  impl detail — a DRY "default cycle = off+on" would silently reintroduce the
  lock-out (§D). The PDU `PowerCycle` is atomic all the way down to the firmware
  verb.
- **`SetBootDevice` — `nil` on PXE/Default, error on disk/other.** A PDU can't set
  boot order, so PDU-managed devices MUST have **PXE as permanent firmware boot
  order** so every cycle re-PXEs. Since switches now go through deploy (Q5
  resolved: yes), this is load-bearing: `BootPXE`/`BootDefault` are satisfied by
  the permanent-PXE firmware setting (return `nil`); `BootDisk` returns an error
  rather than silently lying to the executor (matches `BMCController.SetBootDevice`,
  which errors on unsupported devices).
- **`ChassisSerial` — `("", nil)`.** A PDU knows its *own* serial (we read it over
  Redfish) but not the downstream device's chassis identity. The worker's
  serial-verification gate treats empty as "skip", correctly bypassing chassis
  verification for PDU-controlled devices. The host-side `vin` oracle (reading PSU
  input voltage via lm-sensors from inside the switch OS) can't substitute — it
  requires the target to be up, and a power controller runs when the target may be
  down. (That oracle is a useful deploy/commission-time health check — out of scope
  here; see §Out of Scope.)

## Relationship to platform/pdumgmt — this replaces it

The platform `~/src/platform/internal/pdumgmt/` tool is the working reference and
the thing this work **supersedes**. We do not import it or shell out to it — we lift
the proven primitives into slagworks, rewritten:

- **Lift, rewritten:** the APC CLI actuation verbs (`olDlyReboot`/`olOff`/`olOn`/
  `olStatus`/`olRbootTime`), the Redfish read shape (`redfishGet` minus the
  SSH-through-gateway hop — our site worker has direct OOB reach), and the NetBox
  PSU→cable→outlet→PDU topology walk. We do NOT carry over its shell-out
  (`sshpass`+`ssh`): slagworks talks to the NMC with native crypto/ssh (banner
  workaround above), keeping the password in-process and dropping the external
  binary dependency. The APC result-code parse (`E000`/`E1xx`) is kept.
- **End-state:** the operator-facing PDU verbs `pdumgmt` owns migrate to `slagctl`
  (`slagctl power {on,off,cycle,status}`), so `pdumgmt` can be retired rather than
  lingering. That migration is staged, not a Phase-1 deliverable.
- **Out of the replacement's first cut:** `pdumgmt`'s metrics/firmware/provision/
  audit surfaces are not power control and are not in scope here (§Out of Scope);
  they migrate later or are explicitly dropped, tracked as separate Linear issues.
  Phase 1 stays tight: power control for the BMC-less switches.

## Phasing

Each phase is independently buildable and testable.

**Phase 0 — DONE (this session):** secret consolidated to one serial-keyed,
kind-aware `pop/<pop>/bmc-creds-admin` across all pops; `internal/awssecrets`
rewritten serial-keyed + kind-aware; actuation + status primitives validated live
on leaf-3/leaf-4.

**Phase 1 — NetBox power-topology read + `pdu_runtime` resolution.** Add `PDUOutlet`
/`PDURuntime`; `ListPowerPortsForDevices` + outlet-endpoint resolution in the NetBox
client (single-value `device_id=`, avoid the `__in` footgun); `resolvePDURuntime`
parallel to `resolveBMCRuntime`, filtering admin-secret entries on `Kind=="pdu"`.
*Testable:* an `OpPowerCycle` for leaf-3 carries two `PDUOutlet`s
(`10.254.240.154:4`, `10.254.240.155:4`) with `apc` creds; unit-test against a
NetBox fixture for indices + the strict `^Outlet (\d+)$` parse.

**Phase 2 — `PDUController`, single outlet.** Replace the stub: Redfish status read
(`Oem.SwitchedState`) + native crypto/ssh `olOff`/`olOn`/`olRbootTime`/`olDlyReboot`
for ONE outlet (cryptlib banner workaround); parse `E000`/`E1xx`;
SetBootDevice/ChassisSerial per contract. No external-binary dependency. Inject the
SSH-conn + Redfish-client as interfaces (mirroring `bmcClient`, `bmc.go:48`) so
tests assert exact CLI strings / Redfish paths. *Testable:* fake-transport unit
tests + one manual single-outlet end-to-end (exercised by hand on leaf-4 outlet 5).

**Phase 3 — multi-feed + factory selection.** `PDUController` over `[]PDUOutlet`:
atomic cycle on all feeds, off-all/on-all for terminal ops, aggregate Redfish
status per §D. Rename `bmcControllerFactory` → `powerControllerFactory`; add the
precedence branch + `--via` override; remove the executor `BMCIP==""` hard-fails.
*Testable:* factory builds a PDU controller for leaf-3; `OpPowerCycle` cycles both
outlets with overlap; partial-failure surfaces the directional rules; status folds
correctly. (Dual-feed cold restart already validated live on leaf-3.)

**Phase 4 — executor + slagctl + lifecycle.** `runPowerOp` end-to-end for a PDU
device (no PDU-specific code needed — the Controller abstraction's payoff).
`slagctl power {on,off,cycle,status} <machine>` with the §A consent echo (print
resolved transport + outlets before actuating) and `--via`. Because switches go
through deploy (Q5), confirm `runLifecycleOp` routes through `powerControllerFactory`
too. *Testable:* `slagctl power cycle leaf-3` → global op (`pdu_runtime` resolved) →
site worker → both outlets cycle → status confirms.

## Numbers

- **Outlets per device:** 1 (non-redundant) or 2 (redundant-PSU). No device has >2
  feeds. Fan-out loop is N≤2 — nothing to parallelize, so within-machine sequential
  actuation is correct.
- **PDUs per pop:** single-digit (left/right per rack pair).
- **PDU-managed devices per pop:** the BMC-less switches (leaf + spine). ~6 per pop
  → ~12 outlet resolutions per relevant op. Servers stay on BMC even though they're
  also plugged into PDUs, so PDU-managed count ≈ switch count, not host count.
- **NetBox cost:** one `power-ports` fetch per PDU-controlled machine, at
  op-creation (not per-snapshot, not a hot path). Negligible.

These confirm the design is correctly sized: tiny fan-out, single-digit PDUs,
switch-count PDU-managed devices.

## Open Questions

- **Q-status-firmware-floor.** Our pop1 PDUs run NMC `v2.5.3.1` on the OEM
  `DCIMPower` Redfish tree. If any PDU is on a *different* firmware whose Redfish
  status shape differs (e.g. the 3.4.x standard `PowerEquipment` tree), the status
  read path must handle both, or we set a firmware floor and verify it at PDU
  onboarding. Resolve by auditing actual firmware across pops before Phase 2.
- **Q-rboottime-tuning.** 60 s `RebootDuration` gave ~56 s dual-feed overlap at
  3.4 s skew on leaf-3 — ample. If dispatch skew is ever larger (slow OOB, more
  feeds), confirm the margin holds; the knob is configurable. Decide a default after
  Phase 3 multi-feed testing on more devices.
- **Q-status-redfish-vs-cli-blessing.** Status is Redfish `Oem.SwitchedState`;
  actuation is SSH. Confirm the operator is fine with the two-transport split (it's
  forced by the firmware, but worth an explicit nod since `slagctl power status` and
  `slagctl power cycle` then hit different planes).

## Out of Scope

- **PDU power metrics / monitoring** (amps/volts/watts) — the platform tooling's
  job; slagworks reads outlet *state*, not consumption.
- **PDU administration** — firmware, NMC config, outlet naming/labeling/scheduling.
- **Non-APC PDU vendors** — this hardcodes APC NMC CLI verbs + the APC Redfish OEM
  tree. A second vendor is a new transport behind the same `Controller` interface,
  designed when one appears (no speculative abstraction).
- **The host-side `vin` verification oracle** — requires the target up; can't live
  in a power controller. A possible future deploy/commission-time health check.
- **Rack-level / batch power orchestration + cold-start ordering** — v1 is
  per-machine, operator-driven, no cross-machine sequencing. Batch power (and the
  cross-worker PDU serialization it would require) is a separate orchestration layer.
- **MQTT / pdu-mqtt** — dead idea from chainlink #86's title; no broker, no need.

## Key files

| Concern | File |
|---|---|
| Controller interface | `internal/power/power.go` |
| PDU controller (stub → impl) | `internal/power/pdu.go` |
| BMC controller (pattern to follow) | `internal/power/bmc.go` |
| Controller factory + selection | `cmd/slagworksd/site.go` (`bmcControllerFactory` → `powerControllerFactory`) |
| Executor power/lifecycle ops | `internal/worker/executor.go` |
| Op-envelope cred resolution (pattern) | `internal/api/bmc_runtime.go` (`resolveBMCRuntime` → add `resolvePDURuntime`) |
| Admin secret reader (done, serial-keyed) | `internal/awssecrets/awssecrets.go` |
| Snapshot builder (NetBox reads) | `internal/snapshot/builder.go` |
| NetBox client | `internal/netbox/` |
| Reference being replaced (do NOT depend on) | `~/src/platform/internal/pdumgmt/` |
| Live ground-truth notes | `~/src/scratch/pdu-control/redfish-ground-truth.md` |
| Commissioning-flow PDU note | `docs/archeology/commissioning-flow-2026-03.md` |
