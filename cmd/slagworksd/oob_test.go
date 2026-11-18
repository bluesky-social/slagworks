package main

import (
	"net"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestDeriveOOBBind(t *testing.T) {
	ifaces := []localIface{
		{name: "lo", addrs: []net.IP{net.ParseIP("127.0.0.1")}},
		{name: "leaf4", addrs: []net.IP{net.ParseIP("10.254.25.1")}},
		{name: "eno3", addrs: []net.IP{net.ParseIP("10.254.240.11"), net.ParseIP("10.1.1.97")}},
	}

	t.Run("matches the OOB-subnet interface", func(t *testing.T) {
		bind, ok, err := deriveOOBBind("10.254.240.0/22", ifaces)
		if err != nil || !ok {
			t.Fatalf("expected a bind, got ok=%v err=%v", ok, err)
		}
		if bind.iface != "eno3" {
			t.Errorf("iface = %q, want eno3", bind.iface)
		}
		if !bind.serverIP.Equal(net.ParseIP("10.254.240.11")) {
			t.Errorf("serverIP = %v, want 10.254.240.11", bind.serverIP)
		}
	})

	t.Run("no local interface on the OOB subnet", func(t *testing.T) {
		_, ok, err := deriveOOBBind("10.99.99.0/24", ifaces)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if ok {
			t.Error("expected ok=false when no interface is on the OOB subnet")
		}
	})

	t.Run("bad subnet errors", func(t *testing.T) {
		if _, _, err := deriveOOBBind("not-a-cidr", ifaces); err == nil {
			t.Error("expected error on bad subnet")
		}
	})
}

func TestOOBIfaceParams(t *testing.T) {
	// Two-gateway split pool; this worker is .11, so it should own the
	// .100-.149 slice and carry both slices for peer-NAK classification.
	oob := model.OOBConfig{
		Subnet:       "10.254.240.0/22",
		Gateway:      "10.254.243.253",
		DNS:          []string{"10.254.240.11", "10.254.240.12"},
		NTP:          []string{"10.254.240.1"},
		LeaseSeconds: 300,
		DynamicSlices: []model.OOBDynamicSlice{
			{Start: "10.254.241.100", End: "10.254.241.149", Owner: "10.254.240.11"},
			{Start: "10.254.241.150", End: "10.254.241.199", Owner: "10.254.240.12"},
		},
	}
	serverIP := net.ParseIP("10.254.240.11")

	p, ok, err := oobIfaceParams(oob, serverIP, "definitely-not-a-real-iface", time.Second, nil)
	if err != nil || !ok {
		t.Fatalf("expected params, got ok=%v err=%v", ok, err)
	}
	if !p.ServerIP.Equal(serverIP) {
		t.Errorf("ServerIP = %v, want %v", p.ServerIP, serverIP)
	}
	if !p.Promiscuous {
		t.Error("OOB params must be promiscuous")
	}
	if !p.Gateway.Equal(net.ParseIP("10.254.243.253")) {
		t.Errorf("Gateway = %v", p.Gateway)
	}
	if len(p.DNS) != 2 || len(p.NTP) != 1 {
		t.Errorf("DNS/NTP = %v / %v", p.DNS, p.NTP)
	}
	if p.LeaseDuration != 300*time.Second {
		t.Errorf("LeaseDuration = %v, want 5m", p.LeaseDuration)
	}
	// This worker (.11) owns the .100-.149 slice -> RangeStart/End.
	if !p.RangeStart.Equal(net.ParseIP("10.254.241.100")) || !p.RangeEnd.Equal(net.ParseIP("10.254.241.149")) {
		t.Errorf("my slice = %v-%v, want .100-.149", p.RangeStart, p.RangeEnd)
	}
	// Both slices carried for peer-NAK classification.
	if len(p.DynamicSlices) != 2 {
		t.Errorf("expected 2 dynamic slices carried, got %d", len(p.DynamicSlices))
	}
	if p.Subnet.String() != "10.254.240.0/22" {
		t.Errorf("subnet = %v", p.Subnet.String())
	}
	// ARPProbe is nil here because the fake interface name doesn't resolve —
	// that's the documented safe degradation.
	if p.ARPProbe != nil {
		t.Error("expected nil ARPProbe for an unresolvable interface")
	}
}

// TestOOBIfaceParams_NoOwnedSlice: a worker whose serverIP matches no slice
// owner serves reservations only — no RangeStart/End — but still carries the
// slices for peer-NAK classification.
func TestOOBIfaceParams_NoOwnedSlice(t *testing.T) {
	oob := model.OOBConfig{
		Subnet:  "10.254.240.0/22",
		Gateway: "10.254.243.253",
		DynamicSlices: []model.OOBDynamicSlice{
			{Start: "10.254.241.100", End: "10.254.241.149", Owner: "10.254.240.11"},
		},
	}
	// This worker is .99 — owns no slice.
	p, ok, err := oobIfaceParams(oob, net.ParseIP("10.254.240.99"), "x", time.Second, nil)
	if err != nil || !ok {
		t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
	}
	if p.RangeStart != nil || p.RangeEnd != nil {
		t.Errorf("unowned worker must have no own slice, got %v-%v", p.RangeStart, p.RangeEnd)
	}
	if len(p.DynamicSlices) != 1 {
		t.Errorf("must still carry peer slices, got %d", len(p.DynamicSlices))
	}
}

func TestOOBIfaceParams_IncompleteConfig(t *testing.T) {
	// No gateway -> not ok (can't serve without a gateway option).
	_, ok, err := oobIfaceParams(model.OOBConfig{Subnet: "10.254.240.0/22"}, net.ParseIP("10.254.240.11"), "x", time.Second, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("expected ok=false when gateway is missing")
	}
}

func TestOOBIfaceParams_NoDynamicPoolIsValid(t *testing.T) {
	// Reservations-only OOB segment: no dynamic pool, still valid.
	oob := model.OOBConfig{Subnet: "10.254.240.0/22", Gateway: "10.254.243.253"}
	p, ok, err := oobIfaceParams(oob, net.ParseIP("10.254.240.11"), "x", time.Second, nil)
	if err != nil || !ok {
		t.Fatalf("expected ok, got ok=%v err=%v", ok, err)
	}
	if p.RangeStart != nil || p.RangeEnd != nil {
		t.Errorf("expected no dynamic pool, got %v-%v", p.RangeStart, p.RangeEnd)
	}
}

func TestOOBCollidesWithFabric(t *testing.T) {
	fabricIP := "10.254.25.1"
	cases := []struct {
		name   string
		bind   oobBind
		expect bool
	}{
		{"distinct iface and ip (real topology)", oobBind{iface: "eno3", serverIP: net.ParseIP("10.254.240.11")}, false},
		{"same iface", oobBind{iface: "leaf4", serverIP: net.ParseIP("10.254.240.11")}, true},
		{"same serverIP", oobBind{iface: "eno3", serverIP: net.ParseIP("10.254.25.1")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oobCollidesWithFabric(tc.bind, "leaf4", fabricIP); got != tc.expect {
				t.Errorf("collide = %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestParseIPs(t *testing.T) {
	got := parseIPs([]string{"10.0.0.1", "garbage", "10.0.0.2"})
	if len(got) != 2 {
		t.Fatalf("expected 2 valid IPs (garbage dropped), got %d: %v", len(got), got)
	}
}
