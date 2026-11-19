package inventory

import "testing"

// dmidecode prints a Type 17 size in the largest binary unit that keeps the
// value an integer (dmi_print_memory_size in 3.5; the 32 GiB+ extended-size
// path in every 3.x): a 32 GiB DIMM is "Size: 32 GB", not "Size: 32768 MB".
// DIMMInfo.SizeMB is MiB whatever unit dmidecode chose, because
// netbox.CommissionToSync divides it by 1024 to get the module's GB.
func TestCollectMemory_DIMMSizeUnits(t *testing.T) {
	out := `# dmidecode 3.5
Getting SMBIOS data from sysfs.
SMBIOS 3.3.0 present.

Handle 0x0040, DMI type 17, 92 bytes
Memory Device
	Array Handle: 0x003F
	Total Width: 72 bits
	Data Width: 64 bits
	Size: 32 GB
	Form Factor: DIMM
	Locator: DIMM_A1
	Type: DDR4
	Speed: 3200 MT/s
	Manufacturer: Samsung
	Serial Number: 12345678
	Part Number: M393A4K40DB3-CWE
	Volatile Size: 32 GB

Handle 0x0041, DMI type 17, 92 bytes
Memory Device
	Size: 512 MB
	Locator: DIMM_A2
	Type: DDR4

Handle 0x0042, DMI type 17, 92 bytes
Memory Device
	Size: 1536 MB
	Locator: DIMM_B1
	Type: DDR4

Handle 0x0043, DMI type 17, 92 bytes
Memory Device
	Size: 1 TB
	Locator: DIMM_B2
	Type: DDR5

Handle 0x0044, DMI type 17, 92 bytes
Memory Device
	Size: 2048 kB
	Locator: DIMM_C1
	Type: SDRAM

Handle 0x0045, DMI type 17, 92 bytes
Memory Device
	Size: No Module Installed
	Locator: DIMM_C2
	Type: Unknown

Handle 0x0046, DMI type 17, 92 bytes
Memory Device
	Size: Unknown
	Locator: DIMM_D1
	Type: Unknown
`
	mem := collectMemory(stubRunner{out: out})

	// Empty and unknown-size slots are left out of the inventory.
	want := map[string]int{
		"DIMM_A1": 32 << 10,
		"DIMM_A2": 512,
		"DIMM_B1": 1536,
		"DIMM_B2": 1 << 20,
		"DIMM_C1": 2,
	}
	if len(mem.DIMMs) != len(want) {
		t.Fatalf("got %d DIMMs, want %d: %+v", len(mem.DIMMs), len(want), mem.DIMMs)
	}
	for _, d := range mem.DIMMs {
		w, ok := want[d.Locator]
		if !ok {
			t.Errorf("unexpected DIMM %q in inventory", d.Locator)
			continue
		}
		if d.SizeMB != w {
			t.Errorf("%s: SizeMB = %d, want %d", d.Locator, d.SizeMB, w)
		}
	}
}
