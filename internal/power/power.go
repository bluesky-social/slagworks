package power

import "context"

type State string

const (
	StateOn      State = "on"
	StateOff     State = "off"
	StateUnknown State = "unknown"
)

type BootDevice string

const (
	BootPXE     BootDevice = "pxe"
	BootDisk    BootDevice = "disk"
	BootDefault BootDevice = "default"
	// BootOther is an armed override pointing at a device slagworks
	// doesn't model (CD/USB/etc.). It exists so a BootStatus read of an
	// armed-but-unmapped override reports honestly (Valid=true, the raw
	// vendor token in BootOverride.Raw) instead of collapsing to
	// BootDefault and lying "nothing armed".
	BootOther BootDevice = "other"
)

// BootOverride is the BMC's current next-boot override, as read by
// BootManager.BootStatus.
type BootOverride struct {
	// Device is the override target, mapped to our enum. BootOther when
	// the BMC reports a device we don't model (see Raw).
	Device BootDevice
	// Persistent is true when the override applies to all future boots,
	// false for a one-time (next-boot-only) override.
	Persistent bool
	// Valid is false when no override is armed — the BMC may still report
	// a stale Device value, but it won't act on it. Callers must check
	// Valid before trusting Device.
	Valid bool
	// Raw is the vendor's raw device token, preserved for display and for
	// the BootOther case where the mapped enum loses information.
	Raw string
}

// BootManager is the OPTIONAL boot-override capability. A power.Controller
// that can read and set the BMC's boot-device override implements it;
// callers type-assert (`bm, ok := ctrl.(BootManager)`) and surface "not
// supported on this host" when the assertion fails, rather than degrading.
//
// It is deliberately NOT part of Controller: only SMCController (Supermicro
// hosts with the operator-installed SMCIPMITool) implements it today, and
// keeping it optional means PDUController, the bmclib BMCController, and
// the deploy path don't have to. Adding boot read/write for other BMC hosts
// is a matter of making BMCController implement BootManager — no other
// change.
//
// Note: this is distinct from Controller.SetBootDevice (the one-time
// override the deploy path arms). SetBootOverride is the operator-facing
// write that can also set a persistent override.
type BootManager interface {
	// BootStatus reads the current boot-device override. A successful read
	// with no override armed returns BootOverride{Valid:false}. A
	// transport/auth failure returns a non-nil error — it must NOT be
	// collapsed to Valid:false, or an operator would see "nothing armed"
	// when the BMC is actually unreachable.
	BootStatus(ctx context.Context) (BootOverride, error)
	// SetBootOverride arms a boot-device override. persistent=false is a
	// one-time (next-boot-only) override; persistent=true applies to all
	// future boots. BootDefault clears any armed override (persistent is
	// ignored for BootDefault — clearing has no persistence concept).
	SetBootOverride(ctx context.Context, device BootDevice, persistent bool) error
}

// Controller abstracts power management for bare-metal machines.
// Implementations include BMC (Redfish/IPMI via bmclib) and PDU (SNMP/SSH).
type Controller interface {
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
	PowerCycle(ctx context.Context) error
	PowerStatus(ctx context.Context) (State, error)
	// Transport names the wire path that served the most recent
	// call: "redfish" / "ipmi" for a BMC controller (mapped from
	// bmclib's successful-provider metadata; "bmc" before any call
	// has succeeded), "pdu" for a PDU controller. Surfaced to the
	// operator on power-status reads so "On" carries its provenance.
	Transport() string
	SetBootDevice(ctx context.Context, device BootDevice) error
	// ChassisSerial returns the BMC's reported chassis serial. Used
	// by the site worker to verify the BMC at a given IP is the
	// physical box NetBox said it would be — see the BMC runtime
	// contract spec. Returning ("", nil) means "this controller has
	// no concept of chassis serial" (e.g. a PDU); the worker treats
	// that as "skip the check" rather than aborting.
	ChassisSerial(ctx context.Context) (string, error)
	Close(ctx context.Context) error
}
