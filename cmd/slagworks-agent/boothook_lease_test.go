//go:build !integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBootHook_KeepsTheLease runs the boot hook's own network bring-up (the
// lines of assets/initramfs/scripts/slagworks from its configure-networking
// step up to its mount-efivarfs step) under /bin/sh, with a fake `ip` and a
// fake dhcpcd on PATH.
//
// configure_networking is initramfs-tools': it runs `dhcpcd -1` (oneshot),
// which takes a lease and exits. The address it leaves in the kernel carries
// the lease's lifetime (`valid_lft 300sec` on a site with the default
// 5-minute lease) and the kernel deletes it when that runs out. The
// environment the hook goes on to start has to stay on the network longer
// than that: a deploy has the operation timeout (30 minutes) to finish, and a
// failed or aborted run holds the host on its recovery sshd until an operator
// or the next operation takes it (docs/dev/state-machine.md, "What an abort
// does on the host"). So once it has its address the hook must leave a DHCP
// client running on the interface that renews the lease. Making the address
// permanent is not a substitute: the site worker would still expire the
// lease and could hand the address to another host.
func TestBootHook_KeepsTheLease(t *testing.T) {
	t.Run("a renewing client is left running on the leased interface", func(t *testing.T) {
		out, started := runHookNetworkBringUp(t, "exit 0")
		if !strings.Contains(out, "IFACE=eth0 IP=192.0.2.10/24") {
			t.Fatalf("the hook did not pick up the leased address:\n%s", out)
		}
		for _, argv := range started {
			if strings.Contains(" "+argv+" ", " eth0 ") && !oneshotDHCPCD(argv) {
				return
			}
		}
		t.Fatalf("after network bring-up no renewing DHCP client runs on eth0 (dhcpcd started as: %q); "+
			"the kernel removes the address when the lease runs out, and the deploy environment and its "+
			"recovery sshd leave the network with it", started)
	})

	t.Run("a client that will not start warns and does not stop the boot", func(t *testing.T) {
		out, _ := runHookNetworkBringUp(t, "echo 'dhcpcd: cannot start' >&2; exit 1")
		if !strings.Contains(out, "IFACE=eth0 IP=192.0.2.10/24") {
			t.Fatalf("the hook did not carry on past a dhcpcd that would not start:\n%s", out)
		}
		if !strings.Contains(out, "WARNING") || !strings.Contains(out, "dhcp_lease_seconds") {
			t.Fatalf("the hook did not warn that the address leaves with the lease:\n%s", out)
		}
	})
}

// runHookNetworkBringUp runs the hook's network bring-up block with
// configure_networking stubbed to have leased 192.0.2.10/24 on eth0, and a
// fake dhcpcd whose body is dhcpcdBody. It returns the block's combined
// output and the argument list of every dhcpcd the block started.
func runHookNetworkBringUp(t *testing.T, dhcpcdBody string) (string, []string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "assets", "initramfs", "scripts", "slagworks"))
	if err != nil {
		t.Fatal(err)
	}
	const begin, end = `    _step "configure-networking"`, `    _step "mount-efivarfs"`
	hook := string(src)
	i, j := strings.Index(hook, begin), strings.Index(hook, end)
	if i < 0 || j < i {
		t.Fatalf("network bring-up block not found in the hook (looked for %q ... %q)", begin, end)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	clients := filepath.Join(dir, "dhcpcd-started")
	writeExecutable(t, filepath.Join(bin, "ip"), `#!/bin/sh
case " $* " in
  *" show "*)
    echo "2: eth0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 state UP"
    echo "    inet 192.0.2.10/24 brd 192.0.2.255 scope global dynamic eth0"
    echo "       valid_lft 300sec preferred_lft 300sec" ;;
esac
exit 0
`)
	writeExecutable(t, filepath.Join(bin, "dhcpcd"), "#!/bin/sh\necho \"$*\" >> \"$FAKE_CLIENTS\"\n"+dhcpcdBody+"\n")

	script := `
_step() { :; }
slagworks_recovery_drop() { echo "RECOVERY: $1"; exit 98; }
configure_networking() { DEVICE=eth0; }
` + hook[i:j] + `
echo "IFACE=$SLAG_IFACE IP=$SLAG_IP"
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_CLIENTS=" + clients}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook network bring-up failed: %v\n%s", err, out)
	}

	var started []string
	if data, err := os.ReadFile(clients); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if l != "" {
				started = append(started, l)
			}
		}
	}
	return string(out), started
}

// oneshotDHCPCD reports whether a dhcpcd argument list asks for oneshot mode
// (-1 alone or in a cluster such as -1KL, or --oneshot): a client that exits
// after one lease renews nothing.
func oneshotDHCPCD(argv string) bool {
	for _, a := range strings.Fields(argv) {
		if a == "--oneshot" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "1")) {
			return true
		}
	}
	return false
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil { //nolint:gosec // a test stub that must be executable
		t.Fatal(err)
	}
}
