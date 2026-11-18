package model

import (
	"time"

	"github.com/google/uuid"
)

// StorageScript is a hardware-matched shell script that produces a storage
// layout JSON for the deploy agent. Scripts are stored in the global database
// and synced to site workers on each sync cycle.
type StorageScript struct {
	ID   uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	Name string    `json:"name" gorm:"uniqueIndex;not null"`
	// Priority is a match TIEBREAKER, not a globally-unique key — two scripts
	// matching disjoint hardware may share a priority. Non-unique index only
	// (the unique constraint was a bug; see migration 00021).
	Priority  int       `json:"priority" gorm:"index;not null"`
	Content   string    `json:"content" gorm:"not null"`
	CreatedAt time.Time `json:"created_at"`
}
