package model

import (
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// imageNameRE is what an image name may look like. The name becomes the
// image's directory under the site worker's images dir (and its
// <name>.sha256 marker beside it), so it must be one plain path element:
// starting with a letter or digit rules out "", ".", "..", and hidden
// names, and the character set rules out separators. Lowercase only, so
// two names cannot share a directory on a case-insensitive filesystem
// (slagctl image sync runs on operator laptops).
var imageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidateImageName reports whether name is usable as an image name. The
// API checks it when a row is written, and imagepull.SyncImages checks it
// again before turning the name into a path, since SyncImages also takes
// names from rows written before this check existed and from slagctl
// image sync --images-json.
func ValidateImageName(name string) error {
	if !imageNameRE.MatchString(name) {
		return fmt.Errorf("invalid image name %q: use lowercase letters, digits, '.', '_' and '-', starting with a letter or digit (for example noble-pxe)", name)
	}
	return nil
}

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
