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

// GetInstanceID returns the global instance ID. The ID is created by goose
// migration 00006 on first boot — this method is read-only.
func (s *Store) GetInstanceID() (string, error) {
	var meta model.Meta
	if err := s.db.First(&meta).Error; err != nil {
		return "", fmt.Errorf("querying instance ID (has migration 00006 run?): %w", err)
	}
	return meta.InstanceID, nil
}
