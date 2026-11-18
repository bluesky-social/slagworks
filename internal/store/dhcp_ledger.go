package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
)

// RecordDHCPLeaseEvent folds one lease-mutation event into the ledger,
// in a single transaction, per docs/plans/2026-07-13-dhcp-lease-
// visibility.md Phase 3:
//
//   - allocate/renew: first close (as "superseded") any OTHER open row
//     for the same (site, worker, mac) — a different IP — and any
//     OTHER open row for the same (site, worker, ip) — a different mac
//     — then upsert the open (mac, ip) row itself (bump last_seen/
//     expires_at if it already exists and is open, insert otherwise).
//     The per-IP close is what keeps the ledger honoring DHCP's
//     one-holder-per-IP invariant when an expired address is
//     reallocated to a new MAC without a delivered delete observation
//     (worker crash between the expiry-sweep delete and the
//     subsequent allocate emits distinct observations; the delete can
//     be lost).
//   - delete: close ONLY the matching open (site, worker, mac, ip) row
//     as "deleted". No-op if no such open row exists (a replayed or
//     out-of-order delete for a row already closed some other way).
func (s *Store) RecordDHCPLeaseEvent(ctx context.Context, siteSlug, workerID, event, mac, ip, kind, pool string, machineID *uuid.UUID, expiresAt time.Time) error {
	// Reject anything outside the three events the fold below knows how
	// to handle. Without this, a malformed/version-skewed observation
	// (a stray string in place of "allocate"/"renew"/"delete") would
	// silently fall through the "not delete" branch and get folded as
	// an allocate/renew — corrupting the ledger instead of failing loud
	// so the worker retries or an operator notices (roast finding,
	// 2026-07-13).
	switch event {
	case "allocate", "renew", "delete":
	default:
		return fmt.Errorf("dhcp_lease event: unknown event %q, want allocate/renew/delete", event)
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()

		if event == "delete" {
			return tx.Model(&model.DHCPLeaseLedgerEntry{}).
				Where("site_slug = ? AND worker_id = ? AND mac = ? AND ip = ? AND released_at IS NULL",
					siteSlug, workerID, mac, ip).
				Updates(map[string]any{
					"last_seen":      now,
					"released_at":    now,
					"release_reason": "deleted",
				}).Error
		}

		// allocate/renew. Close any other open row holding this MAC
		// under a different IP.
		if err := tx.Model(&model.DHCPLeaseLedgerEntry{}).
			Where("site_slug = ? AND worker_id = ? AND mac = ? AND ip <> ? AND released_at IS NULL",
				siteSlug, workerID, mac, ip).
			Updates(map[string]any{
				"last_seen":      now,
				"released_at":    now,
				"release_reason": "superseded",
			}).Error; err != nil {
			return err
		}

		// Close any other open row holding this IP under a different MAC.
		if err := tx.Model(&model.DHCPLeaseLedgerEntry{}).
			Where("site_slug = ? AND worker_id = ? AND ip = ? AND mac <> ? AND released_at IS NULL",
				siteSlug, workerID, ip, mac).
			Updates(map[string]any{
				"last_seen":      now,
				"released_at":    now,
				"release_reason": "superseded",
			}).Error; err != nil {
			return err
		}

		// Upsert the open (mac, ip) row: bump last_seen/expires_at (and
		// re-freshen machine_id/kind/pool, which can legitimately change
		// between events — a machine gets registered after its first
		// lease, or a reservation is added) if an open row already
		// exists; otherwise insert one with first_seen = now.
		res := tx.Model(&model.DHCPLeaseLedgerEntry{}).
			Where("site_slug = ? AND worker_id = ? AND mac = ? AND ip = ? AND released_at IS NULL",
				siteSlug, workerID, mac, ip).
			Updates(map[string]any{
				"last_seen":  now,
				"expires_at": expiresAt,
				"machine_id": machineID,
				"kind":       kind,
				"pool":       pool,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			return nil
		}

		return tx.Create(&model.DHCPLeaseLedgerEntry{
			SiteSlug:  siteSlug,
			WorkerID:  workerID,
			MAC:       mac,
			IP:        ip,
			MachineID: machineID,
			Kind:      kind,
			Pool:      pool,
			FirstSeen: now,
			LastSeen:  now,
			ExpiresAt: expiresAt,
		}).Error
	})
}

// dhcpLeaseHistoryDefaultLimit / Max bound the history read: default
// matches Phase 3's spec (100), capped at 1000 so an unbounded ?limit=
// query param can't force a full-table scan.
const (
	dhcpLeaseHistoryDefaultLimit = 100
	dhcpLeaseHistoryMaxLimit     = 1000
)

// ListDHCPLeaseHistory returns ledger rows for siteSlug, newest-first
// by last_seen, optionally filtered by mac and/or ip (both "" = no
// filter on that column). limit <= 0 uses the default (100); anything
// above the max (1000) is clamped.
func (s *Store) ListDHCPLeaseHistory(ctx context.Context, siteSlug, mac, ip string, limit int) ([]model.DHCPLeaseLedgerEntry, error) {
	if limit <= 0 {
		limit = dhcpLeaseHistoryDefaultLimit
	}
	if limit > dhcpLeaseHistoryMaxLimit {
		limit = dhcpLeaseHistoryMaxLimit
	}

	q := s.db.WithContext(ctx).Model(&model.DHCPLeaseLedgerEntry{}).Where("site_slug = ?", siteSlug)
	if mac != "" {
		q = q.Where("mac = ?", mac)
	}
	if ip != "" {
		q = q.Where("ip = ?", ip)
	}

	var rows []model.DHCPLeaseLedgerEntry
	if err := q.Order("last_seen DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// PruneDHCPLeaseLedger deletes CLOSED rows (released_at IS NOT NULL)
// with released_at older than olderThan, per Phase 3's retention rule
// (90 days in the periodic global sweep — see cmd/slagworksd/global.go).
// Open rows are never pruned here regardless of age; they age out
// naturally via the fold's supersede/delete paths.
func (s *Store) PruneDHCPLeaseLedger(ctx context.Context, olderThan time.Time) (int64, error) {
	res := s.db.WithContext(ctx).
		Where("released_at IS NOT NULL AND released_at < ?", olderThan).
		Delete(&model.DHCPLeaseLedgerEntry{})
	return res.RowsAffected, res.Error
}
