package model

import (
	"encoding/json"

	"github.com/google/uuid"
)

type BootAction int

const (
	BootDeploy BootAction = iota
	BootCommission
	BootLocal
)

// ProgressReport is the unified payload sent by agents to report deploy
// progress, log lines, phase transitions, and terminal events.
type ProgressReport struct {
	OperationID string    `json:"op_id"`
	MachineID   string    `json:"machine_id"`
	Phase       string    `json:"phase,omitempty"`
	Failed      bool      `json:"failed,omitempty"`
	Error       string    `json:"error,omitempty"`
	Event       string    `json:"event,omitempty"`
	Logs        []LogLine `json:"logs,omitempty"`

	// CommissionData carries the final inventory on commission_report.
	// The report's machine_id/op_id are the authoritative identity; the
	// inventory is evidence to validate and persist, never a lookup key.
	CommissionData json.RawMessage `json:"commission_data,omitempty"`

	// BMCCreds carries the result of the agent's BMC reconcile phase.
	// Set on the deploy_complete event (one-shot, end of deploy);
	// nil on every other event. Site worker forwards the payload
	// through to global as the deploy_complete observation's Data
	// field; global upserts the monitor cred into AWS Secrets Manager.
	// After the report POST returns, the agent zeroes BMCCreds in memory.
	BMCCreds *BMCCreds `json:"bmc_creds,omitempty"`
}

// BMCCreds is the post-reconcile snapshot of the BMC monitor user the
// agent provisioned. Carries a plaintext password on the wire from agent
// to slagworksd; it is not durable in any log or replayable channel and
// is zeroed in agent memory after the phone-home POST completes.
//
// slagworks provisions no BMC account of its own: it drives BMCs with
// the device's pop/<pop>/bmc-creds-admin entry only.
//
// `host_for_secret` is intentionally absent: global derives the secret
// key (the NetBox device serial) from `op.MachineID` → NetBox device at
// upsert time, so the agent has no need to know its NetBox identity.
type BMCCreds struct {
	// Monitor is the per-host monitor user the agent provisioned.
	// nil = no new password; non-nil triggers a PutSecretValue against
	// the pop's bmc-creds-monitor secret. Pointer-and-omitempty is
	// deliberate: "no rotation" and "rotated to empty" are different
	// states.
	Monitor *BMCUserCred `json:"monitor,omitempty"`

	// BootOrderPersisted is true when the reconciler successfully
	// wrote a persistent [PXE, disk] boot order. Currently always
	// true on success; reported for audit / "did this actually run"
	// observability.
	BootOrderPersisted bool `json:"boot_order_persisted"`

	// LeftoversDisabled is the names of users the reconciler found
	// outside the desired set + reserved slots and disabled. Logged
	// for audit; no functional consumer on the global side.
	LeftoversDisabled []string `json:"leftover_users_disabled,omitempty"`
}

// BMCUserCred is the freshly-generated credential for a single BMC
// user the reconciler created or rotated. In a bmc_reconcile report that
// re-asserted the stored password (MonitorRotated false), Password is
// empty: the report names the account but carries no secret.
type BMCUserCred struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	IPMIUserSlot int    `json:"ipmi_user_slot"`
}

// BMCReconcileReport is the Data of a bmc_reconcile_report observation:
// the site worker's outcome for a remote (LAN) bmc_reconcile op. It is
// the ONLY channel that carries a rotated monitor password from the
// worker to global — op_update results are persisted verbatim in the
// operations table and op logs, so the password never rides one. Global
// upserts a rotated Monitor into pop/<pop>/bmc-creds-monitor, then
// finalizes the op with Result(), which holds only non-secret facts.
type BMCReconcileReport struct {
	// Monitor is the monitor account reconcile wrote or verified the
	// password of: its slot always, and its password only when
	// MonitorRotated. nil when reconcile failed before either.
	Monitor *BMCUserCred `json:"monitor,omitempty"`
	// MonitorRotated is true when the written password was freshly
	// generated (the store had no entry for the device, or the op asked to
	// rotate), so global must store it. False when reconcile re-asserted
	// the stored password the op carried: the password does not ride the
	// report back, and global writes no new secret version.
	MonitorRotated bool `json:"monitor_rotated"`
	// MonitorPasswordWritten is true when reconcile wrote the monitor
	// password (a rotation, a created account, or a stored password the BMC
	// did not hold), and false when the BMC already held the stored one, so
	// nothing was written.
	MonitorPasswordWritten bool `json:"monitor_password_written"`
	// LeftoversDisabled names the accounts reconcile disabled. Each name
	// was also cleared from its slot where the BMC allowed it (Supermicro
	// refuses an empty name).
	LeftoversDisabled []string `json:"leftover_users_disabled,omitempty"`
	// Error is non-empty when reconcile failed after it had started
	// writing (a partial run). Global still persists Monitor, then fails
	// the op with this error. Time-compliance failures are not here; they
	// ride BMCTimeCompliance.TimeError and leave the op's outcome alone.
	Error string `json:"error,omitempty"`
	BMCTimeCompliance
}

// Result is the persisted op result for this report: the error (if any)
// and the non-secret BMCReconcileResult. Never includes the password.
func (r BMCReconcileReport) Result() OpResult {
	res := &BMCReconcileResult{
		MonitorRotated:         r.MonitorRotated,
		MonitorPasswordWritten: r.MonitorPasswordWritten,
		LeftoversDisabled:      r.LeftoversDisabled,
		BMCTimeCompliance:      r.BMCTimeCompliance,
	}
	if r.Monitor != nil {
		res.MonitorSlot = r.Monitor.IPMIUserSlot
	}
	return OpResult{Error: r.Error, BMCReconcile: res}
}

// LogLine is a single log entry from an agent.
type LogLine struct {
	Source    string `json:"source"`
	Timestamp string `json:"ts"`
	Line      string `json:"line"`
}

// MachineProvider abstracts machine state lookups for the boot handler.
// The test harness uses an in-memory implementation; the real worker
// uses a cache backed by snapshot applies.
//
// Commission data is not part of this interface: the agent re-runs the
// inventory collector at deploy time and pipes the result directly to
// the pre-chosen storage script. Global picks the script in the
// snapshot-build step and ships it via StorageScriptForMAC.
type MachineProvider interface {
	BootActionForMAC(mac string) BootAction
	MachineIDForMAC(mac string) (uuid.UUID, bool)
	// MachineByMAC returns the machine snapshot for a MAC, or nil for an
	// unknown MAC. The .State field reflects any local override
	// (RecordPhoneHome). Callers read the per-machine field they need
	// (SystemUUID, StorageScript, KernelParams, NetworkScript,
	// Interfaces, FQDN) directly off the returned snapshot — treating the
	// zero value of each as "unset" with the same semantics the old
	// per-field accessors documented: empty SystemUUID == never
	// commissioned, empty KernelParams == deploy refused, nil
	// StorageScript/NetworkScript == fall back to the built-in default.
	MachineByMAC(mac string) *MachineSnapshot
	// MachineByID returns the machine snapshot by ID, or nil when the
	// machine isn't cached. Same field-access contract as MachineByMAC;
	// used by the agent's per-machine boot-config endpoints (kernel
	// params, network script/facts, hostname, storage script) which key
	// off the machine ID carried on the kernel cmdline.
	MachineByID(id uuid.UUID) *MachineSnapshot
	RecordPhoneHome(machineID uuid.UUID)
	RecordInventoryReport(machineID uuid.UUID, data []byte)
	DeployOpForMAC(mac string) *Operation
	CommissionOpForMAC(mac string) *Operation
	GetSSHKeys() []string
	HandleProgressReport(report *ProgressReport) error
}
