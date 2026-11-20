package ws

// Wire round-trip properties for every message type in messages.go.
//
// Values are generated reflectively (every exported field of every message
// struct, recursively into model.SiteSnapshot, model.Operation and
// slogkit.Envelope), marshalled through MarshalEnvelope, parsed with
// ParseEnvelope and decoded the way hub.go / wsconn.go decode them. The
// comparison is field-by-field and independent of encoding/json: it
// reports every path whose value did not survive, so a field that is
// silently dropped on the wire (a `json:"-"` tag, an asymmetric custom
// marshaller, a tag collision) shows up by name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

var (
	rtTimeType = reflect.TypeOf(time.Time{})
	rtUUIDType = reflect.TypeOf(uuid.UUID{})
	rtRawType  = reflect.TypeOf(json.RawMessage{})
	rtIPType   = reflect.TypeOf(net.IP{})
	rtAnyType  = reflect.TypeOf((*any)(nil)).Elem()
)

func rtJSONValue(t *rapid.T, depth int) any {
	n := 5
	if depth <= 0 {
		n = 3
	}
	switch rapid.IntRange(0, n).Draw(t, "jsonKind") {
	case 0:
		return rapid.SampledFrom([]string{"", "x", "héllo", "<script>&", " ", "a\"b\\c", "0", "null"}).Draw(t, "jsonString")
	case 1:
		return float64(rapid.Int32().Draw(t, "jsonNumber"))
	case 2:
		return rapid.Bool().Draw(t, "jsonBool")
	case 3:
		return nil
	case 4:
		k := rapid.IntRange(0, 3).Draw(t, "jsonArrayLen")
		out := make([]any, 0, k)
		for i := 0; i < k; i++ {
			out = append(out, rtJSONValue(t, depth-1))
		}
		return out
	default:
		k := rapid.IntRange(0, 3).Draw(t, "jsonObjectLen")
		out := map[string]any{}
		for i := 0; i < k; i++ {
			out[rapid.SampledFrom([]string{"a", "b", "password", "Outlet", "pdu_runtime", ""}).Draw(t, "jsonKey")] = rtJSONValue(t, depth-1)
		}
		return out
	}
}

// rtFill sets v to a generated value. Slices, maps, pointers and raw
// messages are generated nil, empty and populated, because "absent" and
// "empty" are the places an omitempty tag changes meaning.
func rtFill(t *rapid.T, v reflect.Value, depth int) {
	typ := v.Type()
	switch {
	case typ == rtTimeType:
		sec := rapid.Int64Range(0, 4_000_000_000).Draw(t, "timeSec")
		ns := rapid.Int64Range(0, 999_999_999).Draw(t, "timeNanos")
		tm := time.Unix(sec, ns)
		if rapid.Bool().Draw(t, "timeLocal") {
			tm = tm.In(time.FixedZone("x", 3600*rapid.IntRange(-11, 11).Draw(t, "tz")))
		} else {
			tm = tm.UTC()
		}
		if rapid.IntRange(0, 9).Draw(t, "zeroTime") == 0 {
			tm = time.Time{}
		}
		v.Set(reflect.ValueOf(tm))
		return
	case typ == rtUUIDType:
		var id uuid.UUID
		if rapid.IntRange(0, 4).Draw(t, "nilUUID") != 0 {
			copy(id[:], rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "uuid"))
		}
		v.Set(reflect.ValueOf(id))
		return
	case typ == rtRawType:
		switch rapid.IntRange(0, 3).Draw(t, "rawKind") {
		case 0:
			v.Set(reflect.Zero(typ)) // nil
		default:
			b, err := json.Marshal(rtJSONValue(t, 2))
			if err != nil {
				t.Fatalf("harness: %v", err)
			}
			v.SetBytes(b)
		}
		return
	case typ == rtIPType:
		v.Set(reflect.ValueOf(net.IPv4(10, byte(rapid.IntRange(0, 255).Draw(t, "ip")), 0, 1)))
		return
	case typ == rtAnyType:
		v.Set(reflect.ValueOf(rapid.SampledFrom([]string{"", "v", "ünï"}).Draw(t, "anyString")))
		return
	}
	switch typ.Kind() {
	case reflect.String:
		v.SetString(rapid.OneOf(
			rapid.SampledFrom([]string{"", "a", "site-a", "worker-1", "<&>", "héllo wörld", "line\nbreak", "tab\t", "\u0000nul", "quote\"s"}),
			rapid.StringN(0, 12, 48),
		).Draw(t, "string"))
	case reflect.Bool:
		v.SetBool(rapid.Bool().Draw(t, "bool"))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		lo, hi := int64(math.MinInt64), int64(math.MaxInt64)
		switch typ.Bits() {
		case 8:
			lo, hi = math.MinInt8, math.MaxInt8
		case 16:
			lo, hi = math.MinInt16, math.MaxInt16
		case 32:
			lo, hi = math.MinInt32, math.MaxInt32
		}
		v.SetInt(rapid.OneOf(rapid.SampledFrom([]int64{0, 1, -1, lo, hi}), rapid.Int64Range(lo, hi)).Draw(t, "int"))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		hi := uint64(math.MaxUint64)
		if typ.Bits() < 64 {
			hi = uint64(1)<<typ.Bits() - 1
		}
		v.SetUint(rapid.Uint64Range(0, hi).Draw(t, "uint"))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(rapid.Int32().Draw(t, "float")) / 8)
	case reflect.Slice:
		mode := rapid.IntRange(0, 3).Draw(t, "sliceMode")
		if mode == 0 {
			v.Set(reflect.Zero(typ))
			return
		}
		n := 0
		if mode > 1 && depth > 0 {
			n = rapid.IntRange(1, 2).Draw(t, "sliceLen")
		}
		if typ.Elem().Kind() == reflect.Uint8 {
			// Raw bytes, including bytes that are not valid UTF-8.
			v.SetBytes(rapid.SliceOfN(rapid.Byte(), 0, 24).Draw(t, "bytes"))
			if mode == 1 {
				v.SetBytes([]byte{})
			}
			return
		}
		s := reflect.MakeSlice(typ, n, n)
		for i := 0; i < n; i++ {
			rtFill(t, s.Index(i), depth-1)
		}
		v.Set(s)
	case reflect.Map:
		mode := rapid.IntRange(0, 3).Draw(t, "mapMode")
		if mode == 0 {
			v.Set(reflect.Zero(typ))
			return
		}
		m := reflect.MakeMap(typ)
		if mode > 1 && depth > 0 && typ.Key().Kind() == reflect.String {
			for _, k := range []string{"k1", "00000000-0000-4000-8000-000000000001"} {
				ev := reflect.New(typ.Elem()).Elem()
				rtFill(t, ev, depth-1)
				m.SetMapIndex(reflect.ValueOf(k).Convert(typ.Key()), ev)
			}
		}
		v.Set(m)
	case reflect.Pointer:
		if depth <= 0 || rapid.IntRange(0, 2).Draw(t, "nilPtr") == 0 {
			v.Set(reflect.Zero(typ))
			return
		}
		p := reflect.New(typ.Elem())
		rtFill(t, p.Elem(), depth-1)
		v.Set(p)
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			if !typ.Field(i).IsExported() {
				continue
			}
			rtFill(t, v.Field(i), depth-1)
		}
	case reflect.Array:
		for i := 0; i < v.Len(); i++ {
			rtFill(t, v.Index(i), depth-1)
		}
	case reflect.Interface:
		// left nil
	default:
		t.Fatalf("harness: rtFill does not know kind %s (%s)", typ.Kind(), typ)
	}
}

func rtRawEqual(a, b json.RawMessage) bool {
	norm := func(r json.RawMessage) any {
		if len(r) == 0 {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(r))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return "undecodable:" + string(r)
		}
		return v
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// rtDiff appends "path: detail" for every place want and got differ. An
// absent container and an empty one are the same value on this wire
// (omitempty), so nil and empty slices/maps/raw messages compare equal.
func rtDiff(path string, want, got reflect.Value, out *[]string) {
	typ := want.Type()
	switch {
	case typ == rtTimeType:
		w, g := want.Interface().(time.Time), got.Interface().(time.Time)
		if !w.Equal(g) {
			*out = append(*out, fmt.Sprintf("%s: time %s became %s", path, w.Format(time.RFC3339Nano), g.Format(time.RFC3339Nano)))
		}
		return
	case typ == rtRawType:
		if !rtRawEqual(want.Bytes(), got.Bytes()) {
			*out = append(*out, fmt.Sprintf("%s: raw %s became %s", path, want.Bytes(), got.Bytes()))
		}
		return
	case typ == rtIPType:
		if !want.Interface().(net.IP).Equal(got.Interface().(net.IP)) {
			*out = append(*out, fmt.Sprintf("%s: ip %v became %v", path, want.Interface(), got.Interface()))
		}
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			if !typ.Field(i).IsExported() {
				continue
			}
			rtDiff(path+"."+typ.Field(i).Name, want.Field(i), got.Field(i), out)
		}
	case reflect.Pointer:
		if want.IsNil() != got.IsNil() {
			*out = append(*out, fmt.Sprintf("%s: pointer nil=%v became nil=%v", path, want.IsNil(), got.IsNil()))
			return
		}
		if !want.IsNil() {
			rtDiff(path, want.Elem(), got.Elem(), out)
		}
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			if !bytes.Equal(want.Bytes(), got.Bytes()) {
				*out = append(*out, fmt.Sprintf("%s: bytes %x became %x", path, want.Bytes(), got.Bytes()))
			}
			return
		}
		if want.Len() != got.Len() {
			*out = append(*out, fmt.Sprintf("%s: len %d became %d", path, want.Len(), got.Len()))
			return
		}
		for i := 0; i < want.Len(); i++ {
			rtDiff(fmt.Sprintf("%s[%d]", path, i), want.Index(i), got.Index(i), out)
		}
	case reflect.Map:
		if want.Len() != got.Len() {
			*out = append(*out, fmt.Sprintf("%s: map len %d became %d", path, want.Len(), got.Len()))
			return
		}
		for _, k := range want.MapKeys() {
			gv := got.MapIndex(k)
			if !gv.IsValid() {
				*out = append(*out, fmt.Sprintf("%s[%v]: key lost", path, k))
				continue
			}
			rtDiff(fmt.Sprintf("%s[%v]", path, k), want.MapIndex(k), gv, out)
		}
	case reflect.Interface:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			*out = append(*out, fmt.Sprintf("%s: %#v became %#v", path, want.Interface(), got.Interface()))
		}
	case reflect.Array:
		for i := 0; i < want.Len(); i++ {
			rtDiff(fmt.Sprintf("%s[%d]", path, i), want.Index(i), got.Index(i), out)
		}
	default:
		if !reflect.DeepEqual(want.Interface(), got.Interface()) {
			*out = append(*out, fmt.Sprintf("%s: %#v became %#v", path, want.Interface(), got.Interface()))
		}
	}
}

// rtMessages is every payload type that crosses the worker<->global and
// slagctl<->global WebSockets, keyed by its envelope type constant.
var rtMessages = []struct {
	msgType string
	sample  any
}{
	{TypeHello, HelloMsg{}},
	{TypeHeartbeat, HeartbeatMsg{}},
	{TypeObservation, ObservationMsg{}},
	{TypeOpUpdate, OpUpdateMsg{}},
	{TypeLogBatch, LogBatchMsg{}},
	{TypeClaimOp, ClaimOpMsg{}},
	{TypeCastBatch, CastBatchMsg{}},
	{TypeCastResume, CastResumeRequestMsg{}},
	{TypeCastResumeOK, CastResumeResponseMsg{}},
	{TypeSnapshot, SnapshotMsg{}},
	{TypeNoChange, NoChangeMsg{}},
	{TypeRPCResponse, RPCResponseMsg{}},
	{TypeClaimOpResp, ClaimOpResponse{}},
	{TypeOpDelta, OpDeltaMsg{}},
	{TypeOpResync, OpResyncMsg{}},
	{TypeOpRemoved, OpRemovedMsg{}},
	{TypePowerStatus, PowerStatusRequestMsg{}},
	{TypePowerStatusResp, PowerStatusResponseMsg{}},
	{TypeBootStatus, BootStatusRequestMsg{}},
	{TypeBootStatusResp, BootStatusResponseMsg{}},
	{TypeDHCPLeases, DHCPLeasesRequestMsg{}},
	{TypeDHCPLeasesResp, DHCPLeasesResponseMsg{}},
	{TypeSubscribed, SubscribedMsg{}},
	{TypeLogStreamSubscribe, LogStreamSubscribeMsg{}},
	{TypeLogStreamEnvelope, LogStreamEnvelopeMsg{}},
	{TypeLogStreamMeta, LogStreamMetaMsg{}},
	{TypeError, ErrorMsg{}},
}

// rtKnownLossy lists field paths that are not carried on the wire by
// design. It is empty: every field of every message survives today. A new
// entry needs a comment saying why the field may be dropped.
var rtKnownLossy = []string{}

func rtLossAllowed(path string) bool {
	for _, p := range rtKnownLossy {
		if strings.Contains(path, p) {
			return true
		}
	}
	return false
}

// rtInjectUnknown adds a field no current binary knows to a JSON object.
func rtInjectUnknown(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return raw
	}
	if bytes.Equal(trimmed, []byte("{}")) {
		return []byte(`{"zz_future_field":{"nested":[1,"two",null]}}`)
	}
	return append([]byte(`{"zz_future_field":{"nested":[1,"two",null]},`), trimmed[1:]...)
}

func TestProp_WireRoundTrip(t *testing.T) {
	seen := map[string]int{}
	lossy := map[string]int{}
	var maxBytes int
	rapid.Check(t, func(rt *rapid.T) {
		m := rtMessages[rapid.IntRange(0, len(rtMessages)-1).Draw(rt, "message")]
		typ := reflect.TypeOf(m.sample)
		want := reflect.New(typ).Elem()
		rtFill(rt, want, 5)
		reqID := rapid.SampledFrom([]string{"", "req-1", "a b", "ünï"}).Draw(rt, "requestID")

		wire, err := MarshalEnvelope(m.msgType, reqID, want.Interface())
		if err != nil {
			rt.Fatalf("%s: MarshalEnvelope failed for a value every field of which is in its type's domain: %v\n  value: %+v", m.msgType, err, want.Interface())
		}
		if len(wire) > maxBytes {
			maxBytes = len(wire)
		}
		env, err := ParseEnvelope(wire)
		if err != nil {
			rt.Fatalf("%s: ParseEnvelope: %v", m.msgType, err)
		}
		if env.Type != m.msgType || env.RequestID != reqID {
			rt.Fatalf("envelope header changed: type %q->%q request_id %q->%q", m.msgType, env.Type, reqID, env.RequestID)
		}
		data := []byte(env.Data)
		if rapid.Bool().Draw(rt, "unknownField") {
			data = rtInjectUnknown(data)
		}
		got := reflect.New(typ)
		if err := unmarshalData(data, got.Interface()); err != nil {
			rt.Fatalf("%s: decode: %v\n  wire: %s", m.msgType, err, data)
		}
		var diffs []string
		rtDiff(typ.Name(), want, got.Elem(), &diffs)
		var hard []string
		for _, d := range diffs {
			path := d[:strings.Index(d, ":")]
			if rtLossAllowed(path) {
				lossy[path]++
				continue
			}
			hard = append(hard, d)
		}
		seen[m.msgType]++
		if len(hard) > 0 {
			sort.Strings(hard)
			rt.Fatalf("%s did not survive the wire:\n  %s\n  wire: %s", m.msgType, strings.Join(hard, "\n  "), wire)
		}
		// Idempotence: re-encoding the decoded value gives the same bytes.
		again, err := MarshalEnvelope(m.msgType, reqID, got.Elem().Interface())
		if err != nil {
			rt.Fatalf("%s: re-marshal: %v", m.msgType, err)
		}
		if !rapid.Bool().Draw(rt, "skipIdem") && !bytes.Equal(again, wire) && !bytes.Contains(data, []byte("zz_future_field")) {
			env2, _ := ParseEnvelope(again)
			if !rtRawEqual(env.Data, env2.Data) {
				rt.Fatalf("%s: encoding is not stable across a round trip:\n  first:  %s\n  second: %s", m.msgType, wire, again)
			}
		}
	})
	missing := []string{}
	for _, m := range rtMessages {
		if seen[m.msgType] == 0 {
			missing = append(missing, m.msgType)
		}
	}
	t.Logf("coverage: %d message types exercised, never drawn: %v, largest wire message %d bytes, known-lossy paths hit: %v",
		len(seen), missing, maxBytes, lossy)
}

// TestProp_ParseEnvelopeNeverPanics feeds arbitrary bytes and near-valid
// envelopes to the parser and to each payload decoder.
func TestProp_ParseEnvelopeNeverPanics(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		var raw []byte
		if rapid.Bool().Draw(rt, "structured") {
			b, _ := json.Marshal(map[string]any{
				"type":       rapid.SampledFrom([]any{"hello", "claim_op", 7, nil, []any{}, ""}).Draw(rt, "type"),
				"request_id": rapid.SampledFrom([]any{"r", 1, nil, map[string]any{}}).Draw(rt, "reqid"),
				"data":       rtJSONValue(rt, 3),
			})
			raw = b
			if rapid.Bool().Draw(rt, "truncate") && len(raw) > 0 {
				raw = raw[:rapid.IntRange(0, len(raw)-1).Draw(rt, "cut")]
			}
		} else {
			raw = rapid.SliceOfN(rapid.Byte(), 0, 64).Draw(rt, "bytes")
		}
		env, err := ParseEnvelope(raw)
		if err != nil {
			return
		}
		for _, m := range rtMessages {
			_ = unmarshalData(env.Data, reflect.New(reflect.TypeOf(m.sample)).Interface())
		}
	})
}

// TestProp_ParseNotification pins the NOTIFY payload grammar documented on
// ParseNotification: "table:id" and "table:deleted:id".
func TestProp_ParseNotification(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		table := rapid.SampledFrom([]string{"machines", "ssh_keys", "site_configs", "operations", "images"}).Draw(rt, "table")
		id := rapid.OneOf(
			rapid.SampledFrom([]string{"6f1c2a52-0b0e-4a3b-9c55-0d0d7c1f2a11", "site-a", "42", "deleted"}),
			rapid.StringMatching(`[a-z0-9-]{1,12}`),
		).Draw(rt, "id")
		deleted := rapid.Bool().Draw(rt, "deleted")
		payload := table + ":" + id
		if deleted {
			payload = table + ":deleted:" + id
		}
		got := ParseNotification(payload)
		if got.Table != table || got.ID != id || got.Deleted != deleted {
			rt.Fatalf("ParseNotification(%q) = %+v, want table=%q id=%q deleted=%v", payload, got, table, id, deleted)
		}
	})
}
