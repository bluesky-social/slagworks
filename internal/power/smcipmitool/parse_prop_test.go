package smcipmitool

// Property tests for the SMCIPMITool output parsers and the runner's
// password scrubbing. Outputs are rendered from a structured model with
// generated padding, ordering, noise lines and line endings; the oracle is
// the model value, not a re-parse.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pgregory.net/rapid"
)

var propSerialGen = rapid.StringMatching(`[A-Z0-9][A-Z0-9-]{0,18}`)

func propNoiseLine(t *rapid.T, label string) string {
	return rapid.SampledFrom([]string{
		"Getting FRU ...",
		"Picked up JAVA_TOOL_OPTIONS: -Xmx64m",
		"WARNING: An illegal reflective access operation has occurred",
		"",
		"   ",
	}).Draw(t, label)
}

func propEOL(t *rapid.T) string {
	return rapid.SampledFrom([]string{"\n", "\r\n"}).Draw(t, "eol")
}

func TestProp_ParseChassisSerial(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ps := propSerialGen.Draw(t, "ps")
		cs := propSerialGen.Draw(t, "cs")
		bs := propSerialGen.Draw(t, "bs")
		type row struct{ label, val string }
		rows := []row{
			{"Chassis Type (CT)", "Other (01h)"},
			{"Chassis Serial Number (CS)", cs},
			{"Board Serial Number (BS)", bs},
			{"Board Product Name (BP)", "H13SSH"},
			{"Product Name (PN)", "AS -1115HS-TNR"},
			{"Product Part Number (PP)", "AS-1115HS-TNR"},
			{"Product Serial Number (PS)", ps},
		}
		// Shuffle deterministically through draws.
		for i := len(rows) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("swap%d", i))
			rows[i], rows[j] = rows[j], rows[i]
		}
		nl := propEOL(t)
		var b strings.Builder
		b.WriteString(propNoiseLine(t, "lead") + nl)
		for i, r := range rows {
			pad := rapid.IntRange(1, 12).Draw(t, fmt.Sprintf("pad%d", i))
			fmt.Fprintf(&b, "%s%s= %s%s", r.label, strings.Repeat(" ", pad), r.val, nl)
			if rapid.IntRange(0, 4).Draw(t, fmt.Sprintf("noise%d", i)) == 0 {
				b.WriteString(propNoiseLine(t, fmt.Sprintf("n%d", i)) + nl)
			}
		}
		got, err := ParseChassisSerial(b.String())
		if err != nil {
			t.Fatalf("ParseChassisSerial: %v\n%q", err, b.String())
		}
		if got != ps {
			t.Fatalf("got %q, want product serial %q (CS %q, BS %q)\n%q", got, ps, cs, bs, b.String())
		}
	})
}

func TestProp_ParsePowerStatus(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		on := rapid.Bool().Draw(t, "on")
		word := "off"
		if on {
			word = "on"
		}
		nl := propEOL(t)
		out := propNoiseLine(t, "pre") + nl + "Power is currently " + word + "." + nl + propNoiseLine(t, "post") + nl
		st, err := ParsePowerStatus(out)
		if err != nil {
			t.Fatalf("ParsePowerStatus(%q): %v", out, err)
		}
		want := PowerOff
		if on {
			want = PowerOn
		}
		if st != want {
			t.Fatalf("ParsePowerStatus(%q) = %q, want %q", out, st, want)
		}
	})
}

func TestProp_ParseBootOption(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		next := rapid.SampledFrom([]string{"UEFI_PXE", "UEFI_Hard-drive", "NONE", "UEFI_USB_CD/DVD"}).Draw(t, "next")
		persistent := rapid.Bool().Draw(t, "persistent")
		valid := rapid.Bool().Draw(t, "valid")
		flag := "Invalid"
		if valid {
			flag = "Valid"
		}
		rows := [][2]string{
			{"Next boot", next},
			{"Persistent", fmt.Sprint(persistent)},
			{"Boot flag", flag},
			{"BIOS boot type", "UEFI"},
		}
		for i := len(rows) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("swap%d", i))
			rows[i], rows[j] = rows[j], rows[i]
		}
		nl := propEOL(t)
		var b strings.Builder
		b.WriteString("Option          | Value" + nl + "----------------|------------------" + nl)
		for i, r := range rows {
			fmt.Fprintf(&b, "%-*s| %s%s", rapid.IntRange(len(r[0])+1, 20).Draw(t, fmt.Sprintf("w%d", i)), r[0], r[1], nl)
		}
		bo, err := ParseBootOption(b.String())
		if err != nil {
			t.Fatalf("ParseBootOption: %v\n%q", err, b.String())
		}
		if bo.NextBoot != next || bo.Persistent != persistent || bo.Valid != valid {
			t.Fatalf("got %+v, want next=%q persistent=%v valid=%v\n%q", bo, next, persistent, valid, b.String())
		}
	})
}

// Arbitrary input must never panic, and an unrecognized power banner must
// never read as a state.
func TestProp_ParsersArbitraryInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "s")
		st, err := ParsePowerStatus(s)
		low := strings.ToLower(s)
		if err == nil && !strings.Contains(low, "power is currently on") && !strings.Contains(low, "power is currently off") {
			t.Fatalf("ParsePowerStatus(%q) = %q with no recognizable banner", s, st)
		}
		_, _ = ParseChassisSerial(s)
		_, _ = ParseBootOption(s)
	})
}

// TestProp_RunnerNeverLeaksPassword: SMCIPMITool echoes its command line on
// usage errors; whatever the tool prints and however it exits, no error from
// a Runner method contains the password.
func TestProp_RunnerNeverLeaksPassword(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "SMCIPMITool")
	// Prints its argv in several shapes to stdout and stderr, then exits
	// with the code in $FAKE_EXIT; for exit 0 prints $FAKE_OUT instead.
	script := "#!/usr/bin/env bash\n" +
		"if [ \"${FAKE_EXIT}\" = 0 ]; then printf '%s' \"$FAKE_OUT\"; exit 0; fi\n" +
		"echo \"usage error: $*\"\n" +
		"echo \"password='$3'\" 1>&2\n" +
		"printf '%s\\n' \"$@\"\n" +
		"exit \"$FAKE_EXIT\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// t.Setenv restores both variables when the test ends; each case then
	// overwrites them for the fake.
	t.Setenv("FAKE_EXIT", "0")
	t.Setenv("FAKE_OUT", "")
	rapid.Check(t, func(rt *rapid.T) {
		// Quotes and backslashes matter: an error that %q-quotes the tool's
		// output escapes them, which hides the password from a scrub applied
		// afterwards.
		pass := rapid.StringMatching(`[A-Za-z0-9!@#$%^&*()_+={}\[\]:;'"\\<>,.?/~ -]{4,20}`).Draw(rt, "pass")
		exit := rapid.SampledFrom([]int{0, 1, 145, 147, 182}).Draw(rt, "exit")
		out := rapid.SampledFrom([]string{"garbage " + pass, "Power is currently maybe " + pass, "no such row"}).Draw(rt, "out")
		runner := &Runner{Bin: bin, Timeout: 5 * time.Second}
		c := Conn{Host: "192.0.2.5", User: "ADMIN", Pass: pass}
		if err := os.Setenv("FAKE_EXIT", fmt.Sprint(exit)); err != nil {
			rt.Fatal(err)
		}
		if err := os.Setenv("FAKE_OUT", out); err != nil {
			rt.Fatal(err)
		}
		var errs []error
		_, err := runner.PowerStatus(context.Background(), c)
		errs = append(errs, err)
		_, err = runner.ChassisSerial(context.Background(), c)
		errs = append(errs, err)
		_, err = runner.BootOption(context.Background(), c)
		errs = append(errs, err)
		errs = append(errs, runner.PowerCycle(context.Background(), c))
		quoted := strconv.Quote(pass)
		escaped := quoted[1 : len(quoted)-1]
		for _, e := range errs {
			if e != nil && (strings.Contains(e.Error(), pass) || strings.Contains(e.Error(), escaped)) {
				rt.Fatalf("error leaks password %q: %v", pass, e)
			}
		}
	})
}
