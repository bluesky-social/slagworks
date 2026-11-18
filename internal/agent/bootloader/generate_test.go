package bootloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.Contains(out, `GRUB_CMDLINE_LINUX="console=tty0 console=ttyS0,115200"`) {
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
	if !strings.Contains(string(got), `GRUB_CMDLINE_LINUX="console=tty0"`) {
		t.Fatalf("cmdline not appended, got:\n%s", got)
	}
}

// TestSetGrubDefaultCmdline_EmptyCmdline: a "none"-derived empty cmdline
// still writes a valid (empty) GRUB_CMDLINE_LINUX="" line.
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
	if !strings.Contains(string(got), `GRUB_CMDLINE_LINUX=""`) {
		t.Fatalf("expected empty cmdline, got:\n%s", got)
	}
}
