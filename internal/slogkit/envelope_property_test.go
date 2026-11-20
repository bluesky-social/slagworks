package slogkit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// Round-trip and hoisting properties for the log envelope.
//
//   - Marshal -> Unmarshal -> Marshal is a fixed point, and the decoded
//     envelope equals the original on every first-class field (times
//     compared with Equal; Extra compared after a JSON normalisation,
//     since numbers decode as float64 by design).
//   - Handler hoisting: for a record built from generated attrs (some
//     first-class keys, some extras, some via WithAttrs), the envelope
//     carries the LAST value given for each key (record attrs after
//     handler attrs), first-class keys never leak into Extra, and the
//     pushed envelope equals the stderr line.

func genUUID(t *rapid.T, label string) uuid.UUID {
	if rapid.Bool().Draw(t, label+"Nil") {
		return uuid.Nil
	}
	var u uuid.UUID
	copy(u[:], rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, label))
	return u
}

func genTime(t *rapid.T, label string) time.Time {
	sec := rapid.Int64Range(-62135596800, 253402300799).Draw(t, label+"Sec") // years 1..9999
	nsec := rapid.Int64Range(0, 999_999_999).Draw(t, label+"Nsec")
	off := rapid.IntRange(-14*3600, 14*3600).Draw(t, label+"Off")
	return time.Unix(sec, nsec).In(time.FixedZone("z", off))
}

func genEnvelope(t *rapid.T) Envelope {
	e := Envelope{
		TS:          genTime(t, "ts"),
		Level:       rapid.SampledFrom([]Level{LevelDebug, LevelInfo, LevelWarn, LevelError}).Draw(t, "level"),
		Source:      rapid.SampledFrom([]Source{SourceAgent, SourceSite, SourceGlobal}).Draw(t, "source"),
		Host:        rapid.String().Draw(t, "host"),
		Msg:         rapid.String().Draw(t, "msg"),
		MachineID:   genUUID(t, "machine"),
		MachineName: rapid.String().Draw(t, "mname"),
		OpID:        genUUID(t, "op"),
		Phase:       rapid.String().Draw(t, "phase"),
		TraceID:     genUUID(t, "trace"),
	}
	if rapid.Bool().Draw(t, "hasRecv") {
		r := genTime(t, "recv")
		e.ReceivedAt = &r
	}
	if rapid.Bool().Draw(t, "hasExtra") {
		e.Extra = map[string]any{}
		for i, n := 0, rapid.IntRange(1, 4).Draw(t, "nExtra"); i < n; i++ {
			k := rapid.StringN(1, 6, -1).Draw(t, "k")
			switch rapid.IntRange(0, 3).Draw(t, "kind") {
			case 0:
				e.Extra[k] = rapid.String().Draw(t, "sv")
			case 1:
				e.Extra[k] = float64(rapid.IntRange(-1<<40, 1<<40).Draw(t, "nv"))
			case 2:
				e.Extra[k] = rapid.Bool().Draw(t, "bv")
			default:
				e.Extra[k] = map[string]any{"inner": rapid.String().Draw(t, "inner")}
			}
		}
	}
	return e
}

func normJSON(t *rapid.T, v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

// viaJSON mirrors what encoding/json does to strings: invalid UTF-8 is
// replaced with U+FFFD on marshal. Strings are compared after that.
func viaJSON(s string) string {
	b, _ := json.Marshal(s)
	var out string
	_ = json.Unmarshal(b, &out)
	return out
}

func TestEnvelope_Property_JSONRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		e := genEnvelope(t)
		b1, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var d Envelope
		if err := json.Unmarshal(b1, &d); err != nil {
			t.Fatalf("unmarshal %s: %v", b1, err)
		}
		b2, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if string(b1) != string(b2) {
			t.Fatalf("not a fixed point:\n %s\n %s", b1, b2)
		}
		if !d.TS.Equal(e.TS) {
			t.Fatalf("ts %v != %v", d.TS, e.TS)
		}
		if (d.ReceivedAt == nil) != (e.ReceivedAt == nil) || (d.ReceivedAt != nil && !d.ReceivedAt.Equal(*e.ReceivedAt)) {
			t.Fatalf("received_at %v != %v", d.ReceivedAt, e.ReceivedAt)
		}
		if d.Level != e.Level || d.Source != e.Source || d.MachineID != e.MachineID || d.OpID != e.OpID || d.TraceID != e.TraceID {
			t.Fatalf("ids/level/source differ: %+v vs %+v", d, e)
		}
		if d.Host != viaJSON(e.Host) || d.Msg != viaJSON(e.Msg) || d.MachineName != viaJSON(e.MachineName) || d.Phase != viaJSON(e.Phase) {
			t.Fatalf("strings differ: %+v vs %+v", d, e)
		}
		if !reflect.DeepEqual(normJSON(t, d.Extra), normJSON(t, e.Extra)) {
			t.Fatalf("extra %v != %v", d.Extra, e.Extra)
		}
	})
}

type propSink struct{ envs []Envelope }

func (c *propSink) Push(e Envelope) { c.envs = append(c.envs, e) }

type lineBuf struct{ lines [][]byte }

func (l *lineBuf) Write(p []byte) (int, error) {
	l.lines = append(l.lines, append([]byte(nil), p...))
	return len(p), nil
}

func TestHandler_Property_LastValueWinsAndNoLeak(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		keys := []string{"trace_id", "machine_id", "op_id", "machine_name", "phase", "x", "y"}
		ids := []uuid.UUID{uuid.New(), uuid.New()}
		genAttr := func(label string) slog.Attr {
			k := rapid.SampledFrom(keys).Draw(t, label+"Key")
			switch k {
			case "trace_id", "machine_id", "op_id":
				id := rapid.SampledFrom(ids).Draw(t, label+"ID")
				if rapid.Bool().Draw(t, label+"AsString") {
					return slog.String(k, id.String())
				}
				return slog.Any(k, id)
			default:
				return slog.String(k, rapid.StringN(0, 5, -1).Draw(t, label+"Val"))
			}
		}
		var hattrs, rattrs []slog.Attr
		for i, n := 0, rapid.IntRange(0, 5).Draw(t, "nH"); i < n; i++ {
			hattrs = append(hattrs, genAttr("h"))
		}
		for i, n := 0, rapid.IntRange(0, 5).Draw(t, "nR"); i < n; i++ {
			rattrs = append(rattrs, genAttr("r"))
		}
		// Model: last value per key, handler attrs first.
		last := map[string]slog.Value{}
		for _, a := range append(append([]slog.Attr(nil), hattrs...), rattrs...) {
			last[a.Key] = a.Value
		}

		sink := &propSink{}
		out := &lineBuf{}
		var h slog.Handler = NewHandler(HandlerOptions{Source: SourceSite, Sink: sink, Stderr: out, Level: slog.LevelDebug})
		h = h.WithAttrs(hattrs)
		rec := slog.NewRecord(time.Unix(1700000000, 0), slog.LevelInfo, "m", 0)
		rec.AddAttrs(rattrs...)
		if err := h.Handle(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		if len(sink.envs) != 1 || len(out.lines) != 1 {
			t.Fatalf("pushed %d, wrote %d", len(sink.envs), len(out.lines))
		}
		env := sink.envs[0]
		wantStr := func(k string) string {
			if v, ok := last[k]; ok {
				if id, isID := v.Any().(uuid.UUID); isID {
					return id.String()
				}
				return v.String()
			}
			return ""
		}
		check := func(k, got string) {
			if w := wantStr(k); w != got {
				t.Fatalf("%s: got %q want %q (h=%v r=%v)", k, got, w, hattrs, rattrs)
			}
		}
		idStr := func(u uuid.UUID) string {
			if u == uuid.Nil {
				return ""
			}
			return u.String()
		}
		check("trace_id", idStr(env.TraceID))
		check("machine_id", idStr(env.MachineID))
		check("op_id", idStr(env.OpID))
		check("machine_name", env.MachineName)
		check("phase", env.Phase)
		for _, k := range []string{"x", "y"} {
			v, ok := env.Extra[k]
			_, want := last[k]
			if ok != want || (ok && fmt.Sprint(v) != wantStr(k)) {
				t.Fatalf("extra %s: got %v,%v want %q", k, v, ok, wantStr(k))
			}
		}
		for _, k := range []string{"trace_id", "machine_id", "op_id", "machine_name", "phase"} {
			if _, leaked := env.Extra[k]; leaked {
				t.Fatalf("first-class key %s leaked into extra", k)
			}
		}
		want, _ := json.Marshal(env)
		if string(append(want, '\n')) != string(out.lines[0]) {
			t.Fatalf("stderr line differs from pushed envelope:\n %s\n %s", out.lines[0], want)
		}
	})
}
