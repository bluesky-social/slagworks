package power

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bmc-toolbox/bmclib/v2/bmc"
	"github.com/bmc-toolbox/common"
)

type mockBMCClient struct {
	powerState  string
	bootDevice  string
	bootPersist bool
	bootEFI     bool
	failPower   bool
	failBoot    bool
	failStatus  bool
	failClose   bool
	failInvent  bool
	returnFalse bool // return (false, nil) instead of (true, nil)
	setCalls    []string
	bootCalls   []string
	invSerial   string // serial returned by mocked Inventory()
	invErr      error  // explicit error from mocked Inventory() (overrides failInvent)
	provider    string // SuccessfulProvider returned by GetMetadata
}

func (m *mockBMCClient) SetPowerState(_ context.Context, state string) (bool, error) {
	m.setCalls = append(m.setCalls, state)
	if m.failPower {
		return false, fmt.Errorf("mock power failure")
	}
	if m.returnFalse {
		return false, nil
	}
	m.powerState = state
	return true, nil
}

func (m *mockBMCClient) GetPowerState(_ context.Context) (string, error) {
	if m.failStatus {
		return "", fmt.Errorf("mock status failure")
	}
	return m.powerState, nil
}

func (m *mockBMCClient) SetBootDevice(_ context.Context, device string, persistent, efi bool) (bool, error) {
	m.bootCalls = append(m.bootCalls, device)
	if m.failBoot {
		return false, fmt.Errorf("mock boot failure")
	}
	if m.returnFalse {
		return false, nil
	}
	m.bootDevice = device
	m.bootPersist = persistent
	m.bootEFI = efi
	return true, nil
}

func (m *mockBMCClient) Inventory(_ context.Context) (*common.Device, error) {
	if m.invErr != nil {
		return nil, m.invErr
	}
	if m.failInvent {
		return nil, fmt.Errorf("mock inventory failure")
	}
	return &common.Device{Common: common.Common{Serial: m.invSerial}}, nil
}

func (m *mockBMCClient) GetMetadata() bmc.Metadata {
	return bmc.Metadata{SuccessfulProvider: m.provider}
}

func (m *mockBMCClient) Close(_ context.Context) error {
	if m.failClose {
		return fmt.Errorf("mock close failure")
	}
	return nil
}

func newTestController(mock *mockBMCClient) *BMCController {
	return newBMCControllerWithClient(mock, "test-host")
}

func TestBMCController_PowerOn(t *testing.T) {
	mock := &mockBMCClient{powerState: "off"}
	ctrl := newTestController(mock)

	if err := ctrl.PowerOn(context.Background()); err != nil {
		t.Fatalf("power on: %v", err)
	}
	if len(mock.setCalls) != 1 || mock.setCalls[0] != "on" {
		t.Fatalf("expected SetPowerState('on'), got %v", mock.setCalls)
	}
}

func TestBMCController_PowerOff(t *testing.T) {
	mock := &mockBMCClient{powerState: "on"}
	ctrl := newTestController(mock)

	if err := ctrl.PowerOff(context.Background()); err != nil {
		t.Fatalf("power off: %v", err)
	}
	if mock.setCalls[0] != "off" {
		t.Fatalf("expected 'off', got %s", mock.setCalls[0])
	}
}

func TestBMCController_PowerCycle(t *testing.T) {
	mock := &mockBMCClient{powerState: "on"}
	ctrl := newTestController(mock)

	if err := ctrl.PowerCycle(context.Background()); err != nil {
		t.Fatalf("power cycle: %v", err)
	}
	if mock.setCalls[0] != "cycle" {
		t.Fatalf("expected 'cycle', got %s", mock.setCalls[0])
	}
}

func TestBMCController_PowerStatus(t *testing.T) {
	mock := &mockBMCClient{powerState: "on"}
	ctrl := newTestController(mock)

	state, err := ctrl.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state != StateOn {
		t.Fatalf("expected on, got %s", state)
	}

	mock.powerState = "off"
	state, err = ctrl.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state != StateOff {
		t.Fatalf("expected off, got %s", state)
	}

	// bmclib returns various capitalization
	for _, raw := range []string{"PoweredOn", "poweredon"} {
		mock.powerState = raw
		state, err = ctrl.PowerStatus(context.Background())
		if err != nil {
			t.Fatalf("status for %q: %v", raw, err)
		}
		if state != StateOn {
			t.Fatalf("expected on for %q, got %s", raw, state)
		}
	}

	mock.powerState = "weird"
	state, err = ctrl.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if state != StateUnknown {
		t.Fatalf("expected unknown for weird state, got %s", state)
	}
}

// TestBMCController_Transport pins the bmclib-provider → operator-
// facing transport-name mapping. "gofish" is bmclib's Redfish provider
// name; reporting it raw would confuse operators, so the mapping is
// part of the contract.
func TestBMCController_Transport(t *testing.T) {
	cases := map[string]string{
		"gofish":     "redfish",
		"ipmitool":   "ipmi",
		"":           "bmc",
		"supermicro": "supermicro", // vendor providers pass through
	}
	for provider, want := range cases {
		ctrl := newTestController(&mockBMCClient{provider: provider})
		if got := ctrl.Transport(); got != want {
			t.Errorf("provider %q: got %q, want %q", provider, got, want)
		}
	}
}

func TestBMCController_SetBootDevice(t *testing.T) {
	mock := &mockBMCClient{}
	ctrl := newTestController(mock)

	if err := ctrl.SetBootDevice(context.Background(), BootPXE); err != nil {
		t.Fatalf("set boot pxe: %v", err)
	}
	if mock.bootDevice != "pxe" {
		t.Fatalf("expected pxe, got %s", mock.bootDevice)
	}
	if mock.bootPersist {
		t.Fatal("expected non-persistent boot")
	}
	if !mock.bootEFI {
		t.Fatal("expected EFI boot")
	}

	if err := ctrl.SetBootDevice(context.Background(), BootDisk); err != nil {
		t.Fatalf("set boot disk: %v", err)
	}
	if mock.bootDevice != "disk" {
		t.Fatalf("expected disk, got %s", mock.bootDevice)
	}

	// BootDefault is a no-op
	if err := ctrl.SetBootDevice(context.Background(), BootDefault); err != nil {
		t.Fatalf("set boot default: %v", err)
	}
	if mock.bootDevice != "disk" {
		t.Fatalf("expected disk (unchanged), got %s", mock.bootDevice)
	}
}

func TestBMCController_PowerFailure(t *testing.T) {
	mock := &mockBMCClient{failPower: true}
	ctrl := newTestController(mock)

	if err := ctrl.PowerOn(context.Background()); err == nil {
		t.Fatal("expected error on power failure")
	}
}

func TestBMCController_StatusFailure(t *testing.T) {
	mock := &mockBMCClient{failStatus: true}
	ctrl := newTestController(mock)

	state, err := ctrl.PowerStatus(context.Background())
	if err == nil {
		t.Fatal("expected error on status failure")
	}
	if state != StateUnknown {
		t.Fatalf("expected unknown on failure, got %s", state)
	}
}

func TestBMCController_BootDeviceFailure(t *testing.T) {
	mock := &mockBMCClient{failBoot: true}
	ctrl := newTestController(mock)

	if err := ctrl.SetBootDevice(context.Background(), BootPXE); err == nil {
		t.Fatal("expected error on boot device failure")
	}
}

func TestBMCController_ErrorIncludesHost(t *testing.T) {
	mock := &mockBMCClient{failPower: true}
	ctrl := newBMCControllerWithClient(mock, "bmc-42.example.com")

	err := ctrl.PowerOn(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bmc-42.example.com") {
		t.Fatalf("error should include host, got: %s", err)
	}
}

func TestBMCController_ProviderReturnedFailure(t *testing.T) {
	mock := &mockBMCClient{returnFalse: true}
	ctrl := newTestController(mock)

	for _, tc := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"PowerOn", ctrl.PowerOn},
		{"PowerOff", ctrl.PowerOff},
		{"PowerCycle", ctrl.PowerCycle},
	} {
		err := tc.fn(context.Background())
		if err == nil {
			t.Fatalf("%s: expected error for (false, nil) return", tc.name)
		}
		if !strings.Contains(err.Error(), "provider returned failure") {
			t.Fatalf("%s: expected 'provider returned failure', got: %s", tc.name, err)
		}
	}

	// SetBootDevice (false, nil) path
	err := ctrl.SetBootDevice(context.Background(), BootPXE)
	if err == nil {
		t.Fatal("SetBootDevice: expected error for (false, nil) return")
	}
	if !strings.Contains(err.Error(), "provider returned failure") {
		t.Fatalf("SetBootDevice: expected 'provider returned failure', got: %s", err)
	}
}

func TestBMCController_SetBootDevice_Invalid(t *testing.T) {
	mock := &mockBMCClient{}
	ctrl := newTestController(mock)

	err := ctrl.SetBootDevice(context.Background(), BootDevice("bios"))
	if err == nil {
		t.Fatal("expected error for unsupported boot device")
	}
	if !strings.Contains(err.Error(), "unsupported boot device") {
		t.Fatalf("expected 'unsupported boot device', got: %s", err)
	}
}

func TestBMCController_CloseError(t *testing.T) {
	mock := &mockBMCClient{failClose: true}
	ctrl := newTestController(mock)

	err := ctrl.Close(context.Background())
	if err == nil {
		t.Fatal("expected error on close failure")
	}
	if !strings.Contains(err.Error(), "test-host") {
		t.Fatalf("close error should include host, got: %s", err)
	}
}

// TestParseFRUProductSerial_PicksProductNotBoard is the regression test for
// the gw-2.pop1 mis-identification: ipmitool prints BOTH "Board Serial" and
// "Product Serial"; only the latter is the chassis service tag (== NetBox
// device.serial). The parser must return the Product Serial, never the Board
// Serial — a substring match on "Serial" would wrongly grab the baseboard.
// Fixture is the real `ipmitool fru print 0` shape from oob-gw-2.pop1's iDRAC.
func TestParseFRUProductSerial_PicksProductNotBoard(t *testing.T) {
	const fru = `FRU Device Description : Builtin FRU Device (ID 0)
 Board Mfg             : DELL
 Board Product         : PowerEdge R640
 Board Serial          : CNIVC008B30820
 Board Part Number     : 0PHYDRA00
 Product Manufacturer  : DELL
 Product Name          : PowerEdge R640
 Product Version       : 01
 Product Serial        : BV95DS2
 Product Asset Tag     :
`
	if got := parseFRUProductSerial(fru); got != "BV95DS2" {
		t.Errorf("parseFRUProductSerial = %q, want %q (must not return Board Serial CNIVC008B30820)", got, "BV95DS2")
	}
}

// TestParseFRUProductSerial_MissingProduct: FRU readable but no Product Serial
// field -> "". ChassisSerial turns this "" into a fail-closed error for
// credentialed BMCs (it must NOT silently grab the Board Serial that IS
// present, nor skip the gate).
func TestParseFRUProductSerial_MissingProduct(t *testing.T) {
	const fru = " Board Serial          : CNIVC008B30820\n Product Name          : PowerEdge R640\n"
	if got := parseFRUProductSerial(fru); got != "" {
		t.Errorf("parseFRUProductSerial = %q, want empty (no Product Serial field present)", got)
	}
}

// TestParseFRUProductSerial_BlankValue: Product Serial field present but blank
// -> "" (so ChassisSerial fails closed rather than passing an empty serial).
func TestParseFRUProductSerial_BlankValue(t *testing.T) {
	const fru = " Board Serial          : CNIVC008B30820\n Product Serial        :\n"
	if got := parseFRUProductSerial(fru); got != "" {
		t.Errorf("parseFRUProductSerial = %q, want empty (blank Product Serial)", got)
	}
}

// TestChassisSerial_NoCreds_FallsBackToInventory: with no IPMI creds the
// controller can't run the FRU read, so it falls back to bmclib Inventory's
// dev.Serial. This is the weaker path (vendor-dependent serial) but still
// runs the gate.
func TestChassisSerial_NoCreds_FallsBackToInventory(t *testing.T) {
	mock := &mockBMCClient{invSerial: "INV-SERIAL-1"}
	ctrl := newBMCControllerWithClient(mock, "test-host") // user == "" => no FRU read
	got, err := ctrl.ChassisSerial(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "INV-SERIAL-1" {
		t.Errorf("ChassisSerial = %q, want INV-SERIAL-1 (Inventory fallback)", got)
	}
}

// TestChassisSerial_NoCreds_InventoryError_Surfaced: with no creds, an
// Inventory transport error is surfaced directly (the gate must not silently
// pass when it can't read a serial at all).
func TestChassisSerial_NoCreds_InventoryError_Surfaced(t *testing.T) {
	mock := &mockBMCClient{invErr: fmt.Errorf("connection refused")}
	ctrl := newBMCControllerWithClient(mock, "test-host") // no creds
	_, err := ctrl.ChassisSerial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("transport error should surface, got: %v", err)
	}
}

// TestChassisSerial_NoCreds_EmptyInventory_FailsClosed: with no creds and a
// BMC that returns a blank serial, the gate must FAIL CLOSED, not return
// ("", nil) — an empty serial would be treated as inconclusive by
// verifyChassisSerial and skip the chassis-identity match on a destructive op.
// (The empty-serial "skip" is reserved for PDUController, a different type.)
func TestChassisSerial_NoCreds_EmptyInventory_FailsClosed(t *testing.T) {
	mock := &mockBMCClient{invSerial: ""} // BMC answers but serial is blank
	ctrl := newBMCControllerWithClient(mock, "test-host")
	_, err := ctrl.ChassisSerial(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot verify chassis identity") {
		t.Fatalf("empty inventory serial should fail closed, got: %v", err)
	}
}
