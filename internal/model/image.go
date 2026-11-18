package model

import (
	"time"

	"github.com/google/uuid"
)

// Image is the single PXE boot image. There is exactly one row
// (conventionally named "noble-pxe"): deploy and commission are the SAME
// artifact booted in different modes (the slagworks.mode= kernel cmdline,
// set per-operation by the boot handler), not different images. Earlier
// versions had a per-Kind row (noble-deploy / noble-commission), which let
// the two rows pin different SHAs and drift out of sync (an earlier change converged
// the build; this converged the distribution). There is no Kind field by
// design — do not reintroduce one.
type Image struct {
	ID        uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	Name      string    `json:"name" gorm:"uniqueIndex;not null"`
	Version   string    `json:"version" gorm:"not null"`
	Arch      string    `json:"arch" gorm:"not null;default:'amd64'"`
	SHA256    string    `json:"sha256" gorm:"not null"`
	URL       string    `json:"url" gorm:"not null"`
	CreatedAt time.Time `json:"created_at" gorm:"autoCreateTime"`
}
