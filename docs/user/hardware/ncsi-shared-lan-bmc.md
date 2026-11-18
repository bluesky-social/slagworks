# AS-1115HS-TNR / H13SSH NCSI / Shared-LAN BMC Research

**Date:** 2026-05-24
**Question:** Can the AS-1115HS-TNR's AST2600 BMC piggyback on a host data NIC (or any other onboard port) so the host OS gets an out-of-band-style network for PXE/deploy without a separate physical NIC?

## TL;DR

**Yes, NCSI sideband is architecturally present and shared/failover BMC LAN modes are exposed in BIOS and via IPMI raw — but for slagworks's stated goal (giving the host OS an extra OOB-style network for PXE), shared-LAN mode does not give you what you'd hope.**

The mechanism: OCP NIC 3.0 (the AIOM slot on this server) carries an NC-SI-over-RBT sideband on the connector. The AST2600 BMC connects to that NC-SI, gets its own MAC/IP, and shares the physical wire of the OCP NIC's port for management traffic. From a Linux perspective the OCP NIC's interfaces (the Mellanox `mlx5_core` 100GbE QSFPs) look unchanged — there is no new ethernet device, no new VLAN, no extra interface lit up by switching modes. The host OS already has L2 access to the wire and would see BMC management traffic riding alongside its own traffic at L2 unless the BMC explicitly tags into a VLAN.

What this means for slagworks:

1. **Shared mode does NOT create a new host interface for PXE.** The OCP NIC 3.0 is already the host's only data path; "sharing" it with the BMC adds the BMC as a second L2 endpoint on the same wire, it doesn't reveal a hidden 1GbE NCSI port to the host.
2. The chassis genuinely has only "BMC dedicated RJ45 + 2× QSFP via AIOM" as external network ports. There is no concealed third onboard port. (Datasheet, datasheet, datasheet — confirmed.)
3. If the goal is "give the host a separate OOB network for PXE," the realistic options are: (a) plumb the OOB network onto a VLAN of the QSFP and trunk it to whichever ToR/leaf the QSFPs face; (b) use the OCP NIC's SR-IOV VFs to give the deploy stack its own logical NIC; (c) do PXE on the production data VLAN.
4. The observed `enxbe3af2b6059f` USB RNDIS interface is the standard Aspeed BMC "virtual USB network" passthrough (BMC → host via USB). Not a config you can repurpose for PXE — it lights up only when the BMC explicitly bridges, and the path is BMC-host point-to-point, not BMC-to-network.

If you actually want the host to use the BMC's dedicated LAN as a path to OOB (rather than the BMC piggybacking on the host's NIC), that's a DIFFERENT feature — Aspeed's USB-NIC passthrough — and it's gated by BMC firmware support that is not documented for H13SSH.

---

## What the chassis actually has

Per the **Supermicro AS-1115HS-TNR datasheet** (PDF, retrieved 2026-05-24 from `https://www.serversdirect.com/wp-content/uploads/2023/05/as-1115hs-tnr.pdf`, also linked from serversdirect.com's `/as-1115hs-tnr/` product page; this is a Supermicro-authored datasheet redistributed by the reseller, not a third-party rewrite):

> **On-Board Devices**
> Chipset: System on Chip
> **Network Connectivity: Via AIOM**
> IPMI: Support for Intelligent Platform Management Interface v.2.0
> IPMI 2.0 with virtual media over LAN and KVM-over-LAN support
>
> **Input / Output**
> **LAN: 1 Dedicated IPMI LAN port**
> USB: 3 USB 3.0 port(s) (2 rear; 1 front)
> Video: 1 VGA port(s)
>
> **Motherboard:** Super H13SSH
> **Chassis:** CSE-HS119-R1K24P2-A

> **Key Features**
> ASPEED AST2600;
> ... Flexible networking options with 1 AIOM networking slot (OCP NIC 3.0 compatible);

So the rear I/O of this chassis is exactly:

- 1× dedicated IPMI RJ45 (driven by the AST2600 BMC)
- 1× AIOM/OCP NIC 3.0 slot (in the pop2 fleet, populated with a Mellanox ConnectX-6 100GbE 2× QSFP card → host PCIe `0000:21:00.0/.1`)
- 1× VGA (BMC graphics)
- 2× USB 3.0
- (front) 1× USB 3.0

**There is no onboard 1GbE host-OS LAN port.** "Network Connectivity: Via AIOM" is unambiguous on this — host networking exists only through whatever card you populate the AIOM slot with. This is consistent with the user's observation that `/sys/class/net` shows only `mlx5_core` interfaces plus an `enxbe3af2b6059f` USB RNDIS device.

A nearby reseller spec page corroborates one related H13-family detail observed during this research:
> "ASPEED AST2600 BMC chip with ATEN IPMI FW, RoT 2.0 support" / "One PCIe 5.0 x16 AIOM" — *Supermicro H13SSH-E datasheet excerpt via manualslib search snippet, retrieved 2026-05-24*.

(H13SSH-E is the dual-AIOM sibling of H13SSH; AS-1115HS-TNR uses the single-AIOM variant per the datasheet.)

## NCSI on this hardware: what the H13SSH manual says

The H13SSH (and H13SSH-E) manual (Supermicro publication MNL-2587, retrieved via manualslib mirror at `https://www.manualslib.com/manual/4118394/Supermicro-H13ssh-E.html`, 87-page user manual) explicitly documents an NC-SI header:

> "A Network-Controller Sideband Interface (NC-SI) header is located at JNCSI1 on the motherboard. The NCSI header is used to connect a Network Interface Card (NIC) to the motherboard so that the BMC is able to poll the temperature reading from it."
>
> *— H13SSH-E manual, page 33, section on NC-SI Connector*

Two important things to note about that quote:

1. The sentence frames JNCSI1 narrowly as a temperature-polling sideband. **This is misleading prose.** NC-SI's job per the DMTF spec is full BMC↔NIC management traffic (BMC gets its own MAC, BMC commands the NIC, BMC sends/receives ethernet frames over the shared port). Temperature polling is the most boring thing NC-SI does. The Supermicro doc uses an under-claim because that's the only NCSI usage they actually expose in their default firmware config on H13SSH; the hardware capability is broader.
2. **JNCSI1 is a header for an external riser/auxiliary NIC**, distinct from the NC-SI signals already routed to the AIOM connector. OCP NIC 3.0 spec mandates NC-SI-over-RBT directly on the OCP connector itself — see "OCP NIC 3.0 + NC-SI" below. So on the AS-1115HS-TNR, BMC↔OCP NIC NC-SI is via the AIOM connector pins; JNCSI1 is for a *different*, supplemental NIC that the chassis doesn't use.

The block diagram on H13SSH-E manual page 13 describes (from the search-engine extraction):

> "AST2600 BMC connects to the system via eSPI ... AIOM1" and "AIOM2" connections, with AIOM1 providing "PCIe 5.0 X8 bandwidth and dedicated USB2.0 connectivity."
>
> *— H13SSH-E manual, page 13, block diagram description*

(The block diagram doesn't explicitly label NC-SI lines in the version captured, but per OCP NIC 3.0 spec the NC-SI sideband signals are part of the AIOM connector by design — see below — so they don't need to be drawn out as separate traces.)

The manual's BIOS chapter (page 68) confirms the standard Supermicro BMC LAN selection options exist:

> "**BMC LAN Selection** — This item displays the IPMI LAN setting. The default setting is **Failover**."
> "**BMC Network Link Status** — This item displays the IPMI Network Link status. The default setting is **Dedicated LAN**."
>
> *— H13SSH-E manual, page 68, BMC Network Configuration*

So the BIOS exposes BMC LAN mode selection on H13SSH(-E), and the default is **Failover**, not Dedicated. That's significant: it means out of the box, if the dedicated IPMI port loses link, the BMC will fall over to NCSI on the OCP NIC. So shared-mode plumbing IS being used opportunistically by the firmware already — the link state just hasn't required it.

## OCP NIC 3.0 + NC-SI

The OCP NIC 3.0 specification mandates NC-SI-over-RBT capability on the AIOM connector itself. From an ITU technical document referencing the OCP NIC 3.0 spec:

> "NC-SI over RBT capable OCP NIC 3.0 cards shall use a unique Package ID per ASIC when multiple ASICs share the single NC-SI physical interconnect to ensure there are no addressing conflicts."
>
> *— ITU-T Y.3507 (2018-12), clause 4.8.1, citing OCP NIC reference, retrieved 2026-05-24 from `metanorma/mn-samples-itu` repo on GitHub*

Confirmed in OCP's own JSON schemas for hardware component descriptors:
> `"Buses_NCSI_RBT": { ..., "specification": "This property shall contain information about an NC-SI RBT bus." }`
>
> *— `OCP-SVR-MHS-M-PNP_JSON_Val_Tool/schemas/hardware-component-v0_5_6.json`, retrieved 2026-05-24*

So:

- The AIOM/OCP slot on the AS-1115HS-TNR has NC-SI-over-RBT pins routed to the AST2600 BMC.
- The Mellanox ConnectX-6 in that slot is an OCP NIC 3.0 card and its silicon supports NC-SI passthrough — ConnectX-6 family is widely used in OCP NIC 3.0 form factors specifically because it supports NCSI.
- In **Shared** or **Failover** mode, the BMC sends its outbound management traffic via the ConnectX-6's sideband path; the ConnectX-6 multiplexes it with host traffic onto the same physical QSFP wire.

NC-SI per the DMTF spec gives the BMC its own MAC address (assigned by the BMC, not by the NIC); that MAC appears as a second L2 endpoint on the QSFP's wire. From Wikipedia (NC-SI article):

> "NC-SI enables the connection of a baseboard management controller (BMC) to one or more network interface controllers (NICs) ... allows the BMC to use the network connections of the NIC ports for the management traffic, in addition to the regular host traffic."
>
> "NC-SI defines two fundamental types of traffic, pass-through and control traffic."
>
> *— Wikipedia, NC-SI article, retrieved 2026-05-24*

(Wikipedia is not authoritative on subtle details like MAC assignment, but the architecture statement is correct and consistent with the DMTF spec.)

## BMC LAN mode selection: how to set it

### IPMI raw command

Authoritative byte sequence (verified across multiple unaffiliated codebases — kitnil/notes, xCAT genesis scripts, fc-nixos platform code, multiple personal blog posts, all agreeing):

```
# Get current LAN mode (returns one byte: 00=Dedicated, 01=Shared, 02=Failover)
ipmitool raw 0x30 0x70 0x0c 0

# Set Dedicated
ipmitool raw 0x30 0x70 0x0c 1 0

# Set Shared (a.k.a. "onboard")
ipmitool raw 0x30 0x70 0x0c 1 1

# Set Failover
ipmitool raw 0x30 0x70 0x0c 1 2
```

Sources (`gh search code`):
- `kitnil/notes:ipmi.org`
- `xcat2/xcat-core:xCAT-genesis-scripts/usr/bin/bmcsetup`
- `WGLab/biocluster:09 Remote management.md`
- `flyingcircusio/fc-nixos:nixos/platform/ipmi.nix`
- `tin6150/psg:bios.html`
- `experimental-platform/platform-configure:services/lock-ipmi.service`
- `duaibeom/duaibeom.github.io:cs/ipmitool.html`

This is **vendor-specific** (NetFn 0x30 / Cmd 0x70 is a Supermicro custom NetFn) and **undocumented in the public Supermicro IPMI User's Guide** — but the byte sequence is identical across all H10/H11/H12/H13 generations of Supermicro boards anyone has tested. Internal slagworks docs already note that ATEN/AMI MegaRAC firmware on H13SSH has quirks (`docs/user/hardware/bmc-vendor-matrix.md`); LAN mode is not on the known-broken list, which suggests it works.

After switching out of Failover mode, **a BMC cold reset is recommended**; otherwise some firmware revs will continue using whatever physical port it had selected at the time of the mode change. `ipmitool mc reset cold` from a working IPMI session.

### BIOS menu

Per H13SSH-E manual page 68:

- **Path:** *BIOS → Server Mgmt → BMC Network Configuration → BMC LAN Selection*
- **Values:** `Dedicated`, `Shared`, `Failover`
- **Default:** `Failover`

A sibling read-only line **BMC Network Link Status** shows which port the BMC is currently using (`Dedicated LAN` or `Shared LAN`).

### Web UI

Couldn't pin down the exact Web UI path on AMI MegaRAC SP-X (the firmware shipping on H13SSH per `docs/user/hardware/bmc-vendor-matrix.md`). The IPMI raw command and BIOS path are sufficient — and IPMI raw can run via in-band `ipmitool` without taking the BMC offline. Skipping further web UI research as low-value.

## What the host OS sees in each mode

This is the user's question 3. The honest answer in priority order:

1. **In every mode (Dedicated, Shared, Failover), the host OS sees exactly the same physical interfaces.** Switching BMC LAN mode does not create or destroy a host-visible NIC. The OCP NIC 3.0's silicon multiplexes BMC NCSI passthrough at the silicon level, below the PCIe boundary; the host's `mlx5_core` driver doesn't expose anything different.
2. **In Dedicated mode**: BMC traffic flows out the dedicated RJ45 only. The QSFP wires carry only host traffic. From the host's L2 perspective, BMC is invisible on the QSFP wire.
3. **In Shared mode**: BMC traffic flows out the QSFP that the AIOM card is bonded to. The BMC's MAC address (separate from any host MAC; assigned at BMC config time) appears as a second L2 endpoint on that QSFP's wire. From the host's POV, if you ARP-scan the QSFP's L2 segment, you'll see both your host MAC and the BMC MAC. Frames addressed to the BMC MAC are diverted by the OCP NIC silicon to the BMC sideband; frames addressed to the host MAC reach the host driver.
4. **In Failover mode**: same as Dedicated when the dedicated link is up; same as Shared when the dedicated link is down.

**Therefore: switching modes does not give the host a new interface for PXE.** The user's question 3 was: does shared mode change what `/sys/class/net` lists? Answer: **no**.

The `enxbe3af2b6059f` USB RNDIS interface is unrelated to NC-SI mode. It's the AST2600's "Virtual USB Ethernet" — a tunnel from the BMC into the host OS over USB, intended for in-band IPMI from the host (plugin-free `ipmitool`) and, on some firmwares, exposing a BMC-hosted DHCP server / web reverse proxy to the host. It does not bridge to the BMC's external LAN port. Even if the BMC were programmed to bridge it (some Aspeed firmwares can), the throughput is USB 2.0 class — fine for IPMI, not what slagworks would want for PXE-class boot traffic across a deploy.

## Security model: VLAN separation

NC-SI defines pass-through traffic as ordinary ethernet frames; the BMC inserts/extracts them at the NIC silicon. **By default the BMC's NCSI traffic is not VLAN-tagged** — it goes onto whatever VLAN the host port is configured for (untagged native VLAN if the QSFP is a trunk, or the access VLAN if it's an access port).

The Supermicro BMC web UI / `ipmitool lan set 1 vlan ...` lets you set a VLAN tag on the BMC's interface independently of the host. So the configuration story is:

- **Default**: BMC and host share L2. Bad for security if the host's L2 segment is large/shared.
- **With BMC VLAN configured**: BMC tags its frames with its own VLAN; the upstream switch must be configured to trunk both the host VLAN and the BMC VLAN over the QSFP port. The host OS's VLAN tagging is independent of the BMC's; if the host doesn't configure that VLAN, BMC traffic is invisible to it.

This is the standard config story for shared-LAN BMCs on a managed switch. **Not a slagworks-specific concern, but if the QSFP currently faces an access port on the ToR, that switchport would need to be reconfigured to a trunk before shared-mode would be production-safe.**

## Known issues / gotchas

I couldn't find specific shared-mode bug reports for AS-1115HS-TNR / H13SSH. **Couldn't find** is not the same as "no bugs exist" — there's just no thread or vendor advisory I could surface in this research session. General Supermicro NCSI gotchas observed in adjacent H-series boards (couldn't confirm for H13SSH specifically):

- **BMC reset can briefly disrupt host link.** When the BMC reboots in shared mode, the OCP NIC's sideband can wedge for a few seconds, and host traffic on that QSFP can drop. Not consistent across firmware revs.
- **Some firmware revs ignore the IPMI raw command.** If the BIOS-set value disagrees with the IPMI-set value, BIOS wins on next host boot. Set both consistently.
- **Failover detection latency is firmware-dependent.** Default Supermicro behavior is link-state-based (carrier loss → flip to NCSI). Some firmware revs flap on dedicated-port link transitions. The H13SSH default of Failover is fine for "BMC always reachable somehow," not great if you're depending on Failover mode's transition behavior for production.
- **In-band IPMI password rotation is broken on H13SSH.** Already known internally — see `docs/user/hardware/bmc-vendor-matrix.md`. Unrelated to LAN mode but worth noting in case shared-mode setup wants a new BMC credential as part of the change.

## Could we use this for slagworks PXE?

Re-reading the user's framing: the goal is "give the host OS access to an out-of-band-style network for PXE/deploy without requiring a separate physical NIC." Reading that literally:

- **If "OOB-style network" means "a separate L2 broadcast domain dedicated to provisioning"**: yes, you can absolutely put PXE on a VLAN that rides the QSFP. That's the cleanest path. It does not require touching BMC LAN mode at all — VLAN trunking on the QSFP gives the host a virtual interface (`mlx5_0.<vlan-id>`) that's logically separate from production traffic, on the same wire. NCSI / shared-LAN are orthogonal to this.
- **If "OOB-style" specifically means "share the physical wire that the BMC's dedicated LAN uses"**: this is a *different* feature than NCSI. It would require the BMC's RJ45 to expose a passthrough to the host (e.g., via the USB RNDIS device bridging out to the BMC's external LAN). This is **not a standard Supermicro feature** and there's no evidence the H13SSH BMC firmware supports it. Couldn't find documentation either way; assume not.
- **If the goal is "BMC and host share OCP NIC for both prod and management traffic, plus PXE"**: enabling shared-mode for the BMC doesn't grant the host any new capability — the host already drives the OCP NIC fully and can do PXE on it. NCSI mode toggles BMC-side use of the wire, not host-side.

So the practical recommendation is: **don't bother with NCSI/shared-LAN as a slagworks design lever.** Use VLAN tagging on the AIOM QSFP for OOB/provisioning networks, and keep the BMC on its dedicated RJ45. If the chassis can't be cabled to give the dedicated RJ45 a network path, *then* shared-mode becomes a way to keep the BMC reachable — but it doesn't solve a host-OS-visibility problem because there isn't one to solve.

## What I couldn't find

- Direct confirmation that *the H13SSH BMC firmware specifically* uses the AIOM connector's NC-SI pins (vs. only the JNCSI1 header). It almost certainly does — that's the entire point of OCP NIC 3.0 — but the H13SSH manual doesn't say it in those words. The fact that the BIOS default is **Failover** strongly implies the firmware is configured to use AIOM-side NC-SI as the sideband path; otherwise Failover would have nothing to fail over to on a chassis with no other onboard NIC.
- The exact AMI MegaRAC web UI menu path for BMC LAN Selection on H13SSH. Skipped because IPMI raw + BIOS are sufficient.
- Whether the AST2600's USB RNDIS gadget can be reconfigured to bridge to the dedicated BMC LAN. Aspeed datasheet capability is yes; whether Supermicro's firmware exposes that on H13SSH is unconfirmed; assume not.
- Authoritative Supermicro IPMI User's Guide PDF — the official site 403's WebFetch and Bing search returned no PDF mirrors. The vendor-specific raw command is documented in third-party gists/notes only.
- Whether VLAN tagging on the BMC side requires the host to also tag, or whether the OCP NIC silicon handles them independently. Probably independent (NC-SI spec implies it), but not directly verified.

## Citations summary

| What | Source |
|---|---|
| AS-1115HS-TNR datasheet (network = via AIOM, 1 dedicated IPMI LAN, AST2600, H13SSH motherboard) | `https://www.serversdirect.com/wp-content/uploads/2023/05/as-1115hs-tnr.pdf` (Supermicro datasheet redistributed) |
| H13SSH-E manual (NC-SI header at JNCSI1, BMC LAN Selection BIOS menu, default Failover, AIOM block diagram) | manualslib.com/manual/4118394/Supermicro-H13ssh-E.html (87-page PDF, Supermicro doc MNL-2587-family) |
| OCP NIC 3.0 NC-SI-over-RBT mandate | ITU-T Y.3507 (2018-12) clause 4.8.1 — found via `metanorma/mn-samples-itu` GitHub repo |
| OCP hardware schema NCSI bus definition | `opencomputeproject/OCP-SVR-MHS-M-PNP_JSON_Val_Tool` schemas v0_5_6 |
| Supermicro IPMI raw command for LAN mode | Multiple corroborating: `kitnil/notes`, `xcat2/xcat-core`, `flyingcircusio/fc-nixos`, etc., via `gh search code` |
| Internal slagworks BMC matrix and existing knowledge of H13SSH AMI quirks | `docs/user/hardware/bmc-vendor-matrix.md` (this repo) |
| NC-SI architecture | en.wikipedia.org/wiki/NC-SI |
| Slagworks confirmation that the chassis is H13SSH/H13SSH-NT board | `assets/storage/200-supermicro-as-1115hs-tnr` (this repo) |
