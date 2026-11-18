package target

import (
	"strings"
	"testing"
)

// TestGenerateDataMarkerUnit pins the shape of the TEST-ONLY preserve-data
// marker unit: it echoes one grep-able PRESERVE_MARKER line (carrying this
// deploy's install tag plus the surviving content, source device, and mdstat
// names), writes the token if-absent (so a recreate lands this deploy's token
// and the negative control isn't vacuous), and only touches /data behind a
// mount check (so a failed mount can't fake a marker on the root fs).
func TestGenerateDataMarkerUnit(t *testing.T) {
	unit := GenerateDataMarkerUnit()

	for _, sect := range []string{"[Unit]", "[Service]", "[Install]"} {
		if !strings.Contains(unit, sect) {
			t.Fatalf("missing %s section:\n%s", sect, unit)
		}
	}
	// Runs on every boot (oneshot, no ConditionPathExists) and after /data
	// mounts.
	if !strings.Contains(unit, "Type=oneshot") {
		t.Fatalf("must be oneshot:\n%s", unit)
	}
	if strings.Contains(unit, "ConditionPathExists") {
		t.Fatalf("must run on every boot (no ConditionPathExists):\n%s", unit)
	}
	// Ordering-only after local-fs; NOT RequiresMountsFor (a hard Requires on
	// the /data mount would skip the whole unit on a mount failure, killing the
	// NOT-MOUNTED diagnostic branch).
	if !strings.Contains(unit, "After=local-fs.target") {
		t.Fatalf("must order after local-fs.target:\n%s", unit)
	}
	if strings.Contains(unit, "RequiresMountsFor") {
		t.Fatalf("must NOT use RequiresMountsFor (a failed /data mount would skip the diagnostic):\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Fatalf("must be enabled at multi-user.target:\n%s", unit)
	}

	// ExecStart runs the marker script FILE, not an inline sh -c payload — so
	// no shell quoting has to survive systemd's ExecStart parser (which mangles
	// the '\'' idiom, the bug that wrote /usr/bin/bash into the marker).
	if !strings.Contains(unit, "ExecStart=/bin/sh "+dataMarkerScriptPath) {
		t.Fatalf("ExecStart must exec the marker script file:\n%s", unit)
	}

	// The content checks inspect the script program directly.
	script := dataMarkerScript("tok-abc123")
	// The script is a real shell file with a shebang, run by /bin/sh.
	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Fatalf("marker script must start with a shebang:\n%s", script)
	}

	// One echoed line carrying every field the qemu test greps, tagged with
	// this deploy's install token (shell-quoted via adjacent-quote
	// concatenation "install="'tok'" so the rendered console line reads
	// `install=tok-abc123`).
	for _, field := range []string{
		`PRESERVE_MARKER install="'tok-abc123'"`,
		"content=$content",
		"source=$src",
		"mdstat=[$mds]",
	} {
		if !strings.Contains(script, field) {
			t.Fatalf("missing echo field %q:\n%s", field, script)
		}
	}
	// The echo reaches the deployed-OS serial console.
	if !strings.Contains(script, "/dev/console") {
		t.Fatalf("marker line must be echoed to /dev/console:\n%s", script)
	}
	// The token is written into the marker file, if-absent, and shell-quoted
	// for the inner parse (shellSingleQuote of a quote-free token yields
	// '<token>').
	if !strings.Contains(script, "printf '%s' 'tok-abc123' > /data/PRESERVE_MARKER") {
		t.Fatalf("token must be shell-quoted in the write:\n%s", script)
	}
	if !strings.Contains(script, "[ -f /data/PRESERVE_MARKER ] ||") {
		t.Fatalf("write must be guarded if-absent so a recreate lands this deploy's token:\n%s", script)
	}
	// The write only happens when /data is a real mountpoint.
	if !strings.Contains(script, "findmnt -no SOURCE /data") {
		t.Fatalf("write must be gated on /data being mounted:\n%s", script)
	}
	if !strings.Contains(script, "NOT-MOUNTED") {
		t.Fatalf("not-mounted path must report NOT-MOUNTED so the test never reads a rootfs marker:\n%s", script)
	}
}

// TestGenerateDataMarkerUnit_TokenQuotingSafe is a regression guard on the
// shell quoting: a token containing a single quote must NOT break out of the
// `printf '%s' <tok>` context and inject commands. Tokens are test-controlled
// today, but the quoting must be correct regardless.
//
// The script is a single shell layer (a file exec'd by /bin/sh — systemd never
// parses its quotes), so the printf operand for the evil token must be exactly
// the shellSingleQuote encoding — one literal argument — with no live
// `; rm -rf /` between two live quotes.
func TestGenerateDataMarkerUnit_TokenQuotingSafe(t *testing.T) {
	evil := `x'; rm -rf /; echo '`
	script := dataMarkerScript(evil)

	// The printf write operand must be the EXACT shellSingleQuote encoding of
	// the evil token — one literal argument, every embedded quote neutralized
	// as '\''. Exact-match is the sound proof: a substring heuristic for the
	// injection sequence false-positives because the token's own bytes
	// (`; rm -rf /; echo `) legitimately appear between the escaped quotes in
	// the SAFE encoding too.
	wantWriteOperand := "printf '%s' " + shellSingleQuote(evil) + " > /data/PRESERVE_MARKER"
	if !strings.Contains(script, wantWriteOperand) {
		t.Fatalf("evil token not safely shell-quoted in the printf write operand:\nwant substring: %s\ngot:\n%s", wantWriteOperand, script)
	}
	// The echo's install= tag must likewise be the exact shellSingleQuote
	// encoding, concatenated between the double-quoted echo segments.
	wantEchoTag := `install="` + shellSingleQuote(evil) + `" content=`
	if !strings.Contains(script, wantEchoTag) {
		t.Fatalf("evil token not safely shell-quoted in the echo tag:\nwant substring: %s\ngot:\n%s", wantEchoTag, script)
	}
}
