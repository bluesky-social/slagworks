//go:build integration

package qemutest

import (
	"bufio"
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

// ioMultiWriter is a tiny wrapper to fan-out one write to many
// targets without losing on a partial-write error from any single
// target. Standard library's io.MultiWriter aborts on the first
// short write; we want best-effort. Errors are dropped so a misbehaving
// log target can't bubble up and stop diagnostic capture.
type bestEffortMultiWriter []io.Writer

func (b bestEffortMultiWriter) Write(p []byte) (int, error) {
	for _, w := range b {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

func ioMultiWriter(ws ...io.Writer) io.Writer {
	return bestEffortMultiWriter(ws)
}

// BMCSim is a simulated BMC — an out-of-process `ipmi_sim` (from
// OpenIPMI) running on the test host, paired with QEMU's
// `ipmi-bmc-extern` + `isa-ipmi-kcs` devices to expose `/dev/ipmi0`
// inside the guest.
//
// Architecture:
//
//	guest /dev/ipmi0 ──► KCS interface ──► QEMU chardev
//	                                       │
//	                                       ▼ TCP socket (VM codec)
//	                                  ipmi_sim
//
// We use TCP (not Unix-domain) because QEMU's ipmi-bmc-extern wants a
// chardev that speaks the OpenIPMI "VM" codec, and ipmi_sim's `serial
// ... codec VM` listener accepts TCP only. localhost-only.
//
// One BMCSim per VM. Tests that don't care about BMC ops can leave
// VMConfig.BMC nil and the IPMI device flags are skipped.
//
// Note on user-management coverage: ipmi-bmc-sim (in-process) doesn't
// implement Set User Name/Password, which is exactly what the agent's
// reconcile phase exercises. ipmi_sim does, plus boot params with
// persistence and a configurable FRU. That's why we go out-of-process.
//
// # SOL routing
//
// When SOLEnable is set, BMCSim spawns a `qemutest-serialrelay`
// subprocess inside the test's netns. The relay listens on two TCP
// ports (one for the QEMU VM's serial chardev, one for ipmi_sim) and
// teees the VM->BMC byte stream onto a unix-socket tap which the test
// process reads. The flow:
//
//	guest /dev/ttyS1 ──► QEMU chardev (connect) ──► relay :vm-port
//	                                                  │
//	                                                  ├─► tap (unix) ──► SerialMonitor
//	                                                  │
//	                                                  ▼
//	                            ipmi_sim sol "tcp:127.0.0.1:bmc-port"
//
// This proves the agent's writes to ttyS1 actually traverse the BMC
// SOL path — the contract that real-hardware deploys depend on.
type BMCSim struct {
	t        *testing.T
	tmpDir   string
	cfg      BMCSimConfig
	cmd      *exec.Cmd
	network  *Network // nsName for ip-netns-exec wrapping; required
	ipmiPort int
	lanPort  int // 0 if ExposeLAN was false
	stderr   *qemuStderrBuffer

	// SOL relay state. Populated only when cfg.SOLEnable is true.
	relayCmd       *exec.Cmd
	relayVMPort    int    // QEMU connects to this — exposed via SOLVMPort()
	relayBMCPort   int    // ipmi_sim connects to this; written into the sol device
	relayTapPath   string // unix socket path the test listens on
	relayTapConn   net.Conn
	relayTapMu     sync.Mutex // guards relayTapConn against the Read/Close race
	relayTapServer net.Listener

	// relayLog buffers the relay's post-ready stdout; relayDrained is
	// closed when the drain goroutine EOFs. Close flushes the buffer
	// via t.Logf FROM THE TEST GOROUTINE — a direct t.Logf in the
	// drain goroutine can outlive the test and panic the whole test
	// binary ("Log in goroutine after TestX has completed"), the
	// exact hazard startTFTPSubprocess documents and avoids.
	relayLog     qemuStderrBuffer
	relayDrained chan struct{}

	closeOnce sync.Once
}

// BMCSimConfig knobs the behavior of the simulated BMC. Defaults
// should match what production BMCs look like at first boot, so the
// agent's reconcile sees a meaningful "missing users" gap.
type BMCSimConfig struct {
	// ChassisSerial seeds the FRU's product/chassis serial. The site
	// worker's chunk-4 chassis-serial check reads this via
	// bmclib.Inventory; the agent's reconcile doesn't care.
	ChassisSerial string

	// SeedUsers populates the simulator's user list at startup. If
	// nil, the simulator starts with just the default ADMIN user
	// (slot 1, name "admin", password "test"). This is the
	// "fresh-from-factory" shape — exactly what the agent's reconcile
	// is supposed to fix.
	//
	// Tests that want to exercise drift recovery (e.g. monitor user
	// nuked) seed only ADMIN. Tests that want to exercise the
	// "already provisioned" no-op path seed ADMIN plus monitor.
	SeedUsers []BMCSimUser

	// ExposeLAN, when true, adds a `startlan 1` section to the
	// simulator config so the test process can drive it with
	// `ipmitool -I lanplus -H 127.0.0.1 -p <LANPort()>`.
	//
	// Production agent reconcile uses in-band IPMI (`-I open`); the
	// LAN path is a unit-test convenience that validates the same
	// command sequences against a real BMC simulator without
	// requiring a full QEMU VM. The wire-level semantics are
	// identical for the commands we issue (chassis ctrl, user mgmt,
	// boot params).
	ExposeLAN bool

	// SOLEnable, when true, wires the VM's ttyS1 serial port through
	// ipmi_sim's SOL device so `ipmitool -I lanplus ... sol
	// activate` carries real bytes the guest emits. Implementation:
	// BMCSim spawns a `qemutest-serialrelay` subprocess inside the
	// netns; the QEMU VM connects to its vm-listen port, ipmi_sim
	// connects to its bmc-listen port, the relay tees the VM->BMC
	// stream onto a unix-socket tap that the test reads (via
	// SerialMonitor).
	//
	// Implies ExposeLAN — SOL only flows over LAN sessions.
	SOLEnable bool

	// SerialRelayBin is the absolute path to the qemutest-serialrelay
	// binary. When SOLEnable is true and this is empty, BMCSim looks
	// for the binary alongside the test process's working directory
	// or in $PATH.
	SerialRelayBin string

	// Network, when non-nil, scopes the ipmi_sim subprocess to the
	// per-test network namespace via `ip netns exec`. Required when
	// the BMC is attached to a qemu VM (which now also runs inside
	// the netns), because qemu's chardev connects to
	// 127.0.0.1:<port> — the netns's loopback, not the host's. Both
	// processes must live in the same netns to share the same
	// loopback. Standalone tests without qemu (TestBMCSim_Standalone)
	// leave this nil and run in host netns.
	Network *Network
}

// BMCSimUser is one entry the simulator will start with.
type BMCSimUser struct {
	Slot      int    // 1..63
	Name      string // <= 16 chars
	Password  string // <= 20 chars
	Privilege string // "user"|"operator"|"admin"|"callback"
	Enabled   bool
}

// BMCSocket returns the TCP address QEMU's ipmi-bmc-extern chardev
// connects to. Empty until Start() returns.
func (b *BMCSim) BMCSocket() string {
	return fmt.Sprintf("127.0.0.1:%d", b.ipmiPort)
}

// NewBMCSim configures a simulated BMC. Start() actually launches
// ipmi_sim. Two-step so the caller can query the address (BMCSocket)
// before passing it to QEMU args.
func (e *Env) NewBMCSim(t *testing.T, cfg BMCSimConfig) *BMCSim {
	t.Helper()

	if _, err := exec.LookPath("ipmi_sim"); err != nil {
		t.Fatalf("qemutest: ipmi_sim not in PATH (install openipmi): %v", err)
	}

	if cfg.ChassisSerial == "" {
		cfg.ChassisSerial = "QEMU-DEFAULT-SERIAL"
	}
	// Allocate a free TCP port up front so the config file references
	// the same port the test eventually queries via BMCSocket().
	port, err := freeTCPPort()
	if err != nil {
		t.Fatalf("qemutest: alloc bmc port: %v", err)
	}

	var lanPort int
	if cfg.ExposeLAN || cfg.SOLEnable {
		// IPMI-over-LAN is UDP per IPMI 2.0 §13. Allocate a UDP-eligible
		// port; freeTCPPort returns one the kernel just released, which
		// is fine for UDP too in practice. SOL implies LAN — the SOL
		// session arrives over the same RMCP+ channel.
		lanPort, err = freeTCPPort()
		if err != nil {
			t.Fatalf("qemutest: alloc bmc lan port: %v", err)
		}
	}

	tmpDir := t.TempDir()
	bmc := &BMCSim{
		t:        t,
		tmpDir:   tmpDir,
		cfg:      cfg,
		network:  cfg.Network,
		ipmiPort: port,
		lanPort:  lanPort,
	}
	t.Cleanup(bmc.Close)
	return bmc
}

// Start launches ipmi_sim. Blocks until the simulator's TCP listener
// accepts connections.
func (b *BMCSim) Start(ctx context.Context) {
	b.t.Helper()

	// Use a stable per-test path under /tmp/bmcsim-debug so failed
	// runs can be inspected post-mortem on cpu2 (t.TempDir() cleans
	// up too aggressively for that). The test name is unique per
	// test, so concurrent tests don't collide.
	debugDir := filepath.Join("/tmp/bmcsim-debug", strings.ReplaceAll(b.t.Name(), "/", "_"))
	_ = os.RemoveAll(debugDir)
	if err := os.MkdirAll(debugDir, 0755); err == nil {
		b.tmpDir = debugDir
	}
	b.t.Logf("bmcsim work dir: %s", b.tmpDir)

	confPath := filepath.Join(b.tmpDir, "bmc.conf")
	cmdPath := filepath.Join(b.tmpDir, "bmc.commands")
	statePath := filepath.Join(b.tmpDir, "state")

	if err := os.MkdirAll(statePath, 0755); err != nil {
		b.t.Fatalf("qemutest: bmcsim state dir: %v", err)
	}

	// Bring up the SOL relay subprocess BEFORE rendering the ipmi_sim
	// conf — the conf embeds the relay's bmc-listen port. The relay
	// runs inside the netns so its 127.0.0.1 listener is reachable
	// from ipmi_sim and from the QEMU VM. The unix tap path is in the
	// debug dir so it survives test cleanup for post-mortem.
	if b.cfg.SOLEnable {
		b.startSerialRelay(ctx)
	}

	// FRU is only used when the test needs the chassis serial check
	// (chunk 4 path). When ChassisSerial is left at the default
	// "QEMU-DEFAULT-SERIAL", we skip the FRU step entirely because
	// ipmi_sim's format validation silently kills the listener bind
	// on parse errors and our hand-rolled FRU format isn't 100%
	// compatible across simulator versions. Tests that opt in by
	// setting a non-default ChassisSerial accept that risk.
	fruPath := ""
	if b.cfg.ChassisSerial != "" && b.cfg.ChassisSerial != "QEMU-DEFAULT-SERIAL" {
		fruPath = filepath.Join(b.tmpDir, "fru.bin")
		if err := writeBMCFRU(fruPath, b.cfg.ChassisSerial); err != nil {
			b.t.Fatalf("qemutest: bmcsim fru file: %v", err)
		}
	}

	if err := os.WriteFile(confPath, []byte(b.renderConf()), 0644); err != nil {
		b.t.Fatalf("qemutest: bmcsim conf: %v", err)
	}
	if err := os.WriteFile(cmdPath, []byte(b.renderCmds(fruPath)), 0644); err != nil {
		b.t.Fatalf("qemutest: bmcsim cmd file: %v", err)
	}

	ipmiSimArgs := []string{
		"-c", confPath,
		"-f", cmdPath,
		"-s", statePath,
		"-n", // no console on stdin/stdout; required for non-interactive launch
		"-d", // debug: print bind addresses + per-request trace to stderr
	}
	args := ipmiSimArgs
	if b.network != nil {
		// Wrap with `ip netns exec <ns> ipmi_sim ...` so the
		// simulator binds 127.0.0.1:<port> inside the per-test netns.
		// qemu (also running inside the netns) connects
		// to the same 127.0.0.1, sharing the netns lo.
		full := append([]string{"netns", "exec", b.network.NSName, "ipmi_sim"}, ipmiSimArgs...)
		b.cmd = exec.CommandContext(ctx, "ip", full...)
	} else {
		b.cmd = exec.CommandContext(ctx, "ipmi_sim", ipmiSimArgs...)
	}
	b.stderr = &qemuStderrBuffer{}
	// Log to a file too — helpful when the buffered capture comes
	// up empty and we need to know if ipmi_sim said anything at all.
	logPath := filepath.Join(b.tmpDir, "ipmi_sim.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		b.t.Fatalf("qemutest: bmcsim log file: %v", err)
	}
	// Tee to both buffer + file; the file outlives the test for
	// post-mortem inspection on cpu2.
	b.cmd.Stderr = ioMultiWriter(b.stderr, logFile)
	b.cmd.Stdout = ioMultiWriter(b.stderr, logFile)

	if err := b.cmd.Start(); err != nil {
		b.t.Fatalf("qemutest: start ipmi_sim: %v", err)
	}
	b.t.Logf("ipmi_sim launched: pid=%d argv=%v", b.cmd.Process.Pid, b.cmd.Args)

	// Wait for ipmi_sim to be ready. Critical subtlety: we MUST NOT
	// probe the BMCSocket port with a test net.Dial here. ipmi_sim's
	// `serial 15 ... codec VM` listener accepts a single connection
	// at a time — that's the whole VM-codec contract. A health-check
	// dial would (a) consume the slot QEMU's about to take, or (b)
	// trigger ipmi_sim to send the VERSION handshake (0xFF 0x01) into
	// a socket we're about to close, leaving ipmi_sim in a half-
	// handshake state when QEMU's real connection arrives.
	//
	// Symptom of getting this wrong: QEMU connects but never reaches
	// `connected=true` because the version exchange races. The kernel
	// then sees every IPMI command synthesized as 0xd2
	// (IPMI_CC_BMC_INIT_IN_PROGRESS) by QEMU's bmc-extern driver,
	// retries 5x, and gives up with "There appears to be no BMC at
	// this location" — exactly the failure phase 2.4 hit before this
	// fix. See README.vm in the OpenIPMI source tree for the codec
	// contract.
	//
	// Instead: poll the kernel's socket table for the LISTEN bind.
	// ipmi_sim has three documented ways to die-or-degrade silently
	// (conf parse errors killing the bind, missing set_working_mc,
	// mc_set_fru_file aborting the rest of the cmd file) — all of
	// which leave the process alive with NO listener, which the old
	// "still alive after 1s" check waved through and surfaced minutes
	// later as QEMU's "There appears to be no BMC at this location".
	// Reading /proc/net/tcp (inside the netns when set) is a positive
	// bind confirmation with zero connection attempts.
	const grace = 5 * time.Second
	const checkInterval = 50 * time.Millisecond
	deadline := time.Now().Add(grace)
	for {
		if b.cmd.ProcessState != nil {
			logContents := readFileBest(filepath.Join(b.tmpDir, "ipmi_sim.log"))
			b.t.Fatalf("qemutest: ipmi_sim exited during startup:\nargs=%v\noutput:\n%s\nlog:\n%s\nconf:\n%s\ncmd file:\n%s",
				args, b.stderr.String(), logContents,
				readFileBest(confPath), readFileBest(cmdPath))
		}
		if b.portBound("tcp", b.ipmiPort) {
			break
		}
		if time.Now().After(deadline) {
			logContents := readFileBest(filepath.Join(b.tmpDir, "ipmi_sim.log"))
			b.t.Fatalf("qemutest: ipmi_sim never bound its VM-codec TCP listener on 127.0.0.1:%d within %s "+
				"(process alive but not listening — conf parse error or set_working_mc failure; "+
				"see the silent-failure notes on renderConf).\noutput:\n%s\nlog:\n%s\nconf:\n%s\ncmd file:\n%s",
				b.ipmiPort, grace, b.stderr.String(), logContents,
				readFileBest(confPath), readFileBest(cmdPath))
		}
		time.Sleep(checkInterval)
	}
	// The LAN (RMCP+/SOL) listener is UDP and binds from the same
	// startlan block; confirm it too when configured. A lost UDP bind
	// is otherwise undetectable (no error on the sender side).
	if b.cfg.ExposeLAN || b.cfg.SOLEnable {
		for !b.portBound("udp", b.lanPort) {
			if time.Now().After(deadline) {
				b.t.Fatalf("qemutest: ipmi_sim never bound its LAN UDP listener on 127.0.0.1:%d within %s\noutput:\n%s",
					b.lanPort, grace, b.stderr.String())
			}
			time.Sleep(checkInterval)
		}
	}
}

// portBound reports whether 127.0.0.1:<port> is bound for the given
// proto ("tcp" → LISTEN state, "udp" → any bind) — read from the
// kernel socket table of the netns ipmi_sim runs in. A read/parse
// failure returns false (the caller keeps polling until its deadline,
// then fails with full diagnostics).
func (b *BMCSim) portBound(proto string, port int) bool {
	if port == 0 {
		return false
	}
	procFile := "/proc/net/" + proto
	var out []byte
	var err error
	if b.network != nil {
		out, err = NSExec(b.network.NSName, "cat", procFile).Output()
	} else {
		out, err = os.ReadFile(procFile)
	}
	if err != nil {
		return false
	}
	// /proc/net/{tcp,udp} rows: "sl local_address rem_address st ..."
	// local_address is hex IP:PORT; st 0A = TCP LISTEN, 07 = UDP.
	wantPort := fmt.Sprintf(":%04X", port)
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[1], wantPort) {
			continue
		}
		if proto == "tcp" && f[3] != "0A" {
			continue
		}
		return true
	}
	return false
}

// readFileBest returns the file contents or a string description of
// the read error. Best-effort helper for failure diagnostics.
func readFileBest(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return fmt.Sprintf("(read %s failed: %v)", p, err)
	}
	return string(data)
}

// renderConf produces the lan.conf-format file ipmi_sim reads at
// startup. ipmi_sim's parser is fragile against multi-line comment
// blocks — keep the rendered file terse, no inline comments. Doc
// lives in this Go function instead.
//
// Required ordering:
//  1. name "..."
//  2. set_working_mc <addr>  (without this, startlan/serial fail
//     silently with no stderr — discovered the hard way)
//  3. serial / startlan blocks (apply to the working MC)
//  4. user N ...   (seed users; honored only when state dir is
//     empty, as per upstream behavior)
//
// Per upstream's lanserv/lan.conf example
// (cminyard/openipmi).
func (b *BMCSim) renderConf() string {
	var sb strings.Builder
	sb.WriteString("name \"qemubmc\"\n\n")
	sb.WriteString("set_working_mc 0x20\n\n")
	fmt.Fprintf(&sb, "serial 15 127.0.0.1 %d codec VM\n\n", b.ipmiPort)

	if b.cfg.ExposeLAN || b.cfg.SOLEnable {
		fmt.Fprintf(&sb, `startlan 1
  addr 127.0.0.1 %d
  priv_limit admin
  allowed_auths_callback none md2 md5 straight
  allowed_auths_user     none md2 md5 straight
  allowed_auths_operator none md2 md5 straight
  allowed_auths_admin    none md2 md5 straight
  guid a123456789abcdefa123456789abcdef
endlan

`, b.lanPort)
	}

	if b.cfg.SOLEnable {
		// `sol <device> <baud>` enables SOL on interface 1 of the MC.
		// 115200 matches what production hardware uses (ttyS1 on
		// pop1+pop3 AMI BMCs); the actual baud is irrelevant in TCP
		// mode but must be a valid value for the parser.
		//
		// Device path MUST be quoted: ipmi_sim's lan.conf parser
		// rejects unquoted strings here with "string value must start
		// with " or '" — discovered by integration test failure.
		//
		// The relay's bmc port is allocated in Start() before this
		// call, so b.relayBMCPort is non-zero by the time we render.
		fmt.Fprintf(&sb, "sol \"tcp:127.0.0.1:%d\" 115200\n\n", b.relayBMCPort)
	}

	// Seed users. Slot 1 must always exist (anonymous user). Slot 2
	// gets an ADMIN by default unless the caller's seed list already
	// covers it.
	users := b.cfg.SeedUsers
	hasSlot2Admin := false
	for _, u := range users {
		if u.Slot == 2 && strings.EqualFold(u.Privilege, "admin") {
			hasSlot2Admin = true
			break
		}
	}
	sb.WriteString(`user 1 true  ""        "test" admin    10       none md2 md5 straight` + "\n")
	if !hasSlot2Admin {
		sb.WriteString(`user 2 true  "ADMIN"   "test" admin    10       none md2 md5 straight` + "\n")
	}
	for _, u := range users {
		fmt.Fprintf(&sb, `user %d %s "%s" "%s" %s 10 none md2 md5 straight`+"\n",
			u.Slot, boolStr(u.Enabled), u.Name, u.Password, u.Privilege)
	}

	// console + trailing newlines: empirically, ipmi_sim's config
	// parser silently fails to bind LAN/serial listeners when the
	// file doesn't end with both a `console` directive and a
	// trailing blank line. Discovered by bisecting against a known-
	// good config from upstream's lan.conf example. Don't remove
	// these without testing on cpu2.
	sb.WriteString("\nconsole 127.0.0.1 0\n\n")
	return sb.String()
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// renderCmds produces the startup-command file (ipmi_sim's -f arg).
// Brings up the BMC management controller. Optionally points it at
// our FRU data so bmclib's Inventory call can read the chassis
// serial — but only when the FRU file is reachable AND ipmi_sim's
// version actually accepts the format we wrote. Empirically the
// stock Ubuntu 24.04 ipmi_sim silently aborts the rest of the cmd
// file (including listener binds) if mc_set_fru_file fails to load
// the file; that makes BMC chassis-serial verification unreliable
// in this harness today. Tests that don't depend on serial leave
// fruPath empty.
//
// Per upstream's ipmisim1.emu example: mc_setbmc declares the BMC
// address, mc_add registers the MC with its IPMI metadata, and
// mc_enable turns it on. Order matters; mc_add fails if the slot
// isn't designated as the BMC first.
func (b *BMCSim) renderCmds(fruPath string) string {
	var s string
	s += "mc_setbmc 0x20\n"
	// mc_add args: slot, ipmb-channel, sdr-flag, device-id, fw-rev1,
	// fw-rev2, ipmi-version, mfg-id, product-id, persist-flag.
	// Values copied from upstream's ipmisim1.emu.
	s += "mc_add 0x20 0 no-device-sdrs 0x23 9 8 0x9f 0x1291 0xf02 persist_sdr\n"
	if fruPath != "" {
		s += fmt.Sprintf("mc_set_fru_file 0x20 0 %s\n", fruPath)
	}
	s += "mc_enable 0x20\n"
	return s
}

// QEMUArgs returns the QEMU args needed to attach this BMC sim to a
// VM. Caller should append these to its own arg list.
func (b *BMCSim) QEMUArgs() []string {
	return []string{
		"-chardev", fmt.Sprintf("socket,id=ipmi0,host=127.0.0.1,port=%d,%s", b.ipmiPort, chardevReconnect(10)),
		"-device", "ipmi-bmc-extern,id=bmc0,chardev=ipmi0",
		"-device", "isa-ipmi-kcs,bmc=bmc0",
	}
}

// Close shuts down the simulator. Idempotent and Cleanup-safe.
func (b *BMCSim) Close() {
	b.closeOnce.Do(func() {
		if b.cmd != nil && b.cmd.Process != nil {
			_ = b.cmd.Process.Kill()
			_ = b.cmd.Wait()
		}
		if b.relayCmd != nil && b.relayCmd.Process != nil {
			_ = b.relayCmd.Process.Kill()
			_ = b.relayCmd.Wait()
			// Flush the buffered relay output from THIS (test)
			// goroutine once the drain goroutine EOFs.
			if b.relayDrained != nil {
				select {
				case <-b.relayDrained:
				case <-time.After(2 * time.Second):
				}
				if out := strings.TrimSpace(b.relayLog.String()); out != "" {
					b.t.Logf("serialrelay output:\n%s", out)
				}
			}
		}
		if b.relayTapServer != nil {
			_ = b.relayTapServer.Close()
		}
		b.relayTapMu.Lock()
		if b.relayTapConn != nil {
			_ = b.relayTapConn.Close()
		}
		b.relayTapMu.Unlock()
		if b.relayTapPath != "" {
			_ = os.Remove(b.relayTapPath)
		}
	})
}

// SOLVMPort returns the relay's vm-listen port for the QEMU chardev to
// connect to. Zero unless SOLEnable was set and Start has been called.
func (b *BMCSim) SOLVMPort() int { return b.relayVMPort }

// SOLConfigured reports whether this BMC was configured with SOL —
// used by VM.buildArgs to distinguish "no SOL requested" (stdio serial
// is correct) from "SOL requested but Start() not yet called"
// (a rigging bug that must fail loudly, not silently degrade the SOL
// path under test to stdio).
func (b *BMCSim) SOLConfigured() bool { return b.cfg.SOLEnable }

// SOLTapReader returns an io.Reader yielding the byte stream the VM
// emits on ttyS1 (which the BMC sees on its SOL device). The reader
// is lazy: the first Read() call will block until the relay
// subprocess dials the unix tap (which itself happens after QEMU's
// chardev connects to the relay). This deferred-Accept shape is
// load-bearing — the test sets up SerialMonitor BEFORE QEMU launches,
// so a synchronous Accept here would deadlock.
//
// Returns nil if SOLEnable was not set.
func (b *BMCSim) SOLTapReader() io.Reader {
	if !b.cfg.SOLEnable {
		return nil
	}
	return &lazyTapReader{b: b}
}

// lazyTapReader defers the unix Accept until the first Read call.
// QEMU has to be running before the relay dials the tap, so the
// SerialMonitor's read goroutine drives the Accept once it spins up.
type lazyTapReader struct {
	b    *BMCSim
	conn net.Conn
}

func (r *lazyTapReader) Read(p []byte) (int, error) {
	if r.conn == nil {
		c, err := r.b.relayTapServer.Accept()
		if err != nil {
			return 0, fmt.Errorf("bmcsim sol tap accept: %w", err)
		}
		r.b.relayTapMu.Lock()
		r.b.relayTapConn = c
		r.b.relayTapMu.Unlock()
		r.conn = c
	}
	return r.conn.Read(p)
}

// startSerialRelay spawns the qemutest-serialrelay subprocess inside
// the test's network namespace, opens the unix tap listener that the
// relay will dial, and parses the relay's "ready vm=N bmc=M\n" line
// to learn the dynamically-allocated ports.
func (b *BMCSim) startSerialRelay(ctx context.Context) {
	b.t.Helper()
	if b.network == nil {
		b.t.Fatalf("qemutest: bmcsim SOLEnable requires a Network for netns isolation")
	}

	binPath := b.cfg.SerialRelayBin
	if binPath == "" {
		// Best-effort defaults: alongside images dir (CI shape), or in
		// $PATH. CI workflow builds the relay into RUNNER_TEMP/images
		// next to qemutest-tftpd; resolveSerialRelayBin handles that.
		binPath = resolveSerialRelayBin(b.t)
	}

	// Tap socket path: AF_UNIX sun_path is 108 bytes max. b.tmpDir
	// resolved to /tmp/bmcsim-debug/<long-test-name>/ earlier in
	// Start(), and concatenating sol-tap.sock onto a long test name
	// pushes us over the limit (failure shape: bind: invalid
	// argument). Use a flat /tmp path keyed off ipmiPort, which is
	// per-BMCSim-unique. The Close() path unlinks the file.
	tapPath := filepath.Join("/tmp", fmt.Sprintf("slag-soltap-%d.sock", b.ipmiPort))
	_ = os.Remove(tapPath)

	tapL, err := net.Listen("unix", tapPath)
	if err != nil {
		b.t.Fatalf("qemutest: bmcsim sol tap listen %q: %v", tapPath, err)
	}
	b.relayTapServer = tapL
	b.relayTapPath = tapPath

	cmd := NSExec(b.network.NSName, binPath,
		"--vm-listen", "127.0.0.1:0",
		"--bmc-listen", "127.0.0.1:0",
		"--tap", tapPath,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		b.t.Fatalf("qemutest: bmcsim sol relay stdout: %v", err)
	}
	cmd.Stderr = os.Stderr // diagnostic; relay's stderr is just log.Printf

	if err := cmd.Start(); err != nil {
		b.t.Fatalf("qemutest: bmcsim sol relay start: %v", err)
	}
	b.relayCmd = cmd
	b.t.Logf("serial relay launched: pid=%d tap=%s", cmd.Process.Pid, tapPath)

	// Read first line: "ready vm=N bmc=M\n". Use bufio so a kernel
	// scheduling race doesn't deliver the line in a partial Read
	// (which would make Sscanf miss).
	type readyResult struct {
		line string
		err  error
	}
	readyCh := make(chan readyResult, 1)
	bufReader := bufio.NewReader(stdout)
	go func() {
		line, err := bufReader.ReadString('\n')
		if err != nil {
			readyCh <- readyResult{err: fmt.Errorf("read first line: %w", err)}
			return
		}
		readyCh <- readyResult{line: line}
	}()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		b.t.Fatalf("qemutest: bmcsim sol relay readiness: ctx cancelled: %v", ctx.Err())
	case rr := <-readyCh:
		if rr.err != nil {
			_ = cmd.Process.Kill()
			b.t.Fatalf("qemutest: bmcsim sol relay readiness: %v", rr.err)
		}
		var vmPort, bmcPort int
		if _, err := fmt.Sscanf(rr.line, "ready vm=%d bmc=%d", &vmPort, &bmcPort); err != nil {
			_ = cmd.Process.Kill()
			b.t.Fatalf("qemutest: bmcsim sol relay first line %q: %v", rr.line, err)
		}
		b.relayVMPort = vmPort
		b.relayBMCPort = bmcPort
		b.t.Logf("serial relay ready: vm=%d bmc=%d", vmPort, bmcPort)
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		b.t.Fatalf("qemutest: bmcsim sol relay readiness: 5s timeout")
	}

	// Drain remaining stdout (relay's log.Printf lines) into a
	// thread-safe buffer; Close flushes it via t.Logf from the test
	// goroutine (see the relayLog field comment for why a direct
	// t.Logf here would be a panic hazard). Drain from bufReader so
	// any bytes the readiness goroutine buffered past the "ready ..."
	// line still surface.
	b.relayDrained = make(chan struct{})
	go func() {
		defer close(b.relayDrained)
		buf := make([]byte, 4096)
		for {
			n, err := bufReader.Read(buf)
			if n > 0 {
				_, _ = b.relayLog.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}

// resolveSerialRelayBin locates the qemutest-serialrelay binary. CI
// builds it into RUNNER_TEMP/images alongside qemutest-tftpd; local
// runs may have it on $PATH or built into the test working dir.
func resolveSerialRelayBin(t *testing.T) string {
	t.Helper()
	// 1. SLAGWORKS_IMAGES_DIR/qemutest-serialrelay (CI shape).
	if dir := os.Getenv("SLAGWORKS_IMAGES_DIR"); dir != "" {
		p := filepath.Join(dir, "qemutest-serialrelay")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 2. $PATH.
	if p, err := exec.LookPath("qemutest-serialrelay"); err == nil {
		return p
	}
	t.Fatalf("qemutest: qemutest-serialrelay binary not found (set SLAGWORKS_IMAGES_DIR or build it into $PATH)")
	return ""
}

// freeTCPPort asks the kernel for a free TCP port by binding to :0
// and reading the assigned port. The OS may reuse this port for
// another process between Close and ipmi_sim's bind; tests run
// sequentially per VM so the race is bounded.
func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// writeBMCFRU writes a minimal IPMI FRU image with a Product Info
// area whose serial-number field is `serial`. ipmi_sim reads this
// at startup via mc_set_fru_file; bmclib's Inventory call returns
// it as Device.Serial.
//
// Format reference: IPMI Platform Management FRU Information Storage
// Definition v1.2. We write Common Header + Product Info area only;
// other areas (Internal Use, Chassis Info, Board Info, MultiRecord)
// are zero-offset and absent.
func writeBMCFRU(path, serial string) error {
	if len(serial) > 64 {
		serial = serial[:64]
	}

	// Product Info area:
	//   1: format version (1)
	//   1: area length in 8-byte units
	//   1: language code (0 = English)
	//   1: manufacturer string (type/length byte = 0xC0 + len, then bytes)
	//   1: product name (same shape)
	//   1: product part/model number (same shape)
	//   1: product version (same shape)
	//   1: product serial number (the field we care about)
	//   1: asset tag (empty)
	//   1: FRU file ID (empty)
	//   1: end-of-fields marker (0xC1)
	//   pad to multiple of 8
	//   1: checksum byte (zero-sum of the area)
	tlString := func(s string) []byte {
		// Type/length byte: top 2 bits = 0b11 (8-bit ASCII), bottom 6 bits = length.
		b := []byte{0xC0 | byte(len(s))}
		return append(b, []byte(s)...)
	}
	emptyTL := byte(0xC0) // empty 8-bit ASCII string

	productBody := []byte{
		1,    // format version
		0,    // length placeholder, fixed up below
		0x00, // language code
	}
	productBody = append(productBody, tlString("Slagworks")...)
	productBody = append(productBody, tlString("QEMU BMC Sim")...)
	productBody = append(productBody, emptyTL) // part/model
	productBody = append(productBody, tlString("v1")...)
	productBody = append(productBody, tlString(serial)...) // serial
	productBody = append(productBody, emptyTL)             // asset tag
	productBody = append(productBody, emptyTL)             // FRU file ID
	productBody = append(productBody, 0xC1)                // end of fields
	for len(productBody)%8 != 7 {                          // leave one byte for checksum
		productBody = append(productBody, 0x00)
	}
	// Length in 8-byte units (after we add the checksum byte).
	productBody[1] = byte((len(productBody) + 1) / 8)
	productBody = append(productBody, zeroSumChecksum(productBody))

	// Common header: 8 bytes.
	//   0: format version (1)
	//   1: internal use area offset (0)
	//   2: chassis info area offset (0)
	//   3: board info area offset (0)
	//   4: product info area offset (in 8-byte units, after header)
	//   5: multirecord area offset (0)
	//   6: pad (0)
	//   7: checksum
	header := []byte{1, 0, 0, 0, 1 /* product at offset 8 */, 0, 0, 0}
	header[7] = zeroSumChecksum(header[:7])

	full := append(header, productBody...)
	return os.WriteFile(path, full, 0644)
}

// zeroSumChecksum returns the byte that, when added to the sum of
// `data` mod 256, yields zero. IPMI's standard area checksum.
func zeroSumChecksum(data []byte) byte {
	var sum byte
	for _, b := range data {
		sum += b
	}
	return -sum
}
