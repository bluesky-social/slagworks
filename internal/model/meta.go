package model

// Meta stores singleton configuration values for the global controller.
// There is exactly one row in this table, keyed by instance_id.
type Meta struct {
	InstanceID string `json:"instance_id" gorm:"primaryKey"`
}
