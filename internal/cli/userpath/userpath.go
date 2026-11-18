// Package userpath resolves the per-user config and state directories
// slagctl keeps files in, and reads and writes the small secret files (the
// OIDC token cache) it stores there.
//
// It deliberately does NOT use os.UserConfigDir. On macOS that resolves to
// ~/Library/Application Support (a hardcoded runtime.GOOS branch that ignores
// $XDG_CONFIG_HOME), which would put an operator's token in a different place
// on their Mac than on the Linux hosts they also run slagctl from. Go's stdlib
// has no state-directory resolver at all.
//
// Resolution follows the XDG Base Directory spec's variables, but with the
// unix-style $HOME/... fallbacks applied on ALL platforms (not just Linux):
//
//   - ConfigDir: $XDG_CONFIG_HOME (if set and absolute) else $HOME/.config
//   - StateDir:  $XDG_STATE_HOME  (if set and absolute) else $HOME/.local/state
//
// Pick the directory by what the file IS:
//   - config: durable operator settings and credentials (the token cache).
//     Worth keeping across machines and reinstalls.
//   - state:  machine-local runtime state between runs (update-check markers
//     and their lock). Not portable, safe to lose.
package userpath

import (
	"fmt"
	"os"
	"path/filepath"
)

// ConfigDir returns the per-application config directory:
// <base>/<app>, where base is $XDG_CONFIG_HOME (if set and absolute) or
// $HOME/.config. When app is empty, the base directory itself is returned.
func ConfigDir(app string) (string, error) {
	base, err := resolveBase("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return join(base, app), nil
}

// ConfigFile returns <ConfigDir(app)>/<name>.
func ConfigFile(app, name string) (string, error) {
	dir, err := ConfigDir(app)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// StateDir returns the per-application state directory:
// <base>/<app>, where base is $XDG_STATE_HOME (if set and absolute) or
// $HOME/.local/state. When app is empty, the base is returned.
func StateDir(app string) (string, error) {
	base, err := resolveBase("XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return "", err
	}
	return join(base, app), nil
}

// StateFile returns <StateDir(app)>/<name>.
func StateFile(app, name string) (string, error) {
	dir, err := StateDir(app)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// resolveBase returns the XDG base dir for the given env var, falling back
// to $HOME/<fallback>. A relative $XDG_* value is rejected (the spec
// requires absolute paths) rather than silently joined onto the cwd.
func resolveBase(envVar, fallback string) (string, error) {
	if v := os.Getenv(envVar); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("userpath: %s is set to a relative path %q; set it to an absolute path or unset it", envVar, v)
		}
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("userpath: resolving home dir: %w", err)
	}
	return filepath.Join(home, fallback), nil
}

// join appends app to base, treating an empty app as "the base itself".
func join(base, app string) string {
	if app == "" {
		return base
	}
	return filepath.Join(base, app)
}

// WriteFile writes data to path with mode 0600, creating the parent dir
// 0700 if needed, via a temp file + atomic rename, so a partial write never
// leaves a half-written secret at the destination.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".userpath-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ReadFile reads the file at path. It is a thin wrapper over os.ReadFile
// so callers route both halves of the token-file lifecycle through this
// package; the error (including os.IsNotExist) is returned unwrapped so
// callers can branch on "no token yet".
func ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // path derived from userpath resolvers + caller-fixed name
}
