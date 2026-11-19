package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/cmd/slagctl/tui"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// errOperationFailed is the non-zero-exit sentinel for a failed op, preserving
// the watchOperation 0/non-zero contract (plan D5).
func errOperationFailed(opID string) error {
	return fmt.Errorf("operation %s failed", opID)
}

// This file wires the cmd package's stream plumbing into the tui package's
// Run(): it builds the ConsoleSource / LogSource / OpPoller callbacks from the
// existing WS endpoints and dispatches `machine deploy/commission -w` and
// `operation watch` into the one 3-pane model (plan D8 — the legacy watchModel
// is deleted). The TUI package owns rendering + reduction; this file owns the
// network and the slagctl globals (apiURL/apiToken).

// runDeployTUI is the entry point for an interactive -w / `operation watch`.
// It fetches the op once (for type + claimed_by backfill), then drives the TUI
// off the live console + logs streams with the op-row poll as liveness
// backstop. Returns a non-nil error iff the op failed or the program errored
// (preserving the watchOperation 0/non-zero contract, plan D5).
func runDeployTUI(ctx context.Context, opID string, holdFlag, noHoldFlag bool) error {
	var op model.Operation
	if err := apiClient.Get(ctx, "/api/v1/operations/"+opID, nil, &op); err != nil {
		return err
	}
	machineID := ""
	if op.MachineID != uuid.Nil {
		machineID = op.MachineID.String()
	}

	// Already terminal before we attached: report the outcome and exit.
	if isTerminal(op.Status) {
		reportTerminalOp(ctx, op)
		if op.Status == model.OpFailed {
			return errOperationFailed(opID)
		}
		return nil
	}

	machine := machineLabel(ctx, machineID)
	resolveWorker := workerResolver(ctx)
	cfg := tui.RunConfig{
		OpType:        op.Type,
		Machine:       machine,
		OpID:          op.ID,
		OpStartedAt:   op.CreatedAt,                // server-side op-create time; anchors the header "elapsed" to the same clock as the phase durations
		Command:       opCommandLine(op, machine),  // human-readable invocation (replaces the raw op UUID)
		Worker:        resolveWorker(op.ClaimedBy), // attach-after-claim backfill (plan D6), UUID -> hostname
		ResolveWorker: resolveWorker,               // mid-stream claim envelopes carry a worker UUID too
		Loc:           time.Local,
		Console:       consoleSource(machineID),
		Logs:          logSource(machineID, opID, op.CreatedAt),
		Poll:          opPoller(opID),
		Interactive:   interactiveStderr(),
		Hold:          holdFlag,
		NoHold:        noHoldFlag,
	}
	result, err := tui.Run(ctx, cfg)
	if err != nil {
		return err
	}
	// Print a one-line terminal summary after the alt-screen tears down, so the
	// operator can tell "completed" / "failed" / "timed out waiting" apart
	// without a separate `operation show`. The TUI renders to the alt
	// screen on stderr; the summary goes to stderr too so it lands wherever the
	// monitor was and doesn't pollute a piped stdout.
	fmt.Fprintln(os.Stderr, watchExitSummary(opID, result))
	if result.Outcome == tui.OutcomeFailed {
		return errOperationFailed(opID)
	}
	return nil
}

// watchExitSummary renders the one-line outcome printed when the watch exits.
// It names the outcome and the last-known phase so a timed-out wait is
// distinguishable from a completion at a glance.
func watchExitSummary(opID string, r tui.RunResult) string {
	phase := r.LastPhase
	if phase == "" {
		phase = "unknown"
	}
	switch r.Outcome {
	case tui.OutcomeCompleted:
		return fmt.Sprintf("operation %s completed in %s", opID, formatDur(r.Elapsed))
	case tui.OutcomeFailed:
		return fmt.Sprintf("operation %s FAILED at phase %q after %s", opID, phase, formatDur(r.Elapsed))
	default: // OutcomeIncomplete
		return fmt.Sprintf("operation %s still in progress (watch ended after %s, last phase %q) — re-run `slagctl operation watch %s` or `slagctl operation show %s`",
			opID, formatDur(r.Elapsed), phase, opID, opID)
	}
}

// formatDur renders a duration compactly for the exit summary, matching the
// TUI's "2m14s" shape. Zero/negative reads as "0s".
func formatDur(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
}

// consoleSource returns a tui.ConsoleSource that streams the machine's BMC SOL
// cast in SNAPSHOT mode: the server replays the entire cast through
// its own throwaway vt emulator and sends ONE reconstructed-screen frame (a few
// KB regardless of ring size), then tails live frames. This both fixes the
// freeze (the old fromStart path streamed the whole hundreds-of-KB ring buffer
// into the client's single-threaded emulator inside the Bubble Tea Update loop,
// wedging it) AND shows the true current screen even on a host that's been quiet
// at a phase for minutes (a tail-window backfill would render blank there,
// because a cursor-addressed SOL screen can't be reconstructed from a mid-stream
// fragment). The live-edge callback fires after the first idle gap (the stream
// has no explicit marker — a pause in frames means the snapshot+backlog drained,
// per console_stream.go's caught-up gate).
func consoleSource(machineID string) tui.ConsoleSource {
	if machineID == "" {
		return nil
	}
	return func(ctx context.Context, onBytes func([]byte, bool), onLiveEdge func()) error {
		url, err := buildConsoleStreamURL(apiURL, machineID, "" /*tail*/, false /*fromStart*/, true /*follow*/, true /*snapshot*/)
		if err != nil {
			return err
		}
		conn, resp, err := websocket.Dial(ctx, url, streamDialOptions(apiToken))
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			return err
		}
		defer conn.CloseNow() //nolint:errcheck

		// Read in a goroutine feeding a channel. We must NOT use a per-read
		// context deadline for live-edge detection: coder/websocket
		// permanently closes the connection when a Read context is cancelled
		// or times out (CWE-664), which would kill the stream right after the
		// backfill drained. Instead the reader always reads on the parent ctx,
		// and the select-loop below detects the backfill lull with a timer
		// that never touches the connection.
		type frame struct {
			raw []byte
			err error
		}
		frames := make(chan frame, 64)
		go func() {
			for {
				_, raw, err := conn.Read(ctx)
				select {
				case frames <- frame{raw, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()

		// Live-edge detection: the cast backfill arrives as a fast burst, then
		// the server polls pg every ~250ms (console_stream.go's caught-up
		// gate). A gap longer than that between frames means the backfill
		// drained and we're at the live edge — fire onLiveEdge once so the
		// model drops the catch-up placeholder.
		const liveEdgeGap = 600 * time.Millisecond
		liveEdgeFired := false
		lull := time.NewTimer(liveEdgeGap)
		defer lull.Stop()
		fireLiveEdge := func() {
			if !liveEdgeFired {
				liveEdgeFired = true
				onLiveEdge()
			}
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-lull.C:
				fireLiveEdge()
			case f := <-frames:
				if f.err != nil {
					return f.err
				}
				if !liveEdgeFired {
					lull.Reset(liveEdgeGap)
				}
				// First frame is the asciicast header ([version,...] won't
				// parse as the [t,"o",data] event tuple) — skipped by the kind
				// check.
				var ev [3]any
				if json.Unmarshal(f.raw, &ev) != nil {
					continue
				}
				if kind, _ := ev[1].(string); kind == "o" {
					if data, ok := ev[2].(string); ok {
						onBytes([]byte(data), !liveEdgeFired)
					}
				}
			}
		}
	}
}

// logSource streams control-plane log envelopes for the op, reconnecting with
// the cursor (plan D4). It subscribes excluding SOL (D3) — phases ride these
// envelopes, and the D6 global lifecycle envelopes carry claim/terminal.
func logSource(machineID, opID string, since time.Time) tui.LogSource {
	return func(ctx context.Context, onEnvelope func(int64, slogkit.Envelope)) error {
		var lastSeen int64
		for {
			err := streamLogEnvelopes(ctx, machineID, opID, since, &lastSeen, onEnvelope)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return nil
			}
			// Transient drop: reconnect from the cursor after a short backoff.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
}

// streamLogEnvelopes opens one /api/v1/logs/stream connection and pumps
// envelopes through onEnvelope, advancing *lastSeen so a reconnect resumes
// past it. Returns nil on clean EOF, error on drop.
func streamLogEnvelopes(ctx context.Context, machineID, opID string, since time.Time, lastSeen *int64, onEnvelope func(int64, slogkit.Envelope)) error {
	streamURL, err := buildLogsStreamURL(apiURL)
	if err != nil {
		return err
	}
	conn, resp, err := websocket.Dial(ctx, streamURL, streamDialOptions(apiToken))
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return err
	}
	defer conn.CloseNow() //nolint:errcheck

	sub := ws.LogStreamSubscribeMsg{
		MachineID: machineID,
		OpID:      opID,
		Sources:   []string{"agent", "site", "global"}, // exclude sol (D3)
		// Bound the backfill to the op's start: the server OR's machine_id and
		// op_id (so this machine's pre-op DHCP/TFTP and any concurrent op on it
		// surface), and `since` keeps that to THIS deploy's window rather than
		// the machine's whole history. On reconnect LastSeenID takes precedence.
		LastSeenID: *lastSeen,
	}
	if !since.IsZero() {
		sub.SinceTimestamp = since.UTC().Format(time.RFC3339Nano)
	}
	if err := writeWSMessage(ctx, conn, ws.TypeLogStreamSubscribe, sub); err != nil {
		return err
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var env ws.Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.Type != ws.TypeLogStreamEnvelope {
			continue
		}
		var msg ws.LogStreamEnvelopeMsg
		if json.Unmarshal(env.Data, &msg) != nil {
			continue
		}
		if msg.ID > *lastSeen {
			*lastSeen = msg.ID
		}
		onEnvelope(msg.ID, msg.Envelope)
	}
}

// opPoller returns the liveness backstop (plan D6): fetch the op row and report
// whether it's terminal. The stream normally beats it; this only catches a
// terminal envelope stranded behind the live-tail watermark.
func opPoller(opID string) tui.OpPoller {
	return func(ctx context.Context) (terminal, failed bool, reason string, err error) {
		var op model.Operation
		if e := apiClient.Get(ctx, "/api/v1/operations/"+opID, nil, &op); e != nil {
			return false, false, "", e
		}
		if !isTerminal(op.Status) {
			return false, false, "", nil
		}
		return true, op.Status == model.OpFailed, opFailureReason(op), nil
	}
}

// opFailureReason pulls the human error out of a failed op's result JSON.
func opFailureReason(op model.Operation) string {
	if op.Status != model.OpFailed {
		return ""
	}
	r, err := op.DecodeResult()
	if err != nil {
		return ""
	}
	return r.Error
}

// machineLabel resolves a machine UUID to its hostname for the header,
// best-effort (falls back to the short UUID).
func machineLabel(ctx context.Context, machineID string) string {
	if machineID == "" {
		return ""
	}
	var m model.Machine
	if err := apiClient.Get(ctx, "/api/v1/machines/"+machineID, nil, &m); err == nil && m.Hostname != "" {
		return m.Hostname
	}
	return shortID(machineID)
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// opCommandLine reconstructs the slagctl invocation that would produce this op,
// so the header can show "machine deploy <host> --destructive" instead of a raw
// op UUID. It is best-effort: the params we persist (destructive/recovery for
// deploy) are reconstructable; anything we don't recognize falls back to
// "<type> <machine>". The machine arg is the resolved hostname (what an operator
// would actually type), falling back to the op's machine UUID.
func opCommandLine(op model.Operation, machine string) string {
	arg := machine
	if arg == "" && op.MachineID != uuid.Nil {
		arg = op.MachineID.String()
	}
	switch op.Type {
	case model.OpDeploy:
		cmd := "machine deploy " + arg
		p, _ := op.DecodeParams()
		if p.Destructive {
			cmd += " --destructive"
		}
		if p.Recovery {
			cmd += " --recovery"
		}
		return cmd
	case model.OpCommission:
		return "machine commission " + arg
	case model.OpPowerOn:
		return "machine power on " + arg
	case model.OpPowerOff:
		return "machine power off " + arg
	case model.OpPowerCycle:
		return "machine power cycle " + arg
	case model.OpBMCReconcile:
		cmd := "machine bmc-reconcile " + arg
		if p, _ := op.DecodeParams(); p.RotateMonitor {
			cmd += " --rotate-monitor"
		}
		return cmd
	default:
		return string(op.Type) + " " + arg
	}
}

// workerResolver returns a function mapping a worker identifier (the op's
// ClaimedBy / a claim envelope's claimed_by, which is a worker UUID) to its
// hostname (SiteWorker.FQDN). The worker list is fetched EAGERLY here, at
// startup (where synchronous fetches are already the norm — op + machine are
// fetched the same way), so the returned closure never does I/O. This matters
// because the closure is invoked from the Bubble Tea Update loop when a claim
// envelope arrives mid-stream; a synchronous HTTP request there would freeze
// the UI event loop. Unknown IDs (or a failed fetch) pass through unchanged —
// the TUI then shortens a UUID itself, so we never show worse than before.
func workerResolver(ctx context.Context) func(string) string {
	byID := map[string]string{}
	var workers []model.SiteWorker
	if err := apiClient.Get(ctx, "/api/v1/workers", nil, &workers); err == nil {
		for _, w := range workers {
			if w.FQDN != "" {
				byID[w.WorkerID] = w.FQDN
			}
		}
	}
	return func(id string) string {
		if id == "" {
			return ""
		}
		if fqdn, ok := byID[id]; ok {
			return fqdn
		}
		return id // unknown: let the TUI shorten the UUID
	}
}

// interactiveStderr reports whether the TUI should hold on failure: stderr is
// a TTY (we render to stderr) and we're not in a detected CI environment.
func interactiveStderr() bool {
	if os.Getenv("CI") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
}
