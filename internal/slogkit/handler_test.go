package slogkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type captureSink struct {
	mu  sync.Mutex
	got []Envelope
}

func (s *captureSink) Push(e Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, e)
}

func (s *captureSink) take() []Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.got
	s.got = nil
	return out
}

func newTestHandler(t *testing.T) (*Handler, *bytes.Buffer, *captureSink) {
	t.Helper()
	stderr := &bytes.Buffer{}
	sink := &captureSink{}
	h := NewHandler(HandlerOptions{
		Source: SourceSite,
		Host:   "test-host",
		Level:  slog.LevelDebug,
		Stderr: stderr,
		Sink:   sink,
		Now:    func() time.Time { return time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC) },
	})
	return h, stderr, sink
}

func TestEnvelopeJSONOmitsZeroUUIDs(t *testing.T) {
	e := Envelope{
		TS:     time.Date(2026, 5, 24, 22, 30, 0, 123456789, time.UTC),
		Level:  LevelInfo,
		Source: SourceSite,
		Host:   "oob-gw-1-pop1",
		Msg:    "hello",
	}
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(buf)
	for _, banned := range []string{"machine_id", "op_id", "trace_id", "00000000-0000"} {
		if strings.Contains(got, banned) {
			t.Errorf("envelope JSON unexpectedly contains %q: %s", banned, got)
		}
	}
	if !strings.Contains(got, `"ts":"2026-05-24T22:30:00.123456789Z"`) {
		t.Errorf("ts not RFC3339Nano: %s", got)
	}
}

func TestEnvelopeJSONIncludesPopulatedFields(t *testing.T) {
	mid := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	tid := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	oid := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	rcv := time.Date(2026, 5, 24, 22, 30, 1, 0, time.UTC)
	e := Envelope{
		TS:          time.Date(2026, 5, 24, 22, 30, 0, 0, time.UTC),
		Level:       LevelWarn,
		Source:      SourceAgent,
		Msg:         "phase",
		MachineID:   mid,
		MachineName: "kworker-l34-1",
		OpID:        oid,
		Phase:       "rsyncing_os",
		TraceID:     tid,
		ReceivedAt:  &rcv,
		Extra:       map[string]any{"duration_ms": 3120.0},
	}
	buf, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(buf, &round); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	checks := map[string]any{
		"machine_id":   mid.String(),
		"machine_name": "kworker-l34-1",
		"op_id":        oid.String(),
		"trace_id":     tid.String(),
		"phase":        "rsyncing_os",
		"received_at":  "2026-05-24T22:30:01Z",
		"level":        "warn",
		"source":       "agent",
	}
	for k, want := range checks {
		if got := round[k]; got != want {
			t.Errorf("field %q: got %v, want %v", k, got, want)
		}
	}
	extra, ok := round["extra"].(map[string]any)
	if !ok || extra["duration_ms"] != 3120.0 {
		t.Errorf("extra missing or wrong: %v", round["extra"])
	}
}

func TestHandlerEmitsToStderrAndSink(t *testing.T) {
	h, stderr, sink := newTestHandler(t)
	logger := slog.New(h)
	logger.Info("hello world", "machine_id", "11111111-1111-1111-1111-111111111111", "phase", "deploy", "extra_key", 42)
	got := sink.take()
	if len(got) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(got))
	}
	env := got[0]
	if env.Msg != "hello world" {
		t.Errorf("msg = %q", env.Msg)
	}
	if env.Source != SourceSite {
		t.Errorf("source = %q", env.Source)
	}
	if env.Host != "test-host" {
		t.Errorf("host = %q", env.Host)
	}
	if env.Phase != "deploy" {
		t.Errorf("phase = %q", env.Phase)
	}
	if env.MachineID.String() != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("machine_id = %v", env.MachineID)
	}
	if env.Extra["extra_key"] != int64(42) {
		t.Errorf("extra.extra_key = %v (%T)", env.Extra["extra_key"], env.Extra["extra_key"])
	}
	// stderr should be one JSON line ending in \n.
	out := stderr.String()
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("stderr not single JSON line: %q", out)
	}
	var round map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSuffix(out, "\n")), &round); err != nil {
		t.Errorf("stderr not valid JSON: %v\n%s", err, out)
	}
}

func TestHandlerWithAttrsPropagates(t *testing.T) {
	h, _, sink := newTestHandler(t)
	tid := uuid.New()
	logger := slog.New(h).With("trace_id", tid.String(), "op_id", uuid.New().String())
	logger.Info("first")
	logger.With("phase", "partitioning").Info("second")
	got := sink.take()
	if len(got) != 2 {
		t.Fatalf("want 2 envelopes, got %d", len(got))
	}
	if got[0].TraceID != tid {
		t.Errorf("first envelope missing trace_id: %v", got[0])
	}
	if got[1].TraceID != tid {
		t.Errorf("second envelope missing trace_id: %v", got[1])
	}
	if got[1].Phase != "partitioning" {
		t.Errorf("phase not propagated through nested With: %q", got[1].Phase)
	}
	if got[0].Phase != "" {
		t.Errorf("phase leaked from sibling logger: %q", got[0].Phase)
	}
}

func TestHandlerLevelFilter(t *testing.T) {
	h, _, sink := newTestHandler(t)
	h.opts.Level = slog.LevelWarn
	logger := slog.New(h)
	logger.Debug("nope")
	logger.Info("nope")
	logger.Warn("yes")
	logger.Error("yes")
	got := sink.take()
	if len(got) != 2 {
		t.Fatalf("want 2 envelopes, got %d: %+v", len(got), got)
	}
	if got[0].Level != LevelWarn || got[1].Level != LevelError {
		t.Errorf("level mismatch: %+v", got)
	}
}

func TestHandlerGroupFlattensIntoExtra(t *testing.T) {
	h, _, sink := newTestHandler(t)
	logger := slog.New(h)
	logger.Info("with group", slog.Group("net", slog.String("iface", "eno3"), slog.Int("vlan", 100)))
	got := sink.take()
	if len(got) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(got))
	}
	netGroup, ok := got[0].Extra["net"].(map[string]any)
	if !ok {
		t.Fatalf("net group missing or wrong type: %v", got[0].Extra["net"])
	}
	if netGroup["iface"] != "eno3" {
		t.Errorf("iface = %v", netGroup["iface"])
	}
	if netGroup["vlan"] != int64(100) {
		t.Errorf("vlan = %v (%T)", netGroup["vlan"], netGroup["vlan"])
	}
}

func TestHandlerErrorAttrRendersString(t *testing.T) {
	h, _, sink := newTestHandler(t)
	logger := slog.New(h)
	// errors.New produces *errors.errorString — unexported field;
	// json.Marshal of the bare error interface gives "{}". The
	// handler must coerce to err.Error() so operators see what
	// actually broke.
	logger.Warn("boom", "err", fmt.Errorf("kaboom: %s", "underlying"))
	got := sink.take()
	if len(got) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(got))
	}
	v, ok := got[0].Extra["err"]
	if !ok {
		t.Fatalf("extra.err missing: %v", got[0].Extra)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("extra.err is %T, want string: %v", v, v)
	}
	if s != "kaboom: underlying" {
		t.Errorf("extra.err = %q", s)
	}
}

func TestHandlerNopSinkNoPanic(t *testing.T) {
	h := NewHandler(HandlerOptions{Source: SourceGlobal, Stderr: &bytes.Buffer{}, Now: time.Now})
	logger := slog.New(h)
	logger.Info("default sink") // exercises NopSink
}

func TestHandlerEnabled(t *testing.T) {
	h, _, _ := newTestHandler(t)
	if !h.Enabled(context.Background(), slog.LevelDebug) {
		t.Errorf("debug should be enabled at level=Debug")
	}
	h.opts.Level = slog.LevelError
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Errorf("info should be disabled at level=Error")
	}
}

func TestHandlerInvalidUUIDFallsToNil(t *testing.T) {
	h, _, sink := newTestHandler(t)
	logger := slog.New(h)
	logger.Info("bad uuid", "trace_id", "not-a-uuid")
	got := sink.take()
	if len(got) != 1 {
		t.Fatalf("want 1 envelope, got %d", len(got))
	}
	if got[0].TraceID != uuid.Nil {
		t.Errorf("expected nil trace_id for invalid input, got %v", got[0].TraceID)
	}
	// Invalid UUID string should NOT land in extra (it consumed the
	// known key); silent drop is acceptable here.
}

func TestHandlerUUIDValueAsAny(t *testing.T) {
	h, _, sink := newTestHandler(t)
	logger := slog.New(h)
	tid := uuid.New()
	logger.Info("uuid attr", slog.Any("trace_id", tid))
	got := sink.take()
	if got[0].TraceID != tid {
		t.Errorf("trace_id not extracted from slog.Any: got %v want %v", got[0].TraceID, tid)
	}
}

func TestHandlerStderrSerialized(t *testing.T) {
	h, stderr, _ := newTestHandler(t)
	logger := slog.New(h)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			logger.Info("concurrent", "i", i)
		}(i)
	}
	wg.Wait()
	// Each line must parse as JSON; an interleaving bug would break this.
	for line := range strings.SplitSeq(strings.TrimRight(stderr.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var any map[string]any
		if err := json.Unmarshal([]byte(line), &any); err != nil {
			t.Errorf("stderr line not valid JSON (interleaving?): %v\n%s", err, line)
		}
	}
}
