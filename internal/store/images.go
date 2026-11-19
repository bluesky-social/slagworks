package store

import (
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
)

func (s *Store) CreateImage(img *model.Image) error {
	if img.ID == uuid.Nil {
		img.ID = uuid.New()
	}
	return s.db.Create(img).Error
}

// UpsertImage creates the image row if one with the given name doesn't
// exist, or updates all mutable fields on the existing row. ID and
// CreatedAt are preserved by gorm's autoCreateTime / primary-key
// semantics.
//
// The row is keyed by name, so the store always assigns the id and any
// id the caller set is discarded: the conflict target is name alone, so
// a caller's id that belongs to another row would fail on the primary
// key instead of resolving. A new row gets a fresh id; an existing row
// keeps its own.
//
// We use UpdateAll rather than an explicit AssignmentColumns list so
// future Image schema additions are picked up automatically; otherwise
// a new column would silently fail to update on follow-latest pins.
//
// clause.Returning{} forces gorm to issue a `RETURNING *` so the
// `img` struct is refreshed with the actual stored row after the
// upsert. Without it, an UPDATE path leaves `img.ID` set to the
// uuid.New() we generated above (rather than the existing row's ID).
// The API handler returns this struct to the caller, so a stale ID
// would reach the operator running `slagctl image follow-latest`.
//
// Used by `slagctl image follow-latest`: an operator-driven workflow
// that resolves the current latest <git-sha>.tar.gz on S3 and points
// the named row at it without caring whether the row already exists.
func (s *Store) UpsertImage(img *model.Image) error {
	img.ID = uuid.New()
	return s.db.Clauses(
		clause.OnConflict{
			Columns:   []clause.Column{{Name: "name"}},
			UpdateAll: true,
		},
		clause.Returning{},
	).Create(img).Error
}

func (s *Store) ListImages() ([]model.Image, error) {
	var images []model.Image
	return images, s.db.Find(&images).Error
}

// DeleteImage deletes the image row by uuid or name. Site workers sync
// whatever image rows exist (imagepull.SyncImages diffs the row set
// against on-disk .sha256 markers); deleting a row simply makes workers
// stop tracking that image on their next snapshot apply. Any file already
// downloaded under the old name stays on disk (SyncImages only pulls
// missing/mismatched entries — it does not prune), which is acceptable:
// disk space on the image cache is not a slagworks-managed constraint.
func (s *Store) DeleteImage(ident string) error {
	if id, err := uuid.Parse(ident); err == nil {
		res := s.db.Delete(&model.Image{}, "id = ?", id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("image %s not found", ident)
		}
		return nil
	}
	res := s.db.Delete(&model.Image{}, "name = ?", ident)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("image %s not found", ident)
	}
	return nil
}
