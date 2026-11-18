package upgrade

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	validSHA     = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"
	validVersion = "0.3.0+git.4.g1a2b3c4"
)

// stubDir holds stand-ins for the commands the helper may reach once its
// arguments pass validation. systemd-run records its argv and succeeds,
// so no test ever starts a real apt transaction; curl and wget record
// that they ran and fail, so a reintroduced HTTP fetch shows up.
func stubDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stubs := map[string]string{
		"systemd-run": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STUB_DIR/systemd-run.args\"\n",
		"curl":        "#!/bin/sh\ntouch \"$STUB_DIR/fetched\"\nexit 1\n",
		"wget":        "#!/bin/sh\ntouch \"$STUB_DIR/fetched\"\nexit 1\n",
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runHelper runs the helper with the stubs first on PATH and returns its
// stderr and exit code.
func runHelper(t *testing.T, stubs string, args ...string) (stderr string, exitCode int) {
	t.Helper()
	script, err := filepath.Abs("slagworks-upgrade")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_DIR="+stubs)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err = cmd.Run()
	if err == nil {
		return errBuf.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return errBuf.String(), exitErr.ExitCode()
	}
	t.Fatalf("run helper: %v", err)
	return "", -1
}

// TestHelper_RejectsBadArguments: the helper runs as root through sudo
// and the sudoers entry allows any arguments, so it validates them
// itself and exits before reaching apt.
func TestHelper_RejectsBadArguments(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantExit int
		wantErr  string
	}{
		{"no arguments", nil, 64, "usage: slagworks-upgrade <git-sha> <package-version>"},
		{"sha only", []string{validSHA}, 64, "usage:"},
		{"extra argument", []string{validSHA, validVersion, "extra"}, 64, "usage:"},
		{"dev build sha", []string{"dev", validVersion}, 65, "invalid git sha"},
		{"short sha", []string{"1a2b3c", validVersion}, 65, "invalid git sha"},
		{"uppercase sha", []string{"1A2B3C4D", validVersion}, 65, "invalid git sha"},
		{"sha with shell metacharacters", []string{"1a2b3c4;reboot", validVersion}, 65, "invalid git sha"},
		{"empty package version", []string{validSHA, ""}, 67, "invalid package version"},
		{"two-part package version", []string{validSHA, "0.3"}, 67, "invalid package version"},
		{"package version with apt syntax", []string{validSHA, "0.3.0 slagworks-agent=0.1.0"}, 67, "invalid package version"},
		{"package version with shell metacharacters", []string{validSHA, "0.3.0;reboot"}, 67, "invalid package version"},
		{"non-hex snapshot sha", []string{validSHA, "0.3.0+git.4.gxyz"}, 67, "invalid package version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubs := stubDir(t)
			stderr, code := runHelper(t, stubs, tt.args...)
			if code != tt.wantExit {
				t.Fatalf("exit %d, want %d; stderr:\n%s", code, tt.wantExit, stderr)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Fatalf("stderr missing %q:\n%s", tt.wantErr, stderr)
			}
			if _, err := os.Stat(filepath.Join(stubs, "systemd-run.args")); err == nil {
				t.Fatal("helper reached systemd-run with invalid arguments")
			}
		})
	}
}

// TestHelper_PinsPackageVersionWithoutFetching: valid arguments go
// straight to an exact-pin apt install of both packages at the given
// version, with no HTTP lookup of the version.
func TestHelper_PinsPackageVersionWithoutFetching(t *testing.T) {
	for _, version := range []string{"0.3.0", validVersion} {
		t.Run(version, func(t *testing.T) {
			stubs := stubDir(t)
			stderr, code := runHelper(t, stubs, validSHA, version)
			if code != 0 {
				t.Fatalf("exit %d, want 0; stderr:\n%s", code, stderr)
			}
			if _, err := os.Stat(filepath.Join(stubs, "fetched")); err == nil {
				t.Fatal("helper ran curl or wget")
			}
			raw, err := os.ReadFile(filepath.Join(stubs, "systemd-run.args"))
			if err != nil {
				t.Fatalf("helper never reached systemd-run: %v; stderr:\n%s", err, stderr)
			}
			args := string(raw)
			for _, want := range []string{
				"--wait",
				"slagworks-apt-upgrade-" + validSHA[:12],
				"slagworks=" + version + " ",
				"slagworks-agent=" + version + "\n",
			} {
				if !strings.Contains(args, want) {
					t.Errorf("systemd-run argv missing %q:\n%s", want, args)
				}
			}
		})
	}
}
