package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/cmd/slagctl/tui"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/slogkit"
)

func newOperationCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "operation", Short: "manage operations"}
	cmd.AddCommand(operationListCmd(), operationShowCmd(), operationWatchCmd())
	return cmd
}

func operationListCmd() *cobra.Command {
	var machineFilter string
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list operations",
		Long: `List operations.

By default only active (non-terminal) operations are shown — pending,
claimed, or running. Pass --all to include completed and failed ops.

Rows are ordered by last activity with the freshest at the BOTTOM, so
under -w the most-recently-changed op tails the table like a log.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			machineSlot := ""
			if machineFilter != "" {
				// Accept the same arg shapes as machine commands so
				// `--machine cpu1.pop1` works without a UUID lookup.
				r, err := resolveMachine(ctx(cmd), apiClient, machineFilter)
				if err != nil {
					return err
				}
				machineSlot = r.Slot()
			}
			ops, err := fetchOperations(ctx(cmd), machineSlot)
			if err != nil {
				return err
			}
			// Active-only by default: drop terminal ops unless --all.
			if !all {
				ops = filterActive(ops)
			}
			return renderOperations(cmd.OutOrStdout(), ops)
		},
	}
	cmd.Flags().StringVar(&machineFilter, "machine", "", "filter by machine ID")
	cmd.Flags().BoolVar(&all, "all", false, "include completed/failed ops (default: active only)")
	return watchable(cmd)
}

// fetchOperations GETs /api/v1/operations, optionally scoped to a single
// machine slot (UUID). Shared by `operation list` and `machine ops`.
func fetchOperations(c context.Context, machineSlot string) ([]model.Operation, error) {
	var ops []model.Operation
	q := url.Values{}
	if machineSlot != "" {
		q["machine"] = []string{machineSlot}
	}
	if err := apiClient.Get(c, "/api/v1/operations", q, &ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// filterActive drops terminal (completed/failed) ops. The server has no
// status filter, so this is client-side.
func filterActive(ops []model.Operation) []model.Operation {
	active := ops[:0]
	for _, op := range ops {
		if !isTerminal(op.Status) {
			active = append(active, op)
		}
	}
	return active
}

// renderOperations sorts ops freshest-at-bottom and prints them as a table
// (or JSON when --json is set). Shared by `operation list` and `machine ops`
// so both surfaces stay column-identical.
func renderOperations(out io.Writer, ops []model.Operation) error {
	// Order by last activity ascending so the freshest op is at the
	// bottom (tail/log view). The server returns created_at desc; we
	// re-sort on a best-effort "last activity" signal (see lastActivity)
	// rather than creation time so an op that just changed phase floats to
	// the bottom even if it was created earlier than a quiet peer.
	sort.SliceStable(ops, func(i, j int) bool {
		return lastActivity(ops[i]).Before(lastActivity(ops[j]))
	})

	if jsonOut {
		return format.PrintJSON(out, ops)
	}
	// An empty table is just a lonely header row, which reads as a broken
	// query rather than "this machine has no operations". Say so plainly.
	if len(ops) == 0 {
		fmt.Fprintln(out, format.DimStyle.Render("no operations"))
		return nil
	}
	rows := make([][]string, len(ops))
	for i, op := range ops {
		// Completed ops keep their last phase in the DB; showing
		// "finalizing" next to status=completed reads as stuck, so blank
		// it. Failed ops KEEP the phase — "failed at partitioning" is the
		// single most useful triage datum. The phase is whatever the
		// target's agent reported, so it is escaped for the terminal.
		phase := format.Untrusted(op.Phase)
		if op.Status == model.OpCompleted {
			phase = ""
		}
		// Machine column shows the human name (NetBox hostname or serial,
		// enriched server-side); fall back to the short UUID when the op
		// has no resolvable machine so the row still points somewhere.
		machine := op.MachineName
		if machine == "" {
			machine = shortID(op.MachineID.String())
		}
		rows[i] = []string{
			machine,
			op.ID.String(),
			string(op.Type),
			format.OperationStatusLabel(string(op.Status)),
			phase,
			op.ClaimedBy,
			format.ShortDuration(time.Since(lastActivity(op))),
		}
	}
	format.PrintTable(out,
		[]string{"MACHINE", "ID", "TYPE", "STATUS", "PHASE", "CLAIMED_BY", "UPDATED"},
		rows)
	return nil
}

// lastActivity returns the best available "this op last changed" timestamp.
//
// PhaseEnteredAt bumps on every phase transition, which covers the bulk of
// a deploy's life (partitioning -> rsyncing_os -> ...). But a status-only
// transition that does NOT change the phase (notably pending -> claimed,
// where Phase is still empty) does not necessarily bump PhaseEnteredAt, so
// we fold in the other lifecycle timestamps and take the latest:
//
//   - CompletedAt — set once on reaching a terminal state; the true last
//     event for completed/failed ops.
//   - ClaimedAt   — set when a worker claims; covers the pending->claimed
//     edge that may precede any phase change.
//   - PhaseEnteredAt — every phase transition during the run.
//   - CreatedAt   — floor; a freshly-created pending op has nothing else.
//
// Taking the max across these gives a monotonic "freshness" key that the
// sort and the UPDATED column both use.
//
// NOTE for the operator: this is a client-side heuristic. If you'd rather
// the server stamp a single authoritative updated_at on every status OR
// phase transition (including pending->claimed), that'd let us drop this
// max() and is the cleaner long-term fix — flagged in PLA if you want it.
func lastActivity(op model.Operation) time.Time {
	t := op.CreatedAt
	if op.ClaimedAt != nil && op.ClaimedAt.After(t) {
		t = *op.ClaimedAt
	}
	if op.PhaseEnteredAt.After(t) {
		t = op.PhaseEnteredAt
	}
	if op.CompletedAt != nil && op.CompletedAt.After(t) {
		t = *op.CompletedAt
	}
	return t
}

func operationShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [id]",
		Short: "show operation details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := uuid.Parse(args[0]); err != nil {
				return fmt.Errorf("invalid operation ID: must be a UUID")
			}
			var op model.Operation
			if err := apiClient.Get(ctx(cmd), "/api/v1/operations/"+args[0], nil, &op); err != nil {
				return err
			}
			return format.PrintJSON(cmd.OutOrStdout(), op)
		},
	}
}

func operationWatchCmd() *cobra.Command {
	var timeout time.Duration
	var plain bool
	var hold, noHold bool
	cmd := &cobra.Command{
		Use:   "watch <op-id|machine>",
		Short: "watch an operation until it completes",
		Long: `Watch an operation until it reaches a terminal state.

The argument is either:

  - an operation UUID (watch that specific op)
  - a machine UUID, NetBox serial, hostname, or unique hostname
    substring (watch the latest active op for that machine)

Machine resolution lets on-call type
"slagctl operation watch cpu1.pop1" without copying op IDs.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opID, err := resolveOperationArg(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			if plain {
				var op model.Operation
				if err := apiClient.Get(cmd.Context(), "/api/v1/operations/"+opID, nil, &op); err != nil {
					return err
				}
				if isTerminal(op.Status) {
					reportTerminalOp(cmd.Context(), op)
					if op.Status == model.OpFailed {
						return errOperationFailed(opID)
					}
					return nil
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
				defer cancel()
				return watchPlain(ctx, op, os.Stdout, plainWatchPollInterval)
			}
			return watchOperation(cmd.Context(), opID, timeout, hold, noHold)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "maximum wait time")
	cmd.Flags().BoolVar(&plain, "plain", false, "stream log entries to stdout without TUI")
	cmd.Flags().BoolVar(&hold, "hold", false, "hold the TUI open until q even when non-interactive (e.g. recording under CI)")
	cmd.Flags().BoolVar(&noHold, "no-hold", false, "tear down immediately on completion instead of holding for q (default: interactive runs hold until q)")
	return cmd
}

// resolveOperationArg disambiguates a `slagctl operation watch <arg>`
// positional. UUID inputs are tried as op IDs first (the historical
// shape); on miss we fall through to machine resolution and pick the
// latest active operation for that machine. Non-UUID inputs go
// straight to machine resolution.
//
// "Active" means non-terminal — pending/claimed/running. If a machine
// has multiple active ops (rare but possible during an abort race),
// we pick the most recently created one.
func resolveOperationArg(c context.Context, arg string) (string, error) {
	if id, err := uuid.Parse(arg); err == nil {
		// Try as operation first.
		var op model.Operation
		err := apiClient.Get(c, "/api/v1/operations/"+id.String(), nil, &op)
		if err == nil {
			return id.String(), nil
		}
		// 404 → fall through to machine resolution. Other errors are
		// fatal (auth, network); don't paper over them.
		var apiErr *slclient.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 {
			return "", fmt.Errorf("fetch operation %s: %w", id, err)
		}
	}

	r, err := resolveMachine(c, apiClient, arg)
	if err != nil {
		return "", err
	}
	var ops []model.Operation
	q := url.Values{"machine": {r.Slot()}}
	if err := apiClient.Get(c, "/api/v1/operations", q, &ops); err != nil {
		return "", fmt.Errorf("list operations for %s: %w", r.Label, err)
	}
	var latest *model.Operation
	for i := range ops {
		op := &ops[i]
		if op.Status == model.OpCompleted || op.Status == model.OpFailed {
			continue
		}
		if latest == nil || op.CreatedAt.After(latest.CreatedAt) {
			latest = op
		}
	}
	if latest == nil {
		return "", fmt.Errorf("no active operation for machine %s", r.Label)
	}
	return latest.ID.String(), nil
}

// watchOperation watches an operation until it reaches a terminal state.
// Interactive (TTY stderr) runs get the 3-pane deploy TUI (cmd/slagctl/tui);
// non-TTY runs fall back to the plain line stream. timeout bounds the whole
// watch via ctx; hold/noHold are the --hold/--no-hold overrides (plan D5).
func watchOperation(ctx context.Context, opID string, timeout time.Duration, hold, noHold bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !term.IsTerminal(int(os.Stderr.Fd())) {
		var op model.Operation
		if err := apiClient.Get(ctx, "/api/v1/operations/"+opID, nil, &op); err != nil {
			return err
		}
		if isTerminal(op.Status) {
			reportTerminalOp(ctx, op)
			if op.Status == model.OpFailed {
				return errOperationFailed(opID)
			}
			return nil
		}
		return watchPlain(ctx, op, os.Stderr, plainWatchPollInterval)
	}

	return runDeployTUI(ctx, opID, hold, noHold)
}

// plainWatchPollInterval is how often watchPlain re-reads the op row as its
// liveness backstop: the same cadence as the TUI's op-row poll.
const plainWatchPollInterval = 5 * time.Second

// watchPlain is the non-TUI watch (`operation watch --plain`, and any watch
// whose stderr is not a terminal: CI, pipes, `machine deploy -w 2>log`). It
// prints op's structured log envelopes to out in the `machine logs` line
// format until the op reaches a terminal state, prints the same one-line
// summary as the TUI to stderr, and returns nil only if the op completed. A
// failed op, or ctx ending first (--timeout, interrupt), returns an error, so
// the exit code is safe for automation to act on. ctx bounds the whole watch.
//
// The envelopes come from /api/v1/logs/stream through logSource, the feed the
// TUI's log pane reads: it backfills from the op's start and resumes from its
// cursor after a drop. Global's source=global envelope for this op carrying
// extra.status completed or failed ends the watch. The subscription also
// matches other ops on the same machine, so the op id must match too. The op
// row is polled every pollEvery as a backstop for a terminal envelope the
// stream never delivers (stranded behind the live-tail watermark, or a stream
// that cannot connect). A first terminal poll waits one more interval so the
// stream, which trails the commit by the live-tail watermark, can print the
// op's last lines before the watch ends.
func watchPlain(ctx context.Context, op model.Operation, out *os.File, pollEvery time.Duration) error {
	opID := op.ID.String()
	machineID := ""
	if op.MachineID != uuid.Nil {
		machineID = op.MachineID.String()
	}
	started := op.CreatedAt
	if started.IsZero() {
		started = time.Now()
	}

	type terminal struct {
		outcome tui.RunOutcome
		reason  string
	}
	streamed := make(chan terminal, 1)
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	streamDone := make(chan struct{})
	var lastPhase string // owned by the stream goroutine until streamDone closes
	go func() {
		defer close(streamDone)
		render := humanRenderer(out)
		_ = logSource(machineID, opID, op.CreatedAt)(streamCtx, func(id int64, env slogkit.Envelope) {
			_ = render(env, id)
			if env.OpID != op.ID {
				return
			}
			if env.Phase != "" {
				lastPhase = env.Phase
			}
			status := model.OperationStatus(env.StringExtra("status"))
			if env.Source != slogkit.SourceGlobal || !isTerminal(status) {
				return
			}
			t := terminal{outcome: tui.OutcomeCompleted, reason: env.StringExtra("error")}
			if status == model.OpFailed {
				t.outcome = tui.OutcomeFailed
			}
			select {
			case streamed <- t:
			default:
			}
		})
	}()

	outcome, reason := tui.OutcomeIncomplete, ""
	poll := opPoller(opID)
	polledTerminal := false
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case t := <-streamed:
			outcome, reason = t.outcome, t.reason
			break wait
		case <-tick.C:
			done, failed, why, err := poll(ctx)
			if err != nil || !done {
				continue
			}
			if !polledTerminal {
				// Restart the interval: a poll slower than pollEvery leaves the
				// next tick already pending, which would skip the grace.
				polledTerminal = true
				tick.Reset(pollEvery)
				continue
			}
			outcome, reason = tui.OutcomeCompleted, why
			if failed {
				outcome = tui.OutcomeFailed
			}
			break wait
		}
	}
	// Stop the stream and wait for it, so no log line lands after the summary
	// and lastPhase is safe to read.
	stopStream()
	<-streamDone
	// A terminal envelope the stream delivered while ctx was expiring (or
	// while a poll was in flight) still decides the outcome: select picks
	// among ready cases at random, so the loop can have left on ctx.Done.
	if outcome == tui.OutcomeIncomplete {
		select {
		case t := <-streamed:
			outcome, reason = t.outcome, t.reason
		default:
		}
	}

	fmt.Fprintln(os.Stderr, watchExitSummary(opID, tui.RunResult{
		Outcome:   outcome,
		LastPhase: lastPhase,
		Elapsed:   time.Since(started),
	}))
	switch outcome {
	case tui.OutcomeCompleted:
		return nil
	case tui.OutcomeFailed:
		if reason != "" {
			return fmt.Errorf("operation %s failed: %s", opID, reason)
		}
		return errOperationFailed(opID)
	default:
		return fmt.Errorf("operation %s had not finished when the watch ended: %w", opID, ctx.Err())
	}
}

func isTerminal(s model.OperationStatus) bool {
	return s == model.OpCompleted || s == model.OpFailed
}

// formatTerminalOpReport renders the one-line summary string for an op that
// was already terminal when watch attached. machine is the resolved hostname
// (or short UUID if resolution failed; empty if the op has no machine).
// The returned string includes ANSI styling matching the op's status.
func formatTerminalOpReport(op model.Operation, machine string) string {
	// The logs hint must name something resolvable by `slagctl machine logs`.
	// machineLabel falls back to shortID on a failed lookup; a short UUID
	// prefix is NOT a valid machine identifier (resolveMachine uuid.Parse
	// requires the full 36-char form). Start with the full UUID and upgrade
	// to the hostname only when machine is a real hostname, not the shortID
	// fallback.
	logsTarget := ""
	if op.MachineID != uuid.Nil {
		logsTarget = op.MachineID.String()
	}
	if machine != "" && machine != shortID(op.MachineID.String()) {
		logsTarget = machine
	}

	machineDesc := ""
	if machine != "" {
		machineDesc = ", " + machine
	}

	ts := ""
	if op.CompletedAt != nil {
		ts = " (" + op.CompletedAt.UTC().Format(time.RFC3339) + ")"
	}

	logsHint := ""
	if logsTarget != "" {
		logsHint = fmt.Sprintf(" — full logs: slagctl machine logs %s --op %s", logsTarget, op.ID)
	}

	if op.Status == model.OpCompleted {
		return format.StateReady.Render(fmt.Sprintf("%s operation %s (%s%s) already completed%s%s",
			format.IconReady, op.ID, string(op.Type), machineDesc, ts, logsHint))
	}
	return format.StateFailed.Render(fmt.Sprintf("%s operation %s (%s%s) already failed%s%s",
		format.IconFailed, op.ID, string(op.Type), machineDesc, ts, logsHint))
}

// reportTerminalOp prints the one-line summary for an op that was already
// terminal when watch attached, plus the command to view its full logs.
// Called before returning from the terminal fast path in both the TTY and
// non-TTY watch branches (preserving the D5 exit-code contract:
// completed→nil, failed→errOperationFailed).
func reportTerminalOp(ctx context.Context, op model.Operation) {
	machineID := ""
	if op.MachineID != uuid.Nil {
		machineID = op.MachineID.String()
	}
	machine := machineLabel(ctx, machineID)
	fmt.Fprintln(os.Stderr, formatTerminalOpReport(op, machine))
}
