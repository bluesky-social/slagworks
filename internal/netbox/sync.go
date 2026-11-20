package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
)

// isJunkDMIValue returns true for placeholder values that BIOS vendors use
// when a DMI field is unset (e.g., "00", "Default string", "N/A", "Not Specified").
func isJunkDMIValue(v string) bool {
	return model.NormalizeHardwareSerial(v) == ""
}

// authoritativeSerial picks the serial for NetBox's built-in `serial`
// field. System serial (DMI Type 1) wins: it's the identity the BMC
// reports for the deploy serial-guard and the key into the per-pop
// bmc-creds-admin secret. Chassis serial (DMI Type 3) is only a
// fallback — on hardware where they differ (Supermicro AS-1115HS-TNR:
// system=S515..., chassis=CH119...) writing chassis-first 404'd cred
// resolution for every freshly commissioned host (flipped 2026-06-11).
// DMI placeholder junk ("Default string", "To Be Filled By O.E.M.") is
// rejected from both sides — the rebind logic keys off `serial`, so a
// fleet of identical placeholders would alias as one machine.
func authoritativeSerial(systemSerial, chassisSerial string) string {
	serial := systemSerial
	if isJunkDMIValue(serial) {
		serial = chassisSerial
	}
	if isJunkDMIValue(serial) {
		return ""
	}
	return serial
}

// cleanDMIModelName normalizes junk whitespace in DMI product names
// before they become NetBox device-type models. Vendors ship strings
// like "AS -1115HS-TNR" (Supermicro, stray space before the hyphen);
// slugified as-is that creates a duplicate device type alongside the
// operator-curated "AS-1115HS-TNR" — one with none of the curated
// custom fields (slagworks_kernel_params), which broke deploys at the
// bootloader phase. Whitespace adjacent to a hyphen is never
// legitimate in a model name, so strip it; legitimate multi-word names
// ("PowerEdge R740") keep their single spaces, with runs collapsed.
func cleanDMIModelName(model string) string {
	m := strings.TrimSpace(model)
	m = strings.Join(strings.Fields(m), " ") // collapse whitespace runs
	m = strings.ReplaceAll(m, " -", "-")
	m = strings.ReplaceAll(m, "- ", "-")
	return m
}

// CommissionSync holds the data needed to reconcile commission results to NetBox.
type CommissionSync struct {
	DeviceID int
	SiteID   int // NetBox site ID, used for cable resolution
	// DeviceRole is the NetBox device role slug ("leaf", "server", ...).
	// Switch roles keep their operator-owned interface names and rows
	// (see ReconcileInterfaces).
	DeviceRole string

	// Device-level fields
	AssetTag    string
	ProductName string // system product name (e.g. "RS501A-E12-RS4U")

	// Board/chassis info from dmidecode. SystemSerial (DMI Type 1) is
	// the authoritative serial for NetBox's built-in `serial` field:
	// it's what the BMC reports for the serial guard and what keys the
	// per-pop bmc-creds-admin secret. ChassisSerial (DMI Type 3) is
	// the fallback when SystemSerial is junk/empty. (Flipped 2026-06-11:
	// chassis-first wrote CH119... serials on AS-1115HS-TNR kworkers,
	// 404ing cred resolution against the S515...-keyed secret.)
	BoardVendor   string
	BoardProduct  string
	BoardSerial   string
	ChassisType   string
	ChassisSerial string
	SystemSerial  string

	// BIOSVersion is tracked as firmware on a "BIOS" Module
	BIOSVersion string
	// BMC firmware version
	BMCFWVersion string
	// BMC IP for verification against allocated IP
	BMCIP string

	// Hardware discovered during commission
	NICs    []NICData
	Modules []ModuleData

	// LLDP raw JSON for cable creation
	LLDPRaw json.RawMessage

	// SlagworksID is the Machine UUID, stamped as slagworks_id custom field
	SlagworksID string

	// Hostname is the normalized machine hostname (e.g., "cpu1.pop3"), set as
	// the NetBox device name.
	Hostname string

	// MachineState is the slagworks machine state, mapped to NetBox device status.
	MachineState string
}

// SyncCommissionData reconciles commission results to NetBox objects.
// This is the main entry point called after a successful commission.
//
// The function is idempotent: calling it multiple times with the same data
// produces identical NetBox state. Callers MUST retry on transient failure,
// as partial completion leaves NetBox in an intermediate state that will be
// corrected by the next successful run.
//
// Device.status is intentionally NOT set here. Status transitions happen
// during enrollment (inventory) and deploy (active), not during commission.
func (c *Client) SyncCommissionData(ctx context.Context, sync CommissionSync) error {
	if sync.DeviceID <= 0 {
		return fmt.Errorf("invalid device ID: %d", sync.DeviceID)
	}

	// 1. Update device fields — authoritative: always set every field so stale
	// NetBox values are cleared when inventory no longer reports them.
	cf := map[string]any{
		"slagworks_id":  emptyToNil(sync.SlagworksID),
		"board_vendor":  emptyToNil(sync.BoardVendor),
		"board_product": emptyToNil(sync.BoardProduct),
		"chassis_type":  emptyToNil(sync.ChassisType),
		"board_serial":  emptyToNil(sync.BoardSerial),
	}

	serial := authoritativeSerial(sync.SystemSerial, sync.ChassisSerial)

	deviceFields := map[string]any{
		"serial":        serial,
		"asset_tag":     emptyToNil(sync.AssetTag),
		"custom_fields": cf,
	}

	// Tags are shared with the operator. NetBox takes `tags` on a PATCH as
	// the device's complete tag set (add_tags only arrived in 4.6), so
	// sending slagworks-managed alone strips every operator tag. Read the
	// current set and write it back with slagworks-managed added. Once the
	// device carries the tag (every recommission and report retry), leave
	// tags out of the PATCH entirely so an operator tag edit racing the
	// sync is never overwritten.
	dev, err := c.GetDevice(ctx, sync.DeviceID)
	if err != nil {
		return fmt.Errorf("reading device %d tags: %w", sync.DeviceID, err)
	}
	if tags, ok := addTag(dev.Tags, "slagworks-managed"); ok {
		deviceFields["tags"] = tags
	}

	if sync.Hostname != "" {
		deviceFields["name"] = sync.Hostname
	}
	if sync.MachineState == "deployed" {
		deviceFields["status"] = "active"
	}

	// device_type is deliberately NOT written here. It is operator-owned
	// topology (see mirror.OnDeviceExternalDrift): network/storage scripts
	// and kernel params key off the curated type, and re-deriving it from the
	// DMI product name moved pop1 leaf-3 from mellanox-sn2700 onto the stale
	// msn2700 type mid-rebuild (2026-10-23). ResolveDevice still sets a type
	// when it creates a brand-new device.

	err = c.UpdateDevice(ctx, sync.DeviceID, deviceFields)
	if err != nil && sync.AssetTag != "" && rejectsField(err, "asset_tag") {
		// NetBox requires a device's asset_tag to be unique across every
		// device and at most 50 characters. The tag is firmware DMI data: a
		// placeholder missing from the junk list, the same tag programmed on
		// two hosts, or a board moved between chassis reports a tag another
		// device already holds. NetBox validates the whole PATCH before
		// writing any of it, so drop the one field it refused and send the
		// rest again; the device keeps whatever asset_tag it had. Failing
		// instead would fail every retry of the commission report the same
		// way until the operation expired.
		c.log.Warn("NetBox refused the commissioned asset tag; leaving the device's asset_tag unchanged and writing its other fields. "+
			"If another device holds this tag, clear it there or correct it in the firmware, then recommission",
			"device_id", sync.DeviceID, "machine_id", sync.SlagworksID, "asset_tag", sync.AssetTag, "err", err)
		delete(deviceFields, "asset_tag")
		err = c.UpdateDevice(ctx, sync.DeviceID, deviceFields)
	}
	if err != nil {
		return fmt.Errorf("updating device: %w", err)
	}

	// 2. Reconcile interfaces
	if err := c.ReconcileInterfaces(ctx, sync.DeviceID, sync.NICs, interfaceNamesOperatorOwned(sync.DeviceRole)); err != nil {
		return fmt.Errorf("reconciling interfaces: %w", err)
	}

	// 3. Reconcile modules
	if len(sync.Modules) > 0 {
		if err := c.ReconcileModules(ctx, sync.DeviceID, sync.Modules); err != nil {
			return fmt.Errorf("reconciling modules: %w", err)
		}
	}

	// 4. Reconcile cables from LLDP
	if len(sync.LLDPRaw) > 0 {
		neighbors := ParseLLDP(sync.LLDPRaw)
		if len(neighbors) > 0 {
			if err := c.ReconcileCables(ctx, sync.DeviceID, sync.SiteID, neighbors); err != nil {
				return fmt.Errorf("reconciling cables: %w", err)
			}
		}
	}

	c.log.Info("commission data synced to NetBox", "device_id", sync.DeviceID)
	return nil
}

// addTag returns the tag list to PATCH so a device keeps its existing tags
// and gains slug, or false when the device already carries slug and its tag
// set needs no write.
func addTag(existing []NestedTag, slug string) ([]map[string]string, bool) {
	tags := make([]map[string]string, 0, len(existing)+1)
	for _, t := range existing {
		if t.Slug == slug {
			return nil, false
		}
		tags = append(tags, map[string]string{"slug": t.Slug})
	}
	return append(tags, map[string]string{"slug": slug}), true
}

// CommissionToSync converts the agent's CommissionResult JSON into a CommissionSync.
// deviceID is the NetBox device ID from Machine.NetBoxDeviceID.
func CommissionToSync(deviceID int, data json.RawMessage) (*CommissionSync, error) {
	var cr commissionResult
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("unmarshal commission data: %w", err)
	}

	sync := &CommissionSync{
		DeviceID: deviceID,
		LLDPRaw:  cr.LLDP,
	}

	// Device fields
	if cr.System != nil {
		sync.SystemSerial = cr.System.SerialNumber
		if !isJunkDMIValue(cr.System.AssetTag) {
			sync.AssetTag = cr.System.AssetTag
		}
		sync.ProductName = cleanDMIModelName(cr.System.ProductName)
		sync.BIOSVersion = cr.System.BIOSVersion
		sync.BoardVendor = cr.System.BoardVendor
		sync.BoardProduct = cr.System.BoardProduct
		sync.BoardSerial = cr.System.BoardSerial
		sync.ChassisType = cr.System.ChassisType
		sync.ChassisSerial = cr.System.ChassisSerial
	}
	if cr.BMC != nil {
		sync.BMCFWVersion = cr.BMC.FirmwareVersion
		sync.BMCIP = cr.BMC.IP
		// Include BMC as a management NIC so it gets reconciled into NetBox
		if cr.BMC.MAC != "" {
			bmcDesc := ""
			if cr.BMC.IP != "" {
				bmcDesc = cr.BMC.IP
			}
			sync.NICs = append(sync.NICs, NICData{
				Name:      "ipmi",
				MAC:       cr.BMC.MAC,
				IntfType:  "1000base-t",
				MgmtOnly:  true,
				FWVersion: cr.BMC.FirmwareVersion,
				Driver:    bmcDesc,
			})
		}
	}

	// NICs
	for _, nic := range cr.NICs {
		// The host-visible BMC USB interface is inventory, not a physical
		// provisioning NIC. Its MAC may be shared across many chassis; retain
		// it in the raw commission blob but never reconcile it into NetBox.
		if model.IsBMCUSBNetworkDriver(nic.Driver) {
			continue
		}
		nd := NICData{
			Name:        nic.Name,
			MAC:         nic.MAC,
			Speed:       parseSpeedKbps(nic.Speed),
			Driver:      nic.Driver,
			FWVersion:   nic.FWVersion,
			PCIAddress:  nic.PCIAddress,
			SRIOVMaxVFs: nic.SRIOVMaxVFs,
			IntfType:    nic.IntfType,
		}
		if nic.Transceiver != nil && nic.Transceiver.Identifier != "" {
			nd.XcvrType = cleanXcvrIdentifier(nic.Transceiver.Identifier)
		}
		sync.NICs = append(sync.NICs, nd)
	}

	// DIMMs. The Locator is a firmware string; a placeholder or empty one
	// cannot name a bay, so it takes an indexed name like the PSU fallback.
	if cr.Memory != nil {
		for i, dimm := range cr.Memory.DIMMs {
			bayName := dimm.Locator
			if isJunkDMIValue(bayName) {
				bayName = fmt.Sprintf("DIMM_%d", i)
			}
			sizeGB := dimm.SizeMB / 1024
			model := dimm.Part
			if model == "" {
				model = fmt.Sprintf("%s %dGB", dimm.Type, sizeGB)
			} else if sizeGB > 0 && dimm.Type != "" {
				model = fmt.Sprintf("%s (%dGB %s)", model, sizeGB, dimm.Type)
			}
			attrData, _ := json.Marshal(map[string]any{
				"size":      dimm.SizeMB / 1024, // GB
				"class":     dimm.Type,
				"data_rate": parseDataRate(dimm.Speed),
			})
			mfg := dimm.Manufacturer
			if mfg == "" {
				mfg = "Unknown"
			}
			sync.Modules = append(sync.Modules, ModuleData{
				BayName:       bayName,
				ProfileName:   "Memory",
				Manufacturer:  mfg,
				Model:         model,
				PartNumber:    dimm.Part,
				Serial:        dimm.Serial,
				AttributeData: attrData,
			})
		}
	}

	// CPUs
	for i, cpu := range cr.CPUs {
		model := cpu.Model
		if model == "" {
			model = "Unknown CPU"
		}
		cpuAttrs := map[string]any{
			"cores": cpu.Cores,
			"speed": parseSpeedGHz(cpu.Speed),
		}
		if cpu.Arch != "" {
			cpuAttrs["arch"] = cpu.Arch
		}
		attrData, _ := json.Marshal(cpuAttrs)
		sync.Modules = append(sync.Modules, ModuleData{
			BayName:       fmt.Sprintf("CPU_%d", i),
			ProfileName:   "CPU",
			Manufacturer:  extractCPUManufacturer(cpu.Model),
			Model:         model,
			AttributeData: attrData,
		})
	}

	// Disks
	for i, disk := range cr.Disks {
		transport := strings.ToUpper(disk.Transport)
		rotational := disk.Rotational != nil && *disk.Rotational
		bayPrefix := "Disk"
		diskType := "HD"
		switch {
		case transport == "NVME":
			bayPrefix = "NVMe"
			diskType = "NVME"
		case rotational:
			bayPrefix = "HDD"
			diskType = "HD"
		case transport == "SAS", transport == "SATA":
			bayPrefix = "SSD"
			diskType = "SSD"
		}

		sizeGB := disk.SizeBytes / (1024 * 1024 * 1024)
		model := strings.TrimSpace(disk.Model)
		if model == "" {
			model = fmt.Sprintf("%s %dGB", diskType, sizeGB)
		} else {
			model = fmt.Sprintf("%s %dGB", model, sizeGB)
		}

		attrData, _ := json.Marshal(map[string]any{
			"size": sizeGB,
			"type": diskType,
		})
		diskMfg := disk.Manufacturer
		if diskMfg == "" {
			diskMfg = extractDiskManufacturer(disk.Model)
		}
		sync.Modules = append(sync.Modules, ModuleData{
			BayName:       fmt.Sprintf("%s_%d", bayPrefix, i),
			ProfileName:   "Hard disk",
			Manufacturer:  diskMfg,
			Model:         model,
			Serial:        disk.Serial,
			FWVersion:     disk.FWVersion,
			AttributeData: attrData,
		})
	}

	// GPUs (filtered from PCI devices)
	gpuIndex := 0
	for _, pci := range cr.PCIDevices {
		if !isGPU(pci.Class) {
			continue
		}
		sync.Modules = append(sync.Modules, ModuleData{
			BayName:      fmt.Sprintf("PCIe_%d", gpuIndex),
			ProfileName:  "GPU",
			Manufacturer: pci.Vendor,
			Model:        pci.Device,
		})
		gpuIndex++
	}

	// BIOS and BMC firmware modules
	if sync.BIOSVersion != "" {
		biosMfg := sync.BoardVendor
		if biosMfg == "" {
			biosMfg = "Unknown"
		}
		sync.Modules = append(sync.Modules, ModuleData{
			BayName:      "BIOS",
			ProfileName:  "Expansion card",
			Manufacturer: biosMfg,
			Model:        "System BIOS",
			FWVersion:    sync.BIOSVersion,
		})
	}
	if sync.BMCFWVersion != "" {
		bmcMfg := sync.BoardVendor
		if bmcMfg == "" {
			bmcMfg = "Unknown"
		}
		sync.Modules = append(sync.Modules, ModuleData{
			BayName:      "BMC",
			ProfileName:  "Expansion card",
			Manufacturer: bmcMfg,
			Model:        "BMC",
			FWVersion:    sync.BMCFWVersion,
		})
	}

	// PSUs. Many boards leave the dmidecode Location at a placeholder, often
	// the same one on every PSU ("Not Specified", "Default string"); like an
	// empty one, it falls back to an indexed name.
	for i, psu := range cr.PSUs {
		bayName := psu.Location
		if isJunkDMIValue(bayName) {
			bayName = fmt.Sprintf("PSU_%d", i)
		}
		model := psu.Model
		if model == "" {
			model = "Power Supply"
		}
		if psu.MaxWatts > 0 {
			model = fmt.Sprintf("%s %dW", model, psu.MaxWatts)
		}
		mfg := sync.BoardVendor
		if mfg == "" {
			mfg = "Unknown"
		}
		sync.Modules = append(sync.Modules, ModuleData{
			BayName:      bayName,
			ProfileName:  "Power supply",
			Manufacturer: mfg,
			Model:        model,
			Serial:       psu.Serial,
		})
	}

	uniqueBayNames(sync.Modules)
	return sync, nil
}

// uniqueBayNames renames each module whose BayName repeats an earlier
// module's. ReconcileModules keys bays and modules by name, so two modules
// with one name cannot both be stored, and firmware names do repeat (a
// board that prints the channel only in Bank Locator reports "DIMM 0" once
// per channel). The first module keeps the name; each later one gets the
// lowest "<name>_<n>" (n >= 2) that no module in the report uses. The agent
// reports components in firmware table order, so the same hardware gets
// the same names on every commission.
func uniqueBayNames(modules []ModuleData) {
	used := make(map[string]bool, len(modules))
	for _, m := range modules {
		used[m.BayName] = true
	}
	assigned := make(map[string]bool, len(modules))
	for i := range modules {
		name := modules[i].BayName
		if !assigned[name] {
			assigned[name] = true
			continue
		}
		for n := 2; ; n++ {
			candidate := fmt.Sprintf("%s_%d", name, n)
			if !used[candidate] {
				used[candidate] = true
				assigned[candidate] = true
				modules[i].BayName = candidate
				break
			}
		}
	}
}

// Internal mirrors of the agent's CommissionResult.
// These are only used for JSON deserialization in CommissionToSync.
type commissionResult struct {
	System     *commissionSystem `json:"system"`
	CPUs       []commissionCPU   `json:"cpus"`
	Memory     *commissionMemory `json:"memory"`
	Disks      []commissionDisk  `json:"disks"`
	NICs       []commissionNIC   `json:"nics"`
	PCIDevices []commissionPCI   `json:"pci_devices"`
	LLDP       json.RawMessage   `json:"lldp"`
	BMC        *commissionBMC    `json:"bmc"`
	PSUs       []commissionPSU   `json:"psus"`
}

type commissionSystem struct {
	Manufacturer  string `json:"manufacturer"`
	ProductName   string `json:"product_name"`
	SerialNumber  string `json:"serial_number"`
	UUID          string `json:"uuid"`
	BIOSVersion   string `json:"bios_version"`
	AssetTag      string `json:"asset_tag"`
	BoardVendor   string `json:"board_vendor"`
	BoardProduct  string `json:"board_product"`
	BoardSerial   string `json:"board_serial"`
	ChassisType   string `json:"chassis_type"`
	ChassisSerial string `json:"chassis_serial"`
}

type commissionCPU struct {
	Model   string `json:"model"`
	Cores   int    `json:"cores"`
	Threads int    `json:"threads"`
	Speed   string `json:"speed"`
	Arch    string `json:"arch"`
}

type commissionMemory struct {
	TotalMB int              `json:"total_mb"`
	DIMMs   []commissionDIMM `json:"dimms"`
}

type commissionDIMM struct {
	Locator      string `json:"locator"`
	SizeMB       int    `json:"size_mb"`
	Type         string `json:"type"`
	Speed        string `json:"speed"`
	Part         string `json:"part_number"`
	Serial       string `json:"serial_number"`
	Manufacturer string `json:"manufacturer"`
}

type commissionDisk struct {
	Name         string `json:"name"`
	Model        string `json:"model"`
	Serial       string `json:"serial"`
	SizeBytes    int64  `json:"size_bytes"`
	Transport    string `json:"transport"`
	Rotational   *bool  `json:"rotational"`
	Manufacturer string `json:"manufacturer"`
	FWVersion    string `json:"firmware_version"`
}

type commissionNIC struct {
	Name        string          `json:"name"`
	MAC         string          `json:"mac"`
	Driver      string          `json:"driver"`
	Speed       string          `json:"speed"`
	FWVersion   string          `json:"firmware_version"`
	PCIAddress  string          `json:"pci_address"`
	SRIOVMaxVFs int             `json:"sriov_max_vfs"`
	IntfType    string          `json:"interface_type"`
	Transceiver *commissionXcvr `json:"transceiver"`
}

type commissionXcvr struct {
	Identifier string `json:"identifier"` // e.g., "0x03 (SFP)", "0x11 (QSFP28)"
}

type commissionPCI struct {
	Slot     string `json:"slot"`
	Class    string `json:"class"`
	Vendor   string `json:"vendor"`
	Device   string `json:"device"`
	VendorID string `json:"vendor_id"`
	DeviceID string `json:"device_id"`
}

type commissionBMC struct {
	IP              string `json:"ip"`
	MAC             string `json:"mac"`
	FirmwareVersion string `json:"firmware_version"`
}

type commissionPSU struct {
	Location string `json:"location"`
	Model    string `json:"model"`
	Serial   string `json:"serial"`
	MaxWatts int    `json:"max_watts"`
	Status   string `json:"status"`
}

// cleanXcvrIdentifier strips the hex prefix from ethtool -m identifiers.
// "0x03 (SFP)" → "SFP", "0x11 (QSFP28)" → "QSFP28".
func cleanXcvrIdentifier(raw string) string {
	if i := strings.Index(raw, "("); i >= 0 {
		if j := strings.Index(raw[i:], ")"); j >= 0 {
			return raw[i+1 : i+j]
		}
	}
	return raw
}

// emptyToNil returns nil for empty strings (serializes as JSON null to clear
// the field in NetBox) and the string itself otherwise.
func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parseSpeedKbps(s string) int {
	// Formats: "25000Mb/s", "10000 Mb/s", "1000Mb/s"
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "Mb/s")
	s = strings.TrimSuffix(s, " Mb/s")
	s = strings.TrimSpace(s)
	var mbps int
	fmt.Sscanf(s, "%d", &mbps)
	return mbps * 1000 // Mbps -> kbps
}

func parseSpeedGHz(s string) float64 {
	// "3400.000 MHz" -> 3.4
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, " MHz")
	s = strings.TrimSuffix(s, "MHz")
	s = strings.TrimSpace(s)
	var mhz float64
	fmt.Sscanf(s, "%f", &mhz)
	return mhz / 1000
}

func parseDataRate(s string) int {
	// "4800 MT/s" -> 4800
	s = strings.TrimSpace(s)
	var rate int
	fmt.Sscanf(s, "%d", &rate)
	return rate
}

func extractCPUManufacturer(model string) string {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "intel"):
		return "Intel"
	case strings.Contains(lower, "amd") || strings.Contains(lower, "epyc") || strings.Contains(lower, "ryzen"):
		return "AMD"
	default:
		return "Unknown"
	}
}

func extractDiskManufacturer(model string) string {
	lower := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(lower, "samsung"):
		return "Samsung"
	case strings.HasPrefix(lower, "kioxia"),
		strings.HasPrefix(lower, "kcd"), // Kioxia enterprise NVMe prefix
		strings.HasPrefix(lower, "kxg"): // Kioxia client NVMe prefix
		return "Kioxia"
	case strings.HasPrefix(lower, "intel"),
		strings.HasPrefix(lower, "ssdpe"), // Intel P-series NVMe
		strings.HasPrefix(lower, "ssdsc"): // Intel S-series SATA
		return "Intel"
	case strings.HasPrefix(lower, "wdc"),
		strings.HasPrefix(lower, "western digital"),
		strings.HasPrefix(lower, "wus"), // WD Ultrastar
		strings.HasPrefix(lower, "wds"): // WD SN-series
		return "Western Digital"
	case strings.HasPrefix(lower, "seagate"),
		strings.HasPrefix(lower, "st"):
		return "Seagate"
	case strings.HasPrefix(lower, "micron"),
		strings.HasPrefix(lower, "mtfd"): // Micron NVMe/SATA prefix
		return "Micron"
	case strings.HasPrefix(lower, "hynix"),
		strings.HasPrefix(lower, "sk hynix"):
		return "SK Hynix"
	case strings.HasPrefix(lower, "toshiba"):
		return "Toshiba"
	case strings.HasPrefix(lower, "crucial"):
		return "Crucial"
	case strings.HasPrefix(lower, "kingston"):
		return "Kingston"
	default:
		return "Unknown"
	}
}

func isGPU(pciClass string) bool {
	lower := strings.ToLower(pciClass)
	return strings.Contains(lower, "display controller") ||
		strings.Contains(lower, "3d controller") ||
		strings.Contains(lower, "vga")
}
