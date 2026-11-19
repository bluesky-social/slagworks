package model

import (
	"time"

	"github.com/google/uuid"
)

// SiteWorker records a site worker process that has connected to the global
// API. Keyed by (site_slug, worker_id); upserted on each report call, so
// last_seen_at reflects the most recent contact. Multiple workers may run
// for the same site (e.g., active + standby).
//
// TokenHash is sha256(plaintext-token) in lowercase hex. The plaintext
// is returned once at registration time and never persisted. Rotation is
// re-registration via the admin path; an existing hash is preserved on
// idempotent RegisterSiteWorker calls from the hub.
type SiteWorker struct {
	ID          uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	SiteSlug    string    `json:"site_slug" gorm:"column:site_slug;not null;uniqueIndex:idx_site_worker_unique"`
	WorkerID    string    `json:"worker_id" gorm:"not null;uniqueIndex:idx_site_worker_unique"`
	FQDN        string    `json:"fqdn"`
	TokenHash   string    `json:"-" gorm:"column:token_hash"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`

	// Serial is the worker host's hardware serial (SMBIOS), reported in the
	// WS hello and verified by global against the NetBox device before the
	// connection is accepted. Recorded here so the claim path (P4b outlet-op
	// self-power interlock) can read a worker's verified serial store-side.
	// Under serial enforcement (production: a real SerialResolver wired) an
	// empty reported serial refuses the hello outright, so a connected
	// worker's row always carries a verified serial. Empty appears only for
	// rows written under a nil resolver (dev/test) or before the worker's
	// first post-P4a hello; such a worker is ineligible for outlet ops.
	// It keeps the board's spelling, which can differ in case from the NetBox
	// device's serial (the gate matches case-insensitively): compare it with
	// SameHardwareSerial.
	Serial string `json:"serial,omitempty" gorm:"column:serial;not null;default:''"`
}
