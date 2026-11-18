package power

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/jacobweinstock/iamt"
)

// AMTWSManPort is Intel AMT's WS-Man-over-TLS port. AMT also listens on
// plaintext :16992, which slagworks never uses: the digest credential and
// the boot override would cross the OOB segment in the clear.
const AMTWSManPort = 16993

// ErrChassisSerialUnsupported is returned by AMTController.ChassisSerial.
// iamt exposes no inventory read, and reading CIM_Chassis would need a
// second WS-Man client next to the vendored one. The chassis-serial gate
// is therefore a BMC-path-only check: the site-worker executor never calls
// ChassisSerial on an AMT controller (see worker.Executor). The error
// exists so an accidental call fails closed instead of reading as "no
// serial concept" (the ("", nil) PDU contract), which would skip the gate.
var ErrChassisSerialUnsupported = errors.New("amt: chassis serial read is not supported over Intel AMT; the serial gate runs on the BMC path only")

// amtClient is the subset of *iamt.Client the controller drives, as an
// interface so tests can substitute a fake without a WS-Man server.
type amtClient interface {
	Open(ctx context.Context) error
	Close(ctx context.Context) error
	PowerOn(ctx context.Context) error
	PowerOff(ctx context.Context) error
	PowerCycle(ctx context.Context) error
	SetPXE(ctx context.Context) error
	IsPoweredOn(ctx context.Context) (bool, error)
}

// AMTController drives an Intel AMT (vPro) endpoint over WS-Man/TLS
// (:16993, digest auth) via the vendored iamt client. Power and power
// status map onto CIM_PowerManagementService; SetBootDevice(pxe) is AMT's
// one-time "Force PXE Boot" (AMT_BootSettingData + SetBootConfigRole +
// ChangeBootOrder), which AMT applies to the next AMT-initiated power
// transition. Not safe for concurrent use, same as BMCController.
type AMTController struct {
	client  amtClient
	host    string
	timeout time.Duration
	log     *slog.Logger
}

// NewAMTController builds a controller for the AMT endpoint at host. host
// is normally a bare IP (the device's NetBox oob_ip) and gets the AMT TLS
// port; a "host:port" form (test fakes) overrides the port. Open performs
// the digest-challenge round trip, so a dead endpoint or a wrong host
// fails here rather than on the first actuation.
//
// TLS verification is off inside iamt (InsecureSkipVerify): AMT presents a
// self-signed certificate minted at provisioning, with no CA slagworks
// could pin. The digest credential is not replayable and the endpoint's
// identity is pinned by NetBox's oob_ip, the same trust model as the
// IPMI/Redfish BMC path.
func NewAMTController(ctx context.Context, host, user, pass string, timeout time.Duration, log *slog.Logger) (*AMTController, error) {
	if log == nil {
		log = slog.Default()
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if host == "" {
		return nil, errors.New("amt: empty host (the device has no NetBox oob_ip)")
	}
	if user == "" || pass == "" {
		return nil, fmt.Errorf("amt %s: incomplete credential (populate the device's kind=amt bmc-creds-admin entry)", host)
	}
	bare, port := host, uint32(AMTWSManPort)
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, perr := strconv.ParseUint(p, 10, 16)
		if perr != nil {
			return nil, fmt.Errorf("amt: bad port in host %q: %w", host, perr)
		}
		bare, port = h, uint32(n)
	}
	client := iamt.NewClient(bare, user, pass, iamt.WithScheme("https"), iamt.WithPort(port))
	c := &AMTController{client: client, host: host, timeout: timeout, log: log}
	octx, cancel := c.withTimeout(ctx)
	defer cancel()
	if err := client.Open(octx); err != nil {
		return nil, fmt.Errorf("amt %s open: %w", host, err)
	}
	return c, nil
}

func (a *AMTController) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, a.timeout)
}

func (a *AMTController) PowerOn(ctx context.Context) error {
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	if err := a.client.PowerOn(ctx); err != nil {
		return fmt.Errorf("amt %s power on: %w", a.host, err)
	}
	return nil
}

func (a *AMTController) PowerOff(ctx context.Context) error {
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	if err := a.client.PowerOff(ctx); err != nil {
		return fmt.Errorf("amt %s power off: %w", a.host, err)
	}
	return nil
}

// PowerCycle cycles a powered-on host and powers on a powered-off one
// (iamt's semantics). iamt picks the first AMT-advertised transition from
// its cycle preference list (graceful variants first, then hard power
// cycle, then master bus reset).
func (a *AMTController) PowerCycle(ctx context.Context) error {
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	if err := a.client.PowerCycle(ctx); err != nil {
		return fmt.Errorf("amt %s power cycle: %w", a.host, err)
	}
	return nil
}

// PowerStatus maps AMT's CIM power state onto On/Off. iamt reports "on"
// only for CIM PowerState 2; sleep and every off variant read as off.
func (a *AMTController) PowerStatus(ctx context.Context) (State, error) {
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	on, err := a.client.IsPoweredOn(ctx)
	if err != nil {
		return StateUnknown, fmt.Errorf("amt %s power status: %w", a.host, err)
	}
	if on {
		return StateOn, nil
	}
	return StateOff, nil
}

// SetBootDevice arms AMT's one-time Force PXE Boot for BootPXE. BootDefault
// is a no-op (same contract as BMCController: keep the firmware order).
// AMT has no "boot from local disk" override in iamt, so BootDisk is an
// explicit error rather than a silent no-op that would leave the host
// booting whatever its firmware order says.
func (a *AMTController) SetBootDevice(ctx context.Context, device BootDevice) error {
	switch device {
	case BootDefault:
		return nil
	case BootPXE:
	default:
		return fmt.Errorf("amt %s: unsupported boot device %q (Intel AMT supports only a one-time pxe override)", a.host, device)
	}
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	if err := a.client.SetPXE(ctx); err != nil {
		return fmt.Errorf("amt %s set one-time pxe boot: %w", a.host, err)
	}
	return nil
}

// ChassisSerial is unsupported over AMT; see ErrChassisSerialUnsupported.
func (a *AMTController) ChassisSerial(context.Context) (string, error) {
	return "", ErrChassisSerialUnsupported
}

// Transport is always "amt": there is exactly one wire path.
func (a *AMTController) Transport() string { return "amt" }

func (a *AMTController) Close(ctx context.Context) error {
	if err := a.client.Close(ctx); err != nil {
		return fmt.Errorf("amt %s close: %w", a.host, err)
	}
	return nil
}

var _ Controller = (*AMTController)(nil)
