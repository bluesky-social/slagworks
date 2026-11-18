package hubhandlers

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opsmetrics"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func metricValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
	metric:
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metric
				}
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue()
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount())
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// TestOpUpdateMetrics pins the operation-shaped metrics (fable-review
// Tier 5): a real phase transition observes the EXITED phase's
// duration on the histogram, an idempotent repeat does not, and
// terminal statuses count outcomes by op type.
func TestOpUpdateMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := opsmetrics.New(reg)

	st := newFake()
	opID := uuid.New()
	st.opByID[opID] = &model.Operation{ID: opID, Type: model.OpDeploy, Status: model.OpRunning}
	st.phases[opID] = "partitioning"

	h := NewOpUpdate(st)
	h.SetMetrics(m)

	send := func(phase, status string) {
		t.Helper()
		err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
			OperationID: opID.String(), Phase: phase, Status: status,
		})
		if err != nil {
			t.Fatalf("op update (phase=%q status=%q): %v", phase, status, err)
		}
	}

	// partitioning -> rsyncing_os: the exited phase (partitioning) is observed.
	send("rsyncing_os", "")
	if got := metricValue(t, reg, "slagworks_op_phase_duration_seconds",
		map[string]string{"op_type": "deploy", "phase": "partitioning"}); got != 1 {
		t.Fatalf("phase histogram: got %v samples for exited phase, want 1", got)
	}
	// Idempotent repeat must NOT observe again.
	send("rsyncing_os", "")
	if got := metricValue(t, reg, "slagworks_op_phase_duration_seconds",
		map[string]string{"op_type": "deploy", "phase": "partitioning"}); got != 1 {
		t.Fatalf("idempotent phase repeat observed the histogram: %v", got)
	}

	send("", string(model.OpCompleted))
	if got := metricValue(t, reg, "slagworks_operation_outcomes_total",
		map[string]string{"op_type": "deploy", "outcome": "completed"}); got != 1 {
		t.Fatalf("outcomes counter: got %v, want 1", got)
	}

	// A second op failing counts on the failed side.
	op2 := uuid.New()
	st.opByID[op2] = &model.Operation{ID: op2, Type: model.OpCommission, Status: model.OpRunning}
	if err := h.HandleOpUpdate(context.Background(), ws.OpUpdateMsg{
		OperationID: op2.String(), Status: string(model.OpFailed),
	}); err != nil {
		t.Fatal(err)
	}
	if got := metricValue(t, reg, "slagworks_operation_outcomes_total",
		map[string]string{"op_type": "commission", "outcome": "failed"}); got != 1 {
		t.Fatalf("failed outcome counter: got %v, want 1", got)
	}
}
