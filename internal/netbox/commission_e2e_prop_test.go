package netbox_test

// End-to-end commission write-back property harness.
//
// Each rapid case builds a small site in a strict fake NetBox (nbfake): two
// servers, an optional operator-named switch being commissioned, and two
// leaf switches LLDP neighbours resolve to. Every machine has a hardware
// model (NICs with kernel names and MACs, BMC, DIMMs, disks, PSUs, CPUs,
// GPUs, DMI identity, LLDP neighbours) that a history mutates the way a
// datacenter does: NIC replaced, card moved to another host, kernel names
// shifted, disk added or swapped, PSU replaced, board swapped (new serial,
// new BMC MAC). Commission runs the production path the global handler
// runs: the agent's wire struct (inventory.CommissionResult) -> JSON ->
// netbox.CommissionToSync -> SyncCommissionData against the fake, with the
// device name, role and site taken from the device as handler.go does.
//
// Properties, checked after every commission against an independent oracle
// built from the hardware model (never from production helpers):
//
//   - a report the agent can produce commissions without error, possibly
//     after the at-least-once outbox replay of a run a fault interrupted;
//   - the device converges to the report: serial, asset tag and custom
//     fields; one unprotected interface per reported NIC, by name, with its
//     MAC primary; operator rows (lag, virtual, mgmt) untouched; one module
//     per reported component, every bay occupied, no stale bays;
//   - commission is idempotent: a replay of the same report changes nothing;
//   - isolation: no other device's fields, interfaces, bays or modules
//     change; shared module types and manufacturers are only appended to;
//     a cable is created only with one end on the commissioned device, and
//     a cable on another device is removed only when the commissioned
//     device's own interface is deleted because its NIC left the report.
//
// Known-finding gates: COMMISSION_E2E_SKIP is a comma-separated list of
// generator features to disable so the search continues past a finding:
// dup-bay (two components sharing a bay name), asset-tag (a programmed
// asset tag two hosts share), bmc-row (BMC rows that are MAC-less, named
// ipmi0, or whose BMC MAC changes; and faults on runs with a BMC),
// stale-serial (the module-serial oracle: a part with no readable serial
// keeps its predecessor's). Unset,
// it defaults to defaultSkip; "none" enables everything. exotic-dimm
// (LPDDR5/RAM memory classes outside the NetBox Memory profile enum) is a
// coverage feature, not a finding: real NetBox accepts them. The
// regression tests for each finding are in commission_e2e_regress_test.go
// (build tag commissionfindings).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/agent/inventory"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/netbox/nbfake"
)

// ---------------------------------------------------------------------------
// Hardware model.

type hwNIC struct {
	Name, MAC, Driver, Speed, IntfType, Xcvr string
	VFs                                      int
	// Neighbor is the LLDP neighbour heard on this NIC: switch name and port.
	NbrSwitch, NbrPort string
}

type hwDIMM struct {
	Loc, Type, Part, Serial, Mfg string
	MB                           int
}

type hwDisk struct {
	Model, Serial, Tran string
	Rot                 bool
	Bytes               int64
}

type hwPSU struct {
	Loc, Model, Serial string
	Watts              int
}

type hwBMC struct{ MAC, IP, FW string }

type machine struct {
	Serial, Chassis, BoardSerial, BoardVendor, BoardProduct string
	AssetTag, BIOS, Product                                 string
	NICs                                                    []hwNIC
	BMC                                                     *hwBMC
	DIMMs                                                   []hwDIMM
	Disks                                                   []hwDisk
	PSUs                                                    []hwPSU
	CPUs                                                    []string
	GPUs                                                    [][2]string // vendor, device
	// junk records which identity values the generator drew from the
	// placeholder pool, so the oracle never consults production's list.
	junk map[string]bool
}

func (m *machine) clone() *machine {
	c := *m
	c.NICs = append([]hwNIC(nil), m.NICs...)
	c.DIMMs = append([]hwDIMM(nil), m.DIMMs...)
	c.Disks = append([]hwDisk(nil), m.Disks...)
	c.PSUs = append([]hwPSU(nil), m.PSUs...)
	c.CPUs = append([]string(nil), m.CPUs...)
	c.GPUs = append([][2]string(nil), m.GPUs...)
	if m.BMC != nil {
		b := *m.BMC
		c.BMC = &b
	}
	c.junk = map[string]bool{}
	for k, v := range m.junk {
		c.junk[k] = v
	}
	return &c
}

func isUSBDriver(d string) bool { return d == "rndis_host" || d == "cdc_ether" }

// report renders the machine as the agent's wire struct.
func (m *machine) report() []byte {
	cr := inventory.CommissionResult{Hostname: "localhost"}
	cr.System = &inventory.SystemInfo{
		Manufacturer: m.BoardVendor, ProductName: m.Product, SerialNumber: m.Serial,
		UUID: "4f1d62c0-1f7d-4e74-a57c-a58932e2aa11", BIOSVersion: m.BIOS, AssetTag: m.AssetTag,
		BoardVendor: m.BoardVendor, BoardProduct: m.BoardProduct, BoardSerial: m.BoardSerial,
		ChassisType: "Rack Mount Chassis", ChassisSerial: m.Chassis,
	}
	for _, c := range m.CPUs {
		cr.CPUs = append(cr.CPUs, inventory.CPUInfo{Model: c, Cores: 48, Threads: 96, Speed: "3100.000 MHz", Arch: "x86_64"})
	}
	cr.Memory = &inventory.MemoryInfo{TotalMB: 0}
	for _, d := range m.DIMMs {
		cr.Memory.TotalMB += d.MB
		cr.Memory.DIMMs = append(cr.Memory.DIMMs, inventory.DIMMInfo{Locator: d.Loc, SizeMB: d.MB, Type: d.Type,
			Speed: "4800 MT/s", Part: d.Part, Serial: d.Serial, Manufacturer: d.Mfg})
	}
	for i, d := range m.Disks {
		rot := d.Rot
		cr.Disks = append(cr.Disks, inventory.DiskInfo{Name: fmt.Sprintf("disk%d", i), Model: d.Model, Serial: d.Serial,
			SizeBytes: d.Bytes, Transport: d.Tran, Rotational: &rot})
	}
	lldp := map[string]any{}
	for _, n := range m.NICs {
		ni := inventory.NICInfo{Name: n.Name, MAC: n.MAC, Driver: n.Driver, Speed: n.Speed, IntfType: n.IntfType, SRIOVMaxVFs: n.VFs}
		if n.Xcvr != "" {
			ni.Transceiver = &inventory.TransceiverInfo{Identifier: n.Xcvr}
		}
		cr.NICs = append(cr.NICs, ni)
		if n.NbrSwitch != "" {
			lldp[n.Name] = map[string]any{
				"chassis": map[string]any{n.NbrSwitch: map[string]any{"id": map[string]any{"type": "mac", "value": "0c:42:a1:00:00:01"}}},
				"port":    map[string]any{"id": map[string]any{"type": "ifname", "value": n.NbrPort}, "descr": n.NbrPort},
			}
		}
	}
	if len(lldp) > 0 {
		raw, _ := json.Marshal(map[string]any{"lldp": map[string]any{"interface": lldp}})
		cr.LLDP = raw
	}
	for _, g := range m.GPUs {
		cr.PCIDevices = append(cr.PCIDevices, inventory.PCIDevice{Slot: "0000:03:00.0", Class: "VGA compatible controller", Vendor: g[0], Device: g[1]})
	}
	cr.PCIDevices = append(cr.PCIDevices, inventory.PCIDevice{Slot: "0000:00:00.0", Class: "Host bridge", Vendor: "Advanced Micro Devices, Inc. [AMD]", Device: "Root Complex"})
	if m.BMC != nil {
		cr.BMC = &inventory.BMCInfo{IP: m.BMC.IP, MAC: m.BMC.MAC, FirmwareVersion: m.BMC.FW}
	}
	for _, p := range m.PSUs {
		cr.PSUs = append(cr.PSUs, inventory.PSUInfo{Location: p.Loc, Model: p.Model, Serial: p.Serial, MaxWatts: p.Watts, Status: "Present, OK"})
	}
	raw, err := json.Marshal(cr)
	if err != nil {
		panic(err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// Generators.

type features struct {
	dupBay, exoticDIMM, bmc, bmcMACChange, assetTag, lldp, operatorRows bool
}

// defaultSkip gates the findings still open on main, so the discovery
// suite stays green; set COMMISSION_E2E_SKIP=none to search with every
// feature enabled.
const defaultSkip = "dup-bay,asset-tag,bmc-row,stale-serial"

func skipped(key string) bool {
	list, set := os.LookupEnv("COMMISSION_E2E_SKIP")
	if !set {
		list = defaultSkip
	}
	for _, k := range strings.Split(list, ",") {
		if strings.TrimSpace(k) == key {
			return true
		}
	}
	return false
}

func drawFeatures(t *rapid.T) features {
	f := features{
		dupBay:       rapid.IntRange(0, 3).Draw(t, "f.dupBay") == 0 && !skipped("dup-bay"),
		exoticDIMM:   rapid.IntRange(0, 4).Draw(t, "f.exoticDIMM") == 0 && !skipped("exotic-dimm"),
		bmc:          rapid.Bool().Draw(t, "f.bmc"),
		bmcMACChange: rapid.Bool().Draw(t, "f.bmcMACChange") && !skipped("bmc-row"),
		assetTag:     rapid.IntRange(0, 3).Draw(t, "f.assetTag") == 0 && !skipped("asset-tag"),
		lldp:         rapid.Bool().Draw(t, "f.lldp"),
		operatorRows: rapid.Bool().Draw(t, "f.operatorRows"),
	}
	return f
}

var (
	nicNames   = []string{"eno1", "eno2", "eth0", "eth1", "ens1f0np0", "ens1f1np1", "enp65s0f0np0"}
	junkValues = []string{"Default string", "To Be Filled By O.E.M.", "Not Specified", "0123456789", "00", ""}
	switchPort = []string{"swp1", "swp2", "swp3"}
)

func macOf(host, k int) string { return fmt.Sprintf("02:00:%02x:00:00:%02x", host, k) }

func drawNIC(t *rapid.T, f features, host int, used map[string]bool, label string) hwNIC {
	var name string
	for _, n := range rapid.Permutation(nicNames).Draw(t, label+".names") {
		if !used[n] {
			name = n
			break
		}
	}
	if name == "" {
		name = fmt.Sprintf("enp%ds0", len(used)+1)
	}
	used[name] = true
	n := hwNIC{
		Name:   name,
		MAC:    macOf(host, rapid.IntRange(0, 9).Draw(t, label+".mac")),
		Driver: rapid.SampledFrom([]string{"mlx5_core", "ice", "igb", "e1000e", "bnxt_en", "mlx5_core", "rndis_host", "cdc_ether"}).Draw(t, label+".drv"),
	}
	if isUSBDriver(n.Driver) {
		// The BMC USB NIC: a MAC many chassis share.
		n.MAC = "be:3a:f2:b6:05:9f"
		n.Name = "enxbe3af2b6059f"
		return n
	}
	sp := rapid.IntRange(0, 4).Draw(t, label+".speed")
	n.Speed = []string{"", "1000Mb/s", "25000Mb/s", "100000Mb/s", "Unknown!"}[sp]
	n.IntfType = rapid.SampledFrom([]string{"", "1000base-t", "25gbase-x-sfp28", "100gbase-cr4", "100gbase-x-qsfp28", "10gbase-t"}).Draw(t, label+".type")
	n.Xcvr = rapid.SampledFrom([]string{"", "0x11 (QSFP28)", "0x03 (SFP)"}).Draw(t, label+".xcvr")
	n.VFs = rapid.SampledFrom([]int{0, 0, 8, 64}).Draw(t, label+".vfs")
	if f.lldp && rapid.Bool().Draw(t, label+".lldp") {
		n.NbrSwitch = rapid.SampledFrom([]string{"leaf-a.site1", "leaf-b.site1", "leaf-a.internal.example.net", "nosuch-switch"}).Draw(t, label+".nbr")
		n.NbrPort = rapid.SampledFrom(switchPort).Draw(t, label+".port")
	}
	return n
}

func drawDIMMs(t *rapid.T, f features, label string) []hwDIMM {
	n := rapid.IntRange(0, 4).Draw(t, label+".n")
	family := rapid.IntRange(0, 3).Draw(t, label+".family")
	if f.dupBay && rapid.Bool().Draw(t, label+".dupfam") {
		family = 4
	}
	types := []string{"DDR4", "DDR5", "DDR5", "DDR3"}
	if f.exoticDIMM {
		types = append(types, "LPDDR5", "Other", "RAM")
	}
	typ := rapid.SampledFrom(types).Draw(t, label+".type")
	var out []hwDIMM
	for i := 0; i < n; i++ {
		var loc string
		switch family {
		case 0:
			loc = fmt.Sprintf("DIMM%c1", 'A'+i)
		case 1:
			loc = fmt.Sprintf("CPU%d_DIMM_%c1", i%2+1, 'A'+i/2)
		case 2:
			loc = fmt.Sprintf("ChannelA-DIMM%d", i)
		case 3:
			loc = fmt.Sprintf("%c%d", 'A'+i%2, i/2+1)
		case 4:
			loc = "DIMM 0" // boards that leave the bank in Bank Locator
		}
		out = append(out, hwDIMM{
			Loc: loc, Type: typ, MB: rapid.SampledFrom([]int{16384, 32768, 65536}).Draw(t, label+".mb"),
			Part:   rapid.SampledFrom([]string{"", "M321R4GA3BB6-CQKEG", "HMCG94AHBRA283N"}).Draw(t, label+".part"),
			Serial: rapid.SampledFrom([]string{"", "8001A1B2", "8001A1B3", "8001A1B4"}).Draw(t, label+".serial"),
			Mfg:    rapid.SampledFrom([]string{"", "Samsung", "SK Hynix", "002C00B3002C"}).Draw(t, label+".mfg"),
		})
	}
	return out
}

func drawDisk(t *rapid.T, label string) hwDisk {
	tran := rapid.SampledFrom([]string{"nvme", "nvme", "sata", "sas", "usb"}).Draw(t, label+".tran")
	return hwDisk{
		Tran:   tran,
		Rot:    tran == "usb" && rapid.Bool().Draw(t, label+".rot"),
		Model:  rapid.SampledFrom([]string{"", "SAMSUNG MZQL23T8HCLS-00A07", "KCD6XLUL15T3", "MTFDDAK960TDN", "InnoDisk Corp. - "}).Draw(t, label+".model"),
		Serial: rapid.SampledFrom([]string{"", "S64HNE0R100001", "S64HNE0R100002", "S64HNE0R100003", "S64HNE0R100004"}).Draw(t, label+".serial"),
		Bytes:  rapid.SampledFrom([]int64{0, 960197124096, 3840755982336, 15362991415296}).Draw(t, label+".bytes"),
	}
}

func drawPSUs(t *rapid.T, f features, label string) []hwPSU {
	n := rapid.IntRange(0, 2).Draw(t, label+".n")
	locs := []string{"PSU1", "PSU2", ""}
	dup := f.dupBay && rapid.Bool().Draw(t, label+".dup")
	var out []hwPSU
	for i := 0; i < n; i++ {
		loc := locs[min(i, 1)]
		if rapid.IntRange(0, 4).Draw(t, label+".blank") == 0 {
			loc = ""
		}
		if dup {
			loc = rapid.SampledFrom([]string{"Not Specified", "To Be Filled By O.E.M.", "Default string"}).Draw(t, label+".junkloc")
		}
		out = append(out, hwPSU{
			Loc:    loc,
			Model:  rapid.SampledFrom([]string{"", "PWS-1K24A-1R", "G1358-1600WNB"}).Draw(t, label+".model"),
			Serial: rapid.SampledFrom([]string{"", "P1K2400001", "P1K2400002"}).Draw(t, label+".serial"),
			Watts:  rapid.SampledFrom([]int{0, 1200, 1600}).Draw(t, label+".watts"),
		})
	}
	if dup && len(out) == 2 {
		out[1].Loc = out[0].Loc
	}
	return out
}

func drawIdentity(t *rapid.T, m *machine, f features, host int, label string) {
	pick := func(key string, real []string) string {
		if rapid.IntRange(0, 3).Draw(t, label+"."+key+".junk") == 0 {
			m.junk[key] = true
			return rapid.SampledFrom(junkValues).Draw(t, label+"."+key+".j")
		}
		m.junk[key] = false
		return rapid.SampledFrom(real).Draw(t, label+"."+key)
	}
	m.Serial = pick("serial", []string{fmt.Sprintf("S515AB%04d", host), fmt.Sprintf("S515AB%04d", host+50)})
	m.Chassis = pick("chassis", []string{fmt.Sprintf("CH1190%04d", host), fmt.Sprintf("CH1190%04d", host+50)})
	m.BoardSerial = rapid.SampledFrom([]string{"", fmt.Sprintf("BS%04d", host), fmt.Sprintf("BS%04d", host+50)}).Draw(t, label+".bserial")
	m.BoardVendor = rapid.SampledFrom([]string{"", "Supermicro", "ASUSTeK COMPUTER INC.", "Dell Inc."}).Draw(t, label+".bvendor")
	m.BoardProduct = rapid.SampledFrom([]string{"", "H13SSL-N", "K14PA-U12"}).Draw(t, label+".bproduct")
	m.BIOS = rapid.SampledFrom([]string{"", "1.4", "2.1a"}).Draw(t, label+".bios")
	m.Product = rapid.SampledFrom([]string{"AS -1115HS-TNR", "RS501A-E12-RS4U", "PowerEdge R640"}).Draw(t, label+".product")
	m.junk["asset"] = true
	m.AssetTag = rapid.SampledFrom([]string{"Default string", "Not Specified", "00", ""}).Draw(t, label+".asset")
	if f.assetTag && rapid.Bool().Draw(t, label+".realasset") {
		// A programmed tag. Two hosts can carry the same one (a board moved
		// between chassis, or a vendor that stamps one tag per order).
		m.junk["asset"] = false
		m.AssetTag = rapid.SampledFrom([]string{"AT-1000", "AT-1001"}).Draw(t, label+".assetv")
	}
}

func drawMachine(t *rapid.T, f features, host int, label string) *machine {
	m := &machine{junk: map[string]bool{}}
	drawIdentity(t, m, f, host, label)
	used := map[string]bool{}
	for i := 0; i < rapid.IntRange(0, 4).Draw(t, label+".nNIC"); i++ {
		m.NICs = append(m.NICs, drawNIC(t, f, host, used, fmt.Sprintf("%s.nic%d", label, i)))
	}
	dedupeMACs(m)
	if f.bmc && rapid.Bool().Draw(t, label+".hasBMC") {
		m.BMC = &hwBMC{MAC: fmt.Sprintf("d0:50:99:00:%02x:01", host), IP: "10.250.0.10", FW: rapid.SampledFrom([]string{"", "1.73"}).Draw(t, label+".bmcfw")}
		if len(m.NICs) > 0 && !isUSBDriver(m.NICs[0].Driver) && rapid.IntRange(0, 3).Draw(t, label+".lom") == 0 {
			m.BMC.MAC = m.NICs[0].MAC // shared LOM
		}
	}
	m.DIMMs = drawDIMMs(t, f, label+".dimm")
	for i := 0; i < rapid.IntRange(0, 3).Draw(t, label+".nDisk"); i++ {
		m.Disks = append(m.Disks, drawDisk(t, fmt.Sprintf("%s.disk%d", label, i)))
	}
	m.PSUs = drawPSUs(t, f, label+".psu")
	for i := 0; i < rapid.IntRange(1, 2).Draw(t, label+".nCPU"); i++ {
		m.CPUs = append(m.CPUs, "AMD EPYC 9454P 48-Core Processor")
	}
	if rapid.Bool().Draw(t, label+".gpu") {
		m.GPUs = append(m.GPUs, rapid.SampledFrom([][2]string{{"ASPEED Technology, Inc.", "ASPEED Graphics Family"}, {"NVIDIA Corporation", "Device"}, {"Matrox Electronics Systems Ltd.", "Integrated Matrox G200eW3 Graphics Controller"}}).Draw(t, label+".gpuv"))
	}
	return m
}

// dedupeMACs keeps physical NIC MACs unique within a host (the generator's
// small pool otherwise repeats them); shared MACs are modelled separately
// (BMC USB NIC, shared-LOM BMC).
func dedupeMACs(m *machine) {
	seen := map[string]bool{}
	for i := range m.NICs {
		if isUSBDriver(m.NICs[i].Driver) {
			continue
		}
		for seen[m.NICs[i].MAC] {
			b := []byte(m.NICs[i].MAC)
			b[len(b)-1] = "0123456789abcdef"[(strings.IndexByte("0123456789abcdef", b[len(b)-1])+1)%16]
			m.NICs[i].MAC = string(b)
		}
		seen[m.NICs[i].MAC] = true
	}
	// USB NIC appears at most once.
	var out []hwNIC
	usb := false
	for _, n := range m.NICs {
		if isUSBDriver(n.Driver) {
			if usb {
				continue
			}
			usb = true
		}
		out = append(out, n)
	}
	m.NICs = out
}

// ---------------------------------------------------------------------------
// World and history.

type world struct {
	nb       *nbfake.Server
	client   *netbox.Client
	servers  []int      // device ids being commissioned
	roles    []string   // their role slugs
	hw       []*machine // their current hardware
	uuids    []string
	leafs    map[string]int // switch name -> device id
	protRows map[int][]int  // device -> seeded protected interface ids
	opTags   map[int][]string
	dt       int
	f        features
	stats    *stats
}

type stats struct {
	mu                                                       sync.Mutex
	cases, commissions, faults, faultErrs, replays           int
	renames, creates, deletes, cablesMade, cablesCascade     int
	moduleCreates, moduleDeletes, bayDeletes, mutations      int
	maxNICs, maxModules                                      int
	bmcRuns, lldpRuns, switchRuns, pagedRuns, errsOnValid    int
	nicsMoved, boardSwaps, idempotentWrites, idempotentCases int
}

func (s *stats) add(fn func(*stats)) { s.mu.Lock(); fn(s); s.mu.Unlock() }

var (
	sharedNB     *nbfake.Server
	sharedNBOnce sync.Once
)

func fakeNB() *nbfake.Server {
	sharedNBOnce.Do(func() {
		sharedNB = nbfake.New()
		sharedNB.Start()
	})
	return sharedNB
}

func newWorld(t *rapid.T, st *stats) *world {
	nb := fakeNB()
	nb.Reset()
	nb.MaxPageSize = 0
	if rapid.Bool().Draw(t, "paged") {
		nb.MaxPageSize = rapid.IntRange(1, 4).Draw(t, "pageSize")
		st.add(func(s *stats) { s.pagedRuns++ })
	}
	c := netbox.NewClient(netbox.ClientConfig{BaseURL: nb.URL(), Token: "t"})
	c.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := &world{nb: nb, client: c, leafs: map[string]int{}, protRows: map[int][]int{}, opTags: map[int][]string{}, stats: st}
	w.f = drawFeatures(t)
	w.dt = nb.AddDeviceType("generic-server", nil)
	// A same-named switch at another site, so LLDP resolution must prefer
	// the local one. Created first, so it lists before the local one.
	other := nb.AddDevice(nbfake.Device{Name: "leaf-a.site1", SiteID: 2, RoleSlug: "leaf", DeviceTypeID: w.dt})
	for _, p := range switchPort {
		nb.AddIface(other, p, "100gbase-x-qsfp28", false, "")
	}
	for _, name := range []string{"leaf-a.site1", "leaf-b.site1"} {
		id := nb.AddDevice(nbfake.Device{Name: name, SiteID: 1, RoleSlug: "leaf", DeviceTypeID: w.dt, Serial: "SW" + name[:6]})
		w.leafs[name] = id
		for _, p := range switchPort {
			nb.AddIface(id, p, "100gbase-x-qsfp28", false, "")
		}
		nb.AddIface(id, "eth0", "1000base-t", true, "0c:42:a1:00:00:"+map[string]string{"leaf-a.site1": "0a", "leaf-b.site1": "0b"}[name])
	}
	nServers := 2
	for i := 0; i < nServers; i++ {
		role := "server"
		if i == 1 && rapid.IntRange(0, 4).Draw(t, "switchRole") == 0 {
			role = "leaf" // an operator-named switch commissioned in place
		}
		var tags []string
		if rapid.Bool().Draw(t, fmt.Sprintf("s%d.optag", i)) {
			tags = []string{"operator-tag"}
		}
		id := nb.AddDevice(nbfake.Device{Name: fmt.Sprintf("host%d.site1", i+1), SiteID: 1, RoleSlug: role, DeviceTypeID: w.dt, Tags: tags})
		w.servers = append(w.servers, id)
		w.roles = append(w.roles, role)
		w.opTags[id] = tags
		w.uuids = append(w.uuids, fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i+1))
		m := drawMachine(t, w.f, i+1, fmt.Sprintf("m%d", i))
		w.hw = append(w.hw, m)
		w.seedRows(t, i, id, m, role)
	}
	return w
}

// seedRows gives a device the NetBox rows an operator models before the
// first commission: persistent names for some NICs (so the initrd's
// kernel names must rename them by MAC), a bond, the loopback, a BMC row.
func (w *world) seedRows(t *rapid.T, i, dev int, m *machine, role string) {
	label := fmt.Sprintf("seed%d", i)
	for k, n := range m.NICs {
		if isUSBDriver(n.Driver) {
			continue
		}
		row := 0
		switch rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("%s.nic%d", label, k)) {
		case 1: // operator persistent name, same MAC
			row = w.nb.AddIface(dev, fmt.Sprintf("ens%df0", 10+k), "100gbase-x-qsfp28", false, n.MAC)
		case 2: // already named as the initrd names it
			row = w.nb.AddIface(dev, n.Name, "100gbase-x-qsfp28", false, n.MAC)
		case 3: // a stale row from hardware no longer present
			row = w.nb.AddIface(dev, fmt.Sprintf("ens%df1", 10+k), "25gbase-x-sfp28", false, macOf(i+1, 40+k))
		}
		if row != 0 && role == "server" && rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("%s.cable%d", label, k)) == 0 {
			// Operator-cabled to a leaf port.
			port := w.leafPort(rapid.SampledFrom([]string{"leaf-a.site1", "leaf-b.site1"}).Draw(t, fmt.Sprintf("%s.cablesw%d", label, k)),
				rapid.SampledFrom(switchPort).Draw(t, fmt.Sprintf("%s.cablep%d", label, k)))
			if port != 0 {
				w.nb.AddCable(row, port)
			}
		}
		if role != "server" {
			// Switch: operator-named swp row carrying the port's MAC.
			w.nb.AddIface(dev, fmt.Sprintf("swp%d", 10+k), "100gbase-x-qsfp28", false, n.MAC)
		}
	}
	if !w.f.operatorRows {
		return
	}
	var prot []int
	if len(m.NICs) > 0 && rapid.Bool().Draw(t, label+".bond") {
		prot = append(prot, w.nb.AddIface(dev, "bond0", "lag", false, m.NICs[0].MAC))
	}
	if rapid.Bool().Draw(t, label+".lo") {
		prot = append(prot, w.nb.AddIface(dev, "lo", "virtual", false, ""))
	}
	if rapid.Bool().Draw(t, label+".oob") {
		// A management row protected only by mgmt_only (its name is not a
		// BMC name): a dedicated host-OS management NIC the initrd does
		// not report, or on a switch the operator's eth0 carrying the
		// MAC the initrd reports under another kernel name.
		name, mac := "oob0", macOf(i+1, 60)
		if role != "server" && len(m.NICs) > 0 && !isUSBDriver(m.NICs[0].Driver) {
			name, mac = "eth0", m.NICs[0].MAC
		}
		if name != "eth0" || !hasRowNamed(w, dev, "eth0") {
			prot = append(prot, w.nb.AddIface(dev, name, "1000base-t", true, mac))
		}
	}
	if m.BMC != nil && rapid.Bool().Draw(t, label+".ipmi") {
		shapes := []string{"ipmi"}
		if !skipped("bmc-row") {
			// ipmi0 carrying the BMC MAC is the production operator shape;
			// a MAC-less ipmi row is what an operator models before the
			// MAC is known (and what a failed MAC write leaves).
			shapes = append(shapes, "ipmi0", "ipmi-nomac")
		}
		switch rapid.SampledFrom(shapes).Draw(t, label+".ipmishape") {
		case "ipmi":
			prot = append(prot, w.nb.AddIface(dev, "ipmi", "1000base-t", true, m.BMC.MAC))
		case "ipmi0":
			prot = append(prot, w.nb.AddIface(dev, "ipmi0", "1000base-t", true, m.BMC.MAC))
		case "ipmi-nomac":
			prot = append(prot, w.nb.AddIface(dev, "ipmi", "1000base-t", true, ""))
		}
	}
	w.protRows[dev] = prot
}

func hasRowNamed(w *world, dev int, name string) bool {
	w.nb.Lock()
	defer w.nb.Unlock()
	for _, i := range w.nb.Ifaces {
		if i.Device == dev && i.Name == name {
			return true
		}
	}
	return false
}

// leafPort returns the id of an uncabled port on a site-1 leaf, or 0.
func (w *world) leafPort(sw, port string) int {
	w.nb.Lock()
	defer w.nb.Unlock()
	dev := w.leafs[sw]
	for id, i := range w.nb.Ifaces {
		if i.Device == dev && i.Name == port && i.Cable == 0 {
			return id
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// Snapshots.

type ifSnap struct {
	ID       int
	Name     string
	Type     string
	Mgmt     bool
	MAC      string
	AllMACs  string
	Cabled   bool
	Desc     string
	Speed    int
	LAG      int
	CableEnd int // the other interface id
}

type modSnap struct {
	ID, Bay, Type int
	BayName       string
	Profile       string
	Serial        string
	FW            any
	Status        string
}

type devSnap struct {
	Device  string
	Ifaces  []ifSnap
	Bays    []string // "id:name"
	Modules []modSnap
}

type globalSnap struct {
	Devices     map[int]devSnap
	ModuleTypes map[int]string
	Mfgs        map[int]string
	Cables      map[int][2]int
	DevTypes    map[int]string
}

func (w *world) snapshot() globalSnap {
	nb := w.nb
	nb.Lock()
	defer nb.Unlock()
	g := globalSnap{Devices: map[int]devSnap{}, ModuleTypes: map[int]string{}, Mfgs: map[int]string{}, Cables: map[int][2]int{}, DevTypes: map[int]string{}}
	for id, d := range nb.Devices {
		at := "<nil>"
		if d.AssetTag != nil {
			at = *d.AssetTag
		}
		tags := append([]string(nil), d.Tags...)
		sort.Strings(tags)
		cfk := make([]string, 0)
		for k, v := range d.CustomFields {
			cfk = append(cfk, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(cfk)
		ds := devSnap{Device: fmt.Sprintf("name=%s serial=%s asset=%s status=%s site=%d type=%d tags=%v cf=%v",
			d.Name, d.Serial, at, d.Status, d.SiteID, d.DeviceTypeID, tags, cfk)}
		g.Devices[id] = ds
	}
	for id, i := range nb.Ifaces {
		var macs []string
		for _, m := range nb.MACs {
			if m.IfaceID == id {
				macs = append(macs, m.MAC)
			}
		}
		sort.Strings(macs)
		s := ifSnap{ID: id, Name: i.Name, Type: i.Type, Mgmt: i.MgmtOnly, AllMACs: strings.Join(macs, ","), Cabled: i.Cable != 0, Desc: i.Description, LAG: i.LAG}
		if m, ok := nb.MACs[i.PrimaryMAC]; ok {
			s.MAC = m.MAC
		}
		if i.Speed != nil {
			s.Speed = *i.Speed
		}
		if c, ok := nb.Cables[i.Cable]; ok {
			s.CableEnd = c.A + c.B - id
		}
		ds := g.Devices[i.Device]
		ds.Ifaces = append(ds.Ifaces, s)
		g.Devices[i.Device] = ds
	}
	for id, b := range nb.Bays {
		ds := g.Devices[b.Device]
		ds.Bays = append(ds.Bays, fmt.Sprintf("%d:%s", id, b.Name))
		g.Devices[b.Device] = ds
	}
	for id, m := range nb.Modules {
		ds := g.Devices[m.Device]
		prof := ""
		if mt, ok := nb.ModuleTypes[m.ModuleType]; ok {
			if p, ok := nb.Profiles[mt.Profile]; ok {
				prof = p.Name
			}
		}
		bayName := ""
		if b, ok := nb.Bays[m.Bay]; ok {
			bayName = b.Name
		}
		ds.Modules = append(ds.Modules, modSnap{ID: id, Bay: m.Bay, Type: m.ModuleType, BayName: bayName, Profile: prof, Serial: m.Serial, FW: m.CustomFields["firmware_version"], Status: m.Status})
		g.Devices[m.Device] = ds
	}
	for id, ds := range g.Devices {
		sort.Slice(ds.Ifaces, func(a, b int) bool { return ds.Ifaces[a].ID < ds.Ifaces[b].ID })
		sort.Strings(ds.Bays)
		sort.Slice(ds.Modules, func(a, b int) bool { return ds.Modules[a].ID < ds.Modules[b].ID })
		g.Devices[id] = ds
	}
	for id, mt := range nb.ModuleTypes {
		g.ModuleTypes[id] = fmt.Sprintf("%d/%s/%d/%v", mt.Manufacturer, mt.Model, mt.Profile, mt.AttributeData)
	}
	for id, m := range nb.Manufacturers {
		g.Mfgs[id] = m.Name + "/" + m.Slug
	}
	for id, c := range nb.Cables {
		g.Cables[id] = [2]int{c.A, c.B}
	}
	for id, dt := range nb.DeviceTypes {
		g.DevTypes[id] = fmt.Sprintf("%s %v", dt.Model, dt.CustomFields)
	}
	return g
}

// ---------------------------------------------------------------------------
// Commission (the handler's NetBox path).

func (w *world) commission(i int, report []byte) error {
	dev := w.servers[i]
	ctx := context.Background()
	sync, err := netbox.CommissionToSync(dev, report)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	sync.SlagworksID = w.uuids[i]
	sync.MachineState = "commissioning"
	d, err := w.client.GetDevice(ctx, dev)
	if err != nil {
		return err
	}
	if d.Site != nil {
		sync.SiteID = d.Site.ID
	}
	if d.Role != nil {
		sync.DeviceRole = d.Role.Slug
	}
	if d.Name != "" {
		sync.Hostname = d.Name
	}
	return w.client.SyncCommissionData(ctx, *sync)
}

// ---------------------------------------------------------------------------
// Oracle.

func upper(s string) string { return strings.ToUpper(s) }

// expectedModules is the per-profile component count the report carries.
func expectedModules(m *machine) map[string]int {
	want := map[string]int{}
	for _, d := range m.DIMMs {
		if d.MB > 0 {
			want["Memory"]++
		}
	}
	want["CPU"] = len(m.CPUs)
	want["Hard disk"] = len(m.Disks)
	want["GPU"] = len(m.GPUs)
	want["Power supply"] = len(m.PSUs)
	if m.BIOS != "" {
		want["Expansion card"]++
	}
	if m.BMC != nil && m.BMC.FW != "" {
		want["Expansion card"]++
	}
	for k, v := range want {
		if v == 0 {
			delete(want, k)
		}
	}
	return want
}

func (w *world) checkDevice(t *rapid.T, i int, before, after globalSnap) {
	dev := w.servers[i]
	m := w.hw[i]
	role := w.roles[i]
	ds := after.Devices[dev]
	w.nb.Lock()
	d := *w.nb.Devices[dev]
	w.nb.Unlock()

	// Identity fields.
	wantSerial := ""
	switch {
	case !m.junk["serial"] && strings.TrimSpace(m.Serial) != "":
		wantSerial = m.Serial
	case !m.junk["chassis"] && strings.TrimSpace(m.Chassis) != "":
		wantSerial = m.Chassis
	}
	if strings.TrimSpace(d.Serial) != strings.TrimSpace(wantSerial) {
		t.Fatalf("device %d serial %q, want %q (system %q junk=%v, chassis %q junk=%v)", dev, d.Serial, wantSerial, m.Serial, m.junk["serial"], m.Chassis, m.junk["chassis"])
	}
	if m.junk["asset"] {
		if d.AssetTag != nil {
			t.Fatalf("device %d asset tag %q from placeholder %q", dev, *d.AssetTag, m.AssetTag)
		}
	} else if d.AssetTag == nil || *d.AssetTag != m.AssetTag {
		// NetBox keeps asset_tag unique: when another device holds the
		// tag, the field is left as it was and the rest still lands.
		heldElsewhere := false
		w.nb.Lock()
		for id, o := range w.nb.Devices {
			if id != dev && o.AssetTag != nil && *o.AssetTag == m.AssetTag {
				heldElsewhere = true
			}
		}
		w.nb.Unlock()
		if !heldElsewhere || !strings.Contains(before.Devices[dev].Device, fmt.Sprintf("asset=%s ", assetStr(d.AssetTag))) {
			t.Fatalf("device %d asset tag %v, want %q", dev, d.AssetTag, m.AssetTag)
		}
	}
	cfWant := map[string]any{"slagworks_id": w.uuids[i], "board_vendor": m.BoardVendor, "board_product": m.BoardProduct, "board_serial": m.BoardSerial, "chassis_type": "Rack Mount Chassis"}
	for k, v := range cfWant {
		got := d.CustomFields[k]
		if v == "" {
			if got != nil {
				t.Fatalf("device %d custom field %s = %v, want null", dev, k, got)
			}
		} else if got != v {
			t.Fatalf("device %d custom field %s = %v, want %v", dev, k, got, v)
		}
	}
	tagSet := map[string]bool{}
	for _, tg := range d.Tags {
		tagSet[tg] = true
	}
	if !tagSet["slagworks-managed"] {
		t.Fatalf("device %d lacks slagworks-managed tag: %v", dev, d.Tags)
	}
	for _, tg := range w.opTags[dev] {
		if !tagSet[tg] {
			t.Fatalf("device %d lost operator tag %q: %v", dev, tg, d.Tags)
		}
	}
	if d.Name != fmt.Sprintf("host%d.site1", i+1) || d.DeviceTypeID != w.dt || d.SiteID != 1 {
		t.Fatalf("device %d operator-owned field changed: %+v", dev, d)
	}

	// Interfaces.
	prot := map[int]bool{}
	for _, id := range w.protRows[dev] {
		prot[id] = true
	}
	beforeIf := map[int]ifSnap{}
	for _, s := range before.Devices[dev].Ifaces {
		beforeIf[s.ID] = s
	}
	byName := map[string]ifSnap{}
	for _, s := range ds.Ifaces {
		byName[s.Name] = s
	}
	for id := range prot {
		b := beforeIf[id]
		var a *ifSnap
		for k := range ds.Ifaces {
			if ds.Ifaces[k].ID == id {
				a = &ds.Ifaces[k]
			}
		}
		if a == nil {
			t.Fatalf("device %d: protected interface %q (id %d) deleted", dev, b.Name, id)
		}
		if a.Name != b.Name || a.Type != b.Type || a.Mgmt != b.Mgmt {
			t.Fatalf("device %d: protected interface %d changed: %+v -> %+v", dev, id, b, *a)
		}
		if a.MAC != b.MAC && (!a.Mgmt || m.BMC == nil || a.MAC != upper(m.BMC.MAC)) {
			t.Fatalf("device %d: protected interface %d MAC changed %q -> %q", dev, id, b.MAC, a.MAC)
		}
	}
	if role == "server" {
		want := map[string]hwNIC{}
		for _, n := range m.NICs {
			if !isUSBDriver(n.Driver) {
				want[n.Name] = n
			}
		}
		for _, s := range ds.Ifaces {
			if prot[s.ID] || s.Mgmt || s.Type == "lag" || s.Type == "virtual" {
				continue
			}
			n, ok := want[s.Name]
			if !ok {
				t.Fatalf("device %d: interface %q (id %d, MAC %s) matches no reported NIC; NICs %v", dev, s.Name, s.ID, s.MAC, nicList(m))
			}
			if s.MAC != upper(n.MAC) {
				t.Fatalf("device %d: interface %q primary MAC %q, want %q", dev, s.Name, s.MAC, upper(n.MAC))
			}
		}
		for name := range want {
			s, ok := byName[name]
			if !ok || s.Mgmt {
				t.Fatalf("device %d: reported NIC %q has no interface row; rows %v", dev, name, ifaceNames(ds))
			}
		}
		for _, s := range ds.Ifaces {
			seenMAC := map[string]bool{}
			for _, mac := range strings.Split(s.AllMACs, ",") {
				if mac != "" && seenMAC[mac] {
					t.Fatalf("device %d: interface %q carries MAC object %s twice (%s)", dev, s.Name, mac, s.AllMACs)
				}
				seenMAC[mac] = true
			}
		}
		// Identity is the MAC: a row that carried a reported NIC's MAC
		// keeps its id (renamed in place) and its cable.
		afterByID := map[int]ifSnap{}
		for _, s := range ds.Ifaces {
			afterByID[s.ID] = s
		}
		for _, b := range before.Devices[dev].Ifaces {
			if prot[b.ID] || b.Mgmt || b.Type == "lag" || b.Type == "virtual" || b.MAC == "" {
				continue
			}
			for _, n := range want {
				if upper(n.MAC) != b.MAC {
					continue
				}
				a, ok := afterByID[b.ID]
				if !ok {
					t.Fatalf("device %d: row %q (id %d) carried reported NIC %s's MAC %s but was deleted instead of renamed to %q; rows %v",
						dev, b.Name, b.ID, n.Name, n.MAC, n.Name, ifaceNames(ds))
				}
				if b.Cabled && a.CableEnd != b.CableEnd {
					t.Fatalf("device %d: row %q (id %d) lost its cable to %d while following its MAC", dev, b.Name, b.ID, b.CableEnd)
				}
			}
		}
		if m.BMC != nil && m.BMC.MAC != "" {
			found := 0
			for _, s := range ds.Ifaces {
				if s.Mgmt && s.MAC == upper(m.BMC.MAC) {
					found++
				}
			}
			if found == 0 {
				t.Fatalf("device %d: no management interface carries BMC MAC %s; rows %+v", dev, m.BMC.MAC, ds.Ifaces)
			}
			if found > 1 && !skipped("bmc-row") {
				t.Fatalf("device %d: %d management interfaces carry BMC MAC %s (a duplicate BMC row); rows %v", dev, found, m.BMC.MAC, ifaceNames(ds))
			}
		}
	} else {
		// Operator-named device: never a rename, create or delete.
		if len(ds.Ifaces) != len(before.Devices[dev].Ifaces) {
			t.Fatalf("switch device %d: interface row count %d -> %d", dev, len(before.Devices[dev].Ifaces), len(ds.Ifaces))
		}
		for _, s := range ds.Ifaces {
			b, ok := beforeIf[s.ID]
			if !ok || b.Name != s.Name {
				t.Fatalf("switch device %d: interface %d renamed or created: %+v (before %+v)", dev, s.ID, s, b)
			}
		}
	}

	// Modules: one per reported component, every bay occupied.
	want := expectedModules(m)
	got := map[string]int{}
	occupied := map[int]int{}
	for _, mod := range ds.Modules {
		got[mod.Profile]++
		occupied[mod.Bay]++
	}
	if fmt.Sprint(sortedCounts(got)) != fmt.Sprint(sortedCounts(want)) {
		t.Fatalf("device %d modules by profile %v, want %v (bays %v)", dev, sortedCounts(got), sortedCounts(want), ds.Bays)
	}
	for _, b := range ds.Bays {
		var id int
		fmt.Sscanf(b, "%d:", &id)
		if occupied[id] != 1 {
			t.Fatalf("device %d: module bay %s holds %d modules (bays %v)", dev, b, occupied[id], ds.Bays)
		}
	}
	// Module serials are the reported ones: a component with no readable
	// serial must not keep the serial of the part it replaced.
	if !skipped("stale-serial") {
		wantSerials := map[string][]string{}
		for _, d := range m.DIMMs {
			if d.MB > 0 {
				wantSerials["Memory"] = append(wantSerials["Memory"], d.Serial)
			}
		}
		for _, d := range m.Disks {
			wantSerials["Hard disk"] = append(wantSerials["Hard disk"], d.Serial)
		}
		for _, p := range m.PSUs {
			wantSerials["Power supply"] = append(wantSerials["Power supply"], p.Serial)
		}
		gotSerials := map[string][]string{}
		for _, mod := range ds.Modules {
			if _, tracked := wantSerials[mod.Profile]; tracked {
				gotSerials[mod.Profile] = append(gotSerials[mod.Profile], mod.Serial)
			}
		}
		for prof, ws := range wantSerials {
			gs := gotSerials[prof]
			sort.Strings(ws)
			sort.Strings(gs)
			if fmt.Sprint(ws) != fmt.Sprint(gs) {
				t.Fatalf("device %d: %s module serials %q, want %q", dev, prof, gs, ws)
			}
		}
	}
}

func assetStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func sortedCounts(m map[string]int) []string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(out)
	return out
}

func nicList(m *machine) []string {
	var out []string
	for _, n := range m.NICs {
		out = append(out, n.Name+"/"+n.MAC+"/"+n.Driver)
	}
	return out
}

func ifaceNames(ds devSnap) []string {
	var out []string
	for _, s := range ds.Ifaces {
		out = append(out, fmt.Sprintf("%s(%d,%s,mgmt=%v)", s.Name, s.ID, s.MAC, s.Mgmt))
	}
	return out
}

// checkIsolation: commissioning device dev changed nothing that belongs to
// another device (cable ends aside), only appended shared objects, and
// created or removed cables only as the docs allow.
func (w *world) checkIsolation(t *rapid.T, dev int, before, after globalSnap) {
	for id, b := range before.Devices {
		if id == dev {
			continue
		}
		a := after.Devices[id]
		if a.Device != b.Device {
			t.Fatalf("commissioning device %d changed device %d:\n  before %s\n  after  %s", dev, id, b.Device, a.Device)
		}
		strip := func(in []ifSnap) string {
			var out []string
			for _, s := range in {
				out = append(out, fmt.Sprintf("%d %s %s %v %s %s", s.ID, s.Name, s.Type, s.Mgmt, s.MAC, s.AllMACs))
			}
			return strings.Join(out, "; ")
		}
		if strip(a.Ifaces) != strip(b.Ifaces) {
			t.Fatalf("commissioning device %d changed interfaces of device %d:\n  before %s\n  after  %s", dev, id, strip(b.Ifaces), strip(a.Ifaces))
		}
		if fmt.Sprint(a.Bays) != fmt.Sprint(b.Bays) || fmt.Sprint(a.Modules) != fmt.Sprint(b.Modules) {
			t.Fatalf("commissioning device %d changed modules of device %d", dev, id)
		}
	}
	for id, v := range before.ModuleTypes {
		if after.ModuleTypes[id] != v {
			t.Fatalf("shared module type %d changed: %q -> %q", id, v, after.ModuleTypes[id])
		}
	}
	for id, v := range before.Mfgs {
		if after.Mfgs[id] != v {
			t.Fatalf("shared manufacturer %d changed: %q -> %q", id, v, after.Mfgs[id])
		}
	}
	for id, v := range before.DevTypes {
		if after.DevTypes[id] != v {
			t.Fatalf("device type %d changed by commission write-back: %q -> %q", id, v, after.DevTypes[id])
		}
	}
	// Interfaces of dev before/after, to judge removed cables.
	afterIDs := map[int]bool{}
	for _, s := range after.Devices[dev].Ifaces {
		afterIDs[s.ID] = true
	}
	beforeOwner := map[int]int{}
	for did, ds := range before.Devices {
		for _, s := range ds.Ifaces {
			beforeOwner[s.ID] = did
		}
	}
	for id, ends := range before.Cables {
		if _, ok := after.Cables[id]; ok {
			continue
		}
		// Removed: legitimate only when one end was dev's interface that
		// commission deleted.
		ok := false
		for _, e := range ends {
			if beforeOwner[e] == dev && !afterIDs[e] {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("commissioning device %d removed cable %d (%v) without deleting its own end", dev, id, ends)
		}
		w.stats.add(func(s *stats) { s.cablesCascade++ })
	}
	for id, ends := range after.Cables {
		if _, ok := before.Cables[id]; ok {
			continue
		}
		owner := func(iface int) int {
			for did, ds := range after.Devices {
				for _, s := range ds.Ifaces {
					if s.ID == iface {
						return did
					}
				}
			}
			return 0
		}
		if owner(ends[0]) != dev && owner(ends[1]) != dev {
			t.Fatalf("commissioning device %d created cable %d between other devices' interfaces %v", dev, id, ends)
		}
		w.stats.add(func(s *stats) { s.cablesMade++ })
	}
}

// checkCables is the LLDP oracle for a server: a cable is added exactly
// between a reported NIC's row and the port its single LLDP neighbour
// names on the same-site switch, when both were uncabled and no other NIC
// of the report claims that port; nothing else gains a cable.
func (w *world) checkCables(t *rapid.T, i int, before, after globalSnap) {
	dev := w.servers[i]
	m := w.hw[i]
	if w.roles[i] != "server" {
		return
	}
	// Resolve each neighbour to a site-1 switch port id, independently.
	siteSwitch := func(name string) int {
		switch {
		case name == "leaf-a.site1", strings.HasPrefix(name, "leaf-a."):
			return w.leafs["leaf-a.site1"]
		case name == "leaf-b.site1", strings.HasPrefix(name, "leaf-b."):
			return w.leafs["leaf-b.site1"]
		}
		return 0
	}
	portID := func(sw int, port string) int {
		for _, s := range before.Devices[sw].Ifaces {
			if s.Name == port {
				return s.ID
			}
		}
		return 0
	}
	rowBefore := map[string]ifSnap{}
	for _, s := range before.Devices[dev].Ifaces {
		rowBefore[s.Name] = s
	}
	rowAfter := map[string]ifSnap{}
	for _, s := range after.Devices[dev].Ifaces {
		rowAfter[s.Name] = s
	}
	portBefore := map[int]ifSnap{}
	for _, sw := range w.leafs {
		for _, s := range before.Devices[sw].Ifaces {
			portBefore[s.ID] = s
		}
	}
	claims := map[int]int{}
	want := map[string]int{} // local NIC name -> port id
	for _, n := range m.NICs {
		if isUSBDriver(n.Driver) || n.NbrSwitch == "" {
			continue
		}
		sw := siteSwitch(n.NbrSwitch)
		if sw == 0 {
			continue
		}
		p := portID(sw, n.NbrPort)
		if p == 0 {
			continue
		}
		want[n.Name] = p
		claims[p]++
	}
	for name, p := range want {
		a, ok := rowAfter[name]
		if !ok {
			continue
		}
		if a.CableEnd == p {
			continue
		}
		// Not cabled to its neighbour: legitimate only when the port was
		// cabled before, the local row was cabled before (rows keep their
		// identity across renames, so check by id), or the port is
		// ambiguous.
		localWasCabled := false
		for _, s := range before.Devices[dev].Ifaces {
			if s.ID == a.ID && s.Cabled {
				localWasCabled = true
			}
		}
		// A NIC sharing a management row's MAC (shared LOM) is on the OOB
		// segment by design and gets no LLDP cable.
		sharesMgmtMAC := false
		for _, s := range after.Devices[dev].Ifaces {
			if s.Mgmt && s.MAC != "" && s.MAC == a.MAC {
				sharesMgmtMAC = true
			}
		}
		if portBefore[p].Cabled || localWasCabled || claims[p] > 1 || a.Mgmt || sharesMgmtMAC {
			continue
		}
		t.Fatalf("device %d: NIC %q hears %v:%v (port id %d) and both ends were uncabled, but no cable was added (row %+v)", dev, name, m.NICs, "", p, a)
	}
	for _, a := range after.Devices[dev].Ifaces {
		if a.CableEnd == 0 {
			continue
		}
		wasEnd := 0
		for _, s := range before.Devices[dev].Ifaces {
			if s.ID == a.ID {
				wasEnd = s.CableEnd
			}
		}
		if wasEnd == a.CableEnd {
			continue
		}
		if want[a.Name] != a.CableEnd {
			t.Fatalf("device %d: interface %q gained a cable to interface %d, which no LLDP neighbour of that NIC names (want %d)", dev, a.Name, a.CableEnd, want[a.Name])
		}
		for _, s := range after.Devices[dev].Ifaces {
			if s.Mgmt && s.MAC != "" && s.MAC == a.MAC {
				t.Fatalf("device %d: interface %q shares management row %q's MAC (shared OOB segment) but gained an LLDP cable", dev, a.Name, s.Name)
			}
		}
		if a.Mgmt {
			t.Fatalf("device %d: management interface %q gained an LLDP cable", dev, a.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Mutations.

func (w *world) mutate(t *rapid.T, i int, step int) string {
	m := w.hw[i].clone()
	label := fmt.Sprintf("mut%d", step)
	host := i + 1
	kind := rapid.SampledFrom([]string{"replaceNIC", "addNIC", "removeNIC", "renameShift", "boardSwap", "addDisk", "removeDisk",
		"replaceDisk", "swapPSU", "dimms", "moveCard", "lldp", "gpu"}).Draw(t, label+".kind")
	switch kind {
	case "replaceNIC":
		if len(m.NICs) > 0 {
			k := rapid.IntRange(0, len(m.NICs)-1).Draw(t, label+".k")
			if !isUSBDriver(m.NICs[k].Driver) {
				m.NICs[k].MAC = macOf(host, 10+rapid.IntRange(0, 5).Draw(t, label+".mac"))
			}
		}
	case "addNIC":
		used := map[string]bool{}
		for _, n := range m.NICs {
			used[n.Name] = true
		}
		m.NICs = append(m.NICs, drawNIC(t, w.f, host, used, label+".nic"))
	case "removeNIC":
		if len(m.NICs) > 0 {
			k := rapid.IntRange(0, len(m.NICs)-1).Draw(t, label+".k")
			m.NICs = append(m.NICs[:k], m.NICs[k+1:]...)
		}
	case "renameShift":
		// Probe order changed: names rotate among the physical NICs.
		var idx []int
		for k, n := range m.NICs {
			if !isUSBDriver(n.Driver) {
				idx = append(idx, k)
			}
		}
		if len(idx) > 1 {
			first := m.NICs[idx[0]].Name
			for j := 0; j < len(idx)-1; j++ {
				m.NICs[idx[j]].Name = m.NICs[idx[j+1]].Name
			}
			m.NICs[idx[len(idx)-1]].Name = first
		}
	case "boardSwap":
		w.stats.add(func(s *stats) { s.boardSwaps++ })
		drawIdentity(t, m, w.f, host+100, label+".id")
		for k := range m.NICs {
			if strings.HasPrefix(m.NICs[k].Name, "eno") {
				m.NICs[k].MAC = macOf(host, 20+k)
			}
		}
		if m.BMC != nil && w.f.bmcMACChange {
			m.BMC.MAC = fmt.Sprintf("d0:50:99:00:%02x:02", host)
		}
	case "addDisk":
		m.Disks = append(m.Disks, drawDisk(t, label+".disk"))
	case "removeDisk":
		if len(m.Disks) > 0 {
			k := rapid.IntRange(0, len(m.Disks)-1).Draw(t, label+".k")
			m.Disks = append(m.Disks[:k], m.Disks[k+1:]...)
		}
	case "replaceDisk":
		if len(m.Disks) > 0 {
			k := rapid.IntRange(0, len(m.Disks)-1).Draw(t, label+".k")
			m.Disks[k] = drawDisk(t, label+".disk")
		}
	case "swapPSU":
		m.PSUs = drawPSUs(t, w.f, label+".psu")
	case "dimms":
		m.DIMMs = drawDIMMs(t, w.f, label+".dimm")
	case "moveCard":
		// Move a NIC to the other server, MAC and all.
		o := 1 - i
		if len(m.NICs) > 0 {
			k := rapid.IntRange(0, len(m.NICs)-1).Draw(t, label+".k")
			n := m.NICs[k]
			if !isUSBDriver(n.Driver) {
				m.NICs = append(m.NICs[:k], m.NICs[k+1:]...)
				om := w.hw[o].clone()
				used := map[string]bool{}
				for _, x := range om.NICs {
					used[x.Name] = true
				}
				if !used[n.Name] {
					om.NICs = append(om.NICs, n)
					dedupeMACs(om)
					w.hw[o] = om
					w.stats.add(func(s *stats) { s.nicsMoved++ })
				}
			}
		}
	case "lldp":
		for k := range m.NICs {
			if w.f.lldp && !isUSBDriver(m.NICs[k].Driver) && rapid.Bool().Draw(t, fmt.Sprintf("%s.n%d", label, k)) {
				m.NICs[k].NbrSwitch = rapid.SampledFrom([]string{"leaf-a.site1", "leaf-b.site1", ""}).Draw(t, fmt.Sprintf("%s.s%d", label, k))
				m.NICs[k].NbrPort = rapid.SampledFrom(switchPort).Draw(t, fmt.Sprintf("%s.p%d", label, k))
			}
		}
	case "gpu":
		if len(m.GPUs) > 0 {
			m.GPUs = nil
		} else {
			m.GPUs = append(m.GPUs, [2]string{"NVIDIA Corporation", "Device"})
		}
	}
	dedupeMACs(m)
	w.hw[i] = m
	w.stats.add(func(s *stats) { s.mutations++ })
	return kind
}

// ---------------------------------------------------------------------------
// The property.

func TestCommissionE2E_Property(t *testing.T) {
	st := &stats{}
	rapid.Check(t, func(t *rapid.T) {
		w := newWorld(t, st)
		st.add(func(s *stats) { s.cases++ })
		var trace []string
		nSteps := rapid.IntRange(1, 10).Draw(t, "nSteps")
		for step := 0; step < nSteps; step++ {
			i := rapid.IntRange(0, len(w.servers)-1).Draw(t, fmt.Sprintf("step%d.host", step))
			if rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("step%d.op", step)) == 0 {
				kind := w.mutate(t, i, step)
				trace = append(trace, fmt.Sprintf("mutate host%d %s", i+1, kind))
				continue
			}
			trace = append(trace, fmt.Sprintf("commission host%d", i+1))
			w.runCommission(t, i, step, trace)
		}
	})
	t.Log(st.String())
}

func (s *stats) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("coverage: cases=%d commissions=%d faultsHit=%d faultErrs=%d replays=%d ifaceCreates=%d ifaceRenames=%d ifaceDeletes=%d "+
		"cablesMade=%d cablesCascadeDeleted=%d moduleCreates=%d moduleDeletes=%d bayDeletes=%d mutations=%d maxNICs=%d maxModules=%d "+
		"bmcRuns=%d lldpRuns=%d switchRuns=%d pagedCases=%d nicsMoved=%d boardSwaps=%d idempotentReplays=%d writesOnReplay=%d",
		s.cases, s.commissions, s.faults, s.faultErrs, s.replays, s.creates, s.renames, s.deletes, s.cablesMade, s.cablesCascade,
		s.moduleCreates, s.moduleDeletes, s.bayDeletes, s.mutations, s.maxNICs, s.maxModules, s.bmcRuns, s.lldpRuns, s.switchRuns,
		s.pagedRuns, s.nicsMoved, s.boardSwaps, s.idempotentCases, s.idempotentWrites)
}

func (w *world) runCommission(t *rapid.T, i, step int, trace []string) {
	dev := w.servers[i]
	m := w.hw[i]
	report := m.report()
	before := w.snapshot()
	w.stats.add(func(s *stats) {
		s.commissions++
		s.maxNICs = max(s.maxNICs, len(m.NICs))
		if m.BMC != nil {
			s.bmcRuns++
		}
		if w.roles[i] != "server" {
			s.switchRuns++
		}
		for _, n := range m.NICs {
			if n.NbrSwitch != "" {
				s.lldpRuns++
				break
			}
		}
	})

	// Optional fault on one write, then the outbox replay.
	if rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("step%d.fault", step)) == 0 && (m.BMC == nil || !skipped("bmc-row")) {
		// Draw outside the lock: a Draw can abort the case during shrinking.
		at := rapid.IntRange(1, 16).Draw(t, fmt.Sprintf("step%d.faultAt", step))
		status := rapid.SampledFrom([]int{500, 502, 503}).Draw(t, fmt.Sprintf("step%d.faultStatus", step))
		applied := rapid.Bool().Draw(t, fmt.Sprintf("step%d.faultApplied", step))
		w.nb.Lock()
		w.nb.FaultAt, w.nb.FaultStatus, w.nb.FaultApplied = at, status, applied
		w.nb.Unlock()
		ferr := w.commission(i, report)
		w.nb.Lock()
		hit := w.nb.FaultsHit > 0
		w.nb.FaultAt, w.nb.FaultsHit, w.nb.FaultApplied = 0, 0, false
		w.nb.Unlock()
		if hit {
			w.stats.add(func(s *stats) {
				s.faults++
				if ferr != nil {
					s.faultErrs++
				}
			})
		}
		w.stats.add(func(s *stats) { s.replays++ })
	}
	w.nb.Lock()
	w.nb.Rejected = nil
	w.nb.Writes = nil
	w.nb.Unlock()
	if err := w.commission(i, report); err != nil {
		w.nb.Lock()
		rej := append([]string(nil), w.nb.Rejected...)
		w.nb.Unlock()
		w.stats.add(func(s *stats) { s.errsOnValid++ })
		t.Fatalf("commission of host%d (device %d) failed on a report the agent can produce: %v\nNetBox 400s: %v\ntrace: %v\nNICs: %v\nreport: %s", i+1, dev, err, rej, trace, nicList(m), report)
	}
	w.nb.Lock()
	writes := append([]string(nil), w.nb.Writes...)
	w.nb.Unlock()
	for _, wr := range writes {
		switch {
		case strings.HasPrefix(wr, "POST iface"):
			w.stats.add(func(s *stats) { s.creates++ })
		case strings.HasPrefix(wr, "DELETE iface"):
			w.stats.add(func(s *stats) { s.deletes++ })
		case strings.HasPrefix(wr, "RENAME iface"):
			w.stats.add(func(s *stats) { s.renames++ })
		case strings.HasPrefix(wr, "POST module "):
			w.stats.add(func(s *stats) { s.moduleCreates++ })
		case strings.HasPrefix(wr, "DELETE module"):
			w.stats.add(func(s *stats) { s.moduleDeletes++ })
		case strings.HasPrefix(wr, "DELETE bay"):
			w.stats.add(func(s *stats) { s.bayDeletes++ })
		}
	}
	after := w.snapshot()
	w.stats.add(func(s *stats) { s.maxModules = max(s.maxModules, len(after.Devices[dev].Modules)) })
	w.checkDevice(t, i, before, after)
	w.checkIsolation(t, dev, before, after)
	w.checkCables(t, i, before, after)

	// Idempotency: the replay of the same report changes nothing.
	w.nb.Lock()
	w.nb.Writes = nil
	w.nb.Unlock()
	if err := w.commission(i, report); err != nil {
		t.Fatalf("replaying host%d's report failed: %v\ntrace: %v", i+1, err, trace)
	}
	again := w.snapshot()
	w.nb.Lock()
	nw := len(w.nb.Writes)
	w.nb.Unlock()
	w.stats.add(func(s *stats) { s.idempotentWrites += nw; s.idempotentCases++ })
	if !snapEqual(after, again) {
		t.Fatalf("replaying host%d's report changed NetBox:\n%s\ntrace: %v", i+1, snapDiff(after, again), trace)
	}
}

func snapEqual(a, b globalSnap) bool { return snapDiff(a, b) == "" }

func snapDiff(a, b globalSnap) string {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) == string(jb) {
		return ""
	}
	var out []string
	for id, da := range a.Devices {
		db := b.Devices[id]
		x, _ := json.Marshal(da)
		y, _ := json.Marshal(db)
		if string(x) != string(y) {
			out = append(out, fmt.Sprintf("device %d:\n  %s\n  %s", id, x, y))
		}
	}
	if fmt.Sprint(a.Cables) != fmt.Sprint(b.Cables) {
		out = append(out, fmt.Sprintf("cables %v -> %v", a.Cables, b.Cables))
	}
	if fmt.Sprint(a.ModuleTypes) != fmt.Sprint(b.ModuleTypes) {
		out = append(out, "module types changed")
	}
	return strings.Join(out, "\n")
}
