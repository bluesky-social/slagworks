package worker

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// outletPowerOp builds a pending outlet power op (PDURuntime.Outlet=true) for
// the given machine, downstream serial, verb, and AllowSelf flag.
func outletPowerOp(t *testing.T, machineID uuid.UUID, opType model.OperationType, downstreamSerial string, allowSelf bool) model.Operation {
	t.Helper()
	rt := model.PDURuntime{
		Outlets:          []model.PDUOutlet{{PDUHost: "10.0.0.1", Outlet: 5}},
		DownstreamSerial: downstreamSerial,
		Outlet:           true,
		AllowSelf:        allowSelf,
	}
	raw, err := json.Marshal(model.OpParams{PDURuntime: &rt})
	if err != nil {
		t.Fatal(err)
	}
	return model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: opType,
		Status: model.OpPending, Version: 1, Params: raw,
	}
}

// interlockCache builds a cache with the given self-serial, an in-view PDU
// machine, and the trusted-empty op layer ready for a resync.
func interlockCache(t *testing.T, selfSerial string) (*Cache, uuid.UUID) {
	t.Helper()
	c := NewCacheWithSerial(nil, selfSerial)
	pduMachine := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum:   1,
		Machines: []model.MachineSnapshot{{ID: pduMachine, State: model.MachineStateNew}},
	})
	return c, pduMachine
}

// TestWorkerInterlock_SkipsSelfSerialOutletOp: pendingPowerOps skips an outlet
// op whose DownstreamSerial == the worker's own serial (would cut its feed).
func TestWorkerInterlock_SkipsSelfSerialOutletOp(t *testing.T) {
	c, pdu := interlockCache(t, "SN-SELF")
	seedOpsResync(c, outletPowerOp(t, pdu, model.OpPowerCycle, "SN-SELF", false))
	if got := c.PendingPowerOps(); len(got) != 0 {
		t.Errorf("self-serial outlet cycle must be skipped, got %d ops", len(got))
	}
}

// TestWorkerInterlock_SkipsSelfOutletOpAcrossSerialCase: the worker's serial
// is its SMBIOS spelling, the op's DownstreamSerial is NetBox's. Global
// verified the worker as that NetBox device with a case-insensitive match, so
// a case-only difference is still this worker's own feed.
func TestWorkerInterlock_SkipsSelfOutletOpAcrossSerialCase(t *testing.T) {
	for _, opType := range []model.OperationType{model.OpPowerOff, model.OpPowerCycle} {
		t.Run(string(opType), func(t *testing.T) {
			c, pdu := interlockCache(t, "abc123")
			seedOpsResync(c, outletPowerOp(t, pdu, opType, "ABC123", false))
			if got := c.PendingPowerOps(); len(got) != 0 {
				t.Errorf("outlet %s feeding this worker (NetBox %q, SMBIOS %q) must be skipped, got %d ops",
					opType, "ABC123", "abc123", len(got))
			}
		})
	}
}

// TestWorkerInterlock_HonorsAllowSelfForCycle: an AllowSelf cycle on the
// worker's own feed IS returned (the operator chose the self-cycle).
func TestWorkerInterlock_HonorsAllowSelfForCycle(t *testing.T) {
	c, pdu := interlockCache(t, "SN-SELF")
	seedOpsResync(c, outletPowerOp(t, pdu, model.OpPowerCycle, "SN-SELF", true))
	if got := c.PendingPowerOps(); len(got) != 1 {
		t.Errorf("allow-self self-serial cycle must be served, got %d ops", len(got))
	}
}

// TestWorkerInterlock_HonorsAllowSelfForOn: same as cycle, for `on`.
func TestWorkerInterlock_HonorsAllowSelfForOn(t *testing.T) {
	c, pdu := interlockCache(t, "SN-SELF")
	seedOpsResync(c, outletPowerOp(t, pdu, model.OpPowerOn, "SN-SELF", true))
	if got := c.PendingPowerOps(); len(got) != 1 {
		t.Errorf("allow-self self-serial on must be served, got %d ops", len(got))
	}
}

// TestWorkerInterlock_AlwaysSkipsSelfOff: a self-serial `off` is ALWAYS
// skipped, even with AllowSelf set (INV-1: no serving-worker-off path).
func TestWorkerInterlock_AlwaysSkipsSelfOff(t *testing.T) {
	c, pdu := interlockCache(t, "SN-SELF")
	seedOpsResync(c, outletPowerOp(t, pdu, model.OpPowerOff, "SN-SELF", true))
	if got := c.PendingPowerOps(); len(got) != 0 {
		t.Errorf("self-serial off must always be skipped (even AllowSelf), got %d ops", len(got))
	}
}

// TestWorkerInterlock_OtherDownstreamServed: an outlet op feeding a DIFFERENT
// device (not this worker) is served normally.
func TestWorkerInterlock_OtherDownstreamServed(t *testing.T) {
	c, pdu := interlockCache(t, "SN-SELF")
	seedOpsResync(c, outletPowerOp(t, pdu, model.OpPowerCycle, "SN-OTHER", false))
	if got := c.PendingPowerOps(); len(got) != 1 {
		t.Errorf("outlet op for another device must be served, got %d ops", len(got))
	}
}

// TestWorkerInterlock_UnverifiedWorkerInert: a worker with no verified serial
// ("") never matches a downstream, so the skip is inert — a self-serial-shaped
// op is still served (the global interlock is the authority for that case).
func TestWorkerInterlock_UnverifiedWorkerInert(t *testing.T) {
	c, pdu := interlockCache(t, "") // no verified serial
	seedOpsResync(c, outletPowerOp(t, pdu, model.OpPowerCycle, "SN-ANYTHING", false))
	if got := c.PendingPowerOps(); len(got) != 1 {
		t.Errorf("unverified worker: skip must be inert, got %d ops", len(got))
	}
}

// TestWorkerInterlock_NonOutletPowerOpUnaffected: a machine power op with NO
// PDURuntime (BMC power) is unaffected by the worker-side interlock.
func TestWorkerInterlock_NonOutletPowerOpUnaffected(t *testing.T) {
	c, pdu := interlockCache(t, "SN-SELF")
	// A plain power op, no PDURuntime at all.
	op := model.Operation{
		ID: uuid.New(), MachineID: pdu, Type: model.OpPowerCycle,
		Status: model.OpPending, Version: 1,
	}
	seedOpsResync(c, op)
	if got := c.PendingPowerOps(); len(got) != 1 {
		t.Errorf("non-outlet power op must be unaffected, got %d ops", len(got))
	}
}
