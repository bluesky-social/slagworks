package userpath

import (
	"os"
	"path/filepath"
	"testing"
)

// setHome points $HOME at a temp dir and clears the XDG vars so the
// fallback branch is exercised deterministically regardless of the host
// environment (CI, a dev laptop with XDG set, etc.).
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	return home
}

func TestConfigDir_FallbackIsUnixStyle(t *testing.T) {
	home := setHome(t)
	got, err := ConfigDir("slagctl")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "slagctl")
	if got != want {
		t.Errorf("ConfigDir = %q, want %q (must be ~/.config, never ~/Library)", got, want)
	}
}

func TestStateDir_FallbackIsUnixStyle(t *testing.T) {
	home := setHome(t)
	got, err := StateDir("slagctl")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "slagctl")
	if got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

func TestStateFile_XDGHonored(t *testing.T) {
	setHome(t)
	xdgState := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdgState)
	got, err := StateFile("slagctl", "update-check.lock")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdgState, "slagctl", "update-check.lock"); got != want {
		t.Errorf("StateFile = %q, want %q", got, want)
	}
}

func TestConfigFile(t *testing.T) {
	home := setHome(t)
	got, err := ConfigFile("slagctl", "token.json")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "slagctl", "token.json")
	if got != want {
		t.Errorf("ConfigFile = %q, want %q", got, want)
	}
}

func TestConfigDir_EmptyAppIsBase(t *testing.T) {
	home := setHome(t)
	got, err := ConfigFile("", "shared-token")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "shared-token")
	if got != want {
		t.Errorf("ConfigFile(\"\", ...) = %q, want %q", got, want)
	}
}

func TestConfigDir_XDGHonored(t *testing.T) {
	setHome(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	got, err := ConfigDir("slagctl")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "slagctl")
	if got != want {
		t.Errorf("ConfigDir = %q, want %q (XDG_CONFIG_HOME must win)", got, want)
	}
}

func TestConfigDir_RelativeXDGRejected(t *testing.T) {
	setHome(t)
	t.Setenv("XDG_CONFIG_HOME", "relative/path")
	if _, err := ConfigDir("x"); err == nil {
		t.Error("expected error for relative XDG_CONFIG_HOME, got nil")
	}
}

func TestWriteReadFile_RoundTripAndPerms(t *testing.T) {
	home := setHome(t)
	path := filepath.Join(home, ".config", "toolx", "token.json")
	want := []byte(`{"access_token":"abc"}`)
	if err := WriteFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("round-trip = %q, want %q", got, want)
	}
	// File 0600, dir 0700.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file perm = %o, want 0600", perm)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}
}

func TestReadFile_NotExistIsDetectable(t *testing.T) {
	home := setHome(t)
	_, err := ReadFile(filepath.Join(home, ".config", "toolx", "nope"))
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist error, got %v", err)
	}
}
