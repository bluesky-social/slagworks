// Package smcipmitool builds and runs Supermicro SMCIPMITool invocations for
// the slagworks site worker. It is the vendor-first BMC path (see
// docs/user/hardware/bmc-vendor-matrix.md): for hosts known to be Supermicro,
// power/boot/chassis-serial operations route through this tool instead of
// generic ipmitool/bmclib. SOL and the in-band agent paths are explicitly out
// of scope and stay on ipmitool.
//
// This file is the pure argument-construction layer: it has no process
// execution and no host dependency, so it is fully unit-testable. The verb
// syntax follows the SMCIPMITool 2.30.0 User's Guide:
//   - §3.6.2 ipmi power {status,up,down,reset,cycle,softshutdown}
//   - §3.6.2.8 ipmi power bootoption <Index> <Persistent>
//   - §3.6.5 ipmi fru  (carries "Chassis Serial Number = ...")
//
// The credential shape is the guide's §2 positional form:
//
//	SMCIPMITool <IP> <username> <password> [commands ...]
//
// The tool only accepts the password positionally, so it lands on argv. That
// is visible to other local users via /proc, which is acceptable only
// because the site worker is a single-tenant host. The one hard rule — never log the password — is enforced
// by Redact/String, which every logging path must use.
package smcipmitool

import "strings"

// Boot-option indices from User's Guide §3.6.2.8. Slagworks boots hosts in
// UEFI mode, so we use the UEFI variants; the legacy indices (1=PXE,
// 2=Hard-drive) would no-op on a UEFI box. Exported so the power.SMCController (a different
// package) can map its BootDevice enum onto them for SetBootOption.
const (
	BootIndexUEFIPXE  = "14" // "14: UEFI PXE"
	BootIndexUEFIDisk = "9"  // "9: UEFI Hard-drive"
	BootIndexNone     = "0"  // "0: NONE" — clears any armed override

	// PersistNo requests a one-time override applied only to the next boot
	// ("0: Disable. Options apply to next boot only [Default]"). This matches
	// the non-persistent semantics of the existing BMCController.SetBootDevice.
	PersistNo = "0"
	// PersistYes requests a persistent override applied to all future boots
	// ("1: Enable. Options requested to be persistent for all future boots").
	PersistYes = "1"
)

// redactedSecret is the placeholder substituted for the password in any
// human- or log-facing rendering of an invocation.
const redactedSecret = "***"

// Conn is the connection target for a single BMC. It carries the credentials
// inline; callers resolve these from the same place the bmclib path does
// (the operation envelope), so the credential model is unchanged.
type Conn struct {
	Host string
	User string
	Pass string
}

// argv prepends the positional credential triple to a subcommand token list,
// producing the full argument vector handed to the SMCIPMITool binary.
func (c Conn) argv(sub ...string) []string {
	out := make([]string, 0, 3+len(sub))
	out = append(out, c.Host, c.User, c.Pass)
	out = append(out, sub...)
	return out
}

// PowerStatusArgs builds `ipmi power status`.
func (c Conn) PowerStatusArgs() []string { return c.argv("ipmi", "power", "status") }

// PowerOnArgs builds `ipmi power up`.
func (c Conn) PowerOnArgs() []string { return c.argv("ipmi", "power", "up") }

// PowerOffArgs builds `ipmi power down` (hard off, not softshutdown — mirrors
// bmclib's SetPowerState("off")).
func (c Conn) PowerOffArgs() []string { return c.argv("ipmi", "power", "down") }

// PowerCycleArgs builds `ipmi power cycle`. The guide allows an optional
// [interval]; we omit it to take the BMC default.
func (c Conn) PowerCycleArgs() []string { return c.argv("ipmi", "power", "cycle") }

// BootPXEArgs builds a one-time UEFI PXE boot override.
func (c Conn) BootPXEArgs() []string {
	return c.argv("ipmi", "power", "bootoption", BootIndexUEFIPXE, PersistNo)
}

// BootDiskArgs builds a one-time UEFI hard-drive boot override.
func (c Conn) BootDiskArgs() []string {
	return c.argv("ipmi", "power", "bootoption", BootIndexUEFIDisk, PersistNo)
}

// BootOptionQueryArgs builds `ipmi power bootoption` with NO index args — the
// read-only query form that returns the current next-boot override table
// (parsed by ParseBootOption). Distinct from BootPXEArgs/BootDiskArgs, which
// pass index+persist to SET an override.
func (c Conn) BootOptionQueryArgs() []string { return c.argv("ipmi", "power", "bootoption") }

// ChassisSerialArgs builds `ipmi fru`; the chassis serial is parsed out of its
// output (the "Chassis Serial Number = ..." line).
func (c Conn) ChassisSerialArgs() []string { return c.argv("ipmi", "fru") }

// Redact returns a copy of an SMCIPMITool argv with the password (the third
// positional, per the credential contract) masked. It never mutates its
// input, and is a no-op on an argv too short to contain the password slot.
// Every path that logs or renders an invocation MUST route through this.
func Redact(args []string) []string {
	if len(args) < 3 {
		return args
	}
	out := make([]string, len(args))
	copy(out, args)
	out[2] = redactedSecret
	return out
}

// String renders a subcommand as a single redacted, space-joined line
// suitable for logging. It is the canonical "what we ran" representation;
// it never contains the password.
func (c Conn) String(sub ...string) string {
	return strings.Join(Redact(c.argv(sub...)), " ")
}

// scrubSecret replaces every occurrence of the password in s with the
// redaction placeholder. The runner routes all captured subprocess output
// through this before returning it or folding it into an error, because
// SMCIPMITool echoes its (positional) command line on usage/bad-arg errors
// and would otherwise leak the plaintext password into logs. A no-op when
// pass is empty (we must never replace the empty string, which matches
// everywhere).
func scrubSecret(s, pass string) string {
	if pass == "" {
		return s
	}
	return strings.ReplaceAll(s, pass, redactedSecret)
}
