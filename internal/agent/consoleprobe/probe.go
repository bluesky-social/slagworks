// Package consoleprobe implements the console verification probe
// and its determination-sweep counterpart. Both emit marker bytes to a
// serial tty so they traverse the BMC SOL session up to global's
// console_cast_events table, letting slagworks confirm (or discover)
// which device the BMC SOL is actually wired to. Depends only on
// agent.Context, agent.HTTPGet, and internal/model.
package consoleprobe

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/model"
)

// Probe implements the console probe: at commission, the agent
// writes a unique marker to the tty named in kernel_params, at the
// configured baud. Those bytes traverse the BMC SOL session up to
// global's console_cast_events table. The contract being asserted: "the
// device the operator configured in NetBox is in fact the device whose
// SOL the BMC relays."
//
// Failure mode this catches: the device-type custom field claims
// console=ttyS1, but the BMC's SOL channel is wired to ttyS0 (or
// disabled, or set to a different baud) — so a future deploy would
// land its kernel output where the operator can't see it. We'd
// rather hard-fail commission than discover this at deploy time.
//
// The probe is fire-and-forget on the agent side: emit the marker,
// log the nonce, return. Operator-facing verification (matching the
// nonce against console_cast_events) is a separate concern. The
// agent shouldn't block commission on the verification round-trip
// because the agent
// runs IN the SOL stream it's probing; a verification RPC would
// itself ride the WS connection that drives the SOL pump, which
// hasn't observed the agent's bytes yet.
//
// Sentinel "none" in kernel_params skips probing (e.g. switches
// without SOL).
func Probe(ctx *agent.Context, machineID string) error {
	kpURL := fmt.Sprintf("http://%s/api/v1/machines/%s/kernel-params", ctx.Server, machineID)
	kpBytes, err := agent.HTTPGet(kpURL)
	if err != nil {
		return fmt.Errorf("fetch kernel-params for probe: %w", err)
	}
	kp := strings.TrimSpace(string(kpBytes))
	if kp == "" {
		// New-hardware fallthrough: no NetBox device-type yet, no
		// probe possible. Commission can still complete; deploy
		// will refuse later via the empty-KernelParams gate in
		// the bootloader install.
		ctx.Log.Info("probe skipped (no kernel_params configured)")
		return nil
	}
	if kp == "none" {
		ctx.Log.Info("probe skipped (kernel_params=none, device has no SOL)")
		return nil
	}

	tty, baud, err := parseConsoleParams(kp)
	if err != nil {
		return fmt.Errorf("parse kernel_params %q: %w", kp, err)
	}

	nonce, err := randomNonce()
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	marker := fmt.Sprintf("SLAG-CONSOLE-PROBE-%s-%s\n", machineID, nonce)

	if err := emitProbeMarker(ctx, tty, baud, marker); err != nil {
		return err
	}
	ctx.Log.Info("probe emitted", "dev", "/dev/"+tty, "baud", baud, "nonce", nonce)
	return nil
}

// Sweep is the determination counterpart to Probe: when the device-type
// has no slagworks_kernel_params (typically a type commission itself
// just minted), we don't know which tty the BMC SOL is wired to — so
// probe ALL candidates with a distinct nonce each. The boot handler
// boots this commission with a broadcast console cmdline covering the
// same candidate set, and global resolves which nonce actually arrived
// in console_cast_events, then writes the winning console= back to the
// device-type custom field.
//
// Best-effort by design: a candidate tty that doesn't exist or can't
// be written is skipped (its nonce simply never lands, which is
// itself the signal), and a sweep with zero emitted candidates is not
// an error — the device may have no serial hardware at all, in which
// case the operator sets "none".
func Sweep(ctx *agent.Context, machineID string) *model.ConsoleSweep {
	sweep := &model.ConsoleSweep{}
	for _, tty := range model.ConsoleSweepTTYs {
		nonce, err := randomNonce()
		if err != nil {
			ctx.Log.Warn("sweep: nonce generation failed, skipping tty", "tty", tty, "err", err)
			continue
		}
		marker := fmt.Sprintf("SLAG-CONSOLE-PROBE-%s-%s\n", machineID, nonce)
		if err := emitProbeMarker(ctx, tty, model.ConsoleSweepBaud, marker); err != nil {
			ctx.Log.Info("sweep: tty not probeable, skipping", "tty", tty, "err", err)
			continue
		}
		ctx.Log.Info("sweep probe emitted", "dev", "/dev/"+tty, "baud", model.ConsoleSweepBaud, "nonce", nonce)
		sweep.Candidates = append(sweep.Candidates, model.ConsoleSweepCandidate{
			TTY: tty, Baud: model.ConsoleSweepBaud, Nonce: nonce,
		})
	}
	return sweep
}

// emitProbeMarker writes the marker to /dev/<tty> at the given baud —
// the emission half shared by the verification probe and the
// determination sweep.
func emitProbeMarker(ctx *agent.Context, tty string, baud int, marker string) error {
	devPath := "/dev/" + tty
	// O_NOCTTY: don't make this serial port the controlling terminal
	// for the agent process. O_NONBLOCK: avoid blocking forever if
	// the port is disconnected or DCD/CTS isn't asserted (a stalled
	// commission probe would wedge the deploy lifecycle indefinitely;
	// failing fast is the right call).
	f, err := os.OpenFile(devPath, os.O_WRONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", devPath, err)
	}
	defer f.Close()
	// O_NONBLOCK alone isn't enough: Go registers ttys with its
	// netpoller, so f.Write blocks the goroutine until the fd is
	// writable, regardless of the open flag. Pin a deadline so a
	// stuck SOL pipe can't wedge commission.
	if err := f.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		ctx.Log.Warn("probe: SetWriteDeadline failed (continuing)", "dev", devPath, "err", err)
	}

	// Configure baud via stty: the kernel keeps the previous tty
	// settings if we don't, and a mismatch between kernel_params'
	// declared baud and what the kernel currently has set means the
	// bytes garble through the BMC even when everything else is
	// correct. Failure here is informative but not fatal — the
	// kernel's default for ttySN is usually 115200, which matches
	// what we ship in production.
	if err := ctx.Exec.Run("stty", "-F", devPath, strconv.Itoa(baud)); err != nil {
		ctx.Log.Warn("probe: stty failed (continuing with kernel default)", "dev", devPath, "baud", baud, "err", err)
	}

	// Multiple writes guard against terminal line discipline buffering
	// or character loss on the BMC ↔ tty bridge. The CR/LF combo
	// matches what dmesg-style serial bursts look like.
	for range 5 {
		if _, werr := f.Write([]byte(marker)); werr != nil {
			return fmt.Errorf("write probe marker: %w", werr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// consoleParamRE matches a `console=ttyS1,115200n8` token. Captures
// the tty name (`ttyS1`) and the baud (`115200`). The trailing
// parity/bits/etc segment is ignored — we don't tune those, the
// kernel/stty defaults match what production hardware uses.
var consoleParamRE = regexp.MustCompile(`^console=(tty[A-Za-z0-9]+)(?:,(\d+))?`)

// parseConsoleParams pulls the tty and baud out of a kernel_params
// string like `console=ttyS1,115200n8` (with possible additional
// space-separated tokens we ignore). Returns an error when no
// console= directive is present or the tty name doesn't parse.
//
// CRITICAL: returns the LAST matching console= directive, not the
// first. The Linux kernel maps /dev/console to the last `console=`
// token (per init/main.c console_on_rootfs), and the BMC SOL is
// always wired to whatever winds up as /dev/console. A first-match
// implementation would probe `console=tty0` (VGA) and never hit the
// BMC SOL stream — which is exactly the bug class the probe exists
// to catch.
func parseConsoleParams(kp string) (tty string, baud int, err error) {
	found := false
	for tok := range strings.FieldsSeq(kp) {
		m := consoleParamRE.FindStringSubmatch(tok)
		if m == nil {
			continue
		}
		tty = m[1]
		baud = 115200 // default if the params don't specify
		if m[2] != "" {
			b, perr := strconv.Atoi(m[2])
			if perr == nil {
				baud = b
			}
		}
		found = true
	}
	if !found {
		return "", 0, fmt.Errorf("no console=tty... directive found")
	}
	return tty, baud, nil
}

func randomNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
