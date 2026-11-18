# Dell PowerEdge R640 — slagworks imaging learnings

What it took to image a Dell PowerEdge R640 with slagworks for the first time
(the pop2 new-cage oob-gws, `oob-gw-3/4.pop2`, June 2026). Every item below is a
trap we hit live, with the fix and a commit link. Read this before imaging any
new Dell — most of these are Dell/iDRAC-generic, not pop2-specific.

Commit links are `github.com/bluesky-social/slagworks/commit/<sha>`.

## Hardware shape (R640 as we have it)

- **Boot disks:** 2× SAS SSD behind an **LSI SAS3008 HBA** (not a hardware-RAID
  PERC) — they pass through as raw `sda`/`sdb`, both report `transport=sas`. On
  ours: a Samsung `MZ7LM` + a Micron `MTFDDAK`, ~894 GB each.
- **NICs:** 4 on-board ports across two controllers — an Intel **X710** 10/25G
  SFP+ (`eno1np0`/`eno2np1`, "Integrated NIC 1/2", PCI bus `19:00`) and an Intel
  **I350** 1G (`eno3`/`eno4`, "Integrated NIC 3/4", bus `01:00`), plus a ConnectX
  add-in (`enp94s0f*`). The SMBIOS port label is in
  `/sys/class/net/<nic>/device/label` (`NIC1`..`NIC4`) — the ground truth for
  mapping a kernel name to the BIOS's "Integrated NIC N".
- **Firmware:** UEFI boot, BIOS 2.24.0, iDRAC 7.x. SOL is COM2 = `ttyS1,115200`.
- **BMC:** iDRAC, reachable over IPMI lanplus and Redfish — but see the cred trap.

## The traps (in the order a deploy hits them)

### 1. BMC creds: an `ipmitool`-minted user has no Redfish role

If you create the slagworks BMC user with `ipmitool user set` (because racadm
isn't installed), it gets IPMI-LAN access but **not** an iDRAC-web/Redfish role —
Redfish auth returns 401. That's mostly fine (power/SOL work over IPMI), but it
breaks the commission **chassis-serial check**: bmclib's ipmitool provider is not
an `InventoryGetter`, so `ChassisSerial()`'s `Inventory()` call fails with "no
InventoryGetter implementations found" and **every commission/deploy aborts**
before it can power-cycle the box.

Also: **IPMI-over-LAN is globally disabled** on a fresh iDRAC. Setting the user's
privilege/flags is not enough for RMCP+; you must set the channel access mode:

```
ipmitool raw 0x06 0x40 0x01 0x82 0x84   # channel 1: always-available + per-msg auth
```

Full per-host mint sequence (channel 1, ADMINISTRATOR, slot 3):

```
ipmitool user set name 3 slagworks
ipmitool user set password 3 <pw>
ipmitool user priv 3 4 1
ipmitool user enable 3
ipmitool sol payload enable 1 3
ipmitool channel setaccess 1 3 callin=on ipmi=on link=on privilege=4
ipmitool raw 0x06 0x40 0x01 0x82 0x84
```

Cipher 3, 17, and default all work; slagworks' default (3) is fine.

**Fix:** `ChassisSerial()` falls back to `ipmitool fru print 0` (the FRU **Product
Serial** == NetBox `device.serial`) when bmclib hits the InventoryGetter gap. The
serial safety gate stays intact; it just works over the IPMI channel we already
have. — [`edd2497`](https://github.com/bluesky-social/slagworks/commit/edd249772407938ab1e11318faf3004bd48d66c3)

**Better path (not yet done):** the raw-byte mint sequence above is fragile
and leaves the user without a Redfish role. The oob-gws now carry `racadm`
in-band (installed by host configuration management, see below), so a future
change should mint the
user + enable IPMI-over-LAN + grant a proper iDRAC role via `racadm` (Redfish
where racadm isn't available) — then the FRU fallback becomes belt-and-suspenders
rather than load-bearing. Don't reach for `ipmitool raw` if racadm can do it.

### 2. BIOS PXE-boots the wrong NIC

The iDRAC requests PXE, but the BIOS attempts it on **`NIC.Integrated.1-1-1`**
(the X710, "Integrated NIC 1 Port 1") — which on our oob-gws is the WAN/public
leg, not the OOB segment slagworks serves DHCP on. Result: `PXE-E18: Server
response timeout`, then disk fallback. **IPMI `bootdev pxe` cannot pick *which*
NIC** — the BIOS uses its single configured `PxeDev1Interface`.

**Fix (per-host, persists):** point the UEFI PXE device at the OOB NIC with
racadm, then create a staged BIOS config job (applies on next reboot — which the
commission/deploy power-cycle provides):

```
racadm get BIOS.PxeDev1Settings.PxeDev1Interface          # see current (e.g. NIC.Integrated.1-1-1)
racadm set BIOS.PxeDev1Settings.PxeDev1Interface NIC.Integrated.1-3-1   # eno3 = "NIC 3" = I350/OOB
racadm jobqueue create BIOS.Setup.1-1                     # stage; applies on reboot
```

Map the NIC FQDD with `/sys/class/net/eno3/device/label` (→ `NIC3` →
`NIC.Integrated.1-3-1`). `racadm help BIOS.PxeDev1Settings.PxeDev1Interface`
lists the legal FQDDs.

**Installing racadm:** add Dell's OMSA apt repo (pinned to `jammy`; OMSA ships
no `noble` dist) and install `srvadmin-idracadm8`, which pulls the whole closure
(`idracadm7` -> `hapi` -> `pciutils`, plus `libargtable2-0` from Ubuntu universe)
via apt. No manual `dpkg -i`. The actual binary is
`/opt/dell/srvadmin/bin/idracadm7` (idracadm8 ships no separate binary; the
`racadm` symlink is non-exec — call idracadm7 directly). It works **in-band**
(KCS) as root — no Redfish/network auth. (Historical manual path, if ever needed
off-fleet: grab the jammy debs `srvadmin-idracadm7` + `srvadmin-hapi` +
`srvadmin-omilcore` from `linux.dell.com/repo/community/openmanage/11010/jammy/`
plus `libargtable2-0` and `dpkg -i` — they run on noble.)

### 3. Storage: factory DDF software-RAID holds the boot disk

Dell ships these with **DDF (external-metadata) software-RAID** on the SAS boot
disk even behind a plain HBA: a `external:ddf` container (`md127`) + an inactive
subarray (`md126`), both auto-assembled at initrd start and holding `sda` **open**.
The agent's `mdadm --stop --scan` does **not** reliably tear external containers
down (mdmon monitors them; a subarray must stop before its container). The
symptom is later, at `blockdev --rereadpt`: `BLKRRPART: Device or resource busy`,
FATAL in the storage phase, even though `wipefs`/`sgdisk` appeared to succeed.

The key nuance: `wipefs`/`sgdisk` genuinely **do** succeed — the busy isn't
surviving metadata, it's that the auto-assembled `md127` still has `sda` open, so
the kernel refuses the `BLKRRPART` partition-table re-read. (Same class as the
`AUTO -ddf -imsm` mdadm.conf knob that exists precisely to stop udev
auto-assembling these external containers.)

**Fix:** after `--stop --scan`, parse `/proc/mdstat` and explicitly `mdadm --stop`
every residual md device, **members/subarrays before containers**, so the disk is
released before partitioning. — [`b76c803`](https://github.com/bluesky-social/slagworks/commit/b76c803737e5db0ccb3176cae56b7007dc489f77)

**This is factory/pre-OS state, not a lingering condition.** DDF is what a
stock R640 (or one whose PERC/BIOS RAID config was reset) carries *before* a
successful native install; once a host is imaged with a native `super 1.2`
mirror the container is gone. A 2026-06 read-only sweep of all 7 oob-gws
(`/proc/mdstat` + `mdadm --examine`) found **zero DDF anywhere** — every running
host shows a clean native raid1. So DDF is a gremlin on the *path* to a working
install (almost certainly the explanation for historical "disk busy" install
failures on these boxes), not something to expect on a healthy host. The latent
risk is any factory-fresh unit, a disk swap, or a manual RAID-mode flip.

### 4. Storage: `blockdev --rereadpt` races udev (and you can't `settle` it away)

Even after the DDF teardown, `blockdev --rereadpt /dev/sda` fails EBUSY: right
after `sgdisk` repartitions, **systemd-udevd opens the disk to probe the new
partitions**, and BLKRRPART contends with that open. The obvious fix —
`udevadm settle` — **does not work**, because the slagworks agent runs inside a
chroot where settle no-ops ("Running in chroot, ignoring request"). Proven
interactively: bare rereadpt failed post-sgdisk, then succeeded "after 2 retries"
with a 1s backoff.

**Fix:** the agent's storage executor **retries only `blockdev --rereadpt`** on
failure (bounded, 15×@1s); every other storage command stays fail-fast so a real
error isn't masked. — [`b9a0b30`](https://github.com/bluesky-social/slagworks/commit/b9a0b30c5253b10e539273ca0e219263eaf069a9)

### 5. Network: the oob-gw mgmt NIC's gateway resolves empty

slagworks resolves a per-interface `gateway` only from **`slagworks-production`**
prefixes. An oob-gw's management NIC sits in the **`slagworks-oob`** prefix, whose
`gateway_ip` custom field the per-interface resolver never consults — so the
network-config script receives an empty `gateway` even though the OOB prefix has
one. A netscript that hard-requires a gateway then fails at "applying network
config".

**Fix:** the Dell oob-gw netscript treats the default route as **optional** —
emit it iff a gateway is resolved, else bring the NIC up address-only. Correct
here: phone-home targets the site worker on the *same* OOB /23 (directly
connected, no route needed), and FRR plus the host's configuration management own
the real default route post-boot anyway. — [`dc08c57`](https://github.com/bluesky-social/slagworks/commit/dc08c578ea0d44d5b0f7e43571b6e4270b6bb069)
(assets-only; re-upload with `slagctl network-script delete <id> && upload`.)

### 6. The config scripts themselves

- **Storage** (`assets/storage/150-dell-r640`): matches `system_product=PowerEdge
  R640`; selects boot disks by `transport=sas` (both report sas behind the HBA);
  RAID1 mirror, UEFI dual-ESP, no data array; bare `sdaN` partition suffix.
- **Network** (`assets/network/150-pop2-newcage-oob-gw`): hostname-pinned
  (`oob-gw-[34]\.pop2`) because **all four pop2 oob-gws are device_type "PowerEdge
  R640"** and a netscript can't match on location. Brings up only the OOB mgmt NIC
  (eno3), discriminating it from the public WAN leg by **RFC1918 IP** and from the
  iDRAC port by name.
- — [`66c344b`](https://github.com/bluesky-social/slagworks/commit/66c344b2c320420d73a059a9c4e6413e7171bacd)

## Other gotchas (not R640-specific, but bit us here)

- **Fresh-install postinst:** the `slagworks` deb shipped `/var/lib/slagworks`
  but not its `images/`/`data/` subdirs, so the postinst's EFI symlink failed and
  the first service start crashed on `open local db: ... out of memory (14)`
  (SQLite couldn't create its lease DB). Fixed to create them symlink-safely. —
  [`a91a8e0`](https://github.com/bluesky-social/slagworks/commit/a91a8e0f51e13bcad064498d817b5a25176564ea)
- **Duplicate device-types:** NetBox had **two** "PowerEdge R640" device-types
  (ids 24 and 59); `slagworks_kernel_params` set on the wrong one fails preflight
  with "no kernel_params". Check `device.device_type.id`, set the CF on the one
  the host actually uses. (Same trap as the leaf-6 MSN2700 dup.) Value used:
  `console=tty0 console=ttyS1,115200n8`.
- **`primary_ip4` required:** commission preflight refuses a device with no
  `primary_ip4`. Set it to the OOB mgmt IP (the canonical management address).
- **iDRAC mgmt_only:** the BMC interface (`ipmi0`) must be `mgmt_only=true` in
  NetBox, and the device's canonical `oob_ip` must point to its assigned IP.
  `oob_ip` establishes that the device has a BMC; `mgmt_only` then keeps the
  BMC interface out of host DHCP identity. BMC-less devices may correctly use
  `mgmt_only=true` on their host management/PXE NICs.
- **iDRAC IPMI session exhaustion:** AMI/iDRAC firmware has ~5 session slots;
  rapid SOL + power calls (plus slagworks' castpump) exhaust them and you get
  "Unable to establish IPMI v2 / RMCP+ session" / "Set Session Privilege Level
  failed: 0x81". It drains on its own in a minute or two; don't rapid-retry.

## End-to-end sequence that worked

Model in NetBox (serial, primary_ip4, ipmi0 mgmt_only, kernel_params on the right
device-type, OOB+production prefixes tagged/scoped) → mint iDRAC creds (with the
`raw 0x06 0x40` channel unlock) → install racadm + set `PxeDev1Interface` to the
OOB NIC + stage the BIOS job → upload the storage + network scripts to global →
`slagctl machine commission` (FRU-fallback serial check) → `slagctl machine deploy
--destructive` (DDF teardown + rereadpt retry clear the storage phase;
gateway-optional netscript applies) → reboot to disk → phone home. Then
`slagctl machine pxe-disable` so it boots from disk, and let configuration
management converge the full host config (keepalived/VPN/FRR/netplan).
