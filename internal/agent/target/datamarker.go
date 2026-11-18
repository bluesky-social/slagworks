package target

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// dataMarkerScriptPath is where the marker shell program is written in the
// target. The unit execs this FILE rather than an inline `sh -c` payload:
// systemd's ExecStart parser has its own quote-removal rules and mangles the
// shell `'\”` single-quote-escaping idiom, so a script with embedded single
// quotes (ours has printf '%s', awk '...', and the quoted token) is corrupted
// when passed inline — the bug that wrote /usr/bin/bash into the marker file.
// A file sidesteps systemd's parser entirely — the token is shell-quoted
// exactly once, for the real shell that runs the file.
const dataMarkerScriptPath = "/usr/local/lib/slagworks-data-marker.sh"

// GenerateDataMarkerUnit renders a TEST-ONLY first-boot systemd unit that
// proves a preserved /data array survives a redeploy. It is installed ONLY
// when slagworks.data_marker=<token> is on the kernel cmdline (the
// preserve-data qemu test sets it via the deploy op's params.data_marker);
// production deploys never carry the token, so this unit is never written on
// a real host. ExecStart runs the on-disk script file (dataMarkerScriptPath),
// not an inline payload, so no shell quoting has to survive systemd's parser.
//
// On every boot the script (see dataMarkerScript):
//
//  1. Writes <token> into /data/PRESERVE_MARKER ONLY if the file is absent,
//     and ONLY when /data is a real mountpoint. The write is deliberately
//     "if-absent" and the token is unique per deploy: a broken preserve that
//     recreates (empty) /data therefore lands THIS deploy's token, not the
//     one an earlier deploy wrote, so a test comparing the surviving content
//     against the earlier token detects the destruction instead of seeing it
//     silently rewritten. The mountpoint guard stops a failed /data mount
//     from faking a marker on the root fs.
//
//  2. Echoes ONE grep-able line to /dev/console (= the deployed OS's serial
//     console, which the qemutest SerialMonitor scans):
//
//     PRESERVE_MARKER install=<token> content=<file> source=<dev> mdstat=[<md names>]
//
//     `install=<token>` is this deploy's unique tag, so the test can wait for
//     THIS boot's line (not a stale one still buffered from an earlier boot).
//     `content=` is the surviving marker (== an earlier deploy's token iff the
//     data survived), and `source=`/`mdstat=` prove boot-time reassembly+
//     remount, not just byte survival. All fields on one line so they can
//     never be split across a serial race.
//
// Type=oneshot with no ConditionPathExists so it runs on EVERY boot (the
// deploy-#2 cold boot is where the survival assertion lands).
//
// Ordering is `After=local-fs.target` ONLY — deliberately NOT
// RequiresMountsFor=/data. RequiresMountsFor injects a hard `Requires=`
// on the /data mount unit, so a FAILED /data mount would skip this service
// entirely and the NOT-MOUNTED diagnostic branch (which exists precisely so
// a mount failure surfaces as a loud console line, not an 18-minute test
// timeout) would never run. After=local-fs.target is ordering-only: the unit
// still runs after the fstab mount was ATTEMPTED, whether it succeeded or
// failed, so the diagnostic always fires.
func GenerateDataMarkerUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Slagworks preserve-data test marker (TEST-ONLY)
After=local-fs.target

[Service]
Type=oneshot
RemainAfterExit=no
ExecStart=/bin/sh %s

[Install]
WantedBy=multi-user.target
`, dataMarkerScriptPath)
}

// dataMarkerScript builds the shell program written to dataMarkerScriptPath and
// run by the unit. It is a SINGLE shell layer (the file is exec'd directly by
// /bin/sh), so the token is shell-single-quoted exactly once here; without that
// a token containing a single quote would break out of the printf operand.
// Tokens are test-controlled today ([A-Za-z0-9-]), but quote defensively.
//
// The write is followed by a whole-filesystem `sync` BEFORE the echo, and the
// normal echo only fires if that sync succeeds (a failing sync instead prints
// PRESERVE_MARKER_SYNC_FAILED and exits non-zero). The two-VM qemu test hard-
// quits the writing VM shortly after it sees the normal echo on serial (so a
// second VM can open the same data qcow2 with the array's filesystem already
// flushed); if the echo fired unconditionally, a failing sync would still
// look like a durability proof to the test even though the write could still
// be sitting in the page cache when the VM is killed.
func dataMarkerScript(token string) string {
	qTok := shellSingleQuote(token)
	// Write only after confirming /data is mounted; always echo one line so the
	// test sees state even on the not-mounted (failure) path. The token is
	// shell-single-quoted (qTok) at BOTH interpolation points: the printf write
	// operand, and the echo's install= tag (via adjacent-quote concatenation
	// "install="'<tok>'" ..." so the single-quoted token sits between the
	// double-quoted echo segments).
	return fmt.Sprintf("#!/bin/sh\n"+
		`if findmnt -no SOURCE /data >/dev/null 2>&1; then `+
		`[ -f /data/PRESERVE_MARKER ] || printf '%%s' %s > /data/PRESERVE_MARKER; `+
		`src=$(findmnt -no SOURCE /data); `+
		`content=$(cat /data/PRESERVE_MARKER 2>/dev/null); `+
		`else src=NOT-MOUNTED; content=NO-DATA-MOUNT; fi; `+
		`mds=$(awk 'NR>1 && $1 ~ /^md/ {printf "%%s ", $1}' /proc/mdstat); `+
		`sync || { echo "PRESERVE_MARKER_SYNC_FAILED" > /dev/console; exit 1; }; `+
		`echo "PRESERVE_MARKER install="%s" content=$content source=$src mdstat=[$mds]" > /dev/console`+"\n",
		qTok, qTok)
}

// WriteDataMarkerService installs and enables the TEST-ONLY data-marker unit
// in the target. Called from the deploy path only when the agent sees a
// slagworks.data_marker= token on the kernel cmdline. A no-op on any
// production deploy (no token => never called).
func WriteDataMarkerService(ctx *agent.Context, token string) error {
	ctx.Log.Info("writing preserve-data test marker service (slagworks.data_marker set)")

	// The marker program lives in a file the unit execs, so no shell quoting
	// has to survive systemd's ExecStart parser (which mangles the '\''
	// idiom). 0755 so /bin/sh can read+exec it.
	scriptPath := "/target" + dataMarkerScriptPath
	if err := os.MkdirAll(filepath.Dir(scriptPath), 0755); err != nil {
		return fmt.Errorf("mkdir data-marker script dir: %w", err)
	}
	if err := os.WriteFile(scriptPath, []byte(dataMarkerScript(token)), 0755); err != nil {
		return fmt.Errorf("write data-marker script: %w", err)
	}

	unitPath := "/target/etc/systemd/system/slagworks-data-marker.service"
	if err := os.WriteFile(unitPath, []byte(GenerateDataMarkerUnit()), 0644); err != nil {
		return fmt.Errorf("write data-marker unit: %w", err)
	}

	wantsDir := "/target/etc/systemd/system/multi-user.target.wants"
	if err := os.MkdirAll(wantsDir, 0755); err != nil {
		return fmt.Errorf("mkdir wants dir: %w", err)
	}
	link := wantsDir + "/slagworks-data-marker.service"
	_ = os.Remove(link)
	if err := os.Symlink("/etc/systemd/system/slagworks-data-marker.service", link); err != nil {
		return fmt.Errorf("enable data-marker unit: %w", err)
	}

	ctx.Log.Info("preserve-data test marker service installed")
	return nil
}
