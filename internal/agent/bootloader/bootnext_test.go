package bootloader

import (
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// efiRunner is an agent.Runner that plays efibootmgr. It keeps a small model
// of the firmware's boot variables: `efibootmgr` lists them, `-c` creates the
// Slagworks entry, `-b N -B` deletes one, `-n N` sets BootNext. Everything
// it is asked to run is recorded.
type efiRunner struct {
	runs     []string
	entries  []string // "0003 Slagworks", "0001 UEFI PXE", ...
	bootNext string
}

func (r *efiRunner) Run(name string, args ...string) error {
	argv := strings.Join(append([]string{name}, args...), " ")
	r.runs = append(r.runs, argv)
	if name != "efibootmgr" {
		return nil
	}
	switch {
	case slices.Contains(args, "-c"):
		r.entries = append(r.entries, "0003 Slagworks")
	case slices.Contains(args, "-B"):
		id := args[slices.Index(args, "-b")+1]
		r.entries = slices.DeleteFunc(r.entries, func(e string) bool { return strings.HasPrefix(e, id+" ") })
	case slices.Contains(args, "-n"):
		r.bootNext = args[slices.Index(args, "-n")+1]
	}
	return nil
}

func (r *efiRunner) Output(name string, args ...string) (string, error) {
	r.runs = append(r.runs, strings.Join(append([]string{name}, args...), " "))
	if name != "efibootmgr" {
		return "", nil
	}
	var b strings.Builder
	if r.bootNext != "" {
		b.WriteString("BootNext: " + r.bootNext + "\n")
	}
	b.WriteString("BootCurrent: 0001\nTimeout: 1 seconds\nBootOrder: 0001\n")
	for _, e := range r.entries {
		id, label, _ := strings.Cut(e, " ")
		b.WriteString("Boot" + id + "* " + label + "\tHD(1,GPT,0b3c8f6e)/File(\\EFI\\BOOT\\BOOTX64.EFI)\n")
	}
	return b.String(), nil
}

func (r *efiRunner) Available(string) bool { return true }

// efibootmgrRuns returns the efibootmgr invocations, dropping the efivarfs
// probe's commands, which depend on the machine the test runs on.
func (r *efiRunner) efibootmgrRuns() []string {
	var out []string
	for _, run := range r.runs {
		if strings.HasPrefix(run, "efibootmgr") {
			out = append(out, run)
		}
	}
	return out
}

func testContext(r agent.Runner) *agent.Context {
	return &agent.Context{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Exec: r}
}

// TestRegisterEFIBootEntry_DoesNotArmBootNext is the regression for a
// real-hardware property-test history. A deploy was aborted in its
// finalizing phase, after the bootloader step, and the operator started a
// fresh deploy. The site worker armed the BMC's one-shot PXE override and
// power cycled the host, and the host booted the aborted install off its
// disk instead: the bootloader step had already set BootNext to the
// installed OS, and the firmware honors BootNext ahead of the BMC override.
// The new deploy waited thirty minutes for an agent that never booted.
//
// Registering the boot entry is part of installing the bootloader. It must
// create the entry and leave the next boot alone.
func TestRegisterEFIBootEntry_DoesNotArmBootNext(t *testing.T) {
	// A stale entry from the previous deploy, as on any redeploy.
	r := &efiRunner{entries: []string{"0001 UEFI-PXE", "0000 Slagworks"}}
	if err := registerEFIBootEntry(testContext(r), "/dev/nvme0n1p1"); err != nil {
		t.Fatalf("registerEFIBootEntry: %v", err)
	}

	want := []string{
		"efibootmgr",
		"efibootmgr -b 0000 -B",
		`efibootmgr -c -d /dev/nvme0n1 -p 1 -L Slagworks -l \EFI\BOOT\BOOTX64.EFI`,
	}
	if got := r.efibootmgrRuns(); !slices.Equal(got, want) {
		t.Errorf("efibootmgr invocations:\n got %q\nwant %q", got, want)
	}
	if r.bootNext != "" {
		t.Errorf("registering the boot entry set BootNext to %s: a deploy that ends before deploy_complete "+
			"would leave the host's next boot committed to the unfinished install", r.bootNext)
	}
	if !slices.Contains(r.entries, "0003 Slagworks") || slices.Contains(r.entries, "0000 Slagworks") {
		t.Errorf("boot entries after registration = %v, want the stale Slagworks entry replaced", r.entries)
	}
}

// TestSetBootNextToInstalledOS pins the other half: once the deploy is
// sanctioned, BootNext goes to the registered entry.
func TestSetBootNextToInstalledOS(t *testing.T) {
	t.Run("one Slagworks entry", func(t *testing.T) {
		r := &efiRunner{entries: []string{"0001 UEFI-PXE", "0003 Slagworks"}}
		setBootNextToInstalledOS(testContext(r))
		if want := []string{"efibootmgr", "efibootmgr -n 0003"}; !slices.Equal(r.efibootmgrRuns(), want) {
			t.Errorf("efibootmgr invocations = %q, want %q", r.efibootmgrRuns(), want)
		}
		if r.bootNext != "0003" {
			t.Errorf("BootNext = %q, want 0003", r.bootNext)
		}
	})

	t.Run("no Slagworks entry leaves BootNext alone", func(t *testing.T) {
		r := &efiRunner{entries: []string{"0001 UEFI-PXE"}}
		setBootNextToInstalledOS(testContext(r))
		if r.bootNext != "" {
			t.Errorf("BootNext = %q with no Slagworks entry to point at, want it unset", r.bootNext)
		}
	})

	t.Run("ambiguous entries leave BootNext alone", func(t *testing.T) {
		r := &efiRunner{entries: []string{"0003 Slagworks", "0004 Slagworks"}}
		setBootNextToInstalledOS(testContext(r))
		if r.bootNext != "" {
			t.Errorf("BootNext = %q with two Slagworks entries, want it unset rather than guessed", r.bootNext)
		}
	})
}
