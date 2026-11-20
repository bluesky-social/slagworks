package worker

import (
	"database/sql"
	"net"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// dumpUserSchema returns the sqlite_master DDL for every user object
// (tables + indexes), keyed by object name, excluding goose's own
// bookkeeping table. Used to assert the goose-migrated schema is
// byte-identical to what the pre-goose AutoMigrate path produced.
func dumpUserSchema(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer raw.Close()

	rows, err := raw.Query("SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'goose_%' ORDER BY name")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[name] = ddl
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}

// preGooseSchema is the exact DDL that GORM AutoMigrate emitted for the
// site worker's local DB before the goose migration (fable-review Tier
// 3). Captured from a fresh AutoMigrate'd DB via sqlite_master. The
// adopt-existing-file test hand-writes this to simulate a prod oob-gw's
// pre-migration worker.db, since AutoMigrate is gone from the code.
var preGooseSchema = []string{
	"CREATE TABLE `kv` (`key` text,`value` text,PRIMARY KEY (`key`))",
	"CREATE TABLE `outbox` (`id` integer PRIMARY KEY AUTOINCREMENT,`payload` text NOT NULL,`created_at` datetime)",
	"CREATE TABLE `cached_machines` (`machine_id` text,`state` text NOT NULL,`data` text NOT NULL,PRIMARY KEY (`machine_id`))",
	"CREATE TABLE `dhcp_leases` (`mac` text,`ip` text NOT NULL,`expires_at` datetime NOT NULL,PRIMARY KEY (`mac`))",
	"CREATE TABLE `current_deploy_ops` (`machine_id` text,`operation_id` text NOT NULL,PRIMARY KEY (`machine_id`))",
}

// TestLocalDB_FreshInstallSchema asserts that a fresh OpenLocalDB (which
// now runs goose.Up) produces exactly the schema AutoMigrate used to
// produce — same tables, same DDL, same implicit autoindexes. This is
// the "fresh install matches old shape" half of the migration contract.
func TestLocalDB_FreshInstallSchema(t *testing.T) {
	// A DB built purely by hand-writing the AutoMigrate DDL: the
	// reference shape.
	refPath := filepath.Join(t.TempDir(), "ref.db")
	refDB, err := sql.Open("sqlite", refPath)
	if err != nil {
		t.Fatalf("open ref: %v", err)
	}
	for _, ddl := range preGooseSchema {
		if _, err := refDB.Exec(ddl); err != nil {
			t.Fatalf("exec ref ddl %q: %v", ddl, err)
		}
	}
	refDB.Close()
	want := dumpUserSchema(t, refPath)

	// A DB built by the real OpenLocalDB path (goose).
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	db, err := OpenLocalDB(freshPath)
	if err != nil {
		t.Fatalf("OpenLocalDB: %v", err)
	}
	closeLocalDB(t, db)
	got := dumpUserSchema(t, freshPath)

	assertSchemaEqual(t, want, got)
}

// TestLocalDB_AdoptExistingFile is the load-bearing deployment test: a
// prod oob-gw already has a worker.db created by the old AutoMigrate path
// with real rows in it. When the new binary starts, OpenLocalDB must
// adopt that file in place — data intact, no re-create — and land goose
// version 1.
func TestLocalDB_AdoptExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")

	// 1. Create the DB the OLD way: hand-write the pre-goose schema and
	//    seed rows across every table, exactly as a prod worker.db would
	//    look before the upgrade.
	seedDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	for _, ddl := range preGooseSchema {
		if _, err := seedDB.Exec(ddl); err != nil {
			t.Fatalf("exec seed ddl: %v", err)
		}
	}
	seedMachineID := uuid.New()
	seeds := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO kv (key, value) VALUES (?, ?)", []any{"worker_id", "worker-persisted"}},
		{"INSERT INTO kv (key, value) VALUES (?, ?)", []any{"worker_fqdn", "oob-gw-1.pop3"}},
		{"INSERT INTO kv (key, value) VALUES (?, ?)", []any{"sync_cursor", "1234"}},
		{"INSERT INTO outbox (payload) VALUES (?)", []any{`{"event":"phone_home"}`}},
		{"INSERT INTO cached_machines (machine_id, state, data) VALUES (?, ?, ?)", []any{seedMachineID.String(), "running", `{"id":"` + seedMachineID.String() + `"}`}},
		{"INSERT INTO dhcp_leases (mac, ip, expires_at) VALUES (?, ?, ?)", []any{"52:54:00:aa:bb:01", "10.0.0.50", time.Now().Add(time.Hour)}},
		{"INSERT INTO current_deploy_ops (machine_id, operation_id) VALUES (?, ?)", []any{seedMachineID.String(), "op-abc"}},
	}
	for _, s := range seeds {
		if _, err := seedDB.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed insert %q: %v", s.q, err)
		}
	}
	seedDB.Close()

	// 2. Open with the NEW code. goose.Up must adopt in place.
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatalf("OpenLocalDB on existing file: %v", err)
	}

	// 3. Every seeded row survived.
	if id, fqdn := db.GetWorkerIdentity(); id != "worker-persisted" || fqdn != "oob-gw-1.pop3" {
		t.Errorf("worker identity lost: id=%q fqdn=%q", id, fqdn)
	}
	if got := db.GetSyncCursor(); got != 1234 {
		t.Errorf("sync cursor lost: %d", got)
	}
	if pending := mustListOutbox(t, db); len(pending) != 1 || pending[0].Obs.Event != "phone_home" {
		t.Errorf("outbox row lost: %+v", pending)
	}
	if machines := db.LoadMachineCache(); len(machines) != 1 {
		t.Errorf("cached machine lost: %+v", machines)
	}
	if leases := db.LoadDHCPLeases(); len(leases) != 1 {
		t.Errorf("dhcp lease lost: %+v", leases)
	}
	if op := db.GetCurrentDeployOp(seedMachineID.String()); op != "op-abc" {
		t.Errorf("current deploy op lost: %q", op)
	}
	closeLocalDB(t, db)

	// 4. goose version 1 landed, and the schema is unchanged from the
	//    pre-goose shape (adoption did not re-create tables).
	if v := gooseVersion(t, path); v != 1 {
		t.Errorf("goose version = %d, want 1", v)
	}
	refPath := filepath.Join(t.TempDir(), "ref.db")
	refDB, _ := sql.Open("sqlite", refPath)
	for _, ddl := range preGooseSchema {
		if _, err := refDB.Exec(ddl); err != nil {
			t.Fatalf("exec ref ddl: %v", err)
		}
	}
	refDB.Close()
	assertSchemaEqual(t, dumpUserSchema(t, refPath), dumpUserSchema(t, path))
}

// TestLocalDB_ReopenIdempotent verifies a second OpenLocalDB on an
// already-migrated DB is a clean no-op (goose sees version 1, applies
// nothing) — the ordinary worker-restart path.
func TestLocalDB_ReopenIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.db")
	db1, err := OpenLocalDB(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	db1.SetWorkerIdentity("w1", "host1")
	closeLocalDB(t, db1)

	db2, err := OpenLocalDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if id, _ := db2.GetWorkerIdentity(); id != "w1" {
		t.Errorf("identity lost across reopen: %q", id)
	}
	closeLocalDB(t, db2)

	if v := gooseVersion(t, path); v != 1 {
		t.Errorf("goose version = %d, want 1", v)
	}
}

// TestLocalDB_OpensInWALMode verifies OpenLocalDB leaves the file in WAL
// journal mode, as LocalDB's doc comment promises. In the default
// rollback-journal (delete) mode a reader and a writer block each other
// and only the driver's 5s busy timeout absorbs the wait; past it,
// AppendOutbox fails and the observation stays in memory only. The
// glebarez/go-sqlite driver applies pragmas only from _pragma=... DSN
// parameters, so a mattn-style _journal_mode=WAL is silently ignored.
//
// The existing-file case is the upgrade path: a worker.db created in
// delete mode by an earlier binary must be switched to WAL on open.
func TestLocalDB_OpensInWALMode(t *testing.T) {
	t.Run("fresh file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "worker.db")
		assertWALAfterOpen(t, path)
	})

	t.Run("existing delete-mode file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "worker.db")
		raw, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open raw sqlite: %v", err)
		}
		var mode string
		if err := raw.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatalf("read seed journal_mode: %v", err)
		}
		if mode != "delete" {
			t.Fatalf("seed journal_mode = %q, want delete (test precondition)", mode)
		}
		if _, err := raw.Exec(preGooseSchema[0]); err != nil {
			t.Fatalf("seed ddl: %v", err)
		}
		raw.Close()
		assertWALAfterOpen(t, path)
	})
}

// assertWALAfterOpen opens path with OpenLocalDB, closes it, and checks
// journal_mode from an independent connection. WAL mode is recorded in
// the database file, so a fresh connection (or a second process, such as
// `slagworksd site worker-id`) sees it without any DSN parameters.
func assertWALAfterOpen(t *testing.T, path string) {
	t.Helper()
	db, err := OpenLocalDB(path)
	if err != nil {
		t.Fatalf("OpenLocalDB: %v", err)
	}
	closeLocalDB(t, db)

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw sqlite: %v", err)
	}
	defer raw.Close()
	var mode string
	if err := raw.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q after OpenLocalDB, want wal", mode)
	}
}

func gooseVersion(t *testing.T, path string) int64 {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open for goose version: %v", err)
	}
	defer raw.Close()
	var v int64
	if err := raw.QueryRow("SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1").Scan(&v); err != nil {
		t.Fatalf("query goose version (table missing?): %v", err)
	}
	return v
}

func assertSchemaEqual(t *testing.T, want, got map[string]string) {
	t.Helper()
	names := map[string]struct{}{}
	for n := range want {
		names[n] = struct{}{}
	}
	for n := range got {
		names[n] = struct{}{}
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		if want[n] != got[n] {
			t.Errorf("schema mismatch for %q:\n  want: %q\n  got:  %q", n, want[n], got[n])
		}
	}
}

// closeLocalDB closes the underlying *sql.DB so the file's WAL is
// checkpointed before a raw reopen inspects it.
func closeLocalDB(t *testing.T, db *LocalDB) {
	t.Helper()
	sqlDB, err := db.db.DB()
	if err != nil {
		t.Fatalf("extract sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestLocalDB_WorkerIdentity(t *testing.T) {
	db := openTestDB(t)

	// Empty by default
	id, fqdn := db.GetWorkerIdentity()
	if id != "" || fqdn != "" {
		t.Fatalf("expected empty identity, got id=%q fqdn=%q", id, fqdn)
	}

	// Set and retrieve
	db.SetWorkerIdentity("worker-1", "oob-gw.example.com")
	id, fqdn = db.GetWorkerIdentity()
	if id != "worker-1" || fqdn != "oob-gw.example.com" {
		t.Fatalf("expected worker-1/oob-gw.example.com, got %q/%q", id, fqdn)
	}

	// Overwrite
	db.SetWorkerIdentity("worker-2", "oob-gw-2.example.com")
	id, fqdn = db.GetWorkerIdentity()
	if id != "worker-2" || fqdn != "oob-gw-2.example.com" {
		t.Fatalf("expected worker-2, got %q/%q", id, fqdn)
	}

	// Clear
	db.ClearWorkerIdentity()
	id, fqdn = db.GetWorkerIdentity()
	if id != "" || fqdn != "" {
		t.Fatalf("expected empty after clear, got id=%q fqdn=%q", id, fqdn)
	}
}

func TestLocalDB_SyncCursor(t *testing.T) {
	db := openTestDB(t)

	if got := db.GetSyncCursor(); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}

	db.SetSyncCursor(42)
	if got := db.GetSyncCursor(); got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}

	db.SetSyncCursor(100)
	if got := db.GetSyncCursor(); got != 100 {
		t.Fatalf("expected 100, got %d", got)
	}
}

func TestLocalDB_InstanceID(t *testing.T) {
	db := openTestDB(t)

	if got := db.GetInstanceID(); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}

	db.SetInstanceID("inst-abc")
	if got := db.GetInstanceID(); got != "inst-abc" {
		t.Fatalf("expected inst-abc, got %q", got)
	}
}

func TestLocalDB_ResetSyncState(t *testing.T) {
	db := openTestDB(t)

	db.SetSyncCursor(99)
	db.SetInstanceID("inst-xyz")

	db.ResetSyncState()

	if got := db.GetSyncCursor(); got != 0 {
		t.Fatalf("expected 0 after reset, got %d", got)
	}
	if got := db.GetInstanceID(); got != "" {
		t.Fatalf("expected empty after reset, got %q", got)
	}
}

func TestLocalDB_Outbox_AppendAckRoundtrip(t *testing.T) {
	db := openTestDB(t)

	if got := mustListOutbox(t, db); len(got) != 0 {
		t.Fatalf("expected empty pending outbox, got %d", len(got))
	}

	machineID := uuid.New()
	id1, err := db.AppendOutbox(model.Observation{
		MachineID: machineID,
		Event:     "deploy_complete",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.AppendOutbox(model.Observation{
		MachineID: machineID,
		Event:     "phone_home",
		Timestamp: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("bogus row ids: %d, %d", id1, id2)
	}

	pending := mustListOutbox(t, db)
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(pending))
	}
	if pending[0].Obs.Event != "deploy_complete" || pending[1].Obs.Event != "phone_home" {
		t.Fatalf("unexpected events: %+v", pending)
	}
	if pending[0].RowID != id1 || pending[1].RowID != id2 {
		t.Fatalf("row id mismatch: got %d,%d want %d,%d", pending[0].RowID, pending[1].RowID, id1, id2)
	}

	// List again — still both (no auto-delete).
	if got := mustListOutbox(t, db); len(got) != 2 {
		t.Fatalf("expected list to be non-destructive, got %d", len(got))
	}

	db.AckOutboxRow(id1)
	pending = mustListOutbox(t, db)
	if len(pending) != 1 || pending[0].RowID != id2 {
		t.Fatalf("after ack: %+v", pending)
	}

	db.AckOutboxRow(id2)
	if got := mustListOutbox(t, db); len(got) != 0 {
		t.Fatalf("after acking both: %d remaining", len(got))
	}
}

func TestLocalDB_MachineCache(t *testing.T) {
	db := openTestDB(t)

	// Empty cache
	if got := db.LoadMachineCache(); got != nil {
		t.Fatalf("expected nil for empty cache, got %d entries", len(got))
	}

	// Save and load
	m1 := &model.MachineSnapshot{ID: uuid.New(), State: model.MachineStateNew, BMCIP: "10.0.0.1"}
	m2 := &model.MachineSnapshot{ID: uuid.New(), State: model.MachineStateRunning, BMCIP: "10.0.0.2"}
	machines := map[uuid.UUID]*model.MachineSnapshot{
		m1.ID: m1,
		m2.ID: m2,
	}

	db.SaveMachineCache(machines)

	loaded := db.LoadMachineCache()
	if len(loaded) != 2 {
		t.Fatalf("expected 2 machines, got %d", len(loaded))
	}
	if loaded[m1.ID].State != model.MachineStateNew {
		t.Errorf("m1: expected new, got %s", loaded[m1.ID].State)
	}
	if loaded[m2.ID].State != model.MachineStateRunning {
		t.Errorf("m2: expected deployed, got %s", loaded[m2.ID].State)
	}

	// Save again overwrites (not appends)
	db.SaveMachineCache(map[uuid.UUID]*model.MachineSnapshot{m1.ID: m1})
	loaded = db.LoadMachineCache()
	if len(loaded) != 1 {
		t.Fatalf("expected 1 machine after overwrite, got %d", len(loaded))
	}
}

func TestLocalDB_DHCPLeases(t *testing.T) {
	db := openTestDB(t)

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")
	ip1 := net.ParseIP("10.0.0.50")
	ip2 := net.ParseIP("10.0.0.51")

	// Save leases
	db.SaveDHCPLease(DHCPLease{MAC: mac1, IP: ip1, ExpiresAt: time.Now().Add(time.Hour)})
	db.SaveDHCPLease(DHCPLease{MAC: mac2, IP: ip2, ExpiresAt: time.Now().Add(time.Hour)})

	// Load
	leases := db.LoadDHCPLeases()
	if len(leases) != 2 {
		t.Fatalf("expected 2 leases, got %d", len(leases))
	}

	// Delete one
	db.DeleteDHCPLease(mac1)
	leases = db.LoadDHCPLeases()
	if len(leases) != 1 {
		t.Fatalf("expected 1 lease after delete, got %d", len(leases))
	}
	if leases[0].MAC.String() != mac2.String() {
		t.Fatalf("expected mac2, got %s", leases[0].MAC)
	}

	// Purge all
	db.PurgeDHCPLeases()
	leases = db.LoadDHCPLeases()
	if len(leases) != 0 {
		t.Fatalf("expected 0 after purge, got %d", len(leases))
	}
}

func TestLocalDB_CountActiveDHCPLeases(t *testing.T) {
	db := openTestDB(t)

	if got := db.CountActiveDHCPLeases(); got != 0 {
		t.Fatalf("empty: expected 0, got %d", got)
	}

	mac1, _ := net.ParseMAC("52:54:00:aa:bb:01")
	mac2, _ := net.ParseMAC("52:54:00:aa:bb:02")
	mac3, _ := net.ParseMAC("52:54:00:aa:bb:03")
	db.SaveDHCPLease(DHCPLease{MAC: mac1, IP: net.ParseIP("10.0.0.50"), ExpiresAt: time.Now().Add(time.Hour)})
	db.SaveDHCPLease(DHCPLease{MAC: mac2, IP: net.ParseIP("10.0.0.51"), ExpiresAt: time.Now().Add(time.Hour)})
	// Expired lease — must not be counted.
	db.SaveDHCPLease(DHCPLease{MAC: mac3, IP: net.ParseIP("10.0.0.52"), ExpiresAt: time.Now().Add(-time.Hour)})

	if got := db.CountActiveDHCPLeases(); got != 2 {
		t.Fatalf("expected 2 active leases (expired excluded), got %d", got)
	}
}

func TestLocalDB_DHCPLeases_ExpiredFiltered(t *testing.T) {
	db := openTestDB(t)

	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")
	ip := net.ParseIP("10.0.0.50")

	// Save expired lease
	db.SaveDHCPLease(DHCPLease{MAC: mac, IP: ip, ExpiresAt: time.Now().Add(-time.Hour)})

	// Load should filter expired
	leases := db.LoadDHCPLeases()
	if len(leases) != 0 {
		t.Fatalf("expected 0 (expired filtered), got %d", len(leases))
	}
}

// TestLocalDB_DHCPLeases_ExpiryJudgedByInstant pins that a persisted
// lease's expiry is compared as an instant, not as a wall-clock reading.
// The SQLite driver stores expires_at as text carrying the writer's UTC
// offset, so a row read under a different offset than the one it was
// written with reads up to an hour off: a site-worker restart around a
// DST fall-back (an expiry after the change carries the new offset, "now"
// before it the old one), or a restart after the host's time zone
// changes. Each case pairs the shifted row with a control row of the
// opposite liveness written under the current offset, so the count, the
// load and the load's purge of expired rows each have to tell them apart.
func TestLocalDB_DHCPLeases_ExpiryJudgedByInstant(t *testing.T) {
	_, offset := time.Now().Zone()
	hourBehind := time.FixedZone("hour-behind", offset-3600)
	hourAhead := time.FixedZone("hour-ahead", offset+3600)
	shiftedMAC, _ := net.ParseMAC("52:54:00:aa:bb:01")
	controlMAC, _ := net.ParseMAC("52:54:00:aa:bb:02")

	cases := []struct {
		name        string
		shifted     time.Time
		shiftedLive bool
	}{
		// 20 minutes left, but its wall-clock reading is 40 minutes ago.
		{"live lease written an hour behind", time.Now().Add(20 * time.Minute).In(hourBehind), true},
		// Expired 20 minutes ago, but its wall-clock reading is 40 minutes ahead.
		{"expired lease written an hour ahead", time.Now().Add(-20 * time.Minute).In(hourAhead), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			control, liveMAC := time.Now().Add(-time.Hour), shiftedMAC
			if !tc.shiftedLive {
				control, liveMAC = time.Now().Add(time.Hour), controlMAC
			}
			db.SaveDHCPLease(DHCPLease{MAC: shiftedMAC, IP: net.ParseIP("10.0.0.50"), ExpiresAt: tc.shifted})
			db.SaveDHCPLease(DHCPLease{MAC: controlMAC, IP: net.ParseIP("10.0.0.51"), ExpiresAt: control})

			if n := db.CountActiveDHCPLeases(); n != 1 {
				t.Errorf("CountActiveDHCPLeases = %d, want 1", n)
			}
			if got := db.LoadDHCPLeases(); len(got) != 1 || got[0].MAC.String() != liveMAC.String() {
				t.Errorf("LoadDHCPLeases = %v, want only %s", got, liveMAC)
			}
			// LoadDHCPLeases deletes the rows it judges expired.
			var onDisk []dhcpLeaseRow
			db.db.Find(&onDisk)
			if len(onDisk) != 1 || onDisk[0].MAC != liveMAC.String() {
				t.Errorf("rows left after LoadDHCPLeases = %v, want only %s", onDisk, liveMAC)
			}
		})
	}
}

func TestLocalDB_DHCPLeases_Upsert(t *testing.T) {
	db := openTestDB(t)

	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")
	ip1 := net.ParseIP("10.0.0.50")
	ip2 := net.ParseIP("10.0.0.99")

	db.SaveDHCPLease(DHCPLease{MAC: mac, IP: ip1, ExpiresAt: time.Now().Add(time.Hour)})
	db.SaveDHCPLease(DHCPLease{MAC: mac, IP: ip2, ExpiresAt: time.Now().Add(time.Hour)})

	leases := db.LoadDHCPLeases()
	if len(leases) != 1 {
		t.Fatalf("expected 1 lease (upsert), got %d", len(leases))
	}
	if !leases[0].IP.Equal(ip2) {
		t.Fatalf("expected updated IP %s, got %s", ip2, leases[0].IP)
	}
}

func TestLocalDB_LeaseStore(t *testing.T) {
	db := openTestDB(t)
	store := db.NewLeaseStore()

	mac, _ := net.ParseMAC("52:54:00:aa:bb:01")
	ip := net.ParseIP("10.0.0.50")

	store.SaveLease(mac, ip, time.Now().Add(time.Hour))
	leases := store.LoadLeases()
	if len(leases) != 1 {
		t.Fatalf("expected 1 lease, got %d", len(leases))
	}
	if leases[0].MAC.String() != mac.String() {
		t.Fatalf("expected MAC %s, got %s", mac, leases[0].MAC)
	}

	store.DeleteLease(mac)
	leases = store.LoadLeases()
	if len(leases) != 0 {
		t.Fatalf("expected 0 after delete, got %d", len(leases))
	}
}

// TestCache_ReloadOutboxFromDisk_AfterDrain verifies that after the WS
// drain has removed observations from the in-memory queue but before
// they're acked, a reconnect repopulates them from SQLite for retry.
func TestCache_ReloadOutboxFromDisk_AfterDrain(t *testing.T) {
	db := openTestDB(t)
	c := NewCache(db)
	id := uuid.New()

	// Append two observations.
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateDeploying}},
	})
	c.recordDeployComplete(id, uuid.New(), nil)
	c.RecordPhoneHome(id)

	// Simulate drain (wsconn takes items out of memory to send them).
	obs, _ := c.DrainOutbox()
	if len(obs) != 2 {
		t.Fatalf("drain: got %d, want 2", len(obs))
	}

	// Disconnect + reconnect: reload from SQLite. Rows are still there
	// because nothing acked them.
	c.ReloadOutboxFromDisk()
	obs2, _ := c.DrainOutbox()
	if len(obs2) != 2 {
		t.Fatalf("after reload: got %d, want 2", len(obs2))
	}

	// Ack one; reload should only pull the remaining unacked row.
	c.AckObservation(obs2[0].RowID)
	c.ReloadOutboxFromDisk()
	obs3, _ := c.DrainOutbox()
	if len(obs3) != 1 || obs3[0].RowID != obs2[1].RowID {
		t.Fatalf("after ack + reload: got %+v, want only row %d", obs3, obs2[1].RowID)
	}
}

// TestCache_ReloadOutboxFromDisk_DeduplicatesAgainstInMemory verifies
// reload doesn't push duplicates when SQLite rows are already queued
// in memory.
func TestCache_ReloadOutboxFromDisk_DeduplicatesAgainstInMemory(t *testing.T) {
	db := openTestDB(t)
	c := NewCache(db)
	id := uuid.New()
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{{ID: id, State: model.MachineStateDeploying}},
	})
	c.recordDeployComplete(id, uuid.New(), nil)

	// Reload when the row is already in memory: should be a no-op.
	c.ReloadOutboxFromDisk()
	obs, _ := c.DrainOutbox()
	if len(obs) != 1 {
		t.Fatalf("expected single row post-reload, got %d", len(obs))
	}
}

// TestLocalDB_SaveSnapshot_Atomic verifies that SaveSnapshot writes
// machines + meta in a single transaction, so a restart after
// ApplySnapshot sees both halves (or neither). A torn write would
// manifest as a new SeqNum + stale machines, which the production
// code uses to short-circuit a rebuild on next hello → permanent
// desync.
func TestLocalDB_SaveSnapshot_Atomic(t *testing.T) {
	db := openTestDB(t)

	machineID := uuid.New()
	snap := &model.SiteSnapshot{
		SeqNum: 99,
		Site:   model.SiteInfo{Slug: "pop3"},
		DHCP:   model.DHCPConfig{Subnet: "10.252.0.0/16"},
		Machines: []model.MachineSnapshot{
			{ID: machineID, State: model.MachineStateCommissioned, PXEEligible: true},
		},
	}
	db.SaveSnapshot(snap)

	meta := db.LoadSnapshotMeta()
	machines := db.LoadMachineCache()
	if meta == nil || meta.SeqNum != 99 {
		t.Errorf("meta not persisted or wrong SeqNum: %+v", meta)
	}
	if len(machines) != 1 || machines[machineID] == nil {
		t.Errorf("machines not persisted: %+v", machines)
	}
}

// TestLocalDB_SnapshotMeta covers the snapshot-meta roundtrip used
// to warm-start the site worker's DHCP/TFTP without a live WS
// connection.
func TestLocalDB_SnapshotMeta(t *testing.T) {
	db := openTestDB(t)

	// No cached meta → Load returns nil.
	if got := db.LoadSnapshotMeta(); got != nil {
		t.Errorf("empty DB should return nil meta, got %+v", got)
	}

	snap := &model.SiteSnapshot{
		SeqNum: 42,
		Site:   model.SiteInfo{Slug: "pop3", Name: "pop3"},
		DHCP: model.DHCPConfig{
			Subnet:             "10.252.0.0/16",
			Gateway:            "10.252.1.1",
			EphemeralPoolStart: "10.252.1.200",
			EphemeralPoolEnd:   "10.252.1.254",
		},
		Images:  []model.Image{{Name: "noble-pxe", SHA256: "abc"}},
		SSHKeys: []string{"ssh-ed25519 AAAA"},
		// Machines deliberately left out — they're persisted via
		// SaveMachineCache.
	}
	db.SaveSnapshotMeta(snap)

	got := db.LoadSnapshotMeta()
	if got == nil {
		t.Fatal("expected cached meta, got nil")
	}
	if got.SeqNum != 42 {
		t.Errorf("SeqNum = %d, want 42", got.SeqNum)
	}
	if got.DHCP.Subnet != "10.252.0.0/16" {
		t.Errorf("DHCP.Subnet = %q", got.DHCP.Subnet)
	}
	if len(got.Images) != 1 || got.Images[0].Name != "noble-pxe" {
		t.Errorf("Images not roundtripped: %+v", got.Images)
	}
	if len(got.SSHKeys) != 1 {
		t.Errorf("SSHKeys not roundtripped: %v", got.SSHKeys)
	}
}

// TestNewCache_RehydratesSnapshotMeta verifies a cold-started Cache
// surfaces cached DHCP + site config via GetDHCPConfig / LastSeqNum
// without needing a live ApplySnapshot. This is what lets DHCP/TFTP
// serve from last-known-good state while the WS connection to
// global is still connecting.
func TestNewCache_RehydratesSnapshotMeta(t *testing.T) {
	db := openTestDB(t)

	db.SaveSnapshotMeta(&model.SiteSnapshot{
		SeqNum: 7,
		Site:   model.SiteInfo{Slug: "pop3", Name: "pop3"},
		DHCP: model.DHCPConfig{
			Subnet:             "10.252.0.0/16",
			Gateway:            "10.252.1.1",
			EphemeralPoolStart: "10.252.1.200",
			EphemeralPoolEnd:   "10.252.1.254",
		},
	})

	c := NewCache(db)
	if c.LastSeqNum() != 7 {
		t.Errorf("LastSeqNum = %d, want 7 (rehydrated from meta)", c.LastSeqNum())
	}
	dhcp := c.GetDHCPConfig()
	if dhcp.Subnet != "10.252.0.0/16" {
		t.Errorf("DHCP.Subnet = %q, want 10.252.0.0/16", dhcp.Subnet)
	}
	site := c.GetSiteInfo()
	if site.Slug != "pop3" {
		t.Errorf("Site.Slug = %q, want pop3", site.Slug)
	}
}

func TestNewCache_WithLocalDB(t *testing.T) {
	db := openTestDB(t)

	// Pre-populate outbox and machine cache
	machineID := uuid.New()
	db.SaveMachineCache(map[uuid.UUID]*model.MachineSnapshot{
		machineID: {ID: machineID, State: model.MachineStateRunning},
	})
	if _, err := db.AppendOutbox(model.Observation{
		MachineID: machineID,
		Event:     "phone_home",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// NewCache should load persisted state.
	c := NewCache(db)
	if c.MachineCount() != 1 {
		t.Fatalf("expected 1 cached machine, got %d", c.MachineCount())
	}

	pending, _ := c.DrainOutbox()
	if len(pending) != 1 || pending[0].Obs.Event != "phone_home" {
		t.Fatalf("expected persisted outbox entry, got %v", pending)
	}
}
