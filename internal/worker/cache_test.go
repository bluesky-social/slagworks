package worker

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// snapMachine returns a SiteSnapshot containing one machine with one
// physical interface. A helper kept small so each test reads linearly.
func snapMachine(id uuid.UUID, mac string, state model.MachineState) *model.SiteSnapshot {
	return &model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{
				ID:          id,
				State:       state,
				PXEEligible: true,
				Interfaces: []model.SnapshotInterface{
					{Name: "eth0", MAC: mac, Type: "physical"},
				},
			},
		},
	}
}

// TestActiveBootingOpForMachine covers the helper: it returns the
// active deploy OR commission op (both boot the machine), and skips terminal
// ops and power ops (which don't boot through the phase spine).
func TestActiveBootingOpForMachine(t *testing.T) {
	id := uuid.New()
	mac := "52:54:00:bo:ot:01"

	t.Run("commission op is booting", func(t *testing.T) {
		c := NewCache(nil)
		opID := uuid.New()
		snap := snapMachine(id, mac, model.MachineStateCommissioning)
		c.ApplySnapshot(snap)
		seedOpsResync(c, []model.Operation{
			{ID: opID, MachineID: id, Type: model.OpCommission, Status: model.OpClaimed},
		}...)
		op := c.ActiveBootingOpForMachine(id)
		if op == nil || op.ID != opID {
			t.Fatalf("expected commission op %s, got %+v", opID, op)
		}
	})

	t.Run("deploy op is booting", func(t *testing.T) {
		c := NewCache(nil)
		opID := uuid.New()
		snap := snapMachine(id, mac, model.MachineStateDeploying)
		c.ApplySnapshot(snap)
		seedOpsResync(c, []model.Operation{
			{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpRunning},
		}...)
		op := c.ActiveBootingOpForMachine(id)
		if op == nil || op.ID != opID {
			t.Fatalf("expected deploy op %s, got %+v", opID, op)
		}
	})

	t.Run("power op is not booting", func(t *testing.T) {
		c := NewCache(nil)
		snap := snapMachine(id, mac, model.MachineStateRunning)
		c.ApplySnapshot(snap)
		seedOpsResync(c, []model.Operation{
			{ID: uuid.New(), MachineID: id, Type: model.OpPowerCycle, Status: model.OpClaimed},
		}...)
		if op := c.ActiveBootingOpForMachine(id); op != nil {
			t.Fatalf("power op should not be a booting op, got %+v", op)
		}
	})

	t.Run("terminal op is not active", func(t *testing.T) {
		c := NewCache(nil)
		snap := snapMachine(id, mac, model.MachineStateCommissioned)
		c.ApplySnapshot(snap)
		seedOpsResync(c, []model.Operation{
			{ID: uuid.New(), MachineID: id, Type: model.OpCommission, Status: model.OpCompleted},
		}...)
		if op := c.ActiveBootingOpForMachine(id); op != nil {
			t.Fatalf("completed op should not be active, got %+v", op)
		}
	})
}

func TestCacheBootAction_UnknownMAC_MustNotDeploy(t *testing.T) {
	// SAFETY: an unknown MAC must never receive BootDeploy. If slagworks
	// doesn't recognize the MAC, it must not PXE boot the machine at
	// all — otherwise an unregistered server could get wiped.
	c := NewCache(nil)
	got := c.BootActionForMAC("52:54:00:aa:bb:01")
	if got == model.BootDeploy || got == model.BootCommission {
		t.Fatalf("SAFETY: unknown MAC returned PXE action (%d)", got)
	}
	if got != model.BootLocal {
		t.Fatalf("expected BootLocal for unknown MAC, got %d", got)
	}
}

func TestCacheBootAction_AfterSnapshot(t *testing.T) {
	// Machine state alone no longer drives the boot action — pending ops
	// matter too. A `new` machine without a pending commission op boots
	// local (auto-commission in the httpboot handler is what creates the
	// op). A `commissioned` machine with a pending deploy op boots
	// BootDeploy.
	c := NewCache(nil)
	id1, id2, id3 := uuid.New(), uuid.New(), uuid.New()
	depOpID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{ID: id1, State: model.MachineStateRunning, PXEEligible: true,
				Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:01", Type: "physical"}}},
			{ID: id2, State: model.MachineStateCommissioned, PXEEligible: true,
				Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:02", Type: "physical"}}},
			{ID: id3, State: model.MachineStateCommissioned, PXEEligible: false,
				Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:03", Type: "physical"}}},
		}})
	seedOpsResync(c, []model.Operation{
		{ID: depOpID, MachineID: id2, Type: model.OpDeploy, Status: model.OpPending},
	}...)

	if got := c.BootActionForMAC("52:54:00:aa:bb:01"); got != model.BootLocal {
		t.Fatalf("running MAC: got %d, want BootLocal", got)
	}
	if got := c.BootActionForMAC("52:54:00:aa:bb:02"); got != model.BootDeploy {
		t.Fatalf("commissioned + pending deploy: got %d, want BootDeploy", got)
	}
	if got := c.BootActionForMAC("52:54:00:aa:bb:03"); got != model.BootLocal {
		t.Fatalf("pxe-ineligible MAC: got %d, want BootLocal", got)
	}
}

// TestCacheBootAction_StateMatrix exercises the new boot-action decision
// table: state + active op type + phase. The forward path tests `new`
// (pending commission), `commissioned` (pending deploy), `deploying`
// (with and without awaiting_phone_home), `running` (no ops),
// transient states (already claimed), and `failed` (retry paths).
func TestCacheBootAction_StateMatrix(t *testing.T) {
	cases := []struct {
		name     string
		state    model.MachineState
		opType   model.OperationType // zero means "no active op"
		opStatus model.OperationStatus
		opPhase  string
		want     model.BootAction
	}{
		{"new no ops", model.MachineStateNew, "", "", "", model.BootLocal},
		{"new + pending commission", model.MachineStateNew, model.OpCommission, model.OpPending, "", model.BootCommission},
		{"commissioning (claimed)", model.MachineStateCommissioning, model.OpCommission, model.OpClaimed, "", model.BootCommission},
		{"commissioned no ops", model.MachineStateCommissioned, "", "", "", model.BootLocal},
		{"commissioned + pending deploy", model.MachineStateCommissioned, model.OpDeploy, model.OpPending, "", model.BootDeploy},
		{"deploying agent-side", model.MachineStateDeploying, model.OpDeploy, model.OpClaimed, "partitioning", model.BootDeploy},
		{"deploying awaiting_phone_home", model.MachineStateDeploying, model.OpDeploy, model.OpClaimed, "awaiting_phone_home", model.BootLocal},
		{"running no ops", model.MachineStateRunning, "", "", "", model.BootLocal},
		{"failed + pending commission", model.MachineStateFailed, model.OpCommission, model.OpPending, "", model.BootCommission},
		{"failed + pending deploy", model.MachineStateFailed, model.OpDeploy, model.OpPending, "", model.BootDeploy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCache(nil)
			id := uuid.New()
			snap := snapMachine(id, "aa:aa:aa:aa:aa:aa", tc.state)
			c.ApplySnapshot(snap)
			if tc.opType != "" {
				seedOpsResync(c, model.Operation{
					ID: uuid.New(), MachineID: id, Type: tc.opType, Status: tc.opStatus, Phase: tc.opPhase,
				})
			}
			if got := c.BootActionForMAC("aa:aa:aa:aa:aa:aa"); got != tc.want {
				t.Fatalf("state %s op %s/%s phase %q: got %d, want %d",
					tc.state, tc.opType, tc.opStatus, tc.opPhase, got, tc.want)
			}
		})
	}
}

// TestUpdateOpPhase_AwaitingPhoneHomeNotClobbered is a
// regression test: once a deploy op is awaiting phone-home, the host is
// (re)booting its installed disk. Its firmware-PXE DHCP (OnACK ->
// dhcp_ack) and the boot-script handler (ipxe_loaded) must NOT knock the
// op off awaiting_phone_home, because BootActionForMAC keys BootLocal on
// that phase — a clobber re-serves the deploy initrd and the host
// re-wipes its disk in a loop.
func TestUpdateOpPhase_AwaitingPhoneHomeNotClobbered(t *testing.T) {
	c := NewCache(nil)
	mid := uuid.New()
	opID := uuid.New()
	mac := "52:54:00:ab:cd:ef"
	snap := snapMachine(mid, mac, model.MachineStateDeploying)
	c.ApplySnapshot(snap)
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: mid, Type: model.OpDeploy, Status: model.OpClaimed, Phase: "awaiting_phone_home"},
	}...)

	// Sanity: the install is done, post-reboot PXE should boot-local.
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("precondition: want BootLocal in awaiting_phone_home, got %d", got)
	}

	// Simulate the post-install reboot's firmware DHCP + boot-script
	// fetch, which call UpdateOpPhase with boot-prefix phases.
	for _, p := range []string{"pxe_booting", "dhcp_ack", "ipxe_dhcp", "ipxe_loaded"} {
		c.UpdateOpPhase(opID.String(), p)
		if got := c.BootActionForMAC(mac); got != model.BootLocal {
			t.Fatalf("after UpdateOpPhase(%q): BootAction = %d, want BootLocal (phase must stay awaiting_phone_home)", p, got)
		}
	}

	// And a non-boot-prefix terminal phase (phone_home / completed-ish)
	// must still be allowed through — the guard is scoped to boot-prefix
	// phases only.
	c.UpdateOpPhase(opID.String(), "phone_home")
	if op := c.ActiveBootingOpForMachine(mid); op != nil && op.Phase != "phone_home" {
		t.Fatalf("non-boot-prefix phase must update: got %q, want phone_home", op.Phase)
	}
}

// TestUpdateOpPhase_BootPrefixAllowedWhenNotAwaiting confirms the guard is
// narrow: a deploy op NOT in awaiting_phone_home (e.g. mid-install) accepts
// boot-prefix phase updates normally.
func TestUpdateOpPhase_BootPrefixAllowedWhenNotAwaiting(t *testing.T) {
	c := NewCache(nil)
	mid := uuid.New()
	opID := uuid.New()
	snap := snapMachine(mid, "52:54:00:11:22:33", model.MachineStateDeploying)
	c.ApplySnapshot(snap)
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: mid, Type: model.OpDeploy, Status: model.OpClaimed, Phase: "pxe_booting"},
	}...)

	c.UpdateOpPhase(opID.String(), "dhcp_ack")
	if op := c.ActiveBootingOpForMachine(mid); op == nil || op.Phase != "dhcp_ack" {
		t.Fatalf("boot-prefix phase should update a non-awaiting op; got %v", op)
	}
}

// TestCacheApplySnapshot_DroppedMachineMACDoesNotLinger is the critical
// safety property for the full-replace apply semantics. If a machine is
// removed from the snapshot (Interfaces cleared, or absent entirely),
// its old MAC must no longer resolve to a machine — otherwise DHCP/PXE
// would still ack the MAC and potentially serve a stale boot action.
func TestCacheApplySnapshot_DroppedMachineMACDoesNotLinger(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "52:54:00:de:ad:01"
	depOpID := uuid.New()

	// Pre: machine is commissioned with a pending deploy op, so MAC
	// resolves to BootDeploy.
	snap := snapMachine(id, mac, model.MachineStateCommissioned)
	c.ApplySnapshot(snap)
	seedOpsResync(c, []model.Operation{
		{ID: depOpID, MachineID: id, Type: model.OpDeploy, Status: model.OpPending},
	}...)
	if got := c.BootActionForMAC(mac); got != model.BootDeploy {
		t.Fatalf("pre-drop: got %d, want BootDeploy", got)
	}

	// Drop: machine snapshot has no interfaces.
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateRunning}},
	})
	if got := c.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("post-drop: got %d, want BootLocal (stale MAC must not PXE)", got)
	}
	if _, ok := c.MachineIDForMAC(mac); ok {
		t.Fatal("post-drop: MAC still resolves to machine via MachineIDForMAC")
	}
}

func TestCacheApplySnapshot_MACMovedToDifferentMachine(t *testing.T) {
	c := NewCache(nil)
	id1, id2 := uuid.New(), uuid.New()
	mac := "aa:aa:aa:aa:aa:bb"

	c.ApplySnapshot(snapMachine(id1, mac, model.MachineStateCommissioned))
	if got, _ := c.MachineIDForMAC(mac); got != id1 {
		t.Fatalf("first apply: got %s, want %s", got, id1)
	}

	// MAC reassigned (rare but possible — NIC moved between chassis).
	c.ApplySnapshot(snapMachine(id2, mac, model.MachineStateCommissioned))
	if got, _ := c.MachineIDForMAC(mac); got != id2 {
		t.Fatalf("after reassign: got %s, want %s", got, id2)
	}
}

func TestCacheApplySnapshot_DuplicateMACAcrossMachinesFailsClosed(t *testing.T) {
	c := NewCache(nil)
	firstID, secondID, thirdID := uuid.New(), uuid.New(), uuid.New()
	sharedMAC := "be:3a:f2:b6:05:9f"

	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{ID: firstID, Interfaces: []model.SnapshotInterface{{MAC: sharedMAC, Type: "physical", IP: "10.0.0.11/24"}}},
			{ID: secondID, Interfaces: []model.SnapshotInterface{{MAC: sharedMAC, Type: "physical", IP: "10.0.0.12/24"}}},
			// A third occurrence proves ambiguity is sticky within the whole
			// snapshot rather than allowing the last machine to win again.
			{ID: thirdID, Interfaces: []model.SnapshotInterface{{MAC: sharedMAC, Type: "physical", IP: "10.0.0.13/24"}}},
		},
	})

	if got, ok := c.MachineIDForMAC(sharedMAC); ok {
		t.Fatalf("duplicate MAC resolved to %s; want fail-closed unknown", got)
	}
	if got := c.MachineByMAC(sharedMAC); got != nil {
		t.Fatalf("duplicate MAC resolved to machine %+v", got)
	}
	if got := c.ReservedIPForMAC(sharedMAC); got != nil {
		t.Fatalf("duplicate MAC received arbitrary reservation %s", got)
	}
}

// TestCacheMultiMAC_PhysicalResolve_IPMIExcluded pins that every
// physical NIC MAC resolves to its machine, but a BMC/mgmt_only (ipmi)
// MAC must NOT — a BMC is never a host-OS provisioning DHCP client, and
// the non-promiscuous lease filter (KnownMACs == MachineByMAC != nil)
// would otherwise offer it a host-fabric lease on the wrong network.
func TestCacheMultiMAC_PhysicalResolve_IPMIExcluded(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{
			ID: id, State: model.MachineStateNew, PXEEligible: true,
			Interfaces: []model.SnapshotInterface{
				{Name: "eth0", MAC: "aa:bb:cc:00:00:01", Type: "physical"},
				{Name: "eth1", MAC: "aa:bb:cc:00:00:02", Type: "physical"},
				{Name: "ipmi", MAC: "aa:bb:cc:00:00:03", Type: "ipmi"},
			},
		}},
	})
	// Physical NIC MACs resolve and are known to the lease filter.
	for _, mac := range []string{"aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02"} {
		if got, ok := c.MachineIDForMAC(mac); !ok || got != id {
			t.Fatalf("physical %s: got (%s, %v), want (%s, true)", mac, got, ok, id)
		}
		if c.MachineByMAC(mac) == nil {
			t.Fatalf("physical %s: MachineByMAC returned nil; lease filter would drop it", mac)
		}
	}
	// The ipmi MAC must be invisible to MAC resolution: no lease, no
	// machine_id-tagged offer.
	if got, ok := c.MachineIDForMAC("aa:bb:cc:00:00:03"); ok {
		t.Fatalf("ipmi MAC resolved to %s; BMC must not be a known DHCP client", got)
	}
	if c.MachineByMAC("aa:bb:cc:00:00:03") != nil {
		t.Fatal("ipmi MAC: MachineByMAC non-nil; lease filter would offer the BMC a host lease")
	}
}

func TestCacheReservedIPForMAC_PhysicalOnly(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{
			ID: id, PXEEligible: true, State: model.MachineStateCommissioned,
			Interfaces: []model.SnapshotInterface{
				{Name: "eth0", MAC: "aa:bb:cc:dd:ee:01", Type: "physical", IP: "10.0.0.42/24"},
				{Name: "eth1", MAC: "aa:bb:cc:dd:ee:02", Type: "physical", IP: "10.0.0.43"},
				{Name: "ipmi", MAC: "aa:bb:cc:dd:ee:03", Type: "ipmi", IP: "10.0.0.100"},
			},
		}},
	})

	// CIDR notation resolves to bare IP.
	if got := c.ReservedIPForMAC("aa:bb:cc:dd:ee:01"); !got.Equal(net.ParseIP("10.0.0.42")) {
		t.Fatalf("CIDR: got %v, want 10.0.0.42", got)
	}
	// Bare IP passes through.
	if got := c.ReservedIPForMAC("aa:bb:cc:dd:ee:02"); !got.Equal(net.ParseIP("10.0.0.43")) {
		t.Fatalf("bare: got %v, want 10.0.0.43", got)
	}
	// IPMI excluded.
	if got := c.ReservedIPForMAC("aa:bb:cc:dd:ee:03"); got != nil {
		t.Fatalf("IPMI: got %v, want nil", got)
	}
	// Unknown MAC.
	if got := c.ReservedIPForMAC("ff:ff:ff:ff:ff:ff"); got != nil {
		t.Fatalf("unknown: got %v, want nil", got)
	}
}

func TestCacheLastSeqNum(t *testing.T) {
	c := NewCache(nil)
	if got := c.LastSeqNum(); got != 0 {
		t.Fatalf("initial: got %d, want 0", got)
	}
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 42})
	if got := c.LastSeqNum(); got != 42 {
		t.Fatalf("after apply: got %d, want 42", got)
	}
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 99})
	if got := c.LastSeqNum(); got != 99 {
		t.Fatalf("after second apply: got %d, want 99", got)
	}
}

// TestCacheRecordDeployComplete pins the new (commit 3) semantic:
// deploy_complete does NOT advance machine state. The machine stays
// in `deploying` with phase=awaiting_phone_home until the installed
// OS phones home. The op stays claimed.
func TestCacheRecordDeployComplete(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateDeploying))

	c.recordDeployComplete(id, uuid.New(), nil)

	m := c.MachineByID(id)
	if m == nil || m.State != model.MachineStateDeploying {
		t.Fatalf("local state should stay deploying until phone-home, got %+v", m)
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "deploy_complete" {
		t.Fatalf("outbox: got %+v", obs)
	}
}

// TestCacheRecordPhoneHome_CompletesDeployOp pins the new (commit 3)
// canonical deploy completion path: phone-home advances local state
// to running AND queues an op_update with status=completed for the
// active deploy op, which transitions the global op to completed
// and the machine to running on the next push.
func TestCacheRecordPhoneHome_CompletesDeployOp(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	opID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateDeploying}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpClaimed},
	}...)

	c.RecordPhoneHome(id)

	m := c.MachineByID(id)
	if m == nil || m.State != model.MachineStateRunning {
		t.Fatalf("phone-home should advance state to running, got %+v", m)
	}
	obs, updates := c.DrainOutbox()
	foundPhoneHome := false
	for _, o := range obs {
		if o.Obs.Event == "phone_home" {
			foundPhoneHome = true
		}
	}
	if !foundPhoneHome {
		t.Errorf("phone_home obs not queued: %+v", obs)
	}
	if len(updates) != 1 || updates[0].OperationID != opID.String() ||
		updates[0].Status != string(model.OpCompleted) {
		t.Errorf("op_update completed not queued, got: %+v", updates)
	}
}

// TestCacheRecordPhoneHome_NoActiveOpStillEmitsObs covers the late-
// phone-home case (op already failed/timed-out). The observation
// still flows; no op_update is queued.
func TestCacheRecordPhoneHome_NoActiveOpStillEmitsObs(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateDeploying}},
	})

	c.RecordPhoneHome(id)

	obs, updates := c.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "phone_home" {
		t.Fatalf("phone_home obs missing: %+v", obs)
	}
	if len(updates) != 0 {
		t.Errorf("no active op → no op_update expected, got: %+v", updates)
	}
}

// TestRecordPhoneHome_CompletionSurvivesRestart pins that the deploy's
// completion is durable once the worker has answered the phone-home. The
// installed OS stops phoning home after its first 2xx, so if the only
// record of the completion is the memory-only op_update, a worker restart
// before the drain loop sends it loses the completion for good: global
// keeps the op at awaiting_phone_home and fails the healthy host at
// PhoneHomeTimeout. The phone_home observation is persisted in the SQLite
// outbox, so it must name the deploy op for global to complete it from.
func TestRecordPhoneHome_CompletionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	id, opID := uuid.New(), uuid.New()
	c := NewCache(db)
	c.ApplySnapshot(snapMachine(id, "52:54:00:e1:00:01", model.MachineStateDeploying))
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy,
		Status: model.OpClaimed, Phase: "awaiting_phone_home",
	})

	// The installed OS phones home; a nil return is what gives it its 200.
	if err := c.HandleProgressReport(&model.ProgressReport{
		MachineID: id.String(), OperationID: opID.String(), Event: "phone_home",
	}); err != nil {
		t.Fatalf("phone_home: %v", err)
	}

	// The worker restarts before the drain loop sends anything.
	closeLocalDB(t, db)
	db2, err := OpenLocalDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeLocalDB(t, db2) })
	obs, updates := NewCache(db2).DrainOutbox()

	for _, p := range obs {
		if p.Obs.Event == "phone_home" && p.Obs.OperationID == opID && p.Obs.MachineID == id {
			return
		}
	}
	t.Fatalf("after a restart no durable phone_home observation names deploy op %s (observations %+v, op updates %+v): "+
		"global never learns the host phoned home and fails the deploy at PhoneHomeTimeout", opID, obs, updates)
}

// TestHandleProgressReport_DeployComplete_WithBMCCreds verifies the
// site worker's cache plumbs BMCCreds through to the
// deploy_complete observation's Data field. The observation flows
// to global on the next outbox push, where the monitor cred is
// upserted into pop/<pop>/bmc-creds-monitor.
func TestHandleProgressReport_DeployComplete_WithBMCCreds(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateDeploying))
	opID := uuid.New()
	seedOpsResync(c, model.Operation{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpRunning})

	report := &model.ProgressReport{
		OperationID: opID.String(),
		MachineID:   id.String(),
		Event:       "deploy_complete",
		BMCCreds: &model.BMCCreds{
			Monitor: &model.BMCUserCred{
				Username: "monitor", Password: "DEF456UVW0", IPMIUserSlot: 4,
			},
			BootOrderPersisted: true,
			LeftoversDisabled:  []string{"maas"},
		},
	}
	if err := c.HandleProgressReport(report); err != nil {
		t.Fatalf("HandleProgressReport: %v", err)
	}

	obs, _ := c.DrainOutbox()
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].Obs.Event != "deploy_complete" {
		t.Fatalf("event: got %q want deploy_complete", obs[0].Obs.Event)
	}
	if len(obs[0].Obs.Data) == 0 {
		t.Fatal("Data: empty; expected marshaled BMCCreds")
	}
	var got model.BMCCreds
	if err := json.Unmarshal(obs[0].Obs.Data, &got); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	if got.Monitor == nil || got.Monitor.Username != "monitor" || got.Monitor.Password != "DEF456UVW0" || got.Monitor.IPMIUserSlot != 4 {
		t.Errorf("Monitor round-trip: got %+v", got.Monitor)
	}
	if !got.BootOrderPersisted {
		t.Error("BootOrderPersisted: lost in round-trip")
	}
	if len(got.LeftoversDisabled) != 1 || got.LeftoversDisabled[0] != "maas" {
		t.Errorf("LeftoversDisabled: got %v want [maas]", got.LeftoversDisabled)
	}
}

// TestHandleProgressReport_DeployComplete_NoBMCCreds covers the
// no-rotation re-deploy shape: deploy_complete with no creds (e.g. a
// failed reconcile that still completed deploy, or a deploy that
// skipped reconcile entirely). The observation goes out with an
// empty Data field; global-side handler should treat absent as
// "no creds to upsert."
func TestHandleProgressReport_DeployComplete_NoBMCCreds(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateDeploying))
	opID := uuid.New()
	seedOpsResync(c, model.Operation{ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpRunning})

	report := &model.ProgressReport{
		OperationID: opID.String(),
		MachineID:   id.String(),
		Event:       "deploy_complete",
	}
	if err := c.HandleProgressReport(report); err != nil {
		t.Fatalf("HandleProgressReport: %v", err)
	}

	obs, _ := c.DrainOutbox()
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].Obs.Event != "deploy_complete" {
		t.Fatalf("event: got %q want deploy_complete", obs[0].Obs.Event)
	}
	if len(obs[0].Obs.Data) != 0 {
		t.Errorf("Data: got %q, want empty", string(obs[0].Obs.Data))
	}
}

func TestCachePhoneHome(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateRunning))

	c.RecordPhoneHome(id)

	m := c.MachineByID(id)
	if m == nil || m.State != model.MachineStateRunning {
		t.Fatalf("local state: got %+v, want Verified", m)
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "phone_home" {
		t.Fatalf("outbox: got %+v", obs)
	}
}

func TestCacheRecordCommissionReport_QueuesOutboxKeepsState(t *testing.T) {
	// Plan 2 moves state transitions to global: the worker enqueues a
	// commission_report observation but does NOT flip state to Ready.
	// The next snapshot push carries the authoritative state change.
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateCommissioning))

	data := json.RawMessage(`{"cpu":"test"}`)
	c.recordCommissionReport(id, uuid.New(), data)

	m := c.MachineByID(id)
	if m == nil || m.State != model.MachineStateCommissioning {
		t.Fatalf("state: got %+v, want unchanged (commissioning)", m)
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "commission_report" {
		t.Fatalf("outbox: got %+v", obs)
	}
	if string(obs[0].Obs.Data) != string(data) {
		t.Fatalf("data: got %s, want %s", obs[0].Obs.Data, data)
	}
}

func TestCacheSystemUUID_FromSnapshot(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{
			ID:         id,
			State:      model.MachineStateCommissioned,
			SystemUUID: "01234567-89ab-cdef-0123-456789abcdef",
			Interfaces: []model.SnapshotInterface{{MAC: "aa:aa:aa:00:00:01", Type: "physical"}},
		}},
	})

	if m := c.MachineByMAC("aa:aa:aa:00:00:01"); m == nil || m.SystemUUID != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatalf("got %+v", m)
	}
	if m := c.MachineByMAC("ff:ff:ff:ff:ff:ff"); m != nil {
		t.Fatalf("unknown MAC: got %+v, want nil", m)
	}
}

func TestCacheStorageScript_FromSnapshot(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{
			ID:    id,
			State: model.MachineStateCommissioned,
			StorageScript: &model.SnapshotStorageScript{
				Name:    "100-asus-rs501a",
				Content: "#!/usr/bin/env python3\n...",
			},
			Interfaces: []model.SnapshotInterface{{MAC: "aa:aa:aa:00:00:01", Type: "physical"}},
		}},
	})

	byMAC := c.MachineByMAC("aa:aa:aa:00:00:01")
	if byMAC == nil || byMAC.StorageScript == nil || byMAC.StorageScript.Name != "100-asus-rs501a" {
		t.Fatalf("got %+v", byMAC)
	}
	if c.MachineByMAC("ff:ff:ff:ff:ff:ff") != nil {
		t.Fatal("unknown MAC: expected nil")
	}

	byID := c.MachineByID(id)
	if byID == nil || byID.StorageScript == nil || byID.StorageScript.Name != "100-asus-rs501a" {
		t.Fatalf("by-ID: got %+v", byID)
	}
}

func TestCacheApplySnapshot_OutboxPreserved(t *testing.T) {
	// Outbox is local-only state and must survive snapshot apply. A
	// snapshot push that races a pending observation must not drop the
	// observation.
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateDeploying))
	c.recordDeployComplete(id, uuid.New(), nil)

	// Apply another snapshot — outbox must survive.
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateDeploying))

	obs, _ := c.DrainOutbox()
	if len(obs) != 1 || obs[0].Obs.Event != "deploy_complete" {
		t.Fatalf("outbox not preserved across apply: got %+v", obs)
	}
}

func TestCacheApplySnapshot_PhaseTrackingPreserved(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	op := model.Operation{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning}

	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID, State: model.MachineStateDeploying}}})
	seedOpsResync(c, []model.Operation{op}...)
	c.UpdateOpPhase(opID.String(), "partitioning")
	c.MarkOpFailed(opID.String(), "rsyncing_os", "disk full")

	// Fresh snapshot with same op — failure marker must carry over so
	// UpdateOpPhase stays blocked.
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID, State: model.MachineStateDeploying}}})
	seedOpsResync(c, []model.Operation{op}...)
	if !c.IsOpFailed(opID.String()) {
		t.Fatal("failure marker lost across snapshot apply")
	}
}

func TestCacheOperationQueries(t *testing.T) {
	c := NewCache(nil)
	machineID := uuid.New()

	ops := []model.Operation{
		{ID: uuid.New(), MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending},
		{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpPending},
		{ID: uuid.New(), MachineID: machineID, Type: model.OpCommission, Status: model.OpRunning}, // not pending
		{ID: uuid.New(), MachineID: machineID, Type: model.OpPowerOff, Status: model.OpCompleted}, // not pending
	}
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, ops...)

	if got := len(c.PendingPowerOps()); got != 1 {
		t.Fatalf("power: got %d, want 1", got)
	}
	if got := len(c.PendingLifecycleOps()); got != 1 {
		t.Fatalf("lifecycle: got %d, want 1", got)
	}
}

func TestCacheActiveDeployOpForMachine(t *testing.T) {
	c := NewCache(nil)
	machineID := uuid.New()
	activeOp := uuid.New()

	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, []model.Operation{
		{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpCompleted},
		{ID: activeOp, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning},
		{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpFailed},
	}...)

	got := c.ActiveDeployOpForMachine(machineID)
	if got == nil || got.ID != activeOp {
		t.Fatalf("got %+v, want op %s", got, activeOp)
	}
}

func TestCacheDeployOpForMAC(t *testing.T) {
	c := NewCache(nil)
	machineID := uuid.New()
	opID := uuid.New()
	mac := "aa:aa:aa:00:00:01"

	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{
			ID: machineID, State: model.MachineStateDeploying,
			Interfaces: []model.SnapshotInterface{{MAC: mac, Type: "physical"}},
		}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning},
	}...)

	got := c.DeployOpForMAC(mac)
	if got == nil || got.ID != opID {
		t.Fatalf("got %+v, want op %s", got, opID)
	}
}

func TestCacheFailedOpsLockout(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()

	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning},
	}...)

	c.MarkOpFailed(opID.String(), "partitioning", "disk full")
	c.UpdateOpPhase(opID.String(), "rsyncing_os") // should be blocked

	op := c.GetOperation(opID.String())
	if op == nil {
		t.Fatal("op not found")
	}
	if op.Phase != "partitioning" {
		t.Fatalf("phase: got %q, want partitioning (lockout failed)", op.Phase)
	}
	if op.Status != model.OpFailed {
		t.Fatalf("status: got %s, want failed", op.Status)
	}
}

func TestCacheHandleProgressReport_Phase(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning},
	}...)

	if err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(),
		MachineID:   machineID.String(),
		Phase:       "rsyncing_os",
	}); err != nil {
		t.Fatalf("HandleProgressReport: %v", err)
	}

	op := c.GetOperation(opID.String())
	if op == nil || op.Phase != "rsyncing_os" {
		t.Fatalf("phase: got %+v", op)
	}
}

func TestCacheHandleProgressReport_RejectsMachineOperationMismatch(t *testing.T) {
	c := NewCache(nil)
	wantMachine, otherMachine, opID := uuid.New(), uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{
		{ID: wantMachine},
		{ID: otherMachine},
	}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: otherMachine, Type: model.OpCommission, Status: model.OpRunning,
	})

	err := c.HandleProgressReport(&model.ProgressReport{
		OperationID:    opID.String(),
		MachineID:      wantMachine.String(),
		Event:          "commission_report",
		CommissionData: json.RawMessage(`{"system":{"serial_number":"SN-WRONG"}}`),
	})
	if err == nil {
		t.Fatal("expected mismatched machine/op binding to be rejected")
	}
	obs, updates := c.DrainOutbox()
	if len(obs) != 0 || len(updates) != 0 {
		t.Fatalf("rejected report produced side effects: observations=%+v updates=%+v", obs, updates)
	}
}

func TestCacheHandleProgressReport_RejectsWrongOperationType(t *testing.T) {
	c := NewCache(nil)
	machineID, opID := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning,
	})

	err := c.HandleProgressReport(&model.ProgressReport{
		OperationID:    opID.String(),
		MachineID:      machineID.String(),
		Event:          "commission_report",
		CommissionData: json.RawMessage(`{"system":{"serial_number":"SN-1"}}`),
	})
	if err == nil {
		t.Fatal("expected commission report on deploy operation to be rejected")
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 0 {
		t.Fatalf("rejected report queued observations: %+v", obs)
	}
}

func TestCacheHandleProgressReport_RejectsPhoneHomeForCommissionOperation(t *testing.T) {
	c := NewCache(nil)
	machineID, opID := uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateCommissioning,
	}}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpRunning,
	})

	err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(), MachineID: machineID.String(), Event: "phone_home",
	})
	if err == nil {
		t.Fatal("expected phone-home on commission operation to be rejected")
	}
	observations, updates := c.DrainOutbox()
	if len(observations) != 0 || len(updates) != 0 {
		t.Fatalf("rejected phone-home produced side effects: observations=%+v updates=%+v", observations, updates)
	}
}

func TestCacheHandleProgressReport_CommissionUsesBoundMachine(t *testing.T) {
	c := NewCache(nil)
	machineID, otherMachine, opID := uuid.New(), uuid.New(), uuid.New()
	bootMAC := "52:54:00:aa:bb:01"
	otherMAC := "52:54:00:aa:bb:02"
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{
		{ID: machineID, NetBoxSerial: "SN-EXPECTED", Interfaces: []model.SnapshotInterface{{MAC: bootMAC, Type: "physical"}}},
		{ID: otherMachine, NetBoxSerial: "SN-OTHER", Interfaces: []model.SnapshotInterface{{MAC: otherMAC, Type: "physical"}}},
	}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpRunning,
	})
	inventory := json.RawMessage(`{"system":{"serial_number":"SN-EXPECTED","uuid":"4f1d62c0-1f7d-4e74-a57c-a58932e2aa11"},"nics":[{"mac":"52:54:00:aa:bb:01","driver":"virtio_net"}]}`)

	if err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(), MachineID: machineID.String(),
		Event: "commission_report", CommissionData: inventory,
	}); err != nil {
		t.Fatalf("HandleProgressReport: %v", err)
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 1 {
		t.Fatalf("observations=%+v, want one", obs)
	}
	if obs[0].Obs.MachineID != machineID || obs[0].Obs.OperationID != opID {
		t.Fatalf("observation binding = machine %s op %s, want machine %s op %s",
			obs[0].Obs.MachineID, obs[0].Obs.OperationID, machineID, opID)
	}
	if string(obs[0].Obs.Data) != string(inventory) {
		t.Fatalf("inventory changed: got %s want %s", obs[0].Obs.Data, inventory)
	}
}

func TestCacheHandleProgressReport_RejectsForeignPhysicalMAC(t *testing.T) {
	c := NewCache(nil)
	machineID, otherMachine, opID := uuid.New(), uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{
		{ID: machineID, NetBoxSerial: "SN-EXPECTED", Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:01", Type: "physical"}}},
		{ID: otherMachine, NetBoxSerial: "SN-OTHER", Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:02", Type: "physical"}}},
	}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpRunning,
	})

	err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(), MachineID: machineID.String(), Event: "commission_report",
		CommissionData: json.RawMessage(`{"system":{"serial_number":"SN-EXPECTED"},"nics":[{"mac":"52:54:00:aa:bb:02","driver":"mlx5_core"}]}`),
	})
	if err == nil {
		t.Fatal("expected another machine's physical MAC to be rejected")
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 0 {
		t.Fatalf("rejected inventory queued observations: %+v", obs)
	}
}

func TestCacheHandleProgressReport_RejectsDiskOwnedByAnotherMachine(t *testing.T) {
	c := NewCache(nil)
	machineID, otherMachine, opID := uuid.New(), uuid.New(), uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{
		{ID: machineID, NetBoxSerial: "SN-EXPECTED", Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:01", Type: "physical"}}},
		{ID: otherMachine, NetBoxSerial: "SN-OTHER",
			Interfaces: []model.SnapshotInterface{{MAC: "52:54:00:aa:bb:02", Type: "physical"}},
			Disks:      []model.SnapshotDiskIdentity{{Serial: "DISK-OTHER", WWN: "0x5000other"}}},
	}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpCommission, Status: model.OpRunning,
	})

	err := c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(), MachineID: machineID.String(), Event: "commission_report",
		CommissionData: json.RawMessage(`{"system":{"serial_number":"SN-EXPECTED"},"nics":[{"mac":"52:54:00:aa:bb:01","driver":"mlx5_core"}],"disks":[{"serial":"DISK-OTHER","wwn":"0x5000other"}]}`),
	})
	if err == nil {
		t.Fatal("expected another machine's disk to be rejected")
	}
	obs, _ := c.DrainOutbox()
	if len(obs) != 0 {
		t.Fatalf("rejected inventory queued observations: %+v", obs)
	}
}

func TestCacheHandleProgressReport_Failure(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning},
	}...)

	c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(),
		MachineID:   machineID.String(),
		Phase:       "partitioning",
		Failed:      true,
		Error:       "disk not found",
	})

	if !c.IsOpFailed(opID.String()) {
		t.Fatal("not marked failed")
	}
}

// TestCacheHandleProgressReport_TerminalDeployComplete pins the new
// (commit 3) semantic: a deploy_complete progress report from the
// agent updates the op phase to awaiting_phone_home and queues the
// observation, but does NOT advance machine state and does NOT
// complete the op. Both happen later, when phone-home arrives.
func TestCacheHandleProgressReport_TerminalDeployComplete(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID, State: model.MachineStateDeploying}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning},
	}...)

	c.HandleProgressReport(&model.ProgressReport{
		OperationID: opID.String(),
		MachineID:   machineID.String(),
		Event:       "deploy_complete",
	})

	m := c.MachineByID(machineID)
	if m == nil || m.State != model.MachineStateDeploying {
		t.Fatalf("machine should stay deploying until phone-home, got %+v", m)
	}
	op := c.GetOperation(opID.String())
	if op == nil || op.Phase != "awaiting_phone_home" {
		t.Errorf("op phase: got %+v, want awaiting_phone_home", op)
	}
	if op != nil && (op.Status == model.OpCompleted || op.Status == model.OpFailed) {
		t.Errorf("op should not be terminal yet, got status=%s", op.Status)
	}
	obs, updates := c.DrainOutbox()
	foundComplete := false
	for _, o := range obs {
		if o.Obs.Event == "deploy_complete" {
			foundComplete = true
		}
	}
	if !foundComplete {
		t.Errorf("deploy_complete obs not queued: %+v", obs)
	}
	for _, u := range updates {
		if u.Status == string(model.OpCompleted) {
			t.Errorf("op_update completed should NOT be queued at deploy_complete time, got: %+v", u)
		}
	}
}

func TestCacheHandleProgressReport_PhoneHomeReplayAfterCompletionIsIdempotent(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{Machines: []model.MachineSnapshot{{
		ID: machineID, State: model.MachineStateDeploying,
	}}})
	seedOpsResync(c, model.Operation{
		ID: opID, MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning,
	})

	report := &model.ProgressReport{
		OperationID: opID.String(), MachineID: machineID.String(), Event: "phone_home",
	}
	if err := c.HandleProgressReport(report); err != nil {
		t.Fatalf("first phone-home: %v", err)
	}
	c.DrainOutbox()
	if err := c.HandleProgressReport(report); err != nil {
		t.Fatalf("phone-home replay after local completion: %v", err)
	}
	if observations, updates := c.DrainOutbox(); len(observations) != 0 || len(updates) != 0 {
		t.Fatalf("completed-op replay produced side effects: observations=%+v updates=%+v", observations, updates)
	}

	// Global can remove the completed op before a lost HTTP response is
	// retried. The running-state acknowledgement must remain idempotent even
	// after the exact op has left the active set.
	c.ApplyOpRemoved(opID.String(), machineID.String(), 2, model.MachineStateRunning)
	if err := c.HandleProgressReport(report); err != nil {
		t.Fatalf("phone-home replay after op removal: %v", err)
	}
	if observations, updates := c.DrainOutbox(); len(observations) != 0 || len(updates) != 0 {
		t.Fatalf("removed-op replay produced side effects: observations=%+v updates=%+v", observations, updates)
	}
}

func TestCacheMarkOpClaimed(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending},
	}...)

	c.MarkOpClaimed(opID.String())
	if got := c.GetOperation(opID.String()); got == nil || got.Status != model.OpClaimed {
		t.Fatalf("got %+v", got)
	}
	if got := c.PendingPowerOps(); len(got) != 0 {
		t.Fatalf("claimed op still pending: %+v", got)
	}
}

func TestCacheRecordOpUpdate(t *testing.T) {
	c := NewCache(nil)
	opID := uuid.New()
	machineID := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: machineID}}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: machineID, Type: model.OpPowerOn, Status: model.OpPending},
	}...)

	c.RecordOpUpdate(opID.String(), "completed", json.RawMessage(`{"ok":true}`))

	if got := c.GetOperation(opID.String()); got == nil || got.Status != model.OpCompleted {
		t.Fatalf("in-memory: got %+v", got)
	}
	_, updates := c.DrainOutbox()
	if len(updates) != 1 || updates[0].Status != "completed" {
		t.Fatalf("outbox: got %+v", updates)
	}
}

func TestCacheGetDHCPConfig(t *testing.T) {
	c := NewCache(nil)
	c.ApplySnapshot(&model.SiteSnapshot{
		DHCP: model.DHCPConfig{
			Subnet: "10.0.0.0/24", Gateway: "10.0.0.1",
			EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
			Promiscuous: true,
		},
	})
	got := c.GetDHCPConfig()
	if got.Subnet != "10.0.0.0/24" || got.Gateway != "10.0.0.1" || !got.Promiscuous {
		t.Fatalf("got %+v", got)
	}
}

// TestCacheReloadOutboxFromDisk_UnackedRowsComeBack covers the
// reconnect-recovery path: after a disconnect drops an in-flight
// observation out of memory, the SQLite row is still there and
// ReloadOutboxFromDisk must put it back so the next send retries.
func TestCacheReloadOutboxFromDisk_NoOpWithoutLocalDB(t *testing.T) {
	c := NewCache(nil)
	c.ReloadOutboxFromDisk() // must not panic
}

// TestCacheRequeueOutbox_PrependsItems verifies the failed-send
// recovery path: if drainOnce's send fails mid-batch, the remaining
// items are put back at the head of the queue so the next drain sees
// them before any items enqueued in the interim.
func TestCacheRequeueOutbox_PrependsItems(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	c.ApplySnapshot(snapMachine(id, "aa:aa:aa:00:00:01", model.MachineStateDeploying))

	// Drain an empty queue, then requeue two items.
	c.RequeueOutbox(
		[]PendingOutbox{
			{Obs: model.Observation{MachineID: id, Event: "deploy_complete"}},
			{Obs: model.Observation{MachineID: id, Event: "phone_home"}},
		},
		[]model.OperationUpdate{
			{OperationID: "op-1", Status: "running"},
		},
	)

	// Append one more observation AFTER requeue. It should land at the tail.
	c.recordDeployComplete(id, uuid.New(), nil) // appends a third observation

	obs, updates := c.DrainOutbox()
	if len(obs) != 3 {
		t.Fatalf("got %d obs, want 3", len(obs))
	}
	// Requeued items come first; appended item at the tail.
	if obs[0].Obs.Event != "deploy_complete" || obs[1].Obs.Event != "phone_home" || obs[2].Obs.Event != "deploy_complete" {
		t.Fatalf("wrong order: %+v", obs)
	}
	if len(updates) != 1 || updates[0].OperationID != "op-1" {
		t.Fatalf("updates: %+v", updates)
	}
}

func TestCacheRequeueOutbox_EmptyIsNoOp(t *testing.T) {
	c := NewCache(nil)
	// No panic, no wake emitted.
	c.RequeueOutbox(nil, nil)
	select {
	case <-c.Wake:
		t.Fatal("empty requeue should not wake")
	default:
	}
}

func TestCacheMachineCount(t *testing.T) {
	c := NewCache(nil)
	if c.MachineCount() != 0 {
		t.Fatalf("initial: got %d", c.MachineCount())
	}
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{ID: uuid.New()}, {ID: uuid.New()}, {ID: uuid.New()},
		},
	})
	if c.MachineCount() != 3 {
		t.Fatalf("after apply: got %d, want 3", c.MachineCount())
	}
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: uuid.New()}},
	})
	if c.MachineCount() != 1 {
		t.Fatalf("after shrink: got %d, want 1 (full-replace)", c.MachineCount())
	}
}

// TestCacheApplySnapshot_PersistsMeta verifies that ApplySnapshot
// saves the snapshot meta to LocalDB, so a subsequent NewCache on
// the same DB can serve DHCP/TFTP from cached state during a cold
// start without live WS.
func TestCacheApplySnapshot_PersistsMeta(t *testing.T) {
	db := openTestDB(t)
	c := NewCache(db)

	snap := &model.SiteSnapshot{
		SeqNum: 11,
		Site:   model.SiteInfo{Slug: "pop3", Name: "pop3"},
		DHCP: model.DHCPConfig{
			Subnet:             "10.252.0.0/16",
			Gateway:            "10.252.1.1",
			EphemeralPoolStart: "10.252.1.200",
			EphemeralPoolEnd:   "10.252.1.254",
		},
	}
	c.ApplySnapshot(snap)

	// A brand-new Cache from the same DB rehydrates the meta.
	c2 := NewCache(db)
	if c2.LastSeqNum() != 11 {
		t.Errorf("LastSeqNum after reload: got %d, want 11", c2.LastSeqNum())
	}
	if sub := c2.GetDHCPConfig().Subnet; sub != "10.252.0.0/16" {
		t.Errorf("DHCP.Subnet after reload: got %q", sub)
	}
}

// TestListActiveMachineIDs_CredGating covers the SOL-pump active-set
// rule that replaced the temporary single-host hostname allowlist: a
// machine is "active" (gets a persistent ipmitool SOL session) iff it
// has a BMC IP AND its credentials resolve — either from the machine
// row (post-reconcile) or from an active deploy op's bmc_runtime
// envelope. Credless hosts (PDUs, switches, unprovisioned boxes) must
// be excluded so they don't churn ipmitool + spam "no creds" envelopes.
func TestListActiveMachineIDs_CredGating(t *testing.T) {
	c := NewCache(nil)

	noBMCIP := uuid.New()      // has creds but no BMC IP -> excluded
	rowCreds := uuid.New()     // machine-row creds -> included
	opCreds := uuid.New()      // creds only via active deploy op -> included
	noCreds := uuid.New()      // BMC IP but no creds anywhere -> excluded
	partialCreds := uuid.New() // BMC IP, half creds -> excluded

	opID := uuid.New()
	rt, err := json.Marshal(map[string]any{
		"bmc_runtime": model.BMCRuntime{Username: "console", Password: "test-password"},
	})
	if err != nil {
		t.Fatalf("marshal bmc_runtime: %v", err)
	}

	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{ID: noBMCIP, State: model.MachineStateRunning, BMCUser: "console", BMCPass: "pw"},
			{ID: rowCreds, State: model.MachineStateRunning, BMCIP: "10.0.0.1", BMCUser: "console", BMCPass: "pw"},
			{ID: opCreds, State: model.MachineStateDeploying, BMCIP: "10.0.0.2"},
			{ID: noCreds, State: model.MachineStateRunning, BMCIP: "10.0.0.3"},
			{ID: partialCreds, State: model.MachineStateRunning, BMCIP: "10.0.0.4", BMCUser: "console"},
		}})
	seedOpsResync(c, []model.Operation{
		{ID: opID, MachineID: opCreds, Type: model.OpDeploy, Status: model.OpClaimed, Params: rt},
	}...)

	got := make(map[uuid.UUID]bool)
	for _, id := range c.ListActiveMachineIDs() {
		got[id] = true
	}

	want := map[uuid.UUID]bool{rowCreds: true, opCreds: true}
	for id := range want {
		if !got[id] {
			t.Errorf("machine %s should be in the active set", id)
		}
	}
	for _, id := range []uuid.UUID{noBMCIP, noCreds, partialCreds} {
		if got[id] {
			t.Errorf("machine %s must NOT be in the active set (cred gate)", id)
		}
	}
	if len(got) != 2 {
		t.Errorf("active set size: got %d, want 2 (%v)", len(got), got)
	}
}
