package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/model"
)

// livenessInterval is how often the flush loop reports to the site worker
// when it has nothing else to say. It bounds how long an agent whose
// operation was aborted keeps working before it finds out.
const livenessInterval = 5 * time.Second

// agentReporter posts the agent's progress, logs and final result to the
// site worker's /api/v1/report, and is how the agent learns that its
// operation is over.
//
// The site worker answers a report for an operation it knows has ended
// (operator abort, a timeout sweep, a failure) with 410 Gone. That is the
// only word an agent gets that nobody is waiting for its work any more, so
// every report path checks for it, including the best-effort ones whose
// status is otherwise ignored, and the first 410 calls onInactive. Any other
// rejection keeps its old meaning: a best-effort report is dropped, a final
// report is retried and then fails the run. In particular 409 covers a site
// worker that restarted and has not resynchronized its operations, where the
// operation may be perfectly healthy.
type agentReporter struct {
	log       *slog.Logger
	server    string
	opID      string
	machineID string
	collector *logCollector
	client    *http.Client
	// liveness is the flush loop's tick; see livenessInterval.
	liveness time.Duration
	// onInactive is called once, from whichever goroutine's report drew the
	// first 410, unless the final report was already accepted. main points
	// it at the run's halt; nil ignores the signal.
	onInactive func()

	// mu serializes every report: one request is sent and its answer acted
	// on before the next goes out. Without that a liveness report answered
	// 410 because the final report just completed the operation could be
	// read as an abort.
	mu sync.Mutex
	// finalAccepted is set once the site worker accepted the run's final
	// report (deploy_complete, commission_report). The operation ending
	// after that is the expected outcome, not a reason to stop: a commission
	// agent still probes the console and reboots after global completed its
	// operation.
	finalAccepted bool
	// inactive is set by the first 410 that arrives before finalAccepted.
	inactive bool
}

func newAgentReporter(log *slog.Logger, server, opID, machineID string, collector *logCollector) *agentReporter {
	return &agentReporter{
		log:       log,
		server:    server,
		opID:      opID,
		machineID: machineID,
		collector: collector,
		client:    &http.Client{Timeout: 10 * time.Second},
		liveness:  livenessInterval,
	}
}

// opInactive reports whether the site worker has said this agent's
// operation is over.
func (r *agentReporter) opInactive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inactive
}

func (r *agentReporter) ReportPhase(phase string) {
	lines := r.drainLogs()
	r.post(model.ProgressReport{Phase: phase, Logs: lines})
}

func (r *agentReporter) ReportEvent(event string) {
	lines := r.drainLogs()
	r.post(model.ProgressReport{Event: event, Logs: lines})
}

// ReportDeployComplete posts the deploy_complete event with the
// agent's BMC reconcile output. Single-purpose method: deploy_complete
// is the only event type that carries BMCCreds, and BMCCreds is the
// only event-specific extra payload that exists today.
//
// Returns an error if every retry attempt fails. The agent must abort
// the deploy on a non-nil return — losing this report after a monitor
// password was written to the BMC would leave pop/<pop>/bmc-creds-monitor
// holding a stale password with no record of the new one, breaking the
// host's BMC scrapes.
func (r *agentReporter) ReportDeployComplete(creds *model.BMCCreds) error {
	// Four attempts with exponential backoff (1s, then *5 each retry:
	// 1s/5s/25s). The site worker is reachable over the deploy network —
	// a sustained 30+ second outage at this exact window is the only
	// failure mode this loop doesn't paper over, and that's the
	// operator-attention case anyway. Aggressive retry here is
	// load-bearing: see the method doc — losing this report after the
	// BMC rotation permanently desyncs creds. 2xx counts as success.
	return r.send(model.ProgressReport{
		Event:    "deploy_complete",
		Logs:     r.drainLogs(),
		BMCCreds: creds,
	}, true, agent.PostOptions{
		Client:      r.client,
		Attempts:    4,
		Backoff:     time.Second,
		BackoffMult: 5,
		Name:        "deploy_complete report",
	})
}

// ReportCommissionComplete sends inventory through the same operation-bound
// endpoint as deploy completion. The site rejects the report unless op_id is
// an active commission operation owned by machine_id, then validates the
// inventory identity before queueing it for global persistence.
func (r *agentReporter) ReportCommissionComplete(data json.RawMessage) error {
	return r.send(model.ProgressReport{
		Event:          "commission_report",
		Logs:           r.drainLogs(),
		CommissionData: data,
	}, true, agent.PostOptions{
		Client:   &http.Client{Timeout: 30 * time.Second},
		Attempts: 3,
		Backoff:  2 * time.Second,
		Name:     "commission report",
		AcceptFn: func(status int) bool { return status == http.StatusOK },
	})
}

func (r *agentReporter) ReportFailure(phase, errMsg string) {
	lines := r.drainLogs()
	r.post(model.ProgressReport{
		Phase:  phase,
		Failed: true,
		Error:  errMsg,
		Logs:   lines,
	})
}

// RunFlushLoop ships collected log lines and, every liveness interval,
// reports to the site worker whether or not there is anything to ship. The
// empty report is the liveness check: phase reports can be minutes apart
// (the OS extract), and without it an aborted agent would keep writing to
// the disks until its next phase.
func (r *agentReporter) RunFlushLoop(done <-chan struct{}) {
	ticker := time.NewTicker(r.liveness)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			logs := r.drainLogs()
			if len(logs) == 0 && r.finalReportAccepted() {
				// The operation may be complete by now, and a refused
				// probe would only be noise in the site worker's log.
				continue
			}
			r.post(model.ProgressReport{Logs: logs})
		case <-r.collector.FlushCh():
			r.post(model.ProgressReport{Logs: r.drainLogs()})
		}
	}
}

func (r *agentReporter) finalReportAccepted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finalAccepted
}

func (r *agentReporter) drainLogs() []model.LogLine {
	raw := r.collector.Drain()
	if len(raw) == 0 {
		return nil
	}
	out := make([]model.LogLine, len(raw))
	for i, l := range raw {
		out[i] = model.LogLine{Source: l.Source, Timestamp: l.Timestamp, Line: l.Line}
	}
	return out
}

// post sends a best-effort report: single attempt, no retry. Progress spam
// is flushed every few seconds (RunFlushLoop), so losing one batch is fine —
// the next tick carries the backlog. Only a transport error is worth a log
// line; a refused report is dropped, except that send still acts on 410.
func (r *agentReporter) post(report model.ProgressReport) {
	_ = r.send(report, false, agent.PostOptions{
		Client:   r.client,
		Attempts: 1,
		Name:     "progress report",
		AcceptFn: func(int) bool { return true },
	})
}

// send is the one path every report takes. It stamps the operation binding,
// posts with the caller's retry policy, and handles the answers that mean
// more than success or failure of this one report: an accepted final report
// seals the run, and 410 Gone before that marks the operation inactive and
// calls onInactive. final marks deploy_complete and commission_report.
func (r *agentReporter) send(report model.ProgressReport, final bool, opts agent.PostOptions) error {
	report.OperationID = r.opID
	report.MachineID = r.machineID
	body, err := json.Marshal(report)
	if err != nil {
		r.log.Warn("marshal "+opts.Name, "err", err)
		return fmt.Errorf("marshal %s: %w", opts.Name, err)
	}
	// 410 ends the retry loop: no later attempt can revive the operation.
	opts.FinalFn = func(status int) bool { return status == http.StatusGone }
	url := fmt.Sprintf("http://%s/api/v1/report", r.server)

	r.mu.Lock()
	if r.inactive {
		// Already told to stop; the process is on its way out.
		r.mu.Unlock()
		return fmt.Errorf("%s not sent: the operation is no longer active", opts.Name)
	}
	err = agent.PostJSON(r.log, url, body, opts)
	stop := false
	if se, ok := errors.AsType[*agent.StatusError](err); ok && se.Status == http.StatusGone && !r.finalAccepted {
		r.inactive = true
		stop = true
	}
	if err == nil && final {
		r.finalAccepted = true
	}
	r.mu.Unlock()

	if stop && r.onInactive != nil {
		r.onInactive()
	}
	return err
}
