package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/model"
)

type agentReporter struct {
	log       *slog.Logger
	server    string
	opID      string
	machineID string
	collector *logCollector
	client    *http.Client
}

func newAgentReporter(log *slog.Logger, server, opID, machineID string, collector *logCollector) *agentReporter {
	return &agentReporter{
		log:       log,
		server:    server,
		opID:      opID,
		machineID: machineID,
		collector: collector,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
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
	report := model.ProgressReport{
		OperationID: r.opID,
		MachineID:   r.machineID,
		Event:       "deploy_complete",
		Logs:        r.drainLogs(),
		BMCCreds:    creds,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal deploy_complete report: %w", err)
	}

	// Four attempts with exponential backoff (1s, then *5 each retry:
	// 1s/5s/25s). The site worker is reachable over the deploy network —
	// a sustained 30+ second outage at this exact window is the only
	// failure mode this loop doesn't paper over, and that's the
	// operator-attention case anyway. Aggressive retry here is
	// load-bearing: see the method doc — losing this report after the
	// BMC rotation permanently desyncs creds. 2xx counts as success.
	url := fmt.Sprintf("http://%s/api/v1/report", r.server)
	return agent.PostJSON(r.log, url, body, agent.PostOptions{
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
	report := model.ProgressReport{
		OperationID:    r.opID,
		MachineID:      r.machineID,
		Event:          "commission_report",
		Logs:           r.drainLogs(),
		CommissionData: data,
	}
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal commission report: %w", err)
	}
	url := fmt.Sprintf("http://%s/api/v1/report", r.server)
	return agent.PostJSON(r.log, url, body, agent.PostOptions{
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

func (r *agentReporter) RunFlushLoop(done <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if r.collector.Len() > 0 {
				r.post(model.ProgressReport{Logs: r.drainLogs()})
			}
		case <-r.collector.FlushCh():
			r.post(model.ProgressReport{Logs: r.drainLogs()})
		}
	}
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

func (r *agentReporter) post(report model.ProgressReport) {
	report.OperationID = r.opID
	report.MachineID = r.machineID

	// Skip the heartbeat-with-no-content case — the flush loop wakes
	// every 5s, and an empty post just adds noise to slagworksd logs.
	if report.Phase == "" && report.Event == "" && !report.Failed && len(report.Logs) == 0 && report.BMCCreds == nil && len(report.CommissionData) == 0 {
		return
	}

	body, err := json.Marshal(report)
	if err != nil {
		r.log.Warn("marshal progress report", "err", err)
		return
	}
	url := fmt.Sprintf("http://%s/api/v1/report", r.server)
	// Fire-and-forget: single attempt, no retry. Progress spam is
	// flushed every 5s (RunFlushLoop), so losing one batch is fine —
	// the next tick carries the backlog. Status is deliberately not
	// checked (accept-all); only a transport error is worth a log line,
	// matching the pre-unification behavior. Return value ignored.
	_ = agent.PostJSON(r.log, url, body, agent.PostOptions{
		Client:   r.client,
		Attempts: 1,
		Name:     "progress report",
		AcceptFn: func(int) bool { return true },
	})
}
