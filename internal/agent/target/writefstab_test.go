package target

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// blkidRunner is a fake agent.Runner that answers `blkid -s UUID`/`blkid -s
// TYPE` from a per-device table. Every other command errors (WriteFstab only
// execs blkid). The device is the LAST arg of the blkid invocation.
type blkidRunner struct {
	uuid map[string]string // dev -> UUID ("" -> blkid returns empty)
	typ  map[string]string // dev -> TYPE
	err  map[string]bool   // dev -> blkid errors
}

func (r *blkidRunner) Available(string) bool { return true }

func (r *blkidRunner) Run(name string, args ...string) error {
	return fmt.Errorf("blkidRunner: unexpected Run %s %v", name, args)
}

func (r *blkidRunner) Output(name string, args ...string) (string, error) {
	if name != "blkid" || len(args) < 4 {
		return "", fmt.Errorf("blkidRunner: unexpected Output %s %v", name, args)
	}
	dev := args[len(args)-1]
	if r.err[dev] {
		return "", fmt.Errorf("blkid: exit 2 (no signature) on %s", dev)
	}
	switch args[1] {
	case "UUID":
		return r.uuid[dev], nil
	case "TYPE":
		return r.typ[dev], nil
	}
	return "", fmt.Errorf("blkidRunner: unexpected blkid field %q", args[1])
}

func newBlkidCtx(r *blkidRunner) *agent.Context {
	return &agent.Context{Log: slog.Default(), Exec: r}
}

// TestPreserveMountpoints pins the extract exclusion set: only preserve-type
// mounts are excluded from the OS rsync (created mounts and disk partitions
// are not). This is the data-safety guard -- a preserved array mounted at
// /target/data must not be written into by the root extract.
func TestPreserveMountpoints(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{
			"nvme0n1": {Type: "disk", Partitions: []storage.Partition{
				{Name: "nvme0n1p1", FS: "vfat"}, {Name: "nvme0n1p2", FS: "ext4"},
			}},
			"mkdata": {Type: "raid", Level: 0, Members: []string{"nvme3n1", "nvme4n1"}, FS: "xfs"},
			"mddata": {Type: "preserve", Members: []string{"nvme1n1", "nvme2n1"},
				FS: "xfs", ArrayUUID: "e8c9aa81-1111-2222-3333-444455556666"},
		},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "nvme0n1p2"},
			"/boot/efi": {Device: "nvme0n1p1"},
			"/scratch":  {Device: "mkdata"}, // created raid -> NOT excluded
			"/data":     {Device: "mddata"}, // preserve -> excluded
		},
	}
	got := preserveMountpoints(layout)
	if len(got) != 1 || got[0] != "/data" {
		t.Fatalf("preserveMountpoints = %v, want [/data] (only the preserve mount)", got)
	}
}

// TestWriteFstab_DeclaredFSDisagreement is the wrong-array guard: a preserve
// (or any) device whose layout-declared FS disagrees with blkid's detected
// TYPE FATALs before writing fstab, because a disagreement means the layout
// describes a device other than the one actually on disk.
func TestWriteFstab_DeclaredFSDisagreement(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{
			"nvme0n1": {Type: "disk", Partitions: []storage.Partition{
				{Name: "nvme0n1p1", FS: "vfat", Bootable: true},
				{Name: "nvme0n1p2", FS: "ext4"},
			}},
			"mddata": {Type: "preserve", Members: []string{"nvme1n1", "nvme2n1"},
				FS: "xfs", ArrayUUID: "e8c9aa81-1111-2222-3333-444455556666"},
		},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "nvme0n1p2"},
			"/boot/efi": {Device: "nvme0n1p1"},
			"/data":     {Device: "mddata"},
		},
	}
	mountDevs := storage.ResolveMountDevices(layout)

	// blkid reports ext4 on the assembled /data array, but the layout
	// declares xfs -- disagreement must FATAL.
	r := &blkidRunner{
		uuid: map[string]string{
			mountDevs["/"]:         "root-uuid",
			mountDevs["/boot/efi"]: "esp-uuid",
			mountDevs["/data"]:     "data-uuid",
		},
		typ: map[string]string{
			mountDevs["/"]:         "ext4",
			mountDevs["/boot/efi"]: "vfat",
			mountDevs["/data"]:     "ext4", // != declared xfs
		},
	}
	err := WriteFstab(newBlkidCtx(r), layout, mountDevs)
	if err == nil {
		t.Fatal("expected FATAL on declared-fs/blkid disagreement, got nil")
	}
	if !strings.Contains(err.Error(), "mddata") || !strings.Contains(err.Error(), "xfs") {
		t.Errorf("error should name the device and the declared fs, got: %v", err)
	}
}

// TestWriteFstab_BlkidEmptyUUID fails loudly rather than writing an
// UUID=<empty> entry that would strand the mount at boot.
func TestWriteFstab_BlkidEmptyUUID(t *testing.T) {
	layout := storage.Layout{
		Devices: map[string]storage.Device{
			"nvme0n1": {Type: "disk", Partitions: []storage.Partition{
				{Name: "nvme0n1p2", FS: "ext4"},
			}},
		},
		Mounts: map[string]storage.Mount{"/": {Device: "nvme0n1p2"}},
	}
	mountDevs := storage.ResolveMountDevices(layout)
	r := &blkidRunner{
		uuid: map[string]string{mountDevs["/"]: ""}, // empty UUID
		typ:  map[string]string{mountDevs["/"]: "ext4"},
	}
	err := WriteFstab(newBlkidCtx(r), layout, mountDevs)
	if err == nil || !strings.Contains(err.Error(), "no UUID") {
		t.Fatalf("expected empty-UUID error, got: %v", err)
	}
}
