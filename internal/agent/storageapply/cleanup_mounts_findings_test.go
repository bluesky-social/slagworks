//go:build proptest_findings && !integration

package storageapply

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags proptest_findings ./internal/agent/storageapply/ -run TestCleanupMounts
//
// Found by the QEMU storage property suite (internal/qemutest, generated
// case g0003): a BIOS deploy with mounts /, /boot/efi and /data logged
//
//	umount /target/boot/efi            exit 0
//	umount /target/                    exit 32  "umount: /target/: target is busy."
//	umount failed  path=/target/
//	umount /target/data                exit 0
//	deploy complete
//
// and rebooted with the root filesystem still mounted. GRUB does not replay
// a filesystem journal, so the next boot stopped at
//
//	error: file `/boot/grub/i386-pc/normal.mod' not found.
//	grub rescue>

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// mountTable is a fake Runner that behaves like the kernel's mount table for
// umount: unmounting a path that still has a mount beneath it fails with
// "target is busy", exactly as umount(8) does.
type mountTable struct {
	mounted map[string]bool
	order   []string
	busy    []string
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
	// "/target/" and "/target" are the same mount point.
	path := strings.TrimRight(args[0], "/")
	m.order = append(m.order, path)
	if !m.mounted[path] {
		return fmt.Errorf("umount: %s: not mounted", path)
	}
	for other := range m.mounted {
		if other != path && strings.HasPrefix(other, path+"/") {
			m.busy = append(m.busy, path)
			return fmt.Errorf("umount: %s: target is busy", path)
		}
	}
	delete(m.mounted, path)
	return nil
}

// TestCleanupMounts_LeavesNothingMounted: after CleanupMounts every layout
// mount under /target must be unmounted, the root included. The layout is the
// shape the shipped RS501A script produces: /, /boot/efi and /data.
//
// CleanupMounts sorts by the number of slashes in "/target"+mountpoint. For
// the root that string is "/target/", two slashes, the same as
// "/target/data", so the two tie and their order follows Go's randomized map
// iteration: about half the time the root is unmounted first, fails busy, and
// is left mounted for the reboot. The loop makes the coin flip certain.
func TestCleanupMounts_LeavesNothingMounted(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "mdroot"},
			"/boot/efi": {Device: "nvme0n1p1"},
			"/data":     {Device: "mddata"},
		},
	}
	for i := 0; i < 200; i++ {
		m := newMountTable("/target", "/target/boot/efi", "/target/data")
		CleanupMounts(scriptCtxFor(m), layout)
		if len(m.mounted) != 0 {
			var left []string
			for p := range m.mounted {
				left = append(left, p)
			}
			sort.Strings(left)
			t.Fatalf("iteration %d: CleanupMounts left %v mounted; it ran umount in the order %v and %v failed busy. "+
				"The agent then reports deploy complete and reboots with the root filesystem mounted",
				i, left, m.order, m.busy)
		}
	}
}

// The same for a layout with a nested mount, where depth alone already
// orders correctly today; pinned so a fix keeps it.
func TestCleanupMounts_NestedMounts(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "vda3"},
			"/boot/efi": {Device: "vda2"},
		},
	}
	for i := 0; i < 50; i++ {
		m := newMountTable("/target", "/target/boot/efi")
		CleanupMounts(scriptCtxFor(m), layout)
		if len(m.mounted) != 0 {
			t.Fatalf("iteration %d: left %v mounted (order %v)", i, m.mounted, m.order)
		}
	}
}
