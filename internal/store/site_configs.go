package store

import (
	"github.com/bluesky-social/slagworks/internal/model"
)

// CreateSiteConfig inserts a SiteConfig after validating the payload.
func (s *Store) CreateSiteConfig(cfg *model.SiteConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.db.Create(cfg).Error
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

func (s *Store) UpdateSiteConfig(cfg *model.SiteConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.db.Save(cfg).Error
}

func (s *Store) DeleteSiteConfig(slug string) error {
	return s.db.Delete(&model.SiteConfig{}, "slug = ?", slug).Error
}

// BumpSiteSnapshotMeta atomically bumps last_snapshot_seq by 1 and sets
// last_snapshot_hash in one UPDATE, returning the new seq. Called by the
// snapshot dispatcher after determining the snapshot content changed.
func (s *Store) BumpSiteSnapshotMeta(slug, newHash string) (int64, error) {
	var newSeq int64
	err := s.db.Raw(`
		UPDATE site_configs
		SET last_snapshot_seq = last_snapshot_seq + 1,
		    last_snapshot_hash = ?
		WHERE slug = ?
		RETURNING last_snapshot_seq
	`, newHash, slug).Scan(&newSeq).Error
	return newSeq, err
}
