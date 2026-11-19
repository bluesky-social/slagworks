package model

import "strings"

// NormalizeHardwareIdentifier trims a firmware-provided hardware identifier
// and returns "" for common unprogrammed placeholders. It is suitable for
// SMBIOS/FRU serials, disk serials, and WWNs. Placeholder identifiers cannot
// establish ownership: many unrelated devices report the same value.
func NormalizeHardwareIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	switch lower {
	case "00", "0", "default string", "n/a", "not specified", "none", "unknown",
		"to be filled by o.e.m.", "to be filled by o.e.m", "no asset information",
		"system serial number", "chassis serial number",
		"0123456789": // Supermicro's unprogrammed system and chassis serial
		return ""
	}
	// Blank-fill identifiers occur at several lengths. WWNs commonly render
	// the same all-zero value with a 0x prefix, so strip it for this test only.
	zeroFill := strings.TrimPrefix(lower, "0x")
	if zeroFill != "" && strings.Trim(zeroFill, "0") == "" {
		return ""
	}
	return value
}

// NormalizeHardwareSerial is the serial-specific spelling used by enrollment
// and SMBIOS callers. It shares the placeholder contract with disk identity.
func NormalizeHardwareSerial(value string) string {
	return NormalizeHardwareIdentifier(value)
}

// SameHardwareSerial reports whether a and b are the same non-empty hardware
// serial, ignoring letter case. This is NetBox's relation: its device serial
// filter matches iexact, so the hub's hello gate accepts a worker whose board
// reports "abc123" as the device NetBox records as "ABC123". A check that asks
// whether a worker-reported serial and a NetBox serial name the same device
// must use this relation too, or a worker can be verified as a device and
// then fail to match it.
func SameHardwareSerial(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}
