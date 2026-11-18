package power

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
)

// SMCController controls power via Supermicro's first-party SMCIPMITool
// instead of generic bmclib/ipmitool. It is the vendor-first BMC path for
// known-Supermicro hosts (see docs/user/hardware/bmc-vendor-matrix.md):
// power, boot-device, and chassis-serial route through the vendor tool,
// which speaks a more robust channel to Supermicro BMCs than raw IPMI on
// the AMI/Aptio firmware that bites us elsewhere.
//
// It satisfies the same power.Controller interface as BMCController, so the
// site-worker factory can substitute it transparently. Slagworks does not
// ship SMCIPMITool: operators install it on their site workers (see
// smcipmitool.InstallDoc). When it isn't installed, the factory falls back
// to BMCController unless SLAGWORKS_SMCIPMITOOL=required.
//
// SOL is NOT served here — console capture stays on ipmitool permanently
// (the vendor tool is a curses/JVM-per-stream client with no win on a
// transparent serial relay). This controller is power/boot/serial only.
//
// Like BMCController, it is not safe for concurrent use; the executor
// serializes per-machine power operations.
type SMCController struct {
	runner *smcipmitool.Runner
	conn   smcipmitool.Conn
	log    *slog.Logger
}

// NewSMCController builds a Supermicro power controller targeting the BMC at
// host with the given credentials. Unlike NewBMCController it opens no
// persistent session — SMCIPMITool is a one-shot CLI per operation — so
// there is nothing to fail at construction time and no Open round-trip. The
// timeout bounds each individual invocation (0 → the runner default, which
// matches BMCController's 30s per-op deadline). Close is a no-op but is
// provided to satisfy the interface.
func NewSMCController(host, user, pass string, timeout time.Duration, log *slog.Logger) *SMCController {
	if log == nil {
		log = slog.Default()
	}
	return &SMCController{
		runner: &smcipmitool.Runner{Timeout: timeout, Logger: log},
		conn:   smcipmitool.Conn{Host: host, User: user, Pass: pass},
		log:    log,
	}
}

// SetBin points the controller at a specific SMCIPMITool launcher instead
// of resolving smcipmitool.DefaultBin on PATH.
func (s *SMCController) SetBin(path string) { s.runner.Bin = path }

func (s *SMCController) PowerOn(ctx context.Context) error {
	if err := s.runner.PowerOn(ctx, s.conn); err != nil {
		return fmt.Errorf("smc %s power on: %w", s.conn.Host, err)
	}
	return nil
}

func (s *SMCController) PowerOff(ctx context.Context) error {
	if err := s.runner.PowerOff(ctx, s.conn); err != nil {
		return fmt.Errorf("smc %s power off: %w", s.conn.Host, err)
	}
	return nil
}

func (s *SMCController) PowerCycle(ctx context.Context) error {
	if err := s.runner.PowerCycle(ctx, s.conn); err != nil {
		return fmt.Errorf("smc %s power cycle: %w", s.conn.Host, err)
	}
	return nil
}

func (s *SMCController) PowerStatus(ctx context.Context) (State, error) {
	st, err := s.runner.PowerStatus(ctx, s.conn)
	if err != nil {
		return StateUnknown, fmt.Errorf("smc %s power status: %w", s.conn.Host, err)
	}
	switch st {
	case smcipmitool.PowerOn:
		return StateOn, nil
	case smcipmitool.PowerOff:
		return StateOff, nil
	default:
		// ParsePowerStatus only returns On/Off without an error; an
		// unknown state always arrives as a non-nil error above. Guard
		// anyway so a future parser change can't silently map to On.
		return StateUnknown, nil
	}
}

// SetBootDevice sets a one-time UEFI boot override via SMCIPMITool's
// bootoption verb. BootDefault is a no-op (matching BMCController) — it
// leaves the BMC's existing boot order untouched. PXE and Disk map to the
// UEFI boot indices the wrapper validated against live H13SSH firmware.
func (s *SMCController) SetBootDevice(ctx context.Context, device BootDevice) error {
	switch device {
	case BootPXE:
		if err := s.runner.BootPXE(ctx, s.conn); err != nil {
			return fmt.Errorf("smc %s set boot device pxe: %w", s.conn.Host, err)
		}
		return nil
	case BootDisk:
		if err := s.runner.BootDisk(ctx, s.conn); err != nil {
			return fmt.Errorf("smc %s set boot device disk: %w", s.conn.Host, err)
		}
		return nil
	case BootDefault:
		return nil
	default:
		return fmt.Errorf("smc %s: unsupported boot device: %s", s.conn.Host, device)
	}
}

// ChassisSerial returns the FRU Product Serial parsed from SMCIPMITool's
// FRU output. Like BMCController.ChassisSerial it fails CLOSED on a blank
// serial: the result gates destructive ops, and ("", nil) would make the
// worker skip the identity check (internal/worker/bmcruntime.go), so a
// box whose identity can't be read must not be power-cycled or wiped.
func (s *SMCController) ChassisSerial(ctx context.Context) (string, error) {
	serial, err := s.runner.ChassisSerial(ctx, s.conn)
	if err != nil {
		return "", fmt.Errorf("smc %s chassis serial: %w", s.conn.Host, err)
	}
	if strings.TrimSpace(serial) == "" {
		return "", fmt.Errorf("smc %s: FRU read but Product Serial is empty (cannot verify chassis identity)", s.conn.Host)
	}
	return serial, nil
}

// BootStatus reads the current next-boot override (BootManager). Maps the
// vendor's "Next boot" token onto our BootDevice enum; an armed override
// pointing at a device we don't model becomes BootOther (the raw token is
// preserved in BootOverride.Raw) rather than collapsing to BootDefault. A
// transport/parse failure propagates as an error — never reported as
// "nothing armed".
func (s *SMCController) BootStatus(ctx context.Context) (BootOverride, error) {
	bo, err := s.runner.BootOption(ctx, s.conn)
	if err != nil {
		return BootOverride{}, fmt.Errorf("smc %s boot status: %w", s.conn.Host, err)
	}
	return BootOverride{
		Device:     bootDeviceFromSMC(bo.NextBoot),
		Persistent: bo.Persistent,
		Valid:      bo.Valid,
		Raw:        bo.NextBoot,
	}, nil
}

// bootDeviceFromSMC maps SMCIPMITool's "Next boot" token to our enum. The
// observed tokens are "UEFI_PXE", "UEFI_Hard-drive", and "NONE"; anything
// else that the BMC reports as armed is BootOther so the read stays honest
// (the caller keeps the raw token via BootOverride.Raw).
func bootDeviceFromSMC(nextBoot string) BootDevice {
	switch {
	case strings.EqualFold(nextBoot, "UEFI_PXE"):
		return BootPXE
	case strings.EqualFold(nextBoot, "UEFI_Hard-drive"):
		return BootDisk
	case strings.EqualFold(nextBoot, "NONE"), nextBoot == "":
		return BootDefault
	default:
		return BootOther
	}
}

// SetBootOverride arms a boot-device override (BootManager). persistent
// selects one-time vs all-future-boots. BootDefault clears any armed
// override (persistent is irrelevant for a clear and is ignored). Used by
// the operator-facing boot-set op; the deploy path keeps using the
// one-time-only Controller.SetBootDevice.
func (s *SMCController) SetBootOverride(ctx context.Context, device BootDevice, persistent bool) error {
	persist := smcipmitool.PersistNo
	if persistent {
		persist = smcipmitool.PersistYes
	}
	var index string
	switch device {
	case BootPXE:
		index = smcipmitool.BootIndexUEFIPXE
	case BootDisk:
		index = smcipmitool.BootIndexUEFIDisk
	case BootDefault:
		// Clear the override. Persistence has no meaning for a clear, so
		// force one-time regardless of the persistent arg.
		index, persist = smcipmitool.BootIndexNone, smcipmitool.PersistNo
	default:
		return fmt.Errorf("smc %s: unsupported boot device %q", s.conn.Host, device)
	}
	if err := s.runner.SetBootOption(ctx, s.conn, index, persist); err != nil {
		return fmt.Errorf("smc %s set boot override %s (persistent=%t): %w", s.conn.Host, device, persistent, err)
	}
	return nil
}

// Transport always reports "smcipmitool": every operation on this
// controller goes over the vendor tool's channel, so unlike BMCController
// (which maps bmclib's per-call successful-provider metadata) there is no
// ambiguity to resolve. Surfaced to the operator on power-status reads so
// "On" carries the provenance that it came from the vendor path.
func (s *SMCController) Transport() string { return "smcipmitool" }

// Close is a no-op: SMCIPMITool holds no persistent session (each call is a
// fresh process), so there is nothing to release. Provided to satisfy the
// Controller interface.
func (s *SMCController) Close(ctx context.Context) error { return nil }

var (
	_ Controller  = (*SMCController)(nil)
	_ BootManager = (*SMCController)(nil)
)
