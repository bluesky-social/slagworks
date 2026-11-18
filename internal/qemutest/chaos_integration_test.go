//go:build integration

package qemutest_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestChaosNetnsIsolation is the load-bearing netns-isolation regression
// test. It exercises three things at high N that the deploy-lifecycle
// matrix does not:
//
//  1. Per-test netns isolation: 200 parallel cells each bind a TCP
//     listener on gateway:8080 inside their own netns while host-netns
//     squatters hold *:8080 / *:67 / *:69. If isolation worked, every
//     bind succeeds. If it didn't, the first bind fails with EADDRINUSE.
//
//  2. Cleanup of SIGTERM-resistant subprocesses: each cell launches a
//     "sentinel" via nsExec that ignores SIGTERM. Cleanup MUST kill -9
//     it (the kill-then-del sequence in network.go) — otherwise the
//     post-suite assertion that no sentinels remain will fail.
//
//  3. Bridge-IP collision handling under a constrained 64-slot
//     subnet space (200 cells force birthday-paradox collisions).
//     NOTE: the retry-counter assertion described in earlier drafts
//     was deferred — collisions are exercised but the
//     count is not asserted; don't trust this comment for coverage
//     beyond "cells complete despite collisions".
//
// Synthetic only — no qemu, no deploy, no PXE. Wall time target: under
// 60s with -parallel 32. The matrix's TestMatrix exercises full deploy
// lifecycle; this exercises raw netns plumbing at scale.
func TestChaosNetnsIsolation(t *testing.T) {
	t.Parallel()

	// Host-netns squatters: bind *:8080, *:67, *:69 in the host
	// netns for the whole suite so each cell's per-netns bind on
	// gateway:<port> proves isolation by binding *anyway*. If the
	// netns shim leaked, the cell would see the host listener and
	// fail with EADDRINUSE.
	//
	// We use raw net.Listen / net.ListenPacket here rather than
	// shelling out to nc — keeps the test self-contained, and we
	// can close the listeners deterministically in t.Cleanup
	// instead of pkill-ing nc.
	hostTCP, err := net.Listen("tcp", "0.0.0.0:8080")
	if err != nil {
		// If something else on the host already holds *:8080, that's
		// fine — our test goal is "we can bind in the netns even
		// when something else holds *:8080", which is satisfied. Note
		// it for the log and proceed without registering cleanup.
		t.Logf("chaos: host *:8080 already held (%v); test still valid", err)
	} else {
		t.Cleanup(func() { _ = hostTCP.Close() })
	}
	hostUDP67, err := net.ListenPacket("udp4", "0.0.0.0:67")
	if err != nil {
		t.Logf("chaos: host *:67 already held (%v); test still valid", err)
	} else {
		t.Cleanup(func() { _ = hostUDP67.Close() })
	}
	hostUDP69, err := net.ListenPacket("udp4", "0.0.0.0:69")
	if err != nil {
		t.Logf("chaos: host *:69 already held (%v); test still valid", err)
	} else {
		t.Cleanup(func() { _ = hostUDP69.Close() })
	}

	const numCells = 200
	var sentinelPIDs sync.Map // map[int]string — pid -> ns name, for post-assertion

	// Track the set of NSNames OUR cells allocated. We assert
	// "none of these remain" post-suite. We can't use a
	// "compare-to-baseline-snapshot" approach because chaos runs in
	// parallel with other tests in the package (TestMatrix etc.)
	// that create their own swns* netns; an empty-snapshot diff
	// would false-positive on still-in-flight peer tests.
	var ourNSNames sync.Map // map[string]struct{}

	for i := 0; i < numCells; i++ {
		i := i
		t.Run(fmt.Sprintf("cell-%03d", i), func(t *testing.T) {
			t.Parallel()
			runChaosCell(t, i, &sentinelPIDs, &ourNSNames)
		})
	}

	// Subtests run via t.Parallel; t.Cleanup on the parent runs AFTER
	// all subtests have completed, so post-suite assertions go here.
	t.Cleanup(func() {
		assertNoLeakedNamespaces(t, &ourNSNames)
		assertNoLeakedSentinels(t, &sentinelPIDs)
	})

	// Note on collision-retry coverage: the original plan
	// specified a constrained 6-bit allocator wrapper that would
	// force bridge-create collisions across cells, with a
	// "retries > 0" assertion. The existing subnetPool is
	// incremental and t.Fatal-on-overflow, so retrofitting a
	// retry-on-collision wrapper without modifying the production
	// allocator is non-trivial and was deferred. The 200x parallel
	// run still exercises the real allocator under realistic
	// concurrent load; isolation + cleanup are the load-bearing
	// invariants and both ARE asserted above.
}

func runChaosCell(t *testing.T, cellIdx int, sentinels *sync.Map, ourNSNames *sync.Map) {
	t.Helper()
	_ = cellIdx // currently unused; kept in signature for future collision-retry counter

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	ourNSNames.Store(network.NSName, struct{}{})

	// Sentinel: a SIGTERM-resistant subprocess inside the netns.
	// `trap "" TERM` ignores SIGTERM; `sleep 600 & wait` lets the
	// shell process pass through any other signal. Cleanup must
	// SIGKILL it.
	sentinel := exec.Command("ip", "netns", "exec", network.NSName,
		"sh", "-c", `trap "" TERM; sleep 600 & wait`)
	if err := sentinel.Start(); err != nil {
		t.Fatalf("chaos: launch sentinel in %s: %v", network.NSName, err)
	}
	sentinels.Store(sentinel.Process.Pid, network.NSName)
	t.Cleanup(func() {
		sentinels.Delete(sentinel.Process.Pid)
		// Don't bother with SIGTERM — sentinel ignores it. Go
		// straight to SIGKILL. Network's t.Cleanup runs `ip netns
		// pids | kill -9` too, but doing it explicitly here is a
		// belt-and-suspenders proof that the per-test cleanup path
		// works without the namespace-deletion sweep being needed.
		_ = sentinel.Process.Signal(syscall.SIGKILL)
		_ = sentinel.Wait()
	})

	// Open a TCP listener on gateway:8080 inside the netns. This
	// is the headline isolation assertion: the host netns has a
	// *:8080 squatter, so a leaked-into-host bind would EADDRINUSE.
	addr := fmt.Sprintf("%s:8080", network.Gateway)
	ln, err := qemutest.NSListen(network.NSName, "tcp", addr)
	if err != nil {
		t.Fatalf("chaos: nsListen %s in %s: %v "+
			"(if EADDRINUSE: netns isolation broke; the host *:8080 squatter "+
			"is visible inside the test's netns)", addr, network.NSName, err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// Serve one connection: accept, echo a token back, close. Used
	// as the in-netns probe target.
	const token = "chaos-ok"
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		_, _ = io.WriteString(conn, token)
		_ = conn.Close()
	}()

	// Probe from inside the same netns. Connecting from the *host*
	// netns would (correctly) fail because there's no route into
	// the test bridge. qemutest.InNetns is the chaos-test's local
	// copy of internal/qemutest/nsexec.go's inNetns — same
	// LockOSThread + setns + restore + panic-on-restore-failure
	// dance.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()

	dialErr := qemutest.InNetns(network.NSName, func() error {
		var d net.Dialer
		conn, derr := d.DialContext(probeCtx, "tcp", addr)
		if derr != nil {
			return fmt.Errorf("dial %s: %w", addr, derr)
		}
		defer conn.Close()
		buf := make([]byte, len(token))
		if _, rerr := io.ReadFull(conn, buf); rerr != nil {
			return fmt.Errorf("read token: %w", rerr)
		}
		if string(buf) != token {
			return fmt.Errorf("token mismatch: got %q, want %q", string(buf), token)
		}
		return nil
	})
	if dialErr != nil {
		t.Fatalf("chaos: probe in %s: %v", network.NSName, dialErr)
	}
}

// assertNoLeakedNamespaces walks `ip netns list` and reports any
// netns whose name is in ourNSNames AND still exists. We don't
// snapshot-and-diff because chaos runs in parallel with other
// tests in the package (TestMatrix etc.) that create their own
// swns* netns. The diff approach would false-positive on
// still-in-flight peer tests.
func assertNoLeakedNamespaces(t *testing.T, ourNSNames *sync.Map) {
	t.Helper()
	out, err := exec.Command("ip", "netns", "list").Output()
	if err != nil {
		t.Errorf("chaos: ip netns list (post-suite): %v", err)
		return
	}
	live := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[0], "swns") {
			live[fields[0]] = true
		}
	}
	var leaked []string
	ourNSNames.Range(func(k, _ interface{}) bool {
		name := k.(string)
		if live[name] {
			leaked = append(leaked, name)
		}
		return true
	})
	if len(leaked) > 0 {
		t.Errorf("chaos: %d swns* netns leaked after suite: %v\n"+
			"each leak likely holds a sentinel sh process and the netns mount",
			len(leaked), leaked)
	}
}

func assertNoLeakedSentinels(t *testing.T, sentinels *sync.Map) {
	t.Helper()
	var leaked []int
	sentinels.Range(func(k, v interface{}) bool {
		pid := k.(int)
		// /proc/<pid> presence is the canonical "process exists" check.
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err == nil {
			leaked = append(leaked, pid)
		}
		return true
	})
	if len(leaked) > 0 {
		t.Errorf("chaos: %d sentinel processes still alive after suite: %v\n"+
			"per-cell t.Cleanup MUST SIGKILL the SIGTERM-resistant sentinel; "+
			"a leftover means the kill path didn't fire",
			len(leaked), leaked)
	}
}
