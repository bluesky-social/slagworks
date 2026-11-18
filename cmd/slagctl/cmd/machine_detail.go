package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/model"
)

// commissionStepView mirrors cmd/slagworks-agent.CommissionStep — duplicated
// here because the agent's main package is binary-only and we don't want to
// pull main into the CLI just for a 5-field struct. Field tags must stay in
// sync with the agent definition.
type commissionStepView struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

// printMachineDetail renders the human-facing view of a single machine.
// Includes the per-step commission table when CommissionData carries a
// `steps` array. Older commission_data without `steps` is treated
// as all-ok and the table is omitted.
func printMachineDetail(w io.Writer, m *model.Machine) error {
	fmt.Fprintf(w, "ID:       %s\n", m.ID)
	fmt.Fprintf(w, "Serial:   %s\n", m.NetBoxSerial)
	fmt.Fprintf(w, "State:    %s\n", format.MachineStateLabel(string(m.State)))
	if m.DesiredOS != "" {
		fmt.Fprintf(w, "Desired:  %s\n", m.DesiredOS)
	}
	if m.CurrentOS != "" {
		fmt.Fprintf(w, "Current:  %s\n", m.CurrentOS)
	}
	if m.NetBoxDeviceID != nil {
		fmt.Fprintf(w, "NetBox:   device #%d\n", *m.NetBoxDeviceID)
	}
	if m.Locked {
		fmt.Fprintf(w, "Locked:   %s\n", format.WarnStyle.Render("yes"))
	}
	fmt.Fprintf(w, "PXE:      %t\n", m.PXEEligible)
	fmt.Fprintf(w, "Created:  %s\n", format.HumanDuration(m.CreatedAt))
	if m.CommissionedAt != nil {
		fmt.Fprintf(w, "Commissioned: %s\n", format.HumanDuration(*m.CommissionedAt))
	}

	if len(m.CommissionData) > 0 {
		printCommissionSteps(w, m.CommissionData)
	}
	return nil
}

// printCommissionSteps extracts and renders the steps array from raw
// commission_data JSON. Silently no-ops when the JSON is unparseable or
// missing the `steps` field — older commissions persisted before
// the steps array existed don't have it, and a malformed blob is the agent's bug to
// surface elsewhere, not the operator's view to crash on.
func printCommissionSteps(w io.Writer, raw json.RawMessage) {
	var wrapper struct {
		Steps []commissionStepView `json:"steps"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return
	}
	if len(wrapper.Steps) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, format.HeaderStyle.Render("Commission steps"))
	rows := make([][]string, len(wrapper.Steps))
	for i, s := range wrapper.Steps {
		detail := s.Error
		if detail == "" {
			detail = s.Warning
		}
		rows[i] = []string{
			s.Name,
			renderStepStatus(s.Status),
			strconv.FormatInt(s.DurationMs, 10) + "ms",
			detail,
		}
	}
	format.PrintTable(w, []string{"STEP", "STATUS", "DURATION", "DETAIL"}, rows)
}

func renderStepStatus(status string) string {
	switch status {
	case "ok":
		return format.StateReady.Render(format.IconReady + " ok")
	case "partial":
		return format.WarnStyle.Render("◐ partial")
	case "failed":
		return format.StateFailed.Render(format.IconFailed + " failed")
	default:
		return status
	}
}
