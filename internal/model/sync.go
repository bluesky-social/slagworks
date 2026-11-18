package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// LogEntry is a single log line from a deploy agent.
type LogEntry struct {
	Source string `json:"source"`
	TS     string `json:"ts"`
	Line   string `json:"line"`
}

type Observation struct {
	MachineID   uuid.UUID       `json:"machine_id"`
	OperationID uuid.UUID       `json:"op_id,omitempty"`
	Event       string          `json:"event"`
	Timestamp   time.Time       `json:"timestamp"`
	Data        json.RawMessage `json:"data,omitempty"`

	// SiteSlug and WorkerID identify which site/worker forwarded this
	// observation. Stamped by the hub from the authenticated connection
	// (workerConn.slug / workerConn.workerID) after decode, in
	// handleObservation — NEVER trusted from the worker's wire payload
	// (workers don't set these fields; ObservationMsg carries no such
	// fields). Needed by the dhcp_lease ledger fold, which is keyed
	// per (site, worker) so an HA cage's two workers don't collide on
	// each other's open lease rows.
	SiteSlug string `json:"site_slug,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
}

type OperationUpdate struct {
	OperationID string          `json:"op_id"`
	Status      string          `json:"status,omitempty"`
	Phase       string          `json:"phase,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}
