package power

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	bmclib "github.com/bmc-toolbox/bmclib/v2"
	"github.com/bmc-toolbox/bmclib/v2/bmc"
	"github.com/bmc-toolbox/common"
)

// BuildBMCDialOptions returns the bmclib.Option set + bare host string
// derived from a NetBox BMC IP (which may include a "host:port" suffix
// for the qemutest fake BMC) and a per-host IPMI cipher suite.
//
// The site worker (cmd/slagworksd/site.go bmcControllerFactory) is the
// sole production caller; the helper exists so the test-fake-BMC port
// handling and the AMI-Aptio cipher-3 override live in one place.
// Returns (host, opts) so the caller can pass host into bmclib.NewClient
// alongside whatever other options it cares about.
//
// cipher 0 means "use bmclib default"; non-zero forces the wire
// protocol (suite 3 for AMI Aptio, suite 17 elsewhere). See the
// pop1 H13 deep-dive for context on why this matters.
func BuildBMCDialOptions(bmcIP string, cipher int) (string, []bmclib.Option) {
	host := bmcIP
	var opts []bmclib.Option
	if h, port, err := net.SplitHostPort(bmcIP); err == nil {
		host = h
		opts = append(opts,
			bmclib.WithRedfishPort(port),
			bmclib.WithRedfishUseBasicAuth(true),
			bmclib.WithPerProviderTimeout(3*time.Second),
		)
	}
	if cipher > 0 {
		opts = append(opts, bmclib.WithIpmitoolCipherSuite(fmt.Sprintf("%d", cipher)))
	}
	return host, opts
}

// bmcClient is the subset of bmclib.Client methods used by BMCController.
// Defined as an interface for testability.
type bmcClient interface {
	SetPowerState(ctx context.Context, state string) (bool, error)
	GetPowerState(ctx context.Context) (string, error)
	SetBootDevice(ctx context.Context, device string, persistent, efi bool) (bool, error)
	Inventory(ctx context.Context) (*common.Device, error)
	GetMetadata() bmc.Metadata
	Close(ctx context.Context) error
}

// BMCController controls power via Redfish/IPMI using bmclib.
// Not safe for concurrent use — callers must serialize access or
// create separate controllers per goroutine.
type BMCController struct {
	client  bmcClient
	host    string
	timeout time.Duration
	log     *slog.Logger

	// user/pass/cipher are retained for the ipmitool FRU fallback in
	// ChassisSerial: bmclib's ipmitool provider in this version does not
	// implement InventoryGetter, so on a BMC reachable ONLY over IPMI (e.g.
	// a Dell iDRAC user provisioned via `ipmitool user set`, which has no
	// Redfish/iDRAC-web role) bmclib's Inventory() errors with "no
	// InventoryGetter implementations found". We then shell `ipmitool fru
	// print 0` ourselves and read the Product Serial. host may carry a
	// "host:port" suffix (qemutest fake BMC); fruSerialViaIPMItool splits it.
	user   string
	pass   string
	cipher int
}

// NewBMCController creates a BMC power controller.
// The timeout applies as a per-operation deadline if the caller's context
// has no deadline set. Call Close when done to release the BMC connection.
func NewBMCController(ctx context.Context, host, user, pass string, timeout time.Duration, log *slog.Logger, opts ...bmclib.Option) (*BMCController, error) {
	if log == nil {
		log = slog.Default()
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	client := bmclib.NewClient(host, user, pass, opts...)
	if err := client.Open(ctx); err != nil {
		return nil, fmt.Errorf("bmc open %s: %w", host, err)
	}

	return &BMCController{client: client, host: host, timeout: timeout, log: log, user: user, pass: pass}, nil
}

// SetIPMICipher records the IPMI cipher suite to use for the ipmitool FRU
// fallback in ChassisSerial. 0 = ipmitool default. The cipher otherwise
// lives opaquely inside the bmclib opts; the factory passes it here so the
// fallback's `-C` matches the one bmclib's ipmitool provider uses.
func (b *BMCController) SetIPMICipher(cipher int) { b.cipher = cipher }

// newBMCControllerWithClient creates a BMCController with a pre-built client (for testing).
func newBMCControllerWithClient(client bmcClient, host string) *BMCController {
	return &BMCController{client: client, host: host, timeout: 30 * time.Second, log: slog.Default()}
}

func (b *BMCController) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.timeout)
}

func (b *BMCController) PowerOn(ctx context.Context) error {
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	ok, err := b.client.SetPowerState(ctx, "on")
	if err != nil {
		return fmt.Errorf("bmc %s power on: %w", b.host, err)
	}
	if !ok {
		return fmt.Errorf("bmc %s power on: provider returned failure", b.host)
	}
	return nil
}

func (b *BMCController) PowerOff(ctx context.Context) error {
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	ok, err := b.client.SetPowerState(ctx, "off")
	if err != nil {
		return fmt.Errorf("bmc %s power off: %w", b.host, err)
	}
	if !ok {
		return fmt.Errorf("bmc %s power off: provider returned failure", b.host)
	}
	return nil
}

func (b *BMCController) PowerCycle(ctx context.Context) error {
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	ok, err := b.client.SetPowerState(ctx, "cycle")
	if err != nil {
		return fmt.Errorf("bmc %s power cycle: %w", b.host, err)
	}
	if !ok {
		return fmt.Errorf("bmc %s power cycle: provider returned failure", b.host)
	}
	return nil
}

func (b *BMCController) PowerStatus(ctx context.Context) (State, error) {
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	state, err := b.client.GetPowerState(ctx)
	if err != nil {
		return StateUnknown, fmt.Errorf("bmc %s power status: %w", b.host, err)
	}

	s := strings.ToLower(strings.TrimSpace(state))
	switch {
	case s == "on" || s == "poweredon" || strings.Contains(s, "power is on"):
		return StateOn, nil
	case s == "off" || s == "poweredoff" || strings.Contains(s, "power is off"):
		return StateOff, nil
	default:
		b.log.Warn("unrecognized BMC power state, treating as unknown", "host", b.host, "state", state)
		return StateUnknown, nil
	}
}

// SetBootDevice sets the next boot device. BootDefault is a no-op — it
// preserves whatever boot order the machine already has. Use BootPXE or
// BootDisk to set a one-time (non-persistent) UEFI boot override.
func (b *BMCController) SetBootDevice(ctx context.Context, device BootDevice) error {
	var bmcDevice string
	switch device {
	case BootPXE:
		bmcDevice = "pxe"
	case BootDisk:
		bmcDevice = "disk"
	case BootDefault:
		return nil
	default:
		return fmt.Errorf("bmc %s: unsupported boot device: %s", b.host, device)
	}

	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	ok, err := b.client.SetBootDevice(ctx, bmcDevice, false, true)
	if err != nil {
		return fmt.Errorf("bmc %s set boot device %s: %w", b.host, device, err)
	}
	if !ok {
		return fmt.Errorf("bmc %s set boot device %s: provider returned failure", b.host, device)
	}
	return nil
}

// ChassisSerial returns the BMC's reported chassis serial via
// bmclib's Inventory call (which fans out across providers, ending
// up at Redfish /redfish/v1/Chassis/<id> for Supermicro/Dell, FRU
// for IPMI-only firmware). Used by the site-worker to verify the
// BMC at host actually belongs to the chassis NetBox said it would.
//
// Returns ("", err) on transport failure; ("", nil) when the BMC
// answered but reported an empty serial (rare; some firmware
// reports "0123456789" placeholders we treat as empty by trimming).
func (b *BMCController) ChassisSerial(ctx context.Context) (string, error) {
	ctx, cancel := b.withTimeout(ctx)
	defer cancel()

	// The canonical chassis identity is the FRU *Product* Serial (== the
	// Dell service tag == NetBox device.serial; see
	// internal/power/smcipmitool/parse.go). bmclib's
	// Inventory() returns common.Device.Serial, but that field is
	// vendor-dependent: a Dell iDRAC populates it from the *baseboard*
	// serial (FRU "Board Serial", e.g. CNIVC008B30820), NOT the service
	// tag (e.g. BV95DS2). Trusting dev.Serial therefore fails the serial-
	// match gate on every Redfish-capable Dell BMC (the iDRAC `root` user),
	// while the IPMI-only path below happened to be correct all along.
	//
	// So: whenever we have IPMI creds, read the FRU Product Serial directly
	// over ipmitool and treat it as authoritative — it's the same field the
	// gate compares against and it's consistent across vendors. bmclib
	// Inventory's dev.Serial is only used as a last resort when no creds are
	// available to run the FRU read (it's still better than no check at all).
	if b.user != "" {
		serial, ferr := b.fruSerialViaIPMItool(ctx)
		if ferr != nil {
			return "", fmt.Errorf("bmc %s: read FRU Product Serial via ipmitool: %w", b.host, ferr)
		}
		// We successfully read the FRU but it has no usable Product Serial
		// (field absent or blank). Since this is now the authoritative path
		// for any credentialed BMC and the result gates a destructive op,
		// fail CLOSED rather than returning ("", nil): an empty serial would
		// be treated as "inconclusive" by verifyChassisSerial and skip the
		// match entirely (see internal/worker/bmcruntime.go). We must not
		// power-cycle/wipe a box whose canonical identity we can't read.
		if strings.TrimSpace(serial) == "" {
			return "", fmt.Errorf("bmc %s: FRU read but Product Serial is empty/absent (cannot verify chassis identity)", b.host)
		}
		return serial, nil
	}

	// No IPMI creds: fall back to bmclib Inventory. This path can return a
	// vendor's idea of "Serial" (baseboard on Dell), so it's a weaker check;
	// it exists so the gate still runs on controllers we can't ipmitool.
	dev, err := b.client.Inventory(ctx)
	if err != nil {
		return "", fmt.Errorf("bmc %s inventory: %w", b.host, err)
	}
	// Fail CLOSED on an unreadable serial, same as the FRU path above: a
	// BMCController that can't produce a serial must not let the destructive
	// op proceed (verifyChassisSerial would treat "" as inconclusive and skip
	// the match). The empty-serial "skip" is reserved for PDUController, which
	// legitimately has no downstream chassis serial — a different controller
	// type, so hardening this BMC path doesn't affect it.
	if dev == nil || strings.TrimSpace(dev.Serial) == "" {
		return "", fmt.Errorf("bmc %s inventory: empty/absent serial (cannot verify chassis identity)", b.host)
	}
	return strings.TrimSpace(dev.Serial), nil
}

// fruSerialViaIPMItool shells `ipmitool fru print 0` and returns the
// trimmed "Product Serial" field — the FRU Product Serial that the
// credential store uses as the canonical chassis serial
// (== NetBox device.serial). Mirrors castpump's ipmitool invocation
// (lanplus, IPMI_PASSWORD env, per-host cipher, host:port split).
func (b *BMCController) fruSerialViaIPMItool(ctx context.Context) (string, error) {
	cipher := b.cipher
	if cipher == 0 {
		cipher = 3 // ipmitool/bmclib default; Dell iDRAC + AMI both accept 3
	}
	host := b.host
	args := []string{"-I", "lanplus", "-U", b.user, "-E", "-C", strconv.Itoa(cipher)}
	if h, port, err := net.SplitHostPort(b.host); err == nil {
		host = h
		args = append(args, "-H", host, "-p", port)
	} else {
		args = append(args, "-H", host)
	}
	args = append(args, "fru", "print", "0")

	cmd := exec.CommandContext(ctx, "ipmitool", args...)
	cmd.Env = append(os.Environ(), "IPMI_PASSWORD="+b.pass)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ipmitool fru print: %w", err)
	}
	return parseFRUProductSerial(string(out)), nil
}

// parseFRUProductSerial extracts the "Product Serial" field from
// `ipmitool fru print 0` output. This is deliberately specific: ipmitool
// prints both "Board Serial" (the baseboard) and "Product Serial" (the
// chassis service tag) and ONLY the latter is the canonical chassis
// identity == NetBox device.serial. A substring/prefix match would pick up
// "Board Serial" too, so we require an exact (case-insensitive) key match.
// Returns "" when no Product Serial field is present (or it's blank).
// ChassisSerial turns that "" into a fail-closed error for credentialed
// BMCs — an unreadable canonical serial must not silently skip the gate.
func parseFRUProductSerial(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "Product Serial") {
			return strings.TrimSpace(val)
		}
	}
	return "" // FRU readable but no Product Serial field; treat as inconclusive
}

// Transport maps bmclib's successful-provider metadata onto the
// operator-facing transport names. bmclib's Redfish provider is
// registered as "gofish"; everything IPMI flows through "ipmitool".
// Before any call has succeeded (or for an unrecognized provider) the
// generic "bmc" is returned rather than guessing.
func (b *BMCController) Transport() string {
	switch b.client.GetMetadata().SuccessfulProvider {
	case "gofish":
		return "redfish"
	case "ipmitool":
		return "ipmi"
	case "":
		return "bmc"
	default:
		// Vendor-specific providers (supermicro, dell, asrockrack,
		// openbmc) are all Redfish-or-vendor-HTTP under the hood, but
		// reporting their bmclib name is more honest than collapsing
		// them to "redfish".
		return b.client.GetMetadata().SuccessfulProvider
	}
}

func (b *BMCController) Close(ctx context.Context) error {
	if err := b.client.Close(ctx); err != nil {
		return fmt.Errorf("bmc %s close: %w", b.host, err)
	}
	return nil
}

var _ Controller = (*BMCController)(nil)
