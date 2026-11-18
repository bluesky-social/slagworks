package model

import (
	"time"

	"github.com/google/uuid"
)

// ConsoleCast is the per-machine perpetual asciicast row. One per
// machine for the machine's lifetime; events append into
// ConsoleCastEvent against the cast's id.
type ConsoleCast struct {
	ID            int64     `gorm:"primaryKey;autoIncrement"`
	MachineID     uuid.UUID `gorm:"type:uuid;uniqueIndex;not null"`
	CastStartedAt time.Time `gorm:"not null"`
	Width         int       `gorm:"not null;default:80"`
	Height        int       `gorm:"not null;default:24"`
}

func (ConsoleCast) TableName() string { return "console_casts" }

// ConsoleCastEvent is one batched chunk of bytes from the BMC SOL
// stream. t_us is monotonic-per-cast (assigned by the site worker
// using prev_max_us + monotonic_since_pump_start; see the design
// doc for rationale).
type ConsoleCastEvent struct {
	ID         int64     `gorm:"primaryKey;autoIncrement"`
	CastID     int64     `gorm:"not null;index:console_cast_events_cast_t,priority:1;index:console_cast_events_cast_received,priority:1"`
	TUs        int64     `gorm:"column:t_us;not null;index:console_cast_events_cast_t,priority:2"`
	ReceivedAt time.Time `gorm:"not null;default:now();index:console_cast_events_cast_received,priority:2"`
	Data       []byte    `gorm:"not null"`
}

func (ConsoleCastEvent) TableName() string { return "console_cast_events" }
