package storageapply

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// mountTable is a fake Runner that answers umount the way the kernel does:
// unmounting a path that still has a mount beneath it fails "target is
// busy", exactly as umount(8) reports EBUSY. It records every umount argv.
type mountTable struct {
	mounted map[string]bool
	argv    []string
}

func newMountTable(paths ...string) *mountTable {
	m := &mountTable{mounted: map[string]bool{}}
	for _, p := range paths {
		m.mounted[p] = true
	}
	return m
}

func (m *mountTable) Available(string) bool { return true }

func (m *mountTable) Output(name string, args ...string) (string, error) {
	return "", fmt.Errorf("mountTable: unexpected Output %s %v", name, args)
}

func (m *mountTable) Run(name string, args ...string) error {
	if name != "umount" || len(args) != 1 {
		return fmt.Errorf("mountTable: unexpected Run %s %v", name, args)
	}
	m.argv = append(m.argv, args[0])
	// umount resolves "/target/" and "/target" to the same mount point.
	p := strings.TrimSuffix(args[0], "/")
	if p == "" {
		p = "/"
	}
	if !m.mounted[p] {
		return fmt.Errorf("umount: %s: not mounted", args[0])
	}
	for other := range m.mounted {
		if strings.HasPrefix(other, p+"/") {
			return fmt.Errorf("umount: %s: target is busy", args[0])
		}
	}
	delete(m.mounted, p)
	return nil
}

func (m *mountTable) left() []string {
	var left []string
	for p := range m.mounted {
		left = append(left, p)
	}
	sort.Strings(left)
	return left
}

func mountTableCtx(m *mountTable) *agent.Context {
	return &agent.Context{Log: slog.Default(), Exec: m}
}

// TestCleanupMounts_UnmountsDeepestFirstRootLast is the regression for a
// BIOS deploy with mounts /, /boot/efi and /data that rebooted with its root
// still mounted: GRUB does not replay an xfs journal, so the next boot
// stopped at "file /boot/grub/i386-pc/normal.mod not found" in grub rescue.
//
// CleanupMounts sorted by the slash count of "/target"+mountpoint. For the
// root that is "/target/", two slashes, the same as "/target/data", so the
// two tied and their order followed Go's randomized map iteration: about
// half the time the root went first, failed busy, and was only logged.
// Repeating the call makes that coin flip certain to land on the old code;
// the fixed order is the same every time.
func TestCleanupMounts_UnmountsDeepestFirstRootLast(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "mdroot"},
			"/boot/efi": {Device: "nvme0n1p1"},
			"/data":     {Device: "mddata"},
			"/var":      {Device: "mdvar"},
			"/var/log":  {Device: "mdlog"},
		},
	}
	var orders [][]string
	for i := range 100 {
		m := newMountTable("/target", "/target/boot/efi", "/target/data", "/target/var", "/target/var/log")
		err := CleanupMounts(mountTableCtx(m), layout)
		if left := m.left(); len(left) != 0 {
			t.Fatalf("call %d: CleanupMounts left %v mounted after running umount on %v (err %v); "+
				"the agent would reboot with the root filesystem mounted", i, left, m.argv, err)
		}
		if err != nil {
			t.Fatalf("call %d: CleanupMounts: %v", i, err)
		}
		orders = append(orders, m.argv)
	}
	want := []string{"/target/boot/efi", "/target/var/log", "/target/data", "/target/var", "/target"}
	for i, got := range orders {
		if !slices.Equal(got, want) {
			t.Fatalf("call %d: umount order %v, want %v", i, got, want)
		}
	}
}

// TestCleanupMounts_FailsWhenRootStaysMounted: a root that will not unmount
// must fail the deploy rather than be logged and rebooted over. Here a
// helper's chroot bind mount (/target/sys) was left behind, so umount of
// /target fails busy, as it did on every UEFI deploy with an md array.
func TestCleanupMounts_FailsWhenRootStaysMounted(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "md0"},
			"/boot/efi": {Device: "vda2"},
		},
	}
	m := newMountTable("/target", "/target/boot/efi", "/target/sys")
	err := CleanupMounts(mountTableCtx(m), layout)
	if err == nil {
		t.Fatalf("CleanupMounts returned nil with %v still mounted", m.left())
	}
	for _, want := range []string{"umount /target:", "target is busy", "redeploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}
