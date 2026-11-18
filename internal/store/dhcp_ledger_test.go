package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Phase 3 (docs/plans/2026-07-13-dhcp-lease-visibility.md) coverage:
// RecordDHCPLeaseEvent's fold matrix and ListDHCPLeaseHistory's
// filters. testStore is the shared dockertest Postgres helper (see
// testpg_test.go).

// openRow fetches the single open (released_at IS NULL) ledger row for
// (siteSlug, workerID, mac), failing the test if there isn't exactly
// one — the per-MAC unique partial index should guarantee this after
// every fold.
func openRowByMAC(t *testing.T, st *Store, siteSlug, workerID, mac string) model.DHCPLeaseLedgerEntry {
	t.Helper()
	var rows []model.DHCPLeaseLedgerEntry
	if err := st.db.Where("site_slug = ? AND worker_id = ? AND mac = ? AND released_at IS NULL", siteSlug, workerID, mac).
		Find(&rows).Error; err != nil {
		t.Fatalf("query open row by mac: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d open rows for mac %q, want 1: %+v", len(rows), mac, rows)
	}
	return rows[0]
}

func openRowsByIP(t *testing.T, st *Store, siteSlug, workerID, ip string) []model.DHCPLeaseLedgerEntry {
	t.Helper()
	var rows []model.DHCPLeaseLedgerEntry
	if err := st.db.Where("site_slug = ? AND worker_id = ? AND ip = ? AND released_at IS NULL", siteSlug, workerID, ip).
		Find(&rows).Error; err != nil {
		t.Fatalf("query open rows by ip: %v", err)
	}
	return rows
}

// TestRecordDHCPLeaseEvent_AllocateThenRenew_BumpsNotInserts pins that
// a renew for the SAME (mac, ip) updates the existing open row's
// last_seen/expires_at rather than inserting a second row.
func TestRecordDHCPLeaseEvent_AllocateThenRenew_BumpsNotInserts(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	expiry1 := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:01", "10.99.1.100", "dynamic", "10.99.1.0/24", nil, expiry1); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	row := openRowByMAC(t, st, "pop1", "worker-1", "aa:bb:cc:00:00:01")
	firstSeen := row.FirstSeen
	if !row.ExpiresAt.Equal(expiry1) {
		t.Fatalf("ExpiresAt after allocate = %v, want %v", row.ExpiresAt, expiry1)
	}

	expiry2 := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "renew", "aa:bb:cc:00:00:01", "10.99.1.100", "dynamic", "10.99.1.0/24", nil, expiry2); err != nil {
		t.Fatalf("renew: %v", err)
	}
	row = openRowByMAC(t, st, "pop1", "worker-1", "aa:bb:cc:00:00:01")
	if !row.ExpiresAt.Equal(expiry2) {
		t.Errorf("ExpiresAt after renew = %v, want %v", row.ExpiresAt, expiry2)
	}
	if !row.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen changed on renew: %v -> %v, want unchanged", firstSeen, row.FirstSeen)
	}

	// Exactly one row total for this mac — a renew must not have inserted.
	var count int64
	if err := st.db.Model(&model.DHCPLeaseLedgerEntry{}).
		Where("site_slug = ? AND worker_id = ? AND mac = ?", "pop1", "worker-1", "aa:bb:cc:00:00:01").
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("got %d total rows for the mac, want 1 (renew must bump, not insert)", count)
	}
}

// TestRecordDHCPLeaseEvent_IPTakeoverByNewMAC_ClosesOldRowSuperseded
// pins the per-IP fold: when a different MAC allocates an IP another
// MAC still holds open, the old holder's row closes as superseded and
// the per-(site,worker,ip) open-row index holds — never two open rows
// for the same IP.
func TestRecordDHCPLeaseEvent_IPTakeoverByNewMAC_ClosesOldRowSuperseded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	ip := "10.99.1.100"
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:01", ip, "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:02", ip, "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("takeover allocate: %v", err)
	}

	open := openRowsByIP(t, st, "pop1", "worker-1", ip)
	if len(open) != 1 {
		t.Fatalf("got %d open rows for ip %q after takeover, want 1: %+v", len(open), ip, open)
	}
	if open[0].MAC != "aa:bb:cc:00:00:02" {
		t.Errorf("open row's MAC = %q, want the new holder aa:bb:cc:00:00:02", open[0].MAC)
	}

	var closed model.DHCPLeaseLedgerEntry
	if err := st.db.Where("site_slug = ? AND worker_id = ? AND ip = ? AND mac = ?", "pop1", "worker-1", ip, "aa:bb:cc:00:00:01").
		First(&closed).Error; err != nil {
		t.Fatalf("query old holder's closed row: %v", err)
	}
	if closed.ReleasedAt == nil || closed.ReleaseReason != "superseded" {
		t.Errorf("old holder's row = %+v, want released_at set + release_reason=superseded", closed)
	}
}

// TestRecordDHCPLeaseEvent_MACMovesIP_ClosesOldRowSuperseded pins the
// per-MAC fold: when the SAME MAC allocates a different IP, the old
// (mac, oldIP) row closes as superseded and a fresh open row exists
// for (mac, newIP).
func TestRecordDHCPLeaseEvent_MACMovesIP_ClosesOldRowSuperseded(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mac := "aa:bb:cc:00:00:03"
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", mac, "10.99.1.100", "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", mac, "10.99.1.200", "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("moved allocate: %v", err)
	}

	row := openRowByMAC(t, st, "pop1", "worker-1", mac)
	if row.IP != "10.99.1.200" {
		t.Errorf("open row's IP = %q, want the new IP 10.99.1.200", row.IP)
	}

	var closed model.DHCPLeaseLedgerEntry
	if err := st.db.Where("site_slug = ? AND worker_id = ? AND mac = ? AND ip = ?", "pop1", "worker-1", mac, "10.99.1.100").
		First(&closed).Error; err != nil {
		t.Fatalf("query old ip's closed row: %v", err)
	}
	if closed.ReleasedAt == nil || closed.ReleaseReason != "superseded" {
		t.Errorf("old IP's row = %+v, want released_at set + release_reason=superseded", closed)
	}
}

// TestRecordDHCPLeaseEvent_Delete_ClosesMatchingOpenRow pins that a
// delete closes exactly the matching open (mac, ip) row with
// release_reason=deleted, AND bumps last_seen (roast finding,
// 2026-07-13: a close that doesn't touch last_seen sorts behind
// unrelated, older-but-still-open rows in ListDHCPLeaseHistory's
// newest-first order, hiding a just-deleted lease behind the default
// limit).
func TestRecordDHCPLeaseEvent_Delete_ClosesMatchingOpenRow(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	mac, ip := "aa:bb:cc:00:00:04", "10.99.1.101"
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", mac, ip, "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	beforeDelete := time.Now()
	time.Sleep(10 * time.Millisecond)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "delete", mac, ip, "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var row model.DHCPLeaseLedgerEntry
	if err := st.db.Where("site_slug = ? AND worker_id = ? AND mac = ? AND ip = ?", "pop1", "worker-1", mac, ip).
		First(&row).Error; err != nil {
		t.Fatalf("query row: %v", err)
	}
	if row.ReleasedAt == nil || row.ReleaseReason != "deleted" {
		t.Errorf("row = %+v, want released_at set + release_reason=deleted", row)
	}
	if !row.LastSeen.After(beforeDelete) {
		t.Errorf("LastSeen = %v, want it bumped to at/after the delete (before-delete was %v)", row.LastSeen, beforeDelete)
	}
}

// TestRecordDHCPLeaseEvent_DeleteForNonexistent_NoOp pins that a
// delete for a (mac, ip) with no matching open row is a no-op — no
// error, no row created or modified.
func TestRecordDHCPLeaseEvent_DeleteForNonexistent_NoOp(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "delete", "aa:bb:cc:00:00:05", "10.99.1.102", "dynamic", "p", nil, time.Now()); err != nil {
		t.Fatalf("delete for nonexistent row: %v", err)
	}

	var count int64
	if err := st.db.Model(&model.DHCPLeaseLedgerEntry{}).
		Where("site_slug = ? AND worker_id = ? AND mac = ?", "pop1", "worker-1", "aa:bb:cc:00:00:05").
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("got %d rows after a no-op delete, want 0", count)
	}
}

// TestRecordDHCPLeaseEvent_MachineIDPersisted pins that a non-nil
// machineID round-trips, and a nil one (unresolved MAC on the
// promiscuous OOB segment) persists as NULL, not a zero UUID.
func TestRecordDHCPLeaseEvent_MachineIDPersisted(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	machineID := uuid.New()
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:06", "10.99.1.103", "static", "p", &machineID, expiry); err != nil {
		t.Fatalf("allocate with machine id: %v", err)
	}
	row := openRowByMAC(t, st, "pop1", "worker-1", "aa:bb:cc:00:00:06")
	if row.MachineID == nil || *row.MachineID != machineID {
		t.Errorf("MachineID = %v, want %s", row.MachineID, machineID)
	}

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:07", "10.99.1.104", "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("allocate without machine id: %v", err)
	}
	row = openRowByMAC(t, st, "pop1", "worker-1", "aa:bb:cc:00:00:07")
	if row.MachineID != nil {
		t.Errorf("MachineID = %v, want nil for an unresolved MAC", row.MachineID)
	}
}

// TestRecordDHCPLeaseEvent_DifferentWorkersDontCollide pins that the
// fold is scoped per (site_slug, worker_id): two workers reusing the
// SAME IP string for DIFFERENT MACs (each worker's own dynamic slice
// can legitimately number addresses the same way) each keep their own
// open row — the per-worker scope in every WHERE clause and the
// composite unique indexes is what makes this safe.
func TestRecordDHCPLeaseEvent_DifferentWorkersDontCollide(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	ip := "10.99.1.105"
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:00:08", ip, "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("worker-1 allocate: %v", err)
	}
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-2", "allocate", "aa:bb:cc:00:00:09", ip, "dynamic", "p", nil, expiry); err != nil {
		t.Fatalf("worker-2 allocate: %v", err)
	}

	row1 := openRowByMAC(t, st, "pop1", "worker-1", "aa:bb:cc:00:00:08")
	row2 := openRowByMAC(t, st, "pop1", "worker-2", "aa:bb:cc:00:00:09")
	if row1.ReleasedAt != nil || row2.ReleasedAt != nil {
		t.Errorf("cross-worker allocate closed a row it shouldn't have: row1=%+v row2=%+v", row1, row2)
	}
}

// TestRecordDHCPLeaseEvent_UnknownEventRejected pins that an event
// string outside allocate/renew/delete is rejected with an error
// rather than silently folded as an allocate/renew (roast finding,
// 2026-07-13: an unvalidated event value would corrupt the ledger).
func TestRecordDHCPLeaseEvent_UnknownEventRejected(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "bogus", "aa:bb:cc:00:00:99", "10.99.1.199", "dynamic", "p", nil, time.Now())
	if err == nil {
		t.Fatal("expected an error for an unknown event value")
	}

	var count int64
	if err := st.db.Model(&model.DHCPLeaseLedgerEntry{}).
		Where("site_slug = ? AND mac = ?", "pop1", "aa:bb:cc:00:00:99").
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("got %d rows after a rejected event, want 0 (no ledger mutation)", count)
	}
}

// TestListDHCPLeaseHistory_FiltersAndOrdering pins mac/ip filters and
// newest-first ordering by last_seen.
func TestListDHCPLeaseHistory_FiltersAndOrdering(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)

	// mac1/ip1 allocated, then renewed (bumps last_seen forward).
	if err := st.RecordDHCPLeaseEvent(ctx, "pop2", "worker-1", "allocate", "aa:bb:cc:00:01:01", "10.99.2.10", "dynamic", "p", nil, expiry); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop2", "worker-1", "allocate", "aa:bb:cc:00:01:02", "10.99.2.11", "dynamic", "p", nil, expiry); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := st.RecordDHCPLeaseEvent(ctx, "pop2", "worker-1", "renew", "aa:bb:cc:00:01:01", "10.99.2.10", "dynamic", "p", nil, expiry.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A different site must not leak into pop2's history.
	if err := st.RecordDHCPLeaseEvent(ctx, "pop3", "worker-1", "allocate", "aa:bb:cc:00:01:01", "10.99.2.10", "dynamic", "p", nil, expiry); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListDHCPLeaseHistory(ctx, "pop2", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d rows for pop2, want 2 (site-scoped): %+v", len(all), all)
	}
	// Newest-first by last_seen: the renewed mac1/ip1 row (bumped last)
	// comes before mac2/ip2.
	if all[0].MAC != "aa:bb:cc:00:01:01" {
		t.Errorf("all[0].MAC = %q, want the more-recently-touched mac1 (newest-first)", all[0].MAC)
	}

	byMAC, err := st.ListDHCPLeaseHistory(ctx, "pop2", "aa:bb:cc:00:01:02", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byMAC) != 1 || byMAC[0].MAC != "aa:bb:cc:00:01:02" {
		t.Fatalf("mac filter got %+v, want exactly mac2's row", byMAC)
	}

	byIP, err := st.ListDHCPLeaseHistory(ctx, "pop2", "", "10.99.2.10", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byIP) != 1 || byIP[0].IP != "10.99.2.10" {
		t.Fatalf("ip filter got %+v, want exactly ip1's row", byIP)
	}

	limited, err := st.ListDHCPLeaseHistory(ctx, "pop2", "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit=1 got %d rows, want 1", len(limited))
	}
}

// TestPruneDHCPLeaseLedger_OnlyClosedRowsOlderThanCutoff pins the
// retention sweep: closed rows older than the cutoff are deleted;
// closed rows newer than it and ALL open rows (regardless of age)
// survive.
func TestPruneDHCPLeaseLedger_OnlyClosedRowsOlderThanCutoff(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	expiry := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	old := time.Now().Add(-100 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	mkClosed := func(mac, ip string, releasedAt time.Time) {
		t.Helper()
		if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", mac, ip, "dynamic", "p", nil, expiry); err != nil {
			t.Fatal(err)
		}
		if err := st.db.Model(&model.DHCPLeaseLedgerEntry{}).
			Where("site_slug = ? AND mac = ? AND ip = ?", "pop1", mac, ip).
			Updates(map[string]any{"released_at": releasedAt, "release_reason": "deleted"}).Error; err != nil {
			t.Fatal(err)
		}
	}

	mkClosed("aa:bb:cc:00:02:01", "10.99.3.1", old)    // should be pruned
	mkClosed("aa:bb:cc:00:02:02", "10.99.3.2", recent) // should survive
	if err := st.RecordDHCPLeaseEvent(ctx, "pop1", "worker-1", "allocate", "aa:bb:cc:00:02:03", "10.99.3.3", "dynamic", "p", nil, expiry); err != nil {
		t.Fatal(err) // open row, should survive regardless of age
	}

	cutoff := time.Now().Add(-90 * 24 * time.Hour)
	n, err := st.PruneDHCPLeaseLedger(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1", n)
	}

	var remaining []model.DHCPLeaseLedgerEntry
	if err := st.db.Where("site_slug = ?", "pop1").Find(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("got %d remaining rows, want 2 (recent-closed + open): %+v", len(remaining), remaining)
	}
}
