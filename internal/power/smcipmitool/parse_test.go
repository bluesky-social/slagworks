package smcipmitool

import "testing"

// Golden fixtures captured from a live Supermicro H13SSH BMC via
// SMCIPMITool 2.30.0 (serials replaced with synthetic values). Where a
// string could not be observed safely on live hardware (e.g. the
// powered-off form, which would require powering the host down), it is
// marked INFERRED and the parser is written to accept
// the documented symmetric form. Re-capture and de-infer when a host is
// legitimately in that state.

// --- power status ---

const fixturePowerOn = "Power is currently on.\n" // OBSERVED, exit 0

// INFERRED from the symmetric phrasing of the observed "on" string and
// User's Guide §3.6.2.1. Not yet observed on live hardware.
const fixturePowerOff = "Power is currently off.\n"

func TestParsePowerStatus(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		want    PowerState
		wantErr bool
	}{
		{"on observed", fixturePowerOn, PowerOn, false},
		{"off inferred", fixturePowerOff, PowerOff, false},
		// Case/whitespace robustness — the tool's exact casing is the
		// authority, but we don't want to be brittle to trailing CRs etc.
		{"on trailing crlf", "Power is currently on.\r\n", PowerOn, false},
		{"unrecognized", "Some unexpected banner\n", PowerUnknown, true},
		{"empty", "", PowerUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParsePowerStatus(tc.out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("state = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- chassis serial (ipmi fru) ---

// OBSERVED full fru output, exit 0. Note the real labels carry the
// "(CS)/(BS)/(PS)" abbreviation suffixes the User's Guide sample omitted.
// The serial we want is the PRODUCT serial (PS) = S500001X0000001, which
// equals the NetBox device serial (commission sources it from DMI
// system-serial-number). The Chassis (CS) and Board (BS) serials are
// DIFFERENT physical-component serials and must NOT be picked.
const fixtureFRU = `Getting FRU ...
Chassis Type (CT)              = Other (01h)
Chassis Part Number (CP)       = CSE-HS119-R1K24P2-A
Chassis Serial Number (CS)     = CH119KN44BP0A98
Board mfg. Date/Time (BDT)     = 2024/12/25 16:00:00 (00 9E E8)
Board Manufacturer Name (BM)   = Supermicro
Board Product Name (BPN)       = H13SSH
Board Serial Number (BS)       = OM24CS605140
Board Part Number (BP)         = H13SSH
Board FRU File ID              =
Product Manufacturer Name (PM) = Supermicro
Product Name (PN)              =
Product PartModel Number (PPM) = AS -1115HS-TNR
Product Version (PV)           =
Product Serial Number (PS)     = S500001X0000001
Product Asset Tag (PAT)        =
Product FRU File ID            =
`

func TestParseChassisSerial(t *testing.T) {
	got, err := ParseChassisSerial(fixtureFRU)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// The PRODUCT serial — the one that matches the NetBox device serial
	// and what bmclib's Inventory().Serial returns. NOT the chassis (CS)
	// or board (BS) serial.
	const want = "S500001X0000001"
	if got != want {
		t.Fatalf("serial = %q, want product serial %q", got, want)
	}
}

func TestParseChassisSerialPicksProductNotChassisOrBoard(t *testing.T) {
	// The fru output has THREE serial-bearing lines (Chassis, Board,
	// Product). The identity check compares against the NetBox device
	// serial (= DMI system-serial-number = the Product serial). Picking
	// Chassis or Board would silently compare against the wrong physical
	// identifier and make every Supermicro op fail the serial-match guard.
	// This was a real bug caught in live-hardware validation.
	got, _ := ParseChassisSerial(fixtureFRU)
	for _, wrong := range []string{"CH119KN44BP0A98", "OM24CS605140"} {
		if got == wrong {
			t.Fatalf("picked the wrong serial %q (chassis/board), want product S500001X0000001", got)
		}
	}
}

func TestParseChassisSerialEmptyValue(t *testing.T) {
	// Some firmware leaves the product serial blank. An empty serial is
	// "no serial reported", not an error — mirrors BMCController.ChassisSerial
	// returning ("",nil).
	const blank = "Getting FRU ...\nProduct Serial Number (PS)     = \nBoard Serial Number (BS)       = X\n"
	got, err := ParseChassisSerial(blank)
	if err != nil {
		t.Fatalf("unexpected err on blank serial: %v", err)
	}
	if got != "" {
		t.Fatalf("serial = %q, want empty", got)
	}
}

func TestParseChassisSerialMissingField(t *testing.T) {
	// No product-serial line at all → error (distinct from blank value).
	_, err := ParseChassisSerial("Getting FRU ...\nBoard Serial Number (BS) = X\n")
	if err == nil {
		t.Fatal("expected error when product serial line absent")
	}
}

// --- boot option query (ipmi power bootoption, no args) ---

// OBSERVED on live hardware after `ipmi power bootoption 14 0`, exit 0.
// The query form (no index args) returns the current next-boot override.
// "Boot flag = Valid" means the override is armed; "Invalid" means nothing
// set it this session. We parse "Next boot" to verify a set took effect.
const fixtureBootOptionPXE = ` Option          |           Value
 ------          |           -----
 Next boot       |        UEFI_PXE
 Boot flag       |           Valid
 Persistent      |           False
`

func TestParseBootOption(t *testing.T) {
	bo, err := ParseBootOption(fixtureBootOptionPXE)
	if err != nil {
		t.Fatalf("ParseBootOption: %v", err)
	}
	if bo.NextBoot != "UEFI_PXE" {
		t.Fatalf("NextBoot = %q, want UEFI_PXE", bo.NextBoot)
	}
	if bo.Persistent {
		t.Fatalf("Persistent = true, want false (one-time)")
	}
	if !bo.Valid {
		t.Fatalf("Valid = false, want true (Boot flag = Valid)")
	}
}

// fixtureBootOptionCleared is the OBSERVED state after `bootoption 0 0`
// (validated live 2026-06-13): Next boot=NONE, Boot flag=Invalid — i.e. no
// override armed. Valid must be false so BootStatus reports "none armed".
const fixtureBootOptionCleared = ` Option          |           Value
 ------          |           -----
 Next boot       |            NONE
 Boot flag       |         Invalid
 Persistent      |           False
`

// fixtureBootOptionPersistent is the OBSERVED state after `bootoption 14 1`
// (validated live): a persistent armed PXE override.
const fixtureBootOptionPersistent = ` Option          |           Value
 ------          |           -----
 Next boot       |        UEFI_PXE
 Boot flag       |           Valid
 Persistent      |            True
`

func TestParseBootOptionFlags(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantValid   bool
		wantPersist bool
	}{
		{"armed one-time", fixtureBootOptionPXE, true, false},
		{"armed persistent", fixtureBootOptionPersistent, true, true},
		{"cleared / invalid", fixtureBootOptionCleared, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bo, err := ParseBootOption(tc.in)
			if err != nil {
				t.Fatalf("ParseBootOption: %v", err)
			}
			if bo.Valid != tc.wantValid {
				t.Errorf("Valid = %v, want %v", bo.Valid, tc.wantValid)
			}
			if bo.Persistent != tc.wantPersist {
				t.Errorf("Persistent = %v, want %v", bo.Persistent, tc.wantPersist)
			}
		})
	}
}

func TestParseBootOptionMissing(t *testing.T) {
	_, err := ParseBootOption("garbage banner\n")
	if err == nil {
		t.Fatal("expected error when Next boot line absent")
	}
}

// --- exit-code classification ---

func TestClassifyExit(t *testing.T) {
	// Exit codes OBSERVED on live hardware: 0 success, 145 connect-fail,
	// 147 bad-args/usage, 182 no-args usage. A nonzero exit means the
	// invocation did not succeed regardless of stdout content; the wrapper
	// must not treat a parseable-looking line on a failed exit as success.
	cases := []struct {
		code    int
		wantErr bool
	}{
		{0, false},
		{145, true},
		{147, true},
		{182, true},
		{1, true},
	}
	for _, tc := range cases {
		err := classifyExit(tc.code, "")
		if (err != nil) != tc.wantErr {
			t.Fatalf("classifyExit(%d) err=%v, wantErr=%v", tc.code, err, tc.wantErr)
		}
	}
}

func TestClassifyExitConnectFailureMessage(t *testing.T) {
	// The connect-failure stdout ("Can't connect to <ip>") should surface
	// in the error so operators see why, not just a bare exit code.
	err := classifyExit(145, "Can't connect to 192.0.2.254\n")
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "Can't connect") {
		t.Fatalf("error %q should include the tool's stderr/stdout reason", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
