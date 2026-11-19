package bootloader

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
)

func TestGenerateBootEntry(t *testing.T) {
	entry := GenerateBootEntry("aaaa-bbbb-cccc", "console=ttyS1,115200n8")

	if !strings.Contains(entry, "title") {
		t.Fatalf("missing title, got:\n%s", entry)
	}
	if !strings.Contains(entry, "linux   /slagworks/vmlinuz") {
		t.Fatalf("missing linux line, got:\n%s", entry)
	}
	if !strings.Contains(entry, "initrd  /slagworks/initrd.img") {
		t.Fatalf("missing initrd line, got:\n%s", entry)
	}
	if !strings.Contains(entry, "root=UUID=aaaa-bbbb-cccc") {
		t.Fatalf("missing root UUID in options, got:\n%s", entry)
	}
	if !strings.Contains(entry, "console=ttyS1,115200n8") {
		t.Fatalf("missing ttyS1 console arg, got:\n%s", entry)
	}
	tty0 := strings.Index(entry, "console=tty0")
	tty1 := strings.Index(entry, "console=ttyS1")
	if tty0 < 0 || tty1 < 0 {
		t.Fatalf("missing tty0 or ttyS1 console arg, got:\n%s", entry)
	}
	if tty0 > tty1 {
		t.Fatalf("tty0 must come before ttyS1 (last console wins for /dev/console), got:\n%s", entry)
	}
}

// TestGenerateBootEntry_NoneSentinel pins that when the kernel
// params source returns "none" (e.g. switches with no SOL), no
// console= is written into the boot entry.
func TestGenerateBootEntry_NoneSentinel(t *testing.T) {
	entry := GenerateBootEntry("aaaa-bbbb-cccc", "none")
	if strings.Contains(entry, "console=") {
		t.Fatalf("none sentinel should suppress console=, got:\n%s", entry)
	}
	if !strings.Contains(entry, "root=UUID=aaaa-bbbb-cccc ro") {
		t.Fatalf("expected root + ro, got:\n%s", entry)
	}
}

// A systemd-boot entry is one "key value" per line, and the kernel params
// are pasted into its options line. They reach GenerateBootEntry only
// through FetchKernelParams, which must refuse a value that would add a
// line (or a tab the bootloader would not pass through as written) instead
// of handing it on. A trailing newline, as a YAML block scalar written to
// NetBox leaves, is trimmed and accepted.
func TestFetchKernelParams_RefusesControlCharacters(t *testing.T) {
	const rootUUID = "0f1e2d3c-4b5a-6978-8695-a4b3c2d1e0f0"
	for _, tc := range []struct {
		served string
		want   string // "" means FetchKernelParams must refuse the value
	}{
		{served: "console=ttyS1,115200n8\nlinux /evil/vmlinuz"},
		{served: "console=ttyS1,115200n8\roptions init=/bin/sh"},
		{served: "console=ttyS1,115200n8\tnomodeset"},
		{served: "console=ttyS1,115200n8\n", want: "console=ttyS1,115200n8"},
		{served: `console=ttyS1,115200n8 acpi_osi="!Windows 2012"`, want: `console=ttyS1,115200n8 acpi_osi="!Windows 2012"`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/machines/m1/kernel-params" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(tc.served))
		}))
		ctx := &agent.Context{Server: strings.TrimPrefix(srv.URL, "http://")}
		kp, err := FetchKernelParams(ctx, "m1")
		srv.Close()

		if tc.want == "" {
			if err == nil {
				t.Errorf("FetchKernelParams accepted %q as %q; the systemd-boot entry built from it is:\n%s",
					tc.served, kp, GenerateBootEntry(rootUUID, kp))
			}
			continue
		}
		if err != nil {
			t.Errorf("FetchKernelParams(%q): %v", tc.served, err)
			continue
		}
		if kp != tc.want {
			t.Errorf("FetchKernelParams(%q) = %q, want %q", tc.served, kp, tc.want)
		}
		var keys []string
		for _, line := range strings.Split(GenerateBootEntry(rootUUID, kp), "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				keys = append(keys, f[0])
			}
		}
		if got := strings.Join(keys, ","); got != "title,linux,initrd,options" {
			t.Errorf("entry for %q has keys %q, want title,linux,initrd,options", kp, got)
		}
	}
}

// TestSetGrubDefaultCmdline_ReplacesExisting covers the common case:
// the stock noble /etc/default/grub ships GRUB_CMDLINE_LINUX="" and the
// agent replaces it in place, leaving the rest of the file intact.
func TestSetGrubDefaultCmdline_ReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grub")
	orig := "GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=\"\"\nGRUB_TIMEOUT=5\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetGrubDefaultCmdline(path, "console=tty0 console=ttyS0,115200"); err != nil {
		t.Fatalf("setGrubDefaultCmdline: %v", err)
	}

	got, _ := os.ReadFile(path)
	out := string(got)
	if !strings.Contains(out, `GRUB_CMDLINE_LINUX='console=tty0 console=ttyS0,115200'`) {
		t.Fatalf("cmdline not set, got:\n%s", out)
	}
	// Untouched lines survive; the line is replaced, not duplicated.
	if !strings.Contains(out, "GRUB_DEFAULT=0") || !strings.Contains(out, "GRUB_TIMEOUT=5") {
		t.Fatalf("clobbered surrounding lines:\n%s", out)
	}
	if strings.Count(out, "GRUB_CMDLINE_LINUX=") != 1 {
		t.Fatalf("expected exactly one GRUB_CMDLINE_LINUX line:\n%s", out)
	}
}

// TestSetGrubDefaultCmdline_AppendsWhenMissing: if no GRUB_CMDLINE_LINUX
// line exists, append one rather than silently dropping the cmdline.
func TestSetGrubDefaultCmdline_AppendsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grub")
	if err := os.WriteFile(path, []byte("GRUB_TIMEOUT=5\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetGrubDefaultCmdline(path, "console=tty0"); err != nil {
		t.Fatalf("setGrubDefaultCmdline: %v", err)
	}

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `GRUB_CMDLINE_LINUX='console=tty0'`) {
		t.Fatalf("cmdline not appended, got:\n%s", got)
	}
}

// TestSetGrubDefaultCmdline_EmptyCmdline: a "none"-derived empty cmdline
// still writes a valid, empty, single-quoted GRUB_CMDLINE_LINUX line.
func TestSetGrubDefaultCmdline_EmptyCmdline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grub")
	if err := os.WriteFile(path, []byte(`GRUB_CMDLINE_LINUX="old"`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := SetGrubDefaultCmdline(path, ""); err != nil {
		t.Fatalf("setGrubDefaultCmdline: %v", err)
	}

	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `GRUB_CMDLINE_LINUX=''`) {
		t.Fatalf("expected empty cmdline, got:\n%s", got)
	}
}

// /etc/default/grub is a shell fragment: update-grub (grub-mkconfig)
// sources it as root. The value SetGrubDefaultCmdline writes must come back
// out of the shell byte for byte, and sourcing the file must not run
// anything.
func TestSetGrubDefaultCmdline_ValueIsShellQuoted(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	for _, cmdline := range []string{
		"console=tty0 console=ttyS1,115200n8 $(touch PWNED)",
		"console=tty0 console=ttyS1,115200n8 `touch PWNED`",
		"console=tty0 console=ttyS1,115200n8 root=$HOME",
		"console=tty0 console=ttyS1,115200n8\tnomodeset",
		`console=tty0 acpi_osi="!Windows 2012"`,
		`console=tty0 x='y' 'z`,
		`console=tty0 a\b \\`,
		"",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "grub")
		if err := os.WriteFile(path, []byte("GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=\"\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SetGrubDefaultCmdline(path, cmdline); err != nil {
			t.Fatalf("SetGrubDefaultCmdline: %v", err)
		}
		cmd := exec.Command("/bin/sh", "-c", `. "$1"; printf '%s' "$GRUB_CMDLINE_LINUX"`, "sh", path)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/expanded-home"}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sourcing the generated file for %q failed: %v", cmdline, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "PWNED")); err == nil {
			t.Errorf("sourcing /etc/default/grub executed a command embedded in the cmdline %q", cmdline)
		}
		if string(out) != cmdline {
			t.Errorf("grub reads GRUB_CMDLINE_LINUX=%q, want %q", out, cmdline)
		}
	}
}
