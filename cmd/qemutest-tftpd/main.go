// qemutest-tftpd is a thin TFTP server used exclusively by the
// qemutest integration harness to serve PXE/iPXE artifacts inside a
// per-test network namespace.
//
// Why a separate binary instead of an in-process tftp goroutine: the
// pin/tftp library opens a fresh UDP socket for every transfer
// (RFC 1350 requires the data port to differ from the control port),
// and those sockets are created on internal handler goroutines we
// can't pin to a netns-locked OS thread. Wrapping qemu-bridge-helper
// /etc/qemu/bridge.conf is fine, but in-process listener pinning
// breaks for TFTP. Running the server as a subprocess inside
// `ip netns exec <ns>` puts all sockets — main listener AND every
// per-transfer ephemeral — in the right netns by the simple fact of
// the process living there.
//
// Readiness handshake: the binary writes a single "ready\n" line to
// stdout after net.ListenUDP succeeds. The qemutest harness blocks
// on that line before launching the VM. Without it, the VM PXE-fetches
// before the bind completes and the test flakes.
//
// Subsequent stdout: structured-ish single-line "tftp:" prefixed
// log records. The harness drains these into a bytes.Buffer that's
// flushed via t.Log on cleanup. No request filtering is implemented
// — the netns isolation makes KnownIPs filtering redundant (only
// VMs on the bridge in the same netns can reach the listener).
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	gotftp "github.com/pin/tftp/v3"

	"github.com/bluesky-social/slagworks/internal/tftp"
)

func main() {
	addr := flag.String("addr", "", "bind address, e.g. 198.18.5.1:69")
	dir := flag.String("dir", "", "directory to serve")
	flag.Parse()

	if *addr == "" || *dir == "" {
		fmt.Fprintln(os.Stderr, "qemutest-tftpd: --addr and --dir are required")
		os.Exit(2)
	}

	udpAddr, err := net.ResolveUDPAddr("udp4", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qemutest-tftpd: resolve %q: %v\n", *addr, err)
		os.Exit(1)
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "qemutest-tftpd: listen %s: %v\n", *addr, err)
		os.Exit(1)
	}

	// Readiness handshake. The harness Scan()s for this line on a
	// timeout before launching the VM. MUST happen after ListenUDP
	// succeeds, BEFORE Serve blocks.
	if _, err := fmt.Println("ready"); err != nil {
		fmt.Fprintf(os.Stderr, "qemutest-tftpd: write ready: %v\n", err)
		os.Exit(1)
	}

	handler := tftp.NewReadHandler(*dir)

	srv := gotftp.NewServer(func(filename string, rf io.ReaderFrom) error {
		return handleRead(handler, filename, rf)
	}, nil)
	srv.SetBlockSize(1468)

	// SIGTERM/SIGINT triggers Shutdown. Serve returns nil when
	// Shutdown is called.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		log.Println("tftp: shutdown signal, stopping")
		srv.Shutdown()
	}()

	log.Printf("tftp: serving %s on %s", *dir, *addr)
	if err := srv.Serve(conn); err != nil {
		log.Printf("tftp: serve error: %v", err)
		os.Exit(1)
	}
}

// handleRead bridges pin/tftp's read-handler signature
// (filename, io.ReaderFrom) to our internal/tftp.ReadHandler. We
// don't filter by remote IP here — the netns the binary runs in is
// the only thing that can reach the listener. The remote-addr probe
// uses pin/tftp's OutgoingTransfer interface assertion (same shape
// our internal/tftp readHandler uses).
func handleRead(h *tftp.ReadHandler, filename string, rf io.ReaderFrom) error {
	var remote string
	if t, ok := rf.(gotftp.OutgoingTransfer); ok {
		addr := t.RemoteAddr()
		remote = addr.String()
	}

	rc, err := h.Open(filename)
	if err != nil {
		log.Printf("tftp: open %q: %v", filename, err)
		return err
	}
	defer rc.Close()

	n, err := rf.ReadFrom(rc)
	if err != nil {
		log.Printf("tftp: transfer %q to %s: %v", filename, remote, err)
		return err
	}
	log.Printf("tftp: served %q to %s (%d bytes)", filename, remote, n)
	return nil
}
