package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// enrollTailscale joins the freshly-deployed host to the tailnet during deploy,
// before reboot, so tailnet identity is the host's SSH access substrate from
// first boot (and ansible can reach it at its inventory ts.net name without an
// OOB-IP override).
//
// The single-use auth key + desired hostname arrive on the kernel cmdline
// (slagworks.tailscale_authkey= / slagworks.tailscale_hostname=), minted per-op
// by global. Absent key => skip (no-op): deploys without enrollment (tests,
// pop3, any op global didn't mint for) behave exactly as before.
//
// Enrollment runs live in the target chroot (plan option b): tailscaled is
// started against the target's own state dir and `tailscale up` registers the
// node while the agent still controls the deploy, so the node is enrolled and
// verifiable before reboot. The auth key reaches `tailscale up` via a file
// (--auth-key=file:<path>) written to the chroot's /run and deleted right
// after — never argv, never an env var, never the installed disk, never logged
// (per Tailscale's "securely handle an auth key" guidance).
//
// SOFT failure: enrollment problems are logged and returned as a non-nil error
// the caller treats as a WARNING, not a deploy-fatal. A host that fails to
// enroll still has a working OS on disk; ansible will enroll it on converge as
// it did before this feature. Enrollment is an enhancement, not a precondition.
func enrollTailscale(ctx *agent.Context, target string) error {
	authKey := kernelArgValue("slagworks.tailscale_authkey")
	if authKey == "" {
		ctx.Log.Info("tailscale: no auth key on cmdline; skipping tailnet enrollment")
		return nil
	}
	hostname := kernelArgValue("slagworks.tailscale_hostname")

	// The binary is baked into the deploy image (packages-deploy.txt). If it's
	// somehow absent (older image rolled before the bake landed), skip rather
	// than fail the deploy — the key goes unused and ansible enrolls later.
	// Check the EXACT binaries enrollment execs: /usr/bin/tailscale (the CLI,
	// run for `up`) and /usr/sbin/tailscaled (the daemon). Either missing =>
	// older image rolled before the bake landed; skip rather than fail (the key
	// goes unused, ansible enrolls on converge).
	if _, err := os.Stat(target + "/usr/bin/tailscale"); err != nil {
		ctx.Log.Warn("tailscale: /usr/bin/tailscale not in target image; skipping enrollment")
		return nil
	}
	if _, err := os.Stat(target + "/usr/sbin/tailscaled"); err != nil {
		ctx.Log.Warn("tailscale: /usr/sbin/tailscaled not in target image; skipping enrollment")
		return nil
	}

	ctx.Log.Info("tailscale: enrolling host onto tailnet", "hostname", hostname)

	// Diagnostics: the enrollment runs in the INITRD's network namespace (the
	// chroot shares it — we bind /proc /sys /dev but never unshare netns), so it
	// reaches the control plane via whatever route the initrd's DHCP bring-up
	// installed. Log the live route table + a control-plane reachability probe
	// BEFORE `tailscale up`, on the captured envelope stream (slog), so a failed
	// enroll is diagnosable from `slagctl machine logs` without an SOL capture.
	// These intentionally log freely: talking to slagworks at all is already a
	// privileged operation and the per-op key is single-use + ephemeral, so
	// there is no meaningful secret-in-logs risk to hedge against here.
	logEnrollNetState(ctx)

	// Ensure a default route exists in the initrd before `tailscale up` — the
	// control plane is off-segment, so without one tailscaled gets an immediate
	// "network unreachable". The DHCP bring-up SHOULD install option-3 as the
	// default, but klibc ipconfig's behavior varies; if no default route is
	// present, install one via the DHCP-advertised gateway (read from the
	// lease) as a belt-and-suspenders. Best-effort: failure here just means
	// enrollment may fail to reach control, which is the soft-fail path.
	ensureDefaultRoute(ctx)

	// Bind the pseudo-filesystems tailscaled needs in the chroot: /dev (for
	// /dev/net/tun), /proc, /sys (netlink/route). applyNetBundle does the same
	// dance for systemctl.
	for _, m := range []struct{ src, dst string }{
		{"/dev", target + "/dev"},
		{"/proc", target + "/proc"},
		{"/sys", target + "/sys"},
	} {
		_ = os.MkdirAll(m.dst, 0755)
		_ = ctx.Exec.Run("mount", "--bind", m.src, m.dst)
	}
	defer func() {
		for _, dst := range []string{target + "/sys", target + "/proc", target + "/dev"} {
			_ = ctx.Exec.Run("umount", dst)
		}
	}()

	// Mount a real tmpfs over the target's /run for the enrollment window. This
	// is what makes the daemon socket AND the auth-key file genuinely transient
	// — RAM-backed, gone on umount, NEVER touching the installed disk even if
	// the agent is SIGKILLed mid-enroll (the bytes were only ever in tmpfs).
	// Unmounted before reboot so the deployed OS boots with its own fresh /run.
	// A plain mkdir here would put the secret on the persistent target rootfs.
	if err := os.MkdirAll(target+"/run", 0755); err != nil {
		return fmt.Errorf("tailscale: mkdir /run: %w", err)
	}
	if err := ctx.Exec.Run("mount", "-t", "tmpfs", "tmpfs", target+"/run"); err != nil {
		return fmt.Errorf("tailscale: mount tmpfs on /run: %w", err)
	}
	defer func() { _ = ctx.Exec.Run("umount", target+"/run") }()

	// DNS for the chroot. tailscaled must resolve the control plane
	// (controlplane.tailscale.com), but the target's /etc/resolv.conf is
	// typically a dangling symlink to /run/systemd/resolve/stub-resolv.conf
	// (systemd-resolved isn't running in the deploy initrd, and /run isn't the
	// host's). Drop a concrete public resolver into the chroot for the
	// enrollment window, restoring the image's original afterward so the
	// deployed OS's resolv.conf (managed by netplan/systemd-resolved) is
	// untouched. 1.1.1.1/1.0.0.1 matches the netplan fallback the netscripts
	// already use.
	restoreResolv := overrideChrootResolvConf(target)
	defer restoreResolv()

	// Start tailscaled in the chroot with its state dir at the target's
	// canonical /var/lib/tailscale, so the state persists into the booted OS
	// and the node comes back as the SAME node after reboot. Background it;
	// `tailscale up` below blocks until it's reachable.
	if err := os.MkdirAll(target+"/var/lib/tailscale", 0700); err != nil {
		return fmt.Errorf("tailscale: mkdir state dir: %w", err)
	}
	if err := os.MkdirAll(target+"/run/tailscale", 0755); err != nil {
		return fmt.Errorf("tailscale: mkdir socket dir: %w", err)
	}
	// --tun=userspace-networking: the deploy initrd has no working iptables /
	// netfilter (the chroot can't load the modules; tailscaled's kernel-TUN
	// engine init fails with "could not get iptables version" and the daemon
	// exits before creating its socket, so `tailscale up` then can't connect).
	// Enrollment only needs to REGISTER the node and persist tailscaled.state —
	// not move data-plane traffic — so userspace mode (no TUN, no netfilter) is
	// exactly right: tailscaled starts, reaches the control plane, registers,
	// and writes state. The state persists to the target's /var/lib/tailscale;
	// the BOOTED OS starts its own tailscaled.service in normal kernel-TUN mode
	// (it has full iptables) and comes back as the same registered node.
	tailscaled := exec.Command("chroot", target, "/usr/sbin/tailscaled",
		"--state=/var/lib/tailscale/tailscaled.state",
		"--socket=/run/tailscale/tailscaled.sock",
		"--tun=userspace-networking")
	// tailscaled logs to stderr; tee it to BOTH the SOL console and a buffer we
	// drain to the captured envelope stream, so a failed enroll is diagnosable
	// from `slagctl machine logs` (the SOL feed rotates and is flaky on these
	// AMI BMCs). No secret risk: the auth key reaches `tailscale up` via a file,
	// never tailscaled's argv/output. One shared tee for both streams so
	// exec.Cmd serializes writes to the unsynchronized buffer (see runTailscaleUp).
	var tsdBuf logBuffer
	tsdTee := io.MultiWriter(os.Stderr, &tsdBuf)
	tailscaled.Stdout = tsdTee
	tailscaled.Stderr = tsdTee
	if err := tailscaled.Start(); err != nil {
		return fmt.Errorf("tailscale: start tailscaled in chroot: %w", err)
	}
	defer tsdBuf.flushTo(ctx.Log, "tailscaled")
	// Always stop the backgrounded daemon before returning: the booted OS
	// starts its own tailscaled.service from the persisted state; a lingering
	// chroot daemon holding the socket/state is just a deploy-env artifact.
	//
	// Use Cmd.Wait (not Process.Wait): tailscaled's Stdout/Stderr is an
	// io.MultiWriter (not an *os.File), so exec.Cmd copies its output through an
	// internal pipe + goroutine that only Cmd.Wait joins. Process.Wait reaps the
	// process but leaves that copier goroutine running — and since this defer
	// runs BEFORE the LIFO-earlier tsdBuf.flushTo, flushTo would read the
	// strings.Builder while the copier is still writing it (race + lost output).
	// Cmd.Wait blocks until the copier is done, so the subsequent flush is safe.
	defer func() {
		_ = tailscaled.Process.Kill()
		_ = tailscaled.Wait()
	}()

	// Give tailscaled a moment to create its socket before `up` connects.
	// `tailscale up` retries the socket itself, but a tiny settle avoids a
	// noisy first-attempt failure in the logs.
	time.Sleep(500 * time.Millisecond)

	if err := runTailscaleUp(ctx, target, authKey, hostname); err != nil {
		ctx.Log.Warn("tailscale: `up` failed", "err", err)
		return fmt.Errorf("tailscale up: %w", err)
	}

	// Enable tailscaled.service in the target so the booted OS starts the
	// daemon from the persisted state and the node comes back online after
	// reboot. The package ships the unit disabled; enrollment owns enabling it
	// (plan §4). Needs the pseudo-fs bind mounts above (still live here).
	if err := ctx.Exec.Run("chroot", target, "systemctl", "enable", "tailscaled"); err != nil {
		return fmt.Errorf("tailscale: enable tailscaled.service: %w", err)
	}

	ctx.Log.Info("tailscale: tailnet enrollment complete", "hostname", hostname)
	return nil
}

// overrideChrootResolvConf writes a concrete public resolver to
// <target>/etc/resolv.conf for the enrollment window and returns a restore
// func that puts the original back. The image's resolv.conf is usually a
// symlink into /run/systemd/resolve (dead in the deploy initrd), so tailscaled
// can't resolve the control plane without this. We preserve+restore so the
// deployed OS boots with exactly the resolv.conf the image shipped (netplan /
// systemd-resolved own it at runtime). Best-effort: failures here just mean
// enrollment may fail to resolve, which is the soft-fail path anyway.
func overrideChrootResolvConf(target string) func() {
	path := target + "/etc/resolv.conf"
	// Stash the original (symlink or file) by renaming it aside, then write a
	// plain file. rename preserves a dangling symlink intact for restore.
	backup := path + ".slagworks-enroll-bak"
	_ = os.Remove(backup)
	hadOriginal := false
	if err := os.Rename(path, backup); err == nil {
		hadOriginal = true
	}
	_ = os.WriteFile(path, []byte("nameserver 1.1.1.1\nnameserver 1.0.0.1\n"), 0644)
	return func() {
		_ = os.Remove(path)
		if hadOriginal {
			_ = os.Rename(backup, path)
		}
	}
}

// runTailscaleUp execs `chroot target tailscale up ...`. The auth key reaches
// tailscale via a file (--auth-key=file:<path>), NOT argv: the key is written
// to a file under the chroot's /run — a tmpfs mounted by the caller, so the
// bytes are RAM-backed and never hit the installed disk — passed by PATH, and
// deleted immediately after. So the secret never appears in argv
// (/proc/<pid>/cmdline) nor in any log envelope. Output goes straight to the
// console; tailscale's own output doesn't echo the key.
func runTailscaleUp(ctx *agent.Context, target, authKey, hostname string) error {
	// /run is the caller's tmpfs (RAM-backed); the socket dir was made there
	// already. Write the key 0600, delete after `up` consumes it.
	keyFileChroot := "/run/tailscale/authkey"
	keyFileHost := target + keyFileChroot
	if err := os.WriteFile(keyFileHost, []byte(authKey), 0600); err != nil {
		return fmt.Errorf("write auth-key file: %w", err)
	}
	defer func() { _ = os.Remove(keyFileHost) }()

	// --socket is a GLOBAL `tailscale` flag and must precede the `up`
	// subcommand: `tailscale --socket=... up ...`. Passing it AFTER `up` makes
	// the up subcommand reject it ("flag provided but not defined: -socket")
	// and print usage, failing enrollment instantly. (This was the actual cause
	// of every early enroll failure — not egress.) The socket must match the
	// non-default --socket tailscaled was started with above.
	args := tailscaleUpArgs(target, keyFileChroot, hostname)
	// Tee `up`'s output to the SOL console AND a buffer drained to the captured
	// envelope stream — `up`'s error message (e.g. the control-plane dial error,
	// a bad/expired key, a tag-not-permitted rejection) is the single most
	// useful diagnostic when enrollment fails, and the SOL feed is flaky/rotates.
	//
	// One shared tee for BOTH stdout and stderr: exec.Cmd only serializes the two
	// streams' writes when Stdout == Stderr (same comparable writer). Two distinct
	// io.MultiWriter values sharing &upBuf would race on the unsynchronized
	// strings.Builder (two copier goroutines). Sharing one writer makes exec.Cmd
	// use a single pipe + serialized writes.
	var upBuf logBuffer
	tee := io.MultiWriter(os.Stdout, &upBuf)
	cmd := exec.Command("chroot", args...)
	cmd.Stdout = tee
	cmd.Stderr = tee
	err := cmd.Run()
	upBuf.flushTo(ctx.Log, "tailscale up")
	return err
}

// tailscaleUpTimeout bounds `tailscale up`. Without --timeout, `up` blocks
// until tailscaled reaches Running, so any enrollment problem (no control-plane
// egress, a rejected key) hung finalize forever: phone-home was never written,
// the op died at global's 25m timeout with the host stuck in the deploy initrd,
// and `up`'s buffered error output was never flushed to the envelope stream
// (pop1 leaf-3, 2026-10-23). With the bound, enrollment soft-fails as designed:
// the error is logged, the deploy finishes, and ansible enrolls on converge.
const tailscaleUpTimeout = "120s"

// tailscaleUpArgs builds the `chroot <target> tailscale ... up ...` argv.
// --socket is a GLOBAL `tailscale` flag and must precede the `up` subcommand:
// passing it after `up` makes the subcommand reject it ("flag provided but not
// defined: -socket"), which was the cause of every early enroll failure.
func tailscaleUpArgs(target, keyFileChroot, hostname string) []string {
	args := []string{target, "/usr/bin/tailscale",
		"--socket=/run/tailscale/tailscaled.sock",
		"up",
		"--auth-key=file:" + keyFileChroot,
		"--accept-dns=false",
		"--ssh=true",
		"--timeout=" + tailscaleUpTimeout,
	}
	if hostname != "" {
		args = append(args, "--hostname="+hostname)
	}
	return args
}

// kernelArgValue extracts the value of a key=value token from /proc/cmdline,
// e.g. kernelArgValue("slagworks.tailscale_authkey") -> "tskey-...". Returns ""
// if the key is absent or has no value. Complements hasKernelArg/cmdlineHas
// (which only test presence). Reads /proc/cmdline fresh each call (cheap; only
// called once per deploy).
func kernelArgValue(key string) string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return cmdlineValue(string(data), key)
}

// cmdlineValue is the testable string-matcher behind kernelArgValue. Returns
// the value of the first `key=value` token whose key matches exactly, or "".
func cmdlineValue(cmdline, key string) string {
	for tok := range strings.FieldsSeq(cmdline) {
		if idx := strings.IndexByte(tok, '='); idx > 0 && tok[:idx] == key {
			return tok[idx+1:]
		}
	}
	return ""
}

// logBuffer is an io.Writer that accumulates subprocess output so it can be
// drained to the captured envelope stream (slog) after the process exits. It
// tees alongside the SOL console; flushTo emits the collected text line-by-line
// at a single log call so a failed enroll is diagnosable from
// `slagctl machine logs` without an SOL capture.
type logBuffer struct{ b strings.Builder }

func (w *logBuffer) Write(p []byte) (int, error) { return w.b.Write(p) }

func (w *logBuffer) flushTo(log *slog.Logger, what string) {
	out := strings.TrimSpace(w.b.String())
	if out == "" {
		return
	}
	log.Info("tailscale: "+what+" output", "output", out)
}

// logEnrollNetState dumps the initrd's live network state (addresses, routes,
// default route presence) to the captured envelope stream before `tailscale
// up`. Enrollment runs in the initrd's netns, so this is exactly the network
// `tailscale up` will use to reach the control plane — the decisive datum when
// an enroll fails ("network is unreachable" => no default route; a DNS error =>
// resolv.conf; a dial timeout => egress/NAT not up). Best-effort; never fatal.
func logEnrollNetState(ctx *agent.Context) {
	for _, c := range [][]string{
		{"ip", "-brief", "addr"},
		{"ip", "route"},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		if err != nil {
			ctx.Log.Warn("tailscale: enroll net-state probe failed", "cmd", strings.Join(c, " "), "err", err)
			continue
		}
		ctx.Log.Info("tailscale: enroll net-state", "cmd", strings.Join(c, " "), "output", strings.TrimSpace(string(out)))
	}
}

// ensureDefaultRoute installs a default route in the initrd if none exists, so
// `tailscale up` can reach the off-segment control plane. The initrd's DHCP
// bring-up (klibc ipconfig) SHOULD install the DHCP option-3 gateway as the
// default, but its behavior varies; this is a belt-and-suspenders. The gateway
// is read from the klibc lease file (/run/net-*.conf, key GATEWAY=...) that the
// initrd writes — the same option-3 the slagworks DHCP server advertised.
// Best-effort: a present default route, a missing lease file, or an add failure
// all just leave the route table as-is (enrollment then soft-fails if it can't
// egress, which is the existing contract).
func ensureDefaultRoute(ctx *agent.Context) {
	if out, _ := exec.Command("ip", "route", "show", "default").Output(); len(strings.TrimSpace(string(out))) > 0 {
		return // already have a default route
	}
	gw := dhcpLeaseGateway()
	if gw == "" {
		ctx.Log.Warn("tailscale: no default route and no DHCP gateway in lease; enrollment may fail to reach control plane")
		return
	}
	if err := ctx.Exec.Run("ip", "route", "add", "default", "via", gw); err != nil {
		ctx.Log.Warn("tailscale: failed to install fallback default route", "via", gw, "err", err)
		return
	}
	ctx.Log.Info("tailscale: installed fallback default route (DHCP gateway)", "via", gw)
}

// dhcpLeaseGateway reads the DHCP-advertised gateway from the klibc initrd
// lease files (/run/net-*.conf), which carry shell-style KEY='value' lines
// including GATEWAY='<router>' (DHCP option 3). Returns "" if no lease file or
// no gateway is found.
func dhcpLeaseGateway() string {
	matches, _ := filepath.Glob("/run/net-*.conf")
	for _, f := range matches {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "GATEWAY=") {
				continue
			}
			gw := strings.Trim(strings.TrimPrefix(line, "GATEWAY="), "'\"")
			if gw != "" && gw != "0.0.0.0" {
				return gw
			}
		}
	}
	return ""
}
