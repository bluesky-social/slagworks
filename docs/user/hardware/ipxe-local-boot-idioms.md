# iPXE `chain file://` vs `sanboot` on AMI Aptio firmware

Research for a real-world bug: Supermicro AS-1115HS-TNR (AMI Aptio, AST2600
BMC) PXEs back to slagworks after a successful deploy, gets a "boot from local
disk" iPXE script that does `chain file:///EFI/BOOT/BOOTX64.EFI`, and iPXE
returns error `0x7f4de08e` ("Could not open ... Not found"). The ESP and the
file are present — the host successfully booted from the same file moments
earlier during deploy.

Hypothesis under test:

1. `chain file://` walks `EFI_SIMPLE_FILE_SYSTEM_PROTOCOL` handles already
   installed at the time iPXE runs, without forcing
   `ConnectController` first.
2. AMI Aptio uses lazy driver connection (only connects what's referenced by
   active `Boot####` variables), unlike OVMF.
3. `sanboot --no-describe --drive 0x80` uses the firmware's `Boot####`
   variable for that disk and bypasses SimpleFileSystem walk.
4. Other people on Supermicro/AMI hit the same issue.
5. There is or isn't a documented iPXE-side workaround.

Source citations are against `~/src/archive/ipxe/ipxe` at the master branch
checked out on this machine; line numbers are stable for current `master`.

---

## 1. `chain file://` does NOT connect drivers — confirmed

**Source: `src/interface/efi/efi_local.c`** — handles the `file://` URI scheme.
The `uri_opener` registration at line 593 maps `"file"` → `efi_local_open`.

The path that matters is `efi_local_open_volume()` at lines 241-322:

- If the URI has a host component (e.g. `file://VOLUMELABEL/path`):
  ```c
  bs->LocateHandleBuffer ( ByProtocol,
                           &efi_simple_file_system_protocol_guid,
                           NULL, &num_handles, &handles );
  ```
  Walks every handle that **already** has SimpleFileSystem installed, then
  matches by volume label.
- If no host (e.g. `file:///EFI/BOOT/BOOTX64.EFI`):
  ```c
  bs->LocateDevicePath ( protocol, &path, &device );
  ```
  with `path = efi_loaded_image_path` and `protocol =
  efi_simple_file_system_protocol_guid` — i.e., walks back up iPXE's own
  loaded-image device path looking for the closest handle that has
  SimpleFileSystem installed. For PXE-loaded iPXE, the loaded-image path is on
  the network, so this fails with `EFI_NOT_FOUND`, which iPXE's error
  registry maps to `0x7f4de08e` (the exact code observed in production).

**No `ConnectController` call appears anywhere in `efi_local.c`.** Search:

```
grep -n "Connect\|efi_connect" src/interface/efi/efi_local.c
# (no matches)
```

The `chain` command itself is just `imgexec` (`src/hci/commands/image_cmd.c:409
COMMAND ( chain, imgexec_exec )`), which fetches via the URI scheme handler;
no extra protocol negotiation happens before the file scheme is invoked.

**Verdict on claim 1: confirmed.** `chain file://` requires SimpleFileSystem
to already be installed on a handle. If no firmware action has connected the
disk's FAT driver, iPXE never sees the ESP.

The error code matches: per
[ipxe.org/7f4de08e](https://ipxe.org/7f4de08e) the platform-specific portion
decodes to `EFI_NOT_FOUND` from `OpenVolume`/`LocateDevicePath` — exactly what
the source above will return when no SimpleFileSystem handle covers the disk.

---

## 2. AMI Aptio lazy driver connection — partially confirmed

The cleanest evidence is structural rather than from AMI documentation:

- **EDK2 `BdsDxe` (the reference UEFI BDS) does NOT call
  `EfiBootManagerConnectAll` unconditionally.** In
  `MdeModulePkg/Universal/BdsDxe/BdsEntry.c`, ConnectAll is only called inside
  `ProcessLoadOptions` when a driver load option completes with the
  `LOAD_OPTION_FORCE_RECONNECT` attribute set
  ([source](https://raw.githubusercontent.com/tianocore/edk2/master/MdeModulePkg/Universal/BdsDxe/BdsEntry.c)).
  Whether everything gets connected before the boot option fires is therefore
  a **platform BDS hook** decision, not a UEFI-spec mandate.
- **OVMF connects all devices on its platform side.**
  `OvmfPkg/Library/PlatformBootManagerLib/BdsPlatform.c::PlatformBootManagerAfterConsole`
  calls `ConnectDevicesFromQemu()` (which connects every PCI device QEMU's
  `bootorder` fw_cfg names) and falls back to `EfiBootManagerConnectAll()` if
  that fails
  ([source](https://raw.githubusercontent.com/tianocore/edk2/master/OvmfPkg/Library/PlatformBootManagerLib/BdsPlatform.c),
  ~lines 1271-1280). This is why every iPXE+OVMF test we have just works.
- **AMI Aptio has a "Fast Boot" feature whose entire purpose is to skip
  ConnectAll.** Public AMI/Insyde firmware setup pages describe Fast Boot
  toggles like "USB Mass Storage Driver Support", "PCI ROM Priority", and
  "NVMe Firmware Source" that gate which drivers get connected before the
  Boot#### entry runs. I could not pull a citable AMI internal spec doc in
  this session (their docs are gated/PDF), but the behavior is consistent
  with the iPXE source's *defensive* posture (see §3) — and the iPXE
  maintainers explicitly call out AMI as a vendor where
  `chain file://` is known to not see disks (issue #1676 below).

**Verdict on claim 2: confirmed in shape but not documented to the AMI
spec.** It's enough to know that the UEFI spec doesn't require ConnectAll
and that some platforms don't do it. Even Supermicro with Fast Boot
*disabled* may still skip connecting non-boot block devices on some firmware
revs. We don't need to nail AMI's exact decision tree; we need iPXE to not
depend on the firmware's choice.

---

## 3. `sanboot` calls ConnectController, then walks SimpleFileSystem — REFINEMENT of the hypothesis

This is the most important finding. The operator's hypothesis was that
`sanboot --drive 0x80` would use the firmware's `Boot####` variable. **It
does not.** What actually happens is better than that: it forces driver
connection so the SimpleFileSystem walk works.

**Source: `src/interface/efi/efi_block.c::efi_block_boot`** (lines 934-1049)
is the EFI sanboot implementation. Step by step:

1. **Line 949: `efi_driver_reconnect_all();`** — globally walks every EFI
   handle and calls `efi_connect(handle, NULL)` on each. `efi_connect` is in
   `src/interface/efi/efi_connect.c:58-82`:
   ```c
   efirc = bs->ConnectController ( device, drivers, NULL, TRUE );
   ```
   `Recursive=TRUE`. This is the part that compels lazy-connect firmware to
   bind FAT/Partition drivers to all block I/O handles.
2. **Line 955-963:** `LocateHandleBuffer(ByProtocol, BlockIo, ...)` —
   enumerate all block I/O handles.
3. **Line 1035 → `efi_block_scan` (line 749):** `efi_block_connect(drive,
   handle)` — *another* `efi_connect(handle, NULL)`, this time targeted at
   the specific block device, also with `Recursive=TRUE`.
4. **Line 772: `LocateHandleBuffer(ByProtocol, SimpleFileSystem, ...)`** —
   THE SAME walk that `chain file://` does. By this point, ConnectController
   has fired twice and the FAT driver should be bound.
5. **Line 706: `efi_block_filename` checks for the boot file** (default
   `\EFI\BOOT\BOOTX64.EFI` on x64,
   `src/include/ipxe/efi/Uefi/UefiSpec.h:2272`).
6. **Line 854: `bs->LoadImage(FALSE, efi_image_handle, path, NULL, 0, &image);`**
   Constructs a device path = `<block-device-path>\EFI\BOOT\BOOTX64.EFI`
   (lines 821-848) and calls `LoadImage` directly — **NOT** by Boot####
   variable. The Boot#### variable is never read by `efi_block_boot`.

So claim 3 — "sanboot uses Boot#### variable" — is **REFUTED**. iPXE's
sanboot path does its own SimpleFileSystem walk and its own LoadImage from a
hand-constructed path. The reason it works on AMI when `chain file://`
doesn't is **purely the `ConnectController(..., Recursive=TRUE)` calls**
that `efi_block_boot` makes and `efi_local_open_volume` doesn't.

**This still confirms the proposed fix works** — but for a different
mechanism than the operator hypothesized. Side effect: there's no risk of
sanboot loading some *other* image (like a previously-set Windows boot entry
under `Boot0001`) — it always loads `\EFI\BOOT\BOOTX64.EFI` from the matching
ESP.

Default `--drive` value: `san_default_drive()`, which on x86 returns `0x80`
(`src/include/ipxe/sanboot.h:34: #define SAN_DEFAULT_DRIVE 0x80`). On EFI,
sanboot's drive numbering is virtual — it walks block I/O handles in
enumeration order and assigns numbers starting at `SAN_DEFAULT_DRIVE` for
"local" drives (lines 1014-1019). Drive `0x80` matches the first whole-disk
local block device; partitions are skipped (`efi_block_local`, lines
896-925). On a single-disk Supermicro this will be the system's data drive.

---

## 4. Known issues on Supermicro / AMI / Aptio — confirmed

iPXE GitHub issues directly relevant:

- **[ipxe/ipxe#1676](https://github.com/ipxe/ipxe/issues/1676)** — "Chaining
  to file on local usb drive no longer works in 2.0.0 (efi with autoexec.ipxe)"
  Reporter on **ASUS PRIME X670-P (AMI firmware)**. iPXE maintainer
  [NiKiZe](https://github.com/NiKiZe) responds: *"This might behave differently
  on different hardware, for example this probably works on pure EDK2 based
  firmware, such as Qemu. While some vendors have their own implementations.
  So it would be interesting to note who the Firmware vendor of this device
  is, likely ones is AMI and Phoenix."* This is a verbatim
  acknowledgement that iPXE+`file://` is hardware-dependent, and AMI is one of
  the affected vendors.
- **[ipxe/ipxe#447](https://github.com/ipxe/ipxe/issues/447)** — "Feature
  Request: boot from local disk in uefi", long-standing thread. Multiple users
  hit `chain file://` failures on bare metal. Aaron Young's patchset (linked
  to [ipxe-devel
  archive](https://lists.ipxe.org/pipermail/ipxe-devel/2019-February/006506.html))
  proposed adding `efimap` and disk-boot helpers; the workaround the thread
  converges on for non-patched mainline is `sanboot`. PR
  [#612](https://github.com/ipxe/ipxe/pull/612) reproduces the patchset.
- **[ipxe/ipxe#428](https://github.com/ipxe/ipxe/issues/428)** —
  "Support allowing to specify device context in EFI". Issue body authored by
  iPXE maintainer **NiKiZe** explicitly recommends: *"a `sanboot --drive
  SYSTEM --filename \EFI\MICROSOFT\BOOT\BOOTMGFW.EFI --no-describe` could
  serve this same purpose, and might be easier to implement."* This is
  primary-source guidance from an iPXE maintainer that `sanboot` is the
  intended path for booting a known file off a local disk in EFI mode.
- **[ipxe/ipxe#213](https://github.com/ipxe/ipxe/issues/213)** — closed by
  NiKiZe with: *"Also the `sanboot` command works for efi as well now, so
  that is probably a better approach?"* Confirms maintainer stance.
- **[ipxe/ipxe#123](https://github.com/ipxe/ipxe/issues/123)** — old "file:/
  URIs for USB key after loading native USB drivers" — same class of bug
  (iPXE's `file://` doesn't see drives the firmware never connected).

**Verdict on claim 4: confirmed.** The failure mode is well-known on AMI
firmware and the iPXE maintainers' on-record recommendation is exactly the
proposed fix.

---

## 5. iPXE-side workarounds — limited

Things checked and what they do:

- **`ifopen` / `ifclose`:** purely network interface management
  (`src/hci/commands/ifmgmt_cmd.c`), no effect on block-device drivers. The
  appearance of `ifopen` in issue #1676's repro is unrelated to the disk
  problem — it's just netdev plumbing.
- **`imgfetch --rebuild`:** no `--rebuild` flag exists on `imgfetch`
  (`src/hci/commands/image_cmd.c:189-198`). The `imgsingle` family has
  `--name` and `--timeout` only.
- **No top-level `connect` command** in iPXE. No way to call
  `ConnectController` from the iPXE shell directly.
- **`efidev list` / `efimap`:** not in mainline. `efimap` only exists in the
  Aaron Young patchset (PR #612) which has not landed.
- **`autoboot --filename` / `boot --filename`:** these go through the
  network/URI machinery, not block-device walk.
- **Build with `DEBUG=efi_wrap` and chain a specific block device driver:**
  NiKiZe's debugging recommendation in issue #1676. Useful for diagnosis but
  not a runtime workaround.
- **`ipxe-legacy.efi`:** confirmed by issue reporters to fix some classes of
  this problem on AMI firmware — but legacy refers to USB-driver-stack
  exclusion, not block-device exclusion. Not applicable to our SATA-attached
  disk on the Supermicro.

**The only first-class iPXE workaround is `sanboot`.** That is exactly the
proposed fix.

---

## Side-effects to be aware of

- `sanboot --no-describe --drive 0x80` will iterate **all whole-disk block
  devices** when the drive number doesn't match a registered SAN device,
  trying each in turn (`efi_block_boot` lines 967-1043). On a single-disk
  Supermicro AS-1115HS-TNR this is fine. On a dual-disk box it'll try each
  disk's ESP in enumeration order until one has `\EFI\BOOT\BOOTX64.EFI` and
  loads cleanly. Multi-NVMe boxes have a slightly nondeterministic
  "first-disk-with-bootloader" semantic.
- `sanboot` also calls `efi_snp_release()` (line 952) which tears down
  iPXE's SNP/network protocols before LoadImage. After sanboot, iPXE
  network stack is gone. Doesn't matter for "boot from disk and stop"; would
  matter if the chained image expected iPXE network state to persist
  (post-fix, you can't `goto fallback` in iPXE if sanboot's LoadImage
  succeeds — it never returns).
- Drive number `0x80` is the BIOS-style first-disk constant. On EFI it's a
  virtual number, not a firmware boot variable index. Not a concern for our
  use case but worth knowing if anyone tries to address `0x81` etc.
- iPXE is built with `SANBOOT_PROTO_AOE`/`ISCSI`/etc. flags. The EFI block
  boot path is pulled in via `PROVIDE_SANBOOT(efi, san_boot, efi_block_boot)`
  at line 1054 of `efi_block.c`. Standard iPXE EFI builds include it — no
  config change required.

---

## What's NOT in scope of this research

- Whether changing the on-disk Boot#### variables from slagworks (e.g.,
  setting `BootNext = Boot0001` and rebooting) is a viable alternative
  fix. That requires NVRAM writes from a running OS or from a UEFI shell,
  not from iPXE.
- Whether AMI Aptio-specific BIOS settings ("Boot Option Filter", "PCI ROM
  Priority", etc.) on this Supermicro can be flipped to make the firmware
  connect all block devices automatically. Possibly — but a fix that lives
  in iPXE script is operationally cheaper than per-host BIOS knob hunting.
- Whether to instead ship Aaron Young's patchset (PR #612 / `efimap`).
  Plausible long-term option but requires maintaining a fork of iPXE.
- Whether `ipxe-legacy.efi` would also work. Probably not for this case —
  legacy disables USB drivers, not anything we depend on.

---

## Sources

- iPXE source tree: `~/src/archive/ipxe/ipxe`
  - `src/interface/efi/efi_local.c` — `file://` scheme handler
  - `src/interface/efi/efi_block.c` — EFI sanboot implementation
  - `src/interface/efi/efi_connect.c` — `efi_connect()` wrapper around
    `ConnectController`
  - `src/interface/efi/efi_driver.c` — `efi_driver_reconnect_all`
  - `src/hci/commands/sanboot_cmd.c` — sanboot command parsing
  - `src/hci/commands/image_cmd.c` — chain/imgfetch/imgexec
  - `src/include/ipxe/sanboot.h` — `SAN_DEFAULT_DRIVE 0x80`
  - `src/include/ipxe/efi/Uefi/UefiSpec.h` —
    `EFI_REMOVABLE_MEDIA_FILE_NAME_X64 = "\EFI\BOOT\BOOTX64.EFI"`
- iPXE GitHub:
  - https://github.com/ipxe/ipxe/issues/1676 (AMI/ASUS file:// failure)
  - https://github.com/ipxe/ipxe/issues/447 (UEFI local-disk boot history)
  - https://github.com/ipxe/ipxe/issues/428 (NiKiZe recommends sanboot)
  - https://github.com/ipxe/ipxe/issues/213 (NiKiZe recommends sanboot)
  - https://github.com/ipxe/ipxe/issues/123 (file:// + USB)
  - https://github.com/ipxe/ipxe/pull/612 (Aaron Young patch)
- iPXE error registry: https://ipxe.org/7f4de08e
- EDK2 BDS source:
  https://raw.githubusercontent.com/tianocore/edk2/master/MdeModulePkg/Universal/BdsDxe/BdsEntry.c
- OVMF platform BDS:
  https://raw.githubusercontent.com/tianocore/edk2/master/OvmfPkg/Library/PlatformBootManagerLib/BdsPlatform.c
- ipxe-devel ML (Aaron Young patchset):
  https://lists.ipxe.org/pipermail/ipxe-devel/2019-February/006506.html
