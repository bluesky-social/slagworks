package worker

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Differential test: model.InMemoryProvider is the stand-in httpboot's tests
// use for the real MachineProvider (worker.Cache). Wherever the two disagree
// on the same inputs, an httpboot test can pass against behavior production
// does not have. This enumerates every combination of op type, op status,
// op owner, reporting machine, op id and event (bounded exhaustive: the
// domain is small enough to cover completely, so no sampling is needed) and
// compares (a) whether HandleProgressReport accepts the report and (b) what
// DeployOpForMAC / CommissionOpForMAC return.
//
// Two patterns are excluded because the stand-in is known to be looser than
// the cache there (see standInAcceptsAnyPhoneHome and the terminal-status
// skip in the OpForMAC test); every other combination must agree.

const (
	diffMACM = "52:54:00:00:00:0a"
	diffMACO = "52:54:00:00:00:0b"
)

var (
	diffM = uuid.MustParse("00000000-0000-4000-8000-00000000000a")
	diffO = uuid.MustParse("00000000-0000-4000-8000-00000000000b")
)

type diffCase struct {
	opType     model.OperationType
	opStatus   model.OperationStatus
	opMachine  uuid.UUID
	repMachine uuid.UUID
	repKnownOp bool
	event      string
}

func (d diffCase) String() string {
	name := func(u uuid.UUID) string {
		if u == diffM {
			return "M"
		}
		return "O"
	}
	return fmt.Sprintf("op{%s,%s,owner=%s} report{machine=%s,knownOp=%v,event=%q}",
		d.opType, d.opStatus, name(d.opMachine), name(d.repMachine), d.repKnownOp, d.event)
}

func diffCommissionData(mac, serial string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"system": map[string]string{"serial_number": serial, "uuid": ""},
		"nics":   []map[string]string{{"mac": mac, "driver": "ixgbe"}},
	})
	return b
}

func diffReport(d diffCase, opID uuid.UUID) *model.ProgressReport {
	rid := opID
	if !d.repKnownOp {
		rid = uuid.MustParse("00000000-0000-4000-8000-0000000000ff")
	}
	r := &model.ProgressReport{OperationID: rid.String(), MachineID: d.repMachine.String(), Event: d.event}
	if d.event == "" {
		r.Phase = "partitioning"
	}
	if d.event == "commission_report" {
		mac, serial := diffMACM, "SER-M"
		if d.repMachine == diffO {
			mac, serial = diffMACO, "SER-O"
		}
		r.CommissionData = diffCommissionData(mac, serial)
	}
	return r
}

func diffCache(d diffCase, opID uuid.UUID) *Cache {
	c := NewCache(nil)
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Machines: []model.MachineSnapshot{
		{ID: diffM, NetBoxSerial: "SER-M", State: model.MachineStateCommissioned, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: diffMACM, Type: "physical"}}},
		{ID: diffO, NetBoxSerial: "SER-O", State: model.MachineStateCommissioned, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: diffMACO, Type: "physical"}}},
	}})
	seedOpsResync(c, model.Operation{ID: opID, MachineID: d.opMachine, Type: d.opType, Status: d.opStatus})
	return c
}

func diffInMem(d diffCase, opID uuid.UUID) *model.InMemoryProvider {
	p := model.NewInMemoryProvider()
	p.Register(diffMACM, diffM)
	p.Register(diffMACO, diffO)
	op := &model.Operation{ID: opID, MachineID: d.opMachine, Type: d.opType, Status: d.opStatus}
	if d.opType == model.OpDeploy {
		p.SetDeployOp(d.opMachine, op)
	} else {
		p.SetCommissionOp(d.opMachine, op)
	}
	return p
}

func opIDOrNil(op *model.Operation) string {
	if op == nil {
		return "nil"
	}
	return op.ID.String()
}

// standInAcceptsAnyPhoneHome marks the reports on which InMemoryProvider is
// looser than worker.Cache: it accepts a phone_home for the machine's own
// active op whatever the op's type or phase, where the cache requires a
// deploy op at phase awaiting_phone_home (the ops here carry no phase).
func standInAcceptsAnyPhoneHome(d diffCase) bool {
	active := d.opStatus == model.OpClaimed || d.opStatus == model.OpRunning
	return d.event == "phone_home" && d.repKnownOp && d.repMachine == d.opMachine && active
}

func allDiffCases() []diffCase {
	var out []diffCase
	for _, typ := range []model.OperationType{model.OpDeploy, model.OpCommission} {
		for _, st := range []model.OperationStatus{model.OpClaimed, model.OpRunning, model.OpCompleted, model.OpFailed} {
			for _, owner := range []uuid.UUID{diffM, diffO} {
				for _, rm := range []uuid.UUID{diffM, diffO} {
					for _, known := range []bool{true, false} {
						for _, ev := range []string{"", "deploy_complete", "commission_report", "phone_home"} {
							out = append(out, diffCase{typ, st, owner, rm, known, ev})
						}
					}
				}
			}
		}
	}
	return out
}

func TestPropInMemoryProviderMatchesCacheReportBinding(t *testing.T) {
	opID := uuid.MustParse("00000000-0000-4000-8000-0000000000c1")
	var divergent []string
	cases := allDiffCases()
	compared := 0
	for _, d := range cases {
		if standInAcceptsAnyPhoneHome(d) {
			continue
		}
		compared++
		cErr := diffCache(d, opID).HandleProgressReport(diffReport(d, opID))
		mErr := diffInMem(d, opID).HandleProgressReport(diffReport(d, opID))
		if (cErr == nil) != (mErr == nil) {
			divergent = append(divergent, fmt.Sprintf("%v: cache=%v inmem=%v", d, cErr, mErr))
		}
	}
	t.Logf("cases=%d compared=%d divergent=%d", len(cases), compared, len(divergent))
	for _, s := range divergent {
		t.Error(s)
	}
}

func TestPropInMemoryProviderMatchesCacheOpForMAC(t *testing.T) {
	opID := uuid.MustParse("00000000-0000-4000-8000-0000000000c1")
	seen := map[string]bool{}
	for _, d := range allDiffCases() {
		// InMemoryProvider returns whatever op was last set for the machine,
		// completed and failed ones included; the cache returns only active
		// ops. Terminal statuses are therefore not compared.
		if d.opStatus == model.OpCompleted || d.opStatus == model.OpFailed {
			continue
		}
		key := fmt.Sprintf("%s/%s/%v", d.opType, d.opStatus, d.opMachine)
		if seen[key] {
			continue
		}
		seen[key] = true
		c, m := diffCache(d, opID), diffInMem(d, opID)
		for _, mac := range []string{diffMACM, diffMACO, "52:54:00:00:00:99"} {
			if a, b := opIDOrNil(c.DeployOpForMAC(mac)), opIDOrNil(m.DeployOpForMAC(mac)); a != b {
				t.Errorf("%v DeployOpForMAC(%s): cache=%s inmem=%s", d, mac, a, b)
			}
			if a, b := opIDOrNil(c.CommissionOpForMAC(mac)), opIDOrNil(m.CommissionOpForMAC(mac)); a != b {
				t.Errorf("%v CommissionOpForMAC(%s): cache=%s inmem=%s", d, mac, a, b)
			}
		}
	}
}
