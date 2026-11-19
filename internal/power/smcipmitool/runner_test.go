package smcipmitool

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeTool writes a Bash script standing in for the SMCIPMITool binary. It
// echoes a canned stdout and exits with a canned code, and (when assertArgv
// is set) records its argv to a file so the test can verify the wrapper
// passed exactly the bytes we expect — including that the real password,
// not the redaction, reached the process. Mirrors the fakeIPMITool pattern
// in internal/bmcreconcile.
func fakeTool(t *testing.T, stdout string, exit int) (path, argvLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tool is a bash script; not for windows")
	}
	dir := t.TempDir()
	path = filepath.Join(dir, "SMCIPMITool")
	argvLog = filepath.Join(dir, "argv.log")
	// printf the stdout literally; record argv; exit with the code.
	script := "#!/usr/bin/env bash\n" +
		"printf '%s' \"$@\" > " + argvLog + "\n" +
		"cat <<'EOF'\n" + stdout + "\nEOF\n" +
		"exit " + itoa(exit) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return path, argvLog
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func testRunner(t *testing.T, stdout string, exit int) (*Runner, string) {
	path, argvLog := fakeTool(t, stdout, exit)
	r := &Runner{Bin: path, Timeout: 5 * time.Second}
	return r, argvLog
}

func TestRunnerPowerStatusOn(t *testing.T) {
	r, argvLog := testRunner(t, "Power is currently on.", 0)
	c := Conn{Host: "192.0.2.9", User: "ADMIN", Pass: "p@ss w/space"}

	state, err := r.PowerStatus(context.Background(), c)
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if state != PowerOn {
		t.Fatalf("state = %q, want on", state)
	}

	// The fake recorded its argv; confirm the wrapper passed the real
	// password (positional 3), proving Redact is a logging-only transform
	// and never reaches the actual exec.
	got, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if !strings.Contains(string(got), "p@ss w/space") {
		t.Fatalf("fake tool did not receive the real password; argv=%q", got)
	}
	if !strings.Contains(string(got), "ipmipowerstatus") {
		// printf '%s' joins args with no separator; the verb tokens are
		// concatenated. Just assert the verb made it through.
		t.Fatalf("fake tool argv missing the power-status verb; argv=%q", got)
	}
}

func TestRunnerNonzeroExitIsError(t *testing.T) {
	// Connect-failure shape: nonzero exit + a reason on stdout. Even though
	// the stdout is not a power line, the error must carry the reason and
	// the call must fail rather than returning a bogus state.
	r, _ := testRunner(t, "Can't connect to 192.0.2.254", 145)
	c := Conn{Host: "192.0.2.254", User: "ADMIN", Pass: "x"}

	_, err := r.PowerStatus(context.Background(), c)
	if err == nil {
		t.Fatal("expected error on nonzero exit")
	}
	if !strings.Contains(err.Error(), "Can't connect") {
		t.Fatalf("error should carry the tool's reason, got: %v", err)
	}
}

func TestRunnerChassisSerial(t *testing.T) {
	r, _ := testRunner(t, fixtureFRU, 0)
	c := Conn{Host: "192.0.2.9", User: "ADMIN", Pass: "x"}

	serial, err := r.ChassisSerial(context.Background(), c)
	if err != nil {
		t.Fatalf("ChassisSerial: %v", err)
	}
	if serial != "S500001X0000001" {
		t.Fatalf("serial = %q", serial)
	}
}

func TestRunnerPowerActuationVerbs(t *testing.T) {
	// On/Off/Cycle have no meaningful stdout to parse; success is exit 0.
	// We assert the wrapper reports success and sent the right verb.
	cases := []struct {
		name string
		call func(*Runner, context.Context, Conn) error
		verb string
	}{
		{"on", (*Runner).PowerOn, "ipmipowerup"},
		{"off", (*Runner).PowerOff, "ipmipowerdown"},
		{"cycle", (*Runner).PowerCycle, "ipmipowercycle"},
		{"pxe", (*Runner).BootPXE, "ipmipowerbootoption140"},
		{"disk", (*Runner).BootDisk, "ipmipowerbootoption90"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, argvLog := testRunner(t, "", 0)
			c := Conn{Host: "h", User: "u", Pass: "pw"}
			if err := tc.call(r, context.Background(), c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got, _ := os.ReadFile(argvLog)
			if !strings.Contains(string(got), tc.verb) {
				t.Fatalf("argv %q missing verb %q", got, tc.verb)
			}
		})
	}
}

func TestRunnerScrubsPasswordFromOutput(t *testing.T) {
	// Regression: if the tool echoes its argv on a usage/bad-arg error, the
	// positional password is in the captured output. The runner must scrub
	// it before that output reaches the caller / the error string.
	const pw = "s3cr3t-PASS"
	// Fake echoes the password back (as SMCIPMITool does on usage errors)
	// and exits nonzero so the output is folded into an error.
	r, _ := testRunner(t, "Usage: SMCIPMITool 10.0.0.1 ADMIN "+pw+" ipmi power status", 147)
	c := Conn{Host: "10.0.0.1", User: "ADMIN", Pass: pw}

	_, err := r.PowerStatus(context.Background(), c)
	if err == nil {
		t.Fatal("expected error on exit 147")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("password leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), redactedSecret) {
		t.Fatalf("expected redaction placeholder in scrubbed error: %v", err)
	}
}

func TestRunnerParsesRawOutputWhenPasswordIsASubstring(t *testing.T) {
	// The password is scrubbed only from errors, never from the output a
	// successful call parses: a password that happens to appear in normal
	// output (here "on", and a digit run from the serial) must not corrupt
	// the parsed power state or chassis serial.
	r, _ := testRunner(t, "Power is currently on.", 0)
	state, err := r.PowerStatus(context.Background(), Conn{Host: "h", User: "u", Pass: "on"})
	if err != nil || state != PowerOn {
		t.Fatalf("PowerStatus = %v, %v; want PowerOn, nil", state, err)
	}

	r, _ = testRunner(t, "  Product Serial Number (PS)     = S500001X0000001", 0)
	serial, err := r.ChassisSerial(context.Background(), Conn{Host: "h", User: "u", Pass: "00001"})
	if err != nil || serial != "S500001X0000001" {
		t.Fatalf("ChassisSerial = %q, %v; want S500001X0000001, nil", serial, err)
	}
}

func TestRunnerScrubsPasswordFromParseErrors(t *testing.T) {
	// A parse error quotes the raw output; it must not carry the password.
	const pw = "s3cr3t-PASS"
	r, _ := testRunner(t, "unexpected banner "+pw, 0)
	_, err := r.PowerStatus(context.Background(), Conn{Host: "h", User: "u", Pass: pw})
	if err == nil {
		t.Fatal("expected a parse error")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("password leaked into parse error: %v", err)
	}
}

func TestRunnerScrubsPasswordBeforeQuotingParseError(t *testing.T) {
	// The power-status parse error renders the output it rejected: it trims
	// the output and quotes it with %q. Both change the password's bytes (%q
	// escapes `"`, `\` and control characters; the trim drops whitespace at
	// the ends), so the password has to be scrubbed from the output before
	// it is rendered. Scrubbing the finished message misses it.
	//
	// Each password ends in the same distinctive tail, which both renderings
	// leave intact: if the tail is in the error, so is the password.
	const (
		tail   = "-7Kq"
		prefix = "Usage: SMCIPMITool 10.0.0.5 ADMIN "
	)
	cases := []struct {
		name   string
		pass   string
		suffix string // banner text after the echoed password
	}{
		{"double quote, escaped by %q", `pw"quote` + tail, " ipmi power status"},
		{"backslash, escaped by %q", `pw\slash` + tail, " ipmi power status"},
		{"tab, escaped by %q", "pw\ttab" + tail, " ipmi power status"},
		{"trailing newline, trimmed", "pw-newline" + tail + "\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The fake tool exits 0 with a usage banner that echoes the
			// connection arguments, password included, and that the
			// power-state parser does not recognize.
			r, _ := testRunner(t, prefix+tc.pass+tc.suffix, 0)
			_, err := r.PowerStatus(context.Background(), Conn{Host: "10.0.0.5", User: "ADMIN", Pass: tc.pass})
			if err == nil {
				t.Fatal("an unrecognized banner must be an error")
			}
			if strings.Contains(err.Error(), tail) {
				t.Fatalf("password leaked into parse error: %v", err)
			}
			// The error still quotes the output, with the password redacted,
			// so the operator can see what the tool printed.
			if !strings.Contains(err.Error(), prefix+redactedSecret) {
				t.Fatalf("parse error should quote the scrubbed output: %v", err)
			}
		})
	}
}

// Note: the "process succeeds, then ctx cancels in the race window" case
// (runErr == nil && ctx.Err() != nil) from the runner's guard is not
// directly unit-tested — a pre-cancelled context makes exec.Start() fail
// (runErr != nil), so it can't reproduce that specific window
// deterministically without internal hooks. The fix (gating the ctx-error
// return on runErr != nil) is correct by construction; forcing a flaky
// timing test would be worse than relying on inspection here.

func TestRunnerContextTimeout(t *testing.T) {
	// A tool that hangs must be killed by the context, not block forever.
	dir := t.TempDir()
	path := filepath.Join(dir, "SMCIPMITool")
	script := "#!/usr/bin/env bash\nsleep 30\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Bin: path, Timeout: 200 * time.Millisecond}
	c := Conn{Host: "h", User: "u", Pass: "pw"}

	start := time.Now()
	_, err := r.PowerStatus(context.Background(), c)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("runner did not honor timeout; took %v", elapsed)
	}
}
