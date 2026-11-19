//go:build integration

package qemutest_test

// Storage property suite, part 1: the case model, the storage-script renderer
// and the expected-tree oracle.
//
// A case is a set of virtual disks plus a HISTORY of deploys onto them. Each
// step boots a brand new VM (new machine identity, fresh NVRAM) that inherits
// the disks exactly as the previous step left them: nothing wipes them in
// between. The oracle is written against the layout the case asks for, not
// against storage.Plan, so the planner and the oracle do not share an
// implementation.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type propDisk struct {
	ID string `json:"id"` // "d0".., stable across the steps of a case
	// Bus is virtio, nvme, sata or scsi.
	Bus string `json:"bus"`
	// Bytes is unique within a case: the storage script and the oracle both
	// find a disk by its size, which survives any kernel renaming.
	Bytes int64 `json:"bytes"`
	LBS   int   `json:"lbs"` // logical block size, 512 or 4096
	// Discard makes guest discards unmap, so discarded ranges read back as
	// zeroes the way they do on most SSDs. Off, a discard is ignored and the
	// old bytes stay, the way they do on a spinning disk.
	Discard bool `json:"discard,omitempty"`
	// Seed is content written before the first step: "", ext4, luks, dos,
	// ambiv (ext4 plus ZFS uberblocks, which blkid reports as ambivalent).
	Seed string `json:"seed,omitempty"`
}

type propPart struct {
	Size     string `json:"size"` // "512M", "5G", "rest", or "pct:40" (percent of the disk)
	FS       string `json:"fs,omitempty"`
	ESP      bool   `json:"esp,omitempty"`
	BIOSGrub bool   `json:"bios_grub,omitempty"`
	Mount    string `json:"mount,omitempty"`
}

type propDiskLayout struct {
	Disk  string     `json:"disk"`
	Parts []propPart `json:"parts"`
}

type propMember struct {
	Disk string `json:"disk"`
	Part int    `json:"part"` // 1-based partition number, 0 for the whole disk
}

type propArray struct {
	Name    string       `json:"name"`
	Type    string       `json:"type"` // raid or preserve
	Level   int          `json:"level"`
	Members []propMember `json:"members"`
	FS      string       `json:"fs,omitempty"`
	Mount   string       `json:"mount,omitempty"`
	UUID    string       `json:"uuid,omitempty"`
}

type propLayout struct {
	Disks  []propDiskLayout `json:"disks"`
	Arrays []propArray      `json:"arrays,omitempty"`
	Retain []string         `json:"retain,omitempty"`
}

type propStep struct {
	// Attach lists the disks present for this step, in firmware order: the
	// first is the firmware's first disk.
	Attach      []string   `json:"attach"`
	Layout      propLayout `json:"layout"`
	Destructive bool       `json:"destructive,omitempty"`
	WipeAll     bool       `json:"wipe_all,omitempty"`
	// FatalAt stops the agent when it enters the named phase.
	FatalAt string `json:"fatal_at,omitempty"`
	// KillAtCmd powers the VM off as soon as the Nth planned storage command
	// (1-based, as the agent logs the plan) has completed.
	KillAtCmd int `json:"kill_at_cmd,omitempty"`
	// Reboot restarts the installed system once more after verification and
	// checks it comes back on the same root. The first boot after a UEFI
	// deploy goes through the one-shot BootNext; this exercises the path every
	// later boot takes (PXE, then the boot-local script).
	Reboot bool `json:"reboot,omitempty"`
	// Expect is "ok", "fatal:<stage>", "fatal_at", "killed", or several of
	// those joined with "|" when the history leaves the outcome open.
	Expect string `json:"expect"`
	// Why says, for a reader of a failing case, what rule produced Expect.
	Why string `json:"why,omitempty"`
	// Hazard names a written-up finding this step is known to walk into.
	// The driver then logs the step's tree violations instead of failing.
	Hazard string `json:"hazard,omitempty"`
}

type propCase struct {
	Name     string     `json:"name"`
	Firmware string     `json:"firmware"` // uefi or bios
	Disks    []propDisk `json:"disks"`
	Steps    []propStep `json:"steps"`
}

func (c propCase) disk(id string) propDisk {
	for _, d := range c.Disks {
		if d.ID == id {
			return d
		}
	}
	panic("prop: unknown disk " + id)
}

func (c propCase) JSON() string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const propMiB = int64(1) << 20

// propSizeBytes resolves a partition size spec against its disk. ok is false
// for "rest".
func propSizeBytes(spec string, diskBytes int64) (int64, bool) {
	switch {
	case spec == "rest":
		return 0, false
	case strings.HasPrefix(spec, "pct:"):
		n, err := strconv.Atoi(spec[4:])
		if err != nil {
			panic("prop: bad size " + spec)
		}
		return diskBytes * int64(n) / 100 / propMiB * propMiB, true
	case strings.HasSuffix(spec, "M"):
		n, err := strconv.Atoi(strings.TrimSuffix(spec, "M"))
		if err != nil {
			panic("prop: bad size " + spec)
		}
		return int64(n) * propMiB, true
	case strings.HasSuffix(spec, "G"):
		n, err := strconv.Atoi(strings.TrimSuffix(spec, "G"))
		if err != nil {
			panic("prop: bad size " + spec)
		}
		return int64(n) * 1024 * propMiB, true
	}
	panic("prop: bad size " + spec)
}

// propRenderScript renders the storage script for one step. The script finds
// each disk by size in the live inventory and emits the layout JSON the agent
// consumes. It also prints the inventory it saw to stderr, which lands on the
// serial console for the harness to read.
func propRenderScript(c propCase, st propStep) string {
	sizes := map[string]int64{}
	for _, d := range c.Disks {
		sizes[d.ID] = d.Bytes
	}
	spec, err := json.Marshal(map[string]any{"sizes": sizes, "layout": st.Layout})
	if err != nil {
		panic(err)
	}
	return `#!/usr/bin/env python3
import json, os, sys

SPEC = json.loads(r'''` + string(spec) + `''')
with open(os.environ["SLAGWORKS_INVENTORY"]) as f:
    inv = json.load(f)

by_size = {}
for d in inv["disks"]:
    by_size.setdefault(d["size_bytes"], []).append(d["name"])
sys.stderr.write("PROPINV " + json.dumps(
    [[d["name"], d["size_bytes"], d.get("serial", ""), d.get("transport", "")] for d in inv["disks"]]) + "\n")

def dname(i):
    names = by_size.get(SPEC["sizes"][i], [])
    if len(names) != 1:
        sys.stderr.write("PROPSCRIPT cannot resolve disk %s: %s\n" % (i, names))
        sys.exit(1)
    return names[0]

def pname(disk, n):
    return "%sp%d" % (disk, n) if disk[-1].isdigit() else "%s%d" % (disk, n)

MIB = 1024 * 1024
layout, mounts = {}, {}
for dl in SPEC["layout"]["disks"]:
    dn = dname(dl["disk"])
    parts = []
    for i, p in enumerate(dl.get("parts") or [], 1):
        size = p["size"]
        if size.startswith("pct:"):
            size = "%dM" % (SPEC["sizes"][dl["disk"]] * int(size[4:]) // 100 // MIB)
        e = {"name": pname(dn, i), "size": size}
        if p.get("fs"):
            e["fs"] = p["fs"]
        if p.get("esp"):
            e["bootable"] = True
        if p.get("bios_grub"):
            e["bios_grub"] = True
        parts.append(e)
        if p.get("mount"):
            mounts[p["mount"]] = {"device": pname(dn, i)}
    dev = {"type": "disk", "ptable": "gpt", "partitions": parts}
    if any(p.get("esp") for p in dl.get("parts") or []):
        dev["boot"] = True
    layout[dn] = dev
for a in SPEC["layout"].get("arrays") or []:
    members = []
    for m in a["members"]:
        dn = dname(m["disk"])
        members.append(dn if m["part"] == 0 else pname(dn, m["part"]))
    dev = {"type": a["type"], "level": a["level"], "members": members}
    if a.get("fs"):
        dev["fs"] = a["fs"]
    if a.get("uuid"):
        dev["array_uuid"] = a["uuid"]
    layout[a["name"]] = dev
    if a.get("mount"):
        mounts[a["mount"]] = {"device": a["name"]}
for r in SPEC["layout"].get("retain") or []:
    layout[dname(r)] = {"type": "retain"}
json.dump({"layout": layout, "mounts": mounts}, sys.stdout)
`
}

// ---- what the booted target reports ----

type propBlk struct {
	KName, PKName, Type          string
	Size                         int64
	FSType, UUID                 string
	PartType, PartLabel, MountPt string
	LogSec                       int64
}

type propMD struct {
	Name, Level, State, Sync, UUID string
	RaidDisks                      int
	Degraded                       string
	Slaves                         []string
}

type propInspect struct {
	Raw      string
	Sections map[string]string
	Devs     map[string]*propBlk
	MDs      map[string]*propMD
	Sigs     map[string][]string // kname -> signature types wipefs lists
	Mounts   map[string]string   // mount point -> source device
}

// propInspectScript runs in the installed system. Every section is plain text
// under a "@@@ name" header so a truncated or garbled reply fails parsing
// loudly instead of yielding a half-read tree.
const propInspectScript = `
export LC_ALL=C PATH=/usr/sbin:/usr/bin:/sbin:/bin
echo "@@@ state"; timeout 120 systemctl is-system-running --wait; systemctl --failed --no-legend --plain
udevadm settle --timeout=30
echo "@@@ lsblk"; lsblk -J -b -l -o KNAME,PKNAME,TYPE,SIZE,FSTYPE,UUID,PARTTYPE,PARTLABEL,MOUNTPOINT,LOG-SEC
echo "@@@ mounts"; cat /proc/self/mounts
echo "@@@ fstab"; cat /etc/fstab
echo "@@@ mdstat"; cat /proc/mdstat
echo "@@@ md"
for m in /sys/block/md*; do
  [ -d "$m/md" ] || continue
  n=$(basename "$m")
  exp=$(mdadm --detail --export "/dev/$n" 2>/dev/null)
  echo "MD $n level=$(cat $m/md/level) raid_disks=$(cat $m/md/raid_disks) degraded=$(cat $m/md/degraded 2>/dev/null) state=$(cat $m/md/array_state) sync=$(cat $m/md/sync_action 2>/dev/null) uuid=$(echo "$exp" | sed -n 's/^MD_UUID=//p') slaves=$(ls $m/slaves | tr '\n' ',')"
done
echo "@@@ wipefs"
for d in $(lsblk -n -l -o KNAME | sort -u); do
  echo "W $d $(wipefs -J /dev/$d 2>/dev/null | tr -d '\n ')"
done
echo "@@@ blkidp"
for d in $(lsblk -d -n -l -o KNAME); do
  out=$(blkid -p /dev/$d 2>&1); echo "B $d rc=$? $out"
done
echo "@@@ cmdline"; cat /proc/cmdline
echo "@@@ root"; findmnt -n -o SOURCE,FSTYPE,UUID /
echo "@@@ swaps"; cat /proc/swaps
echo "@@@ mdconf"; cat /etc/mdadm/mdadm.conf 2>&1
echo "@@@ df"; df -B1 --output=source,size,used,target /
echo "@@@ efi"; efibootmgr 2>&1 | head -40
echo "@@@ growth"
grep -h -i -E "growpart|resizefs|resize2fs|xfs_growfs" /var/log/cloud-init.log /var/log/cloud-init-output.log 2>/dev/null | tail -12
echo "@@@ kmsg"
dmesg | grep -i -E "recovery|unclean|not clean|has not been unmounted" | head -20
echo "@@@ unitlogs"
for u in $(systemctl --failed --no-legend --plain | awk '{print $1}'); do
  journalctl -b -u "$u" --no-pager -n 12 2>&1
done
echo "@@@ end"
`

func propParseInspect(raw string) (*propInspect, error) {
	ins := &propInspect{
		Raw:      raw,
		Sections: map[string]string{},
		Devs:     map[string]*propBlk{},
		MDs:      map[string]*propMD{},
		Sigs:     map[string][]string{},
		Mounts:   map[string]string{},
	}
	var cur string
	var buf []string
	flush := func() {
		if cur != "" {
			ins.Sections[cur] = strings.Join(buf, "\n")
		}
		buf = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		if name, ok := strings.CutPrefix(line, "@@@ "); ok {
			flush()
			cur = strings.TrimSpace(name)
			continue
		}
		buf = append(buf, line)
	}
	flush()
	if _, ok := ins.Sections["end"]; !ok {
		return nil, fmt.Errorf("inspection output has no end marker (truncated?)")
	}

	var doc struct {
		Blockdevices []map[string]any `json:"blockdevices"`
	}
	if err := json.Unmarshal([]byte(ins.Sections["lsblk"]), &doc); err != nil {
		return nil, fmt.Errorf("parse lsblk JSON: %w", err)
	}
	str := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case float64:
			return strconv.FormatInt(int64(x), 10)
		}
		return ""
	}
	num := func(v any) int64 {
		switch x := v.(type) {
		case float64:
			return int64(x)
		case string:
			n, _ := strconv.ParseInt(x, 10, 64)
			return n
		}
		return 0
	}
	for _, m := range doc.Blockdevices {
		b := &propBlk{
			KName: str(m["kname"]), PKName: str(m["pkname"]), Type: str(m["type"]),
			Size: num(m["size"]), FSType: str(m["fstype"]), UUID: str(m["uuid"]),
			PartType: strings.ToLower(str(m["parttype"])), PartLabel: str(m["partlabel"]),
			MountPt: str(m["mountpoint"]), LogSec: num(m["log-sec"]),
		}
		if prev, ok := ins.Devs[b.KName]; ok {
			// An md device is listed once per member; keep one row.
			if prev.MountPt == "" {
				prev.MountPt = b.MountPt
			}
			continue
		}
		ins.Devs[b.KName] = b
	}

	for _, line := range strings.Split(ins.Sections["md"], "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "MD" {
			continue
		}
		md := &propMD{Name: f[1]}
		for _, kv := range f[2:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "level":
				md.Level = v
			case "raid_disks":
				md.RaidDisks, _ = strconv.Atoi(v)
			case "degraded":
				md.Degraded = v
			case "state":
				md.State = v
			case "sync":
				md.Sync = v
			case "uuid":
				md.UUID = v
			case "slaves":
				for _, s := range strings.Split(v, ",") {
					if s != "" {
						md.Slaves = append(md.Slaves, s)
					}
				}
			}
		}
		sort.Strings(md.Slaves)
		ins.MDs[md.Name] = md
	}

	for _, line := range strings.Split(ins.Sections["wipefs"], "\n") {
		f := strings.SplitN(line, " ", 3)
		if len(f) < 2 || f[0] != "W" {
			continue
		}
		ins.Sigs[f[1]] = nil
		if len(f) < 3 || f[2] == "" {
			continue
		}
		var w struct {
			Signatures []struct {
				Type string `json:"type"`
			} `json:"signatures"`
		}
		if err := json.Unmarshal([]byte(f[2]), &w); err != nil {
			return nil, fmt.Errorf("parse wipefs JSON for %s: %w (%q)", f[1], err, f[2])
		}
		seen := map[string]bool{}
		for _, s := range w.Signatures {
			if !seen[s.Type] {
				seen[s.Type] = true
				ins.Sigs[f[1]] = append(ins.Sigs[f[1]], s.Type)
			}
		}
		sort.Strings(ins.Sigs[f[1]])
	}

	for _, line := range strings.Split(ins.Sections["mounts"], "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && strings.HasPrefix(f[0], "/dev/") {
			ins.Mounts[f[1]] = strings.TrimPrefix(f[0], "/dev/")
		}
	}
	return ins, nil
}

// diskKName finds the kernel name of a case disk in the booted target by size.
func (ins *propInspect) diskKName(bytes int64) string {
	var found []string
	for _, b := range ins.Devs {
		if b.Type == "disk" && b.Size == bytes {
			found = append(found, b.KName)
		}
	}
	if len(found) != 1 {
		return ""
	}
	return found[0]
}

// partsOf returns the partitions of a disk ordered by partition number.
func (ins *propInspect) partsOf(disk string) []*propBlk {
	var out []*propBlk
	for _, b := range ins.Devs {
		if b.Type == "part" && b.PKName == disk {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return propPartNum(disk, out[i].KName) < propPartNum(disk, out[j].KName) })
	return out
}

func propPartNum(disk, part string) int {
	s := strings.TrimPrefix(strings.TrimPrefix(part, disk), "p")
	n, _ := strconv.Atoi(s)
	return n
}

func propPartKName(disk string, n int) string {
	if disk != "" && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

var propPartTypeGUID = map[string]string{
	"EF02": "21686148-6449-6e6f-744e-656564454649",
	"EF00": "c12a7328-f81f-11d2-ba4b-00a0c93ec93b",
	"8200": "0657fd6d-a4ab-43c4-84e5-0933c84b4f4f",
	"8300": "0fc63daf-8483-4772-8e79-3d69d8477de4",
}

func propHex(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// propTreeReport is the verdict of the tree oracle. Hard entries are
// violations of what the layout asked for. Soft entries are observations that
// need a human: leftover signatures under a valid one, arrays assembled from
// disks the layout did not name, units that failed at boot.
type propTreeReport struct {
	Hard []string
	Soft []string
	// Known entries are violations that match a finding already written up
	// (see storage_findings_prop_integration_test.go). They are logged, not
	// failed, so a soak run shows only what is new.
	Known []string
	// MountDev maps each layout mount point to the kernel device expected
	// there, for the fstab and canary checks.
	MountDev map[string]string
}

func (r *propTreeReport) hard(format string, a ...any) {
	r.Hard = append(r.Hard, fmt.Sprintf(format, a...))
}
func (r *propTreeReport) soft(format string, a ...any) {
	r.Soft = append(r.Soft, fmt.Sprintf(format, a...))
}
func (r *propTreeReport) known(format string, a ...any) {
	r.Known = append(r.Known, fmt.Sprintf(format, a...))
}

func propSameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// propCheckTree compares what the installed system reports with what the
// step's layout asked for.
func propCheckTree(c propCase, st propStep, ins *propInspect) *propTreeReport {
	rep := &propTreeReport{MountDev: map[string]string{}}

	kname := map[string]string{} // disk ID -> kernel name
	for _, id := range st.Attach {
		k := ins.diskKName(c.disk(id).Bytes)
		if k == "" {
			rep.hard("disk %s (%d bytes) not found exactly once in the target", id, c.disk(id).Bytes)
			continue
		}
		kname[id] = k
	}
	if len(rep.Hard) > 0 {
		return rep
	}
	devOf := func(m propMember) string {
		if m.Part == 0 {
			return kname[m.Disk]
		}
		return propPartKName(kname[m.Disk], m.Part)
	}

	member := map[string]bool{} // kname -> is a member of a layout array
	for _, a := range st.Layout.Arrays {
		for _, m := range a.Members {
			member[devOf(m)] = true
		}
	}
	inLayout := map[string]bool{} // disk IDs the layout names in any role
	claimed := map[string]bool{}  // knames of every device on a layout disk
	wantSig := map[string][]string{}

	for _, dl := range st.Layout.Disks {
		inLayout[dl.Disk] = true
		dk := kname[dl.Disk]
		claimed[dk] = true
		d := c.disk(dl.Disk)
		parts := ins.partsOf(dk)
		if len(parts) != len(dl.Parts) {
			rep.hard("disk %s (%s): %d partitions, layout asks for %d", dl.Disk, dk, len(parts), len(dl.Parts))
			continue
		}
		switch {
		case len(dl.Parts) > 0:
			wantSig[dk] = []string{"PMBR", "gpt"}
		case member[dk]:
			wantSig[dk] = []string{"linux_raid_member"}
		default:
			wantSig[dk] = nil
		}
		used := propMiB // the first partition starts at 1 MiB
		for i, p := range dl.Parts {
			got := parts[i]
			pk := propPartKName(dk, i+1)
			claimed[pk] = true
			if got.KName != pk {
				rep.hard("disk %s: partition %d is %s, want %s", dl.Disk, i+1, got.KName, pk)
				continue
			}
			if want, fixed := propSizeBytes(p.Size, d.Bytes); fixed {
				grown := d.Bytes - used - got.Size
				switch {
				case got.Size == want:
				case p.Mount == "/" && i == len(dl.Parts)-1 && grown >= 0 && grown <= 2*propMiB:
					// Known finding: cloud-init's growpart grows a root
					// partition into the free space after it on first boot.
					rep.known("growpart: %s is %d bytes, layout asks for %s = %d; the root partition was grown to the end of the disk",
						pk, got.Size, p.Size, want)
				default:
					rep.hard("%s: size %d, layout asks for %s = %d", pk, got.Size, p.Size, want)
				}
				used += want
			} else {
				want := d.Bytes - used
				if diff := want - got.Size; diff < 0 || diff > 2*propMiB {
					rep.hard("%s: size %d, layout asks for the rest of the disk = about %d", pk, got.Size, want)
				}
				used = d.Bytes
			}
			code := "8300"
			switch {
			case p.BIOSGrub:
				code = "EF02"
			case p.ESP:
				code = "EF00"
			case p.FS == "swap":
				code = "8200"
			}
			if got.PartType != propPartTypeGUID[code] {
				rep.hard("%s: partition type %s, want %s (%s)", pk, got.PartType, propPartTypeGUID[code], code)
			}
			if got.PartLabel != pk && i == 0 {
				rep.soft("renamed: %s was %q when it was deployed (kernel names moved between the deploy boot and this one)", pk, got.PartLabel)
			}
			wantFS := p.FS
			if member[pk] {
				wantFS = "linux_raid_member"
			}
			stale := map[string]bool{"vfat": true, "ext4": true, "xfs": true, "swap": true}
			switch {
			case got.FSType == wantFS:
			case wantFS == "" && stale[got.FSType]:
				// Known finding: the stale-state wipe does not reach inside
				// partitions, so a partition the layout leaves unformatted
				// shows whatever filesystem last started at its offset.
				rep.known("stale-fs: %s shows a %s filesystem, layout leaves it unformatted", pk, got.FSType)
			default:
				rep.hard("%s: filesystem %q, layout asks for %q", pk, got.FSType, wantFS)
			}
			if wantFS == "" {
				wantSig[pk] = nil
			} else {
				wantSig[pk] = []string{wantFS}
			}
			if p.Mount != "" {
				rep.MountDev[p.Mount] = pk
			}
		}
	}
	for _, id := range st.Layout.Retain {
		inLayout[id] = true
	}

	// Arrays: every layout array must exist with exactly its members; no
	// other array may hold a device from a layout disk.
	matched := map[string]bool{}
	for _, a := range st.Layout.Arrays {
		var want []string
		for _, m := range a.Members {
			want = append(want, devOf(m))
			inLayout[m.Disk] = true
		}
		var md *propMD
		for _, cand := range ins.MDs {
			if propSameSet(cand.Slaves, want) {
				md = cand
			}
		}
		if md == nil {
			rep.hard("array %s: no md device has exactly members %v (mdstat: %s)", a.Name, want,
				strings.Join(strings.Fields(ins.Sections["mdstat"]), " "))
			continue
		}
		matched[md.Name] = true
		if md.Level != fmt.Sprintf("raid%d", a.Level) {
			rep.hard("array %s (%s): level %s, want raid%d", a.Name, md.Name, md.Level, a.Level)
		}
		if md.RaidDisks != len(a.Members) {
			rep.hard("array %s (%s): raid_disks %d, want %d", a.Name, md.Name, md.RaidDisks, len(a.Members))
		}
		// mdadm builds a new RAID5 degraded and rebuilds the last member, so
		// degraded with a rebuild running is the healthy state of a young
		// array. Degraded and idle is a missing member.
		// (A read-auto array has not started the rebuild yet.)
		if md.Degraded != "" && md.Degraded != "0" && md.Sync != "recover" && md.Sync != "resync" && md.State != "read-auto" {
			rep.hard("array %s (%s): degraded=%s sync=%s", a.Name, md.Name, md.Degraded, md.Sync)
		}
		switch md.State {
		case "clean", "active", "write-pending", "active-idle":
		case "read-auto":
			if a.Mount != "" {
				rep.hard("array %s (%s): state read-auto although it is mounted", a.Name, md.Name)
			}
		default:
			rep.hard("array %s (%s): state %s", a.Name, md.Name, md.State)
		}
		if a.UUID != "" && propHex(md.UUID) != propHex(a.UUID) {
			rep.hard("array %s (%s): UUID %s, layout pins %s", a.Name, md.Name, md.UUID, a.UUID)
		}
		blk := ins.Devs[md.Name]
		if blk == nil {
			rep.hard("array %s: %s missing from lsblk", a.Name, md.Name)
			continue
		}
		if blk.FSType != a.FS {
			rep.hard("array %s (%s): filesystem %q, layout asks for %q", a.Name, md.Name, blk.FSType, a.FS)
		}
		if a.FS == "" {
			wantSig[md.Name] = nil
		} else {
			wantSig[md.Name] = []string{a.FS}
		}
		if a.Mount != "" {
			rep.MountDev[a.Mount] = md.Name
		}
	}
	for name, md := range ins.MDs {
		if matched[name] {
			continue
		}
		onLayout := false
		for _, s := range md.Slaves {
			if claimed[s] {
				onLayout = true
			}
		}
		switch {
		case onLayout:
			rep.hard("leftover array %s (%s, state %s) holds devices of layout disks: %v", name, md.Level, md.State, md.Slaves)
		case st.WipeAll:
			rep.hard("leftover array %s (%s, state %s) on %v survived --wipe-all-disks", name, md.Level, md.State, md.Slaves)
		default:
			rep.soft("array %s (%s, state %s) assembled from disks outside the layout: %v", name, md.Level, md.State, md.Slaves)
		}
	}

	// Mounts and fstab.
	for mp, dev := range rep.MountDev {
		if got := ins.Mounts[mp]; got != dev {
			rep.hard("mount %s: source %q, want %s", mp, got, dev)
		}
	}
	for dev, b := range ins.Devs {
		if !claimed[dev] && !matched[dev] {
			continue
		}
		if b.MountPt == "" || b.MountPt == "[SWAP]" {
			continue
		}
		if rep.MountDev[b.MountPt] != dev {
			rep.hard("%s is mounted at %s, which the layout does not ask for", dev, b.MountPt)
		}
	}
	fstab := map[string][]string{}
	for _, line := range strings.Split(ins.Sections["fstab"], "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.HasPrefix(f[0], "#") {
			continue
		}
		fstab[f[1]] = f
	}
	if len(fstab) != len(rep.MountDev) {
		rep.hard("fstab has %d entries, layout has %d mounts: %q", len(fstab), len(rep.MountDev), ins.Sections["fstab"])
	}
	for mp, dev := range rep.MountDev {
		f, ok := fstab[mp]
		if !ok {
			rep.hard("fstab has no entry for %s", mp)
			continue
		}
		b := ins.Devs[dev]
		if b == nil {
			continue
		}
		if f[0] != "UUID="+b.UUID {
			rep.hard("fstab %s: %s, but %s has UUID %s", mp, f[0], dev, b.UUID)
		}
		if f[2] != b.FSType {
			rep.hard("fstab %s: type %s, but %s is %s", mp, f[2], dev, b.FSType)
		}
	}
	if root := strings.Fields(ins.Sections["root"]); len(root) == 0 || strings.TrimPrefix(root[0], "/dev/") != rep.MountDev["/"] {
		rep.hard("root filesystem is on %q, want %s", ins.Sections["root"], rep.MountDev["/"])
	}

	// Signatures. A device the layout formats must carry that signature and
	// no other; one it leaves raw must carry none.
	for dev, want := range wantSig {
		got, listed := ins.Sigs[dev]
		if !listed {
			rep.soft("%s: wipefs did not list it", dev)
			continue
		}
		if propSameSet(got, want) {
			continue
		}
		// A --destructive deploy wipes its target disks first ("clear
		// stale RAID superblocks and filesystem signatures from the disks
		// this layout will write"), so a foreign signature on the whole
		// disk afterwards is a miss. Under a partition's own signature it
		// is shadowed and reported for a human to weigh.
		if b := ins.Devs[dev]; st.Destructive && b != nil && b.Type == "disk" {
			rep.hard("stale-signature: whole disk %s carries %v after a --destructive deploy, layout implies %v", dev, got, want)
			continue
		}
		if len(want) == 0 {
			continue // an unformatted partition: reported above as stale-fs
		}
		rep.soft("stale-signature: %s carries %v, layout implies %v", dev, got, want)
	}

	// Disks the layout does not name.
	for _, id := range st.Attach {
		if inLayout[id] {
			continue
		}
		dk := kname[id]
		if !st.WipeAll {
			continue
		}
		if n := len(ins.partsOf(dk)); n != 0 {
			rep.hard("bystander %s (%s) still has %d partitions after --wipe-all-disks", id, dk, n)
		}
		if sigs := ins.Sigs[dk]; len(sigs) != 0 {
			rep.hard("bystander %s (%s) still carries %v after --wipe-all-disks", id, dk, sigs)
		}
	}

	// The agent unmounts every filesystem it wrote before it reboots, so the
	// first boot must not find one in need of journal recovery. (Known
	// finding: it can leave the root mounted; see the uefi-root-left-mounted
	// case.)
	for _, line := range strings.Split(ins.Sections["kmsg"], "\n") {
		if strings.Contains(line, "recovery complete") || strings.Contains(line, "Starting recovery") {
			rep.known("unclean-root: the first boot replayed a journal: %s", strings.TrimSpace(line))
		}
	}

	// Units that failed at boot. Two fail on every system this image
	// installs, whatever the storage: cloud-config.service always, and
	// mdmonitor.service whenever an array exists. They are reported once in
	// the campaign notes; anything else is worth a look per case.
	state := strings.Split(strings.TrimSpace(ins.Sections["state"]), "\n")
	if len(state) == 0 || strings.TrimSpace(state[0]) != "running" {
		var other []string
		for _, line := range state[1:] {
			f := strings.Fields(line)
			if len(f) == 0 || f[0] == "cloud-config.service" || f[0] == "mdmonitor.service" {
				continue
			}
			other = append(other, f[0])
		}
		if len(other) > 0 || len(state) < 2 {
			rep.soft("boot-units: state %s, failed: %v", strings.TrimSpace(state[0]), other)
		}
	}
	sort.Strings(rep.Hard)
	sort.Strings(rep.Soft)
	return rep
}
