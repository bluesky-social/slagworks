package bootloader

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// grubInstallRunner stands in for the target's grub-install. It treats
// <target>/boot/grub/i386-pc the way the real one does (clean_grub_dir in
// GRUB's util/grub-install-common.c): every module already there is renamed
// to a "~" backup, the modules are copied in again, and the backups are
// deleted. It records, per disk, how many files it found to back up.
type grubInstallRunner struct {
	t        *testing.T
	target   string
	disks    []string
	backedUp map[string]int
}

var grubInstallModules = []string{"boot.img", "core.img", "normal.mod", "xfs.mod", "mdraid1x.mod"}

func (r *grubInstallRunner) Available(string) bool { return true }

func (r *grubInstallRunner) Output(name string, args ...string) (string, error) {
	return "", fmt.Errorf("grubInstallRunner: unexpected Output %s %v", name, args)
}

func (r *grubInstallRunner) Run(name string, args ...string) error {
	want := []string{r.target, "grub-install", "--target=i386-pc", "--no-floppy", "--recheck"}
	if name != "chroot" || len(args) != len(want)+1 || !slices.Equal(args[:len(want)], want) {
		return fmt.Errorf("grubInstallRunner: unexpected Run %s %v", name, args)
	}
	disk := args[len(want)]
	r.disks = append(r.disks, disk)

	dir := filepath.Join(r.target, "boot", "grub", "i386-pc")
	old, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		r.t.Fatalf("read %s: %v", dir, err)
	}
	r.backedUp[disk] = len(old)
	for _, e := range old {
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(dir, e.Name()+"~")); err != nil {
			r.t.Fatalf("back up %s: %v", e.Name(), err)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		r.t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, m := range grubInstallModules {
		if err := os.WriteFile(filepath.Join(dir, m), []byte(m), 0644); err != nil {
			r.t.Fatalf("install %s: %v", m, err)
		}
	}
	for _, e := range old {
		if err := os.Remove(filepath.Join(dir, e.Name()+"~")); err != nil {
			r.t.Fatalf("delete backup of %s: %v", e.Name(), err)
		}
	}
	return nil
}

// TestGrubInstallOnDisks_EveryRunStartsWithoutModules is the regression for a
// BIOS deploy that left the machine at "grub rescue>" after "error: not a
// correct XFS inode.": an xfs root with GRUB on three disks.
//
// grub-install copies the same modules into /boot/grub/i386-pc on every run.
// From the second run on it found the previous run's copy, renamed each file
// to a backup, copied the modules again and deleted the backups. On XFS the
// deletions can empty a directory block in the middle of the directory; XFS
// frees it, and the directory is left with a hole. The GRUB the image
// installs (2.12 as packaged for Ubuntu 24.04) reads a hole as a block of
// entries for inode 0 and fails the whole lookup, so every module listed
// after the hole is out of reach, normal.mod's dependencies among them. With
// this image's module set the hole is there after three runs and after five,
// and gone again after two, four and six.
//
// So no grub-install run may find modules to back up, however many disks
// there are.
func TestGrubInstallOnDisks_EveryRunStartsWithoutModules(t *testing.T) {
	target := t.TempDir()
	disks := []string{"/dev/vda", "/dev/vdb", "/dev/vdc"}
	r := &grubInstallRunner{t: t, target: target, backedUp: map[string]int{}}
	ctx := &agent.Context{Log: slog.Default(), Exec: r}

	if err := grubInstallOnDisks(ctx, target, disks); err != nil {
		t.Fatalf("grubInstallOnDisks: %v", err)
	}

	if !slices.Equal(r.disks, disks) {
		t.Fatalf("grub-install ran on %v, want one run per disk in order: %v", r.disks, disks)
	}
	for _, disk := range disks {
		if n := r.backedUp[disk]; n != 0 {
			t.Errorf("grub-install on %s found %d files from an earlier run in boot/grub/i386-pc; "+
				"it backs them up and deletes the backups, which can leave a hole in an XFS "+
				"directory that GRUB cannot read past", disk, n)
		}
	}

	// The last run's modules are what the machine boots from.
	dir := filepath.Join(target, "boot", "grub", "i386-pc")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s after the installs: %v", dir, err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := slices.Clone(grubInstallModules)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("boot/grub/i386-pc holds %v after the installs, want %v", got, want)
	}
}

// TestGrubInstallOnDisks_FailsWhenModulesCannotBeRemoved pins the other half:
// when the module directory cannot be removed the deploy fails there, naming
// the directory and the disk, instead of letting grub-install run over it.
func TestGrubInstallOnDisks_FailsWhenModulesCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read-only directory; the failure cannot be staged")
	}
	target := t.TempDir()
	dir := filepath.Join(target, "boot", "grub", "i386-pc")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "normal.mod"), []byte("normal.mod"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0755) })

	r := &grubInstallRunner{t: t, target: target, backedUp: map[string]int{}}
	ctx := &agent.Context{Log: slog.Default(), Exec: r}

	err := grubInstallOnDisks(ctx, target, []string{"/dev/vda", "/dev/vdb"})
	if err == nil {
		t.Fatal("grubInstallOnDisks succeeded although boot/grub/i386-pc could not be removed")
	}
	for _, want := range []string{"remove " + dir + " before grub-install on /dev/vda", "redeploy"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if len(r.disks) != 0 {
		t.Errorf("grub-install ran on %v after the module directory could not be removed", r.disks)
	}
}
