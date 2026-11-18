// qemutest-serialrelay is a per-test subprocess that bridges the VM's
// serial chardev TCP socket to ipmi_sim's SOL device, while teeing the
// VM->BMC byte stream onto a unix-domain socket the test process reads.
//
// Why a separate binary, not an in-process goroutine: QEMU and ipmi_sim
// run inside a per-test network namespace. Their loopback is
// not the host's loopback. Goroutines in the test process bind on the
// host loopback, so neither QEMU nor ipmi_sim can dial a goroutine
// listener. The clean fix is to spawn this relay under
// `ip netns exec <ns>` so its sockets live in the same netns as the
// peers it bridges. The unix-socket tap, by contrast, is a filesystem
// object — it crosses netns boundaries naturally — so the test process
// (in host netns) can still read the captured stream.
//
// Wiring:
//
//	QEMU ttyS1 chardev ──connect──► relay :--vm-listen ──► tee ──► test (--tap unix)
//	                                          │
//	                                          ▼ (also forward)
//	ipmi_sim sol "tcp:..." ──connect──► relay :--bmc-listen
//
// QEMU connects first (set up at VM launch). ipmi_sim connects on
// demand when an `ipmitool sol activate` session opens. Both
// directions are best-effort: if the BMC connection isn't yet up, VM
// bytes still go to the tap; if the tap isn't yet open, VM bytes still
// go to the BMC. Both peers are reconnected if they drop, since
// ipmi_sim retries (per ipmi_lan(5)) and QEMU's chardev `reconnect=`
// flag handles the VM side.
//
// Readiness handshake: prints "ready vm=<port> bmc=<port>\n" on
// stdout after both listeners bind. The test process parses this to
// learn the dynamically-allocated ports.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "qemutest-serialrelay: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	vmListen := flag.String("vm-listen", "127.0.0.1:0", "bind address for the VM-side listener (QEMU connects here)")
	bmcListen := flag.String("bmc-listen", "127.0.0.1:0", "bind address for the BMC-side listener (ipmi_sim connects here)")
	tapPath := flag.String("tap", "", "unix socket path the relay dials and writes the VM->BMC byte stream to")
	flag.Parse()

	if *tapPath == "" {
		return fmt.Errorf("--tap is required")
	}

	vmL, err := net.Listen("tcp", *vmListen)
	if err != nil {
		return fmt.Errorf("listen vm: %w", err)
	}
	defer func() { _ = vmL.Close() }()

	bmcL, err := net.Listen("tcp", *bmcListen)
	if err != nil {
		return fmt.Errorf("listen bmc: %w", err)
	}
	defer func() { _ = bmcL.Close() }()

	vmPort := vmL.Addr().(*net.TCPAddr).Port
	bmcPort := bmcL.Addr().(*net.TCPAddr).Port
	fmt.Printf("ready vm=%d bmc=%d\n", vmPort, bmcPort)

	// SIGTERM/SIGINT close listeners so Accept() returns and exit
	// the process. The test harness sends SIGKILL on hard cleanup,
	// but a clean shutdown lets goroutines flush.
	//
	// os.Exit here is load-bearing: main() blocks on a second
	// <-sigCh below, but a single signal only delivers one value to
	// the buffered channel, so without an explicit exit on this
	// path SIGTERM'd processes wedge on the second receive.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		_ = vmL.Close()
		_ = bmcL.Close()
		os.Exit(0)
	}()

	// One QEMU session per relay run (matches one VM per BMCSim).
	vmConn, err := vmL.Accept()
	if err != nil {
		return fmt.Errorf("vm accept: %w", err)
	}
	log.Printf("vm connected from %s", vmConn.RemoteAddr())

	// Tap is dialed once, after vm accepts. The test process is the
	// listener on the unix socket; if it's not up yet, VM bytes drop
	// briefly until we connect. In practice the test sets up the tap
	// before launching the VM, so this is essentially synchronous.
	tap, err := net.Dial("unix", *tapPath)
	if err != nil {
		return fmt.Errorf("dial tap %q: %w", *tapPath, err)
	}
	log.Printf("tap connected to %s", *tapPath)

	// ipmi_sim's connection is opportunistic. It retries forever
	// (ipmi_lan(5): "the simulator will continue to try to connect"),
	// so if QEMU is up before ipmi_sim, VM bytes flow into the tap
	// alone until the BMC catches up. ipmi_sim may also reconnect
	// mid-test if its session drops, so the accept loop runs forever
	// (until the listener is closed on shutdown).
	bmcCh := make(chan net.Conn, 4)
	go func() {
		defer close(bmcCh)
		for {
			c, err := bmcL.Accept()
			if err != nil {
				return
			}
			log.Printf("bmc connected from %s", c.RemoteAddr())
			bmcCh <- c
		}
	}()

	var bmcConn net.Conn
	var bmcMu sync.Mutex

	// VM->{tap, bmc?}: drain the VM stream into tap first; opportunistic
	// fan-out to bmc when present. A short-write to bmc must not stall
	// the tap, so we copy via a small buffer and best-effort the bmc
	// side.
	go func() {
		defer func() { _ = vmConn.Close() }()
		buf := make([]byte, 4096)
		for {
			n, err := vmConn.Read(buf)
			if n > 0 {
				if _, terr := tap.Write(buf[:n]); terr != nil {
					log.Printf("tap write: %v (continuing)", terr)
				}
				bmcMu.Lock()
				bc := bmcConn
				bmcMu.Unlock()
				if bc != nil {
					// SetWriteDeadline so a wedged BMC peer can't
					// stall the VM byte stream. The tap path is the
					// load-bearing one for SerialMonitor; the BMC
					// path is best-effort.
					_ = bc.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
					if _, berr := bc.Write(buf[:n]); berr != nil {
						log.Printf("bmc write: %v (dropping bmc)", berr)
						_ = bc.Close()
						bmcMu.Lock()
						// Race-safe: only nil bmcConn if it still
						// points at the connection we observed
						// failing. A reconnect goroutine may have
						// already replaced it.
						if bmcConn == bc {
							bmcConn = nil
						}
						bmcMu.Unlock()
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					log.Printf("vm read: %v", err)
				}
				return
			}
		}
	}()

	// BMC->VM: when the BMC eventually connects, copy its writes back
	// to the VM. This direction is rarely used by SOL in our flows
	// (the agent writes to ttyS1 outbound; we don't currently inject
	// inbound), but supporting it keeps the simulator honest in case
	// a future test relies on it.
	//
	// Loops over bmcCh so reconnects are handled: if ipmi_sim's
	// session drops (its `tcp:` mode reconnects), the next bmcCh
	// receive picks up the new connection and resumes copying.
	go func() {
		for bc := range bmcCh {
			bmcMu.Lock()
			bmcConn = bc
			bmcMu.Unlock()
			_, err := io.Copy(vmConn, bc)
			if err != nil && err != io.EOF {
				log.Printf("bmc->vm copy: %v", err)
			}
			_ = bc.Close()
			bmcMu.Lock()
			if bmcConn == bc {
				bmcConn = nil
			}
			bmcMu.Unlock()
		}
	}()

	// Park forever; the signal goroutine os.Exits on SIGTERM/SIGINT.
	select {}
}
