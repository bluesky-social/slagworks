package store

import (
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

// SiteConfigAdmit decides whether a site config may be written, given every
// site_configs row as it stands at the moment of the write (the config's own
// row included, when it exists). A non-nil error refuses the write and is
// returned to the caller unchanged.
type SiteConfigAdmit func(existing []model.SiteConfig) error

// siteConfigScopeLock is the advisory-lock key that serializes every write
// of a site config's scope (netbox_site, netbox_location). The prefix keeps
// it apart from the other advisory-lock domains (WithPopAdvisoryLock).
const siteConfigScopeLock = "slagworks-site-config-scope"

// writeSiteConfig runs admit and write in one transaction that holds the
// site-config scope advisory lock.
//
// No two site configs may claim the same device, and whether a config may be
// written depends on every other row: a row lock on the config being written
// cannot stop a second writer from inserting or re-scoping a different row
// between the check and the write, after which both pass and both land. The
// lock is in Postgres rather than in the process so that it also holds
// between slagworksd replicas sharing the database. It is released when the
// transaction ends. Under READ COMMITTED the list below is a new statement,
// so it sees everything the previous holder committed.
//
// Deletes do not take the lock: removing a config can only shrink the set of
// claimed devices.
func (s *Store) writeSiteConfig(admit SiteConfigAdmit, write func(tx *gorm.DB) error) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", siteConfigScopeLock).Error; err != nil {
			return fmt.Errorf("acquire advisory lock %q: %w", siteConfigScopeLock, err)
		}
		if admit != nil {
			var existing []model.SiteConfig
			if err := tx.Find(&existing).Error; err != nil {
				return fmt.Errorf("list site configs: %w", err)
			}
			if err := admit(existing); err != nil {
				return err
			}
		}
		return write(tx)
	})
}

// CreateSiteConfig inserts a SiteConfig after validating the payload. admit,
// when non-nil, is asked whether the config may be created; the question and
// the insert are atomic with respect to every other site-config create and
// update (see writeSiteConfig).
func (s *Store) CreateSiteConfig(cfg *model.SiteConfig, admit SiteConfigAdmit) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.writeSiteConfig(admit, func(tx *gorm.DB) error {
		return tx.Create(cfg).Error
	})
}

func (s *Store) GetSiteConfig(slug string) (*model.SiteConfig, error) {
	var cfg model.SiteConfig
	if err := s.db.First(&cfg, "slug = ?", slug).Error; err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (s *Store) ListSiteConfigs() ([]model.SiteConfig, error) {
	var out []model.SiteConfig
	if err := s.db.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ListSiteConfigsByNetBoxSite returns every site_config that mirrors the
// given NetBox site slug. Multiple configs can share one NetBox site
// (location-scoped dual-cage), so this is the candidate set the claim
// resolver scope-checks a device against. The PK (slug) is the worker
// identity and need NOT equal netbox_site, so callers that key on
// netbox_site must use this, not GetSiteConfig.
func (s *Store) ListSiteConfigsByNetBoxSite(netboxSite string) ([]model.SiteConfig, error) {
	var out []model.SiteConfig
	if err := s.db.Where("netbox_site = ?", netboxSite).Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// AnySiteConfigForNetBoxSite reports whether any site_config mirrors the
// given NetBox site slug. Used by the mirror's slug-immutability guard,
// which asks "does slagworks manage this NetBox site" (a netbox_site
// question), not "is this a config PK".
func (s *Store) AnySiteConfigForNetBoxSite(netboxSite string) (bool, error) {
	var count int64
	if err := s.db.Model(&model.SiteConfig{}).
		Where("netbox_site = ?", netboxSite).
		Limit(1).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// ErrSiteConfigNotFound is returned when an update targets a slug with no
// site_configs row.
var ErrSiteConfigNotFound = errors.New("site config not found")

// UpdateSiteConfig writes the operator-owned columns of cfg (the ones
// PATCH /site-configs/:slug accepts) and nothing else. The caller loaded
// cfg earlier, and the snapshot dispatcher owns last_snapshot_seq and
// last_snapshot_hash: writing the loaded values back would undo a bump
// committed in between, and the next rebuild would reissue a SeqNum the
// worker has already applied and will drop. Returns ErrSiteConfigNotFound
// when no row matches, so a config deleted mid-request stays deleted.
//
// admit, when non-nil, is asked whether the config may be written as cfg has
// it; the question and the write are atomic with respect to every other
// site-config create and update (see writeSiteConfig).
func (s *Store) UpdateSiteConfig(cfg *model.SiteConfig, admit SiteConfigAdmit) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.writeSiteConfig(admit, func(tx *gorm.DB) error {
		res := tx.Model(&model.SiteConfig{}).Where("slug = ?", cfg.Slug).
			Select("netbox_site", "netbox_location", "ephemeral_pool_start",
				"ephemeral_pool_end", "relay_pools", "promiscuous").
			Updates(cfg)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrSiteConfigNotFound
		}
		return nil
	})
}

func (s *Store) DeleteSiteConfig(slug string) error {
	return s.db.Delete(&model.SiteConfig{}, "slug = ?", slug).Error
}

// BumpSiteSnapshotMeta sets last_snapshot_seq to the next value of the
// site_snapshot_seq sequence and last_snapshot_hash to newHash in one
// UPDATE, returning the new seq. Called by the snapshot dispatcher after
// determining the snapshot content changed.
//
// The value comes from the sequence, not the row, so it is above every
// SeqNum issued before, for this slug or any other. A slug's workers keep
// their persisted SeqNum when its row is deleted, and drop any snapshot
// whose SeqNum is not above it; a per-row counter would restart at 1 when
// the row is re-created.
func (s *Store) BumpSiteSnapshotMeta(slug, newHash string) (int64, error) {
	var newSeq int64
	err := s.db.Raw(`
		UPDATE site_configs
		SET last_snapshot_seq = nextval('site_snapshot_seq'),
		    last_snapshot_hash = ?
		WHERE slug = ?
		RETURNING last_snapshot_seq
	`, newHash, slug).Scan(&newSeq).Error
	return newSeq, err
}
