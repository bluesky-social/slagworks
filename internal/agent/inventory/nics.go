package inventory

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/bluesky-social/slagworks/internal/agent"
)

func collectNICs(r agent.Runner) []NICInfo {
	if !r.Available("ip") {
		return nil
	}
	out, err := r.Output("ip", "-j", "addr", "show")
	if err != nil {
		slog.Warn("ip addr failed", "err", err)
		return nil
	}

	var links []struct {
		Ifname   string `json:"ifname"`
		Address  string `json:"address"`
		AddrInfo []struct {
			Local     string `json:"local"`
			Prefixlen int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal([]byte(out), &links) != nil {
		return nil
	}

	var nics []NICInfo
	for _, link := range links {
		if isVirtualInterface(link.Ifname) {
			continue
		}
		nic := NICInfo{
			Name: link.Ifname,
			MAC:  link.Address,
		}
		for _, ai := range link.AddrInfo {
			if ai.Local != "" {
				nic.Addresses = append(nic.Addresses, fmt.Sprintf("%s/%d", ai.Local, ai.Prefixlen))
			}
		}

		// Carrier state (link up/down) from sysfs
		carrierPath := fmt.Sprintf("/sys/class/net/%s/carrier", link.Ifname)
		if raw, err := os.ReadFile(carrierPath); err == nil {
			up := strings.TrimSpace(string(raw)) == "1"
			nic.LinkUp = &up
		}

		// NUMA node affinity from sysfs
		numaPath := fmt.Sprintf("/sys/class/net/%s/device/numa_node", link.Ifname)
		if raw, err := os.ReadFile(numaPath); err == nil {
			var node int
			if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &node); err == nil && node >= 0 {
				nic.NUMANode = &node
			}
		}

		// ethtool -i for driver info
		if r.Available("ethtool") {
			if ethOut, err := r.Output("ethtool", "-i", link.Ifname); err == nil {
				for line := range strings.SplitSeq(ethOut, "\n") {
					if k, v, ok := strings.Cut(line, ": "); ok {
						switch strings.TrimSpace(k) {
						case "driver":
							nic.Driver = strings.TrimSpace(v)
						case "firmware-version":
							nic.FWVersion = strings.TrimSpace(v)
						case "bus-info":
							nic.PCIAddress = strings.TrimSpace(v)
						}
					}
				}
			}
		}

		// Skip virtual interfaces identified by driver
		if isVirtualDriver(nic.Driver) {
			continue
		}

		// ethtool for speed, port type, and supported link modes
		var supportedModes []string
		if r.Available("ethtool") {
			if ethOut, err := r.Output("ethtool", link.Ifname); err == nil {
				inSupported := false
				for line := range strings.SplitSeq(ethOut, "\n") {
					trimmed := strings.TrimSpace(line)
					if k, v, ok := strings.Cut(trimmed, ":"); ok {
						switch strings.TrimSpace(k) {
						case "Speed":
							nic.Speed = strings.TrimSpace(v)
						case "Port":
							nic.PortType = strings.TrimSpace(v)
						case "Supported ports":
							nic.SupportedPorts = strings.TrimSpace(v)
						case "Supported link modes":
							inSupported = true
							if mode := strings.TrimSpace(v); mode != "" {
								supportedModes = append(supportedModes, mode)
							}
							continue
						default:
							inSupported = false
						}
					} else if inSupported && trimmed != "" {
						supportedModes = append(supportedModes, trimmed)
					}
				}
			}
		}

		// SR-IOV max VFs from sysfs
		if nic.PCIAddress != "" {
			vfPath := fmt.Sprintf("/sys/bus/pci/devices/%s/sriov_totalvfs", nic.PCIAddress)
			if vfRaw, err := os.ReadFile(vfPath); err == nil {
				fmt.Sscanf(strings.TrimSpace(string(vfRaw)), "%d", &nic.SRIOVMaxVFs)
			}
		}

		// Transceiver EEPROM via ethtool -m
		if r.Available("ethtool") {
			if xcvrOut, err := r.Output("ethtool", "-m", link.Ifname); err == nil {
				nic.Transceiver = parseTransceiver(xcvrOut)
			}
		}

		nic.IntfType = classifyInterfaceType(nic.Driver, nic.Speed, nic.PortType, nic.SupportedPorts, nic.Transceiver)

		// Fallback: when link is down, use highest supported link mode
		if nic.IntfType == "" && len(supportedModes) > 0 {
			maxSpeed := maxSupportedSpeed(supportedModes)
			if maxSpeed != "" {
				nic.IntfType = classifyInterfaceType(nic.Driver, maxSpeed, nic.PortType, nic.SupportedPorts, nic.Transceiver)
			}
		}

		nics = append(nics, nic)
	}
	return nics
}

// isVirtualInterface returns true for interface names that are ephemeral
// runtime interfaces (containers, bridges, tunnels) rather than physical hardware.
func isVirtualInterface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{
		"docker", "br-", "veth", "cali", "cni", "flannel",
		"virbr", "fwbr", "fwpr", "fwln", "tap", "tailscale",
		"tun", "wg", "dummy",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// isVirtualDriver returns true for network drivers that represent virtual or
// software-defined interfaces rather than physical hardware.
func isVirtualDriver(driver string) bool {
	switch driver {
	case "bridge", "bonding", "802.1Q VLAN Support", "tun", "veth",
		"wireguard", "vrf", "dummy", "ip6tnl", "sit", "ip_gre",
		"ip6_tunnel", "macvlan", "ipvlan":
		return true
	}
	return false
}

// parseTransceiver reads `ethtool -m` output. The keys are ethtool's own
// labels; it prints the nominal wavelength as "Laser wavelength" for SFP,
// QSFP and CMIS modules alike.
func parseTransceiver(output string) *TransceiverInfo {
	info := &TransceiverInfo{}
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			switch k {
			case "Identifier":
				info.Identifier = v
				found = true
			case "Transceiver type":
				info.TransceiverType = v
				found = true
			case "Connector":
				info.Connector = v
				found = true
			case "Vendor PN":
				info.VendorPN = v
				found = true
			case "Vendor SN":
				info.VendorSN = v
				found = true
			case "Laser wavelength":
				info.Wavelength = v
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	return info
}

// classifyInterfaceType maps NIC attributes to a NetBox interface type slug.
//
// Classification priority:
//  1. Driver-based overrides (virtual, BMC USB)
//  2. Transceiver EEPROM (connector + wavelength + transceiver type string)
//  3. Speed + port type heuristics
//  4. Supported ports fallback (for switch ASICs where port type is "other")
func classifyInterfaceType(driver, speed, portType, supportedPorts string, xcvr *TransceiverInfo) string {
	driver = strings.ToLower(strings.TrimSpace(driver))
	speed = strings.ToLower(strings.TrimSpace(speed))
	portType = strings.ToLower(strings.TrimSpace(portType))
	supportedPorts = strings.ToLower(strings.TrimSpace(supportedPorts))

	// 1. Driver-based overrides
	switch driver {
	case "virtio_net":
		return "virtual"
	case "rndis_host", "cdc_ether":
		return "1000base-t"
	}

	// 2. Transceiver EEPROM classification
	if xcvr != nil {
		connector := strings.ToLower(xcvr.Connector)
		wavelength := strings.ToLower(strings.TrimSpace(xcvr.Wavelength))
		xcvrType := strings.ToLower(xcvr.TransceiverType)
		if t := classifyFromTransceiver(speed, connector, wavelength, xcvrType); t != "" {
			return t
		}
	}

	// Determine fibre vs copper from port type, falling back to supported ports.
	// A direct-attach copper port is an SFP/QSFP cage, so it classifies with
	// fibre; ethtool prints PORT_DA as "Direct Attach Copper".
	isFibre := portType == "fibre" || portType == "direct attach copper"
	if !isFibre && (portType == "other" || portType == "none" || portType == "") {
		isFibre = strings.Contains(supportedPorts, "fibre")
	}

	// 3. Speed-based classification (descending prefix length to avoid overlap)
	switch {
	case strings.HasPrefix(speed, "100000"):
		if isFibre {
			return "100gbase-x-qsfp28"
		}
		return "100gbase-x-cxp"
	case strings.HasPrefix(speed, "50000"):
		return "50gbase-x-sfp28"
	case strings.HasPrefix(speed, "40000"):
		return "40gbase-x-qsfpp"
	case strings.HasPrefix(speed, "25000"):
		if isFibre {
			return "25gbase-x-sfp28"
		}
		return "25gbase-kr"
	case strings.HasPrefix(speed, "10000"):
		if isFibre {
			return "10gbase-x-sfpp"
		}
		return "10gbase-t"
	case strings.HasPrefix(speed, "1000"):
		if isFibre {
			return "1000base-x-sfp"
		}
		return "1000base-t"
	case strings.HasPrefix(speed, "100m"), strings.HasPrefix(speed, "100 "):
		return "100base-tx"
	}
	return ""
}

// maxSupportedSpeed extracts the highest speed from ethtool "Supported link modes" lines.
// Modes look like: "100000baseCR4/Full", "25000baseSR/Full", "1000baseT/Full".
func maxSupportedSpeed(modes []string) string {
	var maxMbps int
	for _, mode := range modes {
		var mbps int
		for _, c := range mode {
			if c >= '0' && c <= '9' {
				mbps = mbps*10 + int(c-'0')
			} else {
				break
			}
		}
		if mbps > maxMbps {
			maxMbps = mbps
		}
	}
	if maxMbps == 0 {
		return ""
	}
	return fmt.Sprintf("%dMb/s", maxMbps)
}

// classifyFromTransceiver uses connector type, wavelength, and the transceiver
// type string from ethtool -m to pick precise NetBox interface types.
//
// Signals: 850nm = SR (short reach), 1310nm = LR (long reach),
// "no separable connector" = DAC/AOC (CR), LC connector = optical.
// The xcvrType string (e.g., "100G Base-LR4", "40G Active Cable") resolves
// ambiguity when connector alone isn't enough (AOC cables use "no separable"
// like DACs but are active optical).
func classifyFromTransceiver(speed, connector, wavelength, xcvrType string) string {
	isLC := strings.Contains(connector, "lc")
	isDAC := strings.Contains(connector, "no separable")
	isShortReach := strings.Contains(wavelength, "850")
	isLR := strings.Contains(xcvrType, "lr") || strings.Contains(xcvrType, "long reach")

	switch {
	case strings.HasPrefix(speed, "100000"):
		switch {
		case isDAC:
			return "100gbase-cr4"
		case isLC && isShortReach:
			return "100gbase-x-qsfp28" // SR4
		case isLC || isLR:
			return "100gbase-lr4"
		default:
			return "100gbase-x-qsfp28"
		}
	case strings.HasPrefix(speed, "25000"):
		switch {
		case isLC && isShortReach:
			return "25gbase-x-sfp28" // SR
		case isLC || isLR:
			return "25gbase-lr"
		default:
			return "25gbase-x-sfp28"
		}
	case strings.HasPrefix(speed, "10000"):
		switch {
		case isLC && isShortReach:
			return "10gbase-x-sfpp" // SR
		case isLC || isLR:
			return "10gbase-lr"
		default:
			return "10gbase-x-sfpp"
		}
	case strings.HasPrefix(speed, "1000"):
		switch {
		case isLC && isShortReach:
			return "1000base-x-sfp" // SX
		case isLC || isLR:
			return "1000base-lx"
		default:
			return "1000base-x-sfp"
		}
	}
	return ""
}
