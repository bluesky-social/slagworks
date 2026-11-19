package ws

// Properties of the hello handshake gates (protocol-version skew, worker
// identity, hardware-serial enforcement) and of the hub's tolerance for
// malformed traffic after a good hello.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

type helloGateResolver struct {
	verdict map[string]error
	mu      sync.Mutex
	asked   []string
}

func (r *helloGateResolver) Resolve(_ context.Context, slug, serial string) error {
	r.mu.Lock()
	r.asked = append(r.asked, slug+"/"+serial)
	r.mu.Unlock()
	return r.verdict[serial]
}

var helloGateStats struct {
	cases, accepted, protoRefused, idRefused, serialMissing, serialMismatch, notHello, badPayload atomic.Int64
}

// TestProp_HelloGate: for a generated first message, the hub either
// completes the handshake (reply carrying Proto, then op_resync, the worker
// registered and its serial persisted) or refuses it loudly with the
// documented error code, never registers the connection, and never
// persists a serial for a refused worker.
func TestProp_HelloGate(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		helloGateStats.cases.Add(1)
		enforce := rapid.Bool().Draw(rt, "serialEnforced")
		// Each gate is reached only if the earlier ones pass, so the early
		// dimensions are heavily biased toward their passing value.
		firstType := TypeHello
		if rapid.IntRange(0, 11).Draw(rt, "wrongFirstType") == 0 {
			firstType = rapid.SampledFrom([]string{TypeHeartbeat, TypeClaimOp, TypeObservation, "nonsense", ""}).Draw(rt, "firstType")
		}
		payloadMode := "object"
		if rapid.IntRange(0, 11).Draw(rt, "oddPayload") == 0 {
			payloadMode = rapid.SampledFrom([]string{"string", "array", "null", "absent"}).Draw(rt, "payload")
		}
		workerID := "worker-1"
		if rapid.IntRange(0, 9).Draw(rt, "wrongWorkerID") == 0 {
			workerID = rapid.SampledFrom([]string{"worker-2", "", "WORKER-1", "worker-1 "}).Draw(rt, "helloWorkerID")
		}
		protoMode := rapid.SampledFrom([]string{"current", "current", "current", "current", "higher", "absent", "zero", "negative", "lower"}).Draw(rt, "proto")
		serial := rapid.SampledFrom([]string{"", "GOOD1", "GOOD1", "GOOD2", "UNKNOWN", "AMBIG", "FOREIGN", "LOOKUPFAIL"}).Draw(rt, "serial")
		lastSeq := rapid.SampledFrom([]int64{0, 1, 7, -1}).Draw(rt, "lastSeq")

		resolver := &helloGateResolver{verdict: map[string]error{
			"UNKNOWN":    ErrSerialUnknown,
			"AMBIG":      ErrSerialAmbiguous,
			"FOREIGN":    ErrSerialOutOfScope,
			"LOOKUPFAIL": errors.New("netbox: connection refused"),
		}}
		auth := newFakeAuth()
		auth.register("tok-1", "site-a", "worker-1")
		cfg := HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{snap: model.SiteSnapshot{SeqNum: 7}}, Logger: propDiscardLogger()}
		if enforce {
			cfg.SerialResolver = resolver
		}
		hub := NewHub(cfg)
		srv := httptest.NewServer(hub)
		defer srv.Close()
		conn, err := propDial(srv, "tok-1")
		if err != nil {
			rt.Fatalf("harness: dial: %v", err)
		}
		defer conn.CloseNow()

		// Assemble the first message as raw JSON so "absent" really is absent.
		fields := map[string]any{"worker_id": workerID, "last_seq_num": lastSeq, "fqdn": "w1.example.test"}
		proto := OpStreamProto
		switch protoMode {
		case "absent":
			proto = 0
		case "zero":
			proto = 0
			fields["proto"] = 0
		case "negative":
			proto = -3
			fields["proto"] = proto
		case "lower":
			proto = OpStreamProto - 1
			fields["proto"] = proto
		case "higher":
			proto = OpStreamProto + 2
			fields["proto"] = proto
		default:
			fields["proto"] = proto
		}
		if serial != "" {
			fields["serial"] = serial
		}
		var data json.RawMessage
		payloadDecodes := true
		payloadEmpty := false
		switch payloadMode {
		case "object":
			data, _ = json.Marshal(fields)
		case "string":
			data = json.RawMessage(`"hello"`)
			payloadDecodes = false
		case "array":
			data = json.RawMessage(`[1,2]`)
			payloadDecodes = false
		case "null":
			data = json.RawMessage(`null`)
			payloadEmpty = true
		case "absent":
			data = nil
			payloadEmpty = true
		}
		envBytes := map[string]any{"type": firstType}
		if data != nil {
			envBytes["data"] = data
		}
		raw, _ := json.Marshal(envBytes)
		var env Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			rt.Fatalf("harness: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := conn.WriteMessage(ctx, env); err != nil {
			rt.Fatalf("harness: write: %v", err)
		}

		// --- reference verdict, in the order hub.go documents the gates ---
		effWorker, effProto, effSerial := workerID, proto, serial
		if payloadEmpty {
			effWorker, effProto, effSerial = "", 0, ""
		}
		wantCode := ""
		switch {
		case firstType != TypeHello:
			wantCode = "protocol_error"
		case !payloadDecodes:
			wantCode = "bad_request"
		case effWorker != "worker-1":
			wantCode = "unauthorized"
		case effProto < OpStreamProto:
			wantCode = "proto_mismatch"
		case enforce && effSerial == "":
			wantCode = "serial_missing"
		case enforce && resolver.verdict[effSerial] != nil:
			wantCode = "serial_mismatch"
		}
		desc := fmt.Sprintf("first=%q payload=%s worker_id=%q proto=%s(%d) serial=%q enforce=%v last_seq=%d",
			firstType, payloadMode, workerID, protoMode, proto, serial, enforce, lastSeq)

		first, err := conn.ReadMessage(ctx)
		if err != nil {
			rt.Fatalf("no reply at all (%v); want %q\n  %s", err, wantCode, desc)
		}
		serialTouches := func() []string {
			auth.mu.Lock()
			defer auth.mu.Unlock()
			var out []string
			for _, tc := range auth.touches {
				if tc.serial != nil {
					out = append(out, *tc.serial)
				}
			}
			return out
		}
		if wantCode != "" {
			switch wantCode {
			case "protocol_error":
				helloGateStats.notHello.Add(1)
			case "bad_request":
				helloGateStats.badPayload.Add(1)
			case "unauthorized":
				helloGateStats.idRefused.Add(1)
			case "proto_mismatch":
				helloGateStats.protoRefused.Add(1)
			case "serial_missing":
				helloGateStats.serialMissing.Add(1)
			case "serial_mismatch":
				helloGateStats.serialMismatch.Add(1)
			}
			if first.Type != TypeError {
				rt.Fatalf("hello was accepted (first reply %q), reference refuses with %q\n  %s", first.Type, wantCode, desc)
			}
			var em ErrorMsg
			if err := json.Unmarshal(first.Data, &em); err != nil {
				rt.Fatalf("decode error reply: %v", err)
			}
			if em.Code != wantCode {
				rt.Fatalf("refused with code %q, reference says %q (message %q)\n  %s", em.Code, wantCode, em.Message, desc)
			}
			if em.Message == "" {
				rt.Fatalf("refusal %q carries no message\n  %s", em.Code, desc)
			}
			if wantCode == "proto_mismatch" && em.Proto != OpStreamProto {
				rt.Fatalf("proto_mismatch reply carries proto %d, want the global's %d\n  %s", em.Proto, OpStreamProto, desc)
			}
			// The hub must hang up, never register, never persist a serial.
			if _, err := conn.ReadMessage(ctx); err == nil {
				rt.Fatalf("connection stayed open and sent more after refusing with %q\n  %s", wantCode, desc)
			}
			if hub.IsWorkerConnected("site-a", "worker-1") {
				rt.Fatalf("refused worker is registered as connected\n  %s", desc)
			}
			if got := serialTouches(); len(got) != 0 {
				rt.Fatalf("a refused hello persisted serial(s) %q\n  %s", got, desc)
			}
			return
		}

		helloGateStats.accepted.Add(1)
		if first.Type != TypeSnapshot && first.Type != TypeNoChange {
			rt.Fatalf("accepted hello got %q as its reply\n  %s", first.Type, desc)
		}
		wantReply := TypeSnapshot
		if lastSeq == 7 {
			wantReply = TypeNoChange
		}
		if first.Type != wantReply {
			rt.Fatalf("hello(last_seq=%d) against seq 7 got %q, want %q\n  %s", lastSeq, first.Type, wantReply, desc)
		}
		var probe struct {
			Proto int `json:"proto"`
		}
		_ = json.Unmarshal(first.Data, &probe)
		if probe.Proto != OpStreamProto {
			rt.Fatalf("%s reply carries proto %d, want %d\n  %s", first.Type, probe.Proto, OpStreamProto, desc)
		}
		second, err := conn.ReadMessage(ctx)
		if err != nil || second.Type != TypeOpResync {
			rt.Fatalf("after the hello reply got (%q, %v), want op_resync\n  %s", second.Type, err, desc)
		}
		if !hub.IsWorkerConnected("site-a", "worker-1") {
			rt.Fatalf("accepted worker is not registered\n  %s", desc)
		}
		if got := serialTouches(); len(got) != 1 || got[0] != serial {
			rt.Fatalf("accepted hello persisted serials %q, want exactly [%q]\n  %s", got, serial, desc)
		}
		if got := hub.ConnectedWorkerSerials("site-a"); len(got) != 1 || got[0] != serial {
			rt.Fatalf("ConnectedWorkerSerials = %q, want [%q]\n  %s", got, serial, desc)
		}
		// A second hello on an established connection is a protocol error.
		if rapid.Bool().Draw(rt, "duplicateHello") {
			if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", Proto: OpStreamProto}); err != nil {
				rt.Fatalf("harness: %v", err)
			}
			dup, err := conn.ReadMessage(ctx)
			if err != nil || dup.Type != TypeError {
				rt.Fatalf("duplicate hello got (%q, %v), want an error reply\n  %s", dup.Type, err, desc)
			}
		}
	})
	t.Logf("coverage: cases=%d accepted=%d refused: not_hello=%d bad_payload=%d worker_id=%d proto=%d serial_missing=%d serial_mismatch=%d",
		helloGateStats.cases.Load(), helloGateStats.accepted.Load(), helloGateStats.notHello.Load(), helloGateStats.badPayload.Load(),
		helloGateStats.idRefused.Load(), helloGateStats.protoRefused.Load(), helloGateStats.serialMissing.Load(), helloGateStats.serialMismatch.Load())
}

type garbageCounters struct {
	obs, upd, logs, casts atomic.Int64
}

func (g *garbageCounters) HandleObservation(context.Context, model.Observation) error {
	g.obs.Add(1)
	return nil
}
func (g *garbageCounters) HandleOpUpdate(context.Context, OpUpdateMsg) error {
	g.upd.Add(1)
	return nil
}
func (g *garbageCounters) HandleLogBatch(context.Context, string, LogBatchMsg) error {
	g.logs.Add(1)
	return nil
}
func (g *garbageCounters) HandleCastBatch(context.Context, string, CastBatchMsg) error {
	g.casts.Add(1)
	return nil
}

var garbageStats struct{ cases, msgs, replies, stillUsable atomic.Int64 }

// TestProp_HubSurvivesMalformedTraffic: after a good hello, any sequence of
// envelopes with arbitrary types, request ids and payloads leaves the hub
// running and the connection usable (a claim_op still gets its response),
// and every RPC-style request that carried a request_id gets exactly one
// reply with that id. A panic in a hub goroutine would abort the test
// binary.
func TestProp_HubSurvivesMalformedTraffic(t *testing.T) {
	types := []string{TypeHeartbeat, TypeObservation, TypeOpUpdate, TypeLogBatch, TypeCastBatch, TypeCastResume,
		TypeClaimOp, TypePowerStatusResp, TypeBootStatusResp, TypeDHCPLeasesResp, TypeSnapshot, TypeOpDelta, "bogus", ""}
	rapid.Check(t, func(rt *rapid.T) {
		garbageStats.cases.Add(1)
		auth := newFakeAuth()
		auth.register("tok-1", "site-a", "worker-1")
		h := &garbageCounters{}
		hub := NewHub(HubConfig{Auth: auth, Rebuilder: &fakeRebuilder{snap: model.SiteSnapshot{SeqNum: 1}},
			ObsHandler: h, OpUpdateHandler: h, LogBatchHandler: h, CastBatchHandler: h, Logger: propDiscardLogger()})
		srv := httptest.NewServer(hub)
		defer srv.Close()
		conn, err := propDial(srv, "tok-1")
		if err != nil {
			rt.Fatalf("harness: dial: %v", err)
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := conn.WriteTyped(ctx, TypeHello, "", HelloMsg{WorkerID: "worker-1", Proto: OpStreamProto}); err != nil {
			rt.Fatalf("harness: %v", err)
		}
		for {
			env, err := conn.ReadMessage(ctx)
			if err != nil {
				rt.Fatalf("harness: hello: %v", err)
			}
			if env.Type == TypeOpResync {
				break
			}
		}

		wantReplies := map[string]bool{}
		n := rapid.IntRange(1, 12).Draw(rt, "messages")
		for i := 0; i < n; i++ {
			typ := rapid.SampledFrom(types).Draw(rt, "type")
			reqID := ""
			if rapid.Bool().Draw(rt, "withReqID") {
				reqID = fmt.Sprintf("g-%d", i)
			}
			var data json.RawMessage
			switch rapid.IntRange(0, 4).Draw(rt, "payloadKind") {
			case 0:
				data = nil
			case 1:
				data = json.RawMessage(`null`)
			case 2:
				// A plausible object with wrong-typed fields.
				data, _ = json.Marshal(map[string]any{
					"op_id":      rapid.SampledFrom([]any{"not-a-uuid", uuid.NewString(), 7, nil, []any{}}).Draw(rt, "op_id"),
					"machine_id": rapid.SampledFrom([]any{"nope", uuid.NewString(), 1.5, nil}).Draw(rt, "machine_id"),
					"event":      rapid.SampledFrom([]any{"commission_report", "", 3}).Draw(rt, "event"),
					"status":     rapid.SampledFrom([]any{"completed", "failed", "weird", 9}).Draw(rt, "status"),
					"envelopes":  rapid.SampledFrom([]any{nil, "x", []any{1, "two"}, []any{map[string]any{"ts": "not-a-time"}}}).Draw(rt, "envelopes"),
					"frames":     rapid.SampledFrom([]any{nil, 5, []any{map[string]any{"data": "!!not base64!!"}}}).Draw(rt, "frames"),
					"data":       rtJSONValue(rt, 2),
					"error":      rapid.SampledFrom([]any{"boom", 1, nil}).Draw(rt, "error"),
				})
			case 3:
				data, _ = json.Marshal(rtJSONValue(rt, 3))
			default:
				// Large, but well inside the hub's read limit (the limit
				// itself is TestHub_AcceptsMultiMiBLogBatch's subject).
				data, _ = json.Marshal(strings.Repeat("a", rapid.IntRange(0, 200_000).Draw(rt, "bigString")))
			}
			env := Envelope{Type: typ, RequestID: reqID, Data: data}
			if data == nil {
				env.Data = json.RawMessage(`null`)
				if rapid.Bool().Draw(rt, "emptyObject") {
					env.Data = json.RawMessage(`{}`)
				}
			}
			if err := conn.WriteMessage(ctx, env); err != nil {
				rt.Fatalf("harness: write %q: %v", typ, err)
			}
			garbageStats.msgs.Add(1)
			// Requests the hub answers by request_id.
			if reqID != "" && (typ == TypeObservation || typ == TypeClaimOp || typ == TypeCastResume) {
				wantReplies[reqID] = true
			}
		}
		// The connection must still work.
		if err := conn.WriteTyped(ctx, TypeClaimOp, "final", ClaimOpMsg{OperationID: uuid.NewString()}); err != nil {
			rt.Fatalf("connection unusable after malformed traffic: %v", err)
		}
		got := map[string]int{}
		for {
			env, err := conn.ReadMessage(ctx)
			if err != nil {
				rt.Fatalf("hub closed the connection (or stopped answering) after malformed traffic: %v; replies so far %v", err, got)
			}
			if env.RequestID != "" {
				got[env.RequestID]++
				garbageStats.replies.Add(1)
			}
			if env.RequestID == "final" {
				break
			}
		}
		garbageStats.stillUsable.Add(1)
		// Observation replies come from goroutines; drain the stragglers.
		deadline := time.Now().Add(5 * time.Second)
		for len(got)-1 < len(wantReplies) && time.Now().Before(deadline) {
			rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
			env, err := conn.ReadMessage(rctx)
			rcancel()
			if err != nil {
				break
			}
			if env.RequestID != "" {
				got[env.RequestID]++
			}
		}
		for id := range wantReplies {
			if got[id] != 1 {
				rt.Fatalf("request %s got %d replies, want exactly 1 (replies: %v)", id, got[id], got)
			}
		}
	})
	t.Logf("coverage: cases=%d messages=%d replies=%d connections_still_usable=%d",
		garbageStats.cases.Load(), garbageStats.msgs.Load(), garbageStats.replies.Load(), garbageStats.stillUsable.Load())
}
