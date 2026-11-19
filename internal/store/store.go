package store

import (
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

type Store struct {
	db  *gorm.DB
	log *slog.Logger
}

func New(db *gorm.DB) *Store {
	return &Store{db: db, log: slog.Default()}
}

func (s *Store) SetLogger(l *slog.Logger) {
	s.log = l
}

func (s *Store) DB() *gorm.DB { return s.db }

// --- Meta ---

// GetInstanceID returns the global instance ID. Migration 00032 creates the
// ID when Migrate first runs against a database; this method only reads it.
func (s *Store) GetInstanceID() (string, error) {
	var meta model.Meta
	if err := s.db.First(&meta).Error; err != nil {
		return "", fmt.Errorf("reading instance ID from the meta table (migration 00032 seeds it; check that Migrate ran against this database): %w", err)
	}
	return meta.InstanceID, nil
}
