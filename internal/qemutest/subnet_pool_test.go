//go:build integration

package qemutest

import (
	"fmt"
	"net"
	"testing"
)

func TestSubnetPoolAllocatesDistinct(t *testing.T) {
	p := newSubnetPool()
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		_, subnet := p.Allocate(t)
		if seen[subnet] {
			t.Fatalf("duplicate subnet: %s", subnet)
		}
		seen[subnet] = true
	}
}

func TestSubnetPoolInsideBenchmarkRange(t *testing.T) {
	p := newSubnetPool()
	_, bench, _ := net.ParseCIDR("198.18.0.0/15")
	for i := 0; i < 50; i++ {
		gw, subnet := p.Allocate(t)

		gwIP := net.ParseIP(gw)
		if gwIP == nil {
			t.Fatalf("invalid gateway %q", gw)
		}
		if !bench.Contains(gwIP) {
			t.Errorf("gateway %s outside 198.18.0.0/15", gw)
		}

		_, sn, err := net.ParseCIDR(subnet)
		if err != nil {
			t.Fatalf("invalid subnet %q: %v", subnet, err)
		}
		if !bench.Contains(sn.IP) {
			t.Errorf("subnet %s outside 198.18.0.0/15", subnet)
		}
	}
}

func TestSubnetPoolWrapsCorrectly(t *testing.T) {
	// Force start near the end of the space, confirm wrap lands in
	// valid territory.
	p := &subnetPool{start: poolSize - 2, runID: "test"}
	firstGW, firstSubnet := p.Allocate(t)
	secondGW, secondSubnet := p.Allocate(t)
	thirdGW, thirdSubnet := p.Allocate(t)

	// After wrap, the third allocation should be at index 0 → 198.18.0.0/24.
	// The first two should be at 510 and 511.
	expectations := []struct {
		gw, subnet string
	}{
		{"198.19.254.1", "198.19.254.0/24"},
		{"198.19.255.1", "198.19.255.0/24"},
		{"198.18.0.1", "198.18.0.0/24"},
	}
	got := []struct{ gw, subnet string }{
		{firstGW, firstSubnet},
		{secondGW, secondSubnet},
		{thirdGW, thirdSubnet},
	}
	for i, e := range expectations {
		if got[i].gw != e.gw || got[i].subnet != e.subnet {
			t.Errorf("allocation %d: got (%s, %s), want (%s, %s)",
				i, got[i].gw, got[i].subnet, e.gw, e.subnet)
		}
	}
}

func TestSubnetPoolExhaustionFails(t *testing.T) {
	p := newSubnetPool()
	// Burn the whole pool.
	for i := 0; i < poolSize; i++ {
		p.Allocate(t)
	}
	// Next allocation must fail. Use a throwaway *testing.T so
	// Fatalf doesn't abort this test.
	fake := &testing.T{}
	defer func() {
		if !fake.Failed() {
			t.Errorf("expected pool to fail the test on exhaustion")
		}
	}()
	// Allocate() calls t.Helper() and t.Fatalf(); Fatalf calls
	// runtime.Goexit on the fake T. Wrap in a goroutine so we keep
	// running regardless.
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Allocate(fake)
	}()
	<-done
	// Touch p so the compiler doesn't lose it.
	_ = fmt.Sprintf("pool=%p", p)
}
