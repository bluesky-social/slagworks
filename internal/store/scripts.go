package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ErrScriptNotFound is returned by the by-name script operations
// (Update*ScriptByName, List*ScriptVersions) when no script exists under the
// given name, so callers can map it to a 404 without collapsing every other
// store failure (connectivity, constraint violation, transaction errors)
// into the same status code.
var ErrScriptNotFound = errors.New("script not found")

// Storage/network scripts are versioned: content is never
// clobbered in place. The row types below are the store-internal shape of
// the two real tables — script identity (id, name, head_version,
// created_at) and its immutable version history (script_id, version,
// priority, content, created_at). model.StorageScript/NetworkScript stay
// the EXTERNAL read shape consumers (snapshot dispatcher, preflight, API
// list handlers) already depend on: List reads a script joined to its head
// version and returns that same struct, so none of those consumers change.

type storageScriptRow struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	Name        string    `gorm:"uniqueIndex;not null"`
	HeadVersion int       `gorm:"column:head_version;not null"`
	CreatedAt   time.Time
}

func (storageScriptRow) TableName() string { return "storage_scripts" }

type storageScriptVersionRow struct {
	ScriptID  uuid.UUID `gorm:"type:uuid;primaryKey;column:script_id"`
	Version   int       `gorm:"primaryKey"`
	Priority  int       `gorm:"not null"`
	Content   string    `gorm:"not null"`
	CreatedAt time.Time
}

func (storageScriptVersionRow) TableName() string { return "storage_script_versions" }

type networkScriptRow struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	Name        string    `gorm:"uniqueIndex;not null"`
	HeadVersion int       `gorm:"column:head_version;not null"`
	CreatedAt   time.Time
}

func (networkScriptRow) TableName() string { return "network_scripts" }

type networkScriptVersionRow struct {
	ScriptID  uuid.UUID `gorm:"type:uuid;primaryKey;column:script_id"`
	Version   int       `gorm:"primaryKey"`
	Priority  int       `gorm:"not null"`
	Content   string    `gorm:"not null"`
	CreatedAt time.Time
}

func (networkScriptVersionRow) TableName() string { return "network_script_versions" }

// listStorageScriptsQuery/listNetworkScriptsQuery join the script row to its
// head version. Raw SQL (rather than gorm's Joins builder) because the
// target — model.StorageScript/NetworkScript — is a flat external DTO with
// no relation to declare; a plain SELECT into it is the clearest expression
// of "read model stays put".
const listStorageScriptsQuery = `
SELECT s.id AS id, s.name AS name, v.priority AS priority, v.content AS content, s.created_at AS created_at
FROM storage_scripts s
JOIN storage_script_versions v ON v.script_id = s.id AND v.version = s.head_version
ORDER BY v.priority ASC`

const listNetworkScriptsQuery = `
SELECT s.id AS id, s.name AS name, v.priority AS priority, v.content AS content, s.created_at AS created_at
FROM network_scripts s
JOIN network_script_versions v ON v.script_id = s.id AND v.version = s.head_version
ORDER BY v.priority ASC`

// listStorageScriptVersionsQuery/listNetworkScriptVersionsQuery join every
// version row to its script identity in one statement, so "head" is computed
// from the same snapshot as the version list — reading the head pointer and
// the version rows as two separate autocommit queries would let a concurrent
// update land in between and mark no row (or the wrong row) as head.
const listStorageScriptVersionsQuery = `
SELECT v.version AS version, v.priority AS priority, v.content AS content, v.created_at AS created_at,
       (v.version = s.head_version) AS head
FROM storage_scripts s
JOIN storage_script_versions v ON v.script_id = s.id
WHERE s.name = ?
ORDER BY v.version ASC`

const listNetworkScriptVersionsQuery = `
SELECT v.version AS version, v.priority AS priority, v.content AS content, v.created_at AS created_at,
       (v.version = s.head_version) AS head
FROM network_scripts s
JOIN network_script_versions v ON v.script_id = s.id
WHERE s.name = ?
ORDER BY v.version ASC`

// --- Storage scripts ---

// CreateStorageScript inserts a new script identity plus its version 1 row,
// transactionally. script.ID is populated if not already set; script.
// CreatedAt is populated from the inserted script row.
func (s *Store) CreateStorageScript(script *model.StorageScript) error {
	if script.ID == uuid.Nil {
		script.ID = uuid.New()
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		row := storageScriptRow{ID: script.ID, Name: script.Name, HeadVersion: 1}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		version := storageScriptVersionRow{
			ScriptID: script.ID,
			Version:  1,
			Priority: script.Priority,
			Content:  script.Content,
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		script.CreatedAt = row.CreatedAt
		return nil
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("a storage script named %q already exists (priority need not be unique)", script.Name)
	}
	return err
}

func (s *Store) ListStorageScripts() ([]model.StorageScript, error) {
	var scripts []model.StorageScript
	return scripts, s.db.Raw(listStorageScriptsQuery).Scan(&scripts).Error
}

// DeleteStorageScript deletes the script identity; its version rows cascade
// via storage_script_versions' ON DELETE CASCADE FK. Semantics unchanged
// from the pre-versioning behavior.
func (s *Store) DeleteStorageScript(id uuid.UUID) error {
	res := s.db.Delete(&storageScriptRow{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("storage script %s not found", id)
	}
	return nil
}

// UpdateStorageScriptByName inserts a new immutable version (head+1) with
// the given priority/content and advances the script's head pointer,
// transactionally. Earlier versions are never touched. Returns the updated
// read-model row plus the new version number. Update is by-name (not ID)
// because the caller only knows the name it uploaded under; delete stays
// by-ID. 404-style error on unknown name — the caller must not create.
func (s *Store) UpdateStorageScriptByName(name string, priority int, content string) (*model.StorageScript, int, error) {
	var script model.StorageScript
	var newVersion int
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var row storageScriptRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("name = ?", name).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: storage script %q", ErrScriptNotFound, name)
			}
			return err
		}
		newVersion = row.HeadVersion + 1
		version := storageScriptVersionRow{
			ScriptID: row.ID,
			Version:  newVersion,
			Priority: priority,
			Content:  content,
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		if err := tx.Model(&row).Update("head_version", newVersion).Error; err != nil {
			return err
		}
		script = model.StorageScript{
			ID:        row.ID,
			Name:      row.Name,
			Priority:  priority,
			Content:   content,
			CreatedAt: row.CreatedAt,
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return &script, newVersion, nil
}

// ListStorageScriptVersions returns every version of a storage script,
// oldest first, with Head marking the one currently served. 404-style
// error on unknown name.
func (s *Store) ListStorageScriptVersions(name string) ([]model.ScriptVersion, error) {
	var out []model.ScriptVersion
	res := s.db.Raw(listStorageScriptVersionsQuery, name).Scan(&out)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("%w: storage script %q", ErrScriptNotFound, name)
	}
	return out, nil
}

// --- Network scripts ---

// CreateNetworkScript is the network-script twin of CreateStorageScript.
func (s *Store) CreateNetworkScript(script *model.NetworkScript) error {
	if script.ID == uuid.Nil {
		script.ID = uuid.New()
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		row := networkScriptRow{ID: script.ID, Name: script.Name, HeadVersion: 1}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		version := networkScriptVersionRow{
			ScriptID: script.ID,
			Version:  1,
			Priority: script.Priority,
			Content:  script.Content,
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		script.CreatedAt = row.CreatedAt
		return nil
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return fmt.Errorf("a network script named %q already exists (priority need not be unique)", script.Name)
	}
	return err
}

func (s *Store) ListNetworkScripts() ([]model.NetworkScript, error) {
	var scripts []model.NetworkScript
	return scripts, s.db.Raw(listNetworkScriptsQuery).Scan(&scripts).Error
}

// DeleteNetworkScript is the network-script twin of DeleteStorageScript.
func (s *Store) DeleteNetworkScript(id uuid.UUID) error {
	res := s.db.Delete(&networkScriptRow{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("network script %s not found", id)
	}
	return nil
}

// UpdateNetworkScriptByName is the network-script twin of
// UpdateStorageScriptByName.
func (s *Store) UpdateNetworkScriptByName(name string, priority int, content string) (*model.NetworkScript, int, error) {
	var script model.NetworkScript
	var newVersion int
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var row networkScriptRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("name = ?", name).First(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: network script %q", ErrScriptNotFound, name)
			}
			return err
		}
		newVersion = row.HeadVersion + 1
		version := networkScriptVersionRow{
			ScriptID: row.ID,
			Version:  newVersion,
			Priority: priority,
			Content:  content,
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}
		if err := tx.Model(&row).Update("head_version", newVersion).Error; err != nil {
			return err
		}
		script = model.NetworkScript{
			ID:        row.ID,
			Name:      row.Name,
			Priority:  priority,
			Content:   content,
			CreatedAt: row.CreatedAt,
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return &script, newVersion, nil
}

// ListNetworkScriptVersions is the network-script twin of
// ListStorageScriptVersions.
func (s *Store) ListNetworkScriptVersions(name string) ([]model.ScriptVersion, error) {
	var out []model.ScriptVersion
	res := s.db.Raw(listNetworkScriptVersionsQuery, name).Scan(&out)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, fmt.Errorf("%w: network script %q", ErrScriptNotFound, name)
	}
	return out, nil
}
