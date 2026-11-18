package store

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func (s *Store) CreateSSHKey(key *model.SSHKey) error {
	if key.ID == uuid.Nil {
		key.ID = uuid.New()
	}
	return s.db.Create(key).Error
}

func (s *Store) ListSSHKeys() ([]model.SSHKey, error) {
	var keys []model.SSHKey
	return keys, s.db.Order("created_at asc").Find(&keys).Error
}

// ListSSHPublicKeys returns just the public-key strings in stable order,
// as needed by the snapshot body.
func (s *Store) ListSSHPublicKeys() ([]string, error) {
	keys, err := s.ListSSHKeys()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.PublicKey
	}
	return out, nil
}

func (s *Store) DeleteSSHKey(id uuid.UUID) error {
	res := s.db.Delete(&model.SSHKey{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("ssh key %s not found", id)
	}
	return nil
}
