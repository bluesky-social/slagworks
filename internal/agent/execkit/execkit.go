// Package execkit is the slagworks-agent's exec plumbing: run a
// command, tee its output to the live log surfaces, and emit one
// structured completion envelope per exec. No slagworks semantics live
// here — domain-specific exec behavior (e.g. storageapply's
// blockdev --rereadpt retry) belongs in the subsystem packages.
package execkit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// outputCapBytes bounds per-exec stdout/stderr capture so a chatty
// command (rsync over a million files, dd showing a long progress
// stream) doesn't blow up the log envelope. 16 KiB per stream is
// enough to see what failed in the common case without paying for
// the worst case. When truncated, the envelope's "stdout"/"stderr"
// field includes a trailing "...[truncated, total N bytes]" marker.
const outputCapBytes = 16 << 10

// configScriptStderrCap bounds how much of a config script's stderr
// RunConfigScript keeps for its error and log lines. The text ends up in the
// operation result, so a runaway script must not bloat it; 8 KiB holds any
// realistic diagnostic many times over. The TAIL is kept, not the head:
// a script's decisive message (an ERROR line, a Python traceback's final
// exception) is the last thing it writes.
const configScriptStderrCap = 8 << 10

// Exec is the production Runner: it spawns real processes, tees their
// output live to Tee (falling back to the agent's stdout/stderr when
// nil), and logs a structured envelope per completed exec.
type Exec struct {
	Log *slog.Logger
	// Tee receives every stdout/stderr byte live, so long-running
	// commands stay visible mid-run. nil falls back to os.Stdout /
	// os.Stderr.
	Tee io.Writer
}

// Run executes a command and returns its error. The full exec outcome
// (argv, exit code, duration, bounded stdout/stderr) ships as one
// structured log envelope.
func (e *Exec) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	stdout := &boundedBuffer{cap: outputCapBytes}
	stderr := &boundedBuffer{cap: outputCapBytes}
	if e.Tee != nil {
		cmd.Stdout = io.MultiWriter(e.Tee, stdout)
		cmd.Stderr = io.MultiWriter(e.Tee, stderr)
	} else {
		cmd.Stdout = io.MultiWriter(os.Stdout, stdout)
		cmd.Stderr = io.MultiWriter(os.Stderr, stderr)
	}
	started := time.Now()
	err := cmd.Run()
	dur := time.Since(started)

	exitCode := 0
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			exitCode = exitErr.ExitCode()
		} else {
			// Couldn't even start (PATH missing, permission denied,
			// etc.). Render as -1 and let the operator read err in
			// the envelope.
			exitCode = -1
		}
	}

	// Argv as a single shell-pasteable string. We don't quote
	// individual tokens — operator readability over strict
	// reproducibility. Tokens with spaces (rare in our exec calls)
	// will look ambiguous; that's a known tradeoff worth flagging
	// only if it bites.
	argv := name
	if len(args) > 0 {
		argv = name + " " + strings.Join(args, " ")
	}

	// Emit a single completion envelope. "exec" stays as the message
	// for log-renderer compatibility; argv + exit_code + duration_ms
	// + truncated stdout/stderr ride in extra. Only attach stdout/
	// stderr when non-empty to keep envelopes tight.
	attrs := []any{
		"cmd", name,
		"args", args,
		"argv", argv,
		"exit_code", exitCode,
		"duration_ms", dur.Milliseconds(),
	}
	if s := stdout.String(); s != "" {
		attrs = append(attrs, "stdout", s)
	}
	if s := stderr.String(); s != "" {
		attrs = append(attrs, "stderr", s)
	}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
		e.Log.Warn("exec", attrs...)
	} else {
		e.Log.Info("exec", attrs...)
	}
	return err
}

// Output executes a command and returns its trimmed stdout.
func (e *Exec) Output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// Available reports whether the named command exists in $PATH.
func (e *Exec) Available(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// boundedBuffer is a write target that retains at most cap bytes of
// the start of the stream, plus a count of total bytes written.
// Useful for capturing exec output for the completion envelope
// without unbounded memory if a command spews megabytes.
type boundedBuffer struct {
	cap   int
	buf   []byte
	total int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	if remain := b.cap - len(b.buf); remain > 0 {
		take := min(len(p), remain)
		b.buf = append(b.buf, p[:take]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b.total <= len(b.buf) {
		return string(b.buf)
	}
	return string(b.buf) + fmt.Sprintf("...[truncated, total %d bytes]", b.total)
}

// tailBuffer is boundedBuffer's mirror image: it retains at most cap bytes
// of the END of the stream, plus a count of total bytes written. Used for
// config-script stderr, where the decisive message comes last.
type tailBuffer struct {
	cap   int
	buf   []byte
	total int
	// lastDropped is the most recent byte to fall out of the window, so
	// String can tell whether the retained text starts on a line boundary.
	lastDropped byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	drop := len(b.buf) + len(p) - b.cap
	if drop <= 0 {
		b.buf = append(b.buf, p...)
		return len(p), nil
	}
	if i := drop - 1; i < len(b.buf) {
		b.lastDropped = b.buf[i]
	} else {
		b.lastDropped = p[i-len(b.buf)]
	}
	if drop >= len(b.buf) {
		b.buf = append(b.buf[:0], p[drop-len(b.buf):]...)
	} else {
		b.buf = append(b.buf[:0], b.buf[drop:]...)
		b.buf = append(b.buf, p...)
	}
	return len(p), nil
}

// String returns the retained tail. When the head was dropped mid-line, that
// partial first line is discarded too so the text starts on a line boundary,
// unless it is the only line retained (one line longer than cap): then its
// end is kept, with any rune the cut split repaired. A leading marker reports
// how much was written in total.
func (b *tailBuffer) String() string {
	if b.total <= len(b.buf) {
		return string(b.buf)
	}
	tail := b.buf
	if b.lastDropped != '\n' {
		if i := bytes.IndexByte(tail, '\n'); i >= 0 && i < len(tail)-1 {
			tail = tail[i+1:]
		}
	}
	tail = bytes.ToValidUTF8(tail, []byte("\uFFFD"))
	return fmt.Sprintf("...[truncated, total %d bytes]\n", b.total) + string(tail)
}

// RunConfigScript runs a pluggable per-host config script in the deploy
// initrd and returns its stdout. It is the shared execution harness behind
// both pluggable-script seams:
//
//   - storage: envName=SLAGWORKS_INVENTORY, data=live inventory JSON,
//     stdout=storage.Layout JSON.
//   - network: envName=SLAGWORKS_NETWORK_FACTS, data=NetBox interface facts
//     JSON, stdout=network bundle JSON.
//
// The only differences between the two were the env-var name, the injected
// data bytes, and how the caller parses stdout — so the write-temp-data /
// write-temp-script+chmod / exec-with-env / capture-stdout dance lives here,
// once. kind ("storage"/"network") labels temp files and error messages.
//
// sidecars are extra files written into the SAME directory as the script,
// keyed by basename — the network seam uses this to ship its Python helper
// library (netlib) so a script's `import slagworks_netlib` resolves against
// its own directory (Python's sys.path[0]) with no PYTHONPATH plumbing. Pass
// nil when the script needs no companions (the storage seam). execkit stays
// domain-agnostic: it just co-locates whatever bytes the caller hands it.
//
// Everything lands in one per-invocation temp directory that is removed before
// returning (no /tmp leak, and no cross-invocation collision on the sidecar
// names). Only stdout carries the structured result. stderr is the script's
// diagnostics channel (the scripts' hard-fail "fix the NetBox model" ERRORs,
// their degraded-but-valid WARNINGs): it streams live to the agent's stderr
// (the serial console), and its tail (configScriptStderrCap) is also logged
// through log, one Warn record per line, whether or not the script succeeded,
// and appended to the returned error on failure. Without that the failure
// reached the operation result as a bare "exit status 1" and the script's
// explanation of what to fix was visible only on the serial console.
func RunConfigScript(log *slog.Logger, kind string, scriptContent []byte, envName string, data []byte, sidecars map[string][]byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "slagworks-"+kind+"-*")
	if err != nil {
		return nil, fmt.Errorf("create %s temp dir: %w", kind, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	dataPath := filepath.Join(dir, "data.json")
	if err := os.WriteFile(dataPath, data, 0600); err != nil {
		return nil, fmt.Errorf("write %s data file: %w", kind, err)
	}

	// Sidecars first, so a companion the script imports on startup is already
	// present when the interpreter loads it.
	for name, content := range sidecars {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			return nil, fmt.Errorf("write %s sidecar %q: %w", kind, name, err)
		}
	}

	scriptPath := filepath.Join(dir, "script")
	if err := os.WriteFile(scriptPath, scriptContent, 0700); err != nil {
		return nil, fmt.Errorf("write %s script file: %w", kind, err)
	}

	log.Info("executing config script", "kind", kind)
	cmd := exec.Command(scriptPath)
	cmd.Env = append(os.Environ(), envName+"="+dataPath)
	stderr := &tailBuffer{cap: configScriptStderrCap}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderr)
	out, err := cmd.Output()

	diag := strings.TrimSpace(stderr.String())
	for line := range strings.SplitSeq(diag, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			log.Warn(kind+" script stderr: "+line, "kind", kind)
		}
	}
	if err != nil {
		if diag == "" {
			return nil, fmt.Errorf("%s script failed: %w (the script wrote nothing to stderr)", kind, err)
		}
		return nil, fmt.Errorf("%s script failed: %w; script stderr:\n%s", kind, err, diag)
	}
	return out, nil
}
