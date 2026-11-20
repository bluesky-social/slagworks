package target

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// mdadmRunner is a fake agent.Runner that answers `mdadm --detail --export
// <dev>` from a per-device table and `mdadm --detail --scan` with every array
// the deploy environment has active, the layout's and any other. Any other
// command errors.
type mdadmRunner struct {
	export map[string]string // dev -> --detail --export output (absent -> error)
	scan   string
	calls  []string
}

func (r *mdadmRunner) Available(string) bool { return true }

func (r *mdadmRunner) Run(name string, args ...string) error {
	return fmt.Errorf("mdadmRunner: unexpected Run %s %v", name, args)
}

func (r *mdadmRunner) Output(name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name != "mdadm" {
		return "", fmt.Errorf("mdadmRunner: unexpected Output %s %v", name, args)
	}
	switch {
	case len(args) == 2 && args[0] == "--detail" && args[1] == "--scan":
		return r.scan, nil
	case len(args) == 3 && args[0] == "--detail" && args[1] == "--export":
		out, ok := r.export[args[2]]
		if !ok {
			return "", fmt.Errorf("mdadm: cannot open %s: No such file or directory", args[2])
		}
		return out, nil
	}
	return "", fmt.Errorf("mdadmRunner: unexpected mdadm %v", args)
}

func exportFor(uuid string) string {
	return "MD_LEVEL=raid1\nMD_DEVICES=2\nMD_METADATA=1.2\nMD_UUID=" + uuid + "\nMD_NAME=deploy:0\n"
}

// rootAndPreserveLayout has a created root mirror and a preserved data
// array: two arrays the installed system must assemble.
func rootAndPreserveLayout() storage.Layout {
	return storage.Layout{
		Devices: map[string]storage.Device{
			"vda": {Type: "disk", PTable: "gpt", Boot: true, Partitions: []storage.Partition{
				{Name: "vda1", Size: "512M", FS: "vfat", Bootable: true}, {Name: "vda2", Size: "rest"},
			}},
			"vdb": {Type: "disk", PTable: "gpt", Partitions: []storage.Partition{
				{Name: "vdb1", Size: "512M"}, {Name: "vdb2", Size: "rest"},
			}},
			"mdroot": {Type: "raid", Level: 1, Members: []string{"vda2", "vdb2"}, FS: "ext4"},
			"mddata": {Type: "preserve", Level: 0, Members: []string{"vdc", "vdd"}, FS: "xfs",
				ArrayUUID: "e8c9aa81-1111-2222-3333-444455556666"},
		},
		Mounts: map[string]storage.Mount{
			"/": {Device: "mdroot"}, "/boot/efi": {Device: "vda1"}, "/data": {Device: "mddata"},
		},
	}
}

// TestGenerateMDADMConf pins the policy: auto-assembly off, the monitor's
// mail address, then one ARRAY line per array identified by UUID alone, so
// the installed system assembles those arrays and no others.
func TestGenerateMDADMConf(t *testing.T) {
	conf := GenerateMDADMConf([]mdArray{
		{Dev: "/dev/md0", UUID: "e8c9aa81:11112222:33334444:55556666"},
		{Dev: "/dev/md1", UUID: "0a1b2c3d:4e5f6a7b:8c9d0e1f:2a3b4c5d"},
	})

	var directives []string
	for line := range strings.SplitSeq(conf, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		directives = append(directives, line)
	}
	want := []string{
		"AUTO -all",
		"MAILADDR root",
		"ARRAY /dev/md0 UUID=e8c9aa81:11112222:33334444:55556666",
		"ARRAY /dev/md1 UUID=0a1b2c3d:4e5f6a7b:8c9d0e1f:2a3b4c5d",
	}
	if strings.Join(directives, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mdadm.conf directives:\n%s\nwant:\n%s\nfull file:\n%s",
			strings.Join(directives, "\n"), strings.Join(want, "\n"), conf)
	}
	if !strings.HasSuffix(conf, "\n") {
		t.Fatal("mdadm.conf must end with a newline")
	}
}

// TestGenerateMDADMConf_LetsTheArrayMonitorStart: the installed system's
// mdmonitor.service runs `mdadm --monitor --scan`, which exits 1 with "No
// mail address or alert command - not monitoring." unless mdadm.conf names
// where events go (MAILADDR or PROGRAM). udev starts that unit for every
// RAID1 or higher array, and the image's own mdadm.conf, which this file
// replaces, carries MAILADDR root. Without it every host deployed onto a
// mirror boots with a failed unit and nothing watching the array.
func TestGenerateMDADMConf_LetsTheArrayMonitorStart(t *testing.T) {
	conf := GenerateMDADMConf([]mdArray{{Dev: "/dev/md0", UUID: "e8c9aa81:11112222:33334444:55556666"}})

	for line := range strings.SplitSeq(conf, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// mdadm matches config keywords without regard to case.
		switch strings.ToUpper(fields[0]) {
		case "MAILADDR", "PROGRAM":
			return
		}
	}
	t.Fatalf("mdadm.conf has neither MAILADDR nor PROGRAM, so `mdadm --monitor --scan` "+
		"(mdmonitor.service) refuses to start on the installed system:\n%s", conf)
}

// TestLayoutMDADMConf_NamesOnlyTheLayoutsArrays is the regression for an
// array on a disk the deploy leaves alone taking the root array's name at
// boot. The deploy environment had that array active (udev assembled it),
// so a conf built from `mdadm --detail --scan` listed it, and without an
// AUTO policy mdadm assembled it even when unlisted. The conf must name the
// layout's arrays, both created and preserved, and turn auto-assembly off.
func TestLayoutMDADMConf_NamesOnlyTheLayoutsArrays(t *testing.T) {
	layout := rootAndPreserveLayout()
	md := storage.MDDevMap(layout)
	r := &mdadmRunner{
		export: map[string]string{
			md["mdroot"]: exportFor("11111111:22222222:33333333:44444444"),
			md["mddata"]: exportFor("e8c9aa81:11112222:33334444:55556666"),
			"/dev/md127": exportFor("99999999:88888888:77777777:66666666"),
		},
		scan: "ARRAY /dev/md0 metadata=1.2 name=deploy:0 UUID=11111111:22222222:33333333:44444444\n" +
			"ARRAY /dev/md1 metadata=1.2 name=deploy:1 UUID=e8c9aa81:11112222:33334444:55556666\n" +
			"ARRAY /dev/md/deploy:0 metadata=1.2 name=deploy:0 UUID=99999999:88888888:77777777:66666666",
	}
	ctx := &agent.Context{Log: slog.Default(), Exec: r}

	conf, ok, err := layoutMDADMConf(ctx, layout)
	if err != nil || !ok {
		t.Fatalf("layoutMDADMConf: ok=%v err=%v", ok, err)
	}
	want := GenerateMDADMConf([]mdArray{
		{Dev: md["mddata"], UUID: "e8c9aa81:11112222:33334444:55556666"},
		{Dev: md["mdroot"], UUID: "11111111:22222222:33333333:44444444"},
	})
	if conf != want {
		t.Fatalf("mdadm.conf:\n%s\nwant:\n%s", conf, want)
	}
	if strings.Contains(conf, "99999999") {
		t.Fatalf("mdadm.conf names the array from outside the layout:\n%s", conf)
	}
}

// TestLayoutMDADMConf_ArrayNotAssembled: a layout array the agent cannot
// read is a hard error naming it, not a conf that leaves it out.
func TestLayoutMDADMConf_ArrayNotAssembled(t *testing.T) {
	layout := rootAndPreserveLayout()
	md := storage.MDDevMap(layout)
	r := &mdadmRunner{export: map[string]string{
		md["mddata"]: exportFor("e8c9aa81:11112222:33334444:55556666"),
	}}
	_, _, err := layoutMDADMConf(&agent.Context{Log: slog.Default(), Exec: r}, layout)
	if err == nil {
		t.Fatal("layoutMDADMConf succeeded with the root array unreadable")
	}
	if !strings.Contains(err.Error(), `"mdroot"`) || !strings.Contains(err.Error(), md["mdroot"]) {
		t.Fatalf("error should name the layout device and its md node, got: %v", err)
	}

	r.export[md["mdroot"]] = "MD_LEVEL=raid1\nMD_DEVICES=2\n"
	if _, _, err := layoutMDADMConf(&agent.Context{Log: slog.Default(), Exec: r}, layout); err == nil ||
		!strings.Contains(err.Error(), "MD_UUID") {
		t.Fatalf("an export without MD_UUID should fail naming it, got: %v", err)
	}
}

// TestLayoutMDADMConf_NoArrays: a layout without arrays leaves the image's
// mdadm.conf alone and runs no mdadm.
func TestLayoutMDADMConf_NoArrays(t *testing.T) {
	layout := storage.DefaultLayout("vda")
	r := &mdadmRunner{}
	_, ok, err := layoutMDADMConf(&agent.Context{Log: slog.Default(), Exec: r}, layout)
	if err != nil || ok {
		t.Fatalf("layoutMDADMConf on a layout without arrays: ok=%v err=%v", ok, err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("no mdadm call expected, got %v", r.calls)
	}
}
