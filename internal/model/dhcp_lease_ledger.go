package model

import (
	"time"

	"github.com/google/uuid"
)

// DHCPLeaseLedgerEntry is one row of the global lease-history ledger
// (Phase 3 of docs/plans/2026-07-13-dhcp-lease-visibility.md).
// ReleasedAt nil means the row is the currently-open lease for its
// (SiteSlug, WorkerID, MAC, IP); a non-nil ReleasedAt + ReleaseReason
// ("superseded" | "deleted") records how it closed. See
// internal/store/dhcp_ledger.go for the fold that maintains the
// invariant that at most one open row exists per (SiteSlug, WorkerID,
// MAC) and per (SiteSlug, WorkerID, IP).
type DHCPLeaseLedgerEntry struct {
	ID            int64      `json:"id" gorm:"primaryKey;autoIncrement"`
	SiteSlug      string     `json:"site_slug" gorm:"column:site_slug;not null"`
	WorkerID      string     `json:"worker_id" gorm:"column:worker_id;not null"`
	MAC           string     `json:"mac" gorm:"not null"`
	IP            string     `json:"ip" gorm:"not null"`
	MachineID     *uuid.UUID `json:"machine_id,omitempty" gorm:"column:machine_id;type:uuid"`
	Kind          string     `json:"kind" gorm:"not null"`
	Pool          string     `json:"pool"`
	FirstSeen     time.Time  `json:"first_seen" gorm:"column:first_seen;not null"`
	LastSeen      time.Time  `json:"last_seen" gorm:"column:last_seen;not null"`
	ExpiresAt     time.Time  `json:"expires_at" gorm:"column:expires_at;not null"`
	ReleasedAt    *time.Time `json:"released_at,omitempty" gorm:"column:released_at"`
	ReleaseReason string     `json:"release_reason,omitempty" gorm:"column:release_reason"`
}

func (DHCPLeaseLedgerEntry) TableName() string { return "dhcp_lease_ledger" }

// Status derives the display status History rows use: "open" (no
// release AND not past expiry), "expired" (open but past ExpiresAt —
// the ledger doesn't require a delete observation to mark this; a
// worker crash or dropped observation shouldn't make a lease look
// perpetually live), or the release reason verbatim ("superseded" /
// "deleted") for a closed row.
func (e DHCPLeaseLedgerEntry) Status() string {
	if e.ReleasedAt != nil {
		return e.ReleaseReason
	}
	if time.Now().After(e.ExpiresAt) {
		return "expired"
	}
	return "open"
}

// DHCPLeaseEvent is the decoded shape of a dhcp_lease observation's
// Data field, as the worker's dhcpLeaseObservation marshals it (see
// internal/worker/cache.go RecordDHCPLease). Kept as a distinct type
// in internal/model (rather than importing internal/worker's private
// type) so internal/commission can decode without a package cycle.
type DHCPLeaseEvent struct {
	Event     string `json:"event"`
	MAC       string `json:"mac"`
	IP        string `json:"ip"`
	Kind      string `json:"kind"`
	Pool      string `json:"pool"`
	ExpiresAt string `json:"expires_at"`
}
