package store

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pressly/goose/v3"

	"github.com/bluesky-social/slagworks/internal/store/migrations"
)

// TestMigrate_ScrubsBMCPasswordsFromStoredOperationResults: releases before
// the ipmitool command line was redacted stored a failed BMC reconcile's
// error verbatim, generated monitor password included, in
// operations.result. The API redacts an operation's params and returns its
// result as stored, so every operator who can list operations reads those
// passwords, and ScrubTerminalOperationCredentials only rewrites params.
// New rows no longer carry them; the rows already in a database must be
// cleaned by the upgrade that claims operators never see credentials.
func TestMigrate_ScrubsBMCPasswordsFromStoredOperationResults(t *testing.T) {
	db := freshDB(t, "mig_result_secrets")
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	// The schema of v0.6.2, the last release with no scrub.
	if _, err := p.UpTo(context.Background(), 32); err != nil {
		t.Fatalf("migrate to 00032: %v", err)
	}

	machine := uuid.New()
	if err := db.Exec(`INSERT INTO machines (id, netbox_serial, state) VALUES (?, 'SER-RESULT-1', 'failed')`, machine).Error; err != nil {
		t.Fatalf("insert machine: %v", err)
	}
	const monitorPW, adminPW = "Hunter2Hunter", "AdminPass99"
	rows := map[string]string{
		// In-band, as a deploy's BMC reconcile phase reported it.
		"in-band set": `{"error":"BMC reconcile: reconcile: bmcreconcile: set password slot 4: ipmitool -I open user set password 4 ` + monitorPW + `: exit status 1","phase":"BMC reconcile"}`,
		// The 20-byte form, with trailing arguments.
		"in-band set 20": `{"error":"BMC reconcile: reconcile: bmcreconcile: set password slot 4: ipmitool -I open user set password 4 ` + monitorPW + ` 20: exit status 1"}`,
		// Over LAN, as a bmc_reconcile op reported it: the admin password
		// rides -P and the monitor password the user test.
		"lan test": `{"bmc_reconcile":{"monitor_rotated":false},"error":"bmcreconcile: ipmitool -I lanplus -H 192.0.2.9 -U admin -P ` + adminPW + ` user test 4 20 ` + monitorPW + `: exit status 1"}`,
		// No secret: must come through untouched.
		"plain": `{"error":"aborted by operator"}`,
	}
	ids := map[string]uuid.UUID{}
	for name, result := range rows {
		id := uuid.New()
		ids[name] = id
		if err := db.Exec(`INSERT INTO operations (id, machine_id, trace_id, type, status, phase, params, result, completed_at)
			VALUES (?, ?, gen_random_uuid(), 'bmc_reconcile', 'failed', 'bmc_reconcile', '{}'::jsonb, ?::jsonb, now())`,
			id, machine, result).Error; err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	st := New(db)
	for name, id := range ids {
		op, err := st.GetOperation(id)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		got := string(op.Result)
		for _, secret := range []string{monitorPW, adminPW} {
			if strings.Contains(got, secret) {
				t.Errorf("%s: the stored result still carries the password %q after Migrate: %s", name, secret, got)
			}
		}
		if name == "plain" {
			if !strings.Contains(got, "aborted by operator") {
				t.Errorf("plain: result changed: %s", got)
			}
			continue
		}
		// The rest of the message is what tells the operator what failed.
		if !strings.Contains(got, "exit status 1") || !strings.Contains(got, "ipmitool") {
			t.Errorf("%s: the scrub removed more than the password: %s", name, got)
		}
	}
}
