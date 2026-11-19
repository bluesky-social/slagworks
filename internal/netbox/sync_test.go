package netbox

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseSpeedKbps(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"25000Mb/s", 25_000_000},
		{"10000 Mb/s", 10_000_000},
		{"1000Mb/s", 1_000_000},
		{"100 Mb/s", 100_000},
		{"0", 0},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseSpeedKbps(tt.in); got != tt.want {
				t.Errorf("parseSpeedKbps(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSpeedGHz(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"3400.000 MHz", 3.4},
		{"2500 MHz", 2.5},
		{"1800MHz", 1.8},
		{"0 MHz", 0},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseSpeedGHz(tt.in); got != tt.want {
				t.Errorf("parseSpeedGHz(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseDataRate(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"4800 MT/s", 4800},
		{"3200 MT/s", 3200},
		{"6400", 6400},
		{"", 0},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseDataRate(tt.in); got != tt.want {
				t.Errorf("parseDataRate(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestCleanXcvrIdentifier(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"0x03 (SFP)", "SFP"},
		{"0x11 (QSFP28)", "QSFP28"},
		{"0x18 (QSFP-DD)", "QSFP-DD"},
		{"raw", "raw"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := cleanXcvrIdentifier(tt.in); got != tt.want {
				t.Errorf("cleanXcvrIdentifier(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractCPUManufacturer(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"Intel(R) Xeon(R) Silver 4314 CPU @ 2.40GHz", "Intel"},
		{"AMD EPYC 9554 64-Core Processor", "AMD"},
		{"AMD Ryzen 9 7950X", "AMD"},
		{"EPYC 9654", "AMD"},
		{"Something exotic", "Unknown"},
		{"", "Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := extractCPUManufacturer(tt.model); got != tt.want {
				t.Errorf("extractCPUManufacturer(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestIsJunkDMIValue(t *testing.T) {
	junk := []string{
		"",
		"Default string",
		"default string",
		"DEFAULT STRING",
		"To Be Filled By O.E.M.",
		"to be filled by o.e.m.",
		"N/A",
		"n/a",
		"Not Specified",
		"None",
		"Unknown",
		"unknown",
		"00",
		"0",
	}
	for _, v := range junk {
		t.Run("junk/"+v, func(t *testing.T) {
			if !isJunkDMIValue(v) {
				t.Errorf("isJunkDMIValue(%q) = false, want true", v)
			}
		})
	}
	real := []string{
		"WM235S600107",
		"ASUSTeK Computer Inc.",
		"RS501A-E12-RS4U",
		"1.23",
	}
	for _, v := range real {
		t.Run("real/"+v, func(t *testing.T) {
			if isJunkDMIValue(v) {
				t.Errorf("isJunkDMIValue(%q) = true, want false", v)
			}
		})
	}
}

// TestCommissionToSync_MalformedJSON verifies the error path for
// unmarshalable JSON, the case netboxtest's
// TestCommissionReport_MalformedPayload depends on.
func TestCommissionToSync_MalformedJSON(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"truncated", `{"system":`},
		{"invalid syntax", `{not valid json`},
		{"wrong top-level", `"just a string"`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CommissionToSync(42, json.RawMessage(tt.raw))
			if err == nil {
				t.Errorf("expected error on malformed JSON %q", tt.raw)
			}
		})
	}
}

// TestCommissionToSync_EmptyPayload verifies an empty but valid JSON
// object succeeds with minimal sync — no crashes on nil sub-blocks.
func TestCommissionToSync_EmptyPayload(t *testing.T) {
	sync, err := CommissionToSync(42, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sync == nil {
		t.Fatal("nil sync")
	}
	if sync.DeviceID != 42 {
		t.Errorf("DeviceID = %d, want 42", sync.DeviceID)
	}
	if len(sync.NICs) != 0 || len(sync.Modules) != 0 {
		t.Errorf("unexpected collections in empty sync: NICs=%d modules=%d",
			len(sync.NICs), len(sync.Modules))
	}
}

// TestCommissionToSync_SystemFields verifies the basic system-block
// decode populates the flat Device fields on CommissionSync.
func TestCommissionToSync_SystemFields(t *testing.T) {
	payload := `{
		"system": {
			"manufacturer": "ASUSTeK Computer Inc.",
			"product_name": "RS501A-E12-RS4U",
			"serial_number": "SN-REAL",
			"uuid": "12345678-1234-1234-1234-123456789abc",
			"bios_version": "1.23",
			"asset_tag": "Default string",
			"board_vendor": "ASUSTeK",
			"board_product": "RS501A-MB",
			"board_serial": "BSN-42",
			"chassis_type": "Rack Mount Chassis",
			"chassis_serial": "CSN-99"
		}
	}`
	sync, err := CommissionToSync(7, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sync.SystemSerial != "SN-REAL" {
		t.Errorf("SystemSerial = %q, want SN-REAL", sync.SystemSerial)
	}
	if sync.ChassisSerial != "CSN-99" {
		t.Errorf("ChassisSerial = %q, want CSN-99", sync.ChassisSerial)
	}
	if sync.ProductName != "RS501A-E12-RS4U" {
		t.Errorf("ProductName = %q, want RS501A-E12-RS4U", sync.ProductName)
	}
	// AssetTag "Default string" is junk and must not propagate.
	if sync.AssetTag != "" {
		t.Errorf("AssetTag = %q, want empty (junk filtered)", sync.AssetTag)
	}
	if sync.BIOSVersion != "1.23" {
		t.Errorf("BIOSVersion = %q", sync.BIOSVersion)
	}
	if sync.BoardVendor != "ASUSTeK" || sync.BoardProduct != "RS501A-MB" {
		t.Errorf("Board fields wrong: vendor=%q product=%q", sync.BoardVendor, sync.BoardProduct)
	}
}

// TestCommissionToSync_ProductNameJunkWhitespace pins cleanDMIModelName
// at the decode boundary: Supermicro's "AS -1115HS-TNR" (stray space
// before the hyphen) must normalize to the curated "AS-1115HS-TNR" so
// FindOrCreateDeviceType resolves the operator's device type instead
// of minting a junk duplicate with no slagworks_kernel_params (the
// bootloader refusal).
func TestCommissionToSync_ProductNameJunkWhitespace(t *testing.T) {
	payload := `{"system": {"product_name": "AS -1115HS-TNR", "serial_number": "S500001X0000002"}}`
	sync, err := CommissionToSync(7, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sync.ProductName != "AS-1115HS-TNR" {
		t.Errorf("ProductName = %q, want AS-1115HS-TNR", sync.ProductName)
	}
}

// TestCleanDMIModelName pins the normalization rules: hyphen-adjacent
// whitespace is junk (no legitimate model has it); interior single
// spaces are legitimate ("PowerEdge R740") and survive with runs
// collapsed.
func TestCleanDMIModelName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"AS -1115HS-TNR", "AS-1115HS-TNR"},
		{"AS- 1115HS-TNR", "AS-1115HS-TNR"},
		{"AS - 1115HS - TNR", "AS-1115HS-TNR"},
		{"PowerEdge R740", "PowerEdge R740"},
		{"PowerEdge   R740", "PowerEdge R740"},
		{"  RS501A-E12-RS4U  ", "RS501A-E12-RS4U"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := cleanDMIModelName(tc.in); got != tc.want {
			t.Errorf("cleanDMIModelName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAuthoritativeSerial pins system-serial-first priority: the
// system serial is what the BMC serial-guard compares and what keys
// the bmc-creds-admin secret; chassis is fallback-only, junk rejected
// from both.
func TestAuthoritativeSerial(t *testing.T) {
	cases := []struct {
		name, system, chassis, want string
	}{
		{"system wins when both set", "S500001X0000002", "CH119KN44BP0A95", "S500001X0000002"},
		{"chassis fallback when system junk", "Default string", "CH119KN44BP0A95", "CH119KN44BP0A95"},
		{"chassis fallback when system empty", "", "CSN-1", "CSN-1"},
		{"both junk yields empty", "To Be Filled By O.E.M.", "0", ""},
		{"unprogrammed supermicro serials yield empty", "0123456789", "0123456789", ""},
		{"both empty yields empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := authoritativeSerial(tc.system, tc.chassis); got != tc.want {
				t.Errorf("authoritativeSerial(%q, %q) = %q, want %q", tc.system, tc.chassis, got, tc.want)
			}
		})
	}
}

// TestCommissionToSync_BMCBecomesNIC verifies that a machine with a
// BMC MAC gets a synthetic "ipmi" NIC entry with MgmtOnly=true.
func TestCommissionToSync_BMCBecomesNIC(t *testing.T) {
	payload := `{
		"bmc": {
			"ip": "10.1.2.3",
			"mac": "aa:bb:cc:dd:ee:ff",
			"firmware_version": "5.11"
		}
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if sync.BMCIP != "10.1.2.3" {
		t.Errorf("BMCIP = %q", sync.BMCIP)
	}
	if sync.BMCFWVersion != "5.11" {
		t.Errorf("BMCFWVersion = %q", sync.BMCFWVersion)
	}
	if len(sync.NICs) != 1 {
		t.Fatalf("expected 1 NIC (the BMC), got %d", len(sync.NICs))
	}
	ipmi := sync.NICs[0]
	if ipmi.Name != "ipmi" || ipmi.MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("BMC NIC: name=%q mac=%q", ipmi.Name, ipmi.MAC)
	}
	if !ipmi.MgmtOnly {
		t.Error("BMC NIC should have MgmtOnly=true")
	}
}

// TestCommissionToSync_BMCNoMACNoNIC: a BMC block without a MAC
// should NOT insert a NIC — a management interface we can't reach
// isn't useful to mirror into NetBox.
func TestCommissionToSync_BMCNoMACNoNIC(t *testing.T) {
	payload := `{
		"bmc": {"ip": "10.1.2.3", "firmware_version": "5.11"}
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(sync.NICs) != 0 {
		t.Errorf("expected 0 NICs (no MAC), got %d", len(sync.NICs))
	}
	if sync.BMCIP != "10.1.2.3" {
		t.Errorf("BMCIP should still be set: %q", sync.BMCIP)
	}
}

// TestCommissionToSync_NICs verifies NIC block → NICData conversion,
// including transceiver cleaning and speed parsing.
func TestCommissionToSync_NICs(t *testing.T) {
	payload := `{
		"nics": [
			{
				"name": "eno1",
				"mac": "3c:ec:ef:aa:bb:01",
				"driver": "i40e",
				"speed": "10000Mb/s",
				"firmware_version": "8.2",
				"pci_address": "0000:65:00.0",
				"sriov_max_vfs": 64,
				"interface_type": "10gbase-t",
				"transceiver": {"identifier": "0x11 (QSFP28)"}
			},
			{
				"name": "eno2",
				"mac": "3c:ec:ef:aa:bb:02",
				"speed": "1000 Mb/s"
			}
		]
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(sync.NICs) != 2 {
		t.Fatalf("expected 2 NICs, got %d", len(sync.NICs))
	}
	first := sync.NICs[0]
	if first.Speed != 10_000_000 {
		t.Errorf("NIC[0].Speed = %d, want 10_000_000 (10G in kbps)", first.Speed)
	}
	if first.XcvrType != "QSFP28" {
		t.Errorf("NIC[0].XcvrType = %q, want QSFP28", first.XcvrType)
	}
	if first.SRIOVMaxVFs != 64 {
		t.Errorf("NIC[0].SRIOVMaxVFs = %d, want 64", first.SRIOVMaxVFs)
	}
	second := sync.NICs[1]
	if second.Speed != 1_000_000 {
		t.Errorf("NIC[1].Speed = %d, want 1_000_000", second.Speed)
	}
	if second.XcvrType != "" {
		t.Errorf("NIC[1] should have no XcvrType, got %q", second.XcvrType)
	}
}

func TestCommissionToSync_BMCUSBNICExcludedFromReconcile(t *testing.T) {
	payload := `{
		"nics": [
			{
				"name": "enxbe3af2b6059f",
				"mac": "be:3a:f2:b6:05:9f",
				"driver": "rndis_host",
				"pci_address": "usb-0000:49:00.4-1.1"
			},
			{
				"name": "ens2f0np0",
				"mac": "7c:c2:55:be:f7:3c",
				"driver": "mlx5_core",
				"pci_address": "0000:21:00.0"
			}
		]
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(sync.NICs) != 1 {
		t.Fatalf("expected only the fabric NIC, got %+v", sync.NICs)
	}
	if got := sync.NICs[0]; got.MAC != "7c:c2:55:be:f7:3c" || got.Driver != "mlx5_core" {
		t.Fatalf("unexpected reconciled NIC: %+v", got)
	}
}

// TestCommissionToSync_DIMMsBecomeMemoryModules verifies memory DIMMs
// produce ModuleData entries with the expected BayName/Profile.
func TestCommissionToSync_DIMMsBecomeMemoryModules(t *testing.T) {
	payload := `{
		"memory": {
			"total_mb": 262144,
			"dimms": [
				{"locator": "DIMM_A1", "size_mb": 32768, "type": "DDR5",
				 "speed": "4800 MT/s", "part_number": "M321R4GA3BB6-CQK",
				 "serial_number": "SN1", "manufacturer": "Samsung"},
				{"locator": "DIMM_B1", "size_mb": 32768, "type": "DDR5",
				 "speed": "4800 MT/s", "part_number": "M321R4GA3BB6-CQK",
				 "serial_number": "SN2", "manufacturer": "Samsung"}
			]
		}
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	memCount := 0
	for _, m := range sync.Modules {
		if m.ProfileName == "Memory" {
			memCount++
			if m.Manufacturer != "Samsung" {
				t.Errorf("DIMM Manufacturer = %q", m.Manufacturer)
			}
			// attribute_data is JSON; verify it parses + has expected keys
			var attrs map[string]any
			if err := json.Unmarshal(m.AttributeData, &attrs); err != nil {
				t.Errorf("DIMM AttributeData unparseable: %v", err)
			}
			if size, ok := attrs["size"].(float64); !ok || size != 32 {
				t.Errorf("DIMM size attr = %v, want 32 (GB)", attrs["size"])
			}
			if rate, ok := attrs["data_rate"].(float64); !ok || rate != 4800 {
				t.Errorf("DIMM data_rate attr = %v, want 4800", attrs["data_rate"])
			}
		}
	}
	if memCount != 2 {
		t.Errorf("expected 2 Memory modules, got %d", memCount)
	}
}

// TestCommissionToSync_DisksClassifiedByTransport verifies the
// disk-type heuristic: NVMe, SATA/SAS SSD, and rotational HDD each
// get the right bay prefix.
func TestCommissionToSync_DisksClassifiedByTransport(t *testing.T) {
	// Pointers so rotational flag is present.
	ft := true
	ff := false
	payload := `{
		"disks": [
			{"name": "nvme0n1", "model": "Samsung SSD 990 PRO 4TB",
			 "serial": "S001", "size_bytes": 4000787030016, "transport": "nvme"},
			{"name": "sda", "model": "Kioxia KIOXIA-PM7-V", "serial": "S002",
			 "size_bytes": 960197124096, "transport": "sata", "rotational": false},
			{"name": "sdb", "model": "Seagate ST18000NM004J",
			 "serial": "S003", "size_bytes": 18000207937536, "transport": "sas",
			 "rotational": true}
		]
	}`
	_ = ft
	_ = ff
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	gotBays := make([]string, 0, 3)
	for _, m := range sync.Modules {
		if m.ProfileName == "Hard disk" {
			gotBays = append(gotBays, m.BayName)
		}
	}
	want := map[string]bool{
		"NVMe_0": true, // nvme transport
		"SSD_1":  true, // sata, non-rotational
		"HDD_2":  true, // sas, rotational
	}
	for _, b := range gotBays {
		if !want[b] {
			t.Errorf("unexpected disk bay: %s", b)
		}
		delete(want, b)
	}
	for b := range want {
		t.Errorf("missing expected disk bay: %s", b)
	}
}

// TestCommissionToSync_GPUsFilteredFromPCI verifies that only PCI
// devices classified as GPUs end up as GPU modules.
func TestCommissionToSync_GPUsFilteredFromPCI(t *testing.T) {
	payload := `{
		"pci_devices": [
			{"slot": "01:00.0", "class": "VGA compatible controller",
			 "vendor": "NVIDIA", "device": "H100"},
			{"slot": "02:00.0", "class": "Ethernet controller",
			 "vendor": "Intel", "device": "i40e"},
			{"slot": "03:00.0", "class": "3D controller",
			 "vendor": "NVIDIA", "device": "A100"}
		]
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	gpuCount := 0
	for _, m := range sync.Modules {
		if m.ProfileName == "GPU" {
			gpuCount++
		}
	}
	if gpuCount != 2 {
		t.Errorf("expected 2 GPUs (VGA + 3D), got %d", gpuCount)
	}
	// Non-GPU PCI devices should NOT produce modules.
	for _, m := range sync.Modules {
		if m.ProfileName == "GPU" && strings.Contains(m.Model, "i40e") {
			t.Errorf("Ethernet controller classified as GPU")
		}
	}
}

// TestCommissionToSync_PSUsBecomeModules verifies PSU-bay handling:
// location fallback, manufacturer-from-board, MaxWatts in model name.
func TestCommissionToSync_PSUsBecomeModules(t *testing.T) {
	payload := `{
		"system": {"board_vendor": "ASUSTeK"},
		"psus": [
			{"location": "PSU_A", "model": "AcBel FS7001", "max_watts": 750,
			 "serial": "PA001"},
			{"model": "No location PSU", "max_watts": 500}
		]
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	psuCount := 0
	for _, m := range sync.Modules {
		if m.ProfileName == "Power supply" {
			psuCount++
			if m.Manufacturer != "ASUSTeK" {
				t.Errorf("PSU manufacturer = %q, want ASUSTeK (fallback to board_vendor)",
					m.Manufacturer)
			}
			if !strings.Contains(m.Model, "W") {
				t.Errorf("PSU model should include wattage: %q", m.Model)
			}
		}
	}
	if psuCount != 2 {
		t.Errorf("expected 2 PSUs, got %d", psuCount)
	}
}

// TestCommissionToSync_BIOSAndBMCModules verifies the synthetic
// BIOS/BMC modules are emitted only when firmware versions are
// present.
func TestCommissionToSync_BIOSAndBMCModules(t *testing.T) {
	payload := `{
		"system": {"bios_version": "1.23", "board_vendor": "ASUSTeK"},
		"bmc": {"firmware_version": "5.11"}
	}`
	sync, err := CommissionToSync(1, json.RawMessage(payload))
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var haveBIOS, haveBMC bool
	for _, m := range sync.Modules {
		if m.BayName == "BIOS" {
			haveBIOS = true
			if m.FWVersion != "1.23" {
				t.Errorf("BIOS module FWVersion = %q", m.FWVersion)
			}
		}
		if m.BayName == "BMC" {
			haveBMC = true
			if m.FWVersion != "5.11" {
				t.Errorf("BMC module FWVersion = %q", m.FWVersion)
			}
		}
	}
	if !haveBIOS {
		t.Error("missing BIOS module")
	}
	if !haveBMC {
		t.Error("missing BMC module")
	}
}
