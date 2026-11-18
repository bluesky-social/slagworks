package bootloader

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/execkit"
)

// TestEfivarfsMounted_True: a /proc/mounts that contains an
// efivarfs entry returns true. The function reads /proc/mounts
// directly so we redirect it via a temp file + reflection-free
// monkey patch — easier: build a small parser-equivalent helper for
// the test and assert the same logic. Here we shadow os.ReadFile
// behavior by writing a fake /proc/mounts-style file and parsing it
// inline; if efivarfsMounted's behavior changes we update both.
func TestEfivarfsMounted_True(t *testing.T) {
	// Use the same parser logic as efivarfsMounted so the test
	// pins that logic, not the file path. Minor duplication is
	// fine — the alternative (file path injection) costs more
	// complexity than a five-line table parse.
	mounts := `proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
efivarfs /sys/firmware/efi/efivars efivarfs rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
`
	if !mountsContainsEfivarfs(mounts) {
		t.Fatal("expected efivarfsMounted to return true for a /proc/mounts with efivarfs")
	}
}

func TestEfivarfsMounted_False(t *testing.T) {
	mounts := `proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
`
	if mountsContainsEfivarfs(mounts) {
		t.Fatal("expected false when efivarfs not in /proc/mounts")
	}
}

// TestEfivarfsMounted_AlternateMountPoint covers the case where
// somebody mounted efivarfs somewhere other than the canonical
// path (rare but legal). The probe should still detect it as
// mounted, because efibootmgr finds variables via the kernel-side
// efivar interface — the userspace mount point is bookkeeping.
func TestEfivarfsMounted_AlternateMountPoint(t *testing.T) {
	mounts := "efivarfs /mnt/efivars efivarfs rw 0 0\n"
	if !mountsContainsEfivarfs(mounts) {
		t.Fatal("expected true for alternate mount point")
	}
}

// TestEfivarfsMounted_RealProcMounts smoke-tests the real
// efivarfsMounted on the host. Result depends on the runner's
// /proc/mounts content, so we don't assert true/false — we only
// assert the call returns without panicking and produces a stable
// boolean (idempotent across two reads).
func TestEfivarfsMounted_RealProcMounts(t *testing.T) {
	// Skip on macOS / non-Linux hosts where /proc/mounts doesn't
	// exist. The test is a Linux-only sanity check.
	if _, err := os.Stat("/proc/mounts"); err != nil {
		t.Skipf("/proc/mounts not present: %v", err)
	}
	a, errA := efivarfsMounted()
	b, errB := efivarfsMounted()
	if errA != nil || errB != nil {
		t.Fatalf("efivarfsMounted errors: %v / %v", errA, errB)
	}
	if a != b {
		t.Errorf("efivarfsMounted not idempotent: %v then %v", a, b)
	}
}

// TestEnsureEfivarfs_NotUEFI: when /sys/firmware/efi is missing
// (BIOS host), ensureEfivarfs should error out at the first stage
// rather than try to mount. The error must reference the missing
// path so a future operator can grep-trace it.
//
// NOTE: this test depends on /sys/firmware/efi NOT existing — true
// on any non-Linux dev laptop and on Linux machines that booted in
// BIOS mode. We skip when /sys/firmware/efi DOES exist (i.e. the
// runner is UEFI Linux), since asserting "this errors" would itself
// be wrong there.
func TestEnsureEfivarfs_NotUEFI(t *testing.T) {
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		t.Skip("/sys/firmware/efi exists on this runner; skipping non-UEFI assertion")
	}
	ctx := &agent.Context{Log: slog.Default(), Exec: &execkit.Exec{Log: slog.Default()}}
	if err := EnsureEfivarfs(ctx); err == nil {
		t.Fatal("expected EnsureEfivarfs to error when /sys/firmware/efi is missing")
	}
}

// TestEfivarfsPath pins the canonical path. Several other call sites
// assume this exact value (the BOOT hook's mount path, the docs
// in registerEFIBootEntry's error message); a typo here breaks all
// of them in lockstep.
func TestEfivarfsPath(t *testing.T) {
	if efivarfsPath != "/sys/firmware/efi/efivars" {
		t.Errorf("efivarfsPath = %q, want %q (the BOOT hook's mount path)",
			efivarfsPath, "/sys/firmware/efi/efivars")
	}
	if !filepath.IsAbs(efivarfsPath) {
		t.Errorf("efivarfsPath must be absolute")
	}
}
