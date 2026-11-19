package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/bootloader"
	"github.com/bluesky-social/slagworks/internal/agent/execkit"
	"github.com/bluesky-social/slagworks/internal/agentlog"
	"github.com/bluesky-social/slagworks/internal/slogkit"

	"flag"
)

// newAgentLogger returns a slog.Logger backed by the slagworks slogkit
// handler with the agent source set and trace/machine/op stamped as
// default attrs on every record. When sink is non-nil, every envelope
// is also pushed to the sink (typically agentlog.Client which POSTs
// batches to the site /logs/ingest endpoint). Stderr stays attached
// regardless so the local SOL viewer still sees the line on
// site-network outage.
func newAgentLogger(traceID, machineID, opID string, sink slogkit.Sink) *slog.Logger {
	h := slogkit.NewHandler(slogkit.HandlerOptions{
		Source: slogkit.SourceAgent,
		Level:  slog.LevelInfo,
		Stderr: os.Stderr,
		Sink:   sink,
	})
	logger := slog.New(h)
	if traceID != "" {
		logger = logger.With("trace_id", traceID)
	}
	if machineID != "" {
		logger = logger.With("machine_id", machineID)
	}
	if opID != "" {
		logger = logger.With("op_id", opID)
	}
	return logger
}

// teeWriter writes to an underlying writer and appends each line to a logCollector.
type teeWriter struct {
	w         io.Writer
	collector *logCollector
	source    string
}

func (tw *teeWriter) Write(p []byte) (int, error) {
	n, err := tw.w.Write(p)
	if tw.collector != nil {
		for line := range strings.SplitSeq(strings.TrimRight(string(p), "\n"), "\n") {
			if line != "" {
				tw.collector.Append(tw.source, line)
			}
		}
	}
	return n, err
}

// compareUUIDs normalizes two UUID strings and checks equality.
// Returns an error if either value is empty, unparseable, or they differ.
func compareUUIDs(expected, actual string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return fmt.Errorf("no expected UUID provided, machine may not be commissioned")
	}

	actual = strings.TrimSpace(actual)
	if actual == "" || actual == "Not Settable" || actual == "Not Present" {
		return fmt.Errorf("dmidecode returned unusable system-uuid: %q", actual)
	}

	exp, err := uuid.Parse(expected)
	if err != nil {
		return fmt.Errorf("expected UUID %q is not valid: %w", expected, err)
	}
	act, err := uuid.Parse(actual)
	if err != nil {
		return fmt.Errorf("actual UUID %q from dmidecode is not valid: %w", actual, err)
	}

	if exp != act {
		return fmt.Errorf("identity mismatch: expected %s but running on %s", exp, act)
	}
	return nil
}

// verifyIdentity checks that this machine's SMBIOS system-uuid matches the
// expected value. Returns an error on mismatch or if dmidecode fails.
func verifyIdentity(expectedUUID string) error {
	expected := strings.TrimSpace(expectedUUID)
	if expected == "" {
		return fmt.Errorf("no expected UUID provided, machine may not be commissioned")
	}

	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(dctx, "dmidecode", "-s", "system-uuid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("dmidecode failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}

	actual := strings.TrimSpace(string(out))
	return compareUUIDs(expected, actual)
}

func main() {
	server := flag.String("server", "", "slagworks worker address (host:port)")
	image := flag.String("image", "noble", "OS image version to deploy")
	mode := flag.String("mode", "deploy", "agent mode: deploy, commission, or stdout")
	destructive := flag.Bool("destructive", false, "skip disk safety check (wipe existing data)")
	wipeAllDisks := flag.Bool("wipe-all-disks", false, "also signature-wipe disks NOT in the storage layout (requires --destructive)")
	machineID := flag.String("machine", "", "machine UUID (from kernel cmdline)")
	opID := flag.String("op", "", "operation UUID (from kernel cmdline)")
	traceID := flag.String("trace", "", "operation trace UUID (from kernel cmdline; minted if absent)")
	expectUUID := flag.String("expect-uuid", "", "expected SMBIOS system-uuid (abort on mismatch)")
	flag.Parse()

	// Set up the structured-logs slog handler. Every line the agent
	// emits ships as a JSON envelope with trace_id/machine_id/op_id
	// pre-populated as default attrs, so site/global can correlate
	// agent lines with the rest of the op lifecycle. If trace_id is
	// missing from the cmdline (debug/manual invocation), mint one locally.
	if *traceID == "" {
		*traceID = uuid.NewString()
	}
	var logSink slogkit.Sink
	var logSinkClient *agentlog.Client
	if *server != "" {
		logSinkClient = agentlog.NewClient(*server, slogkit.SourceAgent)
		logSink = logSinkClient
	}
	log := newAgentLogger(*traceID, *machineID, *opID, logSink)
	slog.SetDefault(log)
	if logSinkClient != nil {
		// Drainer goroutine: lives for the agent's lifetime. Logging
		// is non-blocking on the caller path; this loop owns all
		// network I/O for log shipping.
		go logSinkClient.Run(context.Background())
	}

	// The agent context every subsystem receives: the structured logger
	// (the only output surface), the exec seam, and the site worker
	// address. Exec.Tee is wired below once the deploy-path collector
	// exists; the stdout/inventory modes run with the default tee.
	ek := &execkit.Exec{Log: log}
	ctx := &agent.Context{Log: log, Exec: ek, Server: *server}

	if *mode == "stdout" {
		runCommissionStdout(ctx)
		return
	}

	if *mode == "inventory" {
		if *server == "" {
			fmt.Fprintln(os.Stderr, "slagworks-agent inventory: --server is required")
			os.Exit(1)
		}
		runInventory(ctx)
		return
	}

	if *server == "" {
		log.Error("--server is required")
		os.Exit(1)
	}

	// Correct the wall clock before anything else, for both deploy and
	// commission: a host with a dead CMOS battery boots at its RTC reset
	// epoch, which breaks Tailscale enrollment and the deployed OS's
	// first-boot apt. Running it first also stamps every later envelope
	// and the deployed root's file mtimes with sane time.
	syncClock(log, *server, &http.Client{Timeout: 10 * time.Second}, sysClock{})

	// Set up log collector and reporter if machine/op IDs are provided
	collector := newLogCollector()
	done := make(chan struct{})
	var reporter *agentReporter

	if *machineID != "" && *opID != "" {
		reporter = newAgentReporter(log, *server, *opID, *machineID, collector)
		go startDmesgReader(log, collector, done)
		go reporter.RunFlushLoop(done)
	}

	// Tee exec output live to stdout (SOL) AND the collector, so
	// long-running commands stay visible mid-run in both feeds.
	ek.Tee = &teeWriter{w: os.Stdout, collector: collector, source: "cmd"}

	// SLAGWORKS_FATAL_AT is a TEST-ONLY knob. When set to a phase name
	// (e.g. "installing_bootloader"), the agent fatal()s the moment
	// setPhase enters that phase. The integration test suite uses this
	// to exercise the recovery-sshd hook against a deliberately failed
	// late-stage agent without needing to engineer a real fault. There
	// is no production use; the env var is opt-in and silent when
	// unset, so leaving the hook in shipping binaries costs nothing.
	fatalAtPhase := os.Getenv("SLAGWORKS_FATAL_AT")

	setPhase := func(phase string) {
		if reporter != nil {
			reporter.ReportPhase(phase)
		}
		if fatalAtPhase != "" && phase == fatalAtPhase {
			log.Error(fmt.Sprintf("SLAGWORKS_FATAL_AT=%s honored, exiting non-zero", phase))
			if reporter != nil {
				reporter.ReportFailure(phase, "SLAGWORKS_FATAL_AT="+phase)
			}
			close(done)
			os.Exit(1)
		}
	}

	fatal := func(stage string, err error) {
		msg := fmt.Sprintf("%s: %v", stage, err)
		log.Error("FATAL during "+stage, "err", err)
		if reporter != nil {
			reporter.ReportFailure(stage, msg)
		}
		close(done)
		os.Exit(1)
	}

	log.Info("agent starting")
	log.Info("agent config", "server", *server, "mode", *mode)

	if *mode == "commission" {
		if reporter == nil {
			fatal("commission identity", errors.New("--machine and --op are required"))
		}
		runCommission(ctx, *machineID, reporter, setPhase, fatal)
		return
	}
	if reporter == nil {
		fatal("deploy identity", errors.New("--machine and --op are required"))
	}

	runDeploy(ctx, deployParams{
		image:         *image,
		machineID:     *machineID,
		operationID:   *opID,
		expectUUID:    *expectUUID,
		destructive:   *destructive,
		wipeAllDisks:  *wipeAllDisks,
		reporter:      reporter,
		setPhase:      setPhase,
		fatal:         fatal,
		done:          done,
		checkIdentity: verifyIdentity,
		armBootNext:   bootloader.SetBootNextToInstalledOS,
		reboot:        rebootInto,
	})
}

// rebootInto syncs filesystems and forces a reboot. Logs the reason so
// serial output makes the cause obvious. Calls os.Exit if reboot(2)
// itself returns — at that point we have nothing useful to do.
func rebootInto(ctx *agent.Context, reason string) {
	ctx.Log.Info("rebooting", "reason", reason)
	// Best-effort sync; failures here are not fatal — `reboot -f`
	// will sync regardless via the kernel's reboot(2) sys call.
	_ = ctx.Exec.Run("sync")
	if err := ctx.Exec.Run("reboot", "-f"); err != nil {
		ctx.Log.Error("reboot failed", "err", err)
		os.Exit(1)
	}
	// `reboot -f` doesn't return on success; if we get here something
	// is very wrong.
	os.Exit(0)
}

// hasKernelArg reports whether /proc/cmdline contains the given
// space-delimited token. Returns false on any read error — callers
// treat the arg as absent rather than fail-closed.
func hasKernelArg(want string) bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	return cmdlineHas(string(data), want)
}

// cmdlineHas is the testable string-matcher behind hasKernelArg.
// Matches the bare token AND the key portion of `key=value` forms.
func cmdlineHas(cmdline, want string) bool {
	for tok := range strings.FieldsSeq(cmdline) {
		if tok == want {
			return true
		}
		if idx := strings.IndexByte(tok, '='); idx > 0 && tok[:idx] == want {
			return true
		}
	}
	return false
}
