# Boot Process

Status: live (generated from code, 2026-07-03)

The end-to-end boot chain a machine walks through the first time slagworks
touches it — from the firmware's initial DHCP broadcast to the deployed OS
phoning home. Every stage is implemented in this tree; the file paths cited
next to each claim are the source of truth.

## The chain, at a glance

1. Firmware powers on and PXE-broadcasts DHCP.
2. `slagworksd` (site worker) answers with an IP, a bootfile pointer, and
   its own address as `next-server`.
3. The firmware fetches the appropriate iPXE binary (over HTTP for UEFI
   HTTP-boot firmware, over TFTP for everything else) and executes it.
4. iPXE runs the embedded script (`assets/ipxe/embed.ipxe`), DHCPs again as
   itself, and chains an HTTP URL for a per-machine iPXE script.
5. The HTTP handler decides what this machine should do next — boot local,
   deploy, or commission — and emits an iPXE script that either exits to the
   firmware (or `sanboot`s the local disk) or downloads a kernel + initrd.
6. On a deploy/commission path, the kernel boots with `boot=slagworks`. Stock
   Ubuntu `/init` runs, hands off to the slagworks BOOT hook, which mounts
   the deploy squashfs as an overlay root and execs the `slagworks-agent`
   binary inside a chroot.
7. The agent partitions disks, rsyncs the OS onto them, installs a
   bootloader, seeds config, reports `deploy_complete`, and reboots.
8. The next PXE cycle sees the machine in `awaiting_phone_home` and steers
   it to a local disk boot. The freshly-installed OS starts, a
   `slagworks-phone-home.service` unit POSTs to the site worker, and the
   op transitions to `completed`.

The rest of this document expands each step.

## Stage 1: DHCP

`slagworksd` binds one or more interfaces (fabric leg to a leaf switch, OOB
interface on the management network, or both) and serves DHCP directly from
`internal/dhcp/handler.go`. The multi-interface bind is real: each interface
gets its own `IfaceParams` (server IP, DNS, NTP, pool, ARP prober) via
`Handler.SetIfaceParams`, while the lease table, `KnownMACs`, `ReserveIP`,
and `BootEligible` are shared across all interfaces
(`internal/dhcp/handler.go:120-155`). The arrival interface is the sole
disambiguator for a direct broadcast, so the socket layer threads it in via
`HandlePacketOn(req, iface)`.

Several packet-level details are worth calling out because they gate whether
a machine ever gets booted:

- **Client architecture (option 93).** `tftpBootFilename`
  (`internal/dhcp/handler.go:818`) picks `snponly.efi` for
  `EFI_BC/EFI_X86_64/EFI_IA32` and `undionly.kpxe` for `INTEL_X86PC`. An
  unrecognized value falls through to UEFI with a warn log.
- **HTTP-boot firmware (option 60 `ClassIdentifier`).** If it starts with
  `HTTPClient`, the reply serves the iPXE binary over HTTP
  (`http://<server>:<port>/files/ipxe/<name>`) instead of TFTP, and echoes
  option 60 back as required by the UEFI spec
  (`internal/dhcp/handler.go:760-807`). The Supermicro firmware in the
  target fleet needs the HTTP path — TFTP of the ~1MB binary times out on
  those boards.
- **iPXE user class.** `embed.ipxe` sets `user-class slagworks-ipxe` before
  its first `dhcp` call. `isIPXEClient` detects it
  (`internal/dhcp/handler.go:912`) and returns an HTTP URL to the per-machine
  boot script rather than an iPXE binary. A generic iPXE ROM (e.g. QEMU's
  built-in, which sends `iPXE`) does NOT match this check and gets the TFTP
  filename so it chainloads the slagworks iPXE first.
- **Reservations vs. dynamic pool.** `ReserveIP(mac)` fires before dynamic
  allocation (`internal/dhcp/handler.go:944`). A pool with a nil
  `RangeStart` (reservations-only segment) still serves the full reservation
  set; a non-reserved MAC on such a segment gets no offer, which is what
  lets a slice-owning peer answer in an active/active split-pool cage. A
  reservation also wins over another MAC's live lease on the same address
  (the address moved to a new MAC in NetBox, or the reservation sits inside
  the dynamic pool and a dynamic client drew it first): the handler revokes
  that lease with a warning, and the other MAC is NAKed at its next renew
  and re-DISCOVERs onto a different address. Until that renew, both devices
  answer for the address on the wire.
- **Address conflicts (DHCPDECLINE).** A client that checks its new address
  for duplicates before using it (dhcpcd in the initrd, for instance) sends a
  DECLINE when another host already answers for it, then starts over at
  DISCOVER. `handleDecline` drops that lease and withholds the address from
  dynamic allocation for one lease duration, so the retry gets a different
  address, and logs a warning with the declined address: find and remove the
  other host using it. A static reservation is still offered to its MAC, so a
  declined reservation keeps failing until the conflict is resolved.
- **Client requests only.** A packet whose BOOTP op is not BOOTREQUEST is
  dropped on every segment, whatever its option 53 says (RFC 2131), and
  counted under the `not_bootrequest` drop reason.
- **Known-MAC filter.** When a segment is non-promiscuous and `KnownMACs` is
  set, an unregistered MAC's DHCP is silently dropped at debug level
  (`handler.go:424-428`). A promiscuous segment (typical for OOB where you
  want to serve unknown BMCs and PDUs) bypasses the filter.
- **Boot eligibility.** `BootEligible(mac)` decides whether a reply carries
  a `BootFileName` at all. Segment-agnostic and per-MAC: a promiscuous OOB
  interface will happily hand an idle BMC an address, but with no bootfile,
  because no active booting op targets it. `bootFileFor`
  (`internal/dhcp/handler.go:799`) is where this gate lives.
- **HA / active-active split-pool.** Two site workers sharing one segment
  can each own a slice of the dynamic pool
  (`IfaceParams.DynamicSlices`). On a broadcast REQUEST with no server-id
  (INIT-REBOOT / REBINDING), if the client is trying to keep an address in
  a peer's slice — or outside the subnet entirely — the local worker NAKs
  it to force a re-DISCOVER onto whichever slice-owner picks it up
  (`splitPoolNAK`, `handler.go:680`). Reservations are unsliced and are
  ACKed by either worker. The fabric ephemeral range and each relay pool's
  range are split the same way without operator data: the snapshot lists
  the workers registered to the site config (`DHCPConfig.Workers`), and
  each worker allocates only from its share by position in that list
  (`PoolConfig.WorkerShare`). Each worker keeps its own lease table and
  cannot ARP-probe a relayed subnet, so without the split two workers
  answering one relayed DISCOVER stream could lease one address to two
  hosts. A /31 relay pool is not split: its one client address comes from
  `giaddr`.
- **On-ACK phase advance.** `OnACK(mac, client)` fires after a DHCPACK with
  the kind of client it went to; the site worker uses it to promote a
  booting op's phase to `dhcp_ack` for PXE or HTTP-boot firmware and
  `ipxe_dhcp` for slagworks iPXE after the chainload
  (`cmd/slagworksd/site.go`). An ACK to any other client, such as the
  deploy environment's own dhcpcd (stage 5), leaves the phase alone.
- **Lease persistence.** When a `LeaseStore` is wired, leases are saved on
  every ACK and reloaded on startup with expired entries pruned
  (`handler.go:238-258`, `handler.go:1033-1041`).

## Stage 2: iPXE binary served

The DHCP `next-server` is the site worker itself. Two paths from here:

- **UEFI HTTP-boot.** Firmware fetches `snponly.efi` (or matching arch)
  over HTTP via the `/files/ipxe/` route
  (`internal/httpboot/handler.go:1000-1009`).
- **Everything else.** Firmware TFTPs the binary from the site worker's
  TFTP server (`internal/tftp/server.go`). TFTP block size is pinned at
  1408 with negotiation disabled (`server.go:107-124`) — the default 1468
  puts the frame at exactly 1500 bytes with no headroom for VLAN tags or
  other encapsulation, so a single hop with a tag silently drops the
  oversized data block and the transfer stalls. Optional `KnownIPs`
  filtering only serves clients with an active DHCP lease from this
  server.

Both binaries — `snponly.efi` (UEFI) and `undionly.kpxe` (legacy BIOS) —
live in `assets/ipxe/`. They're built from upstream iPXE (pinned in
`assets/ipxe/UPSTREAM-REF`) with `embed.ipxe` baked in via
`.github/workflows/build-ipxe.yaml`. Rebuilding either binary is required
whenever `embed.ipxe` changes.

## Stage 3: the embedded iPXE script

`assets/ipxe/embed.ipxe` is short — about fifty lines — and mirrors the
canonical iPXE retry idiom (`aws.ipxe` in the iPXE tree). Behavior:

- Set `user-class slagworks-ipxe` so the DHCP handler recognizes it.
- Run a `:retry` loop bounded to 20 attempts (`~5min` worst case).
- Each attempt: `dhcp --timeout 10000` (10s bound), then
  `chain --timeout 15000 ${filename}?mac=${mac}&uuid=${uuid}&platform=${platform}`.
- On chain success, `goto done` and exit.
- On chain failure, fall through `:next`, increment the attempt counter, and
  `goto retry`. A chain miss re-DHCPs from scratch — cheap because the lease
  is short and the loop is idempotent.
- After 20 misses, print a failure banner and drop to the iPXE shell rather
  than looping silently forever.

Note the query params on the chain URL: `mac=${mac}` is the interface that
DHCP'd (not `net0/mac`, which would misroute if the machine booted on `net1`
because `net0` was linkdown); `uuid=${uuid}` and
`platform=${platform}` carry through so the HTTP handler can select the
right per-machine boot script and the right BIOS-vs-UEFI local-boot idiom.

## Stage 4: the HTTP boot handler

`internal/httpboot/handler.go` at path `/boot/ipxe` is where the per-machine
policy lives. `Handler.ServeHTTP`
(`handler.go:139-220`) looks up the MAC's boot action via
`MachineProvider.BootActionForMAC`. The four outcomes:

- **`BootLocal`.** The machine is deployed and running (or otherwise not
  driving an op). Return `bootLocalScript` — split by firmware platform.
  - `platform=pcbios` (legacy BIOS): the script is a bare `exit`. That
    hands control back to the firmware, which proceeds to the next BBS
    boot device — the local disk — and INT13h-boots it directly. We do
    NOT use `sanboot --drive 0x80` here: on AMI Aptio firmware
    (documented in `handler.go:227-244`) `sanboot` prints "Booting from
    SAN device 0x80" and hangs.
  - `platform=efi` or absent: the script loops
    `sanboot --no-describe --drive ${drive}` over drives `0x80` through
    `0x9f` and boots the first whose ESP holds `\EFI\BOOT\BOOTX64.EFI`.
    `sanboot` under UEFI recursively `ConnectController`s the disk's FAT
    driver and directly `LoadImage`s that file, which is the
    iPXE-maintainer-recommended UEFI local-boot idiom. iPXE numbers whole
    local disks from `0x80` in the firmware's block-device order, and a
    storage script may put the ESP on any disk, so the script walks 32
    of them rather than the first two. When none boots it ends with a
    plain `exit`: status 0 makes `embed.ipxe`'s chain succeed and iPXE
    exits to the firmware with `EFI_SUCCESS`. Firmware that continues its
    boot order then reaches the `Slagworks` Boot#### entry the agent
    registered; EDK2-based firmware with a boot menu (OVMF) shows that
    menu instead, which the UEFI spec allows after a successful return. A
    nonzero status would fail the chain, which `embed.ipxe` retries 20
    times before dropping to an iPXE shell.
- **`BootCommission`.** Emits `commissionBootScript` — kernel + initrd from
  the configured image, with `slagworks.mode=commission` on the cmdline.
- **`BootDeploy`.** Emits `deployBootScript` — same image, with
  `slagworks.mode=deploy` (the default when unset) plus the op's UUID,
  machine UUID, trace ID, and the per-op parameters
  (`destructive`, `wipe_all_disks`, `recovery`, `tailscale_authkey`, etc.).
  If the machine has never been commissioned (no `SystemUUID`), the
  handler refuses to serve a deploy script and returns an iPXE `shell`
  drop instead (`handler.go:441-444`). It also refuses a deploy whose
  operation params it cannot decode, rather than booting the installer
  without them (which would drop `recovery` and every other
  params-derived argument). It refuses the same way when a
  value it would put on the kernel line as one `key=value` argument (the
  tailnet auth key, the system UUID, the test-only knobs) contains a
  space, a control or non-ASCII byte, `"`, `\` or `${`, any of which
  would add kernel arguments or iPXE commands; the error names the
  argument, and the site worker log says why. A tailnet hostname that is
  not a valid hostname is not refused: the handler leaves out both
  tailnet arguments and the deploy goes ahead without tailnet enrollment
  (global does not mint a key for such a name in the first place).
  Every kernel boot (deploy, commission, kernel-only) likewise refuses a
  `slagworks_kernel_params` value iPXE would read as more than kernel
  arguments (see `hardware/adding-new-hardware.md`), and a command line
  longer than the 2047 bytes the kernel keeps: the kernel drops the rest
  without failing the boot, and the end of the line holds what the
  installer cannot run without (`slagworks.expect-uuid` on a deploy). The
  length counts each iPXE setting at its longest and the image name iPXE
  puts in front of the line for an EFI kernel.
Each generated script sets `BOOTIF=01-${net0/mac:hexhyp}` and `ip=dhcp` so
stock Ubuntu `/init` (via `configure_networking`) knows which NIC iPXE
chainloaded on and will DHCP on the correct port. `boot=slagworks` is the
BOOT-hook selector; `slagworks.server=` and `slagworks.image=` tell the
initrd hook where to fetch the squashfs from.

The handler also fires an `ipxe_loaded` progress report for the active
boot-driving op the moment the script goes out (`handler.go:181-196`), so
the watch TUI's lifecycle pane advances at HTTP fetch time rather than
waiting on the agent's first envelope.

The same HTTP server hosts a set of per-machine endpoints the agent
consults during a deploy (`handler.go:897-921`):

- `/api/v1/machines/{id}/storage-script`
- `/api/v1/machines/{id}/kernel-params`
- `/api/v1/machines/{id}/network-script`
- `/api/v1/machines/{id}/network-facts`
- `/api/v1/machines/{id}/hostname`
- `/api/v1/machines/{id}/storage-script` (also mirrored at
  `/boot/storage-script?mac=...` for the agent's convenience)
- `/boot/ssh-keys` — operator authorized_keys the agent writes into the
  deployed OS
- `/files/images/<image>/{vmlinuz,initrd,root.squashfs}` — the image tree
- `/files/bin/slagworks-agent` — the agent binary (see the images doc for
  where this comes from; it is NOT part of the image)
- `/files/bin/systemd-bootx64.efi` — the ESP bootloader binary. Not
  part of the image: the `slagworks` deb depends on Ubuntu's
  `systemd-boot-efi` package and the postinstall symlinks
  `/usr/lib/systemd/boot/efi/systemd-bootx64.efi` into the worker's
  images dir; the site readyz probe 503s if it is missing
- `/files/ipxe/<name>` — the iPXE binaries over HTTP
- `/logs/ingest` — the agent's log envelope sink
- `/api/v1/report` — operation-bound deploy and commission reports
- `/api/v1/inventory` — ad-hoc inventory imports

## Stage 5: the initrd BOOT hook

The initrd is now a stock Ubuntu initramfs-tools initrd with a slagworks
BOOT hook baked in at image-build time (see the images doc for the
build). `boot=slagworks` on the kernel cmdline tells stock `/init` to
source `/scripts/slagworks`, which lives at
`assets/initramfs/scripts/slagworks` in this tree. Stock `/init` has
already:

- mounted `/proc`, `/sys`, `/dev` (devtmpfs), `/run`; set up `/dev` symlinks
  and devpts
- started udevd and coldplugged the bus, which autoloads every NIC and
  storage driver by modalias
- parsed `ip=`, `BOOTIF=`, `root=`, and other cmdline options
- sourced `/scripts/functions` so `configure_networking` is in scope

So the BOOT hook doesn't hand-roll driver loads, DHCP, or PCI/kmsg
diagnostics — udev and `configure_networking` own that. `mountroot()` is
what stock `/init` calls; the slagworks version does exactly this:

1. Parse `slagworks.*` params from `/proc/cmdline`
   (`server`, `image`, `mode`, `destructive`, `wipe_all_disks`, `machine`,
   `op`, `trace`, `expect-uuid`, `recovery`, `fatal_at`, `efivarfs`, plus
   `debug_init`). Missing `slagworks.server=` panics to an interactive
   shell.
2. Run `configure_networking` (stock initramfs-tools), which reads
   `BOOTIF=` / `ip=dhcp` and DHCPs via dhcpcd on the right NIC. Discover
   the leased interface either from the exported `DEVICE` or by scanning
   `/sys/class/net` for a global IPv4 lease. Missing lease drops to
   recovery. `configure_networking` runs dhcpcd in oneshot mode, which
   exits once it has the lease, and the kernel deletes the address when
   the lease runs out (5 minutes by default). So the hook then starts
   `dhcpcd -4 -b -K -L` on the leased interface to renew the lease for as
   long as the environment runs: the agent's whole run and any hold in
   recovery afterwards. If dhcpcd will not start, the hook prints a
   warning and carries on, and the address lasts only until the lease
   ends.
3. Mount efivarfs at `/sys/firmware/efi/efivars` when present, so the
   agent's `efibootmgr -c` can write UEFI Boot#### entries later. On BIOS
   hosts this is a no-op. `slagworks.efivarfs=skip` is a test-only knob
   the qemutest matrix uses to repro the pre-fix failure.
4. Mount a private working tmpfs at `/slag` sized at 90% of RAM. The
   default `/run` (10% of RAM) is too small for the ~600MB squashfs on
   low-RAM hosts.
5. `wget` the squashfs from
   `http://$SLAG_SERVER/files/images/$SLAG_IMAGE/root.squashfs`, then
   `modprobe squashfs loop overlay`, mount the squashfs read-only, and
   overlay-mount it onto `$rootmnt`. Overlay upper and workdir live on the
   `/slag` tmpfs; the squashfs itself is left untouched on disk. The
   `overlay/squashfs/loop` modules are baked into the initrd by the
   companion `hooks/slagworks` (`assets/initramfs/hooks/slagworks`).
6. `wget` the agent from
   `http://$SLAG_SERVER/files/bin/slagworks-agent` into
   `$rootmnt/tmp/slagworks-agent` and mark it executable. This is the
   marker the CI smoke test waits for
   (`scripts/image-build/smoke.sh` matches `slagworks: agent ready`).
7. Bind-mount `/dev`, `/proc`, and `/sys` (rbind, so the efivarfs submount
   propagates) into the overlay root.
8. `chroot $rootmnt /tmp/slagworks-agent --server=... --image=... --mode=...`
   (plus `--machine`, `--op`, `--trace`, `--expect-uuid`,
   `--destructive`, `--wipe-all-disks` when set). This never returns
   under normal deploy — the agent reboots on success.
9. On agent success (`rc==0`) the hook runs `reboot -f`. On failure it
   POSTs a `phase=agent_crashed` envelope to `/api/v1/report` and drops
   into recovery. Exit status 3 means the agent stopped because the
   site worker told it the operation is over (an operator abort, a
   timeout, a failure reported elsewhere); the envelope is refused for
   the same reason and the host waits in recovery like any other
   failure.

The recovery path (`slagworks_recovery_drop`) is worth knowing: on any
FATAL — no lease, squashfs download failure, agent nonzero exit,
`slagworks.recovery` on the cmdline — the hook installs the operator
pubkeys baked into the initrd (the image build's
`SLAGWORKS_RECOVERY_AUTHORIZED_KEYS` input) into
the overlay root, generates ephemeral host keys on tmpfs, writes a
key-only sshd config, prints the leased IP to the SOL console, and execs
`sshd -D -e` in the foreground so an operator can `ssh root@<ip>`. The
dhcpcd started in step 2 keeps renewing the lease, so the address stays
up for as long as the host waits there.
If no keys were baked in, the hook drops straight to a serial shell
instead of starting a permission-denying sshd.

Every important step emits `slagworks: STEP <name>` on the console before
the agent starts speaking — that's what makes a hung boot debuggable off a
SOL capture (which is the only channel available in the pre-agent window).
`slagworks.debug_init` on the cmdline turns on `set -x` for full command
tracing.

## Stage 6: the agent

The agent binary is a separate release artifact (see the images doc);
what it does at deploy time lives in `cmd/slagworks-agent/main.go`. Phase
progression is:

`pxe_booting` → `dhcp_ack` → `ipxe_dhcp` → `ipxe_loaded` →
`agent_starting` → `partitioning` → `rsyncing_os` →
`installing_bootloader` → `finalizing` → `bmc_reconcile` →
`awaiting_phone_home` → `completed`.

Phase set to authority:

- `pxe_booting` is set by the site worker executor when it claims a deploy
  op (`internal/worker/executor.go:377`).
- `dhcp_ack` and `ipxe_dhcp` come from the DHCP `OnACK` callback
  (`cmd/slagworksd/site.go`), for the firmware's and iPXE's DHCP only;
  the initrd's own `ip=dhcp` exchange sets no phase.
- `ipxe_loaded` fires the moment the HTTP boot handler serves a script
  (`internal/httpboot/handler.go:181-196`).
- `agent_starting`, `partitioning`, `rsyncing_os`, `installing_bootloader`,
  `finalizing`, and `bmc_reconcile` come from the agent's `setPhase` calls
  (`cmd/slagworks-agent/main.go:273, 312, 349, 355, 390, 443`).
- `awaiting_phone_home` is set by the site worker when the agent POSTs
  `deploy_complete` (`main.go:462-472`) — a single event carries both the
  rotated BMC credentials and the phase transition, so there is no window
  where one landed and the other did not.
- `completed` fires when the deployed OS phones home.

What the agent actually does, in order:

- Read `/proc/cmdline` for `slagworks.*` params and DMI system UUID.
  Refuse to deploy if `--expect-uuid` was provided and it doesn't match —
  this is the primary defense against deploying the wrong machine.
- `partitioning`: fetch the storage script from the site worker, run the
  inventory collector, pipe inventory into the script to produce a
  layout, then execute the plan (`sgdisk`, `mkfs.*`, `mdadm`, LVM, etc.).
  A `--rereadpt` race with udev is retried on EBUSY
  (`cmd/slagworks-agent/main.go:552-580`).
- `rsyncing_os`: mount the target root, rsync the deploy squashfs into
  it.
- `installing_bootloader`: firmware-mode-dependent
  (`cmd/slagworks-agent/main.go:355-384`).
  - **UEFI**: `installSystemdBoot` — copy `systemd-bootx64.efi` to the
    ESP, write a systemd-boot entry pointing at
    `/slagworks/vmlinuz` and `/slagworks/initrd.img`, then
    `efibootmgr -c` to register the boot entry. A missing ESP mount is a
    hard fail. The entry is only registered here; BootNext is set at
    the very end (below).
  - **Legacy BIOS**: `installGRUBBIOS` — install GRUB i386-pc onto each
    disk with a `bios_grub` (EF02) partition. No BIOS-grub partition on
    any disk is a hard fail — we refuse to leave the host unbootable.
    `grub-install` runs once per disk, and `/boot/grub/i386-pc` is
    removed before each run so every run writes a fresh module
    directory. A run that finds the previous run's modules backs them up
    and deletes the backups, which on an XFS root can leave a hole in
    that directory; GRUB 2.12 cannot read past one and stops at
    `error: not a correct XFS inode.`
  - **RAID** (layouts with md arrays): `target.ConfigureRAID` writes
    `/etc/mdadm/mdadm.conf` with `AUTO -all` and an `ARRAY` line, by UUID,
    for each array the layout names, regenerates the target's initramfs so
    it carries that file, and on UEFI replaces the ESP's
    `/slagworks/initrd.img` with it. The installed system then assembles
    those arrays and no others (see `docs/user/storage-scripts.md`). Any
    failure here is a hard fail.
- `finalizing`: write `/etc/fstab`, seed cloud-init (it leaves the
  network, hostname, locale and partition sizes alone), apply the network
  config script (with NetBox facts injected), write the hostname, disable
  first-boot apt timers, write operator SSH keys, enroll on tailscale (if
  an auth key rode the cmdline), and write the
  `slagworks-phone-home.service` unit. Then unmount every target
  filesystem, deepest first and the root last. A filesystem that will not
  unmount fails the deploy here, before BMC reconcile and
  `deploy_complete`: rebooting with it mounted would leave it dirty, and
  GRUB does not replay a journal, so a dirty xfs root stops a BIOS host's
  next boot at the `grub rescue>` prompt.
- `bmc_reconcile`: reconcile BMC users and persistent boot order —
  this is what gets the `slagworks` and `monitor` accounts onto the BMC
  and keeps them in sync on every redeploy. Failure is a soft error on
  hardware that refuses password rotation via ipmitool.
- Report `deploy_complete` (carries `awaiting_phone_home` on the event
  and the rotated BMC credentials in the payload — one round trip).
- UEFI only: set BootNext to the registered boot entry
  (`efibootmgr -n`), so the reboot that follows goes to the installed
  disk. This happens only after the site worker has accepted
  `deploy_complete`. Firmware honors BootNext ahead of the BMC's PXE
  override, so a deploy that is aborted or fails earlier must not leave
  it set: the next deploy's power cycle would boot the abandoned
  install instead of PXE.
- Reboot.

`slagworks.no_reboot` on the cmdline holds the host in the initrd instead
of rebooting, for ad-hoc debugging without losing `/target`.

## Stage 7: post-deploy boot, phone home

After the reboot, a UEFI host whose firmware honors BootNext boots the
installed disk directly. Any other host PXE-broadcasts again. The MAC's
op is now `deploying, phase=awaiting_phone_home` — the site worker maps
this to `BootLocal`, and the HTTP handler emits the platform-appropriate
local-boot script (BIOS `exit`, UEFI `sanboot` over drives
`0x80`-`0x9f`). The firmware boots the freshly installed OS off disk.

The generated `slagworks-phone-home.service`
(`internal/agent/target/target.go` `GeneratePhoneHomeUnit`) fires after
`network-online.target`, POSTs
`{"machine_id":"...","op_id":"...","event":"phone_home"}` to
`http://<site>/api/v1/report`, and — critically — writes the
`/var/lib/slagworks/.phoned-home` marker only on an HTTP 2xx. The unit
loops with `until wget -T 10 -q ...; do sleep 5; done` inside `ExecStart`,
so a bond that reaches administrative up before LACP has fully converged
retries within the same invocation rather than firing once and stranding
the host. If the host reboots mid-loop, the marker doesn't exist yet and
`ConditionPathExists=` re-runs the unit on the next boot.

The site worker forwards phone-home events to global, which flips the op to
`completed` and the machine to `running`. The next PXE cycle finds the
machine `running` with no active op and continues serving `BootLocal`.

## Legacy BIOS vs. UEFI: differences in one place

| Concern | Legacy BIOS | UEFI |
| ------- | ----------- | ---- |
| DHCP option 93 client arch | `INTEL_X86PC` | `EFI_X86_64` / `EFI_BC` / `EFI_IA32` |
| iPXE binary served | `undionly.kpxe` | `snponly.efi` |
| iPXE served over | TFTP | HTTP (option 60 `HTTPClient`) or TFTP fallback |
| Local-boot iPXE idiom | `exit` (return to firmware) | `sanboot` drives `0x80`-`0x9f`, then `exit` |
| Bootloader installed | GRUB i386-pc, per bios_grub disk | systemd-boot on the ESP, registered via `efibootmgr` |
| Requires `bios_grub` (EF02) partition | Yes; deploy fails without one | No |
| Requires ESP mount | No | Yes; deploy fails without `/boot/efi` |
| Console | Depends on machine `KernelParams` from NetBox | Depends on machine `KernelParams` from NetBox |
| efivarfs mount in initrd | Skipped (no `/sys/firmware/efi`) | Auto-mounted |

The firmware mode is read directly from the running environment
(`isUEFI()` checks `/sys/firmware/efi`). No NetBox flag is needed because
the agent boots through the same firmware the deployed OS will use.

## What to consult in code

- DHCP core: `internal/dhcp/handler.go`
- DHCP transport: `internal/dhcp/server.go`
- TFTP: `internal/tftp/server.go`
- iPXE script (source): `assets/ipxe/embed.ipxe`
- HTTP boot handler + endpoints: `internal/httpboot/handler.go`
- initrd BOOT hook: `assets/initramfs/scripts/slagworks`
- initrd hook (module + key baking): `assets/initramfs/hooks/slagworks`
- Agent entrypoint and phases: `cmd/slagworks-agent/main.go`
- Site worker wiring (DHCP/TFTP/HTTP, phase advancers):
  `cmd/slagworksd/site.go`
- Image row model: `internal/model/image.go`
- Executor phase transitions: `internal/worker/executor.go`
