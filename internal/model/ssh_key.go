package model

import (
	"time"

	"github.com/google/uuid"
)

// SSHKey is a global SSH public key injected into deployed and
// recovery-mode machines as /root/.ssh/authorized_keys entries.
type SSHKey struct {
	ID        uuid.UUID `json:"id" gorm:"type:uuid;primaryKey"`
	PublicKey string    `json:"public_key" gorm:"uniqueIndex;not null"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at" gorm:"autoCreateTime"`
}
