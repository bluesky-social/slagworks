package model

import "testing"

func TestNormalizeHardwareSerial(t *testing.T) {
	for _, value := range []string{
		"", "Default string", "To Be Filled By O.E.M.", "N/A",
		"System Serial Number", "Chassis Serial Number", "0000000000",
	} {
		if got := NormalizeHardwareSerial(value); got != "" {
			t.Errorf("NormalizeHardwareSerial(%q) = %q, want empty", value, got)
		}
	}
	if got := NormalizeHardwareSerial("  S515042X5610687  "); got != "S515042X5610687" {
		t.Errorf("real serial = %q", got)
	}
}

func TestNormalizeHardwareIdentifierDropsZeroWWN(t *testing.T) {
	for _, value := range []string{"0", "00000000", "0x0000000000000000", "unknown"} {
		if got := NormalizeHardwareIdentifier(value); got != "" {
			t.Errorf("NormalizeHardwareIdentifier(%q) = %q, want empty", value, got)
		}
	}
	if got := NormalizeHardwareIdentifier(" 0x5000C500AABBCCDD "); got != "0x5000C500AABBCCDD" {
		t.Errorf("real WWN = %q", got)
	}
}
