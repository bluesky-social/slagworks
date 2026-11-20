//go:build integration

package qemutest_test

// Storage property suite, part 2: the driver that plays a case on real VMs.
//
// Every step gets a fresh qcow2 overlay on top of each disk's previous state,
// so after the step `qemu-img map` says exactly which byte ranges the step
// wrote. That is the "untouched disk" oracle: a disk the step must not write
// has an empty overlay.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

const (
	propDeployBudget = 6 * time.Minute
	propBootBudget   = 5 * time.Minute
)

type propRun struct {
	t       *testing.T
	c       propCase
	env     *qemutest.Env
	network *qemutest.Network
	images  *qemutest.ImageDir
	sw      *qemutest.SiteWorker
	slug    string
	seq     int64
	signer  ssh.Signer
	authKey string
	dir     string
	cur     map[string]string // disk ID -> current top image
	// bootTop is, for the step in progress, each disk's second overlay:
	// the one the installed system writes to. See splitPhases.
	bootTop map[string]string
	// canary remembers, per mounted data array, the token written into it
	// and the step that wrote it.
	canary map[string]string
	fails  []string
	notes  []string
}

func (r *propRun) failf(step int, format string, a ...any) {
	msg := fmt.Sprintf("step %d: ", step) + fmt.Sprintf(format, a...)
	r.fails = append(r.fails, msg)
	r.t.Errorf("PROPFAIL %s %s", r.c.Name, msg)
}

func (r *propRun) notef(step int, format string, a ...any) {
	msg := fmt.Sprintf("step %d: ", step) + fmt.Sprintf(format, a...)
	r.notes = append(r.notes, msg)
	r.t.Logf("PROPNOTE %s %s", r.c.Name, msg)
}

func propQemuImg(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("qemu-img", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-img %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// propSeedDisk creates a disk's base image with its prior content. Content is
// laid down in a sparse raw file with ordinary file tools and converted, so
// seeding needs no loop or NBD device on the host.
func propSeedDisk(t *testing.T, d propDisk, path string) {
	t.Helper()
	if d.Seed == "" {
		propQemuImg(t, "create", "-f", "qcow2", path, fmt.Sprint(d.Bytes))
		return
	}
	raw := path + ".raw"
	f, err := os.Create(raw)
	if err != nil {
		t.Fatalf("create %s: %v", raw, err)
	}
	if err := f.Truncate(d.Bytes); err != nil {
		t.Fatalf("truncate %s: %v", raw, err)
	}
	writeAt := func(b []byte, off int64) {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatalf("seed %s: write at %d: %v", d.Seed, off, err)
		}
	}
	mkfsExt4 := func() {
		if out, err := exec.Command("mkfs.ext4", "-q", "-F", raw).CombinedOutput(); err != nil {
			t.Fatalf("seed %s: mkfs.ext4: %v\n%s", d.Seed, err, out)
		}
	}
	switch d.Seed {
	case "ext4":
		mkfsExt4()
	case "ambiv":
		// A disk that held ext4 and was then given to ZFS without a wipe.
		// `zpool create` leaves the first 8 KiB of each label alone, so the
		// ext4 superblock at 1024 survives next to the ZFS uberblocks, and
		// blkid finds two signatures it refuses to choose between.
		mkfsExt4()
		ub := make([]byte, 8)
		binary.LittleEndian.PutUint64(ub, 0x00bab10c)
		for label := int64(0); label < 2; label++ {
			for i := int64(0); i < 16; i++ {
				writeAt(ub, label*256*1024+128*1024+i*1024)
			}
		}
	case "luks":
		hdr := append([]byte{'L', 'U', 'K', 'S', 0xba, 0xbe}, 0x00, 0x01)
		writeAt(hdr, 0)
	case "dos":
		mbr := make([]byte, 512)
		// One Linux partition from sector 2048 covering most of the disk.
		e := mbr[446:462]
		e[4] = 0x83
		binary.LittleEndian.PutUint32(e[8:], 2048)
		binary.LittleEndian.PutUint32(e[12:], uint32(d.Bytes/512-4096))
		mbr[510], mbr[511] = 0x55, 0xaa
		writeAt(mbr, 0)
	default:
		t.Fatalf("prop: unknown seed %q", d.Seed)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", raw, err)
	}
	propQemuImg(t, "convert", "-f", "raw", "-O", "qcow2", raw, path)
	_ = os.Remove(raw)
}

// propWritten returns the guest-visible byte ranges present in the top layer
// of an image, that is the ranges written since the overlay was created.
func propWritten(t *testing.T, path string) [][2]int64 {
	t.Helper()
	out := propQemuImg(t, "map", "--output=json", path)
	var ext []struct {
		Start   int64 `json:"start"`
		Length  int64 `json:"length"`
		Depth   int   `json:"depth"`
		Present *bool `json:"present"`
		Data    bool  `json:"data"`
		Zero    bool  `json:"zero"`
	}
	if err := json.Unmarshal([]byte(out), &ext); err != nil {
		t.Fatalf("parse qemu-img map for %s: %v\n%s", path, err, out)
	}
	var ranges [][2]int64
	for _, e := range ext {
		if e.Depth != 0 {
			continue
		}
		if e.Present != nil && !*e.Present {
			continue
		}
		if e.Present == nil && !e.Data {
			continue
		}
		if n := len(ranges); n > 0 && ranges[n-1][0]+ranges[n-1][1] == e.Start {
			ranges[n-1][1] += e.Length
			continue
		}
		ranges = append(ranges, [2]int64{e.Start, e.Length})
	}
	return ranges
}

func propRangesString(r [][2]int64) string {
	var parts []string
	for i, x := range r {
		if i == 12 {
			parts = append(parts, fmt.Sprintf("... %d more", len(r)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("[%d +%d]", x[0], x[1]))
	}
	return strings.Join(parts, " ")
}

// propDiskArgs builds the QEMU arguments for the disks of one step. The disks
// get bootindex 2.. in Attach order, after the NIC.
func propDiskArgs(c propCase, st propStep, images map[string]string) []string {
	var args []string
	scsi := false
	// q35's AHCI controller has six one-unit buses and QEMU's default
	// CD-ROM sits on ide.2, so SATA disks are placed explicitly.
	sataBus := []int{0, 1, 3, 4, 5}
	for i, id := range st.Attach {
		d := c.disk(id)
		drive := fmt.Sprintf("file=%s,format=qcow2,if=none,id=prop%s", images[id], id)
		if d.Discard {
			drive += ",discard=unmap"
		}
		tail := fmt.Sprintf(",drive=prop%s,serial=PROP-%s,bootindex=%d", id, id, i+2)
		if d.LBS == 4096 {
			tail += ",logical_block_size=4096,physical_block_size=4096"
		}
		var dev string
		switch d.Bus {
		case "virtio":
			dev = "virtio-blk-pci" + tail
		case "nvme":
			dev = "nvme" + tail
		case "sata":
			if len(sataBus) == 0 {
				panic("prop: more than five SATA disks")
			}
			dev = fmt.Sprintf("ide-hd,bus=ide.%d", sataBus[0]) + tail
			sataBus = sataBus[1:]
		case "scsi":
			if !scsi {
				args = append(args, "-device", "virtio-scsi-pci,id=propscsi")
				scsi = true
			}
			dev = "scsi-hd,bus=propscsi.0" + tail
		default:
			panic("prop: unknown bus " + d.Bus)
		}
		args = append(args, "-drive", drive, "-device", dev)
	}
	return args
}

var propFatalRE = regexp.MustCompile(`"msg":"FATAL during ([^"]+)"`)

// propWaitOutcome watches the serial console until the deploy reaches a
// terminal state. It returns "complete", "fatal:<stage>", "fatal_at",
// "killed", "agent_exit" (the agent died without a FATAL line), or "timeout".
func (r *propRun) propWaitOutcome(ctx context.Context, vm *qemutest.VM, st propStep) string {
	deadline := time.Now().Add(propDeployBudget)
	cursor := 0
	killCmd := ""
	for time.Now().Before(deadline) && ctx.Err() == nil {
		lines := vm.Serial.Lines()
		for ; cursor < len(lines); cursor++ {
			l := lines[cursor]
			if st.KillAtCmd > 0 && killCmd == "" {
				if _, cmd, ok := strings.Cut(l, fmt.Sprintf(`"msg":"layout:   [%d] `, st.KillAtCmd)); ok {
					killCmd, _, _ = strings.Cut(cmd, `"`)
				}
			}
			if killCmd != "" && strings.Contains(l, `"msg":"exec"`) && strings.Contains(l, `"argv":"`+killCmd+`"`) {
				r.t.Logf("PROPKILL %s after %q", r.c.Name, killCmd)
				_ = vm.QMP.Quit()
				return "killed"
			}
			if m := propFatalRE.FindStringSubmatch(l); m != nil {
				return "fatal:" + m[1]
			}
			switch {
			case strings.Contains(l, `"msg":"deploy complete"`):
				return "complete"
			case strings.Contains(l, "SLAGWORKS_FATAL_AT=") && strings.Contains(l, "honored"):
				return "fatal_at"
			case strings.Contains(l, "RECOVERY MODE"), strings.Contains(l, "slagworks: FATAL:"):
				return "agent_exit"
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "timeout"
}

// propWaitExit waits for the QEMU process to go away after a quit or a guest
// poweroff, so its images are flushed and unlocked.
func propWaitExit(ctx context.Context, vm *qemutest.VM, d time.Duration) bool {
	err := vm.Serial.WaitFor(ctx, "\x00prop-never\x00", d)
	return err != nil && strings.Contains(err.Error(), "stream ended")
}

func propSerialTail(vm *qemutest.VM, n int) string {
	lines := vm.Serial.Lines()
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (r *propRun) sshRun(cli *ssh.Client, script string) (string, error) {
	sess, err := cli.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	out, err := sess.CombinedOutput(script)
	return string(out), err
}

func (r *propRun) leaseIP(mac string) string {
	for _, l := range r.sw.LocalDB.LoadDHCPLeases() {
		if strings.EqualFold(l.MAC.String(), mac) {
			return l.IP.String()
		}
	}
	return ""
}

func (r *propRun) push(machine qemutest.Machine, op model.Operation) {
	r.seq++
	snap := r.env.NewSnapshot(r.slug, r.network).Seq(r.seq).AddMachine(machine).AddOperation(op).Build()
	snap.SSHKeys = []string{r.authKey}
	r.env.Hub.Push(r.slug, snap)
}

func propExpects(expect, got string) bool {
	for _, e := range strings.Split(expect, "|") {
		if e == got {
			return true
		}
	}
	return false
}

// runStep plays one step and returns the outcome it observed.
func (r *propRun) runStep(ctx context.Context, i int, st propStep) string {
	t := r.t
	name := fmt.Sprintf("%s-s%d", r.c.Name, i)
	systemUUID, mac := matrixIdentity("prop-" + name)
	machineID := uuid.New()

	op := qemutest.NewOperation(machineID, model.OpDeploy)
	op.Params = mustMarshalParams(t, model.OpParams{
		Destructive:  st.Destructive,
		WipeAllDisks: st.WipeAll,
		FatalAt:      st.FatalAt,
	})
	machine := qemutest.Machine{
		ID:            machineID,
		MAC:           mac,
		State:         model.MachineStateDeploying,
		PXEEligible:   true,
		SystemUUID:    systemUUID,
		StorageScript: &model.SnapshotStorageScript{Name: "prop", Content: propRenderScript(r.c, st)},
	}
	r.push(machine, op)
	if r.sw == nil {
		r.sw = r.env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
			Slug:         r.slug,
			Network:      r.network,
			Images:       r.images,
			DefaultImage: "noble-pxe",
		})
	}
	waitBootDeployMAC(ctx, t, r.sw, mac)

	// A fresh overlay per attached disk: what this step writes lands there.
	r.bootTop = map[string]string{}
	images := map[string]string{}
	for _, id := range st.Attach {
		top := filepath.Join(r.dir, fmt.Sprintf("%s.s%d.qcow2", id, i))
		propQemuImg(t, "create", "-f", "qcow2", "-F", "qcow2", "-b", r.cur[id], top)
		r.cur[id] = top
		images[id] = top
	}

	fw := qemutest.UEFI
	if r.c.Firmware == "bios" {
		fw = qemutest.BIOS
	}
	vm := r.env.NewVM(t, qemutest.VMConfig{
		Firmware:      fw,
		MAC:           mac,
		Network:       r.network,
		Memory:        "4096",
		SMBIOSUUID:    systemUUID,
		NoPrimaryDisk: true,
		// The NIC boots ahead of disks that already carry a bootloader.
		NetworkBootFirst: true,
		ExtraArgs:        propDiskArgs(r.c, st, images),
	})
	started := time.Now()
	vm.Start(ctx)
	defer vm.Stop()

	got := r.propWaitOutcome(ctx, vm, st)
	t.Logf("PROPSTEP %s step=%d deploy outcome=%s after %s", r.c.Name, i, got, time.Since(started).Round(time.Second))

	if got == "complete" {
		got = r.afterComplete(ctx, i, st, vm, machine, op)
	} else if got != "killed" {
		// The agent stopped. Give the guest kernel a moment to write back
		// anything it still holds, then power off through QMP so QEMU
		// flushes the images.
		if got == "timeout" || got == "agent_exit" {
			t.Logf("PROPSERIAL %s step=%d tail:\n%s", r.c.Name, i, propSerialTail(vm, 60))
		}
		time.Sleep(3 * time.Second)
		_ = vm.QMP.Quit()
	}
	if !propWaitExit(ctx, vm, 90*time.Second) {
		r.notef(i, "QEMU did not exit within 90s of the step ending; killing it")
	}
	vm.Stop()

	// What did the step write, disk by disk? When the deploy completed, the
	// overlay was split at the reboot: images[id] holds what the agent
	// wrote, r.bootTop[id] what the installed system wrote afterwards.
	agentW := map[string][][2]int64{}
	bootW := map[string][][2]int64{}
	for _, id := range st.Attach {
		agentW[id] = propWritten(t, images[id])
		line := fmt.Sprintf("PROPWRITES %s step=%d disk=%s agent=%d %s", r.c.Name, i, id, len(agentW[id]), propRangesString(agentW[id]))
		if top, ok := r.bootTop[id]; ok {
			bootW[id] = propWritten(t, top)
			line += fmt.Sprintf(" | installed-system=%d %s", len(bootW[id]), propRangesString(bootW[id]))
		}
		t.Log(line)
	}
	// Without a split, a step that booted has both writers in one overlay.
	agentOnly := len(r.bootTop) == len(st.Attach) || (got != "ok" && got != "noboot")
	r.checkWrites(i, st, got, agentW, bootW, agentOnly)

	// A re-created array that never reached its first boot holds no canary
	// any more; forget the old one so a later step does not expect it.
	if got != "ok" && !propStoppedEarly(st, got) {
		for _, a := range st.Layout.Arrays {
			if a.Type == "raid" {
				delete(r.canary, a.Name)
			}
		}
	}

	if !propExpects(st.Expect, got) {
		r.failf(i, "outcome %q, expected %q (%s)", got, st.Expect, st.Why)
	}
	return got
}

// propStoppedEarly reports whether the deploy ended before the agent entered
// its partitioning work, where nothing may have been written yet.
func propStoppedEarly(st propStep, got string) bool {
	switch got {
	case "fatal:safety check", "fatal:md name check", "fatal:stale array member check", "fatal:preserve preflight",
		"fatal:planning storage layout", "fatal:resolving storage layout", "fatal:identity check", "fatal:flags":
		return true
	}
	return got == "fatal_at" && (st.FatalAt == "partitioning" || st.FatalAt == "agent_starting")
}

// checkWrites applies the no-write rules: a deploy refused before the
// partitioning phase writes nothing at all, and the agent never writes a disk
// the layout does not claim, or retains, unless --wipe-all-disks asked for
// it. agentOnly says agentW holds the agent's writes alone; otherwise the
// installed system's writes are mixed in and a hit needs a human.
func (r *propRun) checkWrites(i int, st propStep, got string, agentW, bootW map[string][][2]int64, agentOnly bool) {
	if propStoppedEarly(st, got) {
		for _, id := range st.Attach {
			if len(agentW[id]) != 0 {
				r.failf(i, "deploy stopped at %q before partitioning, yet disk %s was written: %s",
					got, id, propRangesString(agentW[id]))
			}
		}
		return
	}
	role := map[string]string{}
	for _, dl := range st.Layout.Disks {
		role[dl.Disk] = "disk"
	}
	for _, a := range st.Layout.Arrays {
		for _, m := range a.Members {
			if role[m.Disk] == "" {
				role[m.Disk] = a.Type
			}
		}
	}
	for _, id := range st.Layout.Retain {
		role[id] = "retain"
	}
	for _, id := range st.Attach {
		what := ""
		switch {
		case role[id] == "retain":
			what = "retained disk"
		case role[id] == "" && !st.WipeAll:
			what = "disk outside the layout (no --wipe-all-disks)"
		default:
			continue
		}
		if len(agentW[id]) != 0 {
			if agentOnly {
				r.failf(i, "the deploy agent wrote %s %s: %s", what, id, propRangesString(agentW[id]))
			} else {
				r.notef(i, "%s %s was written during the step (agent or installed system): %s", what, id, propRangesString(agentW[id]))
			}
		}
		if len(bootW[id]) != 0 {
			// The installed system assembles whatever arrays it finds and
			// updates their superblocks. Not the agent's doing, but worth
			// seeing.
			r.notef(i, "the installed system wrote %s %s: %s", what, id, propRangesString(bootW[id]))
		}
	}
}

// splitPhases freezes each disk's overlay at the moment the deploy agent has
// exited and the machine is about to reboot, and puts a second overlay on
// top for the installed system. It needs the VM paused only for the
// snapshots themselves.
func (r *propRun) splitPhases(ctx context.Context, i int, st propStep, vm *qemutest.VM) {
	if err := vm.Serial.WaitFor(ctx, "slagworks: rebooting", 30*time.Second); err != nil {
		r.failf(i, "no reboot marker after deploy complete, agent and installed-system writes cannot be separated: %v", err)
		return
	}
	if err := vm.QMP.Stop(); err != nil {
		r.failf(i, "could not pause the VM to separate writes: %v", err)
		return
	}
	for _, id := range st.Attach {
		top := filepath.Join(r.dir, fmt.Sprintf("%s.s%db.qcow2", id, i))
		err := vm.QMP.Execute("blockdev-snapshot-sync", map[string]any{
			"device": "prop" + id, "snapshot-file": top, "format": "qcow2",
		})
		if err != nil {
			// Without the split the agent's writes cannot be told from the
			// installed system's, and the no-write rules would go unchecked.
			r.failf(i, "could not snapshot disk %s at the reboot: %v", id, err)
			continue
		}
		r.bootTop[id] = top
		r.cur[id] = top
	}
	if err := vm.QMP.Cont(); err != nil {
		r.failf(i, "could not resume the VM after the snapshots: %v", err)
	}
}

// afterComplete drives a completed deploy through its first boot, inspects
// the installed system and shuts it down cleanly. It returns "ok" or a
// failure outcome.
func (r *propRun) afterComplete(ctx context.Context, i int, st propStep, vm *qemutest.VM, machine qemutest.Machine, op model.Operation) string {
	t := r.t
	awaiting := op
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	r.push(machine, awaiting)
	r.splitPhases(ctx, i, st, vm)

	if _, err := r.env.Hub.WaitForObservation(ctx, r.slug, "deploy_complete", machine.ID); err != nil {
		r.failf(i, "no deploy_complete observation: %v", err)
	}
	// Wait for the phone-home, but stop at once when the console shows the
	// machine has nowhere left to go.
	phoned := make(chan bool, 1)
	waitCtx, stopWait := context.WithCancel(ctx)
	defer stopWait()
	go func() {
		for deadline := time.Now().Add(propBootBudget); time.Now().Before(deadline) && waitCtx.Err() == nil; {
			if _, err := r.env.Hub.WaitForObservation(waitCtx, r.slug, "phone_home", machine.ID); err == nil {
				phoned <- true
				return
			}
		}
		phoned <- false
	}()
	booted, dead := false, ""
	cursor := 0
wait:
	for {
		select {
		case booted = <-phoned:
			break wait
		case <-time.After(time.Second):
		}
		lines := vm.Serial.Lines()
		for ; cursor < len(lines); cursor++ {
			// ("grub rescue>" is a prompt with no newline and never becomes a
			// line; the line before it does.)
			for _, marker := range []string{"Entering rescue mode", "PXE boot failed after", "No bootable device.", "Dropping to a shell!"} {
				if strings.Contains(lines[cursor], marker) {
					dead = marker
				}
			}
		}
		if dead != "" {
			break wait
		}
	}
	stopWait()
	if !booted {
		if dead != "" {
			t.Logf("PROPDEAD %s step=%d the first boot ended at %q", r.c.Name, i, dead)
		}
		t.Logf("PROPSERIAL %s step=%d tail:\n%s", r.c.Name, i, propSerialTail(vm, 120))
		_ = vm.QMP.Quit()
		return "noboot"
	}

	ip := r.leaseIP(machine.MAC)
	if ip == "" {
		r.failf(i, "installed system phoned home but has no DHCP lease to ssh to")
		_ = vm.QMP.Quit()
		return "ok"
	}
	cli, err := dialRecoverySSH(ctx, ip, r.signer, r.network)
	if err != nil {
		r.failf(i, "installed system phoned home but ssh to %s failed: %v", ip, err)
		_ = vm.QMP.Quit()
		return "ok"
	}
	defer cli.Close()

	raw, err := r.sshRun(cli, propInspectScript)
	if err != nil {
		t.Logf("PROPINSPECT-ERR %s step=%d: %v", r.c.Name, i, err)
	}
	t.Logf("PROPINSPECT %s step=%d\n%s", r.c.Name, i, raw)
	ins, err := propParseInspect(raw)
	if err != nil {
		r.failf(i, "inspection failed: %v", err)
	} else {
		rep := propCheckTree(r.c, st, ins)
		for _, h := range rep.Hard {
			r.failf(i, "tree: %s", h)
		}
		for _, s := range rep.Soft {
			r.notef(i, "tree: %s", s)
		}
		r.checkCanaries(i, st, cli)
		if st.Reboot {
			if !r.checkReboot(ctx, i, vm, cli, ip, ins) {
				// The machine is not coming back; there is nothing to
				// shut down cleanly.
				_ = vm.QMP.Quit()
				return "ok"
			}
			if c2, err := dialRecoverySSH(ctx, ip, r.signer, r.network); err == nil {
				cli = c2
				defer c2.Close()
			}
		}
	}
	// A clean poweroff stops the arrays and flushes the filesystems the way
	// an operator shutdown would, so the next step inherits honest state.
	_, _ = r.sshRun(cli, "sync; systemctl poweroff")
	return "ok"
}

// checkCanaries verifies data survival on mounted arrays: a preserved array
// still holds the token an earlier step wrote, a re-created one is empty.
func (r *propRun) checkCanaries(i int, st propStep, cli *ssh.Client) {
	for _, a := range st.Layout.Arrays {
		if a.Mount == "" || a.Mount == "/" {
			continue
		}
		token := fmt.Sprintf("%s-s%d-%s", r.c.Name, i, a.Name)
		file := a.Mount + "/PROP_CANARY"
		out, err := r.sshRun(cli, fmt.Sprintf(
			`if [ -f %[1]s ]; then cat %[1]s; else printf '%%s' %[2]s > %[1]s && sync && cat %[1]s; fi`, file, token))
		got := strings.TrimSpace(out)
		if err != nil {
			r.failf(i, "canary on %s: %v (%q)", a.Mount, err, got)
			continue
		}
		key := a.Name
		switch a.Type {
		case "preserve":
			if want := r.canary[key]; want == "" {
				r.notef(i, "preserve array %s has no earlier canary to compare (content %q)", a.Name, got)
			} else if got != want {
				r.failf(i, "preserved array %s lost its data: canary %q, want %q", a.Name, got, want)
			}
		default:
			if got != token {
				r.failf(i, "re-created array %s still holds old data: canary %q, want the fresh %q", a.Name, got, token)
			}
		}
		r.canary[key] = got
	}
}

// checkReboot restarts the installed system and requires it to come back on
// the same root filesystem.
func (r *propRun) checkReboot(ctx context.Context, i int, vm *qemutest.VM, cli *ssh.Client, ip string, before *propInspect) bool {
	cursor := vm.Serial.Cursor()
	// The kernel's boot ID tells the second boot from the first: an ssh
	// connection alone would also succeed against a system that never went
	// down.
	const bootIDCmd = "cat /proc/sys/kernel/random/boot_id"
	firstID, err := r.sshRun(cli, bootIDCmd)
	if firstID = strings.TrimSpace(firstID); err != nil || firstID == "" {
		r.failf(i, "second boot: cannot read the first boot's ID: %v (%q)", err, firstID)
		return false
	}
	_, _ = r.sshRun(cli, "sync; systemctl reboot")
	_ = cli.Close()
	var c2 *ssh.Client
	err = fmt.Errorf("the system never went down")
	for deadline := time.Now().Add(propBootBudget); time.Now().Before(deadline) && ctx.Err() == nil; {
		probe, dialErr := dialRecoverySSH(ctx, ip, r.signer, r.network)
		if dialErr != nil {
			err = dialErr
			continue
		}
		id, idErr := r.sshRun(probe, bootIDCmd)
		if id = strings.TrimSpace(id); idErr == nil && id != "" && id != firstID {
			c2, err = probe, nil
			break
		}
		_ = probe.Close()
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		lines := vm.Serial.Lines()
		if cursor < len(lines) {
			lines = lines[cursor:]
		}
		if len(lines) > 80 {
			lines = lines[len(lines)-80:]
		}
		r.failf(i, "second boot: installed system did not come back after a reboot: %v\n%s", err, strings.Join(lines, "\n"))
		return false
	}
	defer c2.Close()
	// Compare the filesystem UUID, not the device: kernel names move
	// between boots (sdb one boot, sdc the next) and that is not a fault.
	out, _ := r.sshRun(c2, "findmnt -n -o UUID /; cat /proc/mdstat")
	want := strings.Fields(before.Sections["root"])
	got := strings.Fields(out)
	if len(want) < 3 || len(got) < 1 || got[0] != want[2] {
		r.failf(i, "second boot: root is %q, first boot had %q", strings.TrimSpace(out), strings.TrimSpace(before.Sections["root"]))
	}
	r.t.Logf("PROPREBOOT %s step=%d ok: %s", r.c.Name, i, strings.Join(strings.Fields(out), " "))
	return true
}

// runPropCase plays a whole case.
func runPropCase(t *testing.T, c propCase) {
	t.Helper()
	t.Logf("PROPCASE %s", c.JSON())
	if why := propKnownDefect(c); why != "" {
		t.Skipf("this history reaches a known defect: %s", why)
	}

	env := qemutest.New(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate keypair: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("ssh signer: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pubkey: %v", err)
	}
	r := &propRun{
		t:       t,
		c:       c,
		env:     env,
		network: env.NewNetwork(t),
		images:  env.UseCanonicalImages(t),
		slug:    "prop-" + c.Name,
		signer:  signer,
		authKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " slagworks-prop-test",
		dir:     t.TempDir(),
		cur:     map[string]string{},
		canary:  map[string]string{},
	}
	for _, d := range c.Disks {
		base := filepath.Join(r.dir, d.ID+".base.qcow2")
		propSeedDisk(t, d, base)
		r.cur[d.ID] = base
	}

	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Minute)
	defer cancel()

	var outcomes []string
	for i, st := range c.Steps {
		if ctx.Err() != nil {
			r.failf(i, "case ran out of time before this step")
			break
		}
		outcomes = append(outcomes, r.runStep(ctx, i, st))
	}
	sort.Strings(r.notes)
	res, _ := json.Marshal(map[string]any{
		"case": c.Name, "outcomes": outcomes, "fails": r.fails, "notes": r.notes,
	})
	t.Logf("PROPRESULT %s", res)
}
