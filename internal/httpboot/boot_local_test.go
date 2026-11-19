package httpboot

import (
	"fmt"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A machine's ESP can sit on any disk a storage script picks, and every boot
// after the deploy's one-shot BootNext goes PXE -> boot-local. The UEFI
// boot-local script therefore has to walk the drives until one boots, and
// when none does it has to end with status 0: embed.ipxe treats a nonzero
// status as a failed chain, retries the whole PXE cycle 20 times and drops to
// an iPXE shell, while status 0 lets iPXE exit to the firmware, as the BIOS
// script does.

// bootLocalLastDrive is the last drive the UEFI boot-local script tries; iPXE
// numbers local drives from 0x80.
const bootLocalLastDrive = 0x9f

func TestBootLocal_UEFI_WalksDrivesToTheBootDisk(t *testing.T) {
	script := serveBootLocal(t, "efi")
	for _, bootDrive := range []uint64{0x80, 0x81, 0x82, 0x83, bootLocalLastDrive} {
		t.Run(fmt.Sprintf("%#x", bootDrive), func(t *testing.T) {
			run := runBootLocalScript(t, script, bootDrive)
			if !run.booted {
				t.Fatalf("the ESP is on drive %#x but the script tried only %s and then ended with status %d; script:\n%s",
					bootDrive, hexDrives(run.tried), run.status, script)
			}
			if want := driveRange(0x80, bootDrive); !slices.Equal(run.tried, want) {
				t.Fatalf("tried drives %s, want %s in order", hexDrives(run.tried), hexDrives(want))
			}
		})
	}
}

func TestBootLocal_UEFI_NoBootableDrive_ReturnsToFirmware(t *testing.T) {
	script := serveBootLocal(t, "efi")
	run := runBootLocalScript(t, script, 0)
	if run.booted {
		t.Fatalf("no drive is bootable, but the model booted one; tried %s", hexDrives(run.tried))
	}
	if run.status != 0 {
		t.Fatalf("with no bootable drive the script ends with status %d after trying %s; embed.ipxe treats that "+
			"as a failed chain, retries 20 times and drops to an iPXE shell instead of exiting to the firmware. "+
			"Script:\n%s", run.status, hexDrives(run.tried), script)
	}
	if want := driveRange(0x80, bootLocalLastDrive); !slices.Equal(run.tried, want) {
		t.Fatalf("tried drives %s, want every drive %s in order", hexDrives(run.tried), hexDrives(want))
	}
}

func serveBootLocal(t *testing.T, platform string) string {
	t.Helper()
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC+"&platform="+platform, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Body.String()
}

// bootLocalRun is how a boot-local script ends under runBootLocalScript.
type bootLocalRun struct {
	tried  []uint64 // drives sanboot attempted, in order
	booted bool     // a sanboot reached the bootable drive (it never returns)
	status int      // the script's exit status when nothing booted
}

// runBootLocalScript interprets a boot-local script against a firmware whose
// only bootable drive is bootDrive (0 for none). It models the iPXE semantics
// the script relies on (src/core/exec.c, src/image/script.c): a line runs
// commands joined by || and &&, an empty command succeeds, the line's status
// is that of the last command that ran, a failing line ends the script with
// that failure, `exit [n]` ends it with status n, `goto` resumes after a
// label, and a :uint8 setting expands as %#x. Anything outside that subset
// fails the test, so a script the model does not understand cannot pass by
// accident.
func runBootLocalScript(t *testing.T, script string, bootDrive uint64) bootLocalRun {
	t.Helper()
	lines := strings.Split(script, "\n")
	if lines[0] != "#!ipxe" {
		t.Fatalf("not an iPXE script:\n%s", script)
	}
	labels := map[string]int{}
	for i, line := range lines {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, ":") {
			labels[l[1:]] = i
		}
	}
	settings := map[string]uint64{}
	var run bootLocalRun
	for pc, steps := 1, 0; pc < len(lines); pc++ {
		if steps++; steps > 10000 {
			t.Fatalf("script does not terminate:\n%s", script)
		}
		line := lines[pc]
		tokens := strings.Fields(line)
		if len(tokens) == 0 || strings.HasPrefix(tokens[0], ":") || strings.HasPrefix(tokens[0], "#") {
			continue
		}
		status, process, jumped := 0, true, false
		for {
			end := slices.IndexFunc(tokens, func(s string) bool { return s == "||" || s == "&&" })
			if end < 0 {
				end = len(tokens)
			}
			argv := slices.Clone(tokens[:end])
			if process {
				for i, arg := range argv {
					argv[i] = expandSetting(t, arg, settings)
				}
			}
			switch {
			case !process:
			case len(argv) == 0:
				status = 0
			case argv[0] == "echo":
				status = 0
			case argv[0] == "set":
				if len(argv) != 3 || !strings.HasSuffix(argv[1], ":uint8") {
					t.Fatalf("model handles only `set <name>:uint8 <value>`, got %q", line)
				}
				v, err := strconv.ParseUint(argv[2], 0, 8)
				if err != nil {
					t.Fatalf("set value %q is not a uint8: %q", argv[2], line)
				}
				settings[strings.TrimSuffix(argv[1], ":uint8")], status = v, 0
			case argv[0] == "inc":
				if _, ok := settings[argv[len(argv)-1]]; !ok || len(argv) != 2 {
					t.Fatalf("model handles only `inc <setting>` on a set setting, got %q", line)
				}
				settings[argv[1]] = (settings[argv[1]] + 1) & 0xff
				status = 0
			case argv[0] == "iseq" && len(argv) == 3:
				status = 1
				if argv[1] == argv[2] {
					status = 0
				}
			case argv[0] == "goto" && len(argv) == 2:
				target, ok := labels[argv[1]]
				if !ok {
					t.Fatalf("goto to a missing label: %q", line)
				}
				pc, jumped, status = target, true, 0
			case argv[0] == "sanboot":
				if len(argv) != 4 || !slices.Equal(argv[:3], []string{"sanboot", "--no-describe", "--drive"}) {
					t.Fatalf("model handles only `sanboot --no-describe --drive <n>`, got %q", line)
				}
				drive, err := strconv.ParseUint(argv[3], 0, 32)
				if err != nil {
					t.Fatalf("sanboot drive %q is not a number iPXE's strtoul accepts: %q", argv[3], line)
				}
				run.tried = append(run.tried, drive)
				if drive == bootDrive {
					run.booted = true
					return run
				}
				status = 1
			case argv[0] == "exit" && len(argv) <= 2:
				run.status = 0
				if len(argv) == 2 {
					n, err := strconv.Atoi(argv[1])
					if err != nil {
						t.Fatalf("exit status %q is not a number: %q", argv[1], line)
					}
					run.status = n
				}
				return run
			default:
				t.Fatalf("model does not handle %q in %q", strings.Join(argv, " "), line)
			}
			if jumped || end == len(tokens) {
				break
			}
			process = (tokens[end] == "||") == (status != 0)
			tokens = tokens[end+1:]
		}
		if status != 0 {
			run.status = status
			return run
		}
	}
	return run
}

// expandSetting expands a whole-token ${name} reference; the boot-local
// script uses no other form of expansion.
func expandSetting(t *testing.T, arg string, settings map[string]uint64) string {
	t.Helper()
	if !strings.Contains(arg, "${") {
		return arg
	}
	name, prefixed := strings.CutPrefix(arg, "${")
	name, suffixed := strings.CutSuffix(name, "}")
	v, set := settings[name]
	if !prefixed || !suffixed || !set {
		t.Fatalf("model expands only whole-token references to set settings, got %q", arg)
	}
	return fmt.Sprintf("%#x", v)
}

func driveRange(first, last uint64) []uint64 {
	var drives []uint64
	for d := first; d <= last; d++ {
		drives = append(drives, d)
	}
	return drives
}

func hexDrives(drives []uint64) string {
	s := make([]string, len(drives))
	for i, d := range drives {
		s[i] = fmt.Sprintf("%#x", d)
	}
	return "[" + strings.Join(s, " ") + "]"
}
