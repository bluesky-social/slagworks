package smcipmitool

import (
	"fmt"
	"strings"
)

// PowerState is the parsed result of `ipmi power status`.
type PowerState string

const (
	PowerOn      PowerState = "on"
	PowerOff     PowerState = "off"
	PowerUnknown PowerState = "unknown"
)

// ParsePowerStatus extracts the power state from `ipmi power status` stdout.
// Observed live form: "Power is currently on." The off form is the
// documented symmetric "Power is currently off." (User's Guide §3.6.2.1).
// Anything else returns (PowerUnknown, error) rather than guessing — an
// unrecognized banner must not be silently read as a power state.
func ParsePowerStatus(out string) (PowerState, error) {
	s := strings.ToLower(strings.TrimSpace(out))
	switch {
	case strings.Contains(s, "power is currently on"):
		return PowerOn, nil
	case strings.Contains(s, "power is currently off"):
		return PowerOff, nil
	default:
		return PowerUnknown, fmt.Errorf("smcipmitool: unrecognized power status output: %q", strings.TrimSpace(out))
	}
}

// productSerialLabel is the exact field label the tool emits for the
// Product Info Area serial, including the "(PS)" abbreviation suffix
// observed on live H13SSH firmware.
//
// We anchor on "Product Serial Number", NOT "Chassis Serial Number",
// because the identity check this serial feeds (worker.verifyChassisSerial)
// compares against the NetBox device serial, which commission sources from
// DMI system-serial-number (commission.go maps system-serial-number ->
// product_serial). On Supermicro the three FRU serials are all distinct:
//
//	Chassis Serial Number (CS) = CH119KN44BP0A98   <- enclosure, NOT NetBox's
//	Board Serial Number   (BS) = OM24CS605140       <- baseboard
//	Product Serial Number (PS) = S500001X0000001    <- == NetBox device serial
//
// bmclib's Inventory().Serial (the BMCController path) also returns the
// system/product serial, so anchoring on PS keeps the vendor path's
// identity check consistent with the generic path. Anchoring on "Chassis"
// (the field's misleading name in the Controller interface) makes every
// Supermicro op fail the serial-match guard against the wrong physical box.
const productSerialLabel = "Product Serial Number"

// ParseChassisSerial extracts the product serial from `ipmi fru` stdout
// (despite the "Chassis" in the name — see productSerialLabel for why the
// Controller-interface term is a misnomer and PS is the correct field). An
// empty value (firmware reported a blank serial) returns ("", nil), matching
// BMCController.ChassisSerial's "no serial reported" contract. A total
// absence of the line returns an error — that means the output wasn't the
// fru shape we expected, which is a real problem.
func ParseChassisSerial(out string) (string, error) {
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), productSerialLabel) {
			continue
		}
		_, after, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		return strings.TrimSpace(after), nil
	}
	return "", fmt.Errorf("smcipmitool: no %q line in fru output", productSerialLabel)
}

// BootOption is the parsed result of the `ipmi power bootoption` query (no
// index args), used to verify a boot-device set took effect.
type BootOption struct {
	NextBoot   string // e.g. "UEFI_PXE", "UEFI_Hard-drive", "NONE"
	Persistent bool   // true = applies to all future boots; false = one-time
	// Valid reflects the "Boot flag" row: true when an override is armed,
	// false when it isn't (the BMC still reports a NextBoot value, but it
	// won't act on it). A reader must check Valid before trusting NextBoot.
	Valid bool
}

// ParseBootOption reads the current next-boot override from the bootoption
// query output (the "Option | Value" table). The value column is whitespace-
// padded; we split on '|' and trim. Returns an error if the "Next boot" row
// is absent (output wasn't the expected table).
func ParseBootOption(out string) (BootOption, error) {
	var bo BootOption
	found := false
	for line := range strings.SplitSeq(out, "\n") {
		k, v, ok := splitTableRow(line)
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "next boot":
			bo.NextBoot = v
			found = true
		case "persistent":
			bo.Persistent = strings.EqualFold(v, "true")
		case "boot flag":
			// Observed values: "Valid" / "Invalid". Anything that isn't
			// exactly "Valid" is treated as not-armed — we never want to
			// report an override as armed on an unrecognized flag value.
			bo.Valid = strings.EqualFold(v, "valid")
		}
	}
	if !found {
		return BootOption{}, fmt.Errorf("smcipmitool: no \"Next boot\" row in bootoption output")
	}
	return bo, nil
}

// splitTableRow splits a "key | value" row on the first '|', trimming both
// sides. Returns ok=false for separator rows (the "----" divider) and any
// line without a '|'.
func splitTableRow(line string) (key, value string, ok bool) {
	before, after, ok := strings.Cut(line, "|")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(before)
	value = strings.TrimSpace(after)
	if key == "" || strings.HasPrefix(key, "---") || strings.HasPrefix(value, "---") {
		return "", "", false
	}
	return key, value, true
}

// classifyExit turns an SMCIPMITool process exit code into a Go error. Exit 0
// is success; any nonzero code is a failure, and we fold the tool's own
// stdout/stderr text (e.g. "Can't connect to <ip>") into the error so the
// reason is visible. The wrapper calls this BEFORE trusting any parsed
// stdout: a nonzero exit means the stdout is not a valid result even if it
// happens to contain a recognizable-looking line.
//
// Observed codes: 0 ok, 145 connect failure, 147 bad args / usage, 182
// no-args usage. We don't switch on the specific nonzero codes (they are
// undocumented and version-specific) — nonzero is failure, and the captured
// output carries the human-readable cause.
func classifyExit(code int, output string) error {
	if code == 0 {
		return nil
	}
	msg := strings.TrimSpace(output)
	if msg == "" {
		return fmt.Errorf("smcipmitool: exit %d", code)
	}
	return fmt.Errorf("smcipmitool: exit %d: %s", code, msg)
}
