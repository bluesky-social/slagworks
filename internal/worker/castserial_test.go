package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/bluesky-social/slagworks/internal/model"
)

const (
	gw1Serial = "9HNZ4Z2" // oob-gw-1.pop1
	gw2Serial = "GW2SERIAL"
)

func gw1Console(port string) *model.SnapshotConsole {
	return &model.SnapshotConsole{Server: "oob-gw-1.pop1", ServerSerial: gw1Serial, Port: port, Baud: 115200}
}

func TestCastTransportFor(t *testing.T) {
	cases := []struct {
		name string
		m    model.MachineSnapshot
		self string
		want castTransport
	}{
		{"amt", model.MachineSnapshot{AMT: true, BMCIP: "10.0.0.1"}, gw1Serial, castTransportAMT},
		{"bmc", model.MachineSnapshot{BMCIP: "10.0.0.1"}, gw1Serial, castTransportIPMI},
		{"bmc keeps precedence over console", model.MachineSnapshot{BMCIP: "10.0.0.1", Console: gw1Console("ttyS4")}, gw1Serial, castTransportIPMI},
		{"amt keeps precedence over console", model.MachineSnapshot{AMT: true, Console: gw1Console("ttyS4")}, gw1Serial, castTransportAMT},
		{"serial on own console server", model.MachineSnapshot{Console: gw1Console("ttyS4")}, gw1Serial, castTransportSerial},
		{"serial match ignores case/space", model.MachineSnapshot{Console: gw1Console("ttyS4")}, " 9hnz4z2 ", castTransportSerial},
		{"serial on peer console server", model.MachineSnapshot{Console: gw1Console("ttyS4")}, gw2Serial, castTransportNone},
		{"unverified self serial", model.MachineSnapshot{Console: gw1Console("ttyS4")}, "", castTransportNone},
		{"console server without serial", model.MachineSnapshot{Console: &model.SnapshotConsole{Server: "x", Port: "ttyS4"}}, "", castTransportNone},
		{"uncabled, no bmc", model.MachineSnapshot{}, gw1Serial, castTransportNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := castTransportFor(&tc.m, tc.self); got != tc.want {
				t.Fatalf("castTransportFor = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTransportCastDialer_SelectsSerial: a BMC-less machine cabled to this
// worker dials the serial transport; one cabled to the peer worker fails
// without dialing anything; a BMC machine with a console port stays on
// ipmitool.
func TestTransportCastDialer_SelectsSerial(t *testing.T) {
	cache := NewCacheWithSerial(nil, gw1Serial)
	local, peer, bmcWithConsole := uuid.New(), uuid.New(), uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: local, Console: gw1Console("ttyS4")})
	addMachineToCache(cache, &model.MachineSnapshot{ID: peer, Console: &model.SnapshotConsole{
		Server: "oob-gw-2.pop1", ServerSerial: gw2Serial, Port: "ttyS4", Baud: 115200,
	}})
	addMachineToCache(cache, &model.MachineSnapshot{ID: bmcWithConsole, BMCIP: "10.252.240.41", Console: gw1Console("ttyS5")})
	ipmi, amt, serial := &recordingDialer{}, &recordingDialer{}, &recordingDialer{}
	d := &TransportCastDialer{Cache: cache, IPMI: ipmi, AMT: amt, Serial: serial}

	if _, err := d.Dial(context.Background(), local); err != nil {
		t.Fatal(err)
	}
	if serial.calls.Load() != 1 || ipmi.calls.Load() != 0 || amt.calls.Load() != 0 {
		t.Fatalf("local serial machine: serial=%d ipmi=%d amt=%d, want 1/0/0", serial.calls.Load(), ipmi.calls.Load(), amt.calls.Load())
	}
	if _, err := d.Dial(context.Background(), peer); err == nil || !strings.Contains(err.Error(), "no console transport on this worker") {
		t.Fatalf("peer-cabled machine: err = %v, want no-transport error", err)
	}
	if _, err := d.Dial(context.Background(), bmcWithConsole); err != nil {
		t.Fatal(err)
	}
	if ipmi.calls.Load() != 1 || serial.calls.Load() != 1 {
		t.Fatalf("BMC machine with console: ipmi=%d serial=%d, want 1/1", ipmi.calls.Load(), serial.calls.Load())
	}

	noSerial := &TransportCastDialer{Cache: cache, IPMI: ipmi, AMT: amt}
	if _, err := noSerial.Dial(context.Background(), local); err == nil || !strings.Contains(err.Error(), "no serial transport") {
		t.Fatalf("serial machine without serial transport: err = %v", err)
	}
}

// TestSerialCastLister narrows to machines on THIS worker's console lines:
// the console server's worker lists them, its peer lists none, BMC
// machines never qualify (so the BMC manager's set stays disjoint), and an
// unverified worker lists nothing.
func TestSerialCastLister(t *testing.T) {
	spine, leaf, uncabled, bmc := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fill := func(c *Cache) {
		addMachineToCache(c, &model.MachineSnapshot{ID: spine, Console: gw1Console("ttyS4")})
		addMachineToCache(c, &model.MachineSnapshot{ID: leaf, Console: gw1Console("ttyS8")})
		addMachineToCache(c, &model.MachineSnapshot{ID: uncabled})
		addMachineToCache(c, &model.MachineSnapshot{ID: bmc, BMCIP: "10.252.240.41", BMCUser: "ADMIN", BMCPass: "pw", Console: gw1Console("ttyS5")})
	}

	self := NewCacheWithSerial(nil, gw1Serial)
	fill(self)
	got := SerialCastLister{Cache: self}.ListActiveMachineIDs()
	slices.SortFunc(got, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	want := []uuid.UUID{spine, leaf}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	if !slices.Equal(got, want) {
		t.Fatalf("console-server worker lists %v, want %v", got, want)
	}
	for _, id := range self.ListActiveMachineIDs() {
		if id == spine || id == leaf {
			t.Fatalf("BMC lister includes serial machine %s; the two pump sets must be disjoint", id)
		}
	}

	peer := NewCacheWithSerial(nil, gw2Serial)
	fill(peer)
	if got := (SerialCastLister{Cache: peer}).ListActiveMachineIDs(); len(got) != 0 {
		t.Fatalf("peer worker lists %v, want none", got)
	}

	unverified := NewCache(nil)
	fill(unverified)
	if got := (SerialCastLister{Cache: unverified}).ListActiveMachineIDs(); len(got) != 0 {
		t.Fatalf("unverified worker lists %v, want none", got)
	}
}

// TestCastPump_NoSilenceWatchdogSession: a session flagged
// NoSilenceWatchdog (a serial line) stays up through silence that would
// make the watchdog reconnect a BMC session.
func TestCastPump_NoSilenceWatchdogSession(t *testing.T) {
	stream := &blockingStream{closed: make(chan struct{})}
	dialer := &recordingDialer{sess: CastSession{Stream: stream, NoSilenceWatchdog: true}}
	pump := NewCastPump(CastPumpConfig{
		MachineID:         uuid.New(),
		Dialer:            dialer,
		Resume:            fixedResume{},
		Sink:              &collectingSink{},
		ReconnectBackoff:  5 * time.Millisecond,
		SilenceThreshold:  20 * time.Millisecond,
		SilenceCheckEvery: 5 * time.Millisecond,
		StateFunc:         func() string { return string(model.MachineStateRunning) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pump.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	got := dialer.calls.Load()
	cancel()
	<-done
	if got != 1 {
		t.Fatalf("silent serial session was re-dialed: %d dials, want 1", got)
	}
}

func TestConsoleDevicePath(t *testing.T) {
	if p, err := consoleDevicePath("", "ttyS4"); err != nil || p != "/dev/ttyS4" {
		t.Fatalf("ttyS4 = %q, %v", p, err)
	}
	for _, bad := range []string{"", "sda", "../ttyS4", "tty/../sda", "ttyS4/", "/dev/ttyS4", `tty\S4`} {
		if _, err := consoleDevicePath("", bad); err == nil {
			t.Errorf("port %q accepted, want rejected", bad)
		}
	}
}

// ptyConsole opens a pty pair and exposes its slave as <dir>/ttyTEST0 (a
// symlink: Linux slaves live at /dev/pts/N, which is not a tty* name).
// Returns the master, the slave (kept open for the test's lifetime), and
// the dev dir to hand the dialer.
func ptyConsole(t *testing.T) (master, slave *os.File, devDir string) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("pty unavailable: %v", err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	devDir = t.TempDir()
	if err := os.Symlink(slave.Name(), filepath.Join(devDir, "ttyTEST0")); err != nil {
		t.Fatal(err)
	}
	return master, slave, devDir
}

func serialTestCache(port string, baud int) (*Cache, uuid.UUID) {
	cache := NewCacheWithSerial(nil, gw1Serial)
	id := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: id, Hostname: "spine-1.pop1", Console: &model.SnapshotConsole{
		Server: "oob-gw-1.pop1", ServerSerial: gw1Serial, Port: port, Baud: baud,
	}})
	return cache, id
}

// TestSerialTTYDialer_StreamsRawBytes: the dialer opens the line raw at
// the snapshot baud, bytes written by the far end arrive untranslated,
// writes are refused, and Close unblocks a pending Read (idempotently,
// concurrently).
func TestSerialTTYDialer_StreamsRawBytes(t *testing.T) {
	master, slave, devDir := ptyConsole(t)
	cache, id := serialTestCache("ttyTEST0", 115200)
	d := &SerialTTYDialer{Cache: cache, DevDir: devDir}

	sess, err := d.Dial(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Stream.Close() }()
	if !sess.NoSilenceWatchdog {
		t.Error("serial session must opt out of the silence watchdog")
	}

	tio, err := unix.IoctlGetTermios(int(slave.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	if tio.Lflag&(unix.ICANON|unix.ECHO|unix.ISIG) != 0 || tio.Iflag&(unix.ICRNL|unix.IXON) != 0 || tio.Oflag&unix.OPOST != 0 {
		t.Errorf("line not raw: iflag=%#x oflag=%#x lflag=%#x", tio.Iflag, tio.Oflag, tio.Lflag)
	}
	if tio.Cflag&unix.CSIZE != unix.CS8 || tio.Cflag&(unix.PARENB|unix.CSTOPB|unix.CRTSCTS) != 0 || tio.Cflag&unix.CLOCAL == 0 {
		t.Errorf("line not 8N1/CLOCAL/no-flow-control: cflag=%#x", tio.Cflag)
	}
	if got := termiosBaud(tio); got != 115200 {
		t.Errorf("baud = %d, want 115200", got)
	}

	want := "login: \r\n\x1b[0m"
	if _, err := master.Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, len(want))
	chunk := make([]byte, 64)
	deadline := time.Now().Add(2 * time.Second)
	for len(buf) < len(want) && time.Now().Before(deadline) {
		n, err := sess.Stream.Read(chunk)
		if err != nil {
			t.Fatal(err)
		}
		buf = append(buf, chunk[:n]...)
	}
	if string(buf) != want {
		t.Fatalf("read %q, want %q (raw, untranslated)", buf, want)
	}

	if _, err := sess.Stream.Write([]byte("\n")); err == nil {
		t.Error("Write succeeded on a read-only console")
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := sess.Stream.Read(chunk)
		readDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { _ = sess.Stream.Close() })
	}
	wg.Wait()
	select {
	case err := <-readDone:
		if err == nil || !isStreamEnd(err) {
			t.Fatalf("blocked Read returned %v after Close, want a stream-end error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a pending Read")
	}
}

// TestSerialTTYDialer_ExclusiveAndAbsent: a second open of a held line
// fails instead of interleaving reads, a missing device fails the dial
// (the pump backs off), and a machine whose console isn't on this worker
// is refused.
func TestSerialTTYDialer_ExclusiveAndAbsent(t *testing.T) {
	_, _, devDir := ptyConsole(t)
	cache, id := serialTestCache("ttyTEST0", 115200)
	d := &SerialTTYDialer{Cache: cache, DevDir: devDir}
	first, err := d.Dial(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Stream.Close() }()
	if _, err := d.Dial(context.Background(), id); err == nil {
		t.Fatal("second dial of a held console line succeeded, want exclusive-access error")
	}

	missingCache, missingID := serialTestCache("ttyNOPE", 115200)
	_, err = (&SerialTTYDialer{Cache: missingCache, DevDir: devDir}).Dial(context.Background(), missingID)
	if err == nil || !errors.Is(err, unix.ENOENT) {
		t.Fatalf("absent device: err = %v, want ENOENT", err)
	}

	peerCache := NewCacheWithSerial(nil, gw2Serial)
	addMachineToCache(peerCache, &model.MachineSnapshot{ID: id, Console: gw1Console("ttyTEST0")})
	if _, err := (&SerialTTYDialer{Cache: peerCache, DevDir: devDir}).Dial(context.Background(), id); err == nil {
		t.Fatal("peer worker dialed a console line that isn't its own")
	}
}
