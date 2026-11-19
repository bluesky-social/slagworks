//go:build !integration

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/model"
)

// newTestControl builds a runControl whose exit panics with a deployStop of
// kind "exit" (stage = the exit code), standing in for os.Exit, and whose
// stopProcs appends to *events so a test can check the order things ended in.
func newTestControl(reporter *agentReporter, events *[]string) *runControl {
	c := &runControl{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		reporter:  reporter,
		stopProcs: func() { *events = append(*events, "stop-procs") },
		done:      make(chan struct{}),
		exit: func(code int) {
			*events = append(*events, fmt.Sprintf("exit-%d", code))
			panic(deployStop{kind: "exit", stage: fmt.Sprint(code)})
		},
	}
	if reporter != nil {
		reporter.onInactive = c.halt
	}
	return c
}

// catchStop runs fn and returns the deployStop it ended with.
func catchStop(t *testing.T, fn func()) deployStop {
	t.Helper()
	var stop deployStop
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			s, ok := r.(deployStop)
			if !ok {
				panic(r)
			}
			stop = s
		}()
		fn()
	}()
	return stop
}

// TestRunDeploy_StopsWhenOperationIsNoLongerActive is the regression for a
// real-hardware property-test history: an operator aborted a destructive
// deploy, global failed the operation and the machine, and the agent kept
// installing, because nothing told it and it ignored the status of its own
// phase reports. Here the abort lands while the agent is in agent_starting;
// its next report, the partitioning phase, is answered 410 Gone. The agent
// must stop there, before any command that touches a disk, kill whatever it
// has running, and exit with the status that keeps the host in recovery
// instead of rebooting it.
func TestRunDeploy_StopsWhenOperationIsNoLongerActive(t *testing.T) {
	const (
		machineID = "6b1f0c3e-9a52-4d55-8f0e-1c2d3e4f5a6b"
		opID      = "0d9e8f7a-6b5c-4d3e-2f1a-0b9c8d7e6f5a"
	)
	layoutJSON, err := json.Marshal(pinnedDataLayout(""))
	if err != nil {
		t.Fatalf("marshal layout: %v", err)
	}

	var mu sync.Mutex
	aborted := false
	var answered []string // "<phase>=<status>" per phase report, in order
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines/" + machineID + "/storage-script":
			fmt.Fprintf(w, "#!/bin/sh\ncat <<'EOF'\n%s\nEOF\n", layoutJSON)
		case "/api/v1/machines/" + machineID + "/network-script":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/report":
			var report model.ProgressReport
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				http.Error(w, "bad report", http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			status := http.StatusOK
			if aborted {
				// What the site worker answers once global removed the op.
				status = http.StatusGone
			}
			if report.Phase != "" {
				answered = append(answered, fmt.Sprintf("%s=%d", report.Phase, status))
			}
			if report.Phase == "agent_starting" {
				// The operator aborts while the agent resolves its layout.
				aborted = true
			}
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	runner := &mdHostRunner{}
	ctx := &agent.Context{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Exec:   runner,
		Server: strings.TrimPrefix(srv.URL, "http://"),
	}
	reporter := newAgentReporter(ctx.Log, ctx.Server, opID, machineID, newLogCollector())
	var events []string
	ctl := newTestControl(reporter, &events)

	stop, stopped := runDeployUntilStop(ctx, deployParams{
		machineID:     machineID,
		operationID:   opID,
		expectUUID:    "ignored-by-checkIdentity",
		destructive:   true,
		reporter:      reporter,
		setPhase:      ctl.setPhase,
		fatal:         ctl.fatal,
		done:          ctl.done,
		checkIdentity: func(string) error { return nil },
	})
	if !stopped {
		t.Fatal("runDeploy returned without stopping")
	}

	mu.Lock()
	defer mu.Unlock()
	if stop.kind == "run" {
		t.Fatalf("the agent ran %q after its operation was aborted (phase reports and their answers: %v; commands: %v)",
			stop.stage, answered, runner.runs)
	}
	if stop.kind != "exit" || stop.stage != fmt.Sprint(exitOpInactive) {
		t.Fatalf("deploy ended with %s %q (%v), want exit status %d", stop.kind, stop.stage, stop.err, exitOpInactive)
	}
	if want := []string{"agent_starting=200", "partitioning=410"}; !slices.Equal(answered, want) {
		t.Errorf("phase reports and their answers = %v, want %v", answered, want)
	}
	if len(runner.runs) != 0 {
		t.Errorf("the agent ran disk commands after the abort: %v", runner.runs)
	}
	if want := []string{"stop-procs", "exit-3"}; !slices.Equal(events, want) {
		t.Errorf("the run ended with %v, want %v: commands in flight are killed before the agent exits", events, want)
	}
	select {
	case <-ctl.done:
	default:
		t.Error("the flush loop's done channel was left open")
	}
}

// TestRunControl_FatalAfterInactiveDefersToHalt covers the abort that
// arrives through the flush loop in the middle of a long command: halt kills
// the command, the command's error comes back to the run, and the run calls
// fatal with it. That error is the abort's doing. Reporting it would log a
// failure that never happened and exit with the failure status.
func TestRunControl_FatalAfterInactiveDefersToHalt(t *testing.T) {
	r, sink, _ := newSinkReporter(t, func(model.ProgressReport) int { return http.StatusGone })
	var events []string
	ctl := newTestControl(r, &events)

	// The flush loop's liveness report draws the 410 and halts.
	if stop := catchStop(t, func() { r.post(model.ProgressReport{}) }); stop.stage != fmt.Sprint(exitOpInactive) {
		t.Fatalf("liveness report answered 410 ended with %+v, want exit status %d", stop, exitOpInactive)
	}
	if want := []string{"stop-procs", "exit-3"}; !slices.Equal(events, want) {
		t.Fatalf("halt ended with %v, want %v", events, want)
	}
	reportsBefore := len(sink.received())

	// In production halt never returned and holds ending for good; the
	// test's exit panicked out of it, so release it to let fatal run.
	ctl.ending.Unlock()
	events = nil
	stop := catchStop(t, func() { ctl.fatal("extracting root filesystem", errors.New("rsync: signal: killed")) })
	if stop.stage != fmt.Sprint(exitOpInactive) {
		t.Errorf("fatal after the operation went inactive exited with %q, want %d", stop.stage, exitOpInactive)
	}
	if got := len(sink.received()); got != reportsBefore {
		t.Errorf("fatal sent %d report(s) for an operation that is over", got-reportsBefore)
	}
}

// TestRunControl_FatalReportsAndExitsOne pins the unchanged failure path: a
// run that fails on its own reports the failure and exits 1.
func TestRunControl_FatalReportsAndExitsOne(t *testing.T) {
	r, sink, _ := newSinkReporter(t, func(model.ProgressReport) int { return http.StatusOK })
	var events []string
	ctl := newTestControl(r, &events)

	stop := catchStop(t, func() { ctl.fatal("writing fstab", errors.New("boom")) })
	if stop.stage != "1" {
		t.Fatalf("fatal exited with %q, want 1", stop.stage)
	}
	reports := sink.received()
	if len(reports) != 1 || !reports[0].Failed || reports[0].Phase != "writing fstab" || reports[0].Error != "writing fstab: boom" {
		t.Errorf("failure report = %+v, want one failed report for stage \"writing fstab\"", reports)
	}
	if r.opInactive() || slices.Contains(events, "stop-procs") {
		t.Errorf("an ordinary failure went through the halt path: %v", events)
	}
}
