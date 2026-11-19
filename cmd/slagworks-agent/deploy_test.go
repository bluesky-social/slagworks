//go:build !integration

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// deployStop is what the test's fatal/setPhase/Run hooks panic with to halt
// runDeploy at the first terminal event, standing in for the os.Exit the
// production fatal performs.
type deployStop struct {
	kind  string // "fatal", "phase", or "run"
	stage string // fatal stage, phase name, or argv
	err   error  // fatal's error
}

// noDiskRunner is an agent.Runner for the pre-destruction part of a deploy:
// lsblk reports one disk, every other probe reports "not available", and any
// Run (the only way the agent mutates a disk: mdadm, wipefs, sgdisk, mkfs,
// rsync, ...) halts the deploy so the test can name it.
type noDiskRunner struct{ runs [][]string }

func (r *noDiskRunner) Run(name string, args ...string) error {
	argv := append([]string{name}, args...)
	r.runs = append(r.runs, argv)
	panic(deployStop{kind: "run", stage: strings.Join(argv, " ")})
}

func (r *noDiskRunner) Output(name string, _ ...string) (string, error) {
	if name == "lsblk" {
		return `{"blockdevices":[{"name":"sda","size":107374182400,"tran":"sata","model":"TestDisk"}]}`, nil
	}
	return "", errors.New(name + " not available in test")
}

func (r *noDiskRunner) Available(string) bool { return false }

// TestRunDeploy_NetworkScriptFailureStopsBeforeDisks is the regression for a
// real-hardware trial: a --destructive deploy of a host whose NetBox model
// lacked the bond its matched network script requires. The network script is
// a pure function of facts the agent has before touching a disk, and it
// hard-fails by design on an incomplete model, yet the agent ran it in the
// finalizing phase: the disks were wiped and the OS and bootloader installed
// before the deploy failed, leaving a wiped host for a problem that was
// knowable up front. The deploy must stop on the network script's failure
// before the partitioning phase and before any disk-mutating command.
func TestRunDeploy_NetworkScriptFailureStopsBeforeDisks(t *testing.T) {
	const machineID = "6b1f0c3e-9a52-4d55-8f0e-1c2d3e4f5a6b"
	layout, err := json.Marshal(storage.DefaultLayout("sda"))
	if err != nil {
		t.Fatalf("marshal layout: %v", err)
	}
	var mu sync.Mutex
	var unexpected []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/machines/" + machineID + "/storage-script":
			fmt.Fprintf(w, "#!/bin/sh\ncat <<'EOF'\n%s\nEOF\n", layout)
		case "/api/v1/machines/" + machineID + "/network-script":
			fmt.Fprint(w, "#!/bin/sh\n"+
				"echo 'ERROR: host does not have exactly one bond interface. Fix the NetBox model.' >&2\n"+
				"exit 1\n")
		case "/api/v1/machines/" + machineID + "/network-facts":
			fmt.Fprint(w, `[{"name":"eno1","mac":"aa:bb:cc:00:00:01","type":"physical"}]`)
		default:
			mu.Lock()
			unexpected = append(unexpected, r.URL.Path)
			mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	runner := &noDiskRunner{}
	ctx := &agent.Context{
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Exec:   runner,
		Server: strings.TrimPrefix(srv.URL, "http://"),
	}
	var phases []string
	p := deployParams{
		machineID:     machineID,
		operationID:   "0d9e8f7a-6b5c-4d3e-2f1a-0b9c8d7e6f5a",
		expectUUID:    "ignored-by-checkIdentity",
		destructive:   true,
		checkIdentity: func(string) error { return nil },
		setPhase: func(phase string) {
			phases = append(phases, phase)
			if phase == "partitioning" {
				panic(deployStop{kind: "phase", stage: phase})
			}
		},
		fatal: func(stage string, err error) {
			panic(deployStop{kind: "fatal", stage: stage, err: err})
		},
		done: make(chan struct{}),
	}

	stop, stopped := runDeployUntilStop(ctx, p)
	if !stopped {
		t.Fatal("runDeploy returned without failing")
	}

	switch stop.kind {
	case "phase":
		t.Fatalf("deploy entered the %q phase although the network script failed (phases: %v)", stop.stage, phases)
	case "run":
		t.Fatalf("deploy ran %q although the network script failed", stop.stage)
	}
	if stop.stage != "rendering network config" {
		t.Fatalf("deploy failed at stage %q (%v), want %q", stop.stage, stop.err, "rendering network config")
	}
	if !strings.Contains(stop.err.Error(), "network script failed") {
		t.Errorf("fatal error should carry the network script failure, got: %v", stop.err)
	}
	if slices.Contains(phases, "partitioning") || len(runner.runs) != 0 {
		t.Errorf("no disk work may precede the failure: phases=%v runs=%v", phases, runner.runs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(unexpected) != 0 {
		t.Errorf("unexpected site-worker requests: %v", unexpected)
	}
}

// pinnedDataUUID is the array_uuid the stale-member tests' /data RAID1 pins.
const pinnedDataUUID = "5d1f0c1e-7a3b-4c2d-9e8f-0a1b2c3d4e5f"

// examineMember renders `mdadm --examine` output for a 1.2-metadata member of
// the array whose UUID is given in mdadm's colon-grouped form.
func examineMember(dev, arrayUUID string) string {
	return dev + `:
          Magic : a92b4efc
        Version : 1.2
    Feature Map : 0x1
     Array UUID : ` + arrayUUID + `
           Name : ubuntu-server:0
     Raid Level : raid1
   Raid Devices : 2
          State : clean
    Device UUID : 0b3c8f6e:1d2a4b5c:6e7f8091:a2b3c4d5
         Events : 17
   Device Role : Active device 0
   Array State : AA ('A' == active, '.' == missing, 'R' == replacing)`
}

// mdHostRunner is an agent.Runner for a four-disk host: vda carries root,
// vdc and vdd are the /data RAID1 members, and vdb is whatever the test
// makes it. examine maps a device to its `mdadm --examine` output; a device
// absent from it has no md superblock. Run records every disk-mutating
// command and halts the deploy at the plan's first sgdisk, so a test sees
// everything the agent did to the disks before it began partitioning.
type mdHostRunner struct {
	examine map[string]string
	runs    []string
}

func (r *mdHostRunner) Run(name string, args ...string) error {
	argv := strings.Join(append([]string{name}, args...), " ")
	r.runs = append(r.runs, argv)
	if name == "sgdisk" {
		panic(deployStop{kind: "run", stage: argv})
	}
	return nil
}

func (r *mdHostRunner) Output(name string, args ...string) (string, error) {
	argv := strings.Join(append([]string{name}, args...), " ")
	switch {
	case argv == "lsblk -Jb -d -o NAME,SIZE,TRAN,MODEL":
		return `{"blockdevices":[` +
			`{"name":"vda","size":10737418240,"tran":"virtio","model":"QEMU"},` +
			`{"name":"vdb","size":4294967296,"tran":"virtio","model":"QEMU"},` +
			`{"name":"vdc","size":4294967296,"tran":"virtio","model":"QEMU"},` +
			`{"name":"vdd","size":4294967296,"tran":"virtio","model":"QEMU"}]}`, nil
	case argv == "lsblk -dnlo NAME,TYPE":
		return "vda disk\nvdb disk\nvdc disk\nvdd disk", nil
	case strings.HasPrefix(argv, "lsblk -nlo NAME /dev/"):
		return strings.TrimPrefix(argv, "lsblk -nlo NAME /dev/"), nil
	case strings.HasPrefix(argv, "mdadm --examine "):
		if out, ok := r.examine[args[len(args)-1]]; ok {
			return out, nil
		}
		return "", errors.New("exit status 1: mdadm: No md superblock detected")
	case argv == "cat /proc/mdstat":
		return "Personalities : [raid1]\nunused devices: <none>", nil
	case strings.HasPrefix(argv, "blkid -p "):
		return "", errors.New("exit status 2") // no signatures: a clean disk
	}
	return "", errors.New(name + " not available in test")
}

func (r *mdHostRunner) Available(string) bool { return false }

// pinnedDataLayout roots the host on vda and builds /data as a RAID1 over
// vdc and vdd with a pinned array_uuid, the way the shipped RS501A script
// pins mddata. vdbType, when set, also places vdb in the layout.
func pinnedDataLayout(vdbType string) storage.Layout {
	l := storage.Layout{
		Devices: map[string]storage.Device{
			"vda": {Type: "disk", PTable: "gpt", Boot: true, Partitions: []storage.Partition{
				{Name: "vda1", FS: "vfat", Size: "512M", Bootable: true},
				{Name: "vda2", FS: "ext4", Size: "rest"},
			}},
			"vdc":    {Type: "disk", PTable: "gpt"},
			"vdd":    {Type: "disk", PTable: "gpt"},
			"mddata": {Type: "raid", Level: 1, Members: []string{"vdc", "vdd"}, FS: "xfs", ArrayUUID: pinnedDataUUID},
		},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "vda2"},
			"/boot/efi": {Device: "vda1"},
			"/data":     {Device: "mddata"},
		},
	}
	if vdbType != "" {
		l.Devices["vdb"] = storage.Device{Type: vdbType}
	}
	return l
}

// TestRunDeploy_StaleMemberWithPinnedArrayUUID is the regression for a
// redeploy found by the QEMU storage property suite: /data is a RAID1 with a
// pinned array_uuid, built over vdb+vdc; a --destructive redeploy builds it
// over vdc+vdd. vdb is now outside the layout and still carries a member
// superblock with the pinned UUID, and nothing examined it. At boot, when
// udev offered vdb to mdadm first, the stale member claimed the array, the
// real members were refused, data.mount failed and the boot fell into
// emergency mode (5 of 12 boots in the property suite).
//
// The agent must examine every device the deploy leaves unrewritten and
// refuse, before touching any disk, when one carries a pinned UUID it is
// about to create, unless --wipe-all-disks will zero that device anyway.
func TestRunDeploy_StaleMemberWithPinnedArrayUUID(t *testing.T) {
	const pinnedMdadm = "5d1f0c1e:7a3b4c2d:9e8f0a1b:2c3d4e5f"
	staleVDB := map[string]string{"/dev/vdb": examineMember("/dev/vdb", pinnedMdadm)}

	tests := []struct {
		name         string
		layout       storage.Layout
		examine      map[string]string
		destructive  bool
		wipeAllDisks bool
		// wantRefusal lists what the refusal must say; nil means the deploy
		// must get past the check and reach partitioning.
		wantRefusal []string
		// notInRefusal lists remedies that would not work and must not be
		// offered.
		notInRefusal []string
	}{
		{
			name:        "destructive redeploy refuses the stale member on a disk outside the layout",
			layout:      pinnedDataLayout(""),
			examine:     staleVDB,
			destructive: true,
			wantRefusal: []string{"/dev/vdb", `"mddata"`, pinnedDataUUID, "--wipe-all-disks", "mdadm --zero-superblock"},
		},
		{
			name:        "non-destructive deploy refuses it too",
			layout:      pinnedDataLayout(""),
			examine:     staleVDB,
			wantRefusal: []string{"/dev/vdb", `"mddata"`, pinnedDataUUID, "--wipe-all-disks"},
		},
		{
			name:         "a retained disk refuses even with --wipe-all-disks, which never touches it",
			layout:       pinnedDataLayout("retain"),
			examine:      staleVDB,
			destructive:  true,
			wipeAllDisks: true,
			wantRefusal:  []string{"/dev/vdb", `"mddata"`, pinnedDataUUID, "mdadm --zero-superblock"},
			notInRefusal: []string{"rerun the deploy with --wipe-all-disks"},
		},
		{
			name:         "--wipe-all-disks proceeds and zeroes the stale member before partitioning",
			layout:       pinnedDataLayout(""),
			examine:      staleVDB,
			destructive:  true,
			wipeAllDisks: true,
		},
		{
			name:        "a member of a different array outside the layout is not a conflict",
			layout:      pinnedDataLayout(""),
			examine:     map[string]string{"/dev/vdb": examineMember("/dev/vdb", "11111111:22222222:33333333:44444444")},
			destructive: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const machineID = "6b1f0c3e-9a52-4d55-8f0e-1c2d3e4f5a6b"
			layoutJSON, err := json.Marshal(tt.layout)
			if err != nil {
				t.Fatalf("marshal layout: %v", err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/machines/" + machineID + "/storage-script":
					fmt.Fprintf(w, "#!/bin/sh\ncat <<'EOF'\n%s\nEOF\n", layoutJSON)
				case "/api/v1/machines/" + machineID + "/network-script":
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			runner := &mdHostRunner{examine: tt.examine}
			ctx := &agent.Context{
				Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
				Exec:   runner,
				Server: strings.TrimPrefix(srv.URL, "http://"),
			}
			var phases []string
			p := deployParams{
				machineID:     machineID,
				operationID:   "0d9e8f7a-6b5c-4d3e-2f1a-0b9c8d7e6f5a",
				expectUUID:    "ignored-by-checkIdentity",
				destructive:   tt.destructive,
				wipeAllDisks:  tt.wipeAllDisks,
				checkIdentity: func(string) error { return nil },
				setPhase:      func(phase string) { phases = append(phases, phase) },
				fatal: func(stage string, err error) {
					panic(deployStop{kind: "fatal", stage: stage, err: err})
				},
				done: make(chan struct{}),
			}

			stop, stopped := runDeployUntilStop(ctx, p)
			if !stopped {
				t.Fatal("runDeploy returned without reaching the plan's first sgdisk")
			}

			if tt.wantRefusal == nil {
				if stop.kind != "run" || !strings.HasPrefix(stop.stage, "sgdisk ") {
					t.Fatalf("deploy stopped at %s %q (%v), want it to reach partitioning", stop.kind, stop.stage, stop.err)
				}
				if tt.wipeAllDisks && !slices.Contains(runner.runs, "mdadm --zero-superblock --force /dev/vdb") {
					t.Errorf("--wipe-all-disks partitioned without zeroing the stale member on /dev/vdb; runs: %v", runner.runs)
				}
				return
			}

			if stop.kind != "fatal" {
				t.Fatalf("deploy reached %s %q although /dev/vdb carries a member superblock with the array UUID "+
					"mddata pins; that member races the real ones at boot (phases %v, runs %v)",
					stop.kind, stop.stage, phases, runner.runs)
			}
			if slices.Contains(phases, "partitioning") || len(runner.runs) != 0 {
				t.Errorf("the refusal must come before any disk work: phases=%v runs=%v", phases, runner.runs)
			}
			msg := stop.err.Error()
			t.Logf("refusal (stage %q): %s", stop.stage, msg)
			for _, want := range tt.wantRefusal {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal (stage %q) should mention %q, got: %v", stop.stage, want, msg)
				}
			}
			for _, bad := range tt.notInRefusal {
				if strings.Contains(msg, bad) {
					t.Errorf("refusal should not offer %q, got: %v", bad, msg)
				}
			}
		})
	}
}

// runDeployUntilStop runs runDeploy until one of the test hooks halts it with
// a deployStop, and reports that stop. stopped is false if runDeploy
// returned on its own.
func runDeployUntilStop(ctx *agent.Context, p deployParams) (stop deployStop, stopped bool) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		s, ok := r.(deployStop)
		if !ok {
			panic(r)
		}
		stop, stopped = s, true
	}()
	runDeploy(ctx, p)
	return deployStop{}, false
}
