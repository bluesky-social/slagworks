package ci

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// entry mirrors the {pkg, test} objects emitted by
// list-integration-tests.sh.
type entry struct {
	Pkg  string `json:"pkg"`
	Test string `json:"test"`
}

// runScript runs scripts/ci/list-integration-tests.sh with its CWD and git
// root set to dir, returning stdout, stderr, and the exit code.
func runScript(t *testing.T, dir string, envOverrides ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	script, err := filepath.Abs("list-integration-tests.sh")
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	cmd := exec.Command(script)
	cmd.Dir = dir
	cmd.Env = envForDir(dir, envOverrides...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if err == nil {
		return out.String(), errBuf.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), errBuf.String(), exitErr.ExitCode()
	}
	t.Fatalf("run script: %v", err)
	return "", "", -1
}

// envForDir returns the process environment with PWD pointed at dir and
// GOWORK cleared, so the fixture's own go.mod is the main module for the
// script's nested `go list` / `go test` calls.
func envForDir(dir string, overrides ...string) []string {
	overridden := map[string]bool{"PWD": true, "GOWORK": true}
	for _, kv := range overrides {
		if key, _, ok := strings.Cut(kv, "="); ok {
			overridden[key] = true
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && overridden[key] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PWD="+dir, "GOWORK=off")
	return append(env, overrides...)
}

// makeFixture builds a minimal git + Go module at a temp dir. files maps
// repo-relative paths to their contents. The module path matches the repo
// so the script's hard-coded qemutest skip matches the fixture package.
func makeFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	// macOS /var is a symlink to /private/var; t.TempDir() returns the
	// /var form while git rev-parse returns the /private/var realpath.
	// Resolve it so the dir git reports and the dir the script greps and
	// passes to go list share one spelling, or go treats the fixture as
	// "outside main module".
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		git := exec.Command("git", args...)
		git.Dir = dir
		if out, err := git.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("go.mod", "module github.com/bluesky-social/slagworks\n\ngo 1.26.5\n")
	for rel, content := range files {
		write(rel, content)
	}
	return dir
}

func TestListIntegrationTests(t *testing.T) {
	const qemutestFile = `//go:build integration

package qemutest

import "testing"

func TestAlways(t *testing.T) {}
`
	const footestFile = `//go:build integration

package footest

import "testing"

func TestFootest(t *testing.T) {}
`
	const helperFile = `//go:build integration

package helper

func Help() {}
`

	tests := []struct {
		name          string
		files         map[string]string
		wantSuccess   bool
		wantStdout    string // exact match when the stdout can't be JSON-parsed
		wantJSON      []entry
		wantStderrSub string
	}{
		{
			name: "only qemutest is skipped -> nonzero with actionable stderr",
			files: map[string]string{
				"internal/qemutest/qemutest_integration_test.go": qemutestFile,
			},
			wantSuccess:   false,
			wantStderrSub: "refusing to emit an empty matrix",
		},
		{
			name:        "no integration-tagged packages -> success with empty matrix",
			files:       map[string]string{},
			wantSuccess: true,
			wantStdout:  "[]\n",
		},
		{
			name: "qemutest skipped plus a runnable package -> runnable only",
			files: map[string]string{
				"internal/qemutest/qemutest_integration_test.go": qemutestFile,
				"internal/footest/footest_integration_test.go":   footestFile,
			},
			wantSuccess: true,
			wantJSON: []entry{
				{Pkg: "github.com/bluesky-social/slagworks/internal/footest", Test: "TestFootest"},
			},
		},
		{
			name: "qemutest skipped plus a helper-only package -> nonzero",
			files: map[string]string{
				"internal/qemutest/qemutest_integration_test.go": qemutestFile,
				"internal/helper/helper_integration.go":          helperFile,
			},
			wantSuccess:   false,
			wantStderrSub: "no runnable integration tests remain",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := makeFixture(t, tt.files)
			stdout, stderr, exitCode := runScript(t, dir)

			if (exitCode == 0) != tt.wantSuccess {
				t.Fatalf("exit code = %d, want success=%v\nstdout=%s\nstderr=%s", exitCode, tt.wantSuccess, stdout, stderr)
			}
			if tt.wantStdout != "" && stdout != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout, tt.wantStdout)
			}
			if tt.wantStderrSub != "" && !strings.Contains(stderr, tt.wantStderrSub) {
				t.Fatalf("stderr = %q, want substring %q", stderr, tt.wantStderrSub)
			}
			if tt.wantJSON != nil {
				var got []entry
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("stdout is not valid JSON: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
				}
				if !reflect.DeepEqual(got, tt.wantJSON) {
					t.Fatalf("JSON = %+v, want %+v", got, tt.wantJSON)
				}
			}
		})
	}
}

func TestListIntegrationTestsPropagatesGrepError(t *testing.T) {
	dir := makeFixture(t, nil)
	binDir := t.TempDir()
	grepPath := filepath.Join(binDir, "grep")
	if err := os.WriteFile(grepPath, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write fake grep: %v", err)
	}

	stdout, stderr, exitCode := runScript(t, dir, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2\nstdout=%s\nstderr=%s", exitCode, stdout, stderr)
	}
	if strings.Contains(stdout, "[]") {
		t.Fatalf("stdout contains an empty matrix after grep failure: %q", stdout)
	}
}
