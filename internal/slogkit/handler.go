package slogkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sink receives envelopes for forwarding to global. On site and global
// workers, the implementation is an in-process ring; on the agent it's
// an HTTP-batching client; in tests it's a slice-collector. Push must
// be non-blocking (drop-on-full is the implementation's policy, not
// the handler's concern).
type Sink interface {
	Push(Envelope)
}

// nopSink discards envelopes. Used when the binary doesn't have a
// forwarding pipeline yet (e.g., a CLI tool that doesn't ship logs to
// global).
type nopSink struct{}

func (nopSink) Push(Envelope) {}

// NopSink returns a Sink that drops everything. Useful for short-lived
// CLI binaries where stderr emit is the only sink that matters.
func NopSink() Sink { return nopSink{} }

// HandlerOptions configures a handler instance.
type HandlerOptions struct {
	// Source identifies the binary; baked into every envelope.
	Source Source

	// Host is the hostname / pod name; baked into every envelope.
	// Empty for agent and sol (the receiver fills those in).
	Host string

	// Level is the minimum slog level emitted. Defaults to
	// slog.LevelInfo when zero.
	Level slog.Leveler

	// Stderr is where each envelope is written as one JSON line.
	// nil disables stderr emission entirely (test usage).
	Stderr io.Writer

	// Sink receives every envelope after it's emitted to stderr.
	// nil uses NopSink.
	Sink Sink

	// Now is overridable for tests. Defaults to time.Now.
	Now func() time.Time
}

// Handler is the slog.Handler that renders Envelopes.
type Handler struct {
	opts HandlerOptions

	// attrs collected via WithAttrs, in addition to the record's own
	// attrs. Stored as a list of slog.Attr so we can resolve them
	// the same way as record attrs.
	attrs []slog.Attr

	// stderrMu serializes writes to stderr so concurrent goroutines
	// don't interleave bytes from separate envelopes.
	stderrMu *sync.Mutex
}

// NewHandler returns a Handler with the given options. Sane defaults
// are filled in.
func NewHandler(opts HandlerOptions) *Handler {
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}
	if opts.Sink == nil {
		opts.Sink = NopSink()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{
		opts:     opts,
		stderrMu: &sync.Mutex{},
	}
}

// Enabled reports whether the handler emits records at the given level.
func (h *Handler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= h.opts.Level.Level()
}

// Handle renders one slog.Record into an Envelope, writes it to stderr
// (one JSON line), and pushes it to the configured Sink.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	env := Envelope{
		TS:     r.Time,
		Level:  levelString(r.Level),
		Source: h.opts.Source,
		Host:   h.opts.Host,
		Msg:    r.Message,
	}
	if env.TS.IsZero() {
		env.TS = h.opts.Now()
	}
	extra := map[string]any{}
	apply := func(a slog.Attr) {
		if a.Equal(slog.Attr{}) {
			return
		}
		hoist(&env, extra, a)
	}
	for _, a := range h.attrs {
		apply(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		apply(a)
		return true
	})
	if len(extra) > 0 {
		env.Extra = extra
	}

	if h.opts.Stderr != nil {
		buf, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("slogkit marshal: %w", err)
		}
		buf = append(buf, '\n')
		h.stderrMu.Lock()
		_, _ = h.opts.Stderr.Write(buf)
		h.stderrMu.Unlock()
	}
	h.opts.Sink.Push(env)
	return nil
}

// WithAttrs returns a child handler that includes the given attrs on
// every subsequent record. The parent's attrs and stderr mutex are
// shared so concurrent emit on parent and child stays serialized.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	cloned := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	cloned = append(cloned, h.attrs...)
	cloned = append(cloned, attrs...)
	return &Handler{
		opts:     h.opts,
		attrs:    cloned,
		stderrMu: h.stderrMu,
	}
}

// WithGroup is required by slog.Handler. We ignore groups: every attr
// is hoisted into either a top-level envelope field or the extra map,
// and group-prefixed keys would just clutter the Loki/Grafana
// experience without buying anything. Groups still appear if a caller
// passes a slog.Group attr value; hoist() unpacks them flat.
func (h *Handler) WithGroup(_ string) slog.Handler {
	return h
}

// hoist places an attr into either a known top-level envelope field
// or the extra map. Recurses into slog.GroupValue so a caller passing
// `slog.Group("op", slog.String("phase", ...))` flattens to
// `extra.op.phase` rather than swallowing the group entirely.
func hoist(env *Envelope, extra map[string]any, a slog.Attr) {
	v := a.Value.Resolve()
	switch a.Key {
	case "trace_id":
		env.TraceID = parseUUID(v)
	case "machine_id":
		env.MachineID = parseUUID(v)
	case "op_id":
		env.OpID = parseUUID(v)
	case "machine_name":
		if s, ok := stringValue(v); ok {
			env.MachineName = s
		}
	case "phase":
		if s, ok := stringValue(v); ok {
			env.Phase = s
		}
	case "received_at":
		if t, ok := timeValue(v); ok {
			tCopy := t
			env.ReceivedAt = &tCopy
		}
	default:
		if v.Kind() == slog.KindGroup {
			grp := map[string]any{}
			for _, sub := range v.Group() {
				gv := sub.Value.Resolve()
				grp[sub.Key] = renderAttrValue(gv)
			}
			extra[a.Key] = grp
			return
		}
		extra[a.Key] = renderAttrValue(v)
	}
}

// renderAttrValue coerces a slog.Value into something JSON-friendly
// for the Extra map. The default of `v.Any()` is fine for primitives
// and structs with exported fields, but loses information for two
// shapes that show up regularly in our logs:
//
//   - error: the `error` interface value typically holds a struct
//     with unexported fields (e.g. *errors.errorString), which JSON
//     marshals as `{}`. Rendering as `Error()` keeps the message.
//   - fmt.Stringer: same hazard for any concrete type whose useful
//     content lives behind String() rather than exported fields.
//
// We deliberately don't try harder than this — full reflective
// JSON-friendliness is a rabbit hole we don't need.
func renderAttrValue(v slog.Value) any {
	a := v.Any()
	if err, ok := a.(error); ok && err != nil {
		return err.Error()
	}
	return a
}

func parseUUID(v slog.Value) uuid.UUID {
	switch v.Kind() {
	case slog.KindString:
		id, err := uuid.Parse(v.String())
		if err != nil {
			return uuid.Nil
		}
		return id
	case slog.KindAny:
		if id, ok := v.Any().(uuid.UUID); ok {
			return id
		}
		return uuid.Nil
	case slog.KindBool, slog.KindDuration, slog.KindFloat64,
		slog.KindInt64, slog.KindTime, slog.KindUint64,
		slog.KindGroup, slog.KindLogValuer:
		return uuid.Nil
	}
	return uuid.Nil
}

func stringValue(v slog.Value) (string, bool) {
	if v.Kind() == slog.KindString {
		return v.String(), true
	}
	return "", false
}

func timeValue(v slog.Value) (time.Time, bool) {
	if v.Kind() == slog.KindTime {
		return v.Time(), true
	}
	return time.Time{}, false
}

func levelString(l slog.Level) Level {
	switch {
	case l <= slog.LevelDebug:
		return LevelDebug
	case l <= slog.LevelInfo:
		return LevelInfo
	case l <= slog.LevelWarn:
		return LevelWarn
	default:
		return LevelError
	}
}
