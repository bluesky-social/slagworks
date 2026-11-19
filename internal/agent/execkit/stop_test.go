//go:build !integration

package execkit

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// waitForFile polls until path exists.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared: the process did not start", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// forkingScript is a shell script whose real work is in a child: the child
// would write survived after two seconds, the parent writes started and
// waits. It is the shape of rsync and of chroot, which fork the workers that
// do the writing.
func forkingScript(started, survived string) string {
	return `(sleep 2; touch "` + survived + `") & touch "` + started + `"; wait`
}

// assertChildDied outlives the forked child's two seconds and fails if it
// wrote its file.
func assertChildDied(t *testing.T, survived string) {
	t.Helper()
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(survived); err == nil {
		t.Error("the process's child kept running after Stop and wrote its file")
	}
}

// TestExecStop_KillsRunningCommandAndItsChildren pins what the agent relies
// on when its operation is aborted mid-install: Stop ends the command in
// flight together with the processes it spawned, and nothing can be started
// afterwards. Before Stop existed the agent could only exit, which left a
// long rsync writing to the disks as an orphan.
func TestExecStop_KillsRunningCommandAndItsChildren(t *testing.T) {
	e := &Exec{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Tee: io.Discard}
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	survived := filepath.Join(dir, "child-survived")

	done := make(chan error, 1)
	go func() { done <- e.Run("sh", "-c", forkingScript(started, survived)) }()
	waitForFile(t, started)

	e.Stop()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil for a command Stop killed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop: the command in flight was not killed")
	}

	if err := e.Run("touch", filepath.Join(dir, "after-stop")); !errors.Is(err, ErrStopped) {
		t.Errorf("Run after Stop = %v, want ErrStopped", err)
	}
	if _, err := e.Output("echo", "after-stop"); !errors.Is(err, ErrStopped) {
		t.Errorf("Output after Stop = %v, want ErrStopped", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "after-stop")); err == nil {
		t.Error("a command ran after Stop")
	}
	assertChildDied(t, survived)
}

// TestGroupStop_KillsEveryKindOfProcessTheAgentStarts covers the processes
// that do not go through Exec.Run: a config script and a daemon the agent
// starts and waits on itself (tailscaled during enrollment). They share the
// agent's one Group with its Exec, so the Stop of a halting agent ends them
// too. An agent that exits around them leaves them running: a storage or
// network script keeps executing, and a tailscale daemon stays up in the
// recovery environment.
func TestGroupStop_KillsEveryKindOfProcessTheAgentStarts(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	procs := &Group{}
	e := &Exec{Log: log, Tee: io.Discard, Procs: procs}
	dir := t.TempDir()
	file := func(name string) string { return filepath.Join(dir, name) }

	// A config script, as storageapply and netapply run them.
	scriptDone := make(chan error, 1)
	go func() {
		script := "#!/bin/sh\n" + forkingScript(file("script-started"), file("script-child-survived")) + "\n"
		_, err := RunConfigScript(procs, log, "storage", []byte(script), "SLAGWORKS_TESTDATA", []byte("{}"), nil)
		scriptDone <- err
	}()
	// A daemon started directly, as the tailscale enrollment starts tailscaled.
	daemon := exec.Command("sh", "-c", forkingScript(file("daemon-started"), file("daemon-child-survived")))
	if err := procs.Start(daemon); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	daemonDone := make(chan error, 1)
	go func() { daemonDone <- procs.Wait(daemon) }()
	// And a command through the Exec that shares the group.
	execDone := make(chan error, 1)
	go func() {
		execDone <- e.Run("sh", "-c", forkingScript(file("exec-started"), file("exec-child-survived")))
	}()
	for _, name := range []string{"script-started", "daemon-started", "exec-started"} {
		waitForFile(t, file(name))
	}

	procs.Stop()

	for name, done := range map[string]chan error{"config script": scriptDone, "daemon": daemonDone, "exec command": execDone} {
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: finished cleanly although Stop killed it", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: still running after Stop", name)
		}
	}

	if _, err := RunConfigScript(procs, log, "network", []byte("#!/bin/sh\necho {}\n"), "SLAGWORKS_TESTDATA", []byte("{}"), nil); !errors.Is(err, ErrStopped) {
		t.Errorf("RunConfigScript after Stop = %v, want ErrStopped", err)
	}
	if err := procs.Start(exec.Command("true")); !errors.Is(err, ErrStopped) {
		t.Errorf("Group.Start after Stop = %v, want ErrStopped", err)
	}

	time.Sleep(2500 * time.Millisecond)
	for _, name := range []string{"script-child-survived", "daemon-child-survived", "exec-child-survived"} {
		if _, err := os.Stat(file(name)); err == nil {
			t.Errorf("%s exists: a child outlived Stop", name)
		}
	}
}

// TestGroup_NilTracksNothing pins the nil Group: commands run as plain
// processes and Stop is a no-op, which is what a Context built by hand gets.
func TestGroup_NilTracksNothing(t *testing.T) {
	var procs *Group
	if err := procs.Run(exec.Command("true")); err != nil {
		t.Fatalf("nil Group Run: %v", err)
	}
	procs.Stop()
	if err := procs.Run(exec.Command("true")); err != nil {
		t.Fatalf("nil Group Run after Stop: %v", err)
	}
}

// TestExecOutput_FailureCarriesStderr pins that Output still hands a failed
// command's stderr to callers through the ExitError, as exec.Cmd.Output
// does; storageapply builds its probe errors from it.
func TestExecOutput_FailureCarriesStderr(t *testing.T) {
	e := &Exec{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	out, err := e.Output("sh", "-c", "echo partial; echo 'device is busy' >&2; exit 4")
	if out != "partial" {
		t.Errorf("stdout = %q, want %q", out, "partial")
	}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exitErr.ExitCode() != 4 {
		t.Fatalf("err = %v, want exit status 4", err)
	}
	if got := string(exitErr.Stderr); got != "device is busy\n" {
		t.Errorf("ExitError stderr = %q, want the command's stderr", got)
	}
}
