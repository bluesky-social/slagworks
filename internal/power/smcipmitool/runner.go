package smcipmitool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"
)

// DefaultBin is the launcher name looked up on PATH when the operator has
// not configured an explicit path (SLAGWORKS_SMCIPMITOOL_BIN). Slagworks
// does not ship SMCIPMITool; operators download it from Supermicro. See
// docs/user/hardware/supermicro-smcipmitool.md.
const DefaultBin = "SMCIPMITool"

// DefaultTimeout bounds a single invocation. SMCIPMITool spawns a JVM
// (hundreds of ms cold) plus a BMC round-trip; 30s matches the bmclib
// per-op default in BMCController so behavior is consistent across the two
// BMC paths.
const DefaultTimeout = 30 * time.Second

// killGrace is how long Run() waits after a context cancel before force
// killing the process tree and returning. Bounds the worst-case extra delay
// a hung/grandchild-pipe-holding invocation adds past its deadline.
const killGrace = 2 * time.Second

// Runner executes SMCIPMITool invocations. It is the only layer that touches
// the process; construction (command.go) and parsing (parse.go) are pure so
// they test without a binary. A Runner is safe to reuse across calls and
// across goroutines — it holds no per-call state.
type Runner struct {
	// Bin is the launcher path/name; "" → DefaultBin (resolved on PATH).
	Bin string
	// Timeout per invocation; 0 → DefaultTimeout.
	Timeout time.Duration
	// Logger; nil → slog.Default(). The runner logs the REDACTED invocation
	// only — the password never reaches a log line.
	Logger *slog.Logger
}

func (r *Runner) bin() string {
	if r.Bin == "" {
		return DefaultBin
	}
	return r.Bin
}

func (r *Runner) timeout() time.Duration {
	if r.Timeout == 0 {
		return DefaultTimeout
	}
	return r.Timeout
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// run executes one invocation and returns combined stdout+stderr. It applies
// the per-call timeout (unless the caller's context already has an earlier
// deadline), logs the redacted command, and maps a nonzero exit to an error
// via classifyExit.
//
// On success the output is returned RAW, for parsing: scrubbing it first
// would corrupt any value that happens to contain the password as a
// substring (a password of "on" breaks the power-state parse, a digit run
// shared with the serial breaks the identity check). Anything that can
// reach a log or error — the error paths here and parse errors in the
// callers (see scrubErr) — is scrubbed.
func (r *Runner) run(ctx context.Context, c Conn, sub ...string) (string, error) {
	// Always bound the call. context.WithTimeout returns the earlier of the
	// caller's deadline and r.timeout(), so a shorter caller deadline still
	// wins while a longer one (or none) is capped — we must NOT skip this
	// when the caller merely has *some* deadline, or a multi-hour parent
	// deadline would let an invocation hang far past DefaultTimeout.
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	args := c.argv(sub...)
	r.log().Debug("smcipmitool exec", "cmd", c.String(sub...), "host", c.Host)

	cmd := exec.CommandContext(ctx, r.bin(), args...)
	// SMCIPMITool is a JVM launched by a bash wrapper; on context kill the
	// wrapper dies but the JVM (a grandchild) can inherit our stdout pipe
	// and keep it open, which would block cmd.Run() on the pipe copy long
	// past the deadline. WaitDelay forces the process tree to be killed and
	// Run() to return shortly after cancellation regardless of lingering
	// pipe holders. (Go 1.20+.)
	cmd.WaitDelay = killGrace
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	// Scrub the password out of the output folded into any error.
	// SMCIPMITool takes the password positionally and echoes its command
	// line back on usage/bad-arg errors (exit 147/182), so the raw output
	// can contain the plaintext secret. This keeps the "never log the
	// password" rule true for the subprocess output, the way Redact keeps it
	// true for the argv we log.
	raw := buf.String()
	out := scrubSecret(raw, c.Pass)

	// A context timeout/cancel is the actionable cause ONLY when the call
	// actually failed. If the process already exited cleanly (runErr == nil)
	// and the context is cancelled in the race window right after, that is a
	// success — reporting a context error there would spuriously fail a
	// completed (and possibly non-idempotent, e.g. power cycle) operation.
	if runErr != nil && ctx.Err() != nil {
		return out, fmt.Errorf("smcipmitool %s: %w", c.String(sub...), ctx.Err())
	}

	code := 0
	if runErr != nil {
		if ee, ok := errors.AsType[*exec.ExitError](runErr); ok {
			code = ee.ExitCode()
		} else {
			// Couldn't even launch (binary missing, perm denied, etc.).
			return out, fmt.Errorf("smcipmitool %s: %w", c.String(sub...), runErr)
		}
	}
	if err := classifyExit(code, out); err != nil {
		return out, err
	}
	return raw, nil
}

// scrubErr scrubs the password from a parse error, whose message may quote
// the raw output run returned.
func scrubErr(err error, pass string) error {
	if err == nil {
		return nil
	}
	return errors.New(scrubSecret(err.Error(), pass))
}

// PowerStatus runs `ipmi power status` and parses the result.
func (r *Runner) PowerStatus(ctx context.Context, c Conn) (PowerState, error) {
	out, err := r.run(ctx, c, "ipmi", "power", "status")
	if err != nil {
		return PowerUnknown, err
	}
	st, err := ParsePowerStatus(out)
	return st, scrubErr(err, c.Pass)
}

// PowerOn runs `ipmi power up`.
func (r *Runner) PowerOn(ctx context.Context, c Conn) error {
	_, err := r.run(ctx, c, "ipmi", "power", "up")
	return err
}

// PowerOff runs `ipmi power down`.
func (r *Runner) PowerOff(ctx context.Context, c Conn) error {
	_, err := r.run(ctx, c, "ipmi", "power", "down")
	return err
}

// PowerCycle runs `ipmi power cycle`.
func (r *Runner) PowerCycle(ctx context.Context, c Conn) error {
	_, err := r.run(ctx, c, "ipmi", "power", "cycle")
	return err
}

// BootPXE sets a one-time UEFI PXE boot override.
func (r *Runner) BootPXE(ctx context.Context, c Conn) error {
	_, err := r.run(ctx, c, "ipmi", "power", "bootoption", BootIndexUEFIPXE, PersistNo)
	return err
}

// BootDisk sets a one-time UEFI hard-drive boot override.
func (r *Runner) BootDisk(ctx context.Context, c Conn) error {
	_, err := r.run(ctx, c, "ipmi", "power", "bootoption", BootIndexUEFIDisk, PersistNo)
	return err
}

// ChassisSerial runs `ipmi fru` and parses the chassis serial.
func (r *Runner) ChassisSerial(ctx context.Context, c Conn) (string, error) {
	out, err := r.run(ctx, c, "ipmi", "fru")
	if err != nil {
		return "", err
	}
	serial, err := ParseChassisSerial(out)
	return serial, scrubErr(err, c.Pass)
}

// BootOption runs the read-only `ipmi power bootoption` query and parses the
// current next-boot override (Next boot / Boot flag / Persistent). A
// transport/exit failure propagates as the error — callers must not treat a
// failed read as "no override armed".
func (r *Runner) BootOption(ctx context.Context, c Conn) (BootOption, error) {
	out, err := r.run(ctx, c, "ipmi", "power", "bootoption")
	if err != nil {
		return BootOption{}, err
	}
	bo, err := ParseBootOption(out)
	return bo, scrubErr(err, c.Pass)
}

// SetBootOption arms a boot-device override: `ipmi power bootoption <index>
// <persist>` where index is one of the bootIndex* constants (or "0" / NONE to
// clear) and persist is "0" (one-time) or "1" (all future boots). This is the
// generalized form behind BootPXE/BootDisk (which hardcode one-time) plus the
// operator-facing persistent + clear paths.
func (r *Runner) SetBootOption(ctx context.Context, c Conn, index, persist string) error {
	_, err := r.run(ctx, c, "ipmi", "power", "bootoption", index, persist)
	return err
}
