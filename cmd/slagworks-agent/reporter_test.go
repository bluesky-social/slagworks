package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
