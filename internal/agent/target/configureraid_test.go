package target

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

// chrootMounts is a fake Runner that keeps a kernel-like mount table for the
// commands ConfigureRAID runs on a UEFI machine with an md array:
//
//   - `mount --bind SRC DST` mounts DST with nothing beneath it, as a
//     non-recursive bind does.
//   - `chroot ROOT update-initramfs -u` mounts efivarfs at
//     ROOT/sys/firmware/efi/efivars when ROOT/sys is mounted, which is what
//     Debian's mdadm initramfs hook does when it finds that directory empty.
//   - `umount PATH` fails "target is busy" while anything is mounted beneath
//     PATH; `umount --recursive PATH` (or -R) unmounts that too.
//
// busy names paths that stay busy whatever umount is asked (an open file).
// mdadm --detail --export answers for /dev/md0, the one array of
// raidRootLayout.
type chrootMounts struct {
	root    string
	mounted map[string]bool
	busy    map[string]bool
	runs    []string
}

// raidRootLayout roots the host on a RAID1 mirror, which the plan assembles
// as /dev/md0.
func raidRootLayout() storage.Layout {
	return storage.Layout{
		Devices: map[string]storage.Device{
			"vda": {Type: "disk", PTable: "gpt", Boot: true, Partitions: []storage.Partition{
				{Name: "vda1", Size: "512M", FS: "vfat", Bootable: true}, {Name: "vda2", Size: "rest"},
			}},
			"vdb": {Type: "disk", PTable: "gpt", Partitions: []storage.Partition{
				{Name: "vdb1", Size: "512M"}, {Name: "vdb2", Size: "rest"},
			}},
			"mdroot": {Type: "raid", Level: 1, Members: []string{"vda2", "vdb2"}, FS: "ext4"},
		},
		Mounts: map[string]storage.Mount{"/": {Device: "mdroot"}, "/boot/efi": {Device: "vda1"}},
	}
}

func newChrootMounts(root string) *chrootMounts {
	return &chrootMounts{root: root, mounted: map[string]bool{}, busy: map[string]bool{}}
}

func (r *chrootMounts) Available(string) bool { return true }

func (r *chrootMounts) Output(name string, args ...string) (string, error) {
	if name == "mdadm" && slices.Equal(args, []string{"--detail", "--export", "/dev/md0"}) {
		return "MD_LEVEL=raid1\nMD_METADATA=1.0\nMD_UUID=3aaa0122:29827cfa:5331ad66:ca767371\n", nil
	}
	return "", fmt.Errorf("chrootMounts: unexpected Output %s %v", name, args)
}

func (r *chrootMounts) Run(name string, args ...string) error {
	r.runs = append(r.runs, name+" "+strings.Join(args, " "))
	switch {
	case name == "mount" && len(args) == 3 && args[0] == "--bind":
		r.mounted[args[2]] = true
		return nil
	case name == "chroot" && slices.Equal(args, []string{r.root, "update-initramfs", "-u"}):
		if r.mounted[r.root+"/sys"] {
			r.mounted[r.root+"/sys/firmware/efi/efivars"] = true
		}
		return nil
	case name == "umount" && len(args) > 0:
		return r.umount(args[len(args)-1], slices.Contains(args, "-R") || slices.Contains(args, "--recursive"))
	}
	return fmt.Errorf("chrootMounts: unexpected Run %s %v", name, args)
}

func (r *chrootMounts) umount(path string, recursive bool) error {
	if !r.mounted[path] {
		return fmt.Errorf("umount: %s: not mounted", path)
	}
	var beneath []string
	for p := range r.mounted {
		if strings.HasPrefix(p, path+"/") {
			beneath = append(beneath, p)
		}
	}
	if r.busy[path] || (len(beneath) > 0 && !recursive) {
		return fmt.Errorf("umount: %s: target is busy", path)
	}
	for _, p := range beneath {
		delete(r.mounted, p)
	}
	delete(r.mounted, path)
	return nil
}

func (r *chrootMounts) left() []string {
	var left []string
	for p := range r.mounted {
		left = append(left, p)
	}
	sort.Strings(left)
	return left
}

// TestConfigureRAID_UnmountsWhatTheChrootMountedBeneathItsBinds is the
// regression for every UEFI deploy with an md array rebooting with its root
// filesystem still mounted. ConfigureRAID bind-mounts /dev, /proc and /sys
// into the target for `update-initramfs`, whose mdadm hook mounts efivarfs
// beneath the /sys bind; ConfigureRAID then ran a plain `umount
// /target/sys`, which failed "target is busy", and discarded the error.
// The bind left behind kept /target from unmounting before reboot. Nothing
// ConfigureRAID mounted, or caused to be mounted, may outlive it.
func TestConfigureRAID_UnmountsWhatTheChrootMountedBeneathItsBinds(t *testing.T) {
	root := t.TempDir()
	r := newChrootMounts(root)
	err := ConfigureRAID(&agent.Context{Log: slog.Default(), Exec: r}, root, raidRootLayout())
	if left := r.left(); len(left) != 0 {
		t.Fatalf("ConfigureRAID left %v mounted (err %v); commands run:\n%s",
			left, err, strings.Join(r.runs, "\n"))
	}
	if err != nil {
		t.Fatalf("ConfigureRAID: %v", err)
	}
}

// TestConfigureRAID_ReturnsBindUnmountFailure: a chroot bind that will not
// unmount is an error, not a discarded exit status, and the other binds are
// still unmounted.
func TestConfigureRAID_ReturnsBindUnmountFailure(t *testing.T) {
	root := t.TempDir()
	r := newChrootMounts(root)
	r.busy[root+"/sys"] = true
	err := ConfigureRAID(&agent.Context{Log: slog.Default(), Exec: r}, root, raidRootLayout())
	if err == nil {
		t.Fatalf("ConfigureRAID returned nil with %v still mounted", r.left())
	}
	for _, want := range []string{root + "/sys", "target is busy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if left, want := r.left(), []string{root + "/sys", root + "/sys/firmware/efi/efivars"}; !slices.Equal(left, want) {
		t.Errorf("left mounted %v, want only the busy bind and its submount %v", left, want)
	}
}
