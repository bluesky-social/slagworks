//go:build integration

package qemutest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestRecoverySSHD_OnCmdline exercises the cmdline trigger leg of
// recovery mode: when slagworks.recovery is on the kernel cmdline (via
// the deploy op's params.recovery=true), the BOOT hook drops
// directly into recovery_drop() before invoking the deploy agent. We
// generate a keypair, append it to the canonical initrd via a cpio
// overlay, boot, then ssh in to prove the baked authorized_keys is
// loaded and sshd accepts the key.
//
// Runtime: ~3-5 min — no deploy, no agent run, just bring the initrd
// up far enough to land in recovery_drop.
func TestRecoverySSHD_OnCmdline(t *testing.T) {
	t.Parallel()

	env, network, signer, fingerprint, images := setupRecoveryTest(t, "noble-pxe")

	systemUUID := "12345678-1234-1234-1234-bbbbbbbbbb02"
	mac := "52:54:00:de:ad:b3"
	machineID := uuid.New()
	slug := "pop3-recovery-cmdline"

	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	deployOp.Params = json.RawMessage(`{"recovery":true}`)

	pushRecoverySnap(t, env, network, slug, mac, systemUUID, machineID, deployOp)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	vm.Start(ctx)

	vm.WaitForSerial(ctx, "RECOVERY MODE: slagworks.recovery on cmdline", qemutest.Budgets.RecoveryMarkerLong)
	// Wait for the line that CONTAINS the IP, not the prior "ready"
	// banner — there's a tiny gap between the two prints in the
	// BOOT hook, and verifyRecoverySSH races the buffered serial
	// stream right after WaitFor returns. Pre-netns-isolation timing made
	// this race usually go the test's way; netns isolation's slightly
	// different timing surfaces it. Match the IP-bearing line
	// directly and the IP is guaranteed buffered when WaitFor wakes.
	vm.WaitForSerial(ctx, "slagworks:   ssh root@", qemutest.Budgets.RecoveryMarkerShort)

	// Sanity: agent never ran (cmdline branch fired before chroot).
	// The agent logs `{"...","msg":"agent starting",...}` (slogkit
	// envelope) as its first line; match that substring so the negative
	// assertion doesn't pass vacuously against the old plain-text marker.
	if vm.Serial.Contains(`"msg":"agent starting"`) {
		t.Error("recovery cmdline did not short-circuit before the deploy agent")
	}

	// RIGGING SELF-CHECK before touching sshd: the recovery hook prints
	// each installed key's fingerprint; our ephemeral test key MUST be
	// among them. If the cpio-overlay injection broke (the
	// unaligned-append bug: kernel silently drops the overlay and the
	// canonical keys serve instead), this fails with an unambiguous
	// "test key never installed" instead of an sshd auth mystery.
	if !vm.Serial.Contains(fingerprint) {
		t.Fatalf("test key fingerprint %s not printed by recovery_drop — "+
			"the authorized_keys overlay was not installed (broken key injection, "+
			"not an sshd bug).\n--- serial keys seen ---\n%s",
			fingerprint, grepSerial(vm, "slagworks: recovery: key"))
	}

	verifyRecoverySSH(ctx, t, vm, signer, network)
}

// TestRecoverySSHD_OnAgentFailure proves the killer-feature path: a
// real (deliberate) agent failure mid-deploy lands us in recovery_drop
// with a working sshd. The agent honors SLAGWORKS_FATAL_AT to fatal at
// installing_bootloader — late enough that partitioning + rsync have
// already mutated disk state, mirroring the kind of bug where the
// operator most needs to ssh in and inspect.
//
// Runtime: ~10-15 min (full deploy through rsync + partial install,
// then crash, then recovery).
func TestRecoverySSHD_OnAgentFailure(t *testing.T) {
	t.Parallel()

	env, network, signer, fingerprint, images := setupRecoveryTest(t, "noble-pxe")

	systemUUID := "12345678-1234-1234-1234-cccccccccc01"
	mac := "52:54:00:de:ad:c1"
	machineID := uuid.New()
	slug := "pop3-recovery-fail"

	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	deployOp.Params = json.RawMessage(`{"fatal_at":"installing_bootloader"}`)

	pushRecoverySnap(t, env, network, slug, mac, systemUUID, machineID, deployOp)

	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:   qemutest.BIOS,
		MAC:        mac,
		Network:    network,
		Memory:     "4096",
		SMBIOSUUID: systemUUID,
		DiskSize:   "10G",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	vm.Start(ctx)

	vm.WaitForSerial(ctx, "SLAGWORKS_FATAL_AT=installing_bootloader honored", qemutest.Budgets.RecoveryMarkerLong)
	vm.WaitForSerial(ctx, "RECOVERY MODE: agent exited with code", qemutest.Budgets.RecoveryMarkerShort)
	// Wait for the line that CONTAINS the IP, not the prior "ready"
	// banner — there's a tiny gap between the two prints in the
	// BOOT hook, and verifyRecoverySSH races the buffered serial
	// stream right after WaitFor returns. Pre-netns-isolation timing made
	// this race usually go the test's way; netns isolation's slightly
	// different timing surfaces it. Match the IP-bearing line
	// directly and the IP is guaranteed buffered when WaitFor wakes.
	vm.WaitForSerial(ctx, "slagworks:   ssh root@", qemutest.Budgets.RecoveryMarkerShort)

	// RIGGING SELF-CHECK before touching sshd: the recovery hook prints
	// each installed key's fingerprint; our ephemeral test key MUST be
	// among them. If the cpio-overlay injection broke (the
	// unaligned-append bug: kernel silently drops the overlay and the
	// canonical keys serve instead), this fails with an unambiguous
	// "test key never installed" instead of an sshd auth mystery.
	if !vm.Serial.Contains(fingerprint) {
		t.Fatalf("test key fingerprint %s not printed by recovery_drop — "+
			"the authorized_keys overlay was not installed (broken key injection, "+
			"not an sshd bug).\n--- serial keys seen ---\n%s",
			fingerprint, grepSerial(vm, "slagworks: recovery: key"))
	}

	verifyRecoverySSH(ctx, t, vm, signer, network)
}

// setupRecoveryTest does the keypair + cpio-overlay + Env/Network
// scaffolding shared by both recovery tests.
func setupRecoveryTest(t *testing.T, imageName string) (*qemutest.Env, *qemutest.Network, ssh.Signer, string, *qemutest.ImageDir) {
	t.Helper()
	env := qemutest.New(t)
	network := env.NewNetwork(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pubkey: %v", err)
	}
	authLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) +
		" slagworks-recovery-test\n"
	// The SHA256 fingerprint recovery_drop prints per installed key
	// (`ssh-keygen -lf`), asserted on serial before dialing.
	fingerprint := ssh.FingerprintSHA256(sshPub)

	images := overlayInitrdAuthKeys(t, env, imageName, []byte(authLine))
	return env, network, signer, fingerprint, images
}

// grepSerial returns the VM serial lines containing substr, for
// failure messages that should show what WAS seen.
func grepSerial(vm *qemutest.VM, substr string) string {
	var out []string
	for _, l := range vm.Serial.Lines() {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return "(none)"
	}
	return strings.Join(out, "\n")
}

func pushRecoverySnap(t *testing.T, env *qemutest.Env, network *qemutest.Network,
	slug, mac, systemUUID string, machineID uuid.UUID, op model.Operation) {
	t.Helper()
	snap := env.NewSnapshot(slug, network).
		AddMachine(qemutest.Machine{
			ID:          machineID,
			MAC:         mac,
			State:       model.MachineStateDeploying,
			PXEEligible: true,
			SystemUUID:  systemUUID,
			StorageScript: &model.SnapshotStorageScript{
				Name:    "default",
				Content: defaultStorageScript,
			},
		}).
		AddOperation(op).
		Build()
	env.Hub.Push(slug, snap)
}

// verifyRecoverySSH parses the VM's IP from the recovery banner, dials
// sshd over the test bridge, authenticates with signer, and runs a
// trivial command to prove a live session. The dial happens INSIDE
// the test's network namespace via qemutest.InNetns — the
// bridge lives in the netns, not the host, so a host-netns dial
// would EHOSTUNREACH.
func verifyRecoverySSH(ctx context.Context, t *testing.T, vm *qemutest.VM, signer ssh.Signer, network *qemutest.Network) {
	t.Helper()
	vmIP := extractIPFromBanner(t, vm)
	t.Logf("recovery sshd reachable at %s", vmIP)

	cli, err := dialRecoverySSH(ctx, vmIP, signer, network)
	if err != nil {
		t.Fatalf("ssh dial: %v\n--- serial dump ---\n%s",
			err, strings.Join(vm.Serial.Lines(), "\n"))
	}
	defer cli.Close()

	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("ssh session: %v", err)
	}
	defer sess.Close()
	out, err := sess.CombinedOutput("echo recovery-ok && uname -r")
	if err != nil {
		t.Fatalf("ssh exec: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "recovery-ok") {
		t.Fatalf("expected 'recovery-ok' in ssh output, got %q", out)
	}
	t.Logf("ssh exec: %s", strings.TrimSpace(string(out)))
}

func extractIPFromBanner(t *testing.T, vm *qemutest.VM) string {
	t.Helper()
	re := regexp.MustCompile(`ssh root@(\d+\.\d+\.\d+\.\d+)`)
	for _, line := range vm.Serial.Lines() {
		if m := re.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	t.Fatalf("could not find recovery banner IP in serial output")
	return ""
}

func dialRecoverySSH(ctx context.Context, ip string, signer ssh.Signer, network *qemutest.Network) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Open the TCP connection INSIDE the test netns. The fd
		// carries the netns; the goroutine that subsequently runs
		// the SSH handshake on it can be on any thread — same
		// pattern as nsListen in nsexec.go.
		var conn net.Conn
		dialErr := qemutest.InNetns(network.NSName, func() error {
			c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "22"), 3*time.Second)
			if err != nil {
				return err
			}
			conn = c
			return nil
		})
		if dialErr != nil {
			lastErr = dialErr
			time.Sleep(500 * time.Millisecond)
			continue
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, ip+":22", cfg)
		if err != nil {
			conn.Close()
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return ssh.NewClient(c, chans, reqs), nil
	}
	return nil, fmt.Errorf("ssh dial: %w", lastErr)
}

// overlayInitrdAuthKeys returns an ImageDir whose <image>/initrd is
// the canonical initrd with a small cpio segment appended that
// installs /etc/slagworks/recovery_authorized_keys=keys. Other files
// are symlinked from canonical to keep the per-test dir cheap.
func overlayInitrdAuthKeys(t *testing.T, env *qemutest.Env, imageName string, keys []byte) *qemutest.ImageDir {
	t.Helper()

	canon := env.UseCanonicalImages(t)
	canonImageDir := filepath.Join(canon.Path, imageName)
	if _, err := os.Stat(canonImageDir); err != nil {
		t.Skipf("canonical image %q not present: %v", imageName, err)
	}

	tmp := t.TempDir()
	imgDir := filepath.Join(tmp, imageName)
	if err := os.MkdirAll(imgDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries, err := os.ReadDir(canonImageDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", canonImageDir, err)
	}
	for _, e := range entries {
		src := filepath.Join(canonImageDir, e.Name())
		dst := filepath.Join(imgDir, e.Name())
		if e.Name() == "initrd" {
			canonBytes, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("read canonical initrd: %v", err)
			}
			// We append an UNCOMPRESSED newc cpio segment to a
			// zstd-compressed initrd. Linux supports concatenated
			// compressed/uncompressed cpio segments in any order
			// (Documentation/filesystems/ramfs-rootfs-initramfs.rst)
			// — but each segment must start 4-byte aligned, and the
			// kernel only skips NUL bytes between segments. Appending
			// directly after an unaligned zstd stream made the kernel
			// log "Initramfs unpacking failed: invalid magic at start
			// of compressed archive" and silently DROP the overlay —
			// the canonical authorized_keys was installed instead of
			// the test key and sshd (correctly) refused it. That was
			// the whole bug. Zero-pad to alignment.
			overlay := qemutest.WriteCpioNewc(
				"etc/slagworks/recovery_authorized_keys", 0o644, keys)
			pad := (4 - len(canonBytes)%4) % 4
			merged := make([]byte, 0, len(canonBytes)+pad+len(overlay))
			merged = append(merged, canonBytes...)
			merged = append(merged, make([]byte, pad)...)
			merged = append(merged, overlay...)
			if err := os.WriteFile(dst, merged, 0644); err != nil {
				t.Fatalf("write overlaid initrd: %v", err)
			}
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			t.Fatalf("symlink %s -> %s: %v", src, dst, err)
		}
	}

	// Also symlink the top-level binaries the harness needs
	// (qemutest-tftpd, slagworks-agent) and any other top-level
	// files (snponly.efi, undionly.kpxe, systemd-bootx64.efi).
	// These live at canon.Path, NOT canonImageDir/<imageName>/.
	// Without the qemutest-tftpd symlink the siteworker's
	// nsExec(sw.Images.Path/qemutest-tftpd, ...) would fail to
	// start the subprocess and EOF on the readiness handshake.
	topEntries, err := os.ReadDir(canon.Path)
	if err != nil {
		t.Fatalf("readdir %s: %v", canon.Path, err)
	}
	for _, e := range topEntries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(canon.Path, e.Name())
		dst := filepath.Join(tmp, e.Name())
		if err := os.Symlink(src, dst); err != nil {
			t.Fatalf("symlink top-level %s -> %s: %v", src, dst, err)
		}
	}

	return &qemutest.ImageDir{Path: tmp}
}
