package model

import (
	"time"

	"github.com/google/uuid"
)

// NetworkScript is a NetBox-identity-matched shell script that emits a
// host network-config bundle (files + units-to-enable) for the deploy
// agent. It is the network analog of StorageScript: scripts live in the
// global database and sync to site workers each cycle. Global performs
// the match per machine at snapshot-build time (against NetBox identity:
// site / device_type / hostname) and attaches the chosen script to the
// MachineSnapshot; the worker never runs the match.
type NetworkScript struct {
	ID   uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	Name string    `json:"name" gorm:"uniqueIndex;not null"`
	// Priority is a match TIEBREAKER (configscript.Match sorts desc, first
	// matching rule-set wins), NOT a globally-unique key — two scripts matching
	// disjoint host sets may share a priority. Non-unique index only (the unique
	// constraint was a bug; see migration 00021).
	Priority  int       `json:"priority" gorm:"index;not null"`
	Content   string    `json:"content" gorm:"not null"`
	CreatedAt time.Time `json:"created_at"`
}
