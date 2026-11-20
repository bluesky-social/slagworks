//go:build integration

package qemutest

// The VM tier: the same histories, interpreter and oracle as the simulated
// tier, against real VMs: real firmware PXE, the worker's real
// DHCP/TFTP/HTTP boot stack in a netns, the real agent partitioning real
// (virtual) disks, the real installed OS phoning home.
//
// A history here costs two deploys (its own and the closing liveness
// deploy), four to six minutes on a CI runner, so a default run covers three
// histories, one per top-level test so they spread over CI shards. The rest
// is opt-in:
//
//	SLAGWORKS_LIFECYCLE_PROP_VM=1         every named and committed history
//	SLAGWORKS_LIFECYCLE_PROP_VM_SEEDS=A-B generated histories
//
//	SLAGWORKS_LIFECYCLE_PROP_VM=1 go test -tags integration ./internal/qemutest \
//	    -run 'TestLifecyclePropVMAll' -parallel 8

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
)

var propVMTiming = propTiming{
	Poll:      250 * time.Millisecond,
	Propagate: 300 * time.Millisecond,
	Quiet:     25 * time.Second,
	Idle:      150 * time.Second,
	Settle:    8 * time.Minute,
	Live:      6 * time.Minute,
	Grace:     3 * time.Second,
	Actuate:   time.Minute,
	Budget:    10 * time.Minute,
}

var propVMGen = propGenConfig{Episodes: 2, MaxFaults: 2, DownMs: []int{0, 3000, 15000, 40000}, Bystanders: 1}

const propRAID1StorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disks = sorted(d["name"] for d in inv["disks"])
if len(disks) != 2:
    print(f"expected 2 disks, got {len(disks)}: {disks}", file=sys.stderr)
    sys.exit(1)

d0, d1 = disks[0], disks[1]

layout = {
    "layout": {
        d0: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{d0}1", "size": "1M", "bios_grub": True},
                {"name": f"{d0}2", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{d0}3", "size": "rest"},
            ],
        },
        d1: {
            "type": "disk", "ptable": "gpt",
            "partitions": [
                {"name": f"{d1}1", "size": "1M", "bios_grub": True},
                {"name": f"{d1}2", "size": "512M"},
                {"name": f"{d1}3", "size": "rest"},
            ],
        },
        "mdroot": {
            "type": "raid", "level": 1,
            "members": [f"{d0}3", f"{d1}3"],
            "fs": "ext4",
        },
    },
    "mounts": {
        "/":          {"device": "mdroot"},
        "/boot/efi":  {"device": f"{d0}2"},
    },
}
json.dump(layout, sys.stdout)
`

// propVMHost drives a VM the way a BMC drives a chassis. Power-off is a
// paused VM (QEMU cannot power a stopped process back on); power-on from
// off resets first, so the host cold-boots as real hardware does.
type propVMHost struct {
	name string
	vm   *VM

	mu  sync.Mutex
	off bool
}

func (h *propVMHost) Name() string { return h.name }
func (h *propVMHost) MAC() string  { return h.vm.Config.MAC }

func (h *propVMHost) PowerCycle() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.vm.QMP.Reset(); err != nil {
		return err
	}
	if h.off {
		h.off = false
		return h.vm.QMP.Cont()
	}
	return nil
}

func (h *propVMHost) PowerOff() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.off = true
	return h.vm.QMP.Stop()
}

func (h *propVMHost) PowerOn() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.off {
		return nil
	}
	if err := h.vm.QMP.Reset(); err != nil {
		return err
	}
	h.off = false
	return h.vm.QMP.Cont()
}

func (h *propVMHost) PowerState() (power.State, error) {
	st, err := h.vm.QMP.QueryStatus()
	if err != nil {
		return power.StateUnknown, err
	}
	if st == "running" {
		return power.StateOn, nil
	}
	return power.StateOff, nil
}

// DiskWrites sums write operations over the VM's data disks (firmware
// flash excluded: OVMF writes its variable store on every boot).
func (h *propVMHost) DiskWrites() int64 {
	var out []struct {
		Device string `json:"device"`
		QDev   string `json:"qdev"`
		Stats  struct {
			WrOperations int64 `json:"wr_operations"`
		} `json:"stats"`
	}
	if err := h.vm.QMP.executeArgs("query-blockstats", nil, &out); err != nil {
		return -1
	}
	var n int64
	for _, d := range out {
		if strings.Contains(d.Device, "pflash") || strings.Contains(d.QDev, "pflash") {
			continue
		}
		n += d.Stats.WrOperations
	}
	return n
}

func runPropVMHistory(t *testing.T, h propHistory) *propRun {
	t.Helper()
	env := New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)
	start, end := ephemeralPoolRange(network.Subnet)
	slug := "prop-vm"
	g := newPropGlobal(t, slug, model.DHCPConfig{
		Subnet: network.SubnetStr, Gateway: network.Gateway,
		EphemeralPoolStart: start, EphemeralPoolEnd: end,
	})
	// A claim on real global is a Postgres transaction of half a dozen
	// statements; in-memory it would be microseconds.
	g.dbLatency = 5 * time.Millisecond
	var hosts []propHost
	var ids []uuid.UUID
	w := newPropWorker(t, propWorkerConfig{
		Slug: slug, Global: g, Network: network, Images: images, DefaultImage: "noble-pxe",
		SyncWait: 5 * time.Second, ExecInterval: 500 * time.Millisecond,
		HostFor: func(id uuid.UUID) propHost {
			for i := range ids {
				if ids[i] == id {
					return hosts[i]
				}
			}
			return nil
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 23*time.Minute)
	t.Cleanup(cancel)

	fw := BIOS
	if h.Setup.UEFI {
		fw = UEFI
	}
	subjectID, subjectUUID := uuid.New(), uuid.NewString()
	cfg := VMConfig{
		Firmware: fw, MAC: "52:54:00:9a:00:0a", Network: network,
		Memory: "4096", SMBIOSUUID: subjectUUID, DiskSize: "10G",
		// OVMF would otherwise boot the disk entry the install registered
		// on every later reset, and a second deploy could never start: the
		// NIC stays first, as on a host whose BMC holds a PXE override,
		// and the agent's one-shot BootNext still wins once.
		NetworkBootFirst: h.Setup.UEFI,
	}
	subjM := propSubjectMachine(h.Setup, subjectID, cfg.MAC, subjectUUID)
	if h.Setup.RAID1 {
		cfg.DiskSize = "6G"
		cfg.ExtraDisks = []ExtraDisk{{Type: DiskVirtio, Size: "6G"}}
		subjM.StorageScript = &model.SnapshotStorageScript{Name: "raid1", Content: propRAID1StorageScript}
	}
	subjVM := env.NewVM(t, cfg)
	subject := &propVMHost{name: "subject", vm: subjVM}
	hosts, ids = append(hosts, subject), append(ids, subjectID)
	g.AddMachine(subjM)

	var byVM *VM
	if h.Setup.Bystander != "" {
		byID, byUUID := uuid.New(), uuid.NewString()
		byVM = env.NewVM(t, VMConfig{
			Firmware: BIOS, MAC: "52:54:00:9a:00:0b", Network: network,
			Memory: "1024", SMBIOSUUID: byUUID, DiskSize: "1G",
		})
		hosts, ids = append(hosts, &propVMHost{name: "bystander", vm: byVM}), append(ids, byID)
		g.AddMachine(propBystanderMachine(h.Setup.Bystander, byID, byVM.Config.MAC, byUUID))
	}

	g.PrimeSnapshot()
	w.Start()
	subjVM.Start(ctx)
	if byVM != nil {
		byVM.Start(ctx)
	}
	// Both hosts PXE-boot with no operation (the first spurious boot);
	// let them reach the boot server before the history starts.
	w.rec.WaitFor(func(e *propHTTPEvent) bool { return e.Kind == "boot" && e.MAC == cfg.MAC }, 0, 90*time.Second)

	r := newPropRun(t, g, w, hosts, ids, propVMTiming)
	r.Run(h)
	r.Liveness(0)
	// The final shutdown is part of P6, so it happens before the check.
	w.Kill()
	r.Check()
	return r
}

// runPropVM runs one history on VMs and reports it. The full report is
// logged whether or not the history passed: it is what a failure is
// reduced from, and what a pass is compared against.
func runPropVM(t *testing.T, h propHistory) {
	t.Helper()
	r := runPropVMHistory(t, h)
	t.Log(r.StatLine(h))
	if vs := r.Violations(); len(vs) > 0 {
		t.Errorf("%d violation(s); first: %s", len(vs), vs[0])
	}
	t.Logf("history report:\n%s", r.Report(h))
	if len(r.Violations()) > 0 {
		t.Logf("worker log tail:\n  %s", strings.Join(r.w.logBuf.Tail(250), "\n  "))
	}
}

// TestLifecyclePropVMClaimRace: the deploy is created while the host is
// already PXE-booting, so the real DHCP ACK's phase update travels right
// behind the executor's claim.
func TestLifecyclePropVMClaimRace(t *testing.T) {
	runPropVM(t, propScaleHistory(propNamedHistory(t, "deploy-created-while-host-is-pxe-booting")))
}

// TestLifecyclePropVMLatchAcrossRestart: the worker restarts right after
// accepting deploy_complete, and the real post-install reboot must boot
// the disk and phone home.
func TestLifecyclePropVMLatchAcrossRestart(t *testing.T) {
	runPropVM(t, propScaleHistory(propNamedHistory(t, "worker-restart-awaiting-phone-home")))
}

// TestLifecyclePropVMLostCompletion: global crashes with the real install's
// completion read but not committed.
func TestLifecyclePropVMLostCompletion(t *testing.T) {
	runPropVM(t, propFileHistory(t, "global-crash-at-phone-home"))
}

// TestLifecyclePropVMAll is the opt-in VM run: every named and committed
// history with SLAGWORKS_LIFECYCLE_PROP_VM=1, generated histories with
// SLAGWORKS_LIFECYCLE_PROP_VM_SEEDS=A-B.
func TestLifecyclePropVMAll(t *testing.T) {
	var hs []propHistory
	if v := os.Getenv("SLAGWORKS_LIFECYCLE_PROP_VM"); v != "" {
		if on, err := strconv.ParseBool(v); err != nil || !on {
			t.Fatalf("SLAGWORKS_LIFECYCLE_PROP_VM=%q: want 1 (or unset)", v)
		}
		for _, h := range propNamedHistories() {
			hs = append(hs, propScaleHistory(h))
		}
		hs = append(hs, propFileHistories(t)...)
	}
	if s := os.Getenv("SLAGWORKS_LIFECYCLE_PROP_VM_SEEDS"); s != "" {
		m := propSeedRange.FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("SLAGWORKS_LIFECYCLE_PROP_VM_SEEDS=%q: want A-B", s)
		}
		lo, _ := strconv.ParseInt(m[1], 10, 64)
		hi, _ := strconv.ParseInt(m[2], 10, 64)
		cfg := propWithFamilies(t, propVMGen)
		cfg.UEFI = os.Getenv("SLAGWORKS_LIFECYCLE_PROP_VM_UEFI") == "1"
		for seed := lo; seed <= hi; seed++ {
			hs = append(hs, propGenHistory(seed, cfg))
		}
	}
	if len(hs) == 0 {
		t.Skip("set SLAGWORKS_LIFECYCLE_PROP_VM=1 or SLAGWORKS_LIFECYCLE_PROP_VM_SEEDS=A-B")
	}
	for _, h := range hs {
		t.Run(h.Name, func(t *testing.T) {
			t.Parallel()
			t.Logf("history: %s", h.JSON())
			runPropVM(t, h)
		})
	}
}

// propVMSetupHistories are named histories under the firmware and storage
// setups the named set never uses: UEFI (the install's BootNext and a NIC
// pinned first), a RAID1 root over two disks, and both.
func propVMSetupHistories(t *testing.T) []propHistory {
	t.Helper()
	setups := []struct {
		tag   string
		setup func(*propSetup)
	}{
		{"uefi", func(s *propSetup) { s.UEFI = true }},
		{"raid1", func(s *propSetup) { s.RAID1 = true }},
		{"uefi-raid1", func(s *propSetup) { s.UEFI, s.RAID1 = true, true }},
	}
	var hs []propHistory
	for _, name := range []string{
		"clean-deploy",
		"reset-during-first-disk-boot",
		"abort-mid-partitioning-then-redeploy",
		"worker-restart-awaiting-phone-home",
	} {
		for _, s := range setups {
			h := propScaleHistory(propNamedHistory(t, name))
			h.Name = name + "+" + s.tag
			s.setup(&h.Setup)
			if h.Setup.UEFI && name == "reset-during-first-disk-boot" {
				// A UEFI install's first boot follows the agent's BootNext
				// straight to the disk, so the PXE-observed local boot this
				// history arms on never happens. Reset instead as soon as
				// deploy_complete is accepted, before the agent arms
				// BootNext: that boot goes PXE, then boot-local.
				h.Name = "reset-after-deploy-complete+" + s.tag
				h.Steps[0].At = &propMoment{Host: 0, Kind: "report", Event: "deploy_complete"}
			}
			hs = append(hs, h)
		}
	}
	return hs
}

// TestLifecyclePropVMSetups runs propVMSetupHistories with
// SLAGWORKS_LIFECYCLE_PROP_VM_SETUPS=1 (twelve histories, two or three
// deploys each).
func TestLifecyclePropVMSetups(t *testing.T) {
	if os.Getenv("SLAGWORKS_LIFECYCLE_PROP_VM_SETUPS") != "1" {
		t.Skip("set SLAGWORKS_LIFECYCLE_PROP_VM_SETUPS=1 to run the named histories under UEFI and RAID1 setups")
	}
	for _, h := range propVMSetupHistories(t) {
		t.Run(h.Name, func(t *testing.T) {
			t.Parallel()
			t.Logf("history: %s", h.JSON())
			runPropVM(t, h)
		})
	}
}

// propScaleHistory converts a history written at the simulated tier's time
// scale to the VM tier's: outage lengths in the hundreds of milliseconds
// become tens of seconds.
func propScaleHistory(h propHistory) propHistory {
	out := h
	out.Steps = append([]propStep(nil), h.Steps...)
	for i, s := range out.Steps {
		switch s.Do {
		case "worker_kill", "hub_down", "hub_crash", "wait", "bmc_slow", "vm_off":
			if s.Ms > 0 && s.Ms < 5000 {
				out.Steps[i].Ms = s.Ms * 20
			}
		}
	}
	return out
}

// propUnscaleHistory is propScaleHistory's inverse for histories committed
// at the VM time scale.
func propUnscaleHistory(h propHistory) propHistory {
	out := h
	out.Steps = append([]propStep(nil), h.Steps...)
	for i, s := range out.Steps {
		switch s.Do {
		case "worker_kill", "hub_down", "hub_crash", "wait", "bmc_slow", "vm_off":
			if s.Ms >= 5000 {
				out.Steps[i].Ms = s.Ms / 20
			}
		}
	}
	return out
}
