package netbox

// Round trip: the agent's wire struct (inventory.CommissionResult) ->
// json.Marshal -> CommissionToSync. The producer and consumer share no Go
// type, so this checks the consumer decodes every generated report and
// preserves the identity-bearing fields: the authoritative serial, every
// non-BMC-USB NIC's name/MAC/speed, the BMC management NIC, DIMM and disk
// module sizes (GiB, floor), and one module per CPU socket.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/agent/inventory"
)

func genCommissionResult(t *rapid.T) inventory.CommissionResult {
	str := func(label string, pool ...string) string { return rapid.SampledFrom(pool).Draw(t, label) }
	cr := inventory.CommissionResult{Hostname: "host"}
	if rapid.IntRange(0, 9).Draw(t, "hasSystem") > 0 {
		cr.System = &inventory.SystemInfo{
			Manufacturer:  str("mfg", "Supermicro", "ASUSTeK Computer Inc.", "Dell Inc.", "Gigabyte é"),
			ProductName:   str("product", "AS -1115HS-TNR", "PowerEdge R740", "RS501A-E12-RS4U"),
			SerialNumber:  str("serial", "S515AB0001", "To Be Filled By O.E.M.", "", "  S515AB0002  ", "Default string"),
			UUID:          "4f1d62c0-1f7d-4e74-a57c-a58932e2aa11",
			ChassisSerial: str("chassis", "", "CH1190001", "0123456789", "Chassis Serial Number"),
			AssetTag:      str("asset", "", "Default string", "TAG-1"),
			BIOSVersion:   str("bios", "", "2.1a"),
		}
	}
	for i := 0; i < rapid.IntRange(0, 4).Draw(t, "nNIC"); i++ {
		cr.NICs = append(cr.NICs, inventory.NICInfo{
			Name:   fmt.Sprintf("eno%d", i+1),
			MAC:    fmt.Sprintf("3c:ec:ef:00:00:%02x", i),
			Driver: str("driver", "mlx5_core", "ice", "cdc_ether", "rndis_host", "igb"),
			Speed:  str("speed", "", "Unknown!", "1000Mb/s", "25000Mb/s", "100000Mb/s"),
		})
	}
	if rapid.Bool().Draw(t, "hasBMC") {
		cr.BMC = &inventory.BMCInfo{
			IP:              str("bmcIP", "", "10.250.3.17"),
			MAC:             str("bmcMAC", "", "d0:50:99:ab:cd:ef"),
			FirmwareVersion: str("bmcFW", "", "1.73"),
		}
	}
	nd := rapid.IntRange(0, 4).Draw(t, "nDIMM")
	if nd > 0 || rapid.Bool().Draw(t, "hasMem") {
		cr.Memory = &inventory.MemoryInfo{TotalMB: 515000}
		for i := 0; i < nd; i++ {
			cr.Memory.DIMMs = append(cr.Memory.DIMMs, inventory.DIMMInfo{
				Locator: fmt.Sprintf("DIMM_A%d", i), SizeMB: rapid.SampledFrom([]int{512, 8192, 32768, 65536}).Draw(t, "dimmMB"),
				Type: "DDR4", Speed: "3200 MT/s",
			})
		}
	}
	for i := 0; i < rapid.IntRange(0, 3).Draw(t, "nDisk"); i++ {
		cr.Disks = append(cr.Disks, inventory.DiskInfo{
			Name: fmt.Sprintf("nvme%dn1", i), Model: str("diskModel", "", "Samsung SSD 990 PRO 4TB"),
			SizeBytes: rapid.SampledFrom([]int64{0, 480103981056, 4000787030016, 15360747397120}).Draw(t, "diskBytes"),
			Transport: str("tran", "nvme", "sata", "usb", ""),
		})
	}
	for i := 0; i < rapid.IntRange(0, 2).Draw(t, "nCPU"); i++ {
		cr.CPUs = append(cr.CPUs, inventory.CPUInfo{Model: "AMD EPYC 9654 96-Core Processor", Cores: 96, Threads: 192, Speed: "3700.0000 MHz"})
	}
	return cr
}

func TestCommissionToSync_Property_RoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cr := genCommissionResult(t)
		data, err := json.Marshal(cr)
		if err != nil {
			t.Fatal(err)
		}
		sync, err := CommissionToSync(42, data)
		if err != nil {
			t.Fatalf("CommissionToSync rejected an agent-produced report: %v\n%s", err, data)
		}
		if cr.System != nil && sync.SystemSerial != cr.System.SerialNumber {
			t.Fatalf("SystemSerial %q, want %q", sync.SystemSerial, cr.System.SerialNumber)
		}
		var wantNICs []inventory.NICInfo
		for _, n := range cr.NICs {
			if n.Driver != "cdc_ether" && n.Driver != "rndis_host" {
				wantNICs = append(wantNICs, n)
			}
		}
		var gotHost []NICData
		var gotBMC []NICData
		for _, n := range sync.NICs {
			if n.MgmtOnly {
				gotBMC = append(gotBMC, n)
			} else {
				gotHost = append(gotHost, n)
			}
		}
		if len(gotHost) != len(wantNICs) {
			t.Fatalf("got %d host NICs, want %d", len(gotHost), len(wantNICs))
		}
		for i, w := range wantNICs {
			g := gotHost[i]
			mbps := 0
			fmt.Sscanf(strings.TrimSuffix(w.Speed, "Mb/s"), "%d", &mbps)
			if g.Name != w.Name || g.MAC != w.MAC || g.Speed != mbps*1000 {
				t.Fatalf("NIC %d: got %+v, want name=%q mac=%q speed=%dkbps", i, g, w.Name, w.MAC, mbps*1000)
			}
		}
		wantBMC := cr.BMC != nil && cr.BMC.MAC != ""
		if (len(gotBMC) == 1) != wantBMC || len(gotBMC) > 1 {
			t.Fatalf("BMC mgmt NICs: got %d, want present=%v", len(gotBMC), wantBMC)
		}
		count := map[string]int{}
		for _, m := range sync.Modules {
			count[m.ProfileName]++
			var attrs map[string]any
			_ = json.Unmarshal(m.AttributeData, &attrs)
			switch m.ProfileName {
			case "Memory":
				var want int
				for _, d := range cr.Memory.DIMMs {
					if d.Locator == m.BayName {
						want = d.SizeMB / 1024
					}
				}
				if int(attrs["size"].(float64)) != want {
					t.Fatalf("DIMM %s size attr %v, want %d GiB", m.BayName, attrs["size"], want)
				}
			case "Hard disk":
				var idx int
				fmt.Sscanf(m.BayName[strings.LastIndex(m.BayName, "_")+1:], "%d", &idx)
				if want := cr.Disks[idx].SizeBytes >> 30; int64(attrs["size"].(float64)) != want {
					t.Fatalf("disk %s size attr %v, want %d GiB", m.BayName, attrs["size"], want)
				}
			}
		}
		if cr.Memory != nil && count["Memory"] != len(cr.Memory.DIMMs) {
			t.Fatalf("Memory modules %d, want %d", count["Memory"], len(cr.Memory.DIMMs))
		}
		if count["Hard disk"] != len(cr.Disks) || count["CPU"] != len(cr.CPUs) {
			t.Fatalf("modules: %v, want disks=%d cpus=%d", count, len(cr.Disks), len(cr.CPUs))
		}
	})
}
