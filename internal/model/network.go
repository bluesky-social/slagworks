package model

import "strings"

// IsBMCUSBNetworkDriver reports whether a NIC driver represents the
// host-visible USB network interface exposed by a BMC. These interfaces are
// useful for in-band BMC access, but they are not provisioning interfaces and
// their factory MAC may be shared by every chassis of the same model. They
// must therefore never participate in machine identity or NetBox physical-NIC
// reconciliation.
func IsBMCUSBNetworkDriver(driver string) bool {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "rndis_host", "cdc_ether":
		return true
	default:
		return false
	}
}
