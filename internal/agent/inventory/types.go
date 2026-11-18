// Package inventory produces a CommissionResult from a host: the wire
// types POSTed to the site worker plus the pure hardware
// collectors/parsers/classifiers behind them. Everything here is
// runner-driven (agent.Runner) with no agent.Context and no HTTP, so it
// is testable outside the cmd blast radius. The orchestration that wraps
// these collectors (phase reporting, the commission report POST) stays
// in cmd/slagworks-agent.
package inventory

import (
	"encoding/json"

	"github.com/bluesky-social/slagworks/internal/model"
)

// CommissionResult is the JSON payload sent to the worker after commissioning.
type CommissionResult struct {
	Hostname   string           `json:"hostname"`
	System     *SystemInfo      `json:"system"`
	CPUs       []CPUInfo        `json:"cpus"`
	Memory     *MemoryInfo      `json:"memory"`
	Disks      []DiskInfo       `json:"disks"`
	NICs       []NICInfo        `json:"nics"`
	PCIDevices []PCIDevice      `json:"pci_devices"`
	LLDP       json.RawMessage  `json:"lldp,omitempty"`
	BMC        *BMCInfo         `json:"bmc,omitempty"`
	PSUs       []PSUInfo        `json:"psus,omitempty"`
	NUMA       []NUMANode       `json:"numa,omitempty"`
	USB        []USBDevice      `json:"usb,omitempty"`
	Steps      []CommissionStep `json:"steps,omitempty"`

	// ConsoleSweep carries the determination-sweep candidates when the
	// device-type had no slagworks_kernel_params at commission time:
	// per-tty nonces the agent emitted so global can resolve which one
	// reached console_cast_events and write the console config back to
	// the device-type. Nil when the device was already configured (the
	// post-report verification probe covers that case).
	ConsoleSweep *model.ConsoleSweep `json:"console_sweep,omitempty"`
}

type SystemInfo struct {
	Manufacturer string `json:"manufacturer"`
	ProductName  string `json:"product_name"`
	SerialNumber string `json:"serial_number"`
	UUID         string `json:"uuid"`
	BIOSVersion  string `json:"bios_version"`
	AssetTag     string `json:"asset_tag"`
	BootMode     string `json:"boot_mode,omitempty"` // "uefi" or "bios"
	// Baseboard (motherboard)
	BoardVendor  string `json:"board_vendor,omitempty"`
	BoardProduct string `json:"board_product,omitempty"`
	BoardSerial  string `json:"board_serial,omitempty"`
	// Chassis
	ChassisType   string `json:"chassis_type,omitempty"`
	ChassisSerial string `json:"chassis_serial,omitempty"`
}

type CPUInfo struct {
	Model   string   `json:"model"`
	Cores   int      `json:"cores"`
	Threads int      `json:"threads"`
	Speed   string   `json:"speed"`
	Arch    string   `json:"arch,omitempty"`
	Flags   []string `json:"flags,omitempty"`
}

type MemoryInfo struct {
	TotalMB int        `json:"total_mb"`
	DIMMs   []DIMMInfo `json:"dimms"`
}

type DIMMInfo struct {
	Locator      string `json:"locator"`
	SizeMB       int    `json:"size_mb"`
	Type         string `json:"type"`
	Speed        string `json:"speed"`
	Part         string `json:"part_number"`
	Serial       string `json:"serial_number"`
	Manufacturer string `json:"manufacturer,omitempty"`
}

type DiskInfo struct {
	Name         string `json:"name"`
	Model        string `json:"model"`
	Serial       string `json:"serial"`
	SizeBytes    int64  `json:"size_bytes"`
	Transport    string `json:"transport"`
	WWN          string `json:"wwn,omitempty"`
	Rotational   *bool  `json:"rotational,omitempty"`
	SMARTHealthy *bool  `json:"smart_healthy,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	FWVersion    string `json:"firmware_version,omitempty"`
}

type NICInfo struct {
	Name           string           `json:"name"`
	MAC            string           `json:"mac"`
	Driver         string           `json:"driver"`
	Speed          string           `json:"speed"`
	FWVersion      string           `json:"firmware_version"`
	PCIAddress     string           `json:"pci_address"`
	SRIOVMaxVFs    int              `json:"sriov_max_vfs,omitempty"`
	PortType       string           `json:"port_type,omitempty"`
	Transceiver    *TransceiverInfo `json:"transceiver,omitempty"`
	IntfType       string           `json:"interface_type,omitempty"`
	SupportedPorts string           `json:"supported_ports,omitempty"`
	Addresses      []string         `json:"addresses,omitempty"`
	LinkUp         *bool            `json:"link_up,omitempty"`
	NUMANode       *int             `json:"numa_node,omitempty"`
}

type TransceiverInfo struct {
	Identifier      string `json:"identifier,omitempty"`
	Connector       string `json:"connector,omitempty"`
	TransceiverType string `json:"transceiver_type,omitempty"`
	VendorPN        string `json:"vendor_pn,omitempty"`
	VendorSN        string `json:"vendor_sn,omitempty"`
	Wavelength      string `json:"wavelength,omitempty"`
}

type PCIDevice struct {
	Slot     string `json:"slot"`
	Class    string `json:"class"`
	Vendor   string `json:"vendor"`
	Device   string `json:"device"`
	VendorID string `json:"vendor_id"`
	DeviceID string `json:"device_id"`
}

type BMCInfo struct {
	IP              string `json:"ip"`
	MAC             string `json:"mac,omitempty"`
	FirmwareVersion string `json:"firmware_version"`
}

type PSUInfo struct {
	Location string `json:"location"`
	Model    string `json:"model,omitempty"`
	Serial   string `json:"serial,omitempty"`
	MaxWatts int    `json:"max_watts,omitempty"`
	Status   string `json:"status,omitempty"`
}

type NUMANode struct {
	ID    int    `json:"id"`
	CPUs  string `json:"cpus"`
	MemMB int    `json:"mem_mb"`
}

type USBDevice struct {
	Bus      string `json:"bus"`
	Device   string `json:"device"`
	VendorID string `json:"vendor_id"`
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
}

// CommissionStep records the outcome of one info-gathering step within
// a commission run. Persisted as part of CommissionResult.Steps so that
// operators can see which surfaces succeeded vs failed without
// recommissioning the host.
type CommissionStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // "ok" | "partial" | "failed"
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	Warning    string `json:"warning,omitempty"`
}
