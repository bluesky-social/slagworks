package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/term"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// fakeGlobal stands in for slagworksd-global with the two routes a plain
// watch reads, both of which the real server registers: the operator log
// stream and the operation row. Any other path, including
// /api/v1/ws/events, 404s as it does on a real server. The log stream
// requires the operator bearer token, records each subscribe frame, replays
// envs as the backfill and then holds the connection open.
type fakeGlobal struct {
	status atomic.Value // model.OperationStatus the op row reports
	subs   chan ws.LogStreamSubscribeMsg

	// hangOpRow stalls op row requests until the client gives up, and holds
	// the stream's envs back until one such request is in flight.
	hangOpRow   atomic.Bool
	opRowStuck  chan struct{}
	markStuckOp sync.Once
}

func newFakeGlobal(t *testing.T, op model.Operation, envs ...slogkit.Envelope) *fakeGlobal {
	t.Helper()
	g := &fakeGlobal{subs: make(chan ws.LogStreamSubscribeMsg, 16), opRowStuck: make(chan struct{})}
	g.status.Store(op.Status)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/operations/"+op.ID.String(), func(w http.ResponseWriter, r *http.Request) {
		if g.hangOpRow.Load() {
			g.markStuckOp.Do(func() { close(g.opRowStuck) })
			<-r.Context().Done()
			return
		}
		row := op
		row.Status = g.status.Load().(model.OperationStatus)
		_ = json.NewEncoder(w).Encode(row)
	})
	mux.HandleFunc("/api/v1/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		ctx := r.Context()
		_, raw, err := c.Read(ctx)
		if err != nil {
			return
		}
		var frame ws.Envelope
		var sub ws.LogStreamSubscribeMsg
		if json.Unmarshal(raw, &frame) != nil || frame.Type != ws.TypeLogStreamSubscribe || json.Unmarshal(frame.Data, &sub) != nil {
			return
		}
		select {
		case g.subs <- sub:
		default:
		}
		write := func(msgType string, data any) {
			buf, _ := ws.MarshalEnvelope(msgType, "", data)
			_ = c.Write(ctx, websocket.MessageText, buf)
		}
		write(ws.TypeSubscribed, ws.SubscribedMsg{})
		if g.hangOpRow.Load() {
			select {
			case <-g.opRowStuck:
			case <-ctx.Done():
				return
			}
		}
		for i, env := range envs {
			if id := int64(i + 1); id > sub.LastSeenID {
				write(ws.TypeLogStreamEnvelope, ws.LogStreamEnvelopeMsg{ID: id, Envelope: env})
			}
		}
		_, _, _ = c.Read(ctx) // hold open until the client leaves
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	prevURL, prevToken, prevClient := apiURL, apiToken, apiClient
	apiURL, apiToken = srv.URL, "tok"
	apiClient = slclient.New(slclient.Config{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()})
	t.Cleanup(func() { apiURL, apiToken, apiClient = prevURL, prevToken, prevClient })
	return g
}

func runningOp() model.Operation {
	return model.Operation{
		ID: uuid.New(), MachineID: uuid.New(), Type: model.OpDeploy,
		Status: model.OpRunning, CreatedAt: time.Now(),
	}
}

func agentLine(op model.Operation, phase, msg string) slogkit.Envelope {
	return slogkit.Envelope{
		TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Host: "agent",
		Msg: msg, Phase: phase, MachineID: op.MachineID, OpID: op.ID,
	}
}

// terminalEnvelope is the envelope global writes when an op ends
// (internal/store/op_events.go emitTerminalEvent).
func terminalEnvelope(machineID, opID uuid.UUID, status model.OperationStatus, errMsg string) slogkit.Envelope {
	extra := map[string]any{"status": string(status)}
	if errMsg != "" {
		extra["error"] = errMsg
	}
	return slogkit.Envelope{
		TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceGlobal, Host: "slagworksd-global",
		Msg: "operation " + string(status), MachineID: machineID, OpID: opID, Extra: extra,
	}
}

// tempOut is a file for watchPlain's log lines and a reader for what landed
// in it.
func tempOut(t *testing.T) (*os.File, func() string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "watch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, func() string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
}

// A plain watch used to dial /api/v1/ws/events, a route global never
// served: the 404 was retried until --timeout (30m by default) and the
// command exited non-zero whatever the op did. It must follow
// /api/v1/logs/stream and end on the op's terminal envelope.
func TestWatchPlain_CompletedOpExitsZero(t *testing.T) {
	op := runningOp()
	g := newFakeGlobal(t, op,
		agentLine(op, "partitioning", "wiping sda"),
		terminalEnvelope(op.MachineID, op.ID, model.OpCompleted, ""))
	out, read := tempOut(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// An hour-long poll interval leaves the stream alone to end the watch.
	if err := watchPlain(ctx, op, out, time.Hour); err != nil {
		t.Fatalf("plain watch of a completed op returned %v; want nil", err)
	}
	got := read()
	for _, want := range []string{"phase=partitioning wiping sda", "operation completed"} {
		if !strings.Contains(got, want) {
			t.Errorf("watch output lacks %q:\n%s", want, got)
		}
	}
	sub := <-g.subs
	if sub.OpID != op.ID.String() || sub.MachineID != op.MachineID.String() {
		t.Errorf("subscribed to op %q machine %q; want op %s machine %s", sub.OpID, sub.MachineID, op.ID, op.MachineID)
	}
}

func TestWatchPlain_FailedOpExitsNonZero(t *testing.T) {
	op := runningOp()
	newFakeGlobal(t, op, terminalEnvelope(op.MachineID, op.ID, model.OpFailed, "no disk matched the storage layout"))
	out, _ := tempOut(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := watchPlain(ctx, op, out, time.Hour)
	if err == nil || errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "failed: no disk matched the storage layout") {
		t.Fatalf("plain watch of a failed op returned %v; want an operation-failed error carrying the reason", err)
	}
}

// The subscription matches the machine as well as the op, so a terminal
// envelope for another op on the same machine arrives too. It must not end
// the watch.
func TestWatchPlain_IgnoresAnotherOpsTerminalEnvelope(t *testing.T) {
	op := runningOp()
	newFakeGlobal(t, op,
		terminalEnvelope(op.MachineID, uuid.New(), model.OpCompleted, ""),
		terminalEnvelope(op.MachineID, op.ID, model.OpFailed, "boom"))
	out, _ := tempOut(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := watchPlain(ctx, op, out, time.Hour); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("watch returned %v; want this op's failure, not the other op's completion", err)
	}
}

// When the stream never delivers the terminal envelope, the op row ends the
// watch.
func TestWatchPlain_OpRowBackstop(t *testing.T) {
	op := runningOp()
	g := newFakeGlobal(t, op, agentLine(op, "finalizing", "sync"))
	g.status.Store(model.OpCompleted)
	out, _ := tempOut(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := watchPlain(ctx, op, out, 20*time.Millisecond); err != nil {
		t.Fatalf("watch of an op whose row reads completed returned %v; want nil", err)
	}
}

// A terminal envelope that arrives while an op-row poll is stalled must still
// decide the outcome when the watch deadline ends that poll.
func TestWatchPlain_StreamedTerminalOutranksDeadline(t *testing.T) {
	op := runningOp()
	g := newFakeGlobal(t, op, terminalEnvelope(op.MachineID, op.ID, model.OpCompleted, ""))
	g.hangOpRow.Store(true)
	out, _ := tempOut(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// The fake sends the completion only once the first poll is stuck, so the
	// watch is blocked in that poll until the deadline cancels it; ctx.Done
	// and the queued completion are then ready together.
	if err := watchPlain(ctx, op, out, 10*time.Millisecond); err != nil {
		t.Fatalf("watch of an op the stream reported completed returned %v; want nil", err)
	}
}

func TestWatchPlain_TimeoutExitsNonZero(t *testing.T) {
	op := runningOp()
	newFakeGlobal(t, op, agentLine(op, "rsyncing_os", "copying"))
	out, _ := tempOut(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if err := watchPlain(ctx, op, out, 20*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("watch that outlived its timeout returned %v; want a deadline error", err)
	}
}

// Both commands that reach watchPlain: `operation watch --plain`, and
// `operation watch` with a non-terminal stderr, which is also the path
// `machine deploy -w` and `machine commission -w` take in CI and pipes.
func TestOperationWatch_PlainPathsFollowLogStream(t *testing.T) {
	for _, entry := range []struct {
		name   string
		flags  []string
		nonTTY bool
	}{
		{name: "plain flag", flags: []string{"--plain"}},
		{name: "non-TTY stderr", nonTTY: true},
	} {
		for _, status := range []model.OperationStatus{model.OpCompleted, model.OpFailed} {
			t.Run(entry.name+"/"+string(status), func(t *testing.T) {
				if entry.nonTTY && term.IsTerminal(int(os.Stderr.Fd())) {
					t.Skip("stderr is a terminal, so `operation watch` would start the TUI")
				}
				op := runningOp()
				newFakeGlobal(t, op, terminalEnvelope(op.MachineID, op.ID, status, ""))
				srvURL := apiURL
				root := NewRootCmd() // binding --api resets apiURL; the flag below sets it back
				SetAPIClient(root, apiClient)
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs(append([]string{"--api", srvURL, "operation", "watch", op.ID.String(), "--timeout", "5s"}, entry.flags...))

				err := root.Execute()
				if errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("watch timed out: %v", err)
				}
				if status == model.OpCompleted && err != nil {
					t.Fatalf("watch of a completed op returned %v; want nil", err)
				}
				if status == model.OpFailed && (err == nil || !strings.Contains(err.Error(), "failed")) {
					t.Fatalf("watch of a failed op returned %v; want an operation-failed error", err)
				}
			})
		}
	}
}
