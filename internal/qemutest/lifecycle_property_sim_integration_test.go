//go:build integration

package qemutest

// The simulated tier: the real site worker (Cache, LocalDB, WSConn,
// Executor, HTTP boot handler) and the real hub + op_update handler, with
// simulated hosts in place of VMs. No KVM, no netns, no root: a history
// takes one to a few seconds, so this tier carries the generated histories
// and the reduction of a failing one. The VM tier
// (lifecycle_property_vm_integration_test.go) runs the same histories
// through the same interpreter and oracle against real VMs.
//
// By default every test runs a small fixed set. Environment knobs scale it
// up for a deep local run:
//
//	SLAGWORKS_LIFECYCLE_PROP_SEEDS=A-B       generated histories (seed range)
//	SLAGWORKS_LIFECYCLE_PROP_FAMILIES=a,b    restrict their fault families
//	SLAGWORKS_LIFECYCLE_PROP_REPEAT=N        run each history N times
//	SLAGWORKS_LIFECYCLE_PROP_HISTORY=<json>  replay one history (or @file)
//	SLAGWORKS_LIFECYCLE_PROP_SHRINK=<text>   with HISTORY: drop steps while
//	                                         a violation containing <text>
//	                                         still reproduces
//	SLAGWORKS_LIFECYCLE_PROP_VERBOSE=1       report passing histories too
//
//	SLAGWORKS_LIFECYCLE_PROP_SEEDS=1-400 go test -tags integration \
//	    ./internal/qemutest -run 'TestLifecyclePropSimGenerated' -parallel 8

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

var propSimTiming = propTiming{
	Poll:      5 * time.Millisecond,
	Propagate: 30 * time.Millisecond,
	Quiet:     150 * time.Millisecond,
	Idle:      700 * time.Millisecond,
	Settle:    6 * time.Second,
	Live:      8 * time.Second,
	Grace:     400 * time.Millisecond,
	Actuate:   3 * time.Second,
}

var propSimGen = propGenConfig{Episodes: 3, MaxFaults: 3, DownMs: []int{0, 20, 150, 600, 1500}, Bystanders: 1}

// propSimDefaultSeeds is the generated-history range a default run covers.
const propSimDefaultSeeds = "1-6"

const propStorageScript = `#!/usr/bin/env python3
import json, os, sys

with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

disk = inv["disks"][0]["name"]

layout = {
    "layout": {
        disk: {
            "type": "disk", "ptable": "gpt", "boot": True,
            "partitions": [
                {"name": f"{disk}1", "size": "1M", "bios_grub": True},
                {"name": f"{disk}2", "fs": "vfat", "size": "512M", "bootable": True},
                {"name": f"{disk}3", "fs": "ext4", "size": "rest"},
            ],
        },
    },
    "mounts": {
        "/":          {"device": f"{disk}3"},
        "/boot/efi":  {"device": f"{disk}2"},
    },
}
json.dump(layout, sys.stdout)
`

// propBystanderMachine returns the model machine for a bystander variant.
func propBystanderMachine(variant string, id uuid.UUID, mac, smbios string) propMachine {
	m := propMachine{
		ID: id, Name: "bystander", MAC: mac, PXEEligible: true, InSnapshot: true, BMCIP: "prop-bmc-b",
		SystemUUID:     smbios,
		CommissionData: json.RawMessage(`{"system":{"uuid":"` + smbios + `"}}`),
		StorageScript:  &model.SnapshotStorageScript{Name: "default", Content: propStorageScript},
	}
	switch variant {
	case "running":
		m.State, m.Locked = model.MachineStateRunning, true
	case "commissioned":
		m.State = model.MachineStateCommissioned
	case "failed":
		m.State = model.MachineStateFailed
	case "new":
		m.State, m.SystemUUID, m.CommissionData = model.MachineStateNew, "", nil
	case "pxe_off":
		m.State, m.PXEEligible = model.MachineStateCommissioned, false
	case "unknown":
		m.State, m.InSnapshot = model.MachineStateNew, false
	}
	return m
}

func propSubjectMachine(setup propSetup, id uuid.UUID, mac, smbios string) propMachine {
	m := propMachine{
		ID: id, Name: "subject", MAC: mac, PXEEligible: true, InSnapshot: true, BMCIP: "prop-bmc-a",
		State:         model.MachineStateCommissioned,
		StorageScript: &model.SnapshotStorageScript{Name: "default", Content: propStorageScript},
	}
	if setup.SubjectNew {
		m.State = model.MachineStateNew
	} else {
		m.SystemUUID = smbios
		m.CommissionData = json.RawMessage(`{"system":{"uuid":"` + smbios + `"}}`)
	}
	return m
}

// propQuiet silences the process-wide default slog logger, which the hub
// and the op_update handler write to (a line per phase transition per
// history), until t and its subtests end. Call it from the top-level test,
// never from parallel subtests: their restores would interleave. The
// worker's own log is kept per history (propLogBuf) and printed with a
// failure.
func propQuiet(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// runPropSimHistory executes one history on the simulated tier and returns the
// run with its violations evaluated.
func runPropSimHistory(t *testing.T, h propHistory) *propRun {
	t.Helper()
	const step = 4 * time.Millisecond
	g := newPropGlobal(t, "prop-sim", model.DHCPConfig{
		Subnet: "198.18.0.0/24", Gateway: "198.18.0.1",
		EphemeralPoolStart: "198.18.0.129", EphemeralPoolEnd: "198.18.0.254",
	})
	var hosts []propHost
	var ids []uuid.UUID
	w := newPropWorker(t, propWorkerConfig{
		Slug: "prop-sim", Global: g, Sim: true,
		SyncWait: 300 * time.Millisecond, ExecInterval: 50 * time.Millisecond,
		HostFor: func(id uuid.UUID) propHost {
			for i := range ids {
				if ids[i] == id {
					return hosts[i]
				}
			}
			return nil
		},
	})
	subject := newPropSimHost("subject", "52:54:00:00:00:0a", uuid.New(), w, step)
	hosts, ids = append(hosts, subject), append(ids, subject.machine)
	g.AddMachine(propSubjectMachine(h.Setup, subject.machine, subject.mac, subject.smbios))
	if h.Setup.Bystander != "" {
		b := newPropSimHost("bystander", "52:54:00:00:00:0b", uuid.New(), w, step)
		if h.Setup.Bystander == "running" {
			// An in-service host: an OS on disk that phoned home long ago.
			b.os, b.phonedHome, b.dirty = "preexisting-install", true, true
		}
		hosts, ids = append(hosts, b), append(ids, b.machine)
		g.AddMachine(propBystanderMachine(h.Setup.Bystander, b.machine, b.mac, b.smbios))
	}
	g.PrimeSnapshot()
	w.Start()
	for _, hst := range hosts {
		_ = hst.PowerOn()
	}
	time.Sleep(10 * step)

	r := newPropRun(t, g, w, hosts, ids, propSimTiming)
	r.Run(h)
	r.Liveness(0)
	// The final shutdown is part of P6, so it happens before the check.
	w.Kill()
	r.Check()
	// Stop the hosts so nothing keeps posting to the dead worker.
	for _, hst := range hosts {
		_ = hst.PowerOff()
	}
	return r
}

func propReport(t *testing.T, r *propRun, h propHistory) {
	t.Helper()
	t.Log(r.StatLine(h))
	if vs := r.Violations(); len(vs) > 0 {
		t.Errorf("%d violation(s); first: %s\n%s", len(vs), vs[0], r.Report(h))
		if tail := r.w.logBuf.Tail(200); len(tail) > 0 {
			t.Logf("worker log tail:\n  %s", strings.Join(tail, "\n  "))
		}
	} else if os.Getenv("SLAGWORKS_LIFECYCLE_PROP_VERBOSE") != "" {
		t.Logf("history report:\n%s", r.Report(h))
	}
}

// propNamedHistories are hand-written histories: a clean lifecycle, a fault
// at each of the delicate moments, and the histories that exercise one
// worker guard each (named in their comments: with that guard removed the
// history fails with the property shown).
func propNamedHistories() []propHistory {
	at := func(before bool, kind, boot, phase, event string) *propMoment {
		return &propMoment{Before: before, Host: 0, Kind: kind, Boot: boot, Phase: phase, Event: event}
	}
	return []propHistory{
		// The post-install reboot PXE-boots; without the awaiting_phone_home
		// -> BootLocal branch of the worker's boot table it is served the
		// installer again (P2).
		{Name: "clean-deploy", Setup: propSetup{Bystander: "running"}, Steps: []propStep{
			{Do: "deploy", Flag: true},
			{Do: "vm_reset", Host: 1},
			{Do: "settle", Flag: true},
			{Do: "vm_reset", Host: 0},
			{Do: "vm_reset", Host: 1},
			{Do: "settle", Flag: true},
		}},
		{Name: "commission-deploy-reset-mid-rsync", Setup: propSetup{SubjectNew: true}, Steps: []propStep{
			{Do: "commission"},
			{Do: "settle", Flag: true},
			{Do: "vm_reset", At: at(false, "report", "", "rsyncing_os", "")},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
		}},
		// The latch survives a worker restart (it is persisted in LocalDB).
		{Name: "worker-restart-awaiting-phone-home", Setup: propSetup{Bystander: "pxe_off"}, Steps: []propStep{
			{Do: "worker_kill", Ms: 600, At: at(false, "report", "", "", "deploy_complete")},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
			{Do: "vm_reset", Host: 1},
			{Do: "settle", Flag: true},
		}},
		{Name: "abort-mid-partitioning-then-redeploy", Steps: []propStep{
			{Do: "abort", At: at(false, "report", "", "partitioning", "")},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
			{Do: "stale_report", Arg: "deploy_complete"},
			{Do: "stale_report", Arg: "phone_home"},
			{Do: "deploy", Flag: true},
			{Do: "stale_report", Arg: "failed"},
			{Do: "settle", Flag: true},
		}},
		{Name: "reset-during-first-disk-boot", Steps: []propStep{
			{Do: "vm_reset", At: at(false, "boot", "local", "", "")},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
		}},
		{Name: "hub-down-across-deploy-tail", Steps: []propStep{
			{Do: "hub_down", Ms: 1500, At: at(true, "report", "", "", "deploy_complete")},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
		}},
		{Name: "expire-while-agent-running", Steps: []propStep{
			{Do: "sweep", Ms: int(31 * time.Minute / time.Millisecond), At: at(false, "report", "", "rsyncing_os", "")},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
		}},
		// The host's boot-prefix phase updates travel right behind the
		// executor's claim on the same connection.
		{Name: "deploy-created-while-host-is-pxe-booting", Steps: []propStep{
			{Do: "deploy", Flag: true, Ms: 2, At: &propMoment{Before: true, Host: 0, Kind: "dhcp", Phase: "ipxe_dhcp"}},
			{Do: "vm_reset"},
			{Do: "settle", Flag: true},
		}},
		{Name: "power-ops-around-deploy", Steps: []propStep{
			{Do: "power", Arg: "off"},
			{Do: "settle", Flag: true},
			{Do: "deploy", Flag: true},
			{Do: "power", Arg: "cycle"},
			{Do: "settle", Flag: true},
			{Do: "power", Arg: "cycle"},
			{Do: "settle", Flag: true},
		}},
		// The tombstone check at the top of the worker's applyOpDelta: an
		// older delta of the aborted op, published after its removal, must
		// not bring it back (P3: the aborted agent's failure report would be
		// accepted for an op global ended). The abort fires BEFORE the
		// report so the worker sends no phase update for the dead op, and
		// the host is not PXE-booted afterwards, which keeps anything else
		// from superseding the replayed delta.
		{Name: "replayed-delta-after-abort", Steps: []propStep{
			{Do: "abort", At: &propMoment{Before: true, Host: 0, Kind: "report", Phase: "rsyncing_os"}},
			{Do: "deploy", Flag: true},
			{Do: "settle", Flag: true},
			{Do: "replay_ops", Ms: 8, Flag: true},
			{Do: "settle", Flag: true},
		}},
		// The worker's untrusted-until-resync rule: a restarted worker that
		// cannot reach global serves BootLocal, never an installer (P1).
		{Name: "restarted-worker-without-global", Steps: []propStep{
			{Do: "deploy", Flag: true},
			{Do: "await", At: &propMoment{Host: 0, Kind: "report", Phase: "agent_starting"}},
			{Do: "hub_down", Ms: 2500},
			{Do: "worker_kill", Ms: 20},
			// First reset: the agent dies before it has written an OS.
			{Do: "wait", Ms: 30},
			{Do: "vm_reset"},
			// Second reset: the worker is back, global still unreachable.
			{Do: "wait", Ms: 600},
			{Do: "vm_reset"},
			{Do: "wait", Ms: 300},
			{Do: "settle", Flag: true},
		}},
		// A phone-home is accepted only once deploy_complete has latched
		// awaiting_phone_home. Here the agent finishes the install but its
		// deploy_complete is refused for longer than it retries (the
		// restarted worker is untrusted), so it halts; the host is later
		// reset and boots the finished install, whose phone-home must not
		// complete the deploy (P3).
		{Name: "deploy-complete-refused-then-install-boots", Steps: []propStep{
			{Do: "deploy", Flag: true},
			{Do: "await", At: &propMoment{Host: 0, Kind: "report", Phase: "agent_starting"}},
			{Do: "hub_down", Ms: 2500},
			{Do: "worker_kill", Ms: 20},
			{Do: "wait", Ms: 800},
			{Do: "vm_reset"},
			{Do: "wait", Ms: 300},
			{Do: "settle", Flag: true},
		}},
	}
}

func propNamedHistory(t *testing.T, name string) propHistory {
	t.Helper()
	for _, h := range propNamedHistories() {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no named history %q", name)
	return propHistory{}
}

// propFileHistories are the committed histories in
// testdata/lifecycle_property_histories.json, written at the VM tier's
// time scale.
func propFileHistories(t *testing.T) []propHistory {
	t.Helper()
	b, err := os.ReadFile("testdata/lifecycle_property_histories.json")
	if err != nil {
		t.Fatalf("read committed histories: %v", err)
	}
	var hs []propHistory
	if err := json.Unmarshal(b, &hs); err != nil {
		t.Fatalf("parse testdata/lifecycle_property_histories.json: %v", err)
	}
	return hs
}

func propFileHistory(t *testing.T, name string) propHistory {
	t.Helper()
	for _, h := range propFileHistories(t) {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no history %q in testdata/lifecycle_property_histories.json", name)
	return propHistory{}
}

// propEnvInt reads a positive integer knob, or def.
func propEnvInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive integer", key, v)
	}
	return n
}

var propSeedRange = regexp.MustCompile(`^(\d+)-(\d+)$`)

// propSeeds parses SLAGWORKS_LIFECYCLE_PROP_SEEDS, or def when it is unset.
// ok is false when neither names a range.
func propSeeds(t *testing.T, def string) (lo, hi int64, ok bool) {
	t.Helper()
	s := os.Getenv("SLAGWORKS_LIFECYCLE_PROP_SEEDS")
	if s == "" {
		s = def
	}
	if s == "" {
		return 0, 0, false
	}
	m := propSeedRange.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("seed range %q: want A-B", s)
	}
	lo, _ = strconv.ParseInt(m[1], 10, 64)
	hi, _ = strconv.ParseInt(m[2], 10, 64)
	if hi < lo {
		t.Fatalf("seed range %q: B is below A", s)
	}
	return lo, hi, true
}

// propWithFamilies applies SLAGWORKS_LIFECYCLE_PROP_FAMILIES.
func propWithFamilies(t *testing.T, cfg propGenConfig) propGenConfig {
	t.Helper()
	v := os.Getenv("SLAGWORKS_LIFECYCLE_PROP_FAMILIES")
	if v == "" {
		return cfg
	}
	known := map[string]bool{}
	for _, f := range propFaultFamilies {
		known[f] = true
	}
	for _, f := range strings.Split(v, ",") {
		if !known[f] {
			t.Fatalf("SLAGWORKS_LIFECYCLE_PROP_FAMILIES: unknown fault family %q (have %v)", f, propFaultFamilies)
		}
		cfg.Families = append(cfg.Families, f)
	}
	return cfg
}

// runPropSim runs each history as a parallel subtest (serialized by
// -parallel 1 in CI), each SLAGWORKS_LIFECYCLE_PROP_REPEAT times.
func runPropSim(t *testing.T, hs []propHistory, transform func(propHistory) propHistory) {
	t.Helper()
	propQuiet(t)
	repeat := propEnvInt(t, "SLAGWORKS_LIFECYCLE_PROP_REPEAT", 1)
	for _, h := range hs {
		for rep := 0; rep < repeat; rep++ {
			t.Run(h.Name, func(t *testing.T) {
				t.Parallel()
				run := h
				if transform != nil {
					run = transform(h)
				}
				propReport(t, runPropSimHistory(t, run), run)
			})
		}
	}
}

// TestLifecyclePropSimNamed runs the hand-written histories and the
// committed ones on the simulated tier.
func TestLifecyclePropSimNamed(t *testing.T) {
	runPropSim(t, propNamedHistories(), nil)
	runPropSim(t, propFileHistories(t), propUnscaleHistory)
}

// TestLifecyclePropSimGenerated runs generated histories on the simulated
// tier: seeds 1-6 by default, SLAGWORKS_LIFECYCLE_PROP_SEEDS for more.
func TestLifecyclePropSimGenerated(t *testing.T) {
	lo, hi, _ := propSeeds(t, propSimDefaultSeeds)
	cfg := propWithFamilies(t, propSimGen)
	var hs []propHistory
	for seed := lo; seed <= hi; seed++ {
		hs = append(hs, propGenHistory(seed, cfg))
	}
	runPropSim(t, hs, nil)
}

func propHistoryFromEnv(t *testing.T) (propHistory, bool) {
	t.Helper()
	src := os.Getenv("SLAGWORKS_LIFECYCLE_PROP_HISTORY")
	if src == "" {
		return propHistory{}, false
	}
	if strings.HasPrefix(src, "@") {
		b, err := os.ReadFile(src[1:])
		if err != nil {
			t.Fatalf("read history file: %v", err)
		}
		src = string(b)
	}
	var h propHistory
	if err := json.Unmarshal([]byte(src), &h); err != nil {
		t.Fatalf("parse SLAGWORKS_LIFECYCLE_PROP_HISTORY: %v", err)
	}
	return h, true
}

// TestLifecyclePropSimReplay replays SLAGWORKS_LIFECYCLE_PROP_HISTORY (the
// `json:` line of a failure report) on the simulated tier. With
// SLAGWORKS_LIFECYCLE_PROP_SHRINK it instead removes steps greedily while a
// violation containing that text still reproduces (each candidate tried
// SLAGWORKS_LIFECYCLE_PROP_REPEAT times; any reproduction keeps the
// removal) and logs the reduced history.
func TestLifecyclePropSimReplay(t *testing.T) {
	h, ok := propHistoryFromEnv(t)
	if !ok {
		t.Skip("set SLAGWORKS_LIFECYCLE_PROP_HISTORY to replay a history")
	}
	sig := os.Getenv("SLAGWORKS_LIFECYCLE_PROP_SHRINK")
	if sig == "" {
		runPropSim(t, []propHistory{h}, nil)
		return
	}
	propQuiet(t)
	repeat := propEnvInt(t, "SLAGWORKS_LIFECYCLE_PROP_REPEAT", 1)
	fails := func(c propHistory) bool {
		for rep := 0; rep < repeat; rep++ {
			found := false
			t.Run("try", func(t *testing.T) {
				for _, v := range runPropSimHistory(t, c).Violations() {
					if strings.Contains(v.String(), sig) {
						found = true
					}
				}
			})
			if found {
				return true
			}
		}
		return false
	}
	if !fails(h) {
		t.Fatalf("history does not reproduce %q", sig)
	}
	for changed := true; changed; {
		changed = false
		for i := 0; i < len(h.Steps); i++ {
			c := h
			c.Steps = append(append([]propStep(nil), h.Steps[:i]...), h.Steps[i+1:]...)
			if fails(c) {
				h = c
				changed = true
				i--
			}
		}
	}
	if h.Setup.Bystander != "" {
		c := h
		c.Setup.Bystander = ""
		ok := true
		for _, s := range c.Steps {
			if s.Host != 0 {
				ok = false
			}
		}
		if ok && fails(c) {
			h = c
		}
	}
	t.Logf("reduced to %d step(s): %s\njson: %s", len(h.Steps), h, h.JSON())
}
