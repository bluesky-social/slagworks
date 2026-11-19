package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestReportCommissionCompleteUsesBoundReportEndpoint(t *testing.T) {
	var got model.ProgressReport
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/report" {
			t.Errorf("path = %q, want /api/v1/report", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode report: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := newAgentReporter(slog.Default(), strings.TrimPrefix(srv.URL, "http://"), "op-1", "machine-1", newLogCollector())
	data := json.RawMessage(`{"system":{"serial_number":"SN-1"}}`)
	if err := r.ReportCommissionComplete(data); err != nil {
		t.Fatalf("ReportCommissionComplete: %v", err)
	}
	if got.OperationID != "op-1" || got.MachineID != "machine-1" || got.Event != "commission_report" {
		t.Fatalf("binding/event = %+v", got)
	}
	if string(got.CommissionData) != string(data) {
		t.Fatalf("commission_data = %s, want %s", got.CommissionData, data)
	}
}

// reportSink is an httptest site worker's /api/v1/report: it records every
// report it receives and answers each with the status respond chooses.
type reportSink struct {
	mu      sync.Mutex
	reports []model.ProgressReport
	respond func(model.ProgressReport) int
}

func (s *reportSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var report model.ProgressReport
	if r.URL.Path != "/api/v1/report" || json.NewDecoder(r.Body).Decode(&report) != nil {
		http.Error(w, "bad report request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.reports = append(s.reports, report)
	status := s.respond(report)
	s.mu.Unlock()
	w.WriteHeader(status)
}

func (s *reportSink) received() []model.ProgressReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.ProgressReport(nil), s.reports...)
}

// newSinkReporter starts a report sink and returns a reporter bound to it
// whose onInactive counts its calls on the returned channel.
func newSinkReporter(t *testing.T, respond func(model.ProgressReport) int) (*agentReporter, *reportSink, chan struct{}) {
	t.Helper()
	sink := &reportSink{respond: respond}
	srv := httptest.NewServer(sink)
	t.Cleanup(srv.Close)
	r := newAgentReporter(slog.New(slog.NewTextHandler(io.Discard, nil)),
		strings.TrimPrefix(srv.URL, "http://"), "op-1", "machine-1", newLogCollector())
	inactive := make(chan struct{}, 16)
	r.onInactive = func() { inactive <- struct{}{} }
	return r, sink, inactive
}

// TestReporter_GoneStopsTheRunOnEveryReportPath is the agent half of the
// regression for an operator abort that did not stop the installer. The
// agent's phase, log and failure reports were fire-and-forget with the
// status deliberately unchecked, so an agent whose deploy had been aborted
// kept partitioning, installing and registering boot entries while every
// report it sent was being refused. 410 Gone from the site worker means the
// operation is over, and every report path must act on it: the reporter
// marks the operation inactive and calls onInactive exactly once.
func TestReporter_GoneStopsTheRunOnEveryReportPath(t *testing.T) {
	gone := func(model.ProgressReport) int { return http.StatusGone }
	paths := []struct {
		name string
		send func(r *agentReporter) error
		// final marks the reports that return an error and normally retry.
		final bool
	}{
		{name: "phase report", send: func(r *agentReporter) error { r.ReportPhase("partitioning"); return nil }},
		{name: "event report", send: func(r *agentReporter) error { r.ReportEvent("some_event"); return nil }},
		{name: "failure report", send: func(r *agentReporter) error { r.ReportFailure("finalizing", "boom"); return nil }},
		{name: "deploy_complete", final: true, send: func(r *agentReporter) error { return r.ReportDeployComplete(nil) }},
		{name: "commission_report", final: true, send: func(r *agentReporter) error {
			return r.ReportCommissionComplete(json.RawMessage(`{}`))
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			r, sink, inactive := newSinkReporter(t, gone)
			start := time.Now()
			err := p.send(r)
			if p.final && err == nil {
				t.Errorf("a final report answered 410 returned nil; the run would carry on to its reboot")
			}
			// A final report normally retries for seconds; a 410 ends that.
			if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
				t.Errorf("the report took %s: a 410 must end the retries at once", elapsed)
			}
			if got := len(sink.received()); got != 1 {
				t.Errorf("site worker saw %d requests, want 1", got)
			}
			if len(inactive) != 1 {
				t.Fatalf("onInactive called %d times, want once: the agent would keep working on an operation that is over", len(inactive))
			}
			if !r.opInactive() {
				t.Error("opInactive() = false after a 410")
			}

			// Whatever the run tries to report next neither reaches the site
			// worker nor signals a second time.
			r.ReportPhase("rsyncing_os")
			if got := len(sink.received()); got != 1 {
				t.Errorf("a report was sent after the operation went inactive (%d requests)", got)
			}
			if len(inactive) != 1 {
				t.Errorf("onInactive called %d times, want once", len(inactive))
			}
		})
	}
}

// TestReporter_LivenessReportFindsAnAbortMidPhase covers the agent that has
// nothing to say: phase reports are minutes apart during the OS extract, and
// the flush loop used to skip a report with no log lines. The loop now sends
// an empty report every interval, and that report learns of the abort.
func TestReporter_LivenessReportFindsAnAbortMidPhase(t *testing.T) {
	aborted := false // guarded by the sink's mutex: respond runs under it
	r, sink, inactive := newSinkReporter(t, func(model.ProgressReport) int {
		if aborted {
			return http.StatusGone
		}
		return http.StatusOK
	})
	r.liveness = 10 * time.Millisecond
	done := make(chan struct{})
	defer close(done)
	go r.RunFlushLoop(done)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	// A healthy run: liveness reports flow, nothing stops.
	waitFor("two liveness reports with nothing to flush", func() bool { return len(sink.received()) >= 2 })
	if len(inactive) != 0 {
		t.Fatal("onInactive fired while the operation was active")
	}
	for _, rep := range sink.received() {
		if rep.OperationID != "op-1" || rep.MachineID != "machine-1" {
			t.Fatalf("liveness report is not bound to the operation: %+v", rep)
		}
	}

	sink.mu.Lock()
	aborted = true
	sink.mu.Unlock()

	select {
	case <-inactive:
	case <-time.After(5 * time.Second):
		t.Fatal("the flush loop never noticed the operation was aborted: a quiet agent would keep installing until its next phase report")
	}
}

// TestReporter_ConflictDoesNotStopTheRun pins that only 410 stops the agent.
// 409 is what a site worker answers when it cannot stand behind "over": it
// restarted and has not resynchronized its operations, for one. Stopping on
// it would fail a healthy deploy every time a worker restarts mid-install.
func TestReporter_ConflictDoesNotStopTheRun(t *testing.T) {
	r, sink, inactive := newSinkReporter(t, func(model.ProgressReport) int { return http.StatusConflict })
	r.ReportPhase("partitioning")
	r.ReportPhase("rsyncing_os")
	r.ReportFailure("rsyncing_os", "boom")
	if len(inactive) != 0 || r.opInactive() {
		t.Fatalf("a 409 stopped the run (onInactive calls: %d)", len(inactive))
	}
	if got := len(sink.received()); got != 3 {
		t.Errorf("site worker saw %d reports, want all 3: a 409 must not silence later reports", got)
	}
}

// TestReporter_GoneAfterAcceptedFinalReportIsExpected covers the commission
// tail. Global completes a commission as soon as it has the report, and the
// site worker drops the operation, while the agent still has a console probe
// to run and a reboot to issue. Reports sent in that tail are answered 410
// because the operation succeeded; the agent must finish, not stop.
func TestReporter_GoneAfterAcceptedFinalReportIsExpected(t *testing.T) {
	completed := false // guarded by the sink's mutex
	r, sink, inactive := newSinkReporter(t, func(rep model.ProgressReport) int {
		if completed {
			return http.StatusGone
		}
		if rep.Event == "commission_report" {
			completed = true
		}
		return http.StatusOK
	})
	r.ReportPhase("reporting")
	if err := r.ReportCommissionComplete(json.RawMessage(`{"system":{}}`)); err != nil {
		t.Fatalf("commission report: %v", err)
	}
	r.ReportPhase("console_probe")
	if len(inactive) != 0 || r.opInactive() {
		t.Fatal("a 410 after the accepted commission report stopped the agent before its console probe and reboot")
	}
	if got := len(sink.received()); got != 3 {
		t.Errorf("site worker saw %d reports, want 3", got)
	}

	// With the final report accepted there is nothing left to learn from a
	// liveness probe, so the flush loop stays quiet unless it has logs.
	r.liveness = 10 * time.Millisecond
	done := make(chan struct{})
	go r.RunFlushLoop(done)
	time.Sleep(100 * time.Millisecond)
	close(done)
	if got := len(sink.received()); got != 3 {
		t.Errorf("flush loop sent %d liveness reports after the final report was accepted, want none", got-3)
	}
}
