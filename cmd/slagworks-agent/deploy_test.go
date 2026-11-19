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
		return `{"blockdevices":[{"name":"sda","size":107374182400,"tran":"sata","model":"TestDisk","log-sec":512}]}`, nil
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
