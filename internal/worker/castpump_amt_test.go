package worker

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

type recordingDialer struct {
	calls atomic.Int64
	sess  CastSession
}

func (d *recordingDialer) Dial(context.Context, uuid.UUID) (CastSession, error) {
	d.calls.Add(1)
	return d.sess, nil
}

// TestTransportCastDialer_SelectsByAMTFlag is the D2 gate for SOL: an AMT
// machine always dials AMT redirection, never ipmitool, and a worker
// without ipmitool still streams its AMT machines.
func TestTransportCastDialer_SelectsByAMTFlag(t *testing.T) {
	cache := NewCache(nil)
	amtID, bmcID := uuid.New(), uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: amtID, BMCIP: "10.252.240.23", AMT: true})
	addMachineToCache(cache, &model.MachineSnapshot{ID: bmcID, BMCIP: "10.252.240.41"})
	ipmi, amt := &recordingDialer{}, &recordingDialer{}
	d := &TransportCastDialer{Cache: cache, IPMI: ipmi, AMT: amt}

	if _, err := d.Dial(context.Background(), amtID); err != nil {
		t.Fatal(err)
	}
	if amt.calls.Load() != 1 || ipmi.calls.Load() != 0 {
		t.Fatalf("AMT machine: amt=%d ipmi=%d dials, want 1/0", amt.calls.Load(), ipmi.calls.Load())
	}
	if _, err := d.Dial(context.Background(), bmcID); err != nil {
		t.Fatal(err)
	}
	if ipmi.calls.Load() != 1 {
		t.Fatalf("BMC machine: ipmi dials = %d, want 1", ipmi.calls.Load())
	}

	noIPMI := &TransportCastDialer{Cache: cache, AMT: amt}
	if _, err := noIPMI.Dial(context.Background(), bmcID); err == nil || !strings.Contains(err.Error(), "ipmitool is not on PATH") {
		t.Errorf("BMC machine without ipmitool: err = %v", err)
	}
	if _, err := noIPMI.Dial(context.Background(), amtID); err != nil {
		t.Errorf("AMT machine must not need ipmitool: %v", err)
	}
	if _, err := d.Dial(context.Background(), uuid.New()); err == nil {
		t.Error("unknown machine: want error")
	}
}

// TestAMTOnlyCastLister: on a worker without ipmitool only AMT machines
// with a credential get pumps.
func TestAMTOnlyCastLister(t *testing.T) {
	cache := NewCache(nil)
	amtID, bmcID := uuid.New(), uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: amtID, BMCIP: "10.252.240.23", AMT: true, BMCUser: "admin", BMCPass: "pw"})
	addMachineToCache(cache, &model.MachineSnapshot{ID: bmcID, BMCIP: "10.252.240.41", BMCUser: "ADMIN", BMCPass: "pw"})
	if got := len(cache.ListActiveMachineIDs()); got != 2 {
		t.Fatalf("full lister: %d machines, want 2", got)
	}
	got := AMTOnlyCastLister{Cache: cache}.ListActiveMachineIDs()
	if len(got) != 1 || got[0] != amtID {
		t.Fatalf("AMT-only lister = %v, want [%s]", got, amtID)
	}
}

type nopStream struct{ io.Reader }

func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (nopStream) Close() error                { return nil }

func TestAMTSOLDialer_EndpointCredsAndGates(t *testing.T) {
	cache := NewCache(nil)
	amtID, noCredID, bmcID := uuid.New(), uuid.New(), uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: amtID, BMCIP: "10.252.240.23", AMT: true, BMCUser: "admin", BMCPass: "amt-pw"})
	addMachineToCache(cache, &model.MachineSnapshot{ID: noCredID, BMCIP: "10.252.240.24", AMT: true})
	addMachineToCache(cache, &model.MachineSnapshot{ID: bmcID, BMCIP: "10.252.240.41", BMCUser: "ADMIN", BMCPass: "pw"})

	var gotHost, gotUser, gotPass string
	var hadDeadline bool
	d := &AMTSOLDialer{Cache: cache, dial: func(ctx context.Context, host, user, pass string) (io.ReadWriteCloser, error) {
		_, hadDeadline = ctx.Deadline()
		gotHost, gotUser, gotPass = host, user, pass
		return nopStream{strings.NewReader("")}, nil
	}}
	sess, err := d.Dial(context.Background(), amtID)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if gotHost != "10.252.240.23" || gotUser != "admin" || gotPass != "amt-pw" {
		t.Errorf("dialed %s as %s/%s, want the snapshot oob_ip and AMT creds", gotHost, gotUser, gotPass)
	}
	if !hadDeadline {
		t.Error("handshake must be bounded by a deadline")
	}
	if sess.Noise != nil {
		t.Error("AMT sessions carry no ipmitool chrome; Noise must be nil (passthrough)")
	}
	if _, err := d.Dial(context.Background(), noCredID); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Errorf("AMT machine without creds: err = %v", err)
	}
	if _, err := d.Dial(context.Background(), bmcID); err == nil {
		t.Error("non-AMT machine: want refusal")
	}
}

// TestIPMIToolPTYDialer_RefusesAMT: the ipmitool dialer itself refuses an
// AMT machine, so no wiring mistake can point ipmitool at an AMT endpoint.
func TestIPMIToolPTYDialer_RefusesAMT(t *testing.T) {
	cache := NewCache(nil)
	id := uuid.New()
	addMachineToCache(cache, &model.MachineSnapshot{ID: id, BMCIP: "10.252.240.23", AMT: true, BMCUser: "admin", BMCPass: "pw"})
	d := &IPMIToolPTYDialer{Cache: cache, Path: "/nonexistent/ipmitool"}
	if _, err := d.Dial(context.Background(), id); err == nil || !strings.Contains(err.Error(), "Intel AMT") {
		t.Fatalf("err = %v, want the AMT refusal", err)
	}
}

// scriptStream yields one script then EOF.
type scriptStream struct {
	r      io.Reader
	closed atomic.Bool
}

func (s *scriptStream) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s *scriptStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *scriptStream) Close() error                { s.closed.Store(true); return nil }

// TestCastPump_NoiseFilterIsPerSession: the pump applies the session's own
// filter: ipmitool chrome is stripped when the dialer supplies the ipmitool
// filter, and the same bytes pass verbatim through a filterless (AMT)
// session.
func TestCastPump_NoiseFilterIsPerSession(t *testing.T) {
	const banner = "[SOL Session operational.  Use ~? for help]\r\n"
	for _, tc := range []struct {
		name  string
		noise CastNoiseFilter
		want  string
	}{
		{"ipmitool", &solNoiseFilter{}, "login: \r\n"},
		{"amt", nil, banner + "login: \r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &scriptStream{r: strings.NewReader(banner + "login: \r\n")}
			sink := &collectingSink{}
			p := NewCastPump(CastPumpConfig{
				MachineID: uuid.New(), Resume: fixedResume{}, Sink: sink,
				Dialer:        &recordingDialer{sess: CastSession{Stream: stream, Noise: tc.noise}},
				FlushInterval: 10 * time.Millisecond,
			})
			if err := p.session(context.Background(), testLogger()); err != nil {
				t.Fatalf("session: %v", err)
			}
			var got strings.Builder
			for _, f := range sink.Frames() {
				got.Write(f.Data)
			}
			if got.String() != tc.want {
				t.Errorf("cast = %q, want %q", got.String(), tc.want)
			}
			if !stream.closed.Load() {
				t.Error("stream must be closed when the session ends")
			}
		})
	}
}

// blockingStream blocks Read until Close, like a quiet TLS connection.
type blockingStream struct {
	once   sync.Once
	closed chan struct{}
}

func (s *blockingStream) Read([]byte) (int, error) {
	<-s.closed
	return 0, errors.New("use of closed network connection")
}
func (s *blockingStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *blockingStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// TestCastPump_CancelClosesBlockedStream: cancelling the pump closes a
// stream whose Read is blocked, so a hung AMT session can't wedge Run.
func TestCastPump_CancelClosesBlockedStream(t *testing.T) {
	stream := &blockingStream{closed: make(chan struct{})}
	p := NewCastPump(CastPumpConfig{
		MachineID: uuid.New(), Resume: fixedResume{}, Sink: &collectingSink{},
		Dialer: &recordingDialer{sess: CastSession{Stream: stream}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel with a blocked stream")
	}
}

// TestPTYStream_CloseKillsAndReaps: Close on an ipmitool-shaped stream kills
// a child that would otherwise run forever, reaps it, and is idempotent.
func TestPTYStream_CloseKillsAndReaps(t *testing.T) {
	ctx, kill := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "/bin/sleep", "60")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		kill()
		t.Skipf("pty unavailable: %v", err)
	}
	s := newPTYStream(cmd, ptmx, kill)
	done := make(chan struct{})
	go func() { _ = s.Close(); _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not kill and reap the child")
	}
	if cmd.ProcessState == nil {
		t.Error("child was not reaped")
	}
}
