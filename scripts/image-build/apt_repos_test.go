package main_test

// Exercises apt-repos.sh's emitted mmdebstrap arguments against a fake chroot,
// so the deployed host's third-party apt config (what a converge and plain
// `apt upgrade` see) is pinned without a 20-minute image build. The hooks run
// exactly as mmdebstrap runs a non-file customize-hook: `sh -c <hook> exec
// <chroot>`.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// aptRepos sources apt-repos.sh in bash and runs fn with args, returning one
// value per output line. fn "rows" prints THIRD_PARTY_APT_REPOS.
func aptRepos(t *testing.T, fn string, args ...string) []string {
	t.Helper()
	script := `source ./apt-repos.sh; "$@"`
	if fn == "rows" {
		script = `source ./apt-repos.sh; printf '%s\n' "${THIRD_PARTY_APT_REPOS[@]}"`
	}
	cmd := exec.Command("bash", append([]string{"-c", script, "bash", fn}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", fn, args, err, out)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

type bakedRepo struct {
	list, keyring string
}

// bakeRepos simulates the deploy-chroot build: mmdebstrap writes the base
// mirrors plus repo_deb_lines into /etc/apt/sources.list, then runs each
// repo_customize_hooks hook. Returns the chroot and the build keydir.
func bakeRepos(t *testing.T, suite string) (string, string, []bakedRepo) {
	t.Helper()
	if runtime.GOOS != "linux" {
		// The hooks use GNU sed -i; they only ever run under mmdebstrap on Linux.
		t.Skip("apt-repos.sh hooks target GNU userland (mmdebstrap on Linux)")
	}
	keydir := t.TempDir()
	chroot := t.TempDir()

	var repos []bakedRepo
	for _, row := range aptRepos(t, "rows") {
		f := strings.Split(row, "|")
		if len(f) != 5 {
			t.Fatalf("THIRD_PARTY_APT_REPOS row %q: want 5 fields", row)
		}
		repos = append(repos, bakedRepo{list: f[0], keyring: f[1]})
		// 0600 to prove the hook normalizes the deployed keyring to 0644
		// rather than inheriting the fetched file's mode.
		if err := os.WriteFile(filepath.Join(keydir, f[1]+".gpg"), []byte("key:"+f[1]), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	base := "deb http://archive.ubuntu.com/ubuntu " + suite + " main restricted universe multiverse\n"
	lines := aptRepos(t, "repo_deb_lines", keydir, suite)
	if err := os.MkdirAll(filepath.Join(chroot, "etc/apt"), 0o755); err != nil {
		t.Fatal(err)
	}
	sl := base + "\n" + strings.Join(lines, "\n\n") + "\n"
	if err := os.WriteFile(filepath.Join(chroot, "etc/apt/sources.list"), []byte(sl), 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := aptRepos(t, "repo_customize_hooks", keydir, suite)
	if len(hooks) != len(repos) {
		t.Fatalf("got %d hooks for %d repos", len(hooks), len(repos))
	}
	for _, h := range hooks {
		script, ok := strings.CutPrefix(h, "--customize-hook=")
		if !ok {
			t.Fatalf("hook %q lacks --customize-hook= prefix", h)
		}
		old := syscall.Umask(0o077)
		out, err := exec.Command("sh", "-c", script, "exec", chroot).CombinedOutput()
		syscall.Umask(old)
		if err != nil {
			t.Fatalf("hook failed: %v\n%s\nhook: %s", err, out, script)
		}
	}
	return chroot, keydir, repos
}

// TestTailscaleRepoPersistedOnDeployedHost pins the paths and content Tailscale's
// install instructions and site Ansible's tailscale role write, so a converge's
// `creates:` guard sees the repo as present and `apt upgrade` can update it.
func TestTailscaleRepoPersistedOnDeployedHost(t *testing.T) {
	chroot, _, _ := bakeRepos(t, "noble")

	got, err := os.ReadFile(filepath.Join(chroot, "etc/apt/sources.list.d/tailscale.list"))
	if err != nil {
		t.Fatalf("tailscale.list not written: %v", err)
	}
	want := "deb [signed-by=/usr/share/keyrings/tailscale-archive-keyring.gpg] https://pkgs.tailscale.com/stable/ubuntu noble main\n"
	if string(got) != want {
		t.Errorf("tailscale.list =\n%q\nwant\n%q", got, want)
	}

	kr := filepath.Join(chroot, "usr/share/keyrings/tailscale-archive-keyring.gpg")
	st, err := os.Stat(kr)
	if err != nil {
		t.Fatalf("tailscale keyring not installed: %v", err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("tailscale keyring mode = %o, want 644", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(kr); string(b) != "key:tailscale-archive-keyring" {
		t.Errorf("tailscale keyring content = %q, want the build-fetched key", b)
	}
}

// TestRepoBakeLeavesNoBuildPathsOrDuplicates checks every table row: each repo
// lives only in its own sources.list.d file (no duplicate in sources.list, no
// signed-by pointing at a build path absent on the deployed host), the base
// Ubuntu mirrors are untouched, and the suite comes from the build's SUITE.
func TestRepoBakeLeavesNoBuildPathsOrDuplicates(t *testing.T) {
	const suite = "jammy" // not noble: proves the codename is not hardcoded
	chroot, keydir, repos := bakeRepos(t, suite)

	sl, err := os.ReadFile(filepath.Join(chroot, "etc/apt/sources.list"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sl), keydir) || strings.Contains(string(sl), "signed-by") {
		t.Errorf("sources.list still carries third-party/build-path lines:\n%s", sl)
	}
	if !strings.Contains(string(sl), "ubuntu "+suite+" main restricted universe multiverse") {
		t.Errorf("sources.list lost the base Ubuntu mirror:\n%s", sl)
	}

	for _, r := range repos {
		b, err := os.ReadFile(filepath.Join(chroot, "etc/apt/sources.list.d", r.list+".list"))
		if err != nil {
			t.Errorf("%s.list not written: %v", r.list, err)
			continue
		}
		line := string(b)
		if !strings.HasPrefix(line, "deb [signed-by=/usr/share/keyrings/"+r.keyring+".gpg] ") ||
			!strings.Contains(line, " "+suite+" ") || strings.Count(line, "\n") != 1 {
			t.Errorf("%s.list = %q: want one deb line signed-by /usr/share/keyrings/%s.gpg for suite %s", r.list, line, r.keyring, suite)
		}
		if _, err := os.Stat(filepath.Join(chroot, "usr/share/keyrings", r.keyring+".gpg")); err != nil {
			t.Errorf("keyring %s not installed: %v", r.keyring, err)
		}
	}
}

// TestBakedSourcesHookReplacesBuildMirror pins the split between the mirror
// the build downloads from and the one the image ships: after the hook, the
// deployed host's sources.list is exactly the baked mirror's three pockets,
// with nothing of the build mirror (which bakeRepos writes, as mmdebstrap
// does) and the third-party repos still in their own files.
func TestBakedSourcesHookReplacesBuildMirror(t *testing.T) {
	const suite = "noble"
	const baked = "https://baked.example/ubuntu"
	chroot, _, repos := bakeRepos(t, suite)

	lines := aptRepos(t, "ubuntu_mirror_lines", baked, suite)
	want := "deb " + baked + " noble main restricted universe multiverse\n" +
		"deb " + baked + " noble-updates main restricted universe multiverse\n" +
		"deb " + baked + " noble-security main restricted universe multiverse\n"
	if got := strings.Join(lines, "\n") + "\n"; got != want {
		t.Fatalf("ubuntu_mirror_lines =\n%s\nwant\n%s", got, want)
	}
	// 0600 to prove the hook sets the deployed file's mode itself.
	src := filepath.Join(t.TempDir(), "baked-sources.list")
	if err := os.WriteFile(src, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}

	hooks := aptRepos(t, "baked_sources_hook", src)
	if len(hooks) != 1 {
		t.Fatalf("baked_sources_hook emitted %d lines, want 1: %q", len(hooks), hooks)
	}
	script, ok := strings.CutPrefix(hooks[0], "--customize-hook=")
	if !ok {
		t.Fatalf("hook %q lacks --customize-hook= prefix", hooks[0])
	}
	old := syscall.Umask(0o077)
	out, err := exec.Command("sh", "-c", script, "exec", chroot).CombinedOutput()
	syscall.Umask(old)
	if err != nil {
		t.Fatalf("hook failed: %v\n%s\nhook: %s", err, out, script)
	}

	slPath := filepath.Join(chroot, "etc/apt/sources.list")
	got, err := os.ReadFile(slPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("sources.list =\n%s\nwant only the baked mirror:\n%s", got, want)
	}
	st, err := os.Stat(slPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("sources.list mode = %o, want 644", st.Mode().Perm())
	}
	for _, r := range repos {
		if _, err := os.Stat(filepath.Join(chroot, "etc/apt/sources.list.d", r.list+".list")); err != nil {
			t.Errorf("%s.list lost: %v", r.list, err)
		}
	}
}
