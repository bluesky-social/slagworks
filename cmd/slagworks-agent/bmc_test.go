package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
)

func bufferLog() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// TestInBandReconcileOptions_FromCmdline: the deploy cmdline's encoded
// slagworks.bmc_admin_user value becomes the reconcile's AdminUser, so the
// slot-2 gate guards the account slagworks drives the BMC with.
func TestInBandReconcileOptions_FromCmdline(t *testing.T) {
	log, _ := bufferLog()
	for _, name := range []string{"ADMIN", `ad min"$x`} {
		opt, err := inBandReconcileOptions(log, bmcreconcile.EncodeAdminUserArg(name))
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if opt.AdminUser != name {
			t.Errorf("AdminUser = %q, want %q", opt.AdminUser, name)
		}
		// Provision-once: no password to assert and no rotation, so a
		// working monitor password is never rewritten by a redeploy.
		if opt.IPMIInterface != "open" || len(opt.Desired) != 1 ||
			!reflect.DeepEqual(opt.Desired[0], bmcreconcile.DesiredUser{Name: bmcreconcile.MonitorUser, Privilege: bmcreconcile.PrivUser}) {
			t.Errorf("in-band options: %+v", opt)
		}
	}
}

// TestInBandReconcileOptions_Absent_LogsGateSkipped: a device with no admin
// entry gets no cmdline arg. Reconcile runs as before (no AdminUser), and
// the skipped gate is logged so it's visible in `slagctl machine logs`.
func TestInBandReconcileOptions_Absent_LogsGateSkipped(t *testing.T) {
	log, buf := bufferLog()
	opt, err := inBandReconcileOptions(log, "")
	if err != nil {
		t.Fatal(err)
	}
	if opt.AdminUser != "" {
		t.Errorf("AdminUser = %q, want empty", opt.AdminUser)
	}
	if !strings.Contains(buf.String(), "slot-2 admin gate skipped") {
		t.Errorf("expected a gate-skipped log line, got %q", buf.String())
	}
}

// TestInBandReconcileOptions_Malformed: an undecodable arg fails closed —
// the reconcile doesn't run at all (soft-fail, nothing written) rather
// than run ungated.
func TestInBandReconcileOptions_Malformed(t *testing.T) {
	log, _ := bufferLog()
	if _, err := inBandReconcileOptions(log, "bad%zz"); err == nil {
		t.Fatal("a malformed slagworks.bmc_admin_user must be an error")
	}
}

// TestReconcileInBand_RefusesWhenAdminNotSlot2 closes R-8d4795: the account
// named on the cmdline sits at slot 3, so the in-band reconcile must refuse
// before any write (only the initial `user list` runs), return no creds,
// and log a line naming the account and its slot.
func TestReconcileInBand_RefusesWhenAdminNotSlot2(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls.log")
	fake := filepath.Join(dir, "ipmitool")
	script := `#!/usr/bin/env bash
echo "$*" >> "` + calls + `"
case "$*" in
  *"user list 1"*)
    printf 'ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit\n'
    printf '1                    true    true       true       ADMINISTRATOR\n'
    printf '2   ADMIN            true    true       true       ADMINISTRATOR\n'
    printf '3   root             true    true       true       ADMINISTRATOR\n'
    ;;
  *) echo "unexpected write: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	log, buf := bufferLog()
	opt, err := inBandReconcileOptions(log, bmcreconcile.EncodeAdminUserArg("root"))
	if err != nil {
		t.Fatal(err)
	}
	opt.IPMITool = fake

	creds, err := reconcileInBand(context.Background(), log, opt)
	if err == nil {
		t.Fatal("expected the slot-2 gate to refuse")
	}
	var gate *bmcreconcile.AdminNotSlot2Error
	if !errors.As(err, &gate) {
		t.Fatalf("expected *AdminNotSlot2Error, got %T: %v", err, err)
	}
	if creds != nil {
		t.Errorf("refusal must report no creds, got %+v", creds)
	}
	got, _ := os.ReadFile(calls)
	if lines := strings.Split(strings.TrimSpace(string(got)), "\n"); len(lines) != 1 || !strings.Contains(lines[0], "user list 1") {
		t.Errorf("expected only the initial user list, got %q", got)
	}
	logs := buf.String()
	for _, want := range []string{"admin_user=root", "admin_slot=3", "slot2_user=ADMIN"} {
		if !strings.Contains(logs, want) {
			t.Errorf("refusal log should carry %s, got %q", want, logs)
		}
	}
}
