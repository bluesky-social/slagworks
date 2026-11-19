package bootloader

// Property-based coverage for the two bootloader configs the agent generates
// from the per-machine kernel params (NetBox device-type
// slagworks_kernel_params). The value is taken through the same path the
// installer uses: FetchKernelParams reads it from the site worker, then
//
//   - GenerateBootEntry writes a systemd-boot (Boot Loader Specification
//     type #1) entry, read back here by an independent BLS line parser;
//   - SetGrubDefaultCmdline writes a GRUB_CMDLINE_LINUX assignment into
//     /etc/default/grub, read back here the way grub-mkconfig reads it: by
//     sourcing the file with /bin/sh.
//
// The invariants are:
//   - FetchKernelParams refuses exactly the values that are empty or still
//     hold a control character once surrounding whitespace is dropped
//   - for every value it accepts, the kernel arguments the installed OS
//     boots with are exactly the intended ones
//   - the generators are deterministic, SetGrubDefaultCmdline is idempotent
//     and preserves every unrelated line
//   - no value adds a line or a key, or is expanded or run by the shell

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// kernelParamsSource is a site worker that serves one kernel-params value,
// so each drawn value reaches the generators through FetchKernelParams.
type kernelParamsSource struct {
	mu     sync.Mutex
	served string
	ctx    *agent.Context
}

func newKernelParamsSource(t *testing.T) *kernelParamsSource {
	t.Helper()
	src := &kernelParamsSource{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		src.mu.Lock()
		defer src.mu.Unlock()
		_, _ = w.Write([]byte(src.served))
	}))
	t.Cleanup(srv.Close)
	src.ctx = &agent.Context{Server: strings.TrimPrefix(srv.URL, "http://")}
	return src
}

// fetch serves value and returns what FetchKernelParams makes of it. ok is
// false when the value was refused; a refusal the model does not expect, or
// an acceptance it does not expect, fails the test.
func (s *kernelParamsSource) fetch(t *rapid.T, value string) (kp string, ok bool) {
	s.mu.Lock()
	s.served = value
	s.mu.Unlock()
	kp, err := FetchKernelParams(s.ctx, "m1")

	want := strings.TrimSpace(value)
	refuse := value == ""
	for _, r := range want {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			refuse = true
		}
	}
	switch {
	case refuse && err == nil:
		t.Fatalf("FetchKernelParams accepted %q as %q; it is empty or holds a control character", value, kp)
	case !refuse && err != nil:
		t.Fatalf("FetchKernelParams(%q): %v", value, err)
	case !refuse && kp != want:
		t.Fatalf("FetchKernelParams(%q) = %q, want %q", value, kp, want)
	}
	return kp, err == nil
}

// blsParse reads a type #1 entry: one "key value" per line, '#' comments,
// repeated `options` concatenated with a space.
func blsParse(entry string) (keys []string, options string) {
	var opts []string
	for _, line := range strings.Split(entry, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		i := strings.IndexAny(trimmed, " \t")
		key, val := trimmed, ""
		if i >= 0 {
			key, val = trimmed[:i], strings.TrimLeft(trimmed[i:], " \t")
		}
		keys = append(keys, key)
		if key == "options" {
			opts = append(opts, val)
		}
	}
	return keys, strings.Join(opts, " ")
}

// asciiFields splits on the separators a kernel command line has.
func asciiFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
}

var benignKP = []string{
	"console=ttyS1,115200n8",
	"console=ttyS0,115200n8",
	"console=ttyS1,115200n8 console=ttyS0,115200n8",
	"console=ttyS0,57600n8",
	"console=ttyS1,115200n8 nomodeset",
	"console=ttyS1,115200n8 intel_iommu=on iommu=pt",
	"console=hvc0",
}

var kpFragments = []string{
	"\nlinux /evil/vmlinuz", "\noptions init=/bin/sh", "\n", "\r", "\t",
	"$(touch PWNED)", "`touch PWNED`", "$HOME", "$", "${x}", "\"", "'", "\\", "\\n", "#", ";", "&", "|",
	"\x01", "\x7f", "\u0085", "\u00e9", "\u2028", "!", "*", "  ", "=", "%",
}

// genKP returns a kernel-params value as the site worker serves it, and
// whether it is in the documented value space (printable ASCII kernel
// arguments separated by single spaces). Outside that space a fragment that
// is special to a line-oriented config or to the shell is spliced in.
func genKP(t *rapid.T, allowHostile bool) (string, bool) {
	max := 5
	if allowHostile {
		max = 9
	}
	switch rapid.IntRange(0, max).Draw(t, "kp_class") {
	case 0:
		return "none", true
	case 1, 2, 3, 4, 5:
		return rapid.SampledFrom(benignKP).Draw(t, "kp"), true
	default:
		s := rapid.SampledFrom(benignKP).Draw(t, "kp_base")
		n := rapid.IntRange(1, 3).Draw(t, "nfrag")
		for i := 0; i < n; i++ {
			frag := rapid.SampledFrom(kpFragments).Draw(t, "frag")
			pos := rapid.IntRange(0, len(s)).Draw(t, "pos")
			s = s[:pos] + frag + s[pos:]
		}
		return s, false
	}
}

func wantKernelArgs(kp string) []string {
	if kp == "none" {
		return nil
	}
	return append([]string{"console=tty0"}, asciiFields(kp)...)
}

// TestProp_GenerateBootEntry_RoundTrip: the entry has exactly the four
// documented keys and its options are root=UUID=<uuid> ro plus the intended
// kernel arguments, for every kernel-params value FetchKernelParams accepts.
func TestProp_GenerateBootEntry_RoundTrip(t *testing.T) {
	t.Run("documented_values", func(t *testing.T) { checkBootEntryRoundTrip(t, false) })
	t.Run("any_value", func(t *testing.T) { checkBootEntryRoundTrip(t, true) })
}

func checkBootEntryRoundTrip(t *testing.T, allowHostile bool) {
	src := newKernelParamsSource(t)
	var benign, hostile, none, refused int
	rapid.Check(t, func(t *rapid.T) {
		served, documented := genKP(t, allowHostile)
		kp, ok := src.fetch(t, served)
		if !ok {
			refused++
			return
		}
		rootUUID := rapid.SampledFrom([]string{
			"0f1e2d3c-4b5a-6978-8695-a4b3c2d1e0f0", "1234-ABCD",
		}).Draw(t, "root_uuid")
		entry := GenerateBootEntry(rootUUID, kp)
		if again := GenerateBootEntry(rootUUID, kp); again != entry {
			t.Fatalf("GenerateBootEntry is not deterministic")
		}
		keys, options := blsParse(entry)
		if got, want := strings.Join(keys, ","), "title,linux,initrd,options"; got != want {
			t.Fatalf("entry keys %q, want %q (kernel params %q added or removed a line):\n%s", got, want, kp, entry)
		}
		want := append([]string{"root=UUID=" + rootUUID, "ro"}, wantKernelArgs(kp)...)
		if got := asciiFields(options); strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
			t.Fatalf("options %q, want %q (kernel params %q)", got, want, kp)
		}
		switch {
		case kp == "none":
			none++
		case documented:
			benign++
		default:
			hostile++
		}
	})
	t.Logf("coverage: benign=%d hostile=%d none=%d refused=%d", benign, hostile, none, refused)
}

// sourceGrubDefault reads GRUB_CMDLINE_LINUX the way grub-mkconfig does.
// It runs in dir so a command substitution that fires leaves a witness.
func sourceGrubDefault(dir, path string) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", `. "$1"; printf '%s' "$GRUB_CMDLINE_LINUX"`, "sh", path)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/expanded-home"}
	out, err := cmd.Output()
	return string(out), err
}

var grubSkeletons = []string{
	"GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX=\"\"\nGRUB_TIMEOUT=5\n",
	"GRUB_DEFAULT=0\nGRUB_TIMEOUT=5\n",
	"GRUB_DEFAULT=0\nGRUB_CMDLINE_LINUX_DEFAULT=\"quiet splash\"\nGRUB_CMDLINE_LINUX=\"old=1\"\n",
	"# GRUB_CMDLINE_LINUX=\"commented\"\nGRUB_CMDLINE_LINUX=\"\"\n",
	"GRUB_DEFAULT=0\n  GRUB_CMDLINE_LINUX=\"indented\"\nGRUB_TIMEOUT=5",
	"",
}

// TestProp_SetGrubDefaultCmdline_ShellRoundTrip: after SetGrubDefaultCmdline
// the file, sourced by sh, yields exactly the cmdline that was set; the call
// is idempotent; every unrelated line is preserved; nothing is executed.
func TestProp_SetGrubDefaultCmdline_ShellRoundTrip(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	t.Run("documented_values", func(t *testing.T) { checkGrubRoundTrip(t, false) })
	t.Run("any_value", func(t *testing.T) { checkGrubRoundTrip(t, true) })
}

func checkGrubRoundTrip(t *testing.T, allowHostile bool) {
	src := newKernelParamsSource(t)
	var benign, hostile, missing, refused int
	rapid.Check(t, func(t *rapid.T) {
		served, documented := genKP(t, allowHostile)
		kp, ok := src.fetch(t, served)
		if !ok {
			refused++
			return
		}
		dir, err := os.MkdirTemp("", "grubprop")
		if err != nil {
			t.Fatalf("mkdtemp: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		path := filepath.Join(dir, "grub")
		// Mirror InstallGRUBBIOS: "none" means an empty cmdline.
		cmdline := ""
		if kp != "none" {
			cmdline = "console=tty0 " + kp
		}
		exists := rapid.IntRange(0, 5).Draw(t, "file_exists") > 0
		skeleton := rapid.SampledFrom(grubSkeletons).Draw(t, "skeleton")
		if exists {
			if err := os.WriteFile(path, []byte(skeleton), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		} else {
			missing++
		}

		if err := SetGrubDefaultCmdline(path, cmdline); err != nil {
			t.Fatalf("SetGrubDefaultCmdline: %v", err)
		}
		first, _ := os.ReadFile(path)
		if err := SetGrubDefaultCmdline(path, cmdline); err != nil {
			t.Fatalf("SetGrubDefaultCmdline (second): %v", err)
		}
		second, _ := os.ReadFile(path)
		if string(first) != string(second) {
			t.Fatalf("not idempotent:\nfirst  %q\nsecond %q", first, second)
		}

		got, err := sourceGrubDefault(dir, path)
		if err != nil {
			t.Fatalf("sourcing the generated /etc/default/grub failed: %v\nfile: %q", err, second)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "PWNED")); statErr == nil {
			t.Fatalf("sourcing the generated /etc/default/grub ran a command from the kernel params %q\nfile: %q", kp, second)
		}
		if got != cmdline {
			t.Fatalf("grub would boot with GRUB_CMDLINE_LINUX=%q, want %q\nfile: %q", got, cmdline, second)
		}
		// Unrelated lines survive, in order.
		if exists {
			var want []string
			for _, l := range strings.Split(skeleton, "\n") {
				if !strings.HasPrefix(strings.TrimSpace(l), "GRUB_CMDLINE_LINUX=") && l != "" {
					want = append(want, l)
				}
			}
			var have []string
			for _, l := range strings.Split(string(second), "\n") {
				if !strings.HasPrefix(l, "GRUB_CMDLINE_LINUX=") && l != "" {
					have = append(have, l)
				}
			}
			if strings.Join(have, "\n") != strings.Join(want, "\n") {
				t.Fatalf("unrelated lines changed:\n have %q\n want %q", have, want)
			}
		}
		if documented {
			benign++
		} else {
			hostile++
		}
	})
	t.Logf("coverage: benign=%d hostile=%d file_missing=%d refused=%d", benign, hostile, missing, refused)
}

// TestProp_SplitDiskPart_RoundTrip: SplitDiskPart inverts the kernel's
// partition naming (p separator after a digit-terminated disk name).
func TestProp_SplitDiskPart_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		disk := rapid.SampledFrom([]string{
			"/dev/sda", "/dev/sdz", "/dev/sdaa", "/dev/vda", "/dev/hdb", "/dev/xvda",
			"/dev/nvme0n1", "/dev/nvme12n3", "/dev/mmcblk0", "/dev/loop7", "/dev/md0", "/dev/md127", "/dev/dm-3",
		}).Draw(t, "disk")
		part := rapid.IntRange(1, 128).Draw(t, "part")
		name := disk
		if last := disk[len(disk)-1]; last >= '0' && last <= '9' {
			name += "p"
		}
		name += strconv.Itoa(part)
		gotDisk, gotPart, err := SplitDiskPart(name)
		if err != nil {
			t.Fatalf("SplitDiskPart(%q): %v", name, err)
		}
		if gotDisk != disk || gotPart != part {
			t.Fatalf("SplitDiskPart(%q) = (%q, %d), want (%q, %d)", name, gotDisk, gotPart, disk, part)
		}
		// A whole disk is never mistaken for a partition of another disk.
		if d, p, err := SplitDiskPart(disk); err == nil {
			t.Fatalf("SplitDiskPart(%q) accepted a whole disk as (%q, %d)", disk, d, p)
		}
	})
}
