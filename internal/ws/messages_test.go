package ws

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	tests := []struct {
		name      string
		msgType   string
		requestID string
		data      any
	}{
		{
			name:    "log_batch",
			msgType: TypeLogBatch,
			data: LogBatchMsg{
				Envelopes: []slogkit.Envelope{
					{Source: slogkit.SourceAgent, Level: slogkit.LevelInfo, Msg: "hello"},
				},
			},
		},
		{
			name:    "op_update",
			msgType: TypeOpUpdate,
			data: OpUpdateMsg{
				OperationID: "660e8400-e29b-41d4-a716-446655440000",
				Status:      "running",
				Phase:       "partitioning",
			},
		},
		{
			name:      "claim_op with request_id",
			msgType:   TypeClaimOp,
			requestID: "req-123",
			data: ClaimOpMsg{
				OperationID: "op-1",
			},
		},
		{
			name:    "subscribed",
			msgType: TypeSubscribed,
			data:    SubscribedMsg{},
		},
		{
			name:    "error",
			msgType: TypeError,
			data:    ErrorMsg{Code: "not_found", Message: "machine not found"},
		},
		{
			name:    "heartbeat",
			msgType: TypeHeartbeat,
			data:    HeartbeatMsg{WorkerID: "worker-1"},
		},
		{
			name:    "hello",
			msgType: TypeHello,
			data: HelloMsg{
				WorkerID:   "worker-1",
				FQDN:       "worker-1.pop3",
				LastSeqNum: 42,
			},
		},
		{
			name:    "no_change",
			msgType: TypeNoChange,
			data:    NoChangeMsg{},
		},
		{
			name:    "snapshot",
			msgType: TypeSnapshot,
			data: SnapshotMsg{
				Snapshot: model.SiteSnapshot{SeqNum: 7, Site: model.SiteInfo{Slug: "pop3"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := MarshalEnvelope(tt.msgType, tt.requestID, tt.data)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			env, err := ParseEnvelope(raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			if env.Type != tt.msgType {
				t.Errorf("type: got %q, want %q", env.Type, tt.msgType)
			}
			if env.RequestID != tt.requestID {
				t.Errorf("request_id: got %q, want %q", env.RequestID, tt.requestID)
			}

			// Verify data round-trips through JSON
			reMarshaled, err := json.Marshal(tt.data)
			if err != nil {
				t.Fatalf("re-marshal: %v", err)
			}
			var want, got any
			json.Unmarshal(reMarshaled, &want)
			json.Unmarshal(env.Data, &got)

			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Errorf("data mismatch:\n  got:  %s\n  want: %s", gotJSON, wantJSON)
			}
		})
	}
}

// TestHelloMsgSerial pins the wire contract for the serial field:
// it round-trips when set and is omitted (omitempty) when empty, so an old
// worker that sends no serial produces byte-identical JSON to the pre-field
// shape.
func TestHelloMsgSerial(t *testing.T) {
	withSerial, err := json.Marshal(HelloMsg{WorkerID: "w1", Serial: "SN-ABC"})
	if err != nil {
		t.Fatal(err)
	}
	var round HelloMsg
	if err := json.Unmarshal(withSerial, &round); err != nil {
		t.Fatal(err)
	}
	if round.Serial != "SN-ABC" {
		t.Fatalf("serial did not round-trip: got %q", round.Serial)
	}

	empty, err := json.Marshal(HelloMsg{WorkerID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(empty), "serial") {
		t.Fatalf("empty serial must be omitted, got %s", empty)
	}
}
