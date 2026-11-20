package worker

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// Property tests for CastPump over the fake pipe dialer.
//
// Model: the cast is the concatenation, across sessions, of each
// session's script with ipmitool chrome removed (oracleStrip from
// solnoise_property_test.go; the fake dialer is given a solNoiseFilter like
// the ipmitool dialer). Properties:
//   - no byte lost, duplicated or reordered: concat(frames.Data) equals
//     the model;
//   - t_us strictly increasing across every frame of the cast (the
//     console stream's cursor is t_us+1, so equal t_us loses a frame),
//     whether each session resumes past the last frame emitted or from
//     a stale offset that lags this pump's own frames;
//   - Run returns promptly after ctx cancel.

type scriptedNoiseDialer struct {
	fakeCastDialer
}

func (d *scriptedNoiseDialer) Dial(ctx context.Context, id uuid.UUID) (CastSession, error) {
	s, err := d.fakeCastDialer.Dial(ctx, id)
	if err != nil {
		return s, err
	}
	s.Noise = &solNoiseFilter{}
	return s, nil
}

func TestCastPump_Property_BytesPreservedAndTUsStrictlyIncreasing(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sessions := rapid.IntRange(1, 4).Draw(t, "sessions")
		var scripts [][]byte
		var want []byte
		for i := 0; i < sessions; i++ {
			s := genNoiseStream(t)
			// Sometimes a large burst so the read loop flushes on size
			// back to back (a pty backlog after a stall).
			if rapid.IntRange(0, 3).Draw(t, "burst") == 0 {
				s = append(s, bytes.Repeat([]byte("B"), rapid.IntRange(4096, 64*1024).Draw(t, "burstLen"))...)
			}
			scripts = append(scripts, s)
			want = append(want, oracleStrip(s)...)
		}
		flushSize := rapid.SampledFrom([]int{1, 7, 64, 4096}).Draw(t, "flushSize")
		readBuf := rapid.SampledFrom([]int{1, 13, 512, 4096}).Draw(t, "readBuf")

		sink := &collectingSink{}
		dialer := &scriptedNoiseDialer{fakeCastDialer{scripts: scripts}}
		// Production resumes from global's MAX(t_us), which lags while
		// this pump's frames are still in flight; a fixed zero offset is
		// the extreme of that lag.
		var resume CastResumeSource = &sinkBackedResume{sink: sink, initial: 0, gapUs: 1}
		if rapid.Bool().Draw(t, "staleResume") {
			resume = fixedResume{v: 0}
		}
		pump := NewCastPump(CastPumpConfig{
			MachineID:        uuid.New(),
			Dialer:           dialer,
			Resume:           resume,
			Sink:             sink,
			FlushInterval:    5 * time.Millisecond,
			FlushSize:        flushSize,
			ReadBufSize:      readBuf,
			ReconnectBackoff: time.Millisecond,
			// A session lasting under this counts as a fast fail; keep the
			// backoff small so the run finishes quickly.
			MaxReconnectBackoff: 2 * time.Millisecond,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); pump.Run(ctx) }()

		deadline := time.Now().Add(10 * time.Second)
		for {
			dialer.mu.Lock()
			calls := dialer.calls
			dialer.mu.Unlock()
			if calls > sessions { // every scripted session has ended
				break
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatalf("pump did not consume %d sessions (calls=%d)", sessions, calls)
			}
			time.Sleep(time.Millisecond)
		}
		cancelAt := time.Now()
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("Run did not return within 2s of cancel")
		}
		if d := time.Since(cancelAt); d > time.Second {
			t.Fatalf("Run took %v to stop", d)
		}

		frames := sink.Frames()
		var got []byte
		for i, f := range frames {
			got = append(got, f.Data...)
			if len(f.Data) == 0 {
				t.Fatalf("frame %d is empty", i)
			}
			if i > 0 && f.TUs <= frames[i-1].TUs {
				t.Fatalf("t_us not strictly increasing: frame %d t=%d after frame %d t=%d (of %d frames)",
					i, f.TUs, i-1, frames[i-1].TUs, len(frames))
			}
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("cast bytes differ from model: got %d bytes want %d", len(got), len(want))
		}
	})
}
