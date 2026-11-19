//go:build proptest_findings && !integration

package storageapply

// Regression tests for the boot-disk safety check. They fail on the code as
// it stands, hence the proptest_findings tag:
//
//	go test -tags proptest_findings ./internal/agent/storageapply/ -run TestCheckDisksClean
//
// Found by the QEMU storage property suite (internal/qemutest,
// TestStoragePropFindings/safety-ambiv): a disk carrying an ext4 filesystem
// AND ZFS labels was partitioned and installed over without --destructive.

import (
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// probeRunner answers the two probes a safety check can make with what the
// real tools print for a given disk, keyed on the command name alone so the
// test does not pin the exact flags.
type probeRunner struct {
	blkidOut  string
	blkidErr  error
	wipefsOut string
	wipefsErr error
	calls     []string
}

func (r *probeRunner) Available(string) bool { return true }

func (r *probeRunner) Run(name string, args ...string) error {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil
}

func (r *probeRunner) Output(name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	switch name {
	case "blkid":
		return r.blkidOut, r.blkidErr
	case "wipefs":
		return r.wipefsOut, r.wipefsErr
	}
	return "", errors.New("probeRunner: unexpected command " + name)
}

func scriptCtxFor(r agent.Runner) *agent.Context {
	return &agent.Context{Log: slog.Default(), Exec: r}
}

// exitStatus returns a real *exec.ExitError carrying code, so a fix that
// tells blkid's "nothing found" (2) from other failures sees what it would
// see in production.
func exitStatus(t *testing.T, code string) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+code).Run()
	if err == nil {
		t.Fatalf("sh -c 'exit %s' succeeded", code)
	}
	return err
}

func singleDiskLayout() storage.Layout {
	return storage.Layout{
		Devices: map[string]storage.Device{
			"vda": {Type: "disk", Boot: true, Partitions: []storage.Partition{
				{Name: "vda1", FS: "vfat", Size: "512M", Bootable: true},
				{Name: "vda2", FS: "ext4", Size: "rest"},
			}},
		},
		Mounts: map[string]storage.Mount{"/": {Device: "vda2"}, "/boot/efi": {Device: "vda1"}},
	}
}

// A disk that holds two filesystem signatures (here ext4, and the ZFS labels
// of a pool later created on it) makes `blkid -p` report NOTHING: it prints
// no output and exits 2, exactly as it does for a blank disk, because its
// safe-probe mode refuses to choose between the two. wipefs, which lists
// every signature, shows both. The safety check must refuse such a disk.
//
// The stdout/exit values below are what the deploy image's util-linux
// returned for that disk in the QEMU reproduction.
func TestCheckDisksClean_DiskWithConflictingSignaturesIsNotClean(t *testing.T) {
	r := &probeRunner{
		blkidOut: "",
		blkidErr: exitStatus(t, "2"),
		wipefsOut: "DEVICE OFFSET  TYPE       UUID                                 LABEL\n" +
			"vda    0x438   ext4       b427ce1f-d0e1-41af-a9ef-034e23e4098c \n" +
			"vda    0x23c00 zfs_member                                      \n",
	}
	err := CheckDisksClean(scriptCtxFor(r), singleDiskLayout())
	if err == nil {
		t.Fatalf("CheckDisksClean passed a disk that carries ext4 and zfs_member signatures "+
			"(blkid -p reports nothing for it); a non-destructive deploy would partition it. probes run: %v", r.calls)
	}
}

// A probe that cannot run at all (binary missing, I/O error on the disk) is
// not evidence that the disk is blank.
func TestCheckDisksClean_ProbeFailureIsNotClean(t *testing.T) {
	broken := errors.New("fork/exec /usr/sbin/blkid: input/output error")
	r := &probeRunner{blkidErr: broken, wipefsErr: broken}
	err := CheckDisksClean(scriptCtxFor(r), singleDiskLayout())
	if err == nil {
		t.Fatalf("CheckDisksClean treated a failed probe as a clean disk. probes run: %v", r.calls)
	}
}

// The clean and dirty cases the check already gets right, pinned so a fix
// keeps them.
func TestCheckDisksClean_BlankDiskIsClean(t *testing.T) {
	r := &probeRunner{blkidErr: exitStatus(t, "2")}
	if err := CheckDisksClean(scriptCtxFor(r), singleDiskLayout()); err != nil {
		t.Fatalf("blank disk refused: %v", err)
	}
}

func TestCheckDisksClean_PartitionedDiskIsNotClean(t *testing.T) {
	r := &probeRunner{
		blkidOut:  `/dev/vda: PTUUID="93680f81-d24c-4751-9019-86595a378c03" PTTYPE="gpt"`,
		wipefsOut: "vda 0x200 gpt\nvda 0x1fffffe00 gpt\nvda 0x1fe PMBR\n",
	}
	if err := CheckDisksClean(scriptCtxFor(r), singleDiskLayout()); err == nil {
		t.Fatal("partitioned disk accepted")
	}
}
