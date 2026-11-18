//go:build integration

package qemutest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Network is a per-test Linux bridge inside a dedicated network
// namespace, with an allocated /24. DHCP/TFTP/HTTP listeners are
// created inside NSName via the helpers in nsexec.go; tap devices
// for each VM attach to Bridge inside the same namespace. Torn
// down via t.Cleanup.
type Network struct {
	NSName    string // "swns<6hex><4digit>" — matches Bridge suffix
	Bridge    string // "sw<6hex><4digit>" per spec, fits IFNAMSIZ=15
	Subnet    *net.IPNet
	SubnetStr string // original CIDR string
	GatewayIP net.IP
	Gateway   string // GatewayIP as string (convenience)
	t         *testing.T
}

// NewNetwork allocates a /24, creates a fresh network namespace,
// and creates the bridge interface inside it with the gateway IP
// assigned and brought up. Requires CAP_NET_ADMIN (run as root
// inside the runner).
//
// Cleanup order (kill-then-del — see plan doc, Cleanup section):
// kill any subprocess still inside the netns first, THEN ip netns
// del. Skipping the kill leaves zombie qemus holding their own
// /proc/self/ns/net references in an anonymous, invisible netns.
func (e *Env) NewNetwork(t *testing.T) *Network {
	t.Helper()

	gateway, subnetStr := e.SubnetPool.Allocate(t)

	_, subnet, err := net.ParseCIDR(subnetStr)
	if err != nil {
		t.Fatalf("qemutest: parse subnet %q: %v", subnetStr, err)
	}

	// Names: "sw" + 6 chars of sha256(runID+t.Name()) + 4-digit seq
	// for the bridge, "swns" + same suffix for the netns. Both fit
	// in IFNAMSIZ=15 (bridge: 12 chars; netns name has no IFNAMSIZ
	// cap but the kernel limits NAME_MAX=255). Including the test
	// name in the hash avoids collisions when two parallel tests
	// each start at seq=1 within their own Env.
	seq := e.bridgeSeq.Add(1)
	h := sha256.Sum256([]byte(e.SubnetPool.RunID() + t.Name()))
	suffix := fmt.Sprintf("%s%04d", hex.EncodeToString(h[:])[:6], seq)
	bridge := "sw" + suffix
	nsName := "swns" + suffix

	n := &Network{
		NSName:    nsName,
		Bridge:    bridge,
		Subnet:    subnet,
		SubnetStr: subnetStr,
		GatewayIP: net.ParseIP(gateway),
		Gateway:   gateway,
		t:         t,
	}

	prefixLen, _ := subnet.Mask.Size()

	// Create the netns first; everything else lives inside it. If
	// any step below fails we still register cleanup so a partial
	// netns doesn't accumulate.
	runIP(t, "ip", "netns", "add", nsName)
	t.Cleanup(func() {
		// Kill anything still running inside the netns BEFORE
		// removing it. `ip netns del` only unmounts the bind-mount
		// under /var/run/netns/; live processes keep the kernel
		// netns alive in an anonymous form (invisible to
		// `ip netns list`) along with all their RAM/fds. The pkill
		// equivalent here uses `ip netns pids` to find them.
		if pidOut, err := exec.Command("ip", "netns", "pids", nsName).Output(); err == nil {
			for _, pidStr := range strings.Fields(string(pidOut)) {
				if pid, err := strconv.Atoi(pidStr); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
		}
		_ = exec.Command("ip", "netns", "del", nsName).Run()
	})

	// Bridge ops via `ip -n <ns>`. Iproute2's -n flag is just a
	// setns wrapper for the ip command itself; cheap and avoids
	// any thread-pinning in our test process.
	runIPNS(t, nsName, "link", "add", bridge, "type", "bridge")
	runIPNS(t, nsName, "addr", "add", fmt.Sprintf("%s/%d", gateway, prefixLen), "dev", bridge)
	runIPNS(t, nsName, "link", "set", bridge, "up")

	// `lo` exists in a fresh netns but starts down. Some test code
	// (and qemu's monitor socket if we ever add one) expects
	// 127.0.0.1 to be reachable; bringing lo up is cheap insurance.
	runIPNS(t, nsName, "link", "set", "lo", "up")

	// Default route via the bridge. Required for DHCP OFFER replies
	// to reach 255.255.255.255 — without a default route, even with
	// SO_BROADCAST set, the kernel returns ENETUNREACH on
	// `WriteTo(255.255.255.255:68)` because the broadcast doesn't
	// match the bridge's /24. Production avoids this by using
	// SO_BINDTODEVICE on the listening socket; we drop SO_BINDTODEVICE
	// in the netns'd path (only one bridge can deliver to us anyway)
	// and rely on a default route here instead. The route's gateway
	// is the bridge IP itself, but the route action is `dev <bridge>`
	// so the kernel just hands the packet to the bridge layer-2 which
	// then broadcasts. Equivalent shape to what SO_BINDTODEVICE
	// would have given us.
	//
	// Tripwire: this works because the kernel's `ip_route_output_flow`
	// path falls back to the main routing table for limited broadcasts
	// (255.255.255.255) when no specific 255.255.255.255 broadcast
	// route exists. If a future kernel/distro tightens that fallback,
	// switch to an explicit `ip route add 255.255.255.255 dev <bridge>`.
	runIPNS(t, nsName, "route", "add", "default", "dev", bridge)

	return n
}

func runIP(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("qemutest: %s %v: %v\n%s", name, args, err, out)
	}
}

// runIPNS runs `ip -n <ns> <args...>`. Use for any ip subcommand
// that targets a link/address/route inside the namespace; iproute2
// handles the setns transparently.
func runIPNS(t *testing.T, ns string, args ...string) {
	t.Helper()
	full := append([]string{"-n", ns}, args...)
	out, err := exec.Command("ip", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("qemutest: ip -n %s %v: %v\n%s", ns, args, err, out)
	}
}
