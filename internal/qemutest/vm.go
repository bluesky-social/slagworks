//go:build integration

package qemutest

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type Firmware int

const (
	UEFI Firmware = iota
	BIOS
)

// iPXE option ROMs. efi-virtio.rom is loaded when Firmware=UEFI so
// OVMF doesn't pick its internal HTTPv4/v6 boot over the NIC's iPXE
// bootstrap. The default SeaBIOS pxe-virtio.rom handles BIOS fine,
// so we leave romfile unset in that case (empty string = QEMU
// default).
//
// The ipxe-qemu package installs it under /usr/lib/ipxe/qemu on Ubuntu
// 24.04 and under /usr/share/qemu on 26.04.
var iPXEEFIROMCandidates = []string{
	"/usr/lib/ipxe/qemu/efi-virtio.rom",
	"/usr/share/qemu/efi-virtio.rom",
}

// iPXEEFIROMPath returns the first candidate that exists. With none present
// it returns the first, so QEMU's own "failed to find romfile" error names a
// path and the fix (install ipxe-qemu) is searchable.
func iPXEEFIROMPath() string {
	for _, p := range iPXEEFIROMCandidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return iPXEEFIROMCandidates[0]
}

// DiskType controls how a disk is presented to the guest.
type DiskType int

const (
	DiskVirtio DiskType = iota
	DiskNVMe
	DiskSATA
)

// ExtraDisk describes an additional disk to attach.
type ExtraDisk struct {
	Type DiskType
	Size string // e.g. "10G"; ignored when Path is set
	// Path, when non-empty, attaches this pre-existing qcow2 file as the
	// disk instead of qemu-img create-ing a fresh one in the VM's tempdir.
	// Used by the preserve-data-disk test to hand a SECOND VM the same
	// data qcow2s an earlier VM wrote its array onto. Size is ignored in
	// this case — the file's own size governs.
	//
	// Ownership: NewVM never deletes a Path-provided disk (cleanup only
	// ever kills the QEMU process; disk files created in-VM live in
	// t.TempDir() and are never explicitly unlinked either, so this is
	// consistent with the existing no-explicit-delete contract). The
	// caller owns the file's lifetime.
	//
	// qcow2 files are exclusively locked by a running QEMU process, so a
	// second VM attaching the same Path must not start until the first
	// VM's process has fully exited — see Stop().
	Path   string
	Serial string
}

// ExtraNIC describes an additional NIC.
type ExtraNIC struct {
	Model string // "virtio-net-pci" (default), "e1000", "e1000e"
	MAC   string
	// Tap, when set, attaches this NIC to a pre-existing tap device inside
	// the netns via `-netdev tap,ifname=<Tap>,script=no` instead of the
	// qemu-bridge-helper bridge path. The caller is responsible for
	// creating the tap and wiring its host side (e.g. enslaving it to a
	// host-side 802.3ad bond — the LACP partner in the bond test). Empty
	// keeps the default behavior: a tap auto-created and enslaved to
	// Network.Bridge by qemu-bridge-helper.
	Tap string
}

type VMConfig struct {
	Firmware Firmware
	MAC      string
	// Network is the per-test bridge + netns the VM attaches to.
	// Required. The qemu subprocess is launched via `ip netns exec
	// <Network.NSName>` so qemu-bridge-helper sees the namespaced
	// bridge.
	Network  *Network
	DiskSize string // e.g. "20G"; default "20G"
	// NoPrimaryDisk omits the harness's historical virtio primary disk.
	// It is used by hardware-shape tests whose real machine boots from an
	// explicitly modelled set of ExtraDisks (for example a two-NVMe RAID1),
	// where injecting an eleventh, unrelated disk would make the storage
	// inventory unlike the machine under test.
	NoPrimaryDisk      bool
	Memory             string // MB; default "2048"
	CPUs               string // default "2"
	ExtraDisks         []ExtraDisk
	ExtraNICs          []ExtraNIC
	SMBIOSUUID         string
	SMBIOSManufacturer string
	SMBIOSProduct      string
	// PreFormatDisk writes an ext4 filesystem onto the primary disk
	// before boot. Used to test the "refuses non-blank disk" safety
	// gate.
	PreFormatDisk bool
	// StartPaused boots the VM with QEMU's -S (CPU stopped until a QMP
	// "cont" or HMP/QMP resume). Test-only control used by the BMC power
	// tests as the deterministic representation of chassis power-off, so a
	// PowerOn via the BMC cannot short-circuit because the machine's status
	// already looks "on". Default false preserves every existing test.
	StartPaused bool

	// BootOrder is the -boot order string ("n" network-first, "c" first
	// disk, etc.). Defaults to "n" (the historical qemutest default),
	// preserving existing test behavior.
	BootOrder string

	// StrictBoot sets -boot strict=on: the firmware only attempts the
	// devices listed in BootOrder and halts rather than falling through to
	// devices outside it. Used to give the BMC power tests a deterministic
	// disk-only boot that cannot be silently green-lit by a network-first
	// firmware fallback. Default false preserves existing tests.
	StrictBoot bool

	// BMCQMPPath, when non-empty, opens a SECOND dedicated QMP socket at
	// this path for the FakeBMC. It exists so the FakeBMC's commands and
	// replies never consume events/replies on the primary test QMP socket
	// (vm.QMP). The path is exposed before Start via VM.BMCQMPPath so a
	// test can construct a FakeBMC that dials it.
	BMCQMPPath string

	// NetBootIndex, when > 0, puts bootindex=<n> on the primary NIC and
	// drops the legacy `-boot order=` argument. OVMF ignores `-boot order`
	// and, with no bootindex anywhere, tries disks before the NIC, so a VM
	// whose disk already carries a bootloader never PXE-boots. Redeploy
	// tests (the storage property suite) set this to 1 so the NIC is first
	// under both firmwares, matching the production "PXE is boot-order #1"
	// invariant. Default 0 preserves every existing test.
	NetBootIndex int

	// ExtraArgs are raw QEMU arguments appended after the harness-built
	// device list. The storage property suite uses them with NoPrimaryDisk
	// to attach disks with a bus, block size, bootindex and discard mode
	// ExtraDisk cannot express.
	ExtraArgs []string

	// BMC, when non-nil, attaches a simulated BMC to the VM via
	// QEMU's ipmi-bmc-extern + isa-ipmi-kcs devices. Inside the guest
	// this surfaces as /dev/ipmi0 once the kernel's ipmi_si +
	// ipmi_devintf modules load (auto-loads on most Ubuntu kernels;
	// the agent's reconcile phase modprobes them explicitly to be
	// safe). See bmcsim.go.
	BMC *BMCSim
}

type VM struct {
	Config         VMConfig
	Serial         *SerialMonitor
	QMP            *QMP
	cmd            *exec.Cmd
	diskPath       string
	extraDiskPaths []string
	nvramPath      string
	qmpPath        string
	ttyS0Path      string
	t              *testing.T
}

// NewVM creates the disk images and prepares QEMU args. Start() must
// be called to actually boot the VM.
func (e *Env) NewVM(t *testing.T, cfg VMConfig) *VM {
	t.Helper()

	if cfg.Network == nil {
		t.Fatalf("qemutest: VMConfig.Network is required")
	}
	if cfg.BMC != nil && cfg.BMC.SOLConfigured() && cfg.BMC.SOLVMPort() == 0 {
		// SOL was requested but the relay port is unset: BMC.Start()
		// hasn't run yet. Silently falling through to the stdio
		// serial (the else branch in buildArgs) would let a SOL-path
		// test PASS while proving nothing about SOL — the marker
		// would arrive via stdio into the same merged serial stream
		// the assertion scans. Rigging bug; fail at construction.
		t.Fatalf("qemutest: BMC configured with SOLEnable but SOLVMPort()==0 — call BMC.Start() before NewVM")
	}
	if cfg.Memory == "" {
		cfg.Memory = "2048"
	}
	if cfg.CPUs == "" {
		cfg.CPUs = "2"
	}
	if cfg.BootOrder == "" {
		cfg.BootOrder = "n"
	}
	if cfg.DiskSize == "" {
		cfg.DiskSize = "20G"
	}

	tmpDir := t.TempDir()

	var diskPath string
	if !cfg.NoPrimaryDisk {
		diskPath = filepath.Join(tmpDir, "disk.qcow2")
		runQemuImg(t, "create", "-f", "qcow2", diskPath, cfg.DiskSize)
	}

	var extraDiskPaths []string
	for i, ed := range cfg.ExtraDisks {
		if ed.Path != "" {
			extraDiskPaths = append(extraDiskPaths, ed.Path)
			continue
		}
		size := ed.Size
		if size == "" {
			size = "10G"
		}
		p := filepath.Join(tmpDir, fmt.Sprintf("extra-disk-%d.qcow2", i))
		runQemuImg(t, "create", "-f", "qcow2", p, size)
		extraDiskPaths = append(extraDiskPaths, p)
	}

	vm := &VM{
		Config:         cfg,
		diskPath:       diskPath,
		extraDiskPaths: extraDiskPaths,
		qmpPath:        filepath.Join(tmpDir, "qmp.sock"),
		ttyS0Path:      filepath.Join(tmpDir, "ttyS0.sock"),
		t:              t,
	}

	if cfg.Firmware == UEFI {
		nvramSrc := "/usr/share/OVMF/OVMF_VARS_4M.fd"
		vm.nvramPath = filepath.Join(tmpDir, "OVMF_VARS.fd")
		data, err := os.ReadFile(nvramSrc)
		if err != nil {
			t.Fatalf("qemutest: read OVMF_VARS from %s: %v", nvramSrc, err)
		}
		if err := os.WriteFile(vm.nvramPath, data, 0644); err != nil {
			t.Fatalf("qemutest: write OVMF_VARS copy: %v", err)
		}
	}

	if cfg.PreFormatDisk && cfg.NoPrimaryDisk {
		t.Fatalf("qemutest: PreFormatDisk requires the primary disk")
	}
	if cfg.PreFormatDisk {
		preFormatDisk(t, diskPath)
	}

	t.Cleanup(vm.cleanup)
	return vm
}

func (vm *VM) buildArgs() []string {
	// NIC ROM selection:
	// - BIOS: use QEMU's default pxe-virtio.rom (SeaBIOS chainloads
	//   it for -boot n). Leave romfile unset.
	// - UEFI: OVMF's default virtio ROM includes HTTPv4/v6 boot,
	//   which tries to GET the DHCP bootfile name over HTTP and
	//   fails against our TFTP flow. Pin the NIC ROM to
	//   efi-virtio.rom (an iPXE build) so boot goes through iPXE
	//   directly, matching real-hardware chainload.
	net0Device := fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", vm.Config.MAC)
	if vm.Config.Firmware == UEFI {
		net0Device += ",romfile=" + iPXEEFIROMPath()
	}
	if vm.Config.NetBootIndex > 0 {
		net0Device += fmt.Sprintf(",bootindex=%d", vm.Config.NetBootIndex)
	}
	// Two serial ports, both captured.
	//   - ttyS0: unix-socket chardev. Carries iPXE/SeaBIOS firmware
	//     output. Test process reads via SerialMonitor.
	//   - ttyS1: routed depending on whether a BMC with SOL is
	//     attached.
	//       * BMC with SOLEnable: TCP chardev connecting to the
	//         qemutest-serialrelay's vm-listen port. The relay tees
	//         the byte stream onto a unix-socket tap (read by
	//         SerialMonitor) and forwards to ipmi_sim's SOL device.
	//         Real-hardware contract: agent writes to ttyS1, BMC SOL
	//         relays — the same contract pop kworkers depend on.
	//       * No BMC, or no SOLEnable: stdio. SerialMonitor reads
	//         QEMU's stdout. Lower-fidelity, but lets tests that
	//         don't care about SOL skip the relay.
	args := []string{
		"-machine", "q35,accel=kvm",
		"-m", vm.Config.Memory,
		"-smp", vm.Config.CPUs,
		"-nographic",
		"-chardev", fmt.Sprintf("socket,id=ttyS0,path=%s,server=on,wait=off", vm.ttyS0Path),
		"-serial", "chardev:ttyS0",
	}
	if vm.Config.BMC != nil && vm.Config.BMC.SOLVMPort() > 0 {
		// reconnect=2: if the relay restarts mid-test (it shouldn't,
		// but defense in depth) QEMU re-establishes the chardev.
		args = append(args,
			"-chardev", fmt.Sprintf("socket,id=ttyS1,host=127.0.0.1,port=%d,%s",
				vm.Config.BMC.SOLVMPort(), chardevReconnect(2)),
			"-serial", "chardev:ttyS1",
		)
	} else {
		args = append(args, "-serial", "stdio")
	}
	args = append(args,
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", vm.qmpPath),
		"-netdev", fmt.Sprintf("bridge,id=net0,br=%s", vm.Config.Network.Bridge),
		"-device", net0Device,
	)
	if !vm.Config.NoPrimaryDisk {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", vm.diskPath))
	}

	// Dedicated second QMP socket for the FakeBMC (when configured). Its
	// own chardev means commands/replies on it never collide with the
	// primary vm.QMP socket the test drives.
	if vm.Config.BMCQMPPath != "" {
		args = append(args, "-qmp",
			fmt.Sprintf("unix:%s,server,nowait", vm.Config.BMCQMPPath))
	}

	for i, ed := range vm.Config.ExtraDisks {
		path := vm.extraDiskPaths[i]
		driveID := fmt.Sprintf("extra%d", i)
		serial := ed.Serial
		if serial == "" {
			serial = fmt.Sprintf("EXTRA%04d", i)
		}
		switch ed.Type {
		case DiskNVMe:
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=%s", path, driveID),
				"-device", fmt.Sprintf("nvme,drive=%s,serial=%s", driveID, serial),
			)
		case DiskSATA:
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=%s", path, driveID),
				"-device", fmt.Sprintf("ide-hd,drive=%s,serial=%s", driveID, serial),
			)
		default:
			// virtio-blk: qcow2 doesn't support a "serial" option on
			// -drive; the serial goes on the -device line.
			args = append(args,
				"-drive", fmt.Sprintf("file=%s,format=qcow2,if=none,id=%s", path, driveID),
				"-device", fmt.Sprintf("virtio-blk-pci,drive=%s,serial=%s", driveID, serial),
			)
		}
	}

	for i, nic := range vm.Config.ExtraNICs {
		netID := fmt.Sprintf("net%d", i+1)
		model := nic.Model
		if model == "" {
			model = "virtio-net-pci"
		}
		// Extra NICs don't PXE-boot (boot flows through net0), but we
		// set romfile=/dev/null so OVMF doesn't enumerate them as
		// boot candidates and try HTTPv4 there. Only the primary NIC
		// participates in boot.
		// romfile= (empty) on EVERY extra NIC regardless of model: extra
		// NICs never boot (boot flows through net0), so we must stop OVMF
		// from enumerating their option ROM and attempting HTTPv4/PXE there.
		// This applies to e1000/e1000e too, not just virtio-net-pci.
		deviceSpec := fmt.Sprintf("%s,netdev=%s,mac=%s,romfile=", model, netID, nic.MAC)
		var netdev string
		if nic.Tap != "" {
			// Pre-created tap: qemu must not run a bring-up/-down script
			// (the caller already wired the tap into a host-side bond).
			netdev = fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", netID, nic.Tap)
		} else {
			netdev = fmt.Sprintf("bridge,id=%s,br=%s", netID, vm.Config.Network.Bridge)
		}
		args = append(args,
			"-netdev", netdev,
			"-device", deviceSpec,
		)
	}

	var smbiosType1 []string
	if vm.Config.SMBIOSUUID != "" {
		smbiosType1 = append(smbiosType1, "uuid="+vm.Config.SMBIOSUUID)
	}
	if vm.Config.SMBIOSManufacturer != "" {
		smbiosType1 = append(smbiosType1, "manufacturer="+vm.Config.SMBIOSManufacturer)
	}
	if vm.Config.SMBIOSProduct != "" {
		smbiosType1 = append(smbiosType1, "product="+vm.Config.SMBIOSProduct)
	}
	if len(smbiosType1) > 0 {
		args = append(args, "-smbios", "type=1,"+strings.Join(smbiosType1, ","))
	}

	if vm.Config.Firmware == UEFI {
		args = append(args,
			"-drive", "if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE_4M.fd",
			"-drive", fmt.Sprintf("if=pflash,format=raw,file=%s", vm.nvramPath),
		)
	}

	// Attach the simulated BMC, if configured. Caller must have
	// called BMC.Start before VM.Start so the chardev's reconnect=10
	// retry actually finds something to talk to.
	if vm.Config.BMC != nil {
		args = append(args, vm.Config.BMC.QEMUArgs()...)
	}

	// Boot order controls (test-only, additive): order defaults to "n"
	// (network-first, the historical default) and strict/start-paused are
	// opt-in, so existing tests are unchanged.
	bootOrder := vm.Config.BootOrder
	if bootOrder == "" {
		bootOrder = "n"
	}
	bootOpts := []string{"order=" + bootOrder}
	if vm.Config.NetBootIndex > 0 {
		// bootindex and the legacy order string are alternatives.
		bootOpts = nil
	}
	if vm.Config.StrictBoot {
		bootOpts = append(bootOpts, "strict=on")
	}
	if len(bootOpts) > 0 {
		args = append(args, "-boot", strings.Join(bootOpts, ","))
	}
	args = append(args, vm.Config.ExtraArgs...)
	if vm.Config.StartPaused {
		args = append(args, "-S")
	}
	return args
}

// Start boots the VM. Serial output flows to SerialMonitor; QMP is
// available via vm.QMP.
func (vm *VM) Start(ctx context.Context) {
	vm.t.Helper()

	args := vm.buildArgs()
	// Run qemu inside the test's network namespace so
	// qemu-bridge-helper sees only the namespaced bridge.
	// `ip netns exec` is just a setns + exec wrapper; no overhead.
	// Operator debugging tip: the equivalent shell is
	// `ip netns exec <ns> qemu-system-x86_64 ...` — handy when
	// attaching strace to a wedged test VM.
	fullArgs := append([]string{"netns", "exec", vm.Config.Network.NSName, "qemu-system-x86_64"}, args...)
	vm.cmd = exec.CommandContext(ctx, "ip", fullArgs...)

	stdout, err := vm.cmd.StdoutPipe()
	if err != nil {
		vm.t.Fatalf("qemutest: stdout pipe: %v", err)
	}
	// Capture QEMU stderr into a buffer so Fatal logs include it
	// when QMP fails to connect (crash on startup).
	var stderrBuf qemuStderrBuffer
	vm.cmd.Stderr = &stderrBuf

	if err := vm.cmd.Start(); err != nil {
		vm.t.Fatalf("qemutest: start QEMU: %v", err)
	}

	// Connect to the ttyS0 unix-socket chardev. QEMU listens with
	// server=on,wait=off so it accepts asynchronously; the socket
	// file may not exist for a few ms after Start().
	ttyS0Conn, err := dialUnixWithRetry(vm.ttyS0Path, 5*time.Second)
	if err != nil {
		vm.t.Fatalf("qemutest: dial ttyS0 socket %s: %v\nQEMU stderr:\n%s",
			vm.ttyS0Path, err, stderrBuf.String())
	}
	vm.t.Cleanup(func() { _ = ttyS0Conn.Close() })

	logger := func(line string) { vm.t.Logf("serial: %s", line) }
	// ttyS1 source: SOL tap when BMC+SOLEnable wired the chardev,
	// otherwise QEMU stdout (the chardev was -serial stdio above).
	// When SOL is taking ttyS1, drain stdout to /dev/null so QEMU's
	// startup banner / occasional warnings don't fill the pipe buffer
	// (~64KB) and block the process.
	var ttyS1Reader io.Reader = stdout
	if vm.Config.BMC != nil && vm.Config.BMC.SOLVMPort() > 0 {
		ttyS1Reader = vm.Config.BMC.SOLTapReader()
		go func() { _, _ = io.Copy(io.Discard, stdout) }()
	}
	vm.Serial = newSerialMonitor(vm.t.Name(), logger, ttyS1Reader, ttyS0Conn)

	qmp, err := newQMP(vm.qmpPath)
	if err != nil {
		vm.t.Fatalf("qemutest: connect QMP: %v\nQEMU stderr:\n%s\nargs: %v",
			err, stderrBuf.String(), args)
	}
	vm.QMP = qmp
}

// dialUnixWithRetry retries a unix socket dial for up to timeout,
// because QEMU creates the socket asynchronously after fork.
func dialUnixWithRetry(path string, timeout time.Duration) (*net.UnixConn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

// WaitForSerial waits for a marker string in serial output.
func (vm *VM) WaitForSerial(ctx context.Context, marker string, timeout time.Duration) {
	vm.t.Helper()
	if err := vm.Serial.WaitFor(ctx, marker, timeout); err != nil {
		vm.t.Logf("--- serial output at failure ---")
		for _, line := range vm.Serial.Lines() {
			vm.t.Logf("  %s", line)
		}
		vm.t.Fatalf("qemutest: %v", err)
	}
	vm.t.Logf("milestone: %s", marker)
}

// BMCQMPPath returns the dedicated FakeBMC QMP socket path. It is known
// as soon as NewVM has run (before Start), so a test can construct a
// FakeBMC bound to this socket before booting the VM.
func (vm *VM) BMCQMPPath() string {
	return vm.Config.BMCQMPPath
}

// WaitForSerialAfter waits for a marker that must appear at a line index
// >= cursor (obtained from Serial.Cursor before an operation). Post-reboot
// assertions must use this rather than WaitForSerial so a marker already
// present before the operation cannot satisfy the wait.
func (vm *VM) WaitForSerialAfter(ctx context.Context, cursor int, marker string, timeout time.Duration) {
	vm.t.Helper()
	if err := vm.Serial.WaitForAfter(ctx, cursor, marker, timeout); err != nil {
		vm.t.Logf("--- serial output at failure ---")
		for _, line := range vm.Serial.Lines() {
			vm.t.Logf("  %s", line)
		}
		vm.t.Fatalf("qemutest: %v", err)
	}
	vm.t.Logf("milestone (after cursor %d): %s", cursor, marker)
}

// Stop terminates the QEMU process via QMP quit and waits for it to exit,
// releasing the qcow2 image lock QEMU holds on every attached disk. Callers
// that need a second VM to open the SAME qcow2 file(s) (e.g. the preserve-
// data-disk test's VM-A -> VM-B handoff) MUST call Stop and let it return
// before starting the second VM — qcow2's exclusive image lock means the
// second qemu-system-x86_64 invocation fails to start otherwise.
//
// Idempotent: safe to call more than once, and safe to call before the
// t.Cleanup(vm.cleanup) teardown runs (cleanup tolerates an already-exited
// process).
func (vm *VM) Stop() {
	vm.t.Helper()
	vm.cleanup()
}

// cleanup terminates the QEMU process, tolerating a process that has
// already exited (e.g. because Stop was called earlier). Registered via
// t.Cleanup and also called directly by Stop, so every step guards against
// a nil/already-gone process instead of assuming first-call state.
func (vm *VM) cleanup() {
	if vm.QMP != nil {
		_ = vm.QMP.Quit()
		_ = vm.QMP.Close()
		vm.QMP = nil
	}
	if vm.cmd != nil && vm.cmd.Process != nil {
		_ = vm.cmd.Process.Kill()
		_ = vm.cmd.Wait()
		vm.cmd = nil
	}
}

// qemuStderrBuffer is a thread-safe buffer used to capture QEMU's
// stderr so we can include it in failure messages.
type qemuStderrBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *qemuStderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *qemuStderrBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// CreateDisk creates a fresh qcow2 image at path with the given size (e.g.
// "6G"). Exported for tests that need a disk file BEFORE any VM exists — the
// preserve-data-disk two-VM test creates the shared data qcow2s this way,
// then attaches the same paths to two VMs in turn via ExtraDisk.Path.
func (e *Env) CreateDisk(t *testing.T, path, size string) {
	t.Helper()
	runQemuImg(t, "create", "-f", "qcow2", path, size)
}

func runQemuImg(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("qemutest: qemu-img %v: %v\n%s", args, err, out)
	}
}

func preFormatDisk(t *testing.T, diskPath string) {
	t.Helper()
	dir := filepath.Dir(diskPath)
	rawPath := filepath.Join(dir, "disk-preformat.raw")
	runQemuImg(t, "convert", "-f", "qcow2", "-O", "raw", diskPath, rawPath)
	if out, err := exec.Command("mkfs.ext4", "-F", rawPath).CombinedOutput(); err != nil {
		t.Fatalf("qemutest: mkfs.ext4 %s: %v\n%s", rawPath, err, out)
	}
	runQemuImg(t, "convert", "-f", "raw", "-O", "qcow2", rawPath, diskPath)
	_ = os.Remove(rawPath)
}
