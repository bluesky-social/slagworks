//go:build !integration

package main

import (
	"testing"
)

func TestCmdlineValue(t *testing.T) {
	cases := []struct {
		cmdline string
		key     string
		want    string
	}{
		// The auth key + hostname tokens the boot handler emits.
		{"ro slagworks.tailscale_authkey=tskey-auth-abc123 quiet", "slagworks.tailscale_authkey", "tskey-auth-abc123"},
		{"slagworks.tailscale_hostname=oob-gw-3-pop2 ro", "slagworks.tailscale_hostname", "oob-gw-3-pop2"},
		// Both present.
		{"slagworks.tailscale_authkey=K slagworks.tailscale_hostname=H", "slagworks.tailscale_hostname", "H"},
		// Absent -> empty (enrollment is skipped).
		{"ro quiet slagworks.machine=abc", "slagworks.tailscale_authkey", ""},
		// Exact key match, not prefix.
		{"slagworks.tailscale_authkey_x=Y", "slagworks.tailscale_authkey", ""},
		// Bare token (no =) is not a value.
		{"slagworks.tailscale_authkey ro", "slagworks.tailscale_authkey", ""},
		// Empty cmdline.
		{"", "slagworks.tailscale_authkey", ""},
		// First match wins.
		{"k=first k=second", "k", "first"},
	}
	for _, c := range cases {
		if got := cmdlineValue(c.cmdline, c.key); got != c.want {
			t.Errorf("cmdlineValue(%q, %q) = %q, want %q", c.cmdline, c.key, got, c.want)
		}
	}
}

// TestTailscaleUpArgs_BoundedAndOrdered pins the two properties that broke
// deploys: `up` must carry --timeout (an unbounded `up` hung finalize forever
// on pop1 leaf-3, 2026-10-23), and the global --socket flag must precede the
// `up` subcommand.
func TestTailscaleUpArgs_BoundedAndOrdered(t *testing.T) {
	args := tailscaleUpArgs("/target", "/run/tailscale/authkey", "leaf-3-pop1")
	idx := func(want string) int {
		for i, a := range args {
			if a == want {
				return i
			}
		}
		return -1
	}
	if idx("--timeout="+tailscaleUpTimeout) < 0 {
		t.Errorf("tailscale up args lack --timeout=%s: %v", tailscaleUpTimeout, args)
	}
	sock, up := idx("--socket=/run/tailscale/tailscaled.sock"), idx("up")
	if sock < 0 || up < 0 || sock > up {
		t.Errorf("--socket must precede `up` (socket=%d up=%d): %v", sock, up, args)
	}
	if idx("--hostname=leaf-3-pop1") < 0 {
		t.Errorf("hostname not passed: %v", args)
	}
	for _, a := range args {
		if a == "tskey" || len(a) > 6 && a[:6] == "tskey-" {
			t.Errorf("auth key must never appear in argv: %v", args)
		}
	}
}
