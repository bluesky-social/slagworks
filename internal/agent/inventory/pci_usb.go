package inventory

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
)

func collectPCI(r agent.Runner) []PCIDevice {
	if !r.Available("lspci") {
		return nil
	}
	out, err := r.Output("lspci", "-nnmm")
	if err != nil {
		slog.Warn("lspci failed", "err", err)
		return nil
	}

	var devices []PCIDevice
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		fields := parseLspciLine(line)
		if fields == nil {
			continue
		}
		devices = append(devices, *fields)
	}
	return devices
}

// parseLspciLine parses a single lspci -nnmm output line.
// Format: Slot "Class [CCCC]" "Vendor [VVVV]" "Device [DDDD]" ...
// The slot is unquoted; remaining fields are space-separated and double-quoted.
func parseLspciLine(line string) *PCIDevice {
	// Extract quoted fields: slot is everything before the first quote,
	// then each "..." is a field.
	quoteIdx := strings.IndexByte(line, '"')
	if quoteIdx < 0 {
		return nil
	}
	slot := strings.TrimSpace(line[:quoteIdx])
	if slot == "" {
		return nil
	}

	// Collect all quoted fields
	var fields []string
	rest := line[quoteIdx:]
	for len(rest) > 0 {
		if rest[0] != '"' {
			rest = rest[1:]
			continue
		}
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			break
		}
		fields = append(fields, rest[1:1+end])
		rest = rest[2+end:]
	}
	if len(fields) < 3 {
		return nil
	}

	extractIDs := func(s string) (name, id string) {
		if idx := strings.LastIndex(s, " ["); idx >= 0 {
			name = s[:idx]
			id = strings.Trim(s[idx+2:], "[]")
			return
		}
		return s, ""
	}

	dev := &PCIDevice{Slot: slot}
	dev.Class, _ = extractIDs(fields[0])
	dev.Vendor, dev.VendorID = extractIDs(fields[1])
	dev.Device, dev.DeviceID = extractIDs(fields[2])
	return dev
}

func collectUSB(r agent.Runner) []USBDevice {
	out, err := r.Output("lsusb")
	if err != nil {
		return nil
	}

	var devices []USBDevice
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "Bus 001 Device 002: ID 1d6b:0003 Linux Foundation 3.0 root hub"
		dev := parseUSBLine(line)
		if dev != nil {
			devices = append(devices, *dev)
		}
	}
	return devices
}

func parseUSBLine(line string) *USBDevice {
	var bus, device, vid, did string
	// "Bus 001 Device 002: ID 1d6b:0003 ..."
	n, _ := fmt.Sscanf(line, "Bus %s Device %s", &bus, &device)
	if n < 2 {
		return nil
	}
	device = strings.TrimSuffix(device, ":")

	_, rest, ok := strings.Cut(line, "ID ")
	if !ok {
		return nil
	}
	parts := strings.SplitN(rest, " ", 2)
	if v, d, ok := strings.Cut(parts[0], ":"); ok {
		vid = v
		did = d
	}
	name := ""
	if len(parts) > 1 {
		name = parts[1]
	}
	return &USBDevice{
		Bus:      bus,
		Device:   device,
		VendorID: vid,
		DeviceID: did,
		Name:     name,
	}
}

// collectLLDPGraceful collects LLDP data only when lldpctl is already available.
// Unlike collectLLDP, it does NOT start lldpd. Returns nil when tool is absent.
func collectLLDPGraceful(r agent.Runner) json.RawMessage {
	if !r.Available("lldpctl") {
		return nil
	}
	out, err := r.Output("lldpctl", "-f", "json")
	if err != nil {
		slog.Warn("lldpctl failed", "err", err)
		return nil
	}
	if !json.Valid([]byte(out)) {
		slog.Warn("lldpctl output is not valid JSON")
		return nil
	}
	return json.RawMessage(out)
}

func collectLLDP(r agent.Runner) json.RawMessage {
	// Check if lldpd is already running
	alreadyRunning := false
	if err := exec.Command("pgrep", "-x", "lldpd").Run(); err == nil {
		alreadyRunning = true
	}

	if !alreadyRunning {
		// Start lldpd and wait for neighbor discovery
		if err := exec.Command("lldpd").Run(); err != nil {
			slog.Warn("failed to start lldpd", "err", err)
			return nil
		}
		slog.Info("waiting for LLDP neighbor discovery")
		time.Sleep(15 * time.Second)
	}

	out, err := r.Output("lldpctl", "-f", "json")
	if err != nil {
		slog.Warn("lldpctl failed", "err", err)
		return nil
	}
	if !json.Valid([]byte(out)) {
		slog.Warn("lldpctl output is not valid JSON")
		return nil
	}
	return json.RawMessage(out)
}

// collectBMCGraceful collects BMC info only when ipmitool is available.
func collectBMCGraceful(r agent.Runner) *BMCInfo {
	if !r.Available("ipmitool") {
		return nil
	}
	return collectBMC(r)
}

func collectBMC(r agent.Runner) *BMCInfo {
	info := &BMCInfo{}

	// BMC IP from ipmitool lan print
	out, err := r.Output("ipmitool", "lan", "print")
	if err != nil {
		return nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			switch k {
			case "IP Address":
				if v != "0.0.0.0" {
					info.IP = v
				}
			case "MAC Address":
				if v != "00:00:00:00:00:00" {
					info.MAC = v
				}
			}
		}
	}

	// BMC firmware from ipmitool mc info
	out, err = r.Output("ipmitool", "mc", "info")
	if err != nil {
		return info
	}
	for line := range strings.SplitSeq(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			if strings.TrimSpace(k) == "Firmware Revision" {
				info.FirmwareVersion = strings.TrimSpace(v)
			}
		}
	}

	if info.IP == "" && info.FirmwareVersion == "" {
		return nil
	}
	return info
}
