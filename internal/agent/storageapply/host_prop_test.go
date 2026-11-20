//go:build !integration

package storageapply

// fakeHost is an agent.Runner that models a deploy target's block layer:
// whole disks, the kernel's partition view, GPT tables, signatures
// (partition tables, filesystems, md superblocks, DDF containers), active
// md arrays (native, external containers and their subarrays),
// /dev/disk/by-id symlinks, and mounts. It answers the exact commands the
// agent's storage path runs (lsblk, blkid, mdadm, wipefs, dd, sgdisk,
// blockdev, mkfs, mkdir, mount, cat /proc/mdstat) with the semantics of
// the real tools, and records every physical disk any command writes.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type hDev struct {
	sigs   []string // "gpt", "ext4", "xfs", "vfat", "swap", "md:<uuid>", "ddf"
	fsUUID string
}

func (d *hDev) mdUUID() string {
	for _, s := range d.sigs {
		if strings.HasPrefix(s, "md:") {
			return strings.TrimPrefix(s, "md:")
		}
	}
	return ""
}

func (d *hDev) dropMD() bool {
	var kept []string
	dropped := false
	for _, s := range d.sigs {
		if strings.HasPrefix(s, "md:") {
			dropped = true
			continue
		}
		kept = append(kept, s)
	}
	d.sigs = kept
	return dropped
}

type hDisk struct {
	name       string
	bytes      int64
	sector     int64 // logical sector size
	tran       string
	model      string
	whole      *hDev
	table      map[int]bool  // on-disk GPT entries
	kernel     map[int]*hDev // partitions the kernel exposes
	orphan     map[int]*hDev // contents of partitions dropped from the table
	unreadable bool          // probing its signatures fails with an I/O error
}

type hArray struct {
	name      string // md127
	members   []string
	uuid      string
	container bool
	parent    string // container of a subarray
	fs        string
	fsUUID    string
}

type fakeHost struct {
	disks     map[string]*hDisk
	links     map[string]string // /dev/disk/by-id/... -> kernel name
	arrays    map[string]*hArray
	mounted   map[string]bool
	writes    map[string]bool
	calls     []string
	preserved map[string]hArray // uuid -> filesystem the array carries
	gen       int
	// noWipefs simulates wipefs missing from the deploy image.
	noWipefs bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		disks:     map[string]*hDisk{},
		links:     map[string]string{},
		arrays:    map[string]*hArray{},
		mounted:   map[string]bool{},
		writes:    map[string]bool{},
		preserved: map[string]hArray{},
	}
}

func (h *fakeHost) addDisk(name string, bytes int64, tran string) *hDisk {
	d := &hDisk{name: name, bytes: bytes, sector: 512, tran: tran, whole: &hDev{},
		table: map[int]bool{}, kernel: map[int]*hDev{}, orphan: map[int]*hDev{}}
	h.disks[name] = d
	return d
}

// addPartition creates a partition already known to the kernel.
func (h *fakeHost) addPartition(disk string, num int, sigs ...string) *hDev {
	d := h.disks[disk]
	d.table[num] = true
	h.gen++
	dev := &hDev{sigs: append([]string{}, sigs...), fsUUID: fmt.Sprintf("fs-%d", h.gen)}
	d.kernel[num] = dev
	if !hasSig(d.whole.sigs, "gpt") {
		d.whole.sigs = append(d.whole.sigs, "gpt")
	}
	return dev
}

func hasSig(sigs []string, s string) bool {
	for _, x := range sigs {
		if x == s {
			return true
		}
	}
	return false
}

func (h *fakeHost) Available(string) bool { return true }

type exitErr struct{ code int }

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(e.code) }

// ref is a resolved device: whole disk (part 0), partition, or md array.
type ref struct {
	disk string
	part int
	md   string
}

func (h *fakeHost) resolve(p string) (ref, *hDev, bool) {
	name := strings.TrimPrefix(p, "/dev/")
	if t, ok := h.links[p]; ok {
		name = t
	}
	if a, ok := h.arrays[name]; ok {
		return ref{md: a.name}, nil, true
	}
	if d, ok := h.disks[name]; ok {
		return ref{disk: name}, d.whole, true
	}
	for _, d := range h.disks {
		for num, dev := range d.kernel {
			if kernelPartName(d.name, num) == name {
				return ref{disk: d.name, part: num}, dev, true
			}
		}
	}
	return ref{}, nil, false
}

func (r ref) kname() string {
	switch {
	case r.md != "":
		return r.md
	case r.part == 0:
		return r.disk
	}
	return kernelPartName(r.disk, r.part)
}

// holder returns the active array holding a device, if any.
func (h *fakeHost) holder(kname string) *hArray {
	for _, a := range h.arrays {
		for _, m := range a.members {
			if m == kname {
				return a
			}
		}
	}
	return nil
}

func (h *fakeHost) busy(r ref) bool {
	if h.mounted[r.kname()] {
		return true
	}
	if r.md != "" {
		for _, a := range h.arrays {
			if a.parent == r.md {
				return true
			}
		}
		return false
	}
	if h.holder(r.kname()) != nil {
		return true
	}
	if r.part == 0 {
		for num := range h.disks[r.disk].kernel {
			if h.busy(ref{disk: r.disk, part: num}) {
				return true
			}
		}
	}
	return false
}

func (h *fakeHost) markWrite(r ref) {
	if r.md != "" {
		for _, m := range h.arrays[r.md].members {
			mr, _, _ := h.resolve("/dev/" + m)
			h.markWrite(mr)
		}
		return
	}
	h.writes[r.disk] = true
}

func (h *fakeHost) mdstat() string {
	var b strings.Builder
	b.WriteString("Personalities : [raid0] [raid1] [raid10] [raid6] [raid5] [raid4]\n")
	names := make([]string, 0, len(h.arrays))
	for n := range h.arrays {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := h.arrays[n]
		var mem []string
		for i, m := range a.members {
			mem = append(mem, fmt.Sprintf("%s[%d]", m, i))
		}
		fmt.Fprintf(&b, "%s : active raid1 %s\n", n, strings.Join(mem, " "))
		switch {
		case a.container:
			b.WriteString("      527704 blocks super external:ddf\n\n")
		case a.parent != "":
			fmt.Fprintf(&b, "      937164800 blocks super external:/%s/0\n\n", a.parent)
		default:
			b.WriteString("      937032704 blocks super 1.2 [2/2] [UU]\n\n")
		}
	}
	b.WriteString("unused devices: <none>\n")
	return b.String()
}

func (h *fakeHost) Output(name string, args ...string) (string, error) {
	h.calls = append(h.calls, name+" "+strings.Join(args, " "))
	j := strings.Join(args, " ")
	switch {
	case name == "lsblk" && j == "-Jb -d -o NAME,SIZE,TRAN,MODEL,LOG-SEC":
		type row struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Tran   string `json:"tran"`
			Model  string `json:"model"`
			LogSec int64  `json:"log-sec"`
		}
		var rows []row
		for _, n := range h.sortedDisks() {
			d := h.disks[n]
			rows = append(rows, row{n, d.bytes, d.tran, d.model, d.sector})
		}
		out, _ := json.Marshal(map[string]any{"blockdevices": rows})
		return string(out), nil
	case name == "lsblk" && j == "-dnlo NAME,TYPE":
		var b strings.Builder
		for _, n := range h.sortedDisks() {
			fmt.Fprintf(&b, "%s disk\n", n)
		}
		for n := range h.arrays {
			fmt.Fprintf(&b, "%s raid1\n", n)
		}
		return strings.TrimSpace(b.String()), nil
	case name == "lsblk" && len(args) == 3 && args[0] == "-nlo" && args[1] == "NAME":
		r, _, ok := h.resolve(args[2])
		if !ok || r.md != "" || r.part != 0 {
			return "", exitErr{32}
		}
		d := h.disks[r.disk]
		lines := []string{d.name}
		if a := h.holder(d.name); a != nil {
			lines = append(lines, a.name)
		}
		nums := make([]int, 0, len(d.kernel))
		for n := range d.kernel {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			pn := kernelPartName(d.name, n)
			lines = append(lines, pn)
			if a := h.holder(pn); a != nil {
				lines = append(lines, a.name)
			}
		}
		return strings.Join(lines, "\n"), nil
	case name == "lsblk" && len(args) == 3 && args[0] == "-snlo" && args[1] == "NAME,TYPE":
		// Inverse listing: the device, then everything beneath it.
		r, _, ok := h.resolve(args[2])
		if !ok {
			return "", exitErr{32}
		}
		return strings.Join(h.inverse(r), "\n"), nil
	case name == "wipefs" && len(args) == 4 && args[0] == "--noheadings" && args[1] == "--output" && args[2] == "TYPE,OFFSET":
		if h.noWipefs {
			return "", fmt.Errorf("exec: \"wipefs\": executable file not found in $PATH")
		}
		r, dev, ok := h.resolve(args[3])
		if !ok || r.md != "" {
			return "", exitErr{1}
		}
		if r.part == 0 && h.disks[r.disk].unreadable {
			return "", exitErr{1}
		}
		var lines []string
		for i, sig := range dev.sigs {
			typ := sig
			if strings.HasPrefix(sig, "md:") {
				typ = "linux_raid_member"
			}
			lines = append(lines, fmt.Sprintf("%s 0x%x", typ, 0x200*(i+1)))
		}
		return strings.Join(lines, "\n"), nil
	case name == "dmesg":
		return "", nil
	case name == "blkid" && len(args) == 4 && args[0] == "-s" && args[1] == "UUID":
		r, dev, ok := h.resolve(args[3])
		if !ok {
			return "", exitErr{2}
		}
		if r.md != "" {
			return h.arrays[r.md].fsUUID, nil
		}
		return dev.fsUUID, nil
	case name == "cat" && j == "/proc/mdstat":
		return h.mdstat(), nil
	case name == "mdadm" && len(args) == 2 && args[0] == "--detail":
		r, _, ok := h.resolve(args[1])
		if !ok || r.md == "" {
			return "", exitErr{1}
		}
		u := h.arrays[r.md].uuid
		return fmt.Sprintf("        Version : 1.2\n           UUID : %s:%s:%s:%s\n", u[0:8], u[8:16], u[16:24], u[24:32]), nil
	}
	return "", fmt.Errorf("fakeHost: unmodelled Output %s %s", name, j)
}

// inverse lists a device and every device beneath it, as `lsblk -s` does.
func (h *fakeHost) inverse(r ref) []string {
	switch {
	case r.md != "":
		lines := []string{r.md + " raid1"}
		for _, m := range h.arrays[r.md].members {
			if mr, _, ok := h.resolve("/dev/" + m); ok {
				lines = append(lines, h.inverse(mr)...)
			}
		}
		return lines
	case r.part != 0:
		return []string{r.kname() + " part", r.disk + " disk"}
	}
	return []string{r.disk + " disk"}
}

func (h *fakeHost) sortedDisks() []string {
	names := make([]string, 0, len(h.disks))
	for n := range h.disks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (h *fakeHost) Run(name string, args ...string) error {
	h.calls = append(h.calls, name+" "+strings.Join(args, " "))
	j := strings.Join(args, " ")
	switch name {
	case "mdadm":
		switch {
		case j == "--stop --scan":
			// One pass over native arrays; external-metadata (DDF/IMSM)
			// subarrays and containers survive it (mdmon holds them).
			for n, a := range h.arrays {
				if !a.container && a.parent == "" && !h.mounted[n] {
					delete(h.arrays, n)
				}
			}
			return nil
		case len(args) == 2 && args[0] == "--stop":
			r, _, ok := h.resolve(args[1])
			if !ok || r.md == "" {
				return exitErr{1}
			}
			if h.busy(r) {
				return exitErr{1}
			}
			delete(h.arrays, r.md)
			return nil
		case len(args) == 3 && args[0] == "--zero-superblock" && args[1] == "--force":
			r, dev, ok := h.resolve(args[2])
			if !ok || r.md != "" {
				return exitErr{1}
			}
			if h.holder(r.kname()) != nil {
				return exitErr{1}
			}
			if !dev.dropMD() {
				return exitErr{1} // "Unrecognised md component device"
			}
			h.markWrite(r)
			return nil
		case len(args) >= 4 && args[0] == "--assemble" && args[2] == "--uuid":
			mdName := strings.TrimPrefix(args[1], "/dev/")
			if _, active := h.arrays[mdName]; active {
				return exitErr{1}
			}
			want := normalizeUUIDHex(args[3])
			var members []string
			for _, m := range args[4:] {
				r, dev, ok := h.resolve(m)
				if !ok || r.md != "" || h.busy(r) || normalizeUUIDHex(dev.mdUUID()) != want {
					return exitErr{1}
				}
				members = append(members, r.kname())
			}
			p := h.preserved[want]
			h.arrays[mdName] = &hArray{name: mdName, members: members, uuid: want, fs: p.fs, fsUUID: p.fsUUID}
			return nil
		case len(args) >= 2 && args[0] == "--create":
			mdName := strings.TrimPrefix(args[1], "/dev/")
			if _, active := h.arrays[mdName]; active {
				return exitErr{1}
			}
			uuid := ""
			var members []string
			for i := 2; i < len(args); i++ {
				switch {
				case args[i] == "--uuid":
					i++
					uuid = normalizeUUIDHex(args[i])
				case strings.HasPrefix(args[i], "--"):
				default:
					members = append(members, args[i])
				}
			}
			if uuid == "" {
				h.gen++
				uuid = fmt.Sprintf("%032x", 0xc0ffee0000+h.gen)
			}
			var refs []ref
			for _, m := range members {
				r, _, ok := h.resolve(m)
				if !ok || r.md != "" || h.busy(r) {
					return exitErr{1}
				}
				refs = append(refs, r)
			}
			var knames []string
			for _, r := range refs {
				_, dev, _ := h.resolve("/dev/" + r.kname())
				dev.sigs = []string{"md:" + uuid}
				h.markWrite(r)
				knames = append(knames, r.kname())
			}
			h.arrays[mdName] = &hArray{name: mdName, members: knames, uuid: uuid}
			return nil
		}
	case "wipefs":
		if len(args) != 2 || args[0] != "-a" {
			break
		}
		if h.noWipefs {
			return fmt.Errorf("exec: \"wipefs\": executable file not found in $PATH")
		}
		r, dev, ok := h.resolve(args[1])
		if !ok {
			return exitErr{1}
		}
		if h.busy(r) {
			return exitErr{1} // O_EXCL open fails
		}
		if r.md != "" {
			a := h.arrays[r.md]
			if a.fs != "" {
				h.markWrite(r)
			}
			a.fs, a.fsUUID = "", ""
			return nil
		}
		if r.part == 0 {
			d := h.disks[r.disk]
			if hasSig(dev.sigs, "gpt") {
				for num, pd := range d.kernel {
					d.orphan[num] = pd
				}
				d.table = map[int]bool{}
				d.kernel = map[int]*hDev{} // wipefs re-reads the emptied table
			}
		}
		if len(dev.sigs) > 0 {
			h.markWrite(r)
		}
		dev.sigs = nil
		return nil
	case "sgdisk":
		if len(args) == 2 && args[0] == "--zap-all" {
			r, dev, ok := h.resolve(args[1])
			if !ok || r.md != "" || r.part != 0 {
				return exitErr{2}
			}
			d := h.disks[r.disk]
			for num := range d.table {
				if pd, ok := d.kernel[num]; ok {
					d.orphan[num] = pd
				}
			}
			d.table = map[int]bool{}
			var kept []string
			for _, s := range dev.sigs {
				if s != "gpt" && s != "ddf" {
					kept = append(kept, s)
				}
			}
			dev.sigs = kept
			h.markWrite(r)
			return nil
		}
		if len(args) == 7 && args[0] == "-n" {
			r, dev, ok := h.resolve(args[6])
			if !ok || r.md != "" || r.part != 0 {
				return exitErr{2}
			}
			num, _ := strconv.Atoi(strings.SplitN(args[1], ":", 2)[0])
			d := h.disks[r.disk]
			d.table[num] = true
			if !hasSig(dev.sigs, "gpt") {
				dev.sigs = append(dev.sigs, "gpt")
			}
			h.markWrite(r)
			return nil
		}
	case "blockdev":
		if len(args) == 2 && args[0] == "--rereadpt" {
			r, _, ok := h.resolve(args[1])
			if !ok || r.md != "" || r.part != 0 {
				return exitErr{1}
			}
			d := h.disks[r.disk]
			if h.holder(d.name) != nil {
				return exitErr{1} // EBUSY: the whole disk is claimed (md/DDF)
			}
			for num := range d.kernel {
				if h.busy(ref{disk: d.name, part: num}) {
					return exitErr{1} // EBUSY
				}
			}
			nk := map[int]*hDev{}
			for num := range d.table {
				if pd, ok := d.kernel[num]; ok {
					nk[num] = pd
				} else if od, ok := d.orphan[num]; ok {
					nk[num] = od // old bytes at the same offset resurface
					delete(d.orphan, num)
				} else {
					nk[num] = &hDev{}
				}
			}
			d.kernel = nk
			return nil
		}
	case "mkfs.fat", "mkfs.ext4", "mkfs.xfs", "mkswap":
		r, dev, ok := h.resolve(args[len(args)-1])
		if !ok || h.busy(r) {
			return exitErr{1}
		}
		fs := map[string]string{"mkfs.fat": "vfat", "mkfs.ext4": "ext4", "mkfs.xfs": "xfs", "mkswap": "swap"}[name]
		h.gen++
		if r.md != "" {
			a := h.arrays[r.md]
			a.fs, a.fsUUID = fs, fmt.Sprintf("fs-%d", h.gen)
		} else {
			dev.sigs, dev.fsUUID = []string{fs}, fmt.Sprintf("fs-%d", h.gen)
		}
		h.markWrite(r)
		return nil
	case "dd":
		// The plan's only dd zeroes the first 4 KiB of a partition: the
		// boot sector and any filesystem magic there. An md 1.2 superblock
		// sits 4 KiB in and survives.
		if len(args) != 5 || args[0] != "if=/dev/zero" || !strings.HasPrefix(args[1], "of=") {
			break
		}
		r, dev, ok := h.resolve(strings.TrimPrefix(args[1], "of="))
		if !ok || r.md != "" || r.part == 0 {
			return exitErr{1}
		}
		var kept []string
		for _, sig := range dev.sigs {
			if strings.HasPrefix(sig, "md:") {
				kept = append(kept, sig)
			}
		}
		dev.sigs = kept
		h.markWrite(r)
		return nil
	case "mkdir":
		return nil
	case "mount":
		if len(args) != 2 {
			break
		}
		r, dev, ok := h.resolve(args[0])
		if !ok || h.busy(r) && r.md == "" && h.holder(r.kname()) != nil {
			return exitErr{32}
		}
		fs := ""
		if r.md != "" {
			fs = h.arrays[r.md].fs
		} else if len(dev.sigs) > 0 {
			fs = dev.sigs[0]
		}
		if fs == "" || fs == "swap" || strings.HasPrefix(fs, "md:") || fs == "gpt" {
			return exitErr{32}
		}
		h.mounted[r.kname()] = true
		return nil
	}
	return fmt.Errorf("fakeHost: unmodelled Run %s %s", name, j)
}
