package inventory

import (
	"log/slog"
	"os"
	"strings"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/model"
)

// dmiSysfsMap maps dmidecode -s keys to /sys/class/dmi/id/ filenames.
// Most sysfs files are world-readable; serial/uuid are root-only.
var dmiSysfsMap = map[string]string{
	"system-manufacturer":     "sys_vendor",
	"system-product-name":     "product_name",
	"system-serial-number":    "product_serial",
	"system-uuid":             "product_uuid",
	"bios-version":            "bios_version",
	"chassis-asset-tag":       "chassis_asset_tag",
	"baseboard-manufacturer":  "board_vendor",
	"baseboard-product-name":  "board_name",
	"baseboard-serial-number": "board_serial",
	"chassis-type":            "chassis_type",
	"chassis-serial-number":   "chassis_serial",
}

func collectSystem(r agent.Runner) *SystemInfo {
	info := &SystemInfo{}
	fields := map[string]*string{
		"system-manufacturer":     &info.Manufacturer,
		"system-product-name":     &info.ProductName,
		"system-serial-number":    &info.SerialNumber,
		"system-uuid":             &info.UUID,
		"bios-version":            &info.BIOSVersion,
		"chassis-asset-tag":       &info.AssetTag,
		"baseboard-manufacturer":  &info.BoardVendor,
		"baseboard-product-name":  &info.BoardProduct,
		"baseboard-serial-number": &info.BoardSerial,
		"chassis-type":            &info.ChassisType,
		"chassis-serial-number":   &info.ChassisSerial,
	}
	for key, ptr := range fields {
		out, err := r.Output("dmidecode", "-s", key)
		if err != nil {
			// Fall back to sysfs
			if sysfsFile, ok := dmiSysfsMap[key]; ok {
				if val, ferr := readSysfs(sysfsFile); ferr == nil && val != "" {
					*ptr = val
					continue
				}
			}
			slog.Warn("dmidecode failed", "key", key, "err", err)
			continue
		}
		*ptr = out
	}
	info.BootMode = detectBootMode()
	return info
}

// collectSystemGraceful collects system info, trying dmidecode first with
// sysfs fallback. Returns nil only when neither source provides any data.
func collectSystemGraceful(r agent.Runner) *SystemInfo {
	if r.Available("dmidecode") {
		return collectSystem(r)
	}
	// dmidecode not available — try sysfs directly
	return collectSystemFromSysfs()
}

// collectSystemFromSysfs reads system info directly from /sys/class/dmi/id/.
func collectSystemFromSysfs() *SystemInfo {
	info := &SystemInfo{}
	fields := map[string]*string{
		"sys_vendor":        &info.Manufacturer,
		"product_name":      &info.ProductName,
		"product_serial":    &info.SerialNumber,
		"product_uuid":      &info.UUID,
		"bios_version":      &info.BIOSVersion,
		"chassis_asset_tag": &info.AssetTag,
		"board_vendor":      &info.BoardVendor,
		"board_name":        &info.BoardProduct,
		"board_serial":      &info.BoardSerial,
		"chassis_type":      &info.ChassisType,
		"chassis_serial":    &info.ChassisSerial,
	}
	any := false
	for file, ptr := range fields {
		val, err := readSysfs(file)
		if err == nil && val != "" {
			*ptr = val
			any = true
		}
	}
	if !any {
		return nil
	}
	info.BootMode = detectBootMode()
	return info
}

// detectBootMode returns "uefi" or "bios" based on whether EFI firmware
// variables are present.
func detectBootMode() string {
	if _, err := os.Stat("/sys/firmware/efi"); err == nil {
		return "uefi"
	}
	return "bios"
}

// readSysfs reads a single value from /sys/class/dmi/id/.
func readSysfs(name string) (string, error) {
	data, err := os.ReadFile("/sys/class/dmi/id/" + name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// isCumulus reports whether the host is running Cumulus Linux.
func isCumulus() bool {
	_, err := os.Stat("/etc/cumulus")
	return err == nil
}

// backfillFromCumulus enriches system info with data from `net show system`,
// which provides serial number and hardware model on switch platforms where
// DMI/sysfs data is often incomplete.
func backfillFromCumulus(r agent.Runner, info *SystemInfo) {
	if info == nil || !r.Available("net") {
		return
	}
	out, err := r.Output("net", "show", "system")
	if err != nil {
		return
	}
	parsed := parseNetShowSystem(out)
	if info.SerialNumber == "" {
		info.SerialNumber = parsed["serial_number"]
	}
	if info.ProductName == "" {
		info.ProductName = parsed["platform"]
	}
	// Edgecore switch naming quirk: "AS -1234" → "AS-1234"
	info.ProductName = strings.ReplaceAll(info.ProductName, "AS -", "AS-")
}

// parseNetShowSystem parses the "Key Name..... Value" format from `net show system`.
func parseNetShowSystem(raw string) map[string]string {
	result := make(map[string]string)
	for line := range strings.SplitSeq(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Split on first sequence of 2+ dots
		idx := strings.Index(line, "..")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimLeft(line[idx:], ".")
		val = strings.TrimSpace(val)
		if key != "" && val != "" {
			result[strings.ToLower(strings.ReplaceAll(key, " ", "_"))] = val
		}
	}
	return result
}

// isJunkDMI returns true for placeholder DMI values.
func isJunkDMI(v string) bool {
	return model.NormalizeHardwareSerial(v) == ""
}

// SystemSerial reads and normalizes the host's SMBIOS system serial number.
// It returns "" (UNVERIFIED) when the serial is absent or a well-known
// placeholder (see isJunkDMI) so a junk value can never accidentally match a
// NetBox device.serial. dmidecode -s is tried first (root on bare metal reads
// SMBIOS directly); on failure it falls back to sysfs, mirroring collectSystem.
//
// This is the single source of truth for "what is this box's attested serial"
// — the commission inventory path and the site worker's self-identity
// handshake both read through here so the placeholder list lives in one place.
func SystemSerial(r agent.Runner) string {
	const key = "system-serial-number"
	raw, err := r.Output("dmidecode", "-s", key)
	if err != nil {
		if val, ferr := readSysfs(dmiSysfsMap[key]); ferr == nil {
			raw = val
		}
	}
	raw = strings.TrimSpace(raw)
	if isJunkDMI(raw) {
		return ""
	}
	return raw
}
