//go:build integration

package qemutest

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"

	"github.com/vishvananda/netns"
)

// InNetns runs fn while the calling goroutine is pinned to a fresh OS
// thread that has been switched into the named network namespace.
//
// Three rules are enforced and non-negotiable:
//
//  1. The thread is locked to the goroutine for the duration of fn so
//     the Go runtime cannot recycle it into another goroutine while
//     it is in our namespace.
//
//  2. Both the original and target netns file descriptors are closed
//     before return. Each *netns.NsHandle holds an open fd; failing
//     to close them leaks one fd per call until RLIMIT_NOFILE.
//
//  3. If restoring the original namespace fails, InNetns panics
//     rather than returning. Returning would let the caller continue
//     executing on a corrupted thread, doing ostensibly-host-netns
//     work (DNS lookups, opens, sockets) inside the test netns. A
//     loud crash is the only correct behavior — silent corruption
//     would surface as flakes in unrelated tests.
//
// fn's return value is the function's return value. Any error from
// the namespace plumbing itself (Get/GetFromName/Set) is wrapped and
// returned without invoking fn; callers see it the same way they
// would any other error.
func InNetns(nsName string, fn func() error) (retErr error) {
	runtime.LockOSThread()
	// DO NOT defer UnlockOSThread unconditionally. Each early-return
	// path must explicitly unlock before the namespace switch happens,
	// because there is no defer to restore from yet.

	origNS, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("netns.Get: %w", err)
	}
	defer origNS.Close()

	testNS, err := netns.GetFromName(nsName)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("netns.GetFromName(%q): %w", nsName, err)
	}
	defer testNS.Close()

	if err := netns.Set(testNS); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("netns.Set(test=%q): %w", nsName, err)
	}

	defer func() {
		if err := netns.Set(origNS); err == nil {
			runtime.UnlockOSThread()
			return
		} else {
			// Cannot recover: the OS thread is permanently in the
			// wrong netns. Returning would leave the caller running
			// on a corrupted thread. Panic crashes the test binary
			// loud and prevents the runtime from recycling this
			// thread into other goroutines.
			panic(fmt.Sprintf("InNetns: netns.Set(orig) failed; thread permanently corrupted: %v", err))
		}
	}()

	return fn()
}

// NSListen creates a net.Listener bound inside the named netns. The
// returned listener's underlying fd carries the netns; subsequent
// Accept() calls inherit it via the kernel even if the serving
// goroutine is not pinned.
func NSListen(nsName, network, addr string) (net.Listener, error) {
	var ln net.Listener
	err := InNetns(nsName, func() error {
		var err error
		ln, err = net.Listen(network, addr)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// NSListenPacket creates a net.PacketConn bound inside the named
// netns. Same fd-carries-netns guarantee as NSListen.
func NSListenPacket(nsName, network, addr string) (net.PacketConn, error) {
	var pc net.PacketConn
	err := InNetns(nsName, func() error {
		var err error
		pc, err = net.ListenPacket(network, addr)
		return err
	})
	if err != nil {
		return nil, err
	}
	return pc, nil
}

// NSExec wraps an existing argv with `ip netns exec <nsName>`. The
// returned *exec.Cmd inherits the namespace through the
// `ip netns exec` setns/exec wrapper; no goroutine pinning is
// required because the subprocess never returns to the host netns.
//
// Note: this does NOT call cmd.Run/Start. The caller is responsible
// for the subprocess lifecycle.
func NSExec(nsName, name string, args ...string) *exec.Cmd {
	full := append([]string{"netns", "exec", nsName, name}, args...)
	return exec.Command("ip", full...)
}
