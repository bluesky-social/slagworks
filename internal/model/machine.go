package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type MachineState string

// State machine: see docs/dev/state-machine.md.
// Forward path is `new → commissioning → commissioned → deploying → running`,
// with `failed` reachable from any post-`new` transient state.
const (
	MachineStateNew           MachineState = "new"
	MachineStateCommissioning MachineState = "commissioning"
	MachineStateCommissioned  MachineState = "commissioned"
	MachineStateDeploying     MachineState = "deploying"
	MachineStateRunning       MachineState = "running"
	MachineStateFailed        MachineState = "failed"
)

var validMachineStates = map[MachineState]bool{
	MachineStateNew:           true,
	MachineStateCommissioning: true,
	MachineStateCommissioned:  true,
	MachineStateDeploying:     true,
	MachineStateRunning:       true,
	MachineStateFailed:        true,
}

func (s MachineState) Valid() bool { return validMachineStates[s] }

// Machine is slagworks-owned runtime state for a NetBox Device.
// Identity (hostname, MACs, BMC IP, site) lives in NetBox and is joined in
// at snapshot-build time.
//
// NetBoxSerial is required because it's the stable key used to re-bind a
// machine when a NetBox Device is deleted and recreated. NetBoxDeviceID is
// nullable because a device may be temporarily unbound.
//
// No BMC credential lives here: the device's pop/<pop>/bmc-creds-admin
// entry (keyed by NetBox serial) is the only BMC credential slagworks uses.
type Machine struct {
	ID             uuid.UUID       `json:"id" gorm:"type:uuid;primaryKey"`
	NetBoxDeviceID *int            `json:"netbox_device_id,omitempty" gorm:"column:netbox_device_id"`
	NetBoxSerial   string          `json:"netbox_serial" gorm:"column:netbox_serial;not null"`
	State          MachineState    `json:"state" gorm:"not null;default:'new'"`
	DesiredOS      string          `json:"desired_os,omitempty"`
	CurrentOS      string          `json:"current_os,omitempty"`
	CommissionData json.RawMessage `json:"commission_data,omitempty" gorm:"type:jsonb"`
	CommissionedAt *time.Time      `json:"commissioned_at,omitempty" gorm:"column:commissioned_at"`
	Locked         bool            `json:"locked" gorm:"not null;default:false"`
	PXEEligible    bool            `json:"pxe_eligible" gorm:"not null;default:false"`
	CreatedAt      time.Time       `json:"created_at" gorm:"autoCreateTime"`

	// Hostname is a transient response field populated by the API
	// when a request explicitly asks for hostname enrichment (e.g.
	// `GET /api/v1/machines?hostname=cpu1`). Sourced from NetBox
	// device.name; not stored in Postgres.
	Hostname string `json:"hostname,omitempty" gorm:"-"`
}
