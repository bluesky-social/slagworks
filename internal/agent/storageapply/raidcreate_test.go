//go:build !integration

package storageapply

import (
	"errors"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// raid5CreateCommand plans a boot disk plus a RAID5 /data array and returns
// the planned `mdadm --create` command, so the tests exercise the exact
// argv the planner emits.
func raid5CreateCommand(t *testing.T) storage.Command {
	t.Helper()
	layout := storage.Layout{
		Devices: map[string]storage.Device{
			"vda": {Type: "disk", Boot: true, Partitions: []storage.Partition{
				{Name: "vda1", FS: "vfat", Size: "512M", Bootable: true},
				{Name: "vda2", FS: "ext4", Size: "rest"},
			}},
			"vdb":    {Type: "disk"},
			"vdc":    {Type: "disk"},
			"vdd":    {Type: "disk"},
			"mddata": {Type: "raid", Level: 5, FS: "xfs", Members: []string{"vdb", "vdc", "vdd"}},
		},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "vda2"},
			"/boot/efi": {Device: "vda1"},
			"/data":     {Device: "mddata"},
		},
	}
	cmds, err := storage.Plan(layout, []storage.BlockDevice{{Name: "vda", LogicalSectorBytes: 512}, {Name: "vdb", LogicalSectorBytes: 512}, {Name: "vdc", LogicalSectorBytes: 512}, {Name: "vdd", LogicalSectorBytes: 512}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, c := range cmds {
		if c.Name == "mdadm" && len(c.Args) > 0 && c.Args[0] == "--create" {
			return c
		}
	}
	t.Fatalf("Plan emitted no mdadm --create command: %v", cmds)
	return storage.Command{}
}

// TestRunCommand_RAIDCreateWithoutPersonality is what an operator sees when
// the boot image's deploy initrd lacks the level's md personality module:
// mdadm prints only "RUN_ARRAY failed: Invalid argument" and exits 1, after
// the member disks were repartitioned, and the kernel logs that it has no
// personality for the level. The FATAL must name the level, the module, and
// the way out, instead of a bare "exit status 1".
func TestRunCommand_RAIDCreateWithoutPersonality(t *testing.T) {
	cmd := raid5CreateCommand(t)
	exitErr := errors.New("exit status 1")
	r := newScriptRunner()
	r.runErr[cmd.String()] = exitErr
	r.output["dmesg"] = "[   41.512345] md/raid1:md1: active with 2 out of 2 mirrors\n" +
		"[   42.103412] md: personality for level 5 is not loaded!\n" +
		"[   42.103420] md: md0 stopped.\n"

	err := RunCommand(scriptCtx(r), cmd)
	if err == nil {
		t.Fatal("RunCommand returned nil for a failed mdadm --create")
	}
	if !errors.Is(err, exitErr) {
		t.Errorf("error does not wrap the mdadm exit error: %v", err)
	}
	for _, want := range []string{
		"RAID 5",
		"/dev/md0",
		"md: personality for level 5 is not loaded",
		"module raid456",
		"assets/initramfs/hooks/slagworks",
		"--destructive",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// TestRunCommand_RAIDCreateOtherFailure: a create that fails for any other
// reason (a busy member, say, before mdadm ever reaches RUN_ARRAY, so no
// personality was requested) must not send the operator off to rebuild the
// image, and must say where the real cause is.
func TestRunCommand_RAIDCreateOtherFailure(t *testing.T) {
	cmd := raid5CreateCommand(t)
	exitErr := errors.New("exit status 1")
	r := newScriptRunner()
	r.runErr[cmd.String()] = exitErr
	// A personality for ANOTHER level missing must not count either.
	r.output["dmesg"] = "[   42.103412] md: personality for level 10 is not loaded!\n"

	err := RunCommand(scriptCtx(r), cmd)
	if !errors.Is(err, exitErr) {
		t.Fatalf("error does not wrap the mdadm exit error: %v", err)
	}
	if strings.Contains(err.Error(), "raid456") || strings.Contains(err.Error(), "hooks/slagworks") {
		t.Errorf("error blames the initrd although the kernel logged no missing raid5 personality:\n%v", err)
	}
	for _, want := range []string{"RAID 5", "/dev/md0", "stderr", "--destructive"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// TestRunCommand_RAIDCreateKernelLogUnreadable: without the kernel log the
// cause is unknown, so the error points at mdadm's stderr and says which
// module could not be ruled out, without claiming it is missing.
func TestRunCommand_RAIDCreateKernelLogUnreadable(t *testing.T) {
	cmd := raid5CreateCommand(t)
	exitErr := errors.New("exit status 1")
	r := newScriptRunner()
	r.runErr[cmd.String()] = exitErr
	r.outputErr["dmesg"] = errors.New("dmesg: read kernel buffer failed: Operation not permitted")

	err := RunCommand(scriptCtx(r), cmd)
	if !errors.Is(err, exitErr) {
		t.Fatalf("error does not wrap the mdadm exit error: %v", err)
	}
	if strings.Contains(err.Error(), "could not load") || strings.Contains(err.Error(), "Rebuild") {
		t.Errorf("error claims a missing module it could not establish:\n%v", err)
	}
	for _, want := range []string{"RAID 5", "/dev/md0", "stderr", "--destructive", "raid456", "Operation not permitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// TestRunCommand_RAIDCreateSucceeds: a successful create runs exactly once
// and never reads the kernel log.
func TestRunCommand_RAIDCreateSucceeds(t *testing.T) {
	cmd := raid5CreateCommand(t)
	r := newScriptRunner()
	if err := RunCommand(scriptCtx(r), cmd); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != cmd.String() {
		t.Errorf("calls = %q, want exactly [%q]", r.calls, cmd.String())
	}
}
