package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/opstate"
)

// ErrMachineNotFound is returned when an operation targets a machine ID
// that does not exist in the database.
var ErrMachineNotFound = errors.New("machine not found")

// ErrMachineLocked is returned when a lifecycle operation is refused
// because the machine is flagged locked.
var ErrMachineLocked = errors.New("machine is locked")

// CreateMachine inserts a new Machine row. NetBoxSerial is required; a
// unique index enforces one row per serial.
func (s *Store) CreateMachine(m *model.Machine) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	if m.NetBoxSerial == "" {
		return errors.New("NetBoxSerial required")
	}
	if m.State == "" {
		m.State = model.MachineStateNew
	}
	return s.db.Create(m).Error
}

func (s *Store) GetMachine(id uuid.UUID) (*model.Machine, error) {
	var m model.Machine
	if err := s.db.First(&m, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// GetMachineByNetBoxDeviceID looks up a live machine by its NetBox Device
// integer ID. Returns gorm.ErrRecordNotFound for unknown IDs.
func (s *Store) GetMachineByNetBoxDeviceID(deviceID int) (*model.Machine, error) {
	var m model.Machine
	if err := s.db.First(&m, "netbox_device_id = ?", deviceID).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// GetMachineByNetBoxSerial returns the machine with the given serial.
func (s *Store) GetMachineByNetBoxSerial(serial string) (*model.Machine, error) {
	var m model.Machine
	err := s.db.First(&m, "netbox_serial = ?", serial).Error
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMachines returns all machines, optionally filtered by state. Site
// filtering is handled at snapshot-build time by joining against NetBox
// data; the Machine row doesn't carry a site.
func (s *Store) ListMachines(state *model.MachineState) ([]model.Machine, error) {
	q := s.db
	if state != nil {
		q = q.Where("state = ?", *state)
	}
	var out []model.Machine
	if err := q.Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}

// ListMachinesByDeviceIDs returns machines matching any of the given NetBox
// device IDs. Used by the snapshot dispatcher to join Postgres state onto
// the NetBox device set for a site.
func (s *Store) ListMachinesByDeviceIDs(ids []int) ([]model.Machine, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []model.Machine
	err := s.db.Where("netbox_device_id IN ?", ids).Find(&out).Error
	return out, err
}

// ListMachinesByIDs returns machines matching any of the given canonical
// UUIDs. Used to enrich operation lists with machine names without a
// per-row lookup.
func (s *Store) ListMachinesByIDs(ids []uuid.UUID) ([]model.Machine, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []model.Machine
	err := s.db.Where("id IN ?", ids).Find(&out).Error
	return out, err
}

// SetMachineDesiredOS writes ONLY the desired_os column, for
// PATCH /machines/:id. Every other machine column has its own writer (state
// and lock through the operation lifecycle, the NetBox binding through the
// mirror), so a write-back of a row the caller read earlier would revert
// whatever those writers committed in between. Returns ErrMachineNotFound
// when no row matches, so a machine deleted mid-request stays deleted.
func (s *Store) SetMachineDesiredOS(id uuid.UUID, desiredOS string) error {
	res := s.db.Model(&model.Machine{}).Where("id = ?", id).Update("desired_os", desiredOS)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrMachineNotFound
	}
	return nil
}

// DeleteMachineIfIdle hard-deletes a machine row and its child rows
// (operations and log_envelopes) unless the machine has an active operation
// (opstate.ActiveStatuses), in which case it changes nothing and returns
// false. machine_interfaces cascades automatically; operations and
// log_envelopes do not (no ON DELETE CASCADE on those FKs). A machine that
// no longer exists returns true: the caller wanted it gone.
//
// The active-op check and the deletes run in one transaction that holds the
// machine row FOR UPDATE. Inserting an operation takes FOR KEY SHARE on that
// row through fk_operations_machine, and the two locks conflict, so a
// concurrent op create is serialized against the delete: a create that
// commits first is seen by the check (the machine is kept), and one that
// reaches the row after the lock is taken waits, then is refused because the
// machine is gone (CreateMachineOp reads the row FOR KEY SHARE and reports
// it not found; CreatePowerOp's insert fails the foreign key). A check made
// outside this transaction could not give that
// guarantee: an op created between it and the delete would be removed along
// with the machine after the operator had been handed its ID.
//
// Called by the mirror on NetBox device deletion. Per spec, hard-delete
// is the only EOL path; soft-decommission is out of scope.
func (s *Store) DeleteMachineIfIdle(id uuid.UUID) (bool, error) {
	deleted := false
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var m model.Machine
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("id").Where("id = ?", id).First(&m).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			deleted = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock machine: %w", err)
		}
		var active int64
		if err := tx.Model(&model.Operation{}).
			Where("machine_id = ? AND status IN ?", id, opstate.ActiveStatuses).
			Count(&active).Error; err != nil {
			return fmt.Errorf("count active operations: %w", err)
		}
		if active > 0 {
			return nil
		}
		if err := tx.Where("machine_id = ?", id).Delete(&model.Operation{}).Error; err != nil {
			return fmt.Errorf("delete operations: %w", err)
		}
		// log_envelopes is keyed by machine_id; clear it too so
		// historical envelopes don't outlive the row.
		if err := tx.Exec("DELETE FROM log_envelopes WHERE machine_id = ?", id).Error; err != nil {
			return fmt.Errorf("delete log_envelopes: %w", err)
		}
		if err := tx.Delete(&model.Machine{}, "id = ?", id).Error; err != nil {
			return err
		}
		deleted = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return deleted, nil
}

func (s *Store) SetMachineLocked(id uuid.UUID, locked bool) error {
	return s.db.Model(&model.Machine{}).Where("id = ?", id).Update("locked", locked).Error
}

// SetMachineNetBoxDeviceID writes ONLY the netbox_device_id column. Used by the
// mirror's rebind path (a NetBox device re-pointing at an existing serial's
// row). A full-row gorm Save there would clobber columns the mirror
// does not own (state, lock, commission data) with whatever stale value the
// mirror's in-memory struct was loaded with, racing a concurrent writer of
// those columns. The mirror owns only the NetBox binding, so scope the write
// to that column.
func (s *Store) SetMachineNetBoxDeviceID(id uuid.UUID, netboxDeviceID int) error {
	return s.db.Model(&model.Machine{}).Where("id = ?", id).
		Update("netbox_device_id", netboxDeviceID).Error
}

// WithPopAdvisoryLock runs fn while holding a Postgres transaction
// advisory lock keyed on the pop slug. Concurrent callers with the
// same pop block until the holder's transaction commits or rolls back;
// callers with different pop slugs run in parallel.
//
// Used to serialize per-pop monitor-cred writes (deploy_complete and
// bmc_reconcile_report): AWS Secrets Manager has no compare-and-swap
// on PutSecretValue, so two hosts in the same pop reconciling within
// the same second would both read-modify-write the same secret and one
// would lose its entry. The lock orders those writers but does not make
// the read-modify-write atomic: Secrets Manager reads are eventually
// consistent, so a holder can read a version older than the previous
// holder's write. awssecrets.Client.UpsertMonitorEntry guards against
// that itself by refusing reads older than its own last write.
//
// Lock domain: hashtext of "slagworks-bmc-monitor:" + popSlug. The
// "slagworks-bmc-monitor:" namespace prefix prevents collisions with
// other advisory locks if we add them later.
func (s *Store) WithPopAdvisoryLock(ctx context.Context, popSlug string, fn func(context.Context) error) error {
	if popSlug == "" {
		return errors.New("store: WithPopAdvisoryLock empty pop slug")
	}
	key := "slagworks-bmc-monitor:" + popSlug
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// pg_advisory_xact_lock auto-releases at tx end (commit OR
		// rollback); no manual release needed even on error.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", key).Error; err != nil {
			return fmt.Errorf("acquire advisory lock %q: %w", key, err)
		}
		return fn(ctx)
	})
}

func (s *Store) SetMachinePXEEligible(id uuid.UUID, eligible bool) error {
	return s.db.Model(&model.Machine{}).Where("id = ?", id).Update("pxe_eligible", eligible).Error
}

func (s *Store) StoreCommissionData(id uuid.UUID, data json.RawMessage) error {
	now := time.Now().UTC()
	return s.db.Model(&model.Machine{}).Where("id = ?", id).Updates(map[string]any{
		"commission_data": jsonbSafe(data),
		"commissioned_at": now,
	}).Error
}
