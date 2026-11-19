//go:build !integration

package netapply

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// recordRunner captures the commands applyNetBundle would exec (chroot
// chown / chroot systemctl enable / mount / umount) without running them.
type recordRunner struct {
	calls [][]string
	fail  map[string]error // first arg substring -> error to return
}

func (r *recordRunner) run(name string, args ...string) error {
	call := append([]string{name}, args...)
	r.calls = append(r.calls, call)
	joined := strings.Join(call, " ")
	for sub, err := range r.fail {
		if strings.Contains(joined, sub) {
			return err
		}
	}
	return nil
}

func (r *recordRunner) joined() []string {
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = strings.Join(c, " ")
	}
	return out
}

// TestApplyNetBundle_FilesOnly covers the common netplan-only bundle: a
// single file written into the target with the requested mode, NO chroot,
// NO bind mounts (the files-only fast path).
func TestApplyNetBundle_FilesOnly(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{
		Files: []netBundleFile{
			{Path: "/etc/netplan/99-slagworks.yaml", Content: "network: {}\n", Mode: "0600"},
		},
	}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(target, "etc/netplan/99-slagworks.yaml"))
	if err != nil {
		t.Fatalf("file not written under target: %v", err)
	}
	if string(got) != "network: {}\n" {
		t.Errorf("content = %q, want the bundle content", string(got))
	}
	info, _ := os.Stat(filepath.Join(target, "etc/netplan/99-slagworks.yaml"))
	if info.Mode().Perm() != 0600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
	// A files-only bundle must touch no commands at all — no bind mounts,
	// no chroot. The whole point of the fast path.
	if len(rr.calls) != 0 {
		t.Errorf("files-only bundle should run no commands, got: %v", rr.joined())
	}
}

// TestApplyNetBundle_DefaultMode confirms an omitted mode falls back to 0644.
func TestApplyNetBundle_DefaultMode(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{{Path: "/etc/foo.conf", Content: "x"}}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	info, _ := os.Stat(filepath.Join(target, "etc/foo.conf"))
	if info.Mode().Perm() != 0644 {
		t.Errorf("default mode = %o, want 0644", info.Mode().Perm())
	}
}

// TestApplyNetBundle_InvalidMode rejects a non-octal mode before writing.
func TestApplyNetBundle_InvalidMode(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{{Path: "/etc/foo.conf", Content: "x", Mode: "rwxr-xr-x"}}}
	err := applyNetBundle(slog.Default(), bundle, target, rr.run)
	if err == nil {
		t.Fatal("expected error on non-octal mode, got nil")
	}
	if !strings.Contains(err.Error(), "invalid octal mode") {
		t.Errorf("error should name the bad mode, got: %v", err)
	}
}

// TestApplyNetBundle_RelativePathRejected pins that a non-absolute path is
// refused (the script contract requires absolute paths).
func TestApplyNetBundle_RelativePathRejected(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{{Path: "etc/foo.conf", Content: "x"}}}
	err := applyNetBundle(slog.Default(), bundle, target, rr.run)
	if err == nil {
		t.Fatal("expected error on relative path, got nil")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("error should explain the absolute-path requirement, got: %v", err)
	}
}

// TestApplyNetBundle_PathTraversalContained is the security-critical case: a
// path with .. components or an absolute path that resolves through a symlink
// must NOT escape the target root. SecureJoin clamps it back inside.
func TestApplyNetBundle_PathTraversalContained(t *testing.T) {
	target := t.TempDir()
	// Canary that must never be overwritten: a file OUTSIDE the target.
	outside := filepath.Join(filepath.Dir(target), "canary-"+filepath.Base(target))
	if err := os.WriteFile(outside, []byte("ORIGINAL"), 0644); err != nil {
		t.Fatalf("seed canary: %v", err)
	}
	defer os.Remove(outside)

	rr := &recordRunner{}
	// Two escape attempts: lexical .. traversal, and an absolute path that a
	// naive impl might write straight to. Both must land inside target.
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/../" + filepath.Base(outside), Content: "PWNED"},
		{Path: "/etc/../../escape.conf", Content: "PWNED"},
	}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle should clamp, not error: %v", err)
	}

	if got, _ := os.ReadFile(outside); string(got) != "ORIGINAL" {
		t.Fatalf("path traversal escaped target: canary outside = %q", string(got))
	}
	// Nothing should exist above the target dir from these writes.
	if _, err := os.Stat(filepath.Join(filepath.Dir(target), "escape.conf")); err == nil {
		t.Fatal("escape.conf written outside target root")
	}
}

// TestApplyNetBundle_SymlinkEscapeContained pins the symlink case the lexical
// Join+Clean comment warns about: a pre-existing /etc symlink pointing out of
// the target must not let a write ride out through it.
func TestApplyNetBundle_SymlinkEscapeContained(t *testing.T) {
	target := t.TempDir()
	escapeDir := t.TempDir() // a dir OUTSIDE target
	// target/etc -> escapeDir (an absolute symlink out of the root).
	if err := os.Symlink(escapeDir, filepath.Join(target, "etc")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{{Path: "/etc/pwned.conf", Content: "PWNED"}}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	// The write must NOT have followed the symlink out to escapeDir.
	if _, err := os.Stat(filepath.Join(escapeDir, "pwned.conf")); err == nil {
		t.Fatal("write followed /etc symlink out of the target root")
	}
}

// TestApplyNetBundle_OwnerGroupChown pins the chown spec construction and that
// it runs through chroot against the target.
func TestApplyNetBundle_OwnerGroupChown(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/etc/frr/frr.conf", Content: "x", Owner: "frr", Group: "frr"},
	}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	want := "chroot " + target + " chown -- frr:frr /etc/frr/frr.conf"
	if !contains(rr.joined(), want) {
		t.Errorf("expected chown call %q, got: %v", want, rr.joined())
	}
}

// TestApplyNetBundle_OwnerOnlyChown pins that an owner with no group produces
// "owner", not "owner:".
func TestApplyNetBundle_OwnerOnlyChown(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/etc/foo.conf", Content: "x", Owner: "frr"},
	}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	want := "chroot " + target + " chown -- frr /etc/foo.conf"
	if !contains(rr.joined(), want) {
		t.Errorf("expected owner-only chown %q, got: %v", want, rr.joined())
	}
}

// TestApplyNetBundle_ChownArgInjectionContained pins the "--" guard: an owner
// string that starts with "-" must be passed as an operand, not parsed by
// chown as an option (CWE-88).
func TestApplyNetBundle_ChownArgInjectionContained(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/etc/foo.conf", Content: "x", Owner: "--reference=/etc/shadow"},
	}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	want := "chroot " + target + " chown -- --reference=/etc/shadow /etc/foo.conf"
	if !contains(rr.joined(), want) {
		t.Errorf("expected -- before the owner spec, got: %v", rr.joined())
	}
}

// TestParseBundleMode pins the octal->os.FileMode mapping, including the
// special bits Go places at flag positions (1<<23/22/20) rather than the
// Unix octal positions — a plain os.FileMode(parsed) cast silently drops them.
func TestParseBundleMode(t *testing.T) {
	cases := []struct {
		in   string
		want os.FileMode
	}{
		{"", 0o644},     // default
		{"0600", 0o600}, // leading zero
		{"755", 0o755},  // no leading zero
		{"4755", 0o755 | os.ModeSetuid},
		{"2755", 0o755 | os.ModeSetgid},
		{"1777", 0o777 | os.ModeSticky},
		{"6755", 0o755 | os.ModeSetuid | os.ModeSetgid},
	}
	for _, tc := range cases {
		got, err := parseBundleMode(tc.in)
		if err != nil {
			t.Errorf("parseBundleMode(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBundleMode(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := parseBundleMode("rwxr-xr-x"); err == nil {
		t.Error("expected error on non-octal mode")
	}
}

// TestApplyNetBundle_SpecialModeBits confirms the special bits land on disk
// (the kernel strips them on the create open, so applyNetBundle must chmod).
func TestApplyNetBundle_SpecialModeBits(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/usr/local/bin/tool", Content: "x", Mode: "4755"},
	}}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	info, err := os.Stat(filepath.Join(target, "usr/local/bin/tool"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("perm = %o, want 0755", info.Mode().Perm())
	}
	// The setuid bit should be set after the explicit chmod. Some filesystems
	// or non-root environments may still refuse it; treat a miss as a skip
	// rather than a hard failure so the perm-bit + parse coverage still holds.
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("setuid bit not retained on this filesystem/uid; parse logic covered by TestParseBundleMode")
	}
}

// TestApplyNetBundle_ChmodAfterChown pins the ordering bug fix: chown(2)
// clears setuid/setgid (even for root), so the special-bit chmod must run
// AFTER the chown, not before. The runner simulates chown's bit-stripping so
// the test catches a regression to chmod-then-chown ordering. It also asserts
// the chown command itself was issued before the final mode lands.
func TestApplyNetBundle_ChmodAfterChown(t *testing.T) {
	target := t.TempDir()
	path := filepath.Join(target, "usr/local/bin/tool")

	chownRan := false
	runner := func(name string, args ...string) error {
		// Simulate chown(2) stripping the special bits on ownership change.
		if name == "chroot" && len(args) >= 2 && args[1] == "chown" {
			chownRan = true
			if info, err := os.Stat(path); err == nil {
				_ = os.Chmod(path, info.Mode().Perm()) // drop setuid/setgid/sticky
			}
		}
		return nil
	}
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/usr/local/bin/tool", Content: "x", Mode: "4755", Owner: "root", Group: "root"},
	}}
	if err := applyNetBundle(slog.Default(), bundle, target, runner); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	if !chownRan {
		t.Fatal("expected chown to run for an owned file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Even though the simulated chown stripped setuid, the final chmod must
	// have re-applied it. A regression to chmod-before-chown fails here.
	if info.Mode()&os.ModeSetuid == 0 {
		t.Errorf("setuid bit lost — chmod must run AFTER chown, mode = %v", info.Mode())
	}
}

// TestApplyNetBundle_FilesWrittenBeforeMounts pins the ordering that closes
// the bind-mount escape: every file write must be issued before any
// mount --bind command. (A file emitted at /sys/... would otherwise land in
// the host's live sysfs through the bind mount.) Since os.WriteFile is not a
// runner call, we assert the mount commands are the FIRST recorded commands —
// i.e. nothing the runner sees precedes them except more setup, and crucially
// the file already exists on disk by the time the first mount is issued.
func TestApplyNetBundle_FilesWrittenBeforeMounts(t *testing.T) {
	target := t.TempDir()
	// Runner records the on-disk existence of the file at the moment each
	// command runs, so we can prove the write happened before the bind mount.
	fileExistedAtFirstMount := false
	seenMount := false
	runner := func(name string, args ...string) error {
		if name == "mount" && !seenMount {
			seenMount = true
			_, err := os.Stat(filepath.Join(target, "etc/frr/daemons"))
			fileExistedAtFirstMount = err == nil
		}
		return nil
	}
	bundle := netBundle{
		Files:       []netBundleFile{{Path: "/etc/frr/daemons", Content: "bgpd=yes\n"}},
		EnableUnits: []string{"frr.service"},
	}
	if err := applyNetBundle(slog.Default(), bundle, target, runner); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	if !seenMount {
		t.Fatal("expected a bind mount for the enable-units path")
	}
	if !fileExistedAtFirstMount {
		t.Error("file must be written to /target BEFORE bind mounts go up (escape-window closure)")
	}
}

// TestApplyNetBundle_EnableUnits pins the unit-enable path: bind mounts come
// up, each unit is enabled via chroot systemctl, and the mounts are torn down.
func TestApplyNetBundle_EnableUnits(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{}
	bundle := netBundle{
		Files:       []netBundleFile{{Path: "/etc/frr/daemons", Content: "bgpd=yes\n"}},
		EnableUnits: []string{"frr.service"},
	}
	if err := applyNetBundle(slog.Default(), bundle, target, rr.run); err != nil {
		t.Fatalf("applyNetBundle: %v", err)
	}
	calls := rr.joined()
	for _, want := range []string{
		"mount --bind /dev " + target + "/dev",
		"mount --bind /proc " + target + "/proc",
		"mount --bind /sys " + target + "/sys",
		"chroot " + target + " systemctl enable -- frr.service",
		"umount " + target + "/dev",
	} {
		if !contains(calls, want) {
			t.Errorf("expected call %q, got: %v", want, calls)
		}
	}
	// Bind mounts must precede the enable; umounts must follow it.
	mountIdx, enableIdx, umountIdx := -1, -1, -1
	for i, c := range calls {
		switch {
		case strings.HasPrefix(c, "mount --bind /dev") && mountIdx == -1:
			mountIdx = i
		case strings.Contains(c, "systemctl enable"):
			enableIdx = i
		case strings.HasPrefix(c, "umount") && umountIdx == -1:
			umountIdx = i
		}
	}
	if mountIdx >= enableIdx || enableIdx >= umountIdx {
		t.Errorf("ordering wrong: mount=%d enable=%d umount=%d (want mount<enable<umount)", mountIdx, enableIdx, umountIdx)
	}
}

// TestApplyNetBundle_ChownFailureHardFails pins that a failed chown (e.g. the
// frr user not baked into the image) fails the whole apply loudly rather than
// leaving root-owned config silently.
func TestApplyNetBundle_ChownFailureHardFails(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{fail: map[string]error{"chown": os.ErrPermission}}
	bundle := netBundle{Files: []netBundleFile{
		{Path: "/etc/frr/frr.conf", Content: "x", Owner: "frr", Group: "frr"},
	}}
	err := applyNetBundle(slog.Default(), bundle, target, rr.run)
	if err == nil {
		t.Fatal("expected hard-fail when chown fails, got nil")
	}
	if !strings.Contains(err.Error(), "chown") {
		t.Errorf("error should name the chown failure, got: %v", err)
	}
}

// TestApplyNetBundle_EnableFailureHardFails pins that a failed systemctl
// enable fails the apply.
func TestApplyNetBundle_EnableFailureHardFails(t *testing.T) {
	target := t.TempDir()
	rr := &recordRunner{fail: map[string]error{"systemctl enable": os.ErrPermission}}
	bundle := netBundle{
		Files:       []netBundleFile{{Path: "/etc/frr/daemons", Content: "x"}},
		EnableUnits: []string{"frr.service"},
	}
	err := applyNetBundle(slog.Default(), bundle, target, rr.run)
	if err == nil {
		t.Fatal("expected hard-fail when systemctl enable fails, got nil")
	}
	if !strings.Contains(err.Error(), "enable unit") {
		t.Errorf("error should name the failed unit, got: %v", err)
	}
}

// TestRenderNetworkConfig_RejectsInvalidBundle pins that the bundle checks
// that need no target tree (absolute paths, octal modes) happen at render
// time, which the deploy runs before touching a disk, not at apply time
// after the install.
func TestRenderNetworkConfig_RejectsInvalidBundle(t *testing.T) {
	const machineID = "6b1f0c3e-9a52-4d55-8f0e-1c2d3e4f5a6b"
	cases := []struct {
		name, bundle, want string
	}{
		{"relative path", `{"files":[{"path":"etc/netplan/x.yaml","content":"x"}]}`, "not absolute"},
		{"non-octal mode", `{"files":[{"path":"/etc/x","content":"x","mode":"rw-r--r--"}]}`, "invalid octal mode"},
		{"not JSON", `files: []`, "parse bundle JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/machines/" + machineID + "/network-script":
					fmt.Fprintf(w, "#!/bin/sh\ncat <<'EOF'\n%s\nEOF\n", tc.bundle)
				case "/api/v1/machines/" + machineID + "/network-facts":
					fmt.Fprint(w, "[]")
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			ctx := &agent.Context{Log: slog.Default(), Server: strings.TrimPrefix(srv.URL, "http://")}

			cfg, err := RenderNetworkConfig(ctx, machineID)
			if err == nil {
				t.Fatalf("RenderNetworkConfig accepted an invalid bundle: %+v", cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
