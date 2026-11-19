//go:build !integration

package main

import (
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// completeDeployHarness runs completeDeploy with every side effect recorded
// in order: the deploy_complete report, the BootNext write, the reboot.
// fatal halts the run with a deployStop, as the production fatal exits.
type completeDeployHarness struct {
	events []string
	done   chan struct{}
}

func (h *completeDeployHarness) params() deployParams {
	h.done = make(chan struct{})
	return deployParams{
		done: h.done,
		fatal: func(stage string, err error) {
			h.events = append(h.events, "fatal:"+stage)
			panic(deployStop{kind: "fatal", stage: stage, err: err})
		},
		armBootNext: func(*agent.Context) { h.events = append(h.events, "arm-bootnext") },
		reboot:      func(_ *agent.Context, reason string) { h.events = append(h.events, "reboot:"+reason) },
	}
}

func (h *completeDeployHarness) run(report func() error) (stop deployStop, stopped bool) {
	ctx := &agent.Context{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p := h.params()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		s, ok := r.(deployStop)
		if !ok {
			panic(r)
		}
		stop, stopped = s, true
	}()
	completeDeploy(ctx, p, report)
	return deployStop{}, false
}

// TestCompleteDeploy_BootNextOnlyAfterDeployCompleteAccepted is the deploy
// half of the regression for a real-hardware property-test history: a deploy
// aborted in its finalizing phase left BootNext pointing at its half-finished
// install, and the next deploy's power cycle booted that disk instead of PXE
// (the firmware honors BootNext ahead of the BMC's PXE override). BootNext
// commits the host's next boot to the installed OS, so it may be armed only
// after the site worker has accepted deploy_complete, and never by a deploy
// whose completion was refused.
func TestCompleteDeploy_BootNextOnlyAfterDeployCompleteAccepted(t *testing.T) {
	t.Run("accepted: report, then BootNext, then reboot", func(t *testing.T) {
		h := &completeDeployHarness{}
		_, stopped := h.run(func() error {
			h.events = append(h.events, "report-deploy-complete")
			return nil
		})
		if stopped {
			t.Fatalf("completeDeploy failed an accepted deploy: %v", h.events)
		}
		want := []string{"report-deploy-complete", "arm-bootnext", "reboot:deploy"}
		if !slices.Equal(h.events, want) {
			t.Errorf("completion order = %v, want %v", h.events, want)
		}
		select {
		case <-h.done:
		default:
			t.Error("the flush loop's done channel was left open")
		}
	})

	t.Run("refused: fatal, BootNext untouched, no reboot", func(t *testing.T) {
		h := &completeDeployHarness{}
		stop, stopped := h.run(func() error {
			h.events = append(h.events, "report-deploy-complete")
			// What the reporter returns for an operation the site worker
			// no longer holds, after its retries.
			return errors.New("deploy_complete report failed after 4 attempts: HTTP 409")
		})
		if !stopped || stop.stage != "reporting deploy_complete" {
			t.Fatalf("a refused deploy_complete ended with %+v (stopped=%v), want fatal at \"reporting deploy_complete\"", stop, stopped)
		}
		if slices.Contains(h.events, "arm-bootnext") {
			t.Errorf("BootNext was armed for a deploy the site worker refused to complete: %v", h.events)
		}
		if slices.Contains(h.events, "reboot:deploy") {
			t.Errorf("the host was rebooted into a deploy the site worker refused to complete: %v", h.events)
		}
		want := []string{"report-deploy-complete", "fatal:reporting deploy_complete"}
		if !slices.Equal(h.events, want) {
			t.Errorf("events = %v, want %v", h.events, want)
		}
	})

	t.Run("no reporter: BootNext and reboot with nothing to wait for", func(t *testing.T) {
		h := &completeDeployHarness{}
		if _, stopped := h.run(nil); stopped {
			t.Fatalf("completeDeploy failed without a reporter: %v", h.events)
		}
		if want := []string{"arm-bootnext", "reboot:deploy"}; !slices.Equal(h.events, want) {
			t.Errorf("events = %v, want %v", h.events, want)
		}
	})
}
