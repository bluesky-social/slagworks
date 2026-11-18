# BMC Management: IPMI vs Redfish Vendor Matrix

Status: research draft for review. No implementation in this commit.

## TL;DR

The IPMI 2.0 specification has been frozen since 2015; its reference user-space
client (`ipmitool`) was archived by its maintainer in February 2023; the active
DMTF standard for new BMC management work is Redfish, which ships
quarterly. On the slagworks fleet specifically, in-band IPMI password
rotation is already broken on Supermicro H13SSH (`AS-1115HS-TNR`,
the bulk of pop1/pop2) — the firmware rejects every `user set password`
shape `ipmitool` produces, almost certainly because of an AMI MegaRAC
password-complexity check that surfaces as IPMI completion code
`0xC8` ("Request data field length exceeded"), not because of a true
length issue. Redfish is the path forward for password rotation
on H13SSH; the rest of the BMC interactions (boot order, power cycle,
inventory) should follow because we don't want two clients with
overlapping coverage. Recommendation: add a Redfish client (`gofish`),
default to it for password rotation on Supermicro AMI BMCs, leave
in-band IPMI as the boot-order writer until Redfish persistent
boot override is verified on real hardware (it has known
"Continuous gets cleared after first reboot" quirks on
Supermicro). No vendor-conditional code path on the *slagworksd*
side: the agent already runs a vendor-aware codebase, that's where
the conditional belongs.

## Industry trajectory

### IPMI: frozen, not deprecated

The most recent IPMI specification revision is **IPMI v2.0 rev 1.1
Errata 7, published 2015-04-21**. No revision has shipped in the
~11 years since. The IPMI Promoter group (Intel + Dell + HP + NEC)
has not announced a 3.0 effort. Source: Wikipedia [Intelligent Platform
Management Interface](https://en.wikipedia.org/wiki/Intelligent_Platform_Management_Interface),
which cites the published specification document. Intel's official
"IPMI Technical Resources" page is the canonical home for the
specification but currently 403s on programmatic fetch — needs
human verification of any deprecation language Intel may have
added there.

I cannot find a primary-source Intel statement saying IPMI is
"deprecated." The often-repeated "Intel announced IPMI deprecation
in favor of Redfish" claim circulating in industry blogs appears
to descend from a 2018 ServeTheHome article that I could not locate
on a refetch — **mark as commonly-stated-but-unverified**. What is
verifiable: Intel has not published an IPMI revision since 2015,
and none of the major BMC firmware vendors are advertising new
features against IPMI.

ipmitool, the de facto reference user-space client, is **archived
on GitHub as of February 2023**. Last release: v1.8.19 on
2022-08-30. Last commit on the default branch: 2023-01-12. Sources:
[github.com/ipmitool/ipmitool](https://github.com/ipmitool/ipmitool)
(archive flag, latest release tag, last commit timestamp via
`gh api repos/ipmitool/ipmitool/commits`). There is no fork that
the major distros have rallied around — Debian/Ubuntu, RHEL,
and Alpine are still shipping 1.8.19 with their own patches.

The Linux kernel IPMI subsystem (`drivers/char/ipmi/`) is **still
under active maintenance** — concrete bug fixes and a new SSIF BMC
unit-test framework landed in April 2026 (e.g., commit "ipmi: Add
limits to event and receive message requests", 2026-04-20). This
is not "frozen." Source: `gh api repos/torvalds/linux/commits` with
`path=drivers/char/ipmi`. So in-band IPMI access from a Linux host
is not going away — but the user-space tooling that drives it is
not getting new features.

### Redfish: active, quarterly cadence

Redfish was first published as a base-models 1.0 spec in
**August 2015**. It is updated by DMTF on a roughly quarterly
release cadence, with bundles labeled `<year>.<quarter>`. The most
recent published bundle is **2026.1**, committed to
`DMTF/Redfish-Publications` on 2026-05-18; previous bundles
in the trailing 12 months: 2025.4 (2026-01-20), 2025.3
(2025-10-20), 2025.2 (2025-06-17), 2025.1 (2025-03-24).
Source:
[github.com/DMTF/Redfish-Publications](https://github.com/DMTF/Redfish-Publications)
commit history. New schemas continue to land in every release.

DMTF's reference tooling tracks the spec actively:
[Redfish-Service-Validator](https://github.com/DMTF/Redfish-Service-Validator)
last commit 2026-05-15;
[Redfish-Tacklebox](https://github.com/DMTF/Redfish-Tacklebox)
2026-05-15;
[Redfish-Event-Listener](https://github.com/DMTF/Redfish-Event-Listener)
2026-05-01.

For Go callers, [stmcginnis/gofish](https://github.com/stmcginnis/gofish)
is the de facto library; release cadence is monthly (v0.21.0 series
shipped Feb–Apr 2026; v0.21.6 on 2026-04-14). It depends only on the
standard library + `golang.org/x/crypto` and is well-known to
consumers like
[ironcore-dev/metal-operator](https://github.com/ironcore-dev/metal-operator)
which uses it in production for bare-metal lifecycle.

For Python, [openstack/sushy](https://opendev.org/openstack/sushy)
backs OpenStack Ironic. It scopes itself to "what Ironic needs"
and intentionally omits user/account management — useful as a
reality check on what the OpenStack community considers safe-to-use,
but not the right shape for slagworks's user reconcile use case.

### Vendor adoption

The Wikipedia [Redfish article](https://en.wikipedia.org/wiki/Redfish_(specification))
lists Dell iDRAC 7/8/9, HPE iLO and Moonshot, Lenovo XClarity, IBM
Power Systems, Cisco IMC, Fujitsu iRMCS5, Advantech SKY, and Supermicro
X10/X11 as Redfish-supporting BMC platforms. The article was last
substantively updated in 2017 — current generation (X12/X13/H12/H13
on Supermicro; iDRAC9-and-newer on Dell) all advertise Redfish
support, but I'd want to mark "X14 / current ASUS RS501A
status" as **needs vendor confirmation** rather than assert.

## Vendor matrix

### Supermicro `H13SSH` / `AS-1115HS-TNR` (kworker, nova-gen2 — 82 chassis at pop1/pop2)

BMC firmware family: AMI MegaRAC SP-X (Supermicro brands the firmware
build but the underlying stack is AMI's). Source: a fleet DIMM
inventory + AMI's [MegaRAC SP-X product
page](https://www.ami.com/megarac/) ("AMI's mature and widely deployed
BMC firmware solution").

**IPMI status**: BROKEN for password rotation. Every form of
`ipmitool user set password` fails with "Request data field length
exceeded" on the H13SSH AMI build that ships on `kworker-l34-1.pop1`.
Other user-management primitives (set name, set privilege, channel
setaccess, enable) work, as does boot-order persistence via
`chassis bootparam set bootflag force_pxe`. Source:
`docs/archeology/handoffs/2026-05-21-kworker-l34-1-first-deploy.md`
lines 297–322 (manual reproduction on hardware).

The "Request data field length exceeded" error is misleading. IPMI
completion code `0xC8` is named `IPMI_COMP_CODE_REQUEST_LENGTH_LIMIT`
in the spec, but the TrueNAS team documented in
[truenas/middleware PR #11325](https://github.com/truenas/middleware/pull/11325)
that AMI BMCs return this same code when the password fails the
firmware's complexity check ("...the error messages from ipmitool
for passwords that lack sufficient complexity are less than helpful
('Request data field length limit exceeded')"). slagworks generates
passwords from `[A-Z0-9]` only — no lowercase, no special — which
is exactly the shape that trips AMI complexity-policy rejection.
**This is a likely root cause of the kworker failure** but
remains unverified on real hardware until we test a complexity-passing
password through the same `ipmitool user set password` codepath.

**Redfish status**: account management endpoints are present at
the standard URL pattern. POST to `/redfish/v1/AccountService/Accounts/`
creates a user; PATCH to `/redfish/v1/AccountService/Accounts/<id>/`
updates one. Standard payload shape (`UserName`, `Password`, `RoleId`,
`Enabled`). Source:
[flaviotorres/supermicro-redfish](https://github.com/flaviotorres/supermicro-redfish)
which is a small shell script collection targeting Supermicro
specifically and uses these exact endpoints — the URL pattern is
the DMTF-standard `AccountService` resource, not a Supermicro OEM
extension.

**Redfish boot-order status**: present but with a documented
quirk. The `/redfish/v1/Systems/<id>` resource exposes `Boot`
with `BootSourceOverrideTarget` and `BootSourceOverrideEnabled`,
and the standard `Continuous` value should persist across reboots.
However, the [matti/superfish](https://github.com/matti/superfish)
client (Python, Supermicro-specific) carries a documented warning:

> Note: some Supermicro BMCs clear "Continuous" boot override after
> the first reboot. Prefer ``one_time=True`` (the default) and
> re-set before each boot if multiple reboots are expected.

Source: `src/superfish/bmc.py`. Whether H13SSH specifically is
affected is **needs hardware verification**. If it is, in-band
IPMI's `chassis bootparam` (which works today) is the safer
boot-order writer regardless.

**Redfish Host Interface (in-band) status**: confirmed present.
Supermicro Update Manager (SUM) advertises `Redfish_HI` as one of
its supported in-band interfaces ("Execute In-Band commands through
Redfish Host Interface"). Source:
[Supermicro SUM 2.10.0 documentation](https://github.com/markwdalton/lambdalabs/blob/main/documentation/BIOS/supermicro-sum/Supermicro-SUM-usage.txt)
captured from an x86_64 build dated 2022-12-09. The Redfish Host
Interface is defined in DMTF DSP0270 and presents as a USB-NIC
device on the host (typically a link-local address in
`169.254.0.0/16`); the host can talk to the BMC's Redfish endpoint
without the BMC's data-network IP being reachable.

**Authentication**: Redfish supports HTTP Basic and session-token
auth (DMTF spec). On AMI-based Supermicro BMCs, both are exposed.
Default credential is `ADMIN/ADMIN` from the factory, replaced by
slagworks's vendor-managed `pop/<pop>/bmc-creds-admin` AWS secret
on commissioning.

**CVE noise**: AMI MegaRAC SP-X is the firmware family most recently
implicated in CVE-2024-54085 (Redfish authentication bypass on
versions 12.x < 12.7 and 13.x < 13.5). Worth tracking firmware
revisions across the H13SSH fleet — if any are below those
thresholds, an internal-network unauthenticated RCE pre-exists
slagworks's choice of management protocol. Source:
[The Hacker News, 2025-03-19](https://thehackernews.com/2025/03/new-critical-ami-bmc-vulnerability.html)
referenced in
[mthcht/ThreatIntel-Reports](https://github.com/mthcht/ThreatIntel-Reports).

### ASUS `RS501A-E12-RS4U` (pop3 — 3 chassis today, fleet expanding)

BMC firmware family: ASMB12-iKVM (ASUS-branded; ASUS is also an
AMI MegaRAC OEM, but cross-confirmation that RS501A specifically
ships AMI is **needs vendor confirmation** — not load-bearing for
our recommendation).

**IPMI status**: working today for password rotation, listing,
boot-order persistence. Slagworks's bmcreconcile has been exercised
against cpu1/cpu2/cpu3-pop3 and succeeds. Quirks documented in
`MEMORY.md`: ASUS's IPMI session pool is small (~5 slots) and
requires BMC reset via Redfish to clear stuck sessions; SOL is
on `ttyS1`, not `ttyS0`; Redfish password changes do not
propagate to the IPMI auth channel (cpu3: `console` user works
for IPMI but `admin` doesn't, even though both exist on the
Redfish side). This last quirk is operationally important — it
means a Redfish-only password rotation could *silently* leave
the IPMI channel unauthenticatable, which would break power
control via `slagworksd`. **In-band IPMI password rotation
remains the safer write path on RS501A until we verify Redfish
writes propagate.**

**Redfish status**: confirmed present. ASUS sibling SKU `RS720A-E12`
exposes Redfish over USB Host Interface (Protocol ID 04 in DMI
Type 42) per
[linuxhw/DMI](https://github.com/linuxhw/DMI/blob/master/Server/ASUSTek%20Computer/RS720/RS720A-E12-RS12/6002D3D7ADE4)
— RS501A-E12 is the same generation, almost certainly the same
firmware base, but a direct DMI dump from one of our chassis would
confirm. CVE-2024-54085 also enumerates `Asus RS720A-E11-RS24U
firmware 1.2.27` as affected — independent confirmation that the
ASUS server line ships Redfish at the standard `/redfish/v1/AccountService/Accounts`
endpoint.

**Redfish boot-order status**: unverified on RS501A specifically.
DMTF-standard `BootSourceOverride*` should work; the Supermicro
"Continuous gets cleared" quirk has not been reported for ASUS,
but absence of evidence is not evidence of absence.

**Authentication**: same as Supermicro. Both Basic and session-token
exposed.

### Other SKUs in the fleet (for reference, not in scope of this design)

`AS-1015CS-TNR` (41 chassis, pop1+pop2, EPYC 9004 single-socket): same
AMI MegaRAC firmware family as H13SSH. Should be assumed to share the
IPMI password-rotation failure mode until tested otherwise. Verify on
hardware before committing.

`AS-2125HS-TNR` (~26 chassis, pop1+pop2, EPYC 9004 dual-socket): same.

### Cross-vendor capability snapshot

| Capability                       | H13SSH (AMI)         | RS501A (ASUS)            |
| -------------------------------- | -------------------- | ------------------------ |
| IPMI: list users                 | works                | works                    |
| IPMI: set name                   | works                | works                    |
| IPMI: set password               | **fails (0xC8)**     | works                    |
| IPMI: set priv / enable          | works                | works                    |
| IPMI: set persistent PXE boot    | works                | works                    |
| IPMI: SOL                        | works (ttyS0)        | works (ttyS1, per chassis) |
| Redfish: AccountService URL      | standard (`/redfish/v1/AccountService/Accounts`) | standard (same path) |
| Redfish: create user (POST)      | likely works (verify) | likely works (verify)   |
| Redfish: change password (PATCH) | likely works (verify) | likely works (verify)   |
| Redfish: persistent PXE boot     | unverified; documented quirk | unverified         |
| Redfish: USB Host Interface      | confirmed (SUM Redfish_HI) | confirmed via sibling DMI dump |
| Redfish auth modes               | Basic + session       | Basic + session          |

"Likely works (verify)" rows mean: the standard URL pattern is
known-present, and at least one independent source uses it on
that vendor; we have not yet driven it from slagworks's own
codebase against our own hardware.

## Recommendation for slagworks

### Adopt Redfish as the BMC management foundation, not the fallback

The current trajectory of "build more on top of IPMI" runs into
two structural problems beyond just the H13SSH password bug:

1. **Tooling decay.** ipmitool is archived. Distros are shipping
   patched 1.8.19 forever. Any new feature we want (better
   diagnostics, structured output, async operations) we either
   contribute upstream — to a project with no maintainer to merge
   — or fork and carry. By contrast, gofish ships features
   monthly and tracks the active spec.
2. **Spec ceiling.** IPMI is frozen at the 2015 feature surface.
   Redfish has shipped, in the last 12 months alone, four bundle
   releases with new schemas for storage, power, telemetry, and
   firmware update. The capabilities slagworks will *want* in
   the next year — structured firmware update, telemetry pulls,
   richer inventory — exist only on the Redfish side.

Conclusion: build the next layer on Redfish. Keep the in-band IPMI
path for the things it currently does well (boot-order persistence,
where it definitively works on every SKU we have) and let it shrink
over time as Redfish coverage gets verified on each vendor.

### Concrete plan

**Phase 1 — add Redfish client, side-by-side with IPMI.**
Pull `github.com/stmcginnis/gofish` into `cmd/slagworks-agent/`. Define
a `bmc.Client` interface that abstracts the operations we
actually do today: `ListUsers`, `SetUserPassword`, `SetUserPrivilege`,
`EnableUser`, `DisableUser`, `SetPersistentBoot`. Implement two
backends behind it: `bmc.IPMIClient` (the existing reconcile.go
ipmitool path, refactored as an interface implementation, no
behavior change) and `bmc.RedfishClient` (new, gofish-based,
talks via the BMC's data-network IP for v1; in-band Redfish
Host Interface is a follow-up).

**Phase 2 — pick the backend per-operation, not per-SKU.**

The selection logic lives in the agent. The slagworksd side is
unaware. Two config inputs:

- `BMC_PASSWORD_BACKEND`: enum `ipmi` | `redfish` | `prefer-redfish`.
  Default: `prefer-redfish`. Means "try Redfish first; if it
  errors with a known-permanent shape (auth fail, 404), fall
  back to IPMI."
- `BMC_BOOT_BACKEND`: enum `ipmi` | `redfish`. Default: `ipmi`.
  Stays IPMI until persistent boot-override is verified on real
  hardware for at least one vendor — per the Supermicro
  "Continuous gets cleared" quirk.

These are agent-side env vars set in the deploy phase params.
Per-machine override is via the existing `params` JSON on the
op (already plumbed; that's how `force` flows through today).
The eventual default end-state is `BMC_PASSWORD_BACKEND=redfish`
and `BMC_BOOT_BACKEND=ipmi-then-redfish` once we've burned in
Redfish boot persistence behavior on each vendor.

**Why not vendor-conditional code?** The agent already runs
inside the deployed OS, where we have access to DMI Type 1
manufacturer/product strings and Redfish Host Interface presence.
A vendor-table that says "RS501A: IPMI; H13SSH: Redfish" is
tempting but brittle: it bakes the current hardware fleet into
the codebase, has to be updated for every new SKU, and the
right answer is usually "try Redfish, fall back if it doesn't
work." Per-operation policy with a single `prefer-redfish`
default does the same job dynamically.

**Phase 3 — switch in-band path to Redfish Host Interface.** Once
Phase 1+2 are stable against the data-network Redfish IP, move
the agent to use the USB-NIC Redfish Host Interface (link-local
`169.254.x.y` typically). This removes the requirement for the
agent to know the BMC's network IP at all and removes the
operator-set BMC password from the agent's required inputs (the
host interface is implicitly authenticated by host-side root).
This is the architectural payoff Redfish makes available that
in-band IPMI through `/dev/ipmi0` already gives us today —
preserving the "agent doesn't need network creds" property
slagworks designed around in `bmcreconcile/reconcile.go`.

**Phase 4 — sunset IPMI for management.** Keep `/dev/ipmi0` for
SOL and watchdog (Linux subsystem still active, no Redfish
equivalent for the kernel watchdog). Stop calling ipmitool
from the agent for any reconcile step. Drop the ipmitool
binary dependency from the deploy squashfs.

### Password-complexity quick fix (parallel to the above)

The H13SSH `0xC8` failure is *probably* a complexity-policy
bug, not a true ipmitool incompatibility. Independent of the
Redfish migration, change `bmcreconcile.generatePassword` to
emit a password from `[A-Za-z0-9]` plus a guaranteed digit
and special character — same bit-of-entropy budget, complexity-policy
compliant. If this fixes IPMI rotation on H13SSH, the urgency
on Redfish password rotation drops to "still strategically
correct, but not on fire." Test this against `kworker-l34-1.pop1`
before merging — it's a five-line change in
[reconcile.go](../../../internal/bmcreconcile/reconcile.go)
around the `alphabet` constant and a one-time hardware run.

This does not change the Redfish recommendation — Redfish is
strategically correct for the reasons above — but it does
reframe the immediate timeline.

## Open questions (need real-hardware verification)

1. **Does a complexity-compliant password (e.g., `Aa9!`-shape)
   succeed on H13SSH via `ipmitool user set password`?** If yes,
   the kworker incident was a slagworks password-generator bug,
   not a fundamental IPMI failure. Validates the "TrueNAS
   theory" cited above. Test target: `kworker-l34-1.pop1` BMC.
2. **Does a Redfish PATCH to `/redfish/v1/AccountService/Accounts/<id>/`
   with `Password` actually rotate the IPMI-auth credential on
   H13SSH, or only the Redfish-auth credential?** ASUS RS501A
   has the documented split where Redfish writes don't propagate
   to IPMI auth (`MEMORY.md`); we need to verify whether
   Supermicro AMI behaves the same.
3. **Does `BootSourceOverrideEnabled=Continuous` persist across
   reboots on H13SSH and on RS501A?** The matti/superfish quirk
   (`some Supermicro BMCs clear it`) and the Supermicro OEM
   `FixedBootOrder` workaround in
   [cholcombe973/libredfish](https://github.com/cholcombe973/libredfish/blob/main/src/supermicro.rs)
   imply it depends on firmware revision. If `Continuous` is
   unreliable on either platform, the right design is "set
   `Once` on every boot from slagworksd before issuing the
   power cycle" — which is a different control-plane shape
   than what we have today (write-once persistent boot order).
4. **Does the Redfish Host Interface USB-NIC come up automatically
   on H13SSH and RS501A under our deploy squashfs kernel?** Need to
   inspect `lsusb` and `ip link` after boot. The kernel needs
   the right NCM/CDC USB-NIC driver; if it's not in our squashfs
   the in-band Redfish path is gated on that. (Standard kernels
   include `cdc_ncm`, `cdc_ether`, `usbnet` — these should be
   present, but verify.)
5. **What firmware revisions are running across the H13SSH fleet,
   and are any below AMI MegaRAC SP-X 12.7 / 13.5 (the CVE-2024-54085
   patch threshold)?** This is independently important regardless
   of slagworks's protocol choice.
6. **AMI MegaRAC SP-X vs OneTree on RS501A.** AMI's product
   marketing distinguishes legacy SP-X from a newer "OneTree"
   stack that emphasizes Redfish/PLDM/MCTP. We don't currently
   know which one ships on RS501A-E12-RS4U. Affects whether
   Redfish features on these boxes are first-class or
   "compatibility-grade."
7. **Supermicro `Oem/Supermicro/FixedBootOrder` resource:**
   `cholcombe973/libredfish` uses it as a workaround for boot-order
   unreliability. If we hit the `Continuous` problem on H13SSH,
   the OEM extension is a known-good fallback — but it's
   non-portable. Worth noting we may need to fork the boot
   logic per-vendor even after standardizing the rest on
   Redfish.

## References

- IPMI specification status (last revision 2015-04-21, v2.0 rev 1.1
  Errata 7): <https://en.wikipedia.org/wiki/Intelligent_Platform_Management_Interface>
- Redfish specification (DSP0266) introduction (August 2015), vendor
  list: <https://en.wikipedia.org/wiki/Redfish_(specification)>
- Redfish published bundles (2025.1 through 2026.1):
  <https://github.com/DMTF/Redfish-Publications/commits/main>
- DMTF active Redfish tooling (validators, listeners, tacklebox):
  <https://github.com/DMTF>
- ipmitool repository (archived 2023-02, last release 2022-08-30):
  <https://github.com/ipmitool/ipmitool>
- gofish Redfish client library (active monthly cadence):
  <https://github.com/stmcginnis/gofish>
- sushy (OpenStack Ironic Redfish library, intentionally no user mgmt):
  <https://opendev.org/openstack/sushy>
- Linux kernel IPMI subsystem (active maintenance, April 2026):
  <https://www.kernel.org/doc/html/latest/driver-api/ipmi.html>
- AMI MegaRAC SP-X product page (firmware family on Supermicro AMI):
  <https://www.ami.com/megarac/>
- Supermicro Redfish AccountService usage (URL pattern):
  <https://github.com/flaviotorres/supermicro-redfish>
- Supermicro Redfish boot-override quirk ("Continuous cleared"):
  <https://github.com/matti/superfish/blob/main/src/superfish/bmc.py>
- Supermicro OEM `FixedBootOrder` Redfish extension:
  <https://github.com/cholcombe973/libredfish/blob/main/src/supermicro.rs>
- Supermicro Update Manager (SUM) advertising Redfish Host Interface:
  <https://github.com/markwdalton/lambdalabs/blob/main/documentation/BIOS/supermicro-sum/Supermicro-SUM-usage.txt>
- TrueNAS PR documenting AMI 0xC8 = password complexity, not length:
  <https://github.com/truenas/middleware/pull/11325>
- ipmitool issue #229 (password-handling history; not the H13SSH bug
  but adjacent):
  <https://github.com/ipmitool/ipmitool/issues/229>
- ASUS Redfish Host Interface (RS720A-E12 USB descriptor, sibling SKU):
  <https://github.com/linuxhw/DMI/blob/master/Server/ASUSTek%20Computer/RS720/RS720A-E12-RS12/6002D3D7ADE4>
- CVE-2024-54085 (AMI MegaRAC Redfish authentication bypass):
  <https://thehackernews.com/2025/03/new-critical-ami-bmc-vulnerability.html>
- ironcore-dev/metal-operator (production gofish consumer with
  Supermicro/Lenovo branches):
  <https://github.com/ironcore-dev/metal-operator/blob/main/bmc/redfish.go>
- slagworks kworker-l34-1 first-deploy handoff (manual H13SSH IPMI
  reproduction, 2026-05-21):
  `docs/archeology/handoffs/2026-05-21-kworker-l34-1-first-deploy.md`
- slagworks current bmcreconcile implementation (in-band IPMI from the
  deploy agent; IPMI LAN from the site worker for `bmc_reconcile`):
  `internal/bmcreconcile/reconcile.go`
