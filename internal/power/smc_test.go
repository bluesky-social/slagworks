package power

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeSMCTool writes a Bash stand-in for the SMCIPMITool launcher: it
// records its argv to a file and echoes a canned stdout with a canned exit
// code. This lets the SMCController tests drive the real runner (process
// exec, timeout, output scrub, exit classification) without the JVM/jar.
// Mirrors fakeTool in internal/power/smcipmitool/runner_test.go and the
// fakeIPMITool pattern in internal/bmcreconcile.
func fakeSMCTool(t *testing.T, stdout string, exit int) (binPath, argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tool is a bash script; not for windows")
	}
	dir := t.TempDir()
	binPath = filepath.Join(dir, "SMCIPMITool")
	argvLog = filepath.Join(dir, "argv.log")
	script := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" > " + argvLog + "\n" +
		"cat <<'EOF'\n" + stdout + "\nEOF\n" +
		"exit " + itoaSMC(exit) + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return binPath, argvLog
}

func itoaSMC(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// newTestSMCController builds an SMCController whose runner points at a fake
// binary instead of resolving SMCIPMITool on PATH.
func newTestSMCController(t *testing.T, stdout string, exit int) (*SMCController, string) {
	c := NewSMCController("192.0.2.9", "ADMIN", "s3cr3t", 5*time.Second, nil)
	binPath, argvLog := fakeSMCTool(t, stdout, exit)
	c.runner.Bin = binPath
	return c, argvLog
}

func TestSMCControllerImplementsController(t *testing.T) {
	// Compile-time assertion lives in smc.go; this documents intent and
	// guards against the var _ being removed.
	var _ Controller = NewSMCController("h", "u", "p", 0, nil)
}

func TestSMCControllerPowerStatusOn(t *testing.T) {
	c, _ := newTestSMCController(t, "Power is currently on.", 0)
	st, err := c.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if st != StateOn {
		t.Fatalf("state = %q, want on", st)
	}
}

func TestSMCControllerPowerStatusOff(t *testing.T) {
	c, _ := newTestSMCController(t, "Power is currently off.", 0)
	st, err := c.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if st != StateOff {
		t.Fatalf("state = %q, want off", st)
	}
}

func TestSMCControllerPowerStatusError(t *testing.T) {
	// A connect failure (nonzero exit + reason text) must surface as an
	// error with the host wrapped in, not a bogus state.
	c, _ := newTestSMCController(t, "Can't connect to 192.0.2.9", 145)
	st, err := c.PowerStatus(context.Background())
	if err == nil {
		t.Fatal("expected error on connect failure")
	}
	if st != StateUnknown {
		t.Fatalf("state = %q, want unknown on error", st)
	}
	if !strings.Contains(err.Error(), "192.0.2.9") {
		t.Fatalf("error should name the host: %v", err)
	}
}

func TestSMCControllerPowerActuationVerbs(t *testing.T) {
	cases := []struct {
		name string
		call func(*SMCController, context.Context) error
		want string // substring the fake's recorded argv must contain
	}{
		{"on", (*SMCController).PowerOn, "ipmi power up"},
		{"off", (*SMCController).PowerOff, "ipmi power down"},
		{"cycle", (*SMCController).PowerCycle, "ipmi power cycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, argvLog := newTestSMCController(t, "", 0)
			if err := tc.call(c, context.Background()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got, _ := os.ReadFile(argvLog)
			if !strings.Contains(string(got), tc.want) {
				t.Fatalf("argv %q missing %q", got, tc.want)
			}
		})
	}
}

func TestSMCControllerSetBootDevice(t *testing.T) {
	cases := []struct {
		name   string
		device BootDevice
		want   string // argv substring; "" means no invocation expected
	}{
		{"pxe one-time uefi", BootPXE, "ipmi power bootoption 14 0"},
		{"disk one-time uefi", BootDisk, "ipmi power bootoption 9 0"},
		{"default is a no-op", BootDefault, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, argvLog := newTestSMCController(t, "Set boot device done", 0)
			if err := c.SetBootDevice(context.Background(), tc.device); err != nil {
				t.Fatalf("SetBootDevice(%s): %v", tc.device, err)
			}
			got, _ := os.ReadFile(argvLog)
			if tc.want == "" {
				// BootDefault must not invoke the tool at all.
				if len(got) != 0 {
					t.Fatalf("BootDefault should be a no-op, but tool ran with argv %q", got)
				}
				return
			}
			if !strings.Contains(string(got), tc.want) {
				t.Fatalf("argv %q missing %q", got, tc.want)
			}
		})
	}
}

func TestSMCControllerSetBootDeviceUnsupported(t *testing.T) {
	c, _ := newTestSMCController(t, "", 0)
	err := c.SetBootDevice(context.Background(), BootDevice("floppy"))
	if err == nil {
		t.Fatal("expected error for unsupported boot device")
	}
	if !strings.Contains(err.Error(), "unsupported boot device") {
		t.Fatalf("error should name the cause: %v", err)
	}
}

func TestSMCControllerChassisSerial(t *testing.T) {
	// ChassisSerial must return the PRODUCT serial (matches NetBox device
	// serial), not the chassis (CS) or board (BS) serial. The fru carries
	// all three distinct values.
	const fru = "Chassis Information\n" +
		"  Chassis Type           = Other\n" +
		"  Chassis Serial Number (CS)     = CH119KN44BP0A98\n" +
		"  Board Serial Number (BS)       = BS000000\n" +
		"  Product Serial Number (PS)     = S500001X0000001\n"
	c, _ := newTestSMCController(t, fru, 0)
	serial, err := c.ChassisSerial(context.Background())
	if err != nil {
		t.Fatalf("ChassisSerial: %v", err)
	}
	if serial != "S500001X0000001" {
		t.Fatalf("serial = %q, want product serial S500001X0000001", serial)
	}
}

// A blank Product Serial must fail closed, not return ("", nil): the
// worker treats an empty serial as "skip the identity check", and this
// result gates destructive ops. Matches BMCController.ChassisSerial.
func TestSMCControllerChassisSerialBlankFailsClosed(t *testing.T) {
	c, _ := newTestSMCController(t, "  Product Serial Number (PS)     = \n", 0)
	serial, err := c.ChassisSerial(context.Background())
	if err == nil {
		t.Fatalf("ChassisSerial = %q, nil; want an error for a blank serial", serial)
	}
}

func TestSMCControllerImplementsBootManager(t *testing.T) {
	var _ BootManager = NewSMCController("h", "u", "p", 0, nil)
}

func TestSMCControllerBootStatus(t *testing.T) {
	bootTable := func(next, flag, persist string) string {
		return " Option          |           Value \n" +
			" ------          |           ----- \n" +
			" Next boot       |        " + next + " \n" +
			" Boot flag       |           " + flag + " \n" +
			" Persistent      |           " + persist + " \n"
	}
	cases := []struct {
		name        string
		out         string
		wantDevice  BootDevice
		wantValid   bool
		wantPersist bool
		wantRaw     string
	}{
		{"armed one-time pxe", bootTable("UEFI_PXE", "Valid", "False"), BootPXE, true, false, "UEFI_PXE"},
		{"armed persistent pxe", bootTable("UEFI_PXE", "Valid", "True"), BootPXE, true, true, "UEFI_PXE"},
		{"armed disk", bootTable("UEFI_Hard-drive", "Valid", "False"), BootDisk, true, false, "UEFI_Hard-drive"},
		{"cleared / none", bootTable("NONE", "Invalid", "False"), BootDefault, false, false, "NONE"},
		{"armed unmapped device -> BootOther", bootTable("UEFI_USB", "Valid", "False"), BootOther, true, false, "UEFI_USB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestSMCController(t, tc.out, 0)
			bo, err := c.BootStatus(context.Background())
			if err != nil {
				t.Fatalf("BootStatus: %v", err)
			}
			if bo.Device != tc.wantDevice {
				t.Errorf("Device = %q, want %q", bo.Device, tc.wantDevice)
			}
			if bo.Valid != tc.wantValid {
				t.Errorf("Valid = %v, want %v", bo.Valid, tc.wantValid)
			}
			if bo.Persistent != tc.wantPersist {
				t.Errorf("Persistent = %v, want %v", bo.Persistent, tc.wantPersist)
			}
			if bo.Raw != tc.wantRaw {
				t.Errorf("Raw = %q, want %q", bo.Raw, tc.wantRaw)
			}
		})
	}
}

func TestSMCControllerBootStatusError(t *testing.T) {
	// A connect failure (nonzero exit) must surface as an error, NOT a
	// bogus "nothing armed" BootOverride.
	c, _ := newTestSMCController(t, "Can't connect to 10.0.0.1", 145)
	_, err := c.BootStatus(context.Background())
	if err == nil {
		t.Fatal("expected error on connect failure, not a Valid:false override")
	}
}

func TestSMCControllerSetBootOverride(t *testing.T) {
	cases := []struct {
		name       string
		device     BootDevice
		persistent bool
		wantArgv   string // substring the fake's recorded argv must contain
	}{
		{"pxe one-time", BootPXE, false, "ipmi power bootoption 14 0"},
		{"pxe persistent", BootPXE, true, "ipmi power bootoption 14 1"},
		{"disk one-time", BootDisk, false, "ipmi power bootoption 9 0"},
		{"clear ignores persistent", BootDefault, true, "ipmi power bootoption 0 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, argvLog := newTestSMCController(t, "Set boot device done", 0)
			if err := c.SetBootOverride(context.Background(), tc.device, tc.persistent); err != nil {
				t.Fatalf("SetBootOverride: %v", err)
			}
			got, _ := os.ReadFile(argvLog)
			if !strings.Contains(string(got), tc.wantArgv) {
				t.Fatalf("argv %q missing %q", got, tc.wantArgv)
			}
		})
	}
}

func TestSMCControllerSetBootOverrideUnsupported(t *testing.T) {
	c, _ := newTestSMCController(t, "", 0)
	err := c.SetBootOverride(context.Background(), BootDevice("floppy"), false)
	if err == nil {
		t.Fatal("expected error for unsupported boot device")
	}
}

func TestSMCControllerTransport(t *testing.T) {
	c := NewSMCController("h", "u", "p", 0, nil)
	if got := c.Transport(); got != "smcipmitool" {
		t.Fatalf("Transport() = %q, want smcipmitool", got)
	}
}

func TestSMCControllerClose(t *testing.T) {
	c := NewSMCController("h", "u", "p", 0, nil)
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close should be a no-op, got %v", err)
	}
}
