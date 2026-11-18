// Package slogkit defines the JSON log envelope used across slagworks
// and a slog.Handler that emits envelopes to stderr and pushes them
// into a Sink (an in-process forwarding ring on site/global, or an
// HTTP-batching client on the agent).
//
// Every binary in slagworks (slagworksd global, slagworksd site,
// slagworks-agent) wires this handler into slog.Default at startup.
// One envelope shape, one trace_id flowing across boundaries.
package slogkit

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Source identifies the binary that emitted a log envelope.
type Source string

const (
	SourceGlobal Source = "global"
	SourceSite   Source = "site"
	SourceAgent  Source = "agent"
)

// Level is the canonical level string used in envelopes. Maps 1:1 to
// slog.Level values; we store strings on the wire for debuggability.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Envelope is the JSON record shape every log line in slagworks
// renders as. Field semantics are documented in the structured-logs
// plan; see the table under "Field semantics."
type Envelope struct {
	TS    time.Time `json:"ts"`
	Level Level     `json:"level"`

	Source Source `json:"source"`
	Host   string `json:"host,omitempty"`

	Msg string `json:"msg"`

	MachineID   uuid.UUID `json:"machine_id,omitempty"`
	MachineName string    `json:"machine_name,omitempty"`
	OpID        uuid.UUID `json:"op_id,omitempty"`
	Phase       string    `json:"phase,omitempty"`
	TraceID     uuid.UUID `json:"trace_id,omitempty"`

	// ReceivedAt is stamped by the site worker when it receives an
	// envelope from the agent or its own SOL pump, before forwarding
	// to global. Lets operators see clock skew between agents and
	// the rest of the fleet.
	ReceivedAt *time.Time `json:"received_at,omitempty"`

	// Extra is free-form structured fields pulled from slog attrs
	// that aren't first-class envelope columns. Marshaled inline
	// under "extra"; nil-or-empty omitted.
	Extra map[string]any `json:"extra,omitempty"`
}

// MarshalJSON emits the envelope with stable field ordering and
// without the json/Time-default zero-value handling that would
// otherwise serialize the zero TraceID/MachineID/OpID as
// "00000000-0000-0000-0000-000000000000". We want them omitted.
func (e Envelope) MarshalJSON() ([]byte, error) {
	out := struct {
		TS         string         `json:"ts"`
		Level      Level          `json:"level"`
		Source     Source         `json:"source"`
		Host       string         `json:"host,omitempty"`
		Msg        string         `json:"msg"`
		MachineID  string         `json:"machine_id,omitempty"`
		MachineNm  string         `json:"machine_name,omitempty"`
		OpID       string         `json:"op_id,omitempty"`
		Phase      string         `json:"phase,omitempty"`
		TraceID    string         `json:"trace_id,omitempty"`
		ReceivedAt string         `json:"received_at,omitempty"`
		Extra      map[string]any `json:"extra,omitempty"`
	}{
		TS:        e.TS.UTC().Format(time.RFC3339Nano),
		Level:     e.Level,
		Source:    e.Source,
		Host:      e.Host,
		Msg:       e.Msg,
		MachineNm: e.MachineName,
		Phase:     e.Phase,
		Extra:     e.Extra,
	}
	if e.MachineID != uuid.Nil {
		out.MachineID = e.MachineID.String()
	}
	if e.OpID != uuid.Nil {
		out.OpID = e.OpID.String()
	}
	if e.TraceID != uuid.Nil {
		out.TraceID = e.TraceID.String()
	}
	if e.ReceivedAt != nil {
		out.ReceivedAt = e.ReceivedAt.UTC().Format(time.RFC3339Nano)
	}
	return json.Marshal(out)
}

// StringExtra fetches a string field from Extra, returning "" if missing or
// not a string. Convenience for the common case of pulling a structured attr
// (e.g. an exec envelope's "argv") back out at render time.
func (e Envelope) StringExtra(key string) string {
	if e.Extra == nil {
		return ""
	}
	if v, ok := e.Extra[key].(string); ok {
		return v
	}
	return ""
}

// IntExtra fetches an integer field from Extra. JSON numbers decode as float64
// over the wire, so that's the common case; int/int64 are accepted too for
// in-process callers. ok is false if the key is missing or non-numeric.
func (e Envelope) IntExtra(key string) (int64, bool) {
	if e.Extra == nil {
		return 0, false
	}
	switch v := e.Extra[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}
