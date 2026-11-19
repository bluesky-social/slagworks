package inventory

// Property-based coverage for the on-target inventory parsers.
//
// Each property generates a small hardware MODEL (DIMMs, disks, PCI/USB
// devices, a BMC, NICs with optics), renders it into the exact text or JSON
// shape the real tool prints (dmidecode 3.x, lsblk -Jb, lspci -nnmm, lsusb,
// ipmitool, ip -j addr, ethtool, ethtool -m), feeds it through a map-backed
// agent.Runner, and checks the collector's output against the model. The
// oracle is the generated model plus small independent helpers in this file;
// it never calls the production parsing helpers.
//
// A second family (TestInventory_Property_CollectorsNeverPanic) mutates valid renders
// (truncation, dropped/duplicated/reordered lines, whitespace, unicode,
// zero/huge numbers, empty output) and asserts every collector returns
// without panicking.

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// mapRunner is an agent.Runner backed by a map from the full argv string to
// stdout. A missing key is a command failure. Output trims stdout, exactly
// like execkit.Exec.Output.
type mapRunner struct {
	out       map[string]string
	available map[string]bool
	calls     []string
}

func newMapRunner() *mapRunner {
	return &mapRunner{out: map[string]string{}, available: map[string]bool{}}
}

func (m *mapRunner) set(out string, argv ...string) {
	m.out[strings.Join(argv, " ")] = out
	m.available[argv[0]] = true
}

func (m *mapRunner) Available(name string) bool { return m.available[name] }

func (m *mapRunner) Output(name string, args ...string) (string, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	m.calls = append(m.calls, key)
	out, ok := m.out[key]
	if !ok {
		return "", &os.PathError{Op: "exec", Path: key, Err: os.ErrNotExist}
	}
	return strings.TrimSpace(out), nil
}

func (m *mapRunner) Run(name string, args ...string) error {
	_, err := m.Output(name, args...)
	return err
}

// ---------------------------------------------------------------------------
// dmidecode -t 17 (memory devices)

type genDIMM struct {
	locator string
	sizeMB  int // 0 = empty slot
	typ     string
	part    string
	serial  string
	mfg     string
}

// renderDMISize renders a Type 17 size the way dmidecode >= 3.0 does
// (dmi_print_memory_size: pick the highest 1024-unit with a non-zero chunk,
// fold in the next lower chunk when it is non-zero). legacy=true renders the
// pre-3.0 "N MB" style.
func renderDMISize(sizeMB int, legacy bool) string {
	if sizeMB == 0 {
		return "No Module Installed"
	}
	if legacy {
		return fmt.Sprintf("%d MB", sizeMB)
	}
	units := []string{"MB", "GB", "TB", "PB"}
	var split []int
	v := sizeMB
	for v > 0 {
		split = append(split, v&0x3FF)
		v >>= 10
	}
	i := len(split) - 1
	capacity := split[i]
	if i > 0 && split[i-1] != 0 {
		i--
		capacity = split[i] + split[i+1]<<10
	}
	return fmt.Sprintf("%d %s", capacity, units[i])
}

func renderDmidecode17(dimms []genDIMM, legacy bool) string {
	var b strings.Builder
	b.WriteString("# dmidecode 3.5\nGetting SMBIOS data from sysfs.\nSMBIOS 3.3.0 present.\n\n")
	for i, d := range dimms {
		fmt.Fprintf(&b, "Handle 0x%04X, DMI type 17, 92 bytes\nMemory Device\n", 0x40+i)
		b.WriteString("\tArray Handle: 0x003F\n\tTotal Width: 72 bits\n\tData Width: 64 bits\n")
		fmt.Fprintf(&b, "\tSize: %s\n", renderDMISize(d.sizeMB, legacy))
		b.WriteString("\tForm Factor: DIMM\n\tSet: None\n")
		fmt.Fprintf(&b, "\tLocator: %s\n\tBank Locator: NODE %d\n", d.locator, i)
		fmt.Fprintf(&b, "\tType: %s\n\tType Detail: Synchronous Registered (Buffered)\n", d.typ)
		b.WriteString("\tSpeed: 3200 MT/s\n")
		fmt.Fprintf(&b, "\tManufacturer: %s\n\tSerial Number: %s\n", d.mfg, d.serial)
		fmt.Fprintf(&b, "\tAsset Tag: %s_AssetTag\n\tPart Number: %s    \n", d.locator, d.part)
		b.WriteString("\tRank: 2\n\tConfigured Memory Speed: 3200 MT/s\n")
		if d.sizeMB > 0 && !legacy {
			fmt.Fprintf(&b, "\tNon-Volatile Size: None\n\tVolatile Size: %s\n\tCache Size: None\n\tLogical Size: None\n",
				renderDMISize(d.sizeMB, false))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func genDIMMs(t *rapid.T) []genDIMM {
	n := rapid.IntRange(0, 8).Draw(t, "nDIMM")
	out := make([]genDIMM, n)
	for i := range out {
		out[i] = genDIMM{
			locator: fmt.Sprintf("DIMM_%c%d", 'A'+rune(i/2), i%2+1),
			sizeMB:  rapid.SampledFrom([]int{0, 512, 1024, 1536, 2048, 4096, 8192, 16384, 32768, 65536, 98304, 131072, 262144, 1048576}).Draw(t, "sizeMB"),
			typ:     rapid.SampledFrom([]string{"DDR4", "DDR5", "DDR3"}).Draw(t, "type"),
			part:    rapid.SampledFrom([]string{"M393A4K40DB3-CWE", "HMA84GR7CJR4N-XN", "36ASF4G72PZ-3G2"}).Draw(t, "part"),
			serial:  fmt.Sprintf("%08X", rapid.IntRange(1, 1<<30).Draw(t, "serial")),
			mfg:     rapid.SampledFrom([]string{"Samsung", "SK Hynix", "Micron Technology"}).Draw(t, "mfg"),
		}
	}
	return out
}

// TestInventory_Property_MemoryDIMMSizes: every populated DIMM is reported once, in slot
// order, with its size in MiB (DIMMInfo.SizeMB; netbox.CommissionToSync
// divides by 1024 to get GB) regardless of which unit dmidecode chose.
func TestInventory_Property_MemoryDIMMSizes(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dimms := genDIMMs(t)
		legacy := rapid.Bool().Draw(t, "legacyDmidecode")
		r := newMapRunner()
		r.set(renderDmidecode17(dimms, legacy), "dmidecode", "-t", "17")

		got := collectMemory(r)
		var want []genDIMM
		for _, d := range dimms {
			if d.sizeMB > 0 {
				want = append(want, d)
			}
		}
		if len(got.DIMMs) != len(want) {
			t.Fatalf("got %d DIMMs, want %d", len(got.DIMMs), len(want))
		}
		for i, w := range want {
			g := got.DIMMs[i]
			if g.Locator != w.locator || g.Serial != w.serial || g.Part != w.part || g.Manufacturer != w.mfg || g.Type != w.typ {
				t.Fatalf("DIMM %d identity: got %+v, want %+v", i, g, w)
			}
			if g.SizeMB != w.sizeMB {
				t.Fatalf("DIMM %s size: got %d MB, want %d MB (dmidecode printed %q)",
					w.locator, g.SizeMB, w.sizeMB, renderDMISize(w.sizeMB, legacy))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// dmidecode -t 39 (PSUs)

type genPSU struct {
	location, name, serial string
	watts                  int // 0 = Unknown
	status                 string
}

func renderDmidecode39(psus []genPSU) string {
	var b strings.Builder
	b.WriteString("# dmidecode 3.5\nGetting SMBIOS data from sysfs.\nSMBIOS 3.3.0 present.\n\n")
	for i, p := range psus {
		fmt.Fprintf(&b, "Handle 0x%04X, DMI type 39, 22 bytes\nSystem Power Supply\n", 0x60+i)
		fmt.Fprintf(&b, "\tPower Unit Group: 1\n\tLocation: %s\n\tName: %s\n\tManufacturer: Delta\n", p.location, p.name)
		fmt.Fprintf(&b, "\tSerial Number: %s\n\tAsset Tag: N/A\n\tModel Part Number: %s\n\tRevision: A00\n", p.serial, p.name)
		if p.watts > 0 {
			fmt.Fprintf(&b, "\tMax Power Capacity: %d W\n", p.watts)
		} else {
			b.WriteString("\tMax Power Capacity: Unknown\n")
		}
		fmt.Fprintf(&b, "\tStatus: %s\n\tType: Switching\n\tInput Voltage Range Switching: Auto-switch\n\tPlugged: Yes\n\tHot Replaceable: Yes\n\n", p.status)
	}
	return b.String()
}

func TestInventory_Property_PSUs(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 4).Draw(t, "nPSU")
		psus := make([]genPSU, n)
		for i := range psus {
			psus[i] = genPSU{
				location: fmt.Sprintf("PSU%d", i+1),
				name:     rapid.SampledFrom([]string{"PWS-2K04A-1R", "DPS-1600AB", "To Be Filled By O.E.M."}).Draw(t, "name"),
				serial:   rapid.SampledFrom([]string{"P2K04CS12AB3456", "Not Specified", "N/A", "DSN12345"}).Draw(t, "serial"),
				watts:    rapid.SampledFrom([]int{0, 800, 1600, 2000}).Draw(t, "watts"),
				status:   rapid.SampledFrom([]string{"Present, OK", "Present, Critical", "Not Present"}).Draw(t, "status"),
			}
		}
		r := newMapRunner()
		r.set(renderDmidecode39(psus), "dmidecode", "-t", "39")
		got := collectPSUs(r)
		if len(got) != len(psus) {
			t.Fatalf("got %d PSUs, want %d", len(got), len(psus))
		}
		placeholder := map[string]bool{"to be filled by o.e.m.": true, "not specified": true, "n/a": true}
		for i, p := range psus {
			g := got[i]
			wantModel := p.name
			if placeholder[strings.ToLower(p.name)] {
				wantModel = ""
			}
			wantSerial := p.serial
			if placeholder[strings.ToLower(p.serial)] {
				wantSerial = ""
			}
			if g.Location != p.location || g.Model != wantModel || g.Serial != wantSerial || g.MaxWatts != p.watts || g.Status != p.status {
				t.Fatalf("PSU %d: got %+v, want loc=%q model=%q serial=%q watts=%d status=%q",
					i, g, p.location, wantModel, wantSerial, p.watts, p.status)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// lsblk -Jb + per-disk smartctl

type genBlock struct {
	name, model, serial, tran, typ, wwn string
	size                                int64
	rota                                *bool
	children                            []genBlock
	smart                               *genSmart
}

type genSmart struct {
	passed     bool
	vendor, fw string
}

func genBlockDevs(t *rapid.T) []genBlock {
	pool := []string{"nvme0n1", "nvme1n1", "sda", "sdb", "sdc", "loop0", "loop1", "sr0", "ram0", "md127", "zram0"}
	perm := rapid.Permutation(pool).Draw(t, "names")
	n := rapid.IntRange(0, len(pool)).Draw(t, "nBlock")
	out := make([]genBlock, 0, n)
	for _, name := range perm[:n] {
		typ := "disk"
		switch {
		case strings.HasPrefix(name, "loop"):
			typ = "loop"
		case strings.HasPrefix(name, "sr"):
			typ = "rom"
		case strings.HasPrefix(name, "md"):
			typ = "raid1"
		}
		var rota *bool
		switch rapid.IntRange(0, 2).Draw(t, "rotaKind") {
		case 1:
			v := false
			rota = &v
		case 2:
			v := true
			rota = &v
		}
		b := genBlock{
			name:   name,
			model:  rapid.SampledFrom([]string{"", "Samsung SSD 990 PRO 4TB", "ST8000NM000A-2KE101", "AMI Virtual HDisk0", "Micron_7450_MTFDKCC3T8TFR"}).Draw(t, "model"),
			serial: rapid.SampledFrom([]string{"", "S7EVNX0T12345A", "ZA1B2C3D", "AAAABBBBCCCC3", "  PADDED  ", "Série-中"}).Draw(t, "serial") + name,
			tran:   rapid.SampledFrom([]string{"", "nvme", "sata", "sas", "usb"}).Draw(t, "tran"),
			typ:    typ,
			wwn:    rapid.SampledFrom([]string{"", "eui.002538db11a012a8", "0x5000c500a1b2c3d4", "naa.5000c500a1b2c3d4"}).Draw(t, "wwn"),
			size:   rapid.SampledFrom([]int64{0, 512, 63832064, 480103981056, 4000787030016, 15360747397120, 1 << 50}).Draw(t, "size"),
			rota:   rota,
		}
		if typ == "disk" && rapid.Bool().Draw(t, "hasParts") {
			for p := 1; p <= rapid.IntRange(1, 3).Draw(t, "nParts"); p++ {
				sep := ""
				if name[len(name)-1] >= '0' && name[len(name)-1] <= '9' {
					sep = "p"
				}
				b.children = append(b.children, genBlock{name: fmt.Sprintf("%s%s%d", name, sep, p), typ: "part", size: 1 << 30})
			}
		}
		if rapid.Bool().Draw(t, "hasSmart") {
			b.smart = &genSmart{
				passed: rapid.Bool().Draw(t, "smartPassed"),
				vendor: rapid.SampledFrom([]string{"", "SEAGATE", "SAMSUNG"}).Draw(t, "smartVendor"),
				fw:     "FW-" + name,
			}
		}
		out = append(out, b)
	}
	return out
}

// renderLsblk renders lsblk -Jb output. sizeAsString/rotaAsString select the
// older util-linux encodings (strings for every column) that collectDisks
// claims to tolerate ("JSON encoding varies").
func renderLsblk(devs []genBlock, sizeAsString, rotaAsString bool) string {
	var render func(b genBlock) map[string]any
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	render = func(b genBlock) map[string]any {
		m := map[string]any{
			"name": b.name, "model": nullable(b.model), "serial": nullable(b.serial),
			"tran": nullable(b.tran), "type": b.typ, "wwn": nullable(b.wwn),
		}
		if sizeAsString {
			m["size"] = strconv.FormatInt(b.size, 10)
		} else {
			m["size"] = json.Number(strconv.FormatInt(b.size, 10))
		}
		switch {
		case b.rota == nil:
			m["rota"] = nil
		case rotaAsString:
			if *b.rota {
				m["rota"] = "1"
			} else {
				m["rota"] = "0"
			}
		default:
			m["rota"] = *b.rota
		}
		if len(b.children) > 0 {
			var kids []map[string]any
			for _, c := range b.children {
				kids = append(kids, render(c))
			}
			m["children"] = kids
		}
		return m
	}
	var top []map[string]any
	for _, d := range devs {
		top = append(top, render(d))
	}
	out, _ := json.MarshalIndent(map[string]any{"blockdevices": top}, "", "   ")
	return string(out)
}

func renderSmart(s *genSmart, name string) string {
	out, _ := json.Marshal(map[string]any{
		"json_format_version": []int{1, 0},
		"device":              map[string]any{"name": "/dev/" + name},
		"model_name":          "model-" + name,
		"serial_number":       "smart-serial-" + name,
		"firmware_version":    s.fw,
		"vendor":              s.vendor,
		"smart_status":        map[string]any{"passed": s.passed},
	})
	return string(out)
}

// TestInventory_Property_DisksAttribution: lsblk + smartctl -> []DiskInfo. Every top-level
// type=disk device that is not loop/ram appears exactly once, in lsblk order,
// with its own serial/size/transport/wwn/rotational, and SMART facts from the
// smartctl run against ITS device node (never a neighbour's).
func TestInventory_Property_DisksAttribution(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		devs := genBlockDevs(t)
		sizeAsString := rapid.Bool().Draw(t, "sizeAsString")
		r := newMapRunner()
		r.set(renderLsblk(devs, sizeAsString, false), "lsblk", "-Jb", "-o", "NAME,MODEL,SERIAL,SIZE,TRAN,TYPE,WWN,ROTA")
		r.available["smartctl"] = true
		for _, d := range devs {
			if d.smart != nil {
				r.set(renderSmart(d.smart, d.name), "smartctl", "-i", "-H", "-j", "/dev/"+d.name)
			}
		}
		got := collectDisks(r)

		var want []genBlock
		for _, d := range devs {
			if d.typ == "disk" && !strings.HasPrefix(d.name, "loop") && !strings.HasPrefix(d.name, "ram") {
				want = append(want, d)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("got %d disks, want %d: %+v", len(got), len(want), got)
		}
		for i, w := range want {
			g := got[i]
			if g.Name != w.name || g.Serial != w.serial || g.Model != w.model || g.Transport != w.tran || g.WWN != w.wwn {
				t.Fatalf("disk %d identity: got %+v, want %+v", i, g, w)
			}
			if g.SizeBytes != w.size {
				t.Fatalf("disk %s size: got %d, want %d bytes (sizeAsString=%v)", w.name, g.SizeBytes, w.size, sizeAsString)
			}
			if (g.Rotational == nil) != (w.rota == nil) || (g.Rotational != nil && *g.Rotational != *w.rota) {
				t.Fatalf("disk %s rotational: got %v, want %v", w.name, g.Rotational, w.rota)
			}
			if w.smart == nil {
				if g.SMARTHealthy != nil || g.FWVersion != "" || g.Manufacturer != "" {
					t.Fatalf("disk %s got SMART facts without a smartctl result: %+v", w.name, g)
				}
				continue
			}
			if g.SMARTHealthy == nil || *g.SMARTHealthy != w.smart.passed || g.FWVersion != w.smart.fw || g.Manufacturer != w.smart.vendor {
				t.Fatalf("disk %s SMART mis-attributed: got healthy=%v fw=%q mfg=%q, want %v %q %q",
					w.name, g.SMARTHealthy, g.FWVersion, g.Manufacturer, w.smart.passed, w.smart.fw, w.smart.vendor)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// lspci -nnmm and lsusb

var pciNames = []string{
	"Intel Corporation", "Advanced Micro Devices, Inc. [AMD]", "Mellanox Technologies",
	"MT2910 Family [ConnectX-7]", "Ethernet Controller X710 for 10GbE SFP+", "Device",
	"NVIDIA Corporation", "H100 SXM5 80GB", "ASPEED Technology, Inc.", "ASPEED Graphics Family",
	"Vendor É 漢字", "Weird [name] here",
}

type genPCI struct {
	slot                 string
	class, classID       string
	vendor, vendorID     string
	device, deviceID     string
	rev                  string
	subVendor, subDevice string
}

func genPCIs(t *rapid.T) []genPCI {
	n := rapid.IntRange(0, 6).Draw(t, "nPCI")
	out := make([]genPCI, n)
	for i := range out {
		slot := fmt.Sprintf("%02x:%02x.%d", rapid.IntRange(0, 255).Draw(t, "bus"), rapid.IntRange(0, 31).Draw(t, "dev"), rapid.IntRange(0, 7).Draw(t, "fn"))
		if rapid.Bool().Draw(t, "domain") {
			slot = "0000:" + slot
		}
		out[i] = genPCI{
			slot:      slot,
			class:     rapid.SampledFrom([]string{"Host bridge", "Ethernet controller", "3D controller", "VGA compatible controller", "Non-Volatile memory controller"}).Draw(t, "class"),
			classID:   fmt.Sprintf("%04x", rapid.IntRange(0, 0xffff).Draw(t, "classID")),
			vendor:    rapid.SampledFrom(pciNames).Draw(t, "vendor"),
			vendorID:  fmt.Sprintf("%04x", rapid.IntRange(0, 0xffff).Draw(t, "vendorID")),
			device:    rapid.SampledFrom(pciNames).Draw(t, "device"),
			deviceID:  fmt.Sprintf("%04x", rapid.IntRange(0, 0xffff).Draw(t, "deviceID")),
			rev:       rapid.SampledFrom([]string{"", "01", "a1"}).Draw(t, "rev"),
			subVendor: rapid.SampledFrom(append([]string{""}, pciNames...)).Draw(t, "subVendor"),
			subDevice: rapid.SampledFrom([]string{"", "Device", "Board"}).Draw(t, "subDevice"),
		}
	}
	return out
}

func renderLspci(devs []genPCI) string {
	var b strings.Builder
	for _, d := range devs {
		fmt.Fprintf(&b, "%s \"%s [%s]\" \"%s [%s]\" \"%s [%s]\"", d.slot, d.class, d.classID, d.vendor, d.vendorID, d.device, d.deviceID)
		if d.rev != "" {
			fmt.Fprintf(&b, " -r%s", d.rev)
		}
		b.WriteString(" -p00")
		if d.subVendor == "" {
			b.WriteString(" \"\" \"\"")
		} else {
			fmt.Fprintf(&b, " \"%s [1028]\" \"%s [0927]\"", d.subVendor, d.subDevice)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestInventory_Property_LspciRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		devs := genPCIs(t)
		r := newMapRunner()
		r.set(renderLspci(devs), "lspci", "-nnmm")
		got := collectPCI(r)
		if len(got) != len(devs) {
			t.Fatalf("got %d PCI devices, want %d", len(got), len(devs))
		}
		for i, d := range devs {
			want := PCIDevice{Slot: d.slot, Class: d.class, Vendor: d.vendor, VendorID: d.vendorID, Device: d.device, DeviceID: d.deviceID}
			if got[i] != want {
				t.Fatalf("PCI %d: got %+v, want %+v", i, got[i], want)
			}
		}
	})
}

func TestInventory_Property_LsusbRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 6).Draw(t, "nUSB")
		var b strings.Builder
		var want []USBDevice
		for i := 0; i < n; i++ {
			d := USBDevice{
				Bus:      fmt.Sprintf("%03d", rapid.IntRange(1, 8).Draw(t, "bus")),
				Device:   fmt.Sprintf("%03d", rapid.IntRange(1, 127).Draw(t, "dev")),
				VendorID: fmt.Sprintf("%04x", rapid.IntRange(0, 0xffff).Draw(t, "vid")),
				DeviceID: fmt.Sprintf("%04x", rapid.IntRange(0, 0xffff).Draw(t, "did")),
				Name:     rapid.SampledFrom([]string{"", "Linux Foundation 3.0 root hub", "American Megatrends, Inc. Virtual Keyboard and Mouse", "ID Card Reader", "Gerät 漢"}).Draw(t, "name"),
			}
			fmt.Fprintf(&b, "Bus %s Device %s: ID %s:%s %s\n", d.Bus, d.Device, d.VendorID, d.DeviceID, d.Name)
			want = append(want, d)
		}
		r := newMapRunner()
		r.set(b.String(), "lsusb")
		got := collectUSB(r)
		if len(got) != len(want) {
			t.Fatalf("got %d USB devices, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("USB %d: got %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}

// ---------------------------------------------------------------------------
// ipmitool lan print / mc info

func TestInventory_Property_BMC(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ip := rapid.SampledFrom([]string{"0.0.0.0", "10.250.3.17", "192.168.1.120"}).Draw(t, "ip")
		mac := rapid.SampledFrom([]string{"00:00:00:00:00:00", "3c:ec:ef:12:34:56", "d0:50:99:ab:cd:ef"}).Draw(t, "mac")
		fw := rapid.SampledFrom([]string{"", "1.73", "5.10.00.00", "01.04.13"}).Draw(t, "fw")
		mcFails := rapid.Bool().Draw(t, "mcFails")
		lan := fmt.Sprintf(`Set in Progress         : Set Complete
Auth Type Support       : NONE MD2 MD5 PASSWORD
IP Address Source       : DHCP Address
IP Address              : %s
Subnet Mask             : 255.255.255.0
MAC Address             : %s
SNMP Community String   : public
IP Header               : TTL=0x40 Flags=0x40 Precedence=0x00 TOS=0x10
Default Gateway IP      : 10.250.3.1
Default Gateway MAC     : 00:00:00:00:00:00
Backup Gateway IP       : 0.0.0.0
Backup Gateway MAC      : 00:00:00:00:00:00
802.1q VLAN ID          : Disabled
RMCP+ Cipher Suites     : 1,2,3,6,7,8,11,12,15,16,17
Cipher Suite Priv Max   : XaaaXXaaaXXaaXX
`, ip, mac)
		r := newMapRunner()
		r.set(lan, "ipmitool", "lan", "print")
		if !mcFails {
			mc := "Device ID                 : 32\nDevice Revision           : 1\n"
			if fw != "" {
				mc += "Firmware Revision         : " + fw + "\n"
			}
			mc += "IPMI Version              : 2.0\nManufacturer ID           : 10876\n"
			r.set(mc, "ipmitool", "mc", "info")
		}
		got := collectBMC(r)
		wantIP := ip
		if ip == "0.0.0.0" {
			wantIP = ""
		}
		wantMAC := mac
		if mac == "00:00:00:00:00:00" {
			wantMAC = ""
		}
		wantFW := fw
		if mcFails {
			// Early return keeps whatever lan print produced.
			if got == nil || got.IP != wantIP || got.MAC != wantMAC || got.FirmwareVersion != "" {
				t.Fatalf("mc info failed: got %+v, want ip=%q mac=%q", got, wantIP, wantMAC)
			}
			return
		}
		if wantIP == "" && wantFW == "" {
			if got != nil {
				t.Fatalf("no IP and no firmware: want nil BMC, got %+v", got)
			}
			return
		}
		if got == nil || got.IP != wantIP || got.MAC != wantMAC || got.FirmwareVersion != wantFW {
			t.Fatalf("BMC: got %+v, want ip=%q mac=%q fw=%q", got, wantIP, wantMAC, wantFW)
		}
	})
}

// ---------------------------------------------------------------------------
// ip -j addr + ethtool / ethtool -i / ethtool -m

type genOptic struct {
	identifier string // "0x03 (SFP)" / "0x11 (QSFP28)"
	connector  string // "0x07 (LC)" / "0x23 (No separable connector)"
	nm         int    // 850 or 1310; 0 for copper
	xcvrType   string
}

type genNIC struct {
	name, mac, driver string
	speedMbps         int // 0 = Unknown!
	port              string
	supportedPorts    string // ethtool "Supported ports" value, e.g. "[ FIBRE ]"
	optic             *genOptic
}

// renderEthtoolModule renders `ethtool -m` the way ethtool 6.x prints an
// SFP/QSFP EEPROM (sfpid.c / qsfp.c): "Laser wavelength" is the label.
func renderEthtoolModule(o genOptic) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\t%-41s : %s\n", "Identifier", o.identifier)
	fmt.Fprintf(&b, "\t%-41s : 0x04 (SFP defined by 2-wire interface ID)\n", "Extended identifier")
	fmt.Fprintf(&b, "\t%-41s : %s\n", "Connector", o.connector)
	fmt.Fprintf(&b, "\t%-41s : 0x10 0x00 0x00 0x00 0x00 0x00 0x00 0x00\n", "Transceiver codes")
	fmt.Fprintf(&b, "\t%-41s : %s\n", "Transceiver type", o.xcvrType)
	fmt.Fprintf(&b, "\t%-41s : 0x06 (64B/66B)\n", "Encoding")
	if o.nm > 0 {
		fmt.Fprintf(&b, "\t%-41s : %dnm\n", "Laser wavelength", o.nm)
	} else {
		fmt.Fprintf(&b, "\t%-41s : SFF-8431 appendix E\n", "Passive Cu cmplnce.")
	}
	fmt.Fprintf(&b, "\t%-41s : FINISAR CORP.\n", "Vendor name")
	fmt.Fprintf(&b, "\t%-41s : 00:90:65\n", "Vendor OUI")
	fmt.Fprintf(&b, "\t%-41s : FTLX8574D3BCL\n", "Vendor PN")
	fmt.Fprintf(&b, "\t%-41s : AQK0Z1L\n", "Vendor SN")
	return b.String()
}

func renderEthtool(n genNIC) string {
	speed := "Unknown!"
	if n.speedMbps > 0 {
		speed = fmt.Sprintf("%dMb/s", n.speedMbps)
	}
	return fmt.Sprintf(`Settings for %s:
	Supported ports: %s
	Supported link modes:   %dbaseSR/Full
	Supported pause frame use: Symmetric
	Supports auto-negotiation: No
	Advertised link modes:  Not reported
	Speed: %s
	Duplex: Full
	Port: %s
	PHYAD: 0
	Transceiver: internal
	Link detected: yes
`, n.name, n.supportedPorts, max(n.speedMbps, 1000), speed, n.port)
}

// expectedIntfType is the independent oracle for the NetBox interface type:
// it parses the numeric speed, then picks the medium. For an optic with an LC
// connector the wavelength decides SR (850nm) vs LR (1310nm).
func expectedIntfType(n genNIC) string {
	speed := n.speedMbps
	if speed == 0 {
		// Link down: ethtool's single supported link mode drives the
		// fallback classification (renderEthtool prints max(speed,1000)).
		speed = 1000
	}
	if n.optic != nil {
		lc := strings.Contains(strings.ToLower(n.optic.connector), "lc")
		dac := strings.Contains(strings.ToLower(n.optic.connector), "no separable")
		sr := n.optic.nm == 850
		lr := n.optic.nm == 1310
		switch speed {
		case 100000:
			switch {
			case dac:
				return "100gbase-cr4"
			case lc && sr:
				return "100gbase-x-qsfp28"
			case lc && lr:
				return "100gbase-lr4"
			}
		case 25000:
			switch {
			case lc && sr:
				return "25gbase-x-sfp28"
			case lc && lr:
				return "25gbase-lr"
			}
		case 10000:
			switch {
			case lc && sr:
				return "10gbase-x-sfpp"
			case lc && lr:
				return "10gbase-lr"
			}
		case 1000:
			switch {
			case lc && sr:
				return "1000base-x-sfp"
			case lc && lr:
				return "1000base-lx"
			}
		}
	}
	// A direct-attach copper port is an SFP/QSFP cage and classifies with
	// fibre. A port ethtool reports as "Other" (switch ASICs) is decided by
	// the "Supported ports" line instead.
	fibre := n.port == "FIBRE" || n.port == "Direct Attach Copper" ||
		(n.port == "Other" && strings.Contains(n.supportedPorts, "FIBRE"))
	switch speed {
	case 400000:
		return "400gbase-x-qsfpdd"
	case 100000:
		if fibre {
			return "100gbase-x-qsfp28"
		}
		return "100gbase-x-cxp"
	case 50000:
		return "50gbase-x-sfp28"
	case 40000:
		return "40gbase-x-qsfpp"
	case 25000:
		if fibre {
			return "25gbase-x-sfp28"
		}
		return "25gbase-kr"
	case 10000:
		if fibre {
			return "10gbase-x-sfpp"
		}
		return "10gbase-t"
	case 1000:
		if fibre {
			return "1000base-x-sfp"
		}
		return "1000base-t"
	}
	return ""
}

func genNICs(t *rapid.T, withOptics bool) []genNIC {
	names := rapid.Permutation([]string{"eno1", "eno2", "ens1f0np0", "ens1f1np1", "enp65s0f0", "lo", "docker0", "bond0", "eth0.100"}).Draw(t, "nicNames")
	n := rapid.IntRange(0, len(names)).Draw(t, "nNIC")
	out := make([]genNIC, n)
	for i := range out {
		nic := genNIC{
			name:           names[i],
			mac:            fmt.Sprintf("3c:ec:ef:%02x:%02x:%02x", i, rapid.IntRange(0, 255).Draw(t, "mac4"), rapid.IntRange(0, 255).Draw(t, "mac5")),
			driver:         rapid.SampledFrom([]string{"mlx5_core", "ice", "ixgbe", "igb", "bonding", "802.1Q VLAN Support", "cdc_ether"}).Draw(t, "driver"),
			speedMbps:      rapid.SampledFrom([]int{0, 1000, 10000, 25000, 40000, 50000, 100000, 400000}).Draw(t, "speed"),
			port:           rapid.SampledFrom([]string{"FIBRE", "Twisted Pair", "Direct Attach Copper", "Other"}).Draw(t, "port"),
			supportedPorts: rapid.SampledFrom([]string{"[ FIBRE ]", "[ TP ]", "[ TP MII ]", "[ ]"}).Draw(t, "supportedPorts"),
		}
		if withOptics && nic.port == "FIBRE" && rapid.Bool().Draw(t, "hasOptic") {
			id := "0x03 (SFP)"
			if nic.speedMbps == 100000 {
				id = "0x11 (QSFP28)"
			}
			nm := rapid.SampledFrom([]int{850, 1310}).Draw(t, "nm")
			xt := "10G Ethernet: 10G Base-SR"
			if nm == 1310 {
				xt = "10G Ethernet: 10G Base-LR"
			}
			nic.optic = &genOptic{identifier: id, connector: "0x07 (LC)", nm: nm, xcvrType: xt}
		}
		out[i] = nic
	}
	return out
}

func nicRunner(nics []genNIC) *mapRunner {
	r := newMapRunner()
	var links []map[string]any
	for _, n := range nics {
		links = append(links, map[string]any{
			"ifname": n.name, "address": n.mac,
			"addr_info": []map[string]any{{"family": "inet", "local": "10.1.2." + strconv.Itoa(len(links)+2), "prefixlen": 24}},
		})
		r.set(fmt.Sprintf("driver: %s\nversion: 6.8.0\nfirmware-version: 22.36.1010 (MT_0000000359)\nbus-info: 0000:41:00.%d\nsupports-statistics: yes\n", n.driver, len(links)), "ethtool", "-i", n.name)
		r.set(renderEthtool(n), "ethtool", n.name)
		if n.optic != nil {
			r.set(renderEthtoolModule(*n.optic), "ethtool", "-m", n.name)
		}
	}
	js, _ := json.Marshal(links)
	r.set(string(js), "ip", "-j", "addr", "show")
	return r
}

func isVirtualOracle(n genNIC) bool {
	if n.name == "lo" || strings.HasPrefix(n.name, "docker") {
		return true
	}
	return n.driver == "bonding" || n.driver == "802.1Q VLAN Support"
}

// TestInventory_Property_NICsAttribution: each physical NIC appears once with its own MAC
// and driver; virtual ones are filtered. No MAC from one interface may be
// reported under another interface's name.
func TestInventory_Property_NICsAttribution(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nics := genNICs(t, false)
		got := collectNICs(nicRunner(nics))
		var want []genNIC
		for _, n := range nics {
			if !isVirtualOracle(n) {
				want = append(want, n)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("got %d NICs, want %d", len(got), len(want))
		}
		for i, w := range want {
			if got[i].Name != w.name || got[i].MAC != w.mac || got[i].Driver != w.driver {
				t.Fatalf("NIC %d: got name=%q mac=%q driver=%q, want %q %q %q", i, got[i].Name, got[i].MAC, got[i].Driver, w.name, w.mac, w.driver)
			}
		}
	})
}

// TestInventory_Property_NICInterfaceTypeFromRealEthtool: the interface type derived from
// real-shaped ethtool / ethtool -m output agrees with the oracle (SR vs LR
// optics, speed tier). Driver-based overrides (cdc_ether) are excluded here.
func TestInventory_Property_NICInterfaceTypeFromRealEthtool(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nics := genNICs(t, true)
		got := collectNICs(nicRunner(nics))
		byName := map[string]NICInfo{}
		for _, g := range got {
			byName[g.Name] = g
		}
		for _, n := range nics {
			g, ok := byName[n.name]
			if !ok || n.driver == "cdc_ether" {
				continue
			}
			if want := expectedIntfType(n); g.IntfType != want {
				t.Fatalf("NIC %s speed=%d port=%q optic=%+v: interface type %q, want %q", n.name, n.speedMbps, n.port, n.optic, g.IntfType, want)
			}
		}
	})
}

// TestInventory_Property_ClassifySpeedTiers: classifyInterfaceType never maps a speed to a
// slug of a different speed tier (e.g. 400G to a 40G slug).
func TestInventory_Property_ClassifySpeedTiers(t *testing.T) {
	slugMbps := func(slug string) int {
		switch {
		case strings.HasPrefix(slug, "400g"):
			return 400000
		case strings.HasPrefix(slug, "100g"):
			return 100000
		case strings.HasPrefix(slug, "50g"):
			return 50000
		case strings.HasPrefix(slug, "40g"):
			return 40000
		case strings.HasPrefix(slug, "25g"):
			return 25000
		case strings.HasPrefix(slug, "10g"):
			return 10000
		case strings.HasPrefix(slug, "1000base"):
			return 1000
		case strings.HasPrefix(slug, "100base"):
			return 100
		}
		return -1
	}
	rapid.Check(t, func(t *rapid.T) {
		mbps := rapid.SampledFrom([]int{10, 100, 1000, 2500, 5000, 10000, 25000, 40000, 50000, 100000, 200000, 400000, 800000}).Draw(t, "mbps")
		port := rapid.SampledFrom([]string{"FIBRE", "Twisted Pair", "Other", ""}).Draw(t, "port")
		slug := classifyInterfaceType("mlx5_core", fmt.Sprintf("%dMb/s", mbps), port, "[ FIBRE ]", nil)
		if slug == "" {
			return
		}
		if got := slugMbps(slug); got != mbps {
			t.Fatalf("speed %dMb/s classified as %q (a %d Mb/s slug)", mbps, slug, got)
		}
	})
}

// ---------------------------------------------------------------------------
// lscpu --json (flat, non-terminal output)

func TestInventory_Property_LscpuSockets(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sockets := rapid.SampledFrom([]string{"1", "2", "4", "-", ""}).Draw(t, "sockets")
		cores := rapid.IntRange(1, 128).Draw(t, "cores")
		tpc := rapid.IntRange(1, 2).Draw(t, "tpc")
		model := rapid.SampledFrom([]string{"AMD EPYC 9654 96-Core Processor", "Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz", "Neoverse-N1"}).Draw(t, "model")
		fields := []map[string]string{
			{"field": "Architecture:", "data": "x86_64"},
			{"field": "Model name:", "data": model},
			{"field": "Thread(s) per core:", "data": strconv.Itoa(tpc)},
			{"field": "Core(s) per socket:", "data": strconv.Itoa(cores)},
		}
		if sockets != "" {
			fields = append(fields, map[string]string{"field": "Socket(s):", "data": sockets})
		}
		fields = append(fields, map[string]string{"field": "CPU max MHz:", "data": "3700.0000"}, map[string]string{"field": "Flags:", "data": "fpu vme de"})
		order := rapid.Permutation(fields).Draw(t, "order")
		js, _ := json.Marshal(map[string]any{"lscpu": order})
		r := newMapRunner()
		r.set(string(js), "lscpu", "--json")
		got := collectCPUs(r)
		wantSockets, err := strconv.Atoi(sockets)
		if err != nil || wantSockets < 1 {
			wantSockets = 1
		}
		if len(got) != wantSockets {
			t.Fatalf("got %d CPU entries, want %d", len(got), wantSockets)
		}
		for _, c := range got {
			if c.Model != model || c.Cores != cores || c.Threads != cores*tpc || c.Speed != "3700.0000 MHz" {
				t.Fatalf("CPU: got %+v", c)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Robustness: mutated tool output never panics a collector.

func mutateToolOutput(t *rapid.T, s string) string {
	ops := rapid.IntRange(1, 4).Draw(t, "nMut")
	for i := 0; i < ops; i++ {
		lines := strings.Split(s, "\n")
		switch rapid.IntRange(0, 9).Draw(t, "mutOp") {
		case 0: // truncate
			if len(s) > 0 {
				s = s[:rapid.IntRange(0, len(s)).Draw(t, "cut")]
			}
		case 1: // drop a line
			if len(lines) > 1 {
				k := rapid.IntRange(0, len(lines)-1).Draw(t, "drop")
				s = strings.Join(append(lines[:k:k], lines[k+1:]...), "\n")
			}
		case 2: // duplicate a line
			k := rapid.IntRange(0, len(lines)-1).Draw(t, "dup")
			out := append([]string{}, lines[:k+1]...)
			out = append(out, lines[k:]...)
			s = strings.Join(out, "\n")
		case 3: // reverse line order
			for a, b := 0, len(lines)-1; a < b; a, b = a+1, b-1 {
				lines[a], lines[b] = lines[b], lines[a]
			}
			s = strings.Join(lines, "\n")
		case 4: // whitespace noise
			s = strings.ReplaceAll(s, ": ", rapid.SampledFrom([]string{":", " :  ", ":\t", ":  "}).Draw(t, "ws"))
		case 5: // unicode injection
			k := rapid.IntRange(0, len(s)).Draw(t, "uni")
			s = s[:k] + rapid.SampledFrom([]string{"é", "漢字", "\x00", "\xff\xfe", "\"", "[", "]"}).Draw(t, "uniStr") + s[k:]
		case 6: // numbers: zero / huge / negative
			repl := rapid.SampledFrom([]string{"0", "-1", "99999999999999999999", "18446744073709551615", "1e308"}).Draw(t, "num")
			for _, d := range []string{"16384", "32", "4000787030016", "1600", "25000", "128", "2"} {
				s = strings.Replace(s, d, repl, 1)
			}
		case 7:
			s = ""
		case 8: // CRLF
			s = strings.ReplaceAll(s, "\n", "\r\n")
		case 9: // append garbage
			s += "\n" + rapid.SampledFrom([]string{"Memory Device", "System Power Supply", "Bus 001 Device", "00:00.0 \"", "{", "]"}).Draw(t, "tail")
		}
	}
	return s
}

func TestInventory_Property_CollectorsNeverPanic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		r := newMapRunner()
		r.set(mutateToolOutput(t, renderDmidecode17(genDIMMs(t), rapid.Bool().Draw(t, "legacy"))), "dmidecode", "-t", "17")
		r.set(mutateToolOutput(t, renderDmidecode39([]genPSU{{location: "PSU1", name: "X", serial: "Y", watts: 1600, status: "Present, OK"}})), "dmidecode", "-t", "39")
		devs := genBlockDevs(t)
		r.set(mutateToolOutput(t, renderLsblk(devs, rapid.Bool().Draw(t, "s1"), rapid.Bool().Draw(t, "s2"))), "lsblk", "-Jb", "-o", "NAME,MODEL,SERIAL,SIZE,TRAN,TYPE,WWN,ROTA")
		r.available["smartctl"] = true
		for _, d := range devs {
			if d.smart != nil {
				r.set(mutateToolOutput(t, renderSmart(d.smart, d.name)), "smartctl", "-i", "-H", "-j", "/dev/"+d.name)
			}
		}
		r.set(mutateToolOutput(t, renderLspci(genPCIs(t))), "lspci", "-nnmm")
		r.set(mutateToolOutput(t, "Bus 001 Device 002: ID 1d6b:0003 Linux Foundation 3.0 root hub\nBus 002 Device 001: ID 1d6b:0002"), "lsusb")
		r.set(mutateToolOutput(t, "IP Address              : 10.0.0.5\nMAC Address             : 3c:ec:ef:12:34:56\n"), "ipmitool", "lan", "print")
		r.set(mutateToolOutput(t, "Firmware Revision         : 1.73\n"), "ipmitool", "mc", "info")
		r.set(mutateToolOutput(t, `{"lscpu":[{"field":"Model name:","data":"X"},{"field":"Socket(s):","data":"2"},{"field":"Core(s) per socket:","data":"128"}]}`), "lscpu", "--json")
		nics := genNICs(t, true)
		nr := nicRunner(nics)
		for k, v := range nr.out {
			r.out[k] = mutateToolOutput(t, v)
		}
		for k := range nr.available {
			r.available[k] = true
		}
		// collectCPUs allocates one entry per reported socket with no upper
		// bound, so a corrupted "Socket(s)" value in the billions exhausts
		// memory. Renders the mutator turned into such a count are excluded.
		if out := r.out["lscpu --json"]; !strings.Contains(out, "9999") && !strings.Contains(out, "1844") {
			_ = collectCPUs(r)
		}
		_ = collectMemory(r)
		_ = collectPSUs(r)
		_ = collectDisks(r)
		_ = collectPCI(r)
		_ = collectUSB(r)
		_ = collectBMC(r)
		_ = collectNICs(r)
		_ = parseNetShowSystem(mutateToolOutput(t, "Hostname......... leaf1\nSerial Number.... ABC123\nPlatform......... AS -1234"))
		_ = maxSupportedSpeed(strings.Fields(mutateToolOutput(t, "100000baseCR4/Full 25000baseSR/Full")))
	})
}
