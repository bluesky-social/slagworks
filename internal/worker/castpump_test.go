package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeCastDialer drives the pump with a script of bytes and an exit
// signal, without spawning a real subprocess. The "pty" is a regular
// os.Pipe; the pump reads from it like a pty master.
type fakeCastDialer struct {
	mu      sync.Mutex
	scripts [][]byte // bytes to write per session, oldest first
	exitDur time.Duration
	calls   int

	// optional: invoked per-session by the dialer goroutine; tests
	// use this to interleave a write with the read loop's batching.
	custom func(w io.Writer)
}

// pipeStream is a CastSession stream over an os.Pipe: the pump reads pr;
// Close closes both ends (idempotent), waking a blocked Read like a
// closed pty master or TLS connection would.
type pipeStream struct {
	pr, pw *os.File
	once   sync.Once
}

func (s *pipeStream) Read(p []byte) (int, error)  { return s.pr.Read(p) }
func (s *pipeStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *pipeStream) Close() error {
	s.once.Do(func() { _ = s.pr.Close(); _ = s.pw.Close() })
	return nil
}

func (d *fakeCastDialer) Dial(ctx context.Context, _ uuid.UUID) (CastSession, error) {
	d.mu.Lock()
	d.calls++
	var script []byte
	if len(d.scripts) > 0 {
		script = d.scripts[0]
		d.scripts = d.scripts[1:]
	}
	custom := d.custom
	exitDur := d.exitDur
	d.mu.Unlock()

	pr, pw, err := os.Pipe()
	if err != nil {
		return CastSession{}, err
	}

	// Producer goroutine: writes the script, optionally runs the
	// custom hook, sleeps for exitDur, then closes the writer to
	// signal EOF on the pump's read side.
	go func() {
		if len(script) > 0 {
			_, _ = pw.Write(script)
		}
		if custom != nil {
			custom(pw)
		}
		if exitDur > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(exitDur):
			}
		}
		_ = pw.Close()
	}()

	return CastSession{Stream: &pipeStream{pr: pr, pw: pw}}, nil
}

type collectingSink struct {
	mu     sync.Mutex
	frames []CastFrame
}

func (s *collectingSink) PushCastFrame(f CastFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := f
	cp.Data = append([]byte(nil), f.Data...)
	s.frames = append(s.frames, cp)
}

func (s *collectingSink) Frames() []CastFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CastFrame, len(s.frames))
	copy(out, s.frames)
	return out
}

type fixedResume struct{ v int64 }

func (r fixedResume) ResumeOffsetUs(_ context.Context, _ uuid.UUID) (int64, error) {
	return r.v, nil
}

// sinkBackedResume models the production behavior: each new session
// starts at last-emitted-t_us + a small gap. Tests that span
// reconnects need this so monotonicity holds across sessions.
type sinkBackedResume struct {
	sink    *collectingSink
	initial int64
	gapUs   int64
}

func (r *sinkBackedResume) ResumeOffsetUs(_ context.Context, _ uuid.UUID) (int64, error) {
	frames := r.sink.Frames()
	if len(frames) == 0 {
		return r.initial, nil
	}
	max := frames[0].TUs
	for _, f := range frames {
		if f.TUs > max {
			max = f.TUs
		}
	}
	return max + r.gapUs, nil
}

// TestCastPump_FlushOnSize: bytes larger than FlushSize trigger an
// immediate flush. We send 8KB with FlushSize=4096 and expect at
// least two frames before EOF.
func TestCastPump_FlushOnSize(t *testing.T) {
	mid := uuid.New()
	payload := make([]byte, 8192)
	for i := range payload {
		payload[i] = byte('A' + i%26)
	}
	dialer := &fakeCastDialer{
		scripts: [][]byte{payload},
		exitDur: 50 * time.Millisecond,
	}
	sink := &collectingSink{}

	pump := NewCastPump(CastPumpConfig{
		MachineID:        mid,
		Dialer:           dialer,
		Resume:           fixedResume{v: 0},
		Sink:             sink,
		FlushInterval:    1 * time.Second, // long, so size rule wins
		FlushSize:        4096,
		ReadBufSize:      4096,
		ReconnectBackoff: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go pump.Run(ctx)
	// Let one session complete + a tiny bit of reconnect wait.
	time.Sleep(300 * time.Millisecond)
	cancel()

	frames := sink.Frames()
	if len(frames) < 2 {
		t.Fatalf("expected ≥2 frames (size-based flushes), got %d", len(frames))
	}
	// Concatenated bytes must equal the input.
	var got []byte
	for _, f := range frames {
		got = append(got, f.Data...)
		if f.MachineID != mid {
			t.Errorf("frame machine_id = %v, want %v", f.MachineID, mid)
		}
	}
	if string(got) != string(payload) {
		t.Errorf("concatenated frame data != payload (lengths %d vs %d)", len(got), len(payload))
	}
}

// TestCastPump_FlushOnInterval: a single 100-byte chunk smaller
// than FlushSize gets emitted by the time-based rule. We send 100
// bytes and wait long enough for the FlushInterval ticker to fire.
func TestCastPump_FlushOnInterval(t *testing.T) {
	mid := uuid.New()
	payload := []byte("hello world\n")
	dialer := &fakeCastDialer{
		scripts: [][]byte{payload},
		exitDur: 500 * time.Millisecond,
	}
	sink := &collectingSink{}

	pump := NewCastPump(CastPumpConfig{
		MachineID:        mid,
		Dialer:           dialer,
		Resume:           fixedResume{v: 0},
		Sink:             sink,
		FlushInterval:    50 * time.Millisecond,
		FlushSize:        4096,
		ReadBufSize:      4096,
		ReconnectBackoff: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go pump.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()

	frames := sink.Frames()
	if len(frames) < 1 {
		t.Fatalf("expected ≥1 frame, got 0")
	}
	if string(frames[0].Data) != "hello world\n" {
		t.Errorf("frame data = %q, want %q", frames[0].Data, payload)
	}
}

// TestCastPump_TUsMonotonic: across multiple frames + reconnect
// cycles, t_us must be strictly increasing and >= prev_max_us. The
// sinkBackedResume models the production resume-offset RPC by
// returning last-emitted-t_us + small gap on every session start —
// the same shape as global's MAX(t_us)+wall-clock-gap math.
func TestCastPump_TUsMonotonic(t *testing.T) {
	mid := uuid.New()
	const startOffset int64 = 5_000_000 // 5 seconds in µs
	dialer := &fakeCastDialer{
		scripts: [][]byte{
			[]byte("first\n"),
			[]byte("second\n"),
			[]byte("third\n"),
		},
		exitDur: 80 * time.Millisecond,
	}
	sink := &collectingSink{}
	resume := &sinkBackedResume{sink: sink, initial: startOffset, gapUs: 100_000}

	pump := NewCastPump(CastPumpConfig{
		MachineID:        mid,
		Dialer:           dialer,
		Resume:           resume,
		Sink:             sink,
		FlushInterval:    20 * time.Millisecond,
		FlushSize:        4096,
		ReadBufSize:      4096,
		ReconnectBackoff: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go pump.Run(ctx)
	time.Sleep(700 * time.Millisecond)
	cancel()

	frames := sink.Frames()
	if len(frames) < 2 {
		t.Fatalf("expected ≥2 frames (multiple sessions), got %d", len(frames))
	}
	prev := startOffset - 1
	for i, f := range frames {
		if f.TUs <= prev {
			t.Errorf("frame %d: t_us=%d not strictly > prev=%d", i, f.TUs, prev)
		}
		if f.TUs < startOffset {
			t.Errorf("frame %d: t_us=%d < prev_max_us=%d (lost monotonic guarantee)",
				i, f.TUs, startOffset)
		}
		prev = f.TUs
	}
}

// TestCastPump_RespectsContext: canceling the parent context exits
// the Run loop within a short bound.
func TestCastPump_RespectsContext(t *testing.T) {
	mid := uuid.New()
	dialer := &fakeCastDialer{
		scripts: [][]byte{[]byte("alive")},
		exitDur: 10 * time.Second, // dialer would otherwise hold us
	}
	sink := &collectingSink{}

	pump := NewCastPump(CastPumpConfig{
		MachineID:        mid,
		Dialer:           dialer,
		Resume:           fixedResume{v: 0},
		Sink:             sink,
		FlushInterval:    20 * time.Millisecond,
		ReconnectBackoff: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pump.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit within 2s of ctx cancel")
	}
}

// TestCastPump_DialErrorRetries: a dialer that errors out the first
// session continues into the next dial attempt rather than giving
// up on the pump.
func TestCastPump_DialErrorRetries(t *testing.T) {
	mid := uuid.New()
	var calls int
	var mu sync.Mutex
	failOnce := &erroringDialer{
		next: func(ctx context.Context, m uuid.UUID) (CastSession, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return CastSession{}, errors.New("simulated dial failure")
			}
			d := &fakeCastDialer{scripts: [][]byte{[]byte("recovered\n")}, exitDur: 30 * time.Millisecond}
			return d.Dial(ctx, m)
		},
	}
	sink := &collectingSink{}

	pump := NewCastPump(CastPumpConfig{
		MachineID:        mid,
		Dialer:           failOnce,
		Resume:           fixedResume{v: 0},
		Sink:             sink,
		FlushInterval:    20 * time.Millisecond,
		ReconnectBackoff: 30 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	go pump.Run(ctx)
	time.Sleep(300 * time.Millisecond)
	cancel()

	frames := sink.Frames()
	if len(frames) < 1 {
		t.Fatalf("expected at least one recovered frame after retry, got 0")
	}
	if string(frames[0].Data) != "recovered\n" {
		t.Errorf("recovered frame data = %q, want %q", frames[0].Data, "recovered\n")
	}
}

type erroringDialer struct {
	next func(ctx context.Context, m uuid.UUID) (CastSession, error)
}

func (d *erroringDialer) Dial(ctx context.Context, m uuid.UUID) (CastSession, error) {
	return d.next(ctx, m)
}
