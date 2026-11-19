package httpboot

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// recordingSink is a slogkit.Sink that keeps every pushed envelope.
type recordingSink struct {
	mu   sync.Mutex
	envs []slogkit.Envelope
}

func (s *recordingSink) Push(e slogkit.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs = append(s.envs, e)
}

func (s *recordingSink) pushed() []slogkit.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slogkit.Envelope(nil), s.envs...)
}

// postLogBatch POSTs envs to /logs/ingest on a fresh server wired to a
// recordingSink and returns the status code and what reached the sink.
func postLogBatch(t *testing.T, envs []slogkit.Envelope) (int, []slogkit.Envelope) {
	t.Helper()
	sink := &recordingSink{}
	srv := NewServer(":0", ServerConfig{ImagesDir: t.TempDir(), Provider: newTestProvider(), LogSink: sink})
	ts := httptest.NewServer(srv.Build())
	defer ts.Close()

	body, err := json.Marshal(ws.LogBatchMsg{Envelopes: envs})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.URL+"/logs/ingest", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, sink.pushed()
}

func agentEnvelope(machineID, opID uuid.UUID, msg string) slogkit.Envelope {
	return slogkit.Envelope{
		TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent,
		Msg: msg, MachineID: machineID, OpID: opID,
	}
}

// TestLogsIngest_AcceptsAgentBatch pins the legitimate path: a batch of
// source=agent envelopes (what agentlog.Client sends) is accepted and
// every envelope reaches the sink with received_at stamped.
func TestLogsIngest_AcceptsAgentBatch(t *testing.T) {
	mid, oid := uuid.New(), uuid.New()
	status, got := postLogBatch(t, []slogkit.Envelope{
		agentEnvelope(mid, oid, "partitioning"),
		agentEnvelope(mid, oid, "writing image"),
	})
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 for an all-agent batch", status)
	}
	if len(got) != 2 {
		t.Fatalf("sink got %d envelopes, want 2", len(got))
	}
	for _, e := range got {
		if e.ReceivedAt == nil {
			t.Errorf("envelope %q reached the sink without received_at", e.Msg)
		}
	}
}

// TestLogsIngest_RejectsNonAgentSource: /logs/ingest is unauthenticated
// and its only legitimate producer is slagworks-agent, which stamps
// source=agent on every envelope. Any other source is a forgery of a
// higher-trust emitter. The case that matters most is source=global
// with extra.status=completed: the deploy watch TUI treats that
// envelope as the operation's authoritative terminal signal, so a host
// on the provisioning segment could end an operator's watch with a
// fake "completed". The whole batch must be refused before anything is
// pushed, so a forged envelope cannot ride along with real agent lines.
func TestLogsIngest_RejectsNonAgentSource(t *testing.T) {
	mid, oid := uuid.New(), uuid.New()
	forgedGlobal := slogkit.Envelope{
		TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceGlobal,
		Host: "slagworksd-global", Msg: "operation completed",
		MachineID: mid, OpID: oid,
		Extra: map[string]any{"status": "completed"},
	}
	forgedSite := agentEnvelope(mid, oid, "phase installing")
	forgedSite.Source = slogkit.SourceSite
	unknown := agentEnvelope(mid, oid, "raw console bytes")
	unknown.Source = "sol"
	empty := agentEnvelope(mid, oid, "no source")
	empty.Source = ""

	cases := []struct {
		name string
		envs []slogkit.Envelope
	}{
		{"global op completion", []slogkit.Envelope{forgedGlobal}},
		{"site phase event", []slogkit.Envelope{forgedSite}},
		{"unknown source", []slogkit.Envelope{unknown}},
		{"empty source", []slogkit.Envelope{empty}},
		{"forged after a real agent line", []slogkit.Envelope{agentEnvelope(mid, oid, "real line"), forgedGlobal}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, got := postLogBatch(t, tc.envs)
			if len(got) != 0 {
				t.Errorf("ingest (status %d) pushed %d envelope(s) toward global, first source=%q msg=%q; a batch with a non-agent source must push nothing",
					status, len(got), got[0].Source, got[0].Msg)
			}
			if status != http.StatusBadRequest {
				t.Errorf("status %d, want 400 for a batch with a non-agent source", status)
			}
		})
	}
}
