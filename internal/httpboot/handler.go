package httpboot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

var validMAC = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)

// HandlerConfig configures the boot script handler.
type HandlerConfig struct {
	// DefaultImage is the single PXE image dir name to boot (e.g.,
	// "noble-pxe") — the same dir for deploy and commission (mode is a
	// cmdline, not a separate image). When set, the handler returns a
	// kernel+initrd boot script; when empty, a Phase 0 chainload marker.
	DefaultImage string

	// ImagesDir is the local directory containing the image subdirectory
	// (DefaultImage) the boot scripts serve over HTTP.
	ImagesDir string

	// HTTPPort is the port used in kernel/initrd URLs. Defaults to 8080.
	HTTPPort int

	// DeployMode enables the full deploy boot sequence: kernel + initrd whose
	// BOOT hook downloads the squashfs, mounts it, and runs the slagworks agent.
	// When false (and DefaultImage is set), returns a kernel-only boot script.
	DeployMode bool
}

// Handler serves iPXE boot scripts. Boot decisions and state recording are
// delegated to a MachineProvider.
type Handler struct {
	cfg      HandlerConfig
	log      *slog.Logger
	provider model.MachineProvider
}

func NewHandler(cfg HandlerConfig, provider model.MachineProvider) *Handler {
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 8080
	}
	return &Handler{
		cfg:      cfg,
		log:      slog.Default(),
		provider: provider,
	}
}

func (h *Handler) SetLogger(l *slog.Logger) {
	h.log = l
}

// MarkVerified records that the installed OS phoned home (post-boot).
func (h *Handler) MarkVerified(mac string) {
	id, ok := h.provider.MachineIDForMAC(mac)
	if !ok {
		h.log.Warn("ignoring phone-home for unknown MAC", "mac", mac)
		return
	}
	h.provider.RecordPhoneHome(id)
	h.log.Info("marked machine as verified", "mac", mac, "machine_id", id)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	if mac != "" && !validMAC.MatchString(mac) {
		http.Error(w, "invalid mac", http.StatusBadRequest)
		return
	}

	h.log.Info("boot script request",
		"mac", mac,
		"remote", r.RemoteAddr,
	)

	var script string
	action := h.provider.BootActionForMAC(mac)

	// Report the ipxe_loaded phase for boot-driving operations (deploy or
	// commission, which share the boot-prefix spine) so the watch TUI's
	// lifecycle pane lights up when iPXE fetches its boot script — not just
	// for deploy. Without this, a commission op sits all-pending through the
	// boot prefix until the agent's first phone-home.
	var bootOp *model.Operation
	switch action {
	case model.BootDeploy:
		bootOp = h.provider.DeployOpForMAC(mac)
	case model.BootCommission:
		bootOp = h.provider.CommissionOpForMAC(mac)
	case model.BootLocal:
		// No op-driven boot phase to report for a local boot.
	}
	if bootOp != nil {
		if err := h.provider.HandleProgressReport(&model.ProgressReport{
			OperationID: bootOp.ID.String(),
			MachineID:   bootOp.MachineID.String(),
			Phase:       "ipxe_loaded",
		}); err != nil {
			h.log.Warn("failed to record iPXE boot phase",
				"machine_id", bootOp.MachineID, "op_id", bootOp.ID, "err", err)
		}
	}

	// iPXE reports its firmware platform ("pcbios" / "efi") via the chain
	// URL (embed.ipxe). Absent => treat as UEFI (current behavior), which
	// is correct for the all-UEFI fleet and for any in-field iPXE binary
	// predating the platform-param embed.ipxe.
	platform := r.URL.Query().Get("platform")

	switch {
	case action == model.BootLocal:
		script = h.bootLocalScript(mac, platform)
	case action == model.BootCommission && h.cfg.DeployMode && h.cfg.DefaultImage != "":
		script = h.commissionBootScript(mac)
	case h.cfg.DeployMode && h.cfg.DefaultImage != "":
		script = h.deployBootScript(mac)
	case h.cfg.DefaultImage != "":
		script = h.kernelBootScript(mac)
	default:
		script = h.chainloadCompleteScript(mac)
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, script)
}

// platformBIOS is iPXE's ${platform} value on legacy/BIOS firmware. UEFI
// reports "efi"; an absent value (old iPXE binary, or a request that
// didn't carry the param) is treated as UEFI.
const platformBIOS = "pcbios"

func (h *Handler) bootLocalScript(mac, platform string) string {
	if platform == platformBIOS {
		// Legacy BIOS: hand control back to the firmware, which then
		// proceeds to the next device in its BBS boot order — the local
		// disk (PXE is boot-order #1, disk #2; the MAAS LOCALBOOT idiom).
		// The firmware does the INT13h disk boot itself, which reliably
		// chains the disk's GRUB MBR. We deliberately do NOT use iPXE
		// `sanboot --drive 0x80` here: on AMI Aptio (pop1 leaf-6 / SN2700)
		// sanboot prints "Booting from SAN device 0x80" and then hangs —
		// iPXE's own INT13h hand-off doesn't chain GRUB. `exit` returns to
		// the BIOS, which boots the disk the same way a post-OS `reboot`
		// does. See docs/user/boot-process.md ("Legacy BIOS vs. UEFI").
		return fmt.Sprintf(`#!ipxe
echo slagworks: boot-local (BIOS exit to firmware) for mac %s
exit
`, mac)
	}

	// UEFI (or absent platform → default). iPXE's `chain file:///` only
	// walks EFI_SIMPLE_FILE_SYSTEM_PROTOCOL handles already attached at
	// the time iPXE runs — works on OVMF (eager BdsConnectAll) but fails
	// on Supermicro AMI Aptio (lazy connect: disk FAT driver isn't loaded
	// when iPXE was network-booted, so iPXE returns 0x7f4de08e / "no such
	// file"). `sanboot` calls efi_driver_reconnect_all + efi_block_connect
	// → recursive ConnectController, which force-attaches the FAT driver
	// before loading \EFI\BOOT\BOOTX64.EFI directly via LoadImage. It's
	// the iPXE-maintainer-recommended idiom for UEFI local boot (ipxe/ipxe
	// issues #428, #1676).
	//
	// The script walks drives 0x80-0x9f and boots the first one whose ESP
	// holds \EFI\BOOT\BOOTX64.EFI. iPXE numbers whole local disks from 0x80
	// in the firmware's block-device order (efi_block.c), and a storage
	// script may put the ESP on any of them: stopping at 0x81 left a machine
	// whose ESP is on its third disk unbootable after the deploy's one-shot
	// BootNext. 32 drives covers a 24-bay chassis plus boot NVMe and BMC
	// virtual media; each number past the last real disk is one quick failed
	// sanboot. A loader that returns does not end the walk, so a second ESP
	// (RAID1 root) still gets its turn.
	//
	// When no drive boots, a plain `exit` (status 0) makes embed.ipxe's
	// chain succeed, so iPXE exits to the firmware with EFI_SUCCESS: the
	// same hand-back the BIOS script uses. Firmware that continues its boot
	// order then reaches the "Slagworks" Boot#### entry the agent
	// registered; EDK2-based firmware with a boot menu (OVMF) shows that
	// menu instead, as the UEFI spec allows after a successful return. A
	// nonzero status would fail the chain, which embed.ipxe retries 20 times
	// before dropping to an iPXE shell. A served script cannot make iPXE
	// itself return a failure status; that would take an embed.ipxe change.
	return fmt.Sprintf(`#!ipxe
echo slagworks: boot-local for mac %s
set drive:uint8 0x80
:try_drive
sanboot --no-describe --drive ${drive} ||
inc drive
iseq ${drive} 0xa0 || goto try_drive
echo slagworks: boot-local: no drive 0x80-0x9f booted, handing back to the firmware
exit
`, mac)
}

func (h *Handler) chainloadCompleteScript(mac string) string {
	return fmt.Sprintf(`#!ipxe
echo slagworks: chainload complete, mac %s
exit
`, mac)
}

// consoleParams returns the kernel console= parameters for a machine
// and whether a value was configured at all.
//
// Sourced from MachineSnapshot.KernelParams, which itself is fed by
// the NetBox device_type custom field slagworks_kernel_params.
// When that value is "none" the device opts out of having
// any console= on the cmdline (e.g. switches with no SOL) — that is
// configured=true with an empty string.
//
// There is deliberately NO fallback console: an unset field returns
// configured=false. The commission path answers that with the
// broadcast/determination cmdline (broadcastConsoleParams — the sweep
// discovers the real console and writes it back to the device-type);
// the deploy path refuses (missingConsoleScript). Guessing a single
// serial config (the old tty0+ttyS1 default) produced hosts whose
// console silently didn't match their BMC's SOL port; preflight also
// enforces the field at deploy-op creation.
func (h *Handler) consoleParams(mac string) (string, bool) {
	if h.provider == nil {
		return "", false
	}
	if m := h.provider.MachineByMAC(mac); m != nil {
		return consoleParamsForMachine(m)
	}
	return "", false
}

func consoleParamsForMachine(m *model.MachineSnapshot) (string, bool) {
	kp := m.KernelParams
	switch kp {
	case "none":
		return "", true
	case "":
		return "", false
	default:
		return "console=tty0 " + kp, true
	}
}

// broadcastConsoleParams is the determination-mode console cmdline for
// commissioning a device whose type has no slagworks_kernel_params
// yet: every sweep-candidate tty plus tty0. The kernel mirrors output
// to every console= token, so boot progress is visible on whichever
// port the BMC SOL is actually wired to, and the agent's sweep probe
// (consoleprobe.Sweep) then determines the real one empirically. NOT a
// fallback — deploy still requires the field; this exists only so the
// determination commission can boot at all. Must list the same
// candidates as model.ConsoleSweepTTYs.
func broadcastConsoleParams() string {
	var b strings.Builder
	b.WriteString("console=tty0")
	for _, tty := range model.ConsoleSweepTTYs {
		fmt.Fprintf(&b, " console=%s,%dn8", tty, model.ConsoleSweepBaud)
	}
	return b.String()
}

// missingConsoleScript is served instead of a DEPLOY boot when the
// device-type has no slagworks_kernel_params. It refuses loudly on
// whatever console the firmware gives iPXE rather than booting a
// kernel with a guessed serial config: a wrong console= yields a host
// that deploys "fine" but is invisible over SOL — strictly worse than
// an explicit stop the operator can see and fix. (Commission does NOT
// take this path — an unconfigured device commissions in broadcast
// mode and the sweep determines the config; see broadcastConsoleParams.)
// Preflight already rejects deploy ops on the empty field at creation,
// so reaching this script means NetBox changed under an in-flight op.
func (h *Handler) missingConsoleScript(mac string) string {
	return fmt.Sprintf(`#!ipxe
echo slagworks: ERROR: no slagworks_kernel_params for mac %s
echo slagworks: set the NetBox device-type custom field slagworks_kernel_params
echo slagworks: (a commission determines it automatically -- or set "none" to opt out)
echo slagworks: refusing to deploy with a guessed console config
shell
`, mac)
}

func (h *Handler) kernelBootScript(mac string) string {
	console, ok := h.consoleParams(mac)
	if !ok {
		return h.missingConsoleScript(mac)
	}
	server := fmt.Sprintf("${next-server}:%d", h.cfg.HTTPPort)
	base := fmt.Sprintf("http://%s/files/images/%s", server, h.cfg.DefaultImage)
	// slagworks.server=/image= are mandatory for the boot=slagworks hook:
	// scripts/slagworks parse_cmdline panics to a shell if slagworks.server=
	// is absent (and defaults a missing image to "noble", which would guess
	// the wrong dir here). The deploy/commission scripts pass both; this
	// kernel-only path (DeployMode=false) must too. mode defaults to "deploy"
	// in the hook, which is correct for this path.
	//
	// BOOTIF uses ${netX} (the interface iPXE booted/DHCP'd on), never
	// ${net0}: net0 is merely the first NIC iPXE enumerates. On the MS-01
	// router that is the dark I226-V while PXE ran on the I226-LM, so the
	// initrd waited for DHCP on the wrong port forever (router1.pop3,
	// 2026-10-19; also the July "IPv4 didn't apply" deploy). Same reasoning as
	// embed.ipxe's ${mac}. All three boot templates below follow this.
	return fmt.Sprintf(`#!ipxe
echo slagworks: booting kernel for mac %s
kernel %s/vmlinuz %s BOOTIF=01-${netX/mac:hexhyp} boot=slagworks ip=dhcp slagworks.server=%s slagworks.image=%s
initrd %s/initrd
boot
`, mac, base, console, server, h.cfg.DefaultImage, base)
}

// oobKernelArg returns the " slagworks.oob=amt" cmdline token for a machine
// whose OOB endpoint is Intel AMT (snapshot AMT, i.e. its bmc-creds-admin
// entry is kind=amt), else "". The agent keys its D2 gates on it: commission
// skips bmc_ipmitool and deploy skips bmc_reconcile, since there is no IPMI
// BMC behind an AMT host.
func oobKernelArg(m *model.MachineSnapshot) string {
	if m != nil && m.AMT {
		return " slagworks.oob=amt"
	}
	return ""
}

// bmcAdminUserKernelArg returns the " slagworks.bmc_admin_user=<name>"
// deploy cmdline token: the device's bmc-creds-admin account NAME (never
// the password), which the agent's in-band BMC reconcile uses to refuse
// before any write when that account isn't at slot 2 — otherwise its
// leftover pass would disable slagworks' only BMC credential. The name
// comes from the op's bmc_runtime (resolved by global for this deploy) or,
// for a deploy whose transport isn't the BMC (--via pdu), from the
// snapshot's stamp of the same admin entry. "" for an AMT machine (no IPMI
// reconcile runs) or when neither source has a name; the agent then logs
// that the gate was skipped. The value is encoded so it can't split the
// cmdline or trigger iPXE settings expansion.
func bmcAdminUserKernelArg(m *model.MachineSnapshot, p model.OpParams) string {
	if m == nil || m.AMT {
		return ""
	}
	user := m.BMCUser
	if p.BMCRuntime != nil && p.BMCRuntime.Username != "" {
		user = p.BMCRuntime.Username
	}
	if user == "" {
		return ""
	}
	return " " + bmcreconcile.AdminUserKernelArg + "=" + bmcreconcile.EncodeAdminUserArg(user)
}

func (h *Handler) commissionBootScript(mac string) string {
	machine, op, err := h.boundBootOperation(mac, model.OpCommission)
	if err != nil {
		return h.identityErrorScript(err)
	}
	console, ok := consoleParamsForMachine(machine)
	if !ok {
		// Unset kernel_params: determination commission. Broadcast to
		// every candidate console so the boot is visible over SOL and
		// the agent's sweep can discover which port is really wired.
		console = broadcastConsoleParams()
	}
	server := fmt.Sprintf("${next-server}:%d", h.cfg.HTTPPort)
	imageName := h.resolveImageName()
	base := fmt.Sprintf("http://%s/files/images/%s", server, imageName)

	extras := fmt.Sprintf(" slagworks.machine=%s slagworks.op=%s", machine.ID, op.ID)
	if op.TraceID != uuid.Nil {
		extras += fmt.Sprintf(" slagworks.trace=%s", op.TraceID)
	}
	extras += oobKernelArg(machine)

	// boot=slagworks tells the stock Ubuntu initramfs /init to source our
	// BOOT hook (/scripts/slagworks) and call its mountroot(). ip=dhcp makes
	// stock /init run configure_networking; BOOTIF=01-<mac-with-hyphens>
	// then tells configure_networking which NIC iPXE chainloaded on, so it
	// DHCPs on the right (cabled-to-the-serving-leaf) port instead of
	// guessing. Pxelinux convention, recognized by initramfs-tools.
	return fmt.Sprintf(`#!ipxe
echo slagworks: commission boot for mac %s
kernel %s/vmlinuz %s BOOTIF=01-${netX/mac:hexhyp} boot=slagworks ip=dhcp slagworks.server=%s slagworks.image=%s slagworks.mode=commission%s
initrd %s/initrd
boot
`, mac, base, console, server, imageName, extras, base)
}

func (h *Handler) deployBootScript(mac string) string {
	machine, op, err := h.boundBootOperation(mac, model.OpDeploy)
	if err != nil {
		return h.identityErrorScript(err)
	}
	console, ok := consoleParamsForMachine(machine)
	if !ok {
		return h.missingConsoleScript(mac)
	}
	server := fmt.Sprintf("${next-server}:%d", h.cfg.HTTPPort)
	imageName := h.resolveImageName()
	base := fmt.Sprintf("http://%s/files/images/%s", server, imageName)
	extra := ""

	machineID, opID, traceID := machine.ID.String(), op.ID.String(), ""
	if h.provider != nil {
		if op != nil {
			if op.TraceID != uuid.Nil {
				traceID = op.TraceID.String()
			}
			if p, err := model.DecodeParams(op.Params); err == nil {
				if p.Destructive {
					extra = " slagworks.destructive"
				}
				if p.WipeAllDisks {
					extra += " slagworks.wipe_all_disks"
				}
				if p.Recovery {
					extra += " slagworks.recovery"
				}
				// fatal_at is a TEST-ONLY parameter wired through to the
				// agent's SLAGWORKS_FATAL_AT env var. Used by the recovery
				// sshd integration test to force a clean late-stage
				// fatal. Not exposed via slagctl.
				if p.FatalAt != "" {
					extra += " slagworks.fatal_at=" + p.FatalAt
				}
				// efivarfs is a TEST-ONLY parameter wired through to
				// the BOOT hook's slagworks.efivarfs= kernel
				// cmdline knob. The qemutest matrix uses
				// efivarfs=skip to repro the pop1 ship-blocker
				// (efivarfs not mounted -> efibootmgr -c fails ->
				// agent FATALs in the new behavior, silent-Warned
				// in the old). Production deploys leave this unset.
				//
				// This (and fatal_at above) is not yet gated on
				// SLAGWORKS_TEST=1, so a stray test-fixture op.Params
				// in production could re-introduce the pop1 bug.
				// Deferred until before pop1 takes real traffic.
				if p.Efivarfs != "" {
					extra += fmt.Sprintf(" slagworks.efivarfs=%s", p.Efivarfs)
				}
				// data_marker is a TEST-ONLY parameter wired to the
				// agent's slagworks.data_marker= cmdline token. The
				// preserve-data qemu test uses it to drop a first-boot
				// unit that writes+echoes /data/PRESERVE_MARKER, proving
				// a preserved array survives a redeploy. Production
				// deploys leave this unset (same gating concern
				// as fatal_at/efivarfs).
				if p.DataMarker != "" {
					extra += " slagworks.data_marker=" + p.DataMarker
				}
				// Tailscale deploy-time enrollment (plan
				// 2026-06-21-tailscale-enrollment-at-deploy). The auth
				// key + hostname ride the kernel cmdline; the agent reads
				// them from /proc/cmdline (visible in its chroot) and
				// enrolls before reboot. Absent block => no tokens => the
				// agent skips enrollment. The key is single-use and
				// auto-expiring; cmdline exposure on the box we're imaging
				// is out of the threat model (see the plan). Both tokens
				// are emitted only when the auth key is present so a
				// half-populated block can't produce a hostname-only
				// cmdline the agent would misread.
				//
				// The hostname derives from the NetBox device name, which
				// is free text. Spliced raw, a newline in it would add an
				// iPXE command and a space would add kernel parameters, so
				// a hostname that isn't valid drops both tokens (the agent
				// skips enrollment). Global declines to mint for such a
				// name; this keeps the script safe whatever the op holds.
				if ts := p.TailscaleRuntime; ts != nil && ts.AuthKey != "" {
					if ts.Hostname != "" && !model.ValidHostname(ts.Hostname) {
						h.log.Warn("tailscale enrollment skipped: the op's tailnet hostname is not a valid hostname",
							"machine_id", machineID, "op_id", opID, "hostname", ts.Hostname)
					} else {
						extra += " slagworks.tailscale_authkey=" + ts.AuthKey
						if ts.Hostname != "" {
							extra += " slagworks.tailscale_hostname=" + ts.Hostname
						}
					}
				}
				extra += bmcAdminUserKernelArg(machine, p)
			}
		}

		// Append system UUID for the identity check — agent refuses to
		// deploy unless DMI system UUID matches. Empty means never
		// commissioned; the check below refuses to serve a boot
		// script in that case.
		if machine.SystemUUID != "" {
			extra += fmt.Sprintf(" slagworks.expect-uuid=%s", machine.SystemUUID)
		}
	}

	// If we have a deploy operation but no expected UUID, refuse to serve a boot script
	if machineID != "" && !strings.Contains(extra, "slagworks.expect-uuid=") {
		return fmt.Sprintf("#!ipxe\necho slagworks: ERROR: machine %s not commissioned (no system UUID), cannot deploy\nshell\n", machineID)
	}

	// boot=slagworks makes stock /init source our BOOT hook; ip=dhcp makes it
	// run configure_networking; BOOTIF selects the NIC within it. See
	// commissionBootScript for the full rationale.
	cmdline := fmt.Sprintf("%s BOOTIF=01-${netX/mac:hexhyp} boot=slagworks ip=dhcp slagworks.server=%s slagworks.image=%s",
		console, server, imageName)
	if machineID != "" {
		cmdline += fmt.Sprintf(" slagworks.machine=%s slagworks.op=%s", machineID, opID)
	}
	if traceID != "" {
		cmdline += " slagworks.trace=" + traceID
	}
	cmdline += oobKernelArg(machine)
	cmdline += extra

	return fmt.Sprintf(`#!ipxe
echo slagworks: deploy boot for mac %s
kernel %s/vmlinuz %s
initrd %s/initrd
boot
`, mac, base, cmdline, base)
}

// boundBootOperation is the single identity join used by deploy and
// commission. The boot MAC selects a snapshot machine once; the active op
// must name that exact machine and the requested lifecycle type.
func (h *Handler) boundBootOperation(mac string, want model.OperationType) (*model.MachineSnapshot, *model.Operation, error) {
	if h.provider == nil {
		return nil, nil, errors.New("machine provider unavailable")
	}
	machine := h.provider.MachineByMAC(mac)
	if machine == nil {
		return nil, nil, fmt.Errorf("boot MAC %s does not uniquely identify a machine", mac)
	}
	var op *model.Operation
	switch want {
	case model.OpDeploy:
		op = h.provider.DeployOpForMAC(mac)
	case model.OpCommission:
		op = h.provider.CommissionOpForMAC(mac)
	default:
		return nil, nil, fmt.Errorf("unsupported boot operation type %s", want)
	}
	if op == nil {
		return nil, nil, fmt.Errorf("machine %s has no active %s operation", machine.ID, want)
	}
	if op.MachineID != machine.ID || op.Type != want || op.ID == uuid.Nil ||
		op.Status == model.OpCompleted || op.Status == model.OpFailed {
		return nil, nil, fmt.Errorf("boot identity mismatch: MAC %s resolved machine %s but operation %s is %s for machine %s",
			mac, machine.ID, op.ID, op.Type, op.MachineID)
	}
	return machine, op, nil
}

func (h *Handler) identityErrorScript(err error) string {
	h.log.Error("refusing lifecycle boot with unbound identity", "err", err)
	return fmt.Sprintf("#!ipxe\necho slagworks: ERROR: %s\nshell\n", err)
}

// resolveImageName returns the single PXE image directory name. deploy and
// commission boot the SAME image (distinguished only by the slagworks.mode=
// cmdline, set by the caller), so there is one image dir — h.cfg.DefaultImage
// (conventionally "noble-pxe"), matching the single Image row's Name and the
// on-disk dir SyncImages writes. (Before the single-image distribution this took a kind and
// resolved noble-deploy / noble-commission; those are gone.)
func (h *Handler) resolveImageName() string {
	return h.cfg.DefaultImage
}

// ServerConfig configures the HTTP boot server.
type ServerConfig struct {
	// ImagesDir is the local directory containing image subdirectories.
	ImagesDir string

	// IPXEDir is the directory holding the iPXE binaries (snponly.efi,
	// undionly.kpxe) — the same dir the TFTP server serves. When set,
	// they're also served over HTTP at /files/ipxe/<name> so UEFI
	// HTTP-boot firmware can fetch the binary over reliable HTTP instead
	// of flaky TFTP (the DHCP server hands HTTP-boot clients that URL).
	// Empty disables the HTTP iPXE route.
	IPXEDir string

	// DefaultImage is the image version to boot. Passed to HandlerConfig.
	DefaultImage string

	// HTTPPort is the port for URL construction. Defaults to 8080.
	HTTPPort int

	// DeployMode enables the full deploy boot sequence.
	DeployMode bool

	// Provider is the MachineProvider for boot decisions.
	Provider model.MachineProvider

	// LogSink receives envelopes posted by agents to /logs/ingest.
	// Production wires this to the site worker's logring; nil
	// disables the endpoint.
	LogSink slogkit.Sink

	// AgentBinPath returns the absolute path the http handler should
	// pass to http.ServeFile when a booting host requests the
	// slagworks-agent binary. Production wires this to the path the
	// slagworks .deb installs at (default /usr/bin/slagworks-agent).
	// A nil func or empty return => 503. Tests can leave it nil to
	// omit the agent endpoint.
	AgentBinPath func() string

	// MachineForIP, when non-nil, resolves a client IP to (mac,
	// machine_id, hostname). Used by the request-logging middleware
	// to scope HTTP boot fetch envelopes. Empty strings
	// for fields the lookup can't resolve.
	MachineForIP func(ip string) (mac, machineID, hostname string)

	// OnRequest, when non-nil, is called once per request from the
	// logging middleware with a coarse route group ("boot" | "files" |
	// "logs" | "other") and the status class ("2xx".."5xx"). The site
	// worker wires this to its Prometheus request counter; the
	// callback keeps this package free of a prometheus dependency.
	OnRequest func(route, status string)
}

// Server wraps an http.Server for boot and image endpoints.
type Server struct {
	cfg     ServerConfig
	addr    string
	log     *slog.Logger
	handler *Handler
}

// Handler returns the boot handler, available after buildMux is called.
func (s *Server) Handler() *Handler {
	return s.handler
}

// Build returns the configured http.Handler without starting a listener.
// Used in tests to attach the server to an httptest.Server.
func (s *Server) Build() http.Handler {
	return s.withRequestLogging(s.buildMux())
}

// statusRecorder captures the HTTP status code written by a wrapped
// handler so the logging middleware can include it in the access log
// without depending on net/http internals. ResponseWriter doesn't
// expose the status, so we shadow WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// withRequestLogging wraps a handler with one access-log envelope per
// request. It scopes envelopes to the requesting machine when
// the source IP resolves so per-machine views see PXE-phase HTTP
// fetches. The catch-all `/` 404 logger remains in buildMux for
// path-mismatch detail; this middleware adds the resolved-machine
// scope on top.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		clientIP := r.RemoteAddr
		if i := strings.LastIndex(clientIP, ":"); i >= 0 {
			clientIP = clientIP[:i]
		}
		// Strip IPv6 brackets that net/http leaves on RemoteAddr
		clientIP = strings.Trim(clientIP, "[]")

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"client_ip", clientIP,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if s.cfg.MachineForIP != nil && clientIP != "" {
			if mac, machineID, hostname := s.cfg.MachineForIP(clientIP); machineID != "" {
				attrs = append(attrs,
					"client_mac", mac,
					"machine_id", machineID,
					"hostname", hostname,
				)
			}
		}

		// Severity tracks status code: 2xx info, 4xx warn, 5xx error.
		switch {
		case rec.status >= 500:
			s.log.Error("httpboot fetch", attrs...)
		case rec.status >= 400:
			s.log.Warn("httpboot fetch", attrs...)
		default:
			s.log.Info("httpboot fetch", attrs...)
		}

		if s.cfg.OnRequest != nil {
			s.cfg.OnRequest(routeGroup(r.URL.Path), statusClass(rec.status))
		}
	})
}

// routeGroup maps a request path to a coarse metrics label. Full paths
// would be unbounded-cardinality (machine UUIDs in /boot/<id>/...), so
// we bucket by the first segment.
func routeGroup(path string) string {
	switch {
	case strings.HasPrefix(path, "/boot"):
		return "boot"
	case strings.HasPrefix(path, "/files"):
		return "files"
	case strings.HasPrefix(path, "/logs"):
		return "logs"
	default:
		return "other"
	}
}

// statusClass buckets an HTTP status into 2xx/3xx/4xx/5xx.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

func NewServer(addr string, cfg ServerConfig) *Server {
	return &Server{
		cfg:  cfg,
		addr: addr,
		log:  slog.Default(),
	}
}

func (s *Server) SetLogger(l *slog.Logger) {
	s.log = l
}

// handleLogsIngest accepts a POST'd ws.LogBatchMsg from the agent and
// pushes each envelope into the configured Sink (typically the site
// worker's logring). Fire-and-forget on the agent side: 2xx on
// successful ingest, 503 if the sink isn't wired, 400 on malformed
// body. Validation is intentionally minimal — the OOB-net trust
// boundary makes auth out of scope for v0 (see plan).
func (s *Server) handleLogsIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.LogSink == nil {
		http.Error(w, "log ingest not available", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var msg ws.LogBatchMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	for _, env := range msg.Envelopes {
		// Stamp received_at if the producer didn't.
		if env.ReceivedAt == nil {
			t := now
			env.ReceivedAt = &t
		}
		s.cfg.LogSink.Push(env)
	}
	w.WriteHeader(http.StatusOK)
}

// handleKernelParams returns the per-machine kernel cmdline string
// from MachineSnapshot.KernelParams (sourced from NetBox). Plain text;
// 204 when the device-type custom field has no value (deploy refuses
// such machines). The agent fetches this at deploy time
// and writes it into the systemd-boot entry.
func (s *Server) handleKernelParams(w http.ResponseWriter, machineID string) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		http.Error(w, "invalid machine_id", http.StatusBadRequest)
		return
	}
	m := s.cfg.Provider.MachineByID(id)
	if m == nil || m.KernelParams == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(m.KernelParams))
}

// handleNetworkScript returns the pre-chosen host network-config script for
// the machine (MachineSnapshot.NetworkScript, matched server-side on NetBox
// identity). Plain text; 204 when no script matched, which the agent treats
// as "fall back to the built-in all-en*-DHCP netplan" (same semantics as
// kernel-params and storage scripts).
func (s *Server) handleNetworkScript(w http.ResponseWriter, machineID string) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		http.Error(w, "invalid machine_id", http.StatusBadRequest)
		return
	}
	m := s.cfg.Provider.MachineByID(id)
	if m == nil || m.NetworkScript == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(m.NetworkScript.Content))
}

// handleNetworkFacts returns the machine's NetBox interface facts
// (MachineSnapshot.Interfaces) as a JSON array. The agent injects these into
// the network-config script via SLAGWORKS_NETWORK_FACTS so the script does
// all topology interpretation (/31 legs, loopback, FRR). Always 200 with a
// JSON array (possibly empty); the script decides what to make of it.
func (s *Server) handleNetworkFacts(w http.ResponseWriter, machineID string) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		http.Error(w, "invalid machine_id", http.StatusBadRequest)
		return
	}
	var facts []model.SnapshotInterface
	if m := s.cfg.Provider.MachineByID(id); m != nil {
		facts = m.Interfaces
	}
	if facts == nil {
		facts = []model.SnapshotInterface{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(facts)
}

// handleHostname returns the machine's deployed hostname (MachineSnapshot.FQDN,
// derived from the NetBox device name by global's hostname template). Plain text;
// 204 when unset, which the agent treats as "leave the image default."
func (s *Server) handleHostname(w http.ResponseWriter, machineID string) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		http.Error(w, "invalid machine_id", http.StatusBadRequest)
		return
	}
	m := s.cfg.Provider.MachineByID(id)
	if m == nil || m.FQDN == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(m.FQDN))
}

func (s *Server) handleStorageScript(w http.ResponseWriter, machineID string) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		http.Error(w, "invalid machine_id", http.StatusBadRequest)
		return
	}

	m := s.cfg.Provider.MachineByID(id)
	if m == nil || m.StorageScript == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(m.StorageScript.Content))
}

func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) == 0 || !json.Valid(body) {
		http.Error(w, "body must be valid JSON", http.StatusBadRequest)
		return
	}

	var payload struct {
		NICs []struct {
			MAC string `json:"mac"`
		} `json:"nics"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid inventory JSON", http.StatusBadRequest)
		return
	}

	var machineID uuid.UUID
	for _, nic := range payload.NICs {
		if id, ok := s.cfg.Provider.MachineIDForMAC(nic.MAC); ok {
			machineID = id
			break
		}
	}
	if machineID == uuid.Nil {
		s.log.Info("inventory report: no local MAC match, forwarding to global for resolution")
	}

	s.cfg.Provider.RecordInventoryReport(machineID, body)
	s.log.Info("inventory report queued in outbox", "machine_id", machineID)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{"status":"accepted"}`)
}

func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	handler := NewHandler(HandlerConfig{
		DefaultImage: s.cfg.DefaultImage,
		ImagesDir:    s.cfg.ImagesDir,
		HTTPPort:     s.cfg.HTTPPort,
		DeployMode:   s.cfg.DeployMode,
	}, s.cfg.Provider)
	handler.SetLogger(s.log)
	s.handler = handler
	mux.Handle("/boot/ipxe", handler)

	requirePOST := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return false
		}
		return true
	}

	requireMAC := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		mac := r.URL.Query().Get("mac")
		if mac == "" {
			mac = r.FormValue("mac")
		}
		if mac == "" {
			http.Error(w, "missing mac", http.StatusBadRequest)
			return "", false
		}
		if !validMAC.MatchString(mac) {
			http.Error(w, "invalid mac", http.StatusBadRequest)
			return "", false
		}
		return mac, true
	}

	mux.HandleFunc("/api/v1/report", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var report model.ProgressReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if report.MachineID == "" {
			http.Error(w, "machine_id required", http.StatusBadRequest)
			return
		}
		if report.OperationID == "" {
			http.Error(w, "op_id required", http.StatusBadRequest)
			return
		}
		if err := handler.provider.HandleProgressReport(&report); err != nil {
			handler.log.Warn("rejecting unbound agent report",
				"machine_id", report.MachineID, "op_id", report.OperationID,
				"event", report.Event, "err", err)
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/api/v1/machines/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/machines/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		machineID := parts[0]
		subPath := parts[1]

		switch subPath {
		case "storage-script":
			s.handleStorageScript(w, machineID)
		case "kernel-params":
			s.handleKernelParams(w, machineID)
		case "network-script":
			s.handleNetworkScript(w, machineID)
		case "network-facts":
			s.handleNetworkFacts(w, machineID)
		case "hostname":
			s.handleHostname(w, machineID)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})

	mux.HandleFunc("/logs/ingest", func(w http.ResponseWriter, r *http.Request) {
		s.handleLogsIngest(w, r)
	})

	mux.HandleFunc("/api/v1/inventory", func(w http.ResponseWriter, r *http.Request) {
		if !requirePOST(w, r) {
			return
		}
		s.handleInventory(w, r)
	})

	mux.HandleFunc("/api/v1/phone-home", func(w http.ResponseWriter, r *http.Request) {
		if !requirePOST(w, r) {
			return
		}
		mac, ok := requireMAC(w, r)
		if !ok {
			return
		}
		handler.MarkVerified(mac)
		w.WriteHeader(http.StatusOK)
	})

	// GET /boot/storage-script?mac={mac} — returns the pre-chosen
	// storage script for the machine, or 204 if none matched global-
	// side at snapshot-build time. The agent re-runs the inventory
	// collector against live hardware and pipes the output to this
	// script to produce a storage layout.
	mux.HandleFunc("/boot/storage-script", func(w http.ResponseWriter, r *http.Request) {
		mac, ok := requireMAC(w, r)
		if !ok {
			return
		}
		m := s.cfg.Provider.MachineByMAC(mac)
		if m == nil || m.StorageScript == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(m.StorageScript.Content))
	})

	mux.HandleFunc("/boot/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		keys := s.cfg.Provider.GetSSHKeys()
		if len(keys) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		for _, k := range keys {
			fmt.Fprintln(w, k)
		}
	})

	if s.cfg.ImagesDir != "" {
		s.log.Info("serving images", "dir", s.cfg.ImagesDir)
		fs := http.FileServer(http.Dir(s.cfg.ImagesDir))
		mux.Handle("/files/images/", http.StripPrefix("/files/images/", fs))

		serveFile := func(urlPath, filename string) {
			mux.HandleFunc(urlPath, func(w http.ResponseWriter, r *http.Request) {
				http.ServeFile(w, r, filepath.Join(s.cfg.ImagesDir, filename))
			})
		}
		// overlay.cpio is no longer served: the BOOT hook is baked into the
		// initrd at build time (no separate /init replacement), so nothing
		// fetches it. systemd-bootx64.efi is still copied to target ESPs.
		serveFile("/files/bin/systemd-bootx64.efi", "systemd-bootx64.efi")
	}

	// Serve the iPXE binaries over HTTP for UEFI HTTP-boot firmware. The
	// DHCP server hands HTTP-boot clients http://<server>/files/ipxe/<name>;
	// this is the matching route. Same files the TFTP server serves, now
	// also over reliable HTTP (TFTP of the ~1MB binary times out and retries
	// on the Supermicro boards we run).
	if s.cfg.IPXEDir != "" {
		s.log.Info("serving iPXE binaries over HTTP", "dir", s.cfg.IPXEDir)
		ipxeFS := http.FileServer(http.Dir(s.cfg.IPXEDir))
		mux.Handle("/files/ipxe/", http.StripPrefix("/files/ipxe/", ipxeFS))
	}

	// The slagworks-agent binary lives at the path the slagworks .deb
	// installs to on the gateway host (typically /usr/bin/slagworks-agent).
	// AgentBinPath returns that path; an empty return triggers a 503
	// with Retry-After so a booting host's BOOT hook retries cheaply
	// while an operator fixes the misconfiguration.
	if s.cfg.AgentBinPath != nil {
		mux.HandleFunc("/files/bin/slagworks-agent", func(w http.ResponseWriter, r *http.Request) {
			if path := s.cfg.AgentBinPath(); path != "" {
				http.ServeFile(w, r, path)
				return
			}
			w.Header().Set("Retry-After", "10")
			http.Error(w, "agent binary not configured", http.StatusServiceUnavailable)
		})
	}

	// Catch-all: log every unmatched URL at INFO before serving the
	// 404. Without this, Go's default ServeMux silently 404s, which
	// is exactly what bit us when iPXE on UEFI hit a mistyped path
	// and we had no idea what URL it was actually requesting.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.log.Info("unmatched HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"remote", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)
		http.NotFound(w, r)
	})

	return mux
}

// ListenAndServe starts the HTTP server on the configured address and shuts
// down cleanly when the context is canceled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve runs the HTTP server on a pre-bound listener. Used by the
// qemutest harness to open the listener inside a per-test network
// namespace via nsListen, then hand it here. Shuts down cleanly on
// ctx cancel; takes ownership of the listener (closes it on shutdown).
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := s.buildMux()

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	s.log.Info("HTTP boot server starting", "addr", ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			s.log.Error("shutting down HTTP server", "err", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
