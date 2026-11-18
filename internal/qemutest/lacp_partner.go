//go:build integration

package qemutest

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// LACPPartner is a host-side 802.3ad (LACP) bond that acts as the switch-side
// aggregation partner for a guest VM's bonded NICs — the closest analog in a
// single test host to the real dual-leaf MLAG the pop2 hot-spare hosts plug
// into. It is the piece a plain Linux bridge can't provide: a bridge forwards
// frames but never negotiates 802.3ad, so a guest bond in 802.3ad mode against
// a bridge sits forever with Partner Mac 00:00:00:00:00:00 (exactly the real-
// leaf symptom on bond34 before the host spoke LACP). Standing up a kernel
// bond in 802.3ad makes the host the LACP partner, so the negotiation actually
// happens and a negotiation bug would fail the test.
//
// Topology, all inside the per-test netns:
//
//	guest member NIC 0 ── tap Taps[0] ─┐
//	                                    ├─ host bond (802.3ad) ── enslaved to ──> Network.Bridge ── gateway IP (site worker)
//	guest member NIC 1 ── tap Taps[1] ─┘
//
// The host bond is enslaved into Network.Bridge so the guest, once its bond
// comes up with the deploy-time IP, is L2-adjacent to the gateway and can
// reach the site worker for phone-home over the bonded address (the off-box
// reachability proof). The guest's boot NIC (net0) attaches to the bridge
// directly as usual; only the two bond-member NICs ride the partner bond.
type LACPPartner struct {
	BondName string   // host-side bond interface name (the LACP partner)
	Taps     []string // tap device names; pass each to ExtraNIC.Tap
}

// NewLACPPartner creates the host-side 802.3ad bond and the requested number
// of taps (one per guest bond member), enslaves the taps into the bond, brings
// everything up, and enslaves the bond into network.Bridge. Torn down via
// t.Cleanup. Requires CAP_NET_ADMIN inside the netns (root on the kvm runner).
//
// The bond uses lacp_rate fast + xmit_hash_policy layer3+4 to mirror the
// netplan the L2 bond script renders (and the leaf bond34 policy), and
// miimon=100 like the script. ad_select=stable is the default; the guest and
// partner agree an aggregator over the two links.
func NewLACPPartner(t *testing.T, network *Network, members int) *LACPPartner {
	t.Helper()
	if members < 1 {
		t.Fatalf("qemutest: LACP partner needs >=1 member, got %d", members)
	}

	// Names derive from the bridge suffix (everything after the "sw"
	// prefix) so they're unique per test and collision-free with concurrent
	// runs, and fit IFNAMSIZ=15. Bridge is "sw"+10 chars; "bo"/"t<N>"+10
	// chars stays <=12.
	suffix := strings.TrimPrefix(network.Bridge, "sw")
	bondName := "bo" + suffix
	ns := network.NSName

	// Create the bond with its 802.3ad parameters in one shot. Setting mode
	// + lacp_rate + xmit_hash_policy at `link add` time is the portable form
	// (some iproute2 builds reject `lacp_rate`/`xmit_hash_policy` on a
	// `link set` of an already-created bond, and mode must be fixed before
	// any slave is enslaved). Mirrors the netplan the L2 script renders and
	// the leaf bond34 policy.
	runIPNS(t, ns, "link", "add", bondName, "type", "bond",
		"mode", "802.3ad",
		"miimon", "100",
		"lacp_rate", "fast",
		"xmit_hash_policy", "layer3+4")

	p := &LACPPartner{BondName: bondName}

	// Register teardown NOW, before the tap/enslave loop, so a t.Fatalf in
	// runIPNS mid-setup still unwinds whatever was created. The closure
	// captures p (a pointer), so it sees the evolving p.Taps slice. The
	// netns deletion in NewNetwork's cleanup would reclaim these anyway
	// (they live inside the netns), but unwinding explicitly is cheap
	// belt-and-suspenders and keeps a half-built partner from lingering.
	t.Cleanup(func() {
		_ = exec.Command("ip", "-n", ns, "link", "set", bondName, "nomaster").Run()
		for _, tap := range p.Taps {
			_ = exec.Command("ip", "-n", ns, "link", "del", tap).Run()
		}
		_ = exec.Command("ip", "-n", ns, "link", "del", bondName).Run()
	})

	for i := 0; i < members; i++ {
		tap := fmt.Sprintf("t%d%s", i, suffix)
		if len(tap) > 15 {
			t.Fatalf("qemutest: tap name %q exceeds IFNAMSIZ=15", tap)
		}
		// Create a tap owned by root (qemu runs as root in CI). `multi_queue`
		// off; a plain tap is fine for virtio-net-pci.
		runIPNS(t, ns, "tuntap", "add", "dev", tap, "mode", "tap")
		// A slave must be DOWN when enslaved to a bond.
		runIPNS(t, ns, "link", "set", tap, "down")
		runIPNS(t, ns, "link", "set", tap, "master", bondName)
		p.Taps = append(p.Taps, tap)
	}

	// Bring the bond + slaves up, then enslave the bond into the bridge so
	// the aggregated link reaches the gateway L2. The bridge already exists
	// and is up (NewNetwork). A bond CAN be a bridge port.
	for _, tap := range p.Taps {
		runIPNS(t, ns, "link", "set", tap, "up")
	}
	runIPNS(t, ns, "link", "set", bondName, "up")
	runIPNS(t, ns, "link", "set", bondName, "master", network.Bridge)

	return p
}

// PartnerState returns the host-side bond's /proc/net/bonding view, read from
// inside the netns. Used by the test to assert the partner aggregated both
// links (a sanity check on the harness side, complementing the guest-side
// /proc/net/bonding/<guest bond> assertion).
func (p *LACPPartner) PartnerState(t *testing.T, network *Network) string {
	t.Helper()
	out, err := NSExec(network.NSName, "cat", "/proc/net/bonding/"+p.BondName).CombinedOutput()
	if err != nil {
		t.Fatalf("qemutest: read partner bonding state: %v\n%s", err, out)
	}
	return string(out)
}
