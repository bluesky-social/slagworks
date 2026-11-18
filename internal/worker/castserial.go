package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/bluesky-social/slagworks/internal/model"
)

// castTransport is the console transport a machine's cast pump uses.
type castTransport int

const (
	castTransportNone castTransport = iota
	castTransportIPMI
	castTransportAMT
	castTransportSerial
)

// castTransportFor picks a machine's console transport from its snapshot.
// BMC transports keep precedence: an AMT endpoint dials AMT redirection,
// any other OOB endpoint (BMCIP) dials ipmitool SOL. Only a machine with
// no BMC falls through to its cabled console-server line, and only on the
// worker that IS that console server (its verified hardware serial equals
// the console-server device's NetBox serial) — the tty is a local device
// node, so no other worker can reach it. An empty serial on either side
// never matches.
func castTransportFor(m *model.MachineSnapshot, selfSerial string) castTransport {
	switch {
	case m.AMT:
		return castTransportAMT
	case m.BMCIP != "":
		return castTransportIPMI
	case m.Console != nil && serialsMatch(m.Console.ServerSerial, selfSerial):
		return castTransportSerial
	default:
		return castTransportNone
	}
}

func serialsMatch(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	return a != "" && strings.EqualFold(a, b)
}

// SerialCastLister narrows the cast pump set to machines whose console is
// cabled to THIS worker's serial ports (castTransportSerial). The peer
// worker of the same site lists none of them, so it never dials, backs
// off, or issues resume RPCs for a line it cannot reach. Every such
// machine gets a pump regardless of lifecycle state: a local tty costs
// nothing to hold open and a switch's console matters most when it's
// misbehaving.
type SerialCastLister struct{ *Cache }

// ListActiveMachineIDs returns the machines this worker serial-captures.
func (l SerialCastLister) ListActiveMachineIDs() []uuid.UUID {
	self := l.SelfSerial()
	if strings.TrimSpace(self) == "" {
		return nil
	}
	v := l.view.Load()
	out := make([]uuid.UUID, 0)
	for id, m := range v.machines {
		if castTransportFor(m, self) == castTransportSerial {
			out = append(out, id)
		}
	}
	return out
}

// SerialTTYDialer opens a machine's console-server line: the local tty
// /dev/<SnapshotConsole.Port> on this worker, raw 8N1 at the snapshot's
// baud, no flow control. The line is opened read-only, so the pump can
// never send a byte to the console (a stray byte into a switch's login
// prompt or bootloader is not ours to send).
//
// Exclusive access: flock(LOCK_EX|LOCK_NB) makes a second slagworks pump
// (or any cooperating flock user) fail fast instead of interleaving reads,
// and TIOCEXCL refuses further opens by non-root processes (a getty or
// minicom in the dialout group). A missing device or a held lock fails the
// dial; the pump logs it and backs off like any other dial error.
type SerialTTYDialer struct {
	Cache  *Cache
	Logger *slog.Logger
	// DevDir is the directory holding the tty nodes; "" means /dev. A
	// test seam (tests point it at a pty's directory).
	DevDir string
}

func (d *SerialTTYDialer) Dial(ctx context.Context, machineID uuid.UUID) (CastSession, error) {
	m := d.Cache.MachineByID(machineID)
	if m == nil {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s not in cache", machineID)
	}
	if castTransportFor(m, d.Cache.SelfSerial()) != castTransportSerial {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s has no console-server line on this worker", machineID)
	}
	c := m.Console
	path, err := consoleDevicePath(d.DevDir, c.Port)
	if err != nil {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s: %w", machineID, err)
	}
	f, err := openSerialTTY(path, c.Baud)
	if err != nil {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s console %s:%s: %w", machineID, c.Server, c.Port, err)
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("castpump dialer: serial console opened",
		"machine_id", machineID, "hostname", m.Hostname, "console_server", c.Server, "tty", path, "baud", c.Baud)
	// A local tty has no transport chrome and cannot hang silently the
	// way a BMC's SOL relay can: a quiet line is a quiet console.
	return CastSession{Stream: &ttyStream{f: f}, NoSilenceWatchdog: true}, nil
}

// consoleDevicePath resolves a console-server port name to its device
// node. The name comes from NetBox, so it is held to a bare tty node name
// ("ttyS4"): no path separators, and the tty prefix, so a mistyped port
// name can never open some other device.
func consoleDevicePath(devDir, port string) (string, error) {
	if !strings.HasPrefix(port, "tty") || strings.ContainsAny(port, `/\`) || port != filepath.Clean(port) {
		return "", fmt.Errorf("console-server port name %q is not a tty device name (expected e.g. ttyS4); fix the console-server port name in NetBox", port)
	}
	if devDir == "" {
		devDir = "/dev"
	}
	return filepath.Join(devDir, port), nil
}

// openSerialTTY opens path read-only as a raw 8N1 line at baud with no
// flow control. O_NONBLOCK keeps open from waiting on carrier detect (and
// lets os.NewFile register the fd with the runtime poller, so Close
// unblocks a pending Read); O_NOCTTY keeps the line from becoming the
// daemon's controlling terminal.
func openSerialTTY(path string, baud int) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock %s: %w (another process holds this console line; stop it, slagworks captures this console)", path, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCEXCL, 0); err != nil {
		return nil, fmt.Errorf("TIOCEXCL %s: %w", path, err)
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, fmt.Errorf("get termios %s: %w (not a tty?)", path, err)
	}
	makeRawTermios(t)
	if err := setTermiosSpeed(t, baud); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, t); err != nil {
		return nil, fmt.Errorf("set termios %s: %w", path, err)
	}
	ok = true
	return os.NewFile(uintptr(fd), path), nil
}

// makeRawTermios is cfmakeraw plus 8N1, receiver on, modem control lines
// ignored (CLOCAL), and no hardware or software flow control.
func makeRawTermios(t *unix.Termios) {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL |
		unix.IXON | unix.IXOFF | unix.IXANY
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS
	t.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
}

var errReadOnlyConsole = errors.New("serial console is opened read-only; slagworks does not write to console-server lines")

// ttyStream adapts a read-only tty to the CastSession stream contract.
// Close is idempotent and, because the fd is registered with the runtime
// poller, unblocks a concurrent Read (which then returns os.ErrClosed).
type ttyStream struct {
	f    *os.File
	once sync.Once
}

func (s *ttyStream) Read(p []byte) (int, error)  { return s.f.Read(p) }
func (s *ttyStream) Write(p []byte) (int, error) { return 0, errReadOnlyConsole }

func (s *ttyStream) Close() error {
	s.once.Do(func() { _ = s.f.Close() })
	return nil
}
