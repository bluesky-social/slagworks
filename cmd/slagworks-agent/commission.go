package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/consoleprobe"
	"github.com/bluesky-social/slagworks/internal/agent/inventory"
)

func runInventory(ctx *agent.Context) {
	ctx.Log.Info("inventory collection starting")
	result := inventory.CollectGraceful(ctx.Exec, inventoryOptions())
	ctx.Log.Info("inventory collected")
	if err := reportInventory(ctx, result); err != nil {
		ctx.Log.Error("inventory report failed", "err", err)
		os.Exit(1)
	}
	ctx.Log.Info("inventory report accepted")
}

func reportInventory(ctx *agent.Context, result *inventory.CommissionResult) error {
	return postCommissionResult(ctx, fmt.Sprintf("http://%s/api/v1/inventory", ctx.Server), "inventory report", result)
}

// postCommissionResult marshals a CommissionResult and POSTs it to the
// given site-worker endpoint with the commission-path retry policy: 3
// attempts, flat 2s backoff, success requires an exact 200 (the endpoints
// return 200-or-nothing; a 202/3xx is not an accepted terminal state
// here, unlike the deploy_complete report which accepts any 2xx). Shared
// by reportInventory and reportCommission, which differ only in URL and
// label.
func postCommissionResult(ctx *agent.Context, url, name string, result *inventory.CommissionResult) error {
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal %s data: %w", name, err)
	}
	ctx.Log.Info("reporting "+name, "bytes", len(data))

	return agent.PostJSON(ctx.Log, url, data, agent.PostOptions{
		Client:   &http.Client{Timeout: 30 * time.Second},
		Attempts: 3,
		Backoff:  2 * time.Second,
		Name:     name,
		AcceptFn: func(status int) bool { return status == http.StatusOK },
	})
}

func runCommissionStdout(ctx *agent.Context) {
	result := inventory.Collect(ctx.Exec, inventoryOptions())
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		ctx.Log.Error("encoding commission data", "err", err)
		os.Exit(1)
	}
}

// runCommission drives the commission path. setPhase/fatal are the same
// closures the deploy path uses (main.go): setPhase emits a phase-bearing
// envelope (no-op when reporter is nil, i.e. debug/stdout mode); fatal reports
// the failure and exits non-zero. The phase checkpoints (collecting_inventory
// → reporting → console_probe) drive the TUI's commission lifecycle column
// (plan O3) the same way deploy's setPhase calls drive its spine.
func runCommission(ctx *agent.Context, machineID string, reporter *agentReporter, setPhase func(string), fatal func(string, error)) {
	ctx.Log.Info("commission starting")

	// agent_starting is the first agent-emitted phase on the commission spine
	// (lifecycle.go: commissionSpine), the handoff from the site worker's boot
	// prefix to the agent. The deploy path emits it from main.go; commission
	// returns before that point, so emit it here too — otherwise the spine
	// jumps ipxe_loaded -> collecting_inventory and backfills agent_starting as
	// "skipped" even though the agent did start.
	setPhase("agent_starting")

	setPhase("collecting_inventory")
	result := inventory.Collect(ctx.Exec, inventoryOptions())

	ctx.Log.Info("commission data collected")

	// Console determination sweep: when the device-type has no
	// slagworks_kernel_params (typically a type this very commission
	// is about to mint), probe every candidate tty with a distinct
	// nonce BEFORE reporting, so the candidate set rides the
	// commission report and global can resolve which nonce reached
	// console_cast_events → write the discovered console= back to the
	// device-type. Runs pre-report deliberately; the verification
	// probe for an already-configured device stays post-report below.
	// No setPhase here: the TUI's commission spine places console_probe
	// after reporting; the sweep rides inside collecting_inventory and
	// announces itself via its own stdout lines.
	kp := ""
	if machineID != "" {
		kpBytes, err := agent.HTTPGet(fmt.Sprintf("http://%s/api/v1/machines/%s/kernel-params", ctx.Server, machineID))
		if err != nil {
			fatal("fetch kernel-params", err)
		}
		kp = strings.TrimSpace(string(kpBytes))
		if kp == "" {
			result.ConsoleSweep = consoleprobe.Sweep(ctx, machineID)
		}
	}

	setPhase("reporting")
	if err := reportCommission(ctx, reporter, result); err != nil {
		fatal("commission report", err)
	}

	// Validate the BMC SOL relay matches the configured
	// kernel_params before letting the host onto the deploy path.
	// The probe writes a unique marker to the configured tty at the
	// configured baud; the bytes ride the BMC SOL session up to
	// global's console_cast_events. Operator confirms via the
	// scan endpoint or via slagctl. kernel_params "none" or absent
	// machineID skips the probe (debug mode / no-SOL devices); an
	// UNSET kernel_params ran the determination sweep above instead.
	// The TUI renders console_probe as "skipped" when it never ran.
	if machineID != "" && kp != "" && kp != "none" {
		setPhase("console_probe")
		if err := consoleprobe.Probe(ctx, machineID); err != nil {
			fatal("console probe", err)
		}
	}

	ctx.Log.Info("commission complete")

	// Reboot so the host re-PXEs and the boot-action returns to a
	// steady state. The machine is now `commissioned` (no pending op);
	// boot-action returns BootLocal, iPXE attempts sanboot, and on a
	// fresh-from-factory machine that has no installed OS yet this
	// just exits the iPXE script — the host sits in firmware until
	// the operator schedules a deploy and BMC-cycles it.
	//
	// Without this reboot the agent exits and the kernel panics
	// (PID 1 exited), which is not great optics on the SOL console
	// even though the operational impact is the same.
	if hasKernelArg("slagworks.no_reboot") {
		ctx.Log.Info("slagworks.no_reboot set, holding in initrd")
		select {} // park forever
	}
	rebootInto(ctx, "commission")
}

func reportCommission(ctx *agent.Context, reporter *agentReporter, result *inventory.CommissionResult) error {
	if reporter == nil {
		return fmt.Errorf("commission requires machine and operation identity")
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal commission data: %w", err)
	}
	ctx.Log.Info("reporting commission report", "bytes", len(data))
	if err := reporter.ReportCommissionComplete(data); err != nil {
		return err
	}
	ctx.Log.Info("commission report accepted")
	return nil
}
