package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type OperationType string

const (
	OpDeploy     OperationType = "deploy"
	OpCommission OperationType = "commission"
	OpPowerOn    OperationType = "power_on"
	OpPowerOff   OperationType = "power_off"
	OpPowerCycle OperationType = "power_cycle"
	// OpSetBootDevice arms a BMC boot-device override out-of-band — the
	// operator-facing boot-set, distinct from the one-time override the
	// deploy path arms inline via Controller.SetBootDevice. Params:
	// {device: pxe|disk|default, persistent: bool}. Requires a
	// BootManager-capable controller; fails with a
	// "not supported" error otherwise.
	OpSetBootDevice OperationType = "set_boot_device"
	// OpBMCReconcile brings a BMC's IPMI user table into compliance over
	// the LAN, from the site worker that owns the BMC's OOB network,
	// authenticating with the device's bmc-creds-admin (slot-2) account:
	// one `monitor` account at USER with the password stored for the
	// device in pop/<pop>/bmc-creds-monitor re-asserted (or, when nothing
	// is stored or the op rotates, a freshly generated password, which
	// global stores there), every other non-reserved account disabled and
	// cleared, boot order untouched. Then the BMC's time configuration:
	// NTP on with the OOB prefix's ntp_servers as the only servers,
	// timezone UTC, a clock more than a minute off stepped
	// (BMCTimeCompliance; a time failure is reported, never fails the op).
	// Params: {bmc_runtime, monitor_runtime | rotate_monitor}. The worker
	// reports through a bmc_reconcile_report observation (a generated
	// monitor password must never ride an op_update), and global finalizes
	// the op from it.
	OpBMCReconcile OperationType = "bmc_reconcile"
)

// AllOperationTypes is the canonical set of operation types. The site
// worker's pending-op getters (PendingPowerOps / PendingLifecycleOps) must
// collectively return every type in this list, or an op of the missing type
// is orphaned forever (never claimed, never executed). A guard test in the
// worker package iterates this list to enforce that. When adding a new
// OperationType constant, add it here AND route it in one of the getters.
var AllOperationTypes = []OperationType{
	OpDeploy,
	OpCommission,
	OpPowerOn,
	OpPowerOff,
	OpPowerCycle,
	OpSetBootDevice,
	OpBMCReconcile,
}

type OperationStatus string

const (
	OpPending   OperationStatus = "pending"
	OpClaimed   OperationStatus = "claimed"
	OpRunning   OperationStatus = "running"
	OpCompleted OperationStatus = "completed"
	OpFailed    OperationStatus = "failed"
)

type Operation struct {
	ID             uuid.UUID       `json:"id" gorm:"type:uuid;primaryKey"`
	MachineID      uuid.UUID       `json:"machine_id" gorm:"type:uuid;not null;index"`
	TraceID        uuid.UUID       `json:"trace_id" gorm:"type:uuid;not null"`
	Type           OperationType   `json:"type" gorm:"not null"`
	Status         OperationStatus `json:"status" gorm:"not null;default:'pending'"`
	Phase          string          `json:"phase,omitempty"`
	PhaseEnteredAt time.Time       `json:"phase_entered_at" gorm:"not null;default:now()"`
	ClaimedBy      string          `json:"claimed_by,omitempty"`
	ClaimedAt      *time.Time      `json:"claimed_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at" gorm:"autoCreateTime"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	Params         json.RawMessage `json:"params,omitempty" gorm:"type:jsonb"`
	Result         json.RawMessage `json:"result,omitempty" gorm:"type:jsonb"`
	// Version is a per-op monotonic counter bumped in the same UPDATE as
	// every status/phase/claim/fail mutation (Tier 2, Phase 0). Consumers
	// of the op-delta stream apply last-writer-wins by this value. INSERTs
	// leave it at 0.
	Version int `json:"version" gorm:"not null;default:0"`

	Machine *Machine `json:"machine,omitempty" gorm:"foreignKey:MachineID"`

	// MachineName is a transient response field populated by the API on
	// list responses: the machine's NetBox hostname (falling back to its
	// serial). Not stored in Postgres. Lets `slagctl operation list` /
	// `machine ops` show a human machine name instead of a raw UUID.
	MachineName string `json:"machine_name,omitempty" gorm:"-"`
}
