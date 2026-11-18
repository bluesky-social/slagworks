package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
)

func newMachineCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "machine", Short: "manage machines"}
	cmd.AddCommand(
		machineListCmd(),
		machineShowCmd(),
		machineLogsCmd(),
		machineOpsCmd(),
		machineCredsCmd(),
		machineCommissionCmd(),
		machineDeployCmd(),
		machinePreflightCmd(),
		machineMatchCmd(),
		machinePowerCmd(),
		machineBMCReconcileCmd(),
		machineLockCmd(),
		machineUnlockCmd(),
		machinePXEEnableCmd(),
		machinePXEDisableCmd(),
		machineAbortCmd(),
	)
	return cmd
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func emptyHostname(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// shellQuote wraps a string in POSIX-shell single quotes, escaping
// any embedded single quotes via the canonical '\”  trick. The
// output is safe to paste into bash/sh as a single argument or env-
// var value regardless of what metacharacters s contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func machineListCmd() *cobra.Command {
	var stateFilter string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list machines",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateStateFlag(stateFilter); err != nil {
				return err
			}
			var machines []model.Machine
			q := url.Values{}
			if stateFilter != "" {
				q.Set("state", stateFilter)
			}
			if err := apiClient.Get(ctx(cmd), "/api/v1/machines", q, &machines); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), machines)
			}
			rows := make([][]string, len(machines))
			for i, m := range machines {
				lock := ""
				if m.Locked {
					lock = format.WarnStyle.Render("locked")
				}
				rows[i] = []string{
					m.ID.String(),
					emptyHostname(m.Hostname),
					m.NetBoxSerial,
					format.MachineStateLabel(string(m.State)),
					lock,
					m.DesiredOS,
					format.HumanDuration(m.CreatedAt),
				}
			}
			format.PrintTable(cmd.OutOrStdout(),
				[]string{"ID", "HOSTNAME", "SERIAL", "STATE", "LOCK", "DESIRED_OS", "CREATED"},
				rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateFilter, "state", "", "filter by state")
	return watchable(cmd)
}

func machineShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "show machine details",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var m model.Machine
			if err := apiClient.Get(ctx(cmd), "/api/v1/machines/"+r.Slot(), nil, &m); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), m)
			}
			return printMachineDetail(cmd.OutOrStdout(), &m)
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func machineCommissionCmd() *cobra.Command {
	var watch, pxeEnable bool
	var via string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "commission",
		Short: "trigger commission for a machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch via {
			case "", "bmc", "pdu", "amt":
			default:
				return fmt.Errorf("invalid --via %q (expected bmc, pdu, or amt)", via)
			}
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			if pxeEnable {
				if err := pxeEnableFor(cmd, r); err != nil {
					return err
				}
			}
			var bodyArg any
			if via != "" {
				bodyArg = map[string]string{"via": via}
			}
			if via == "pdu" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Transport: PDU outlets (commission power-cycle actuates the device's switched outlets)\n")
			}
			var op model.Operation
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/commission", bodyArg, &op); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), op)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Commission operation created: %s\n", op.ID)
			if watch {
				return watchOperation(cmd.Context(), op.ID.String(), timeout, false, false)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", format.DimStyle.Render("Run: slagctl operation watch "+op.ID.String()))
			return nil
		},
	}
	bindMachineArg(cmd)
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "watch the operation until it completes")
	cmd.Flags().BoolVar(&pxeEnable, "pxe-enable", false, "set pxe_eligible=true first (skips the separate `machine pxe-enable` step)")
	cmd.Flags().StringVar(&via, "via", "",
		"power transport: \"bmc\", \"pdu\" (power-cycle via the device's switched PDU outlets; for BMC-less switches), or \"amt\" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "maximum wait time (with -w)")
	return cmd
}

func machineDeployCmd() *cobra.Command {
	var destructive, wipeAllDisks, recovery, watch, pxeEnable bool
	var via string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "trigger deploy for a machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch via {
			case "", "bmc", "pdu", "amt":
			default:
				return fmt.Errorf("invalid --via %q (expected bmc, pdu, or amt)", via)
			}
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			if pxeEnable {
				if err := pxeEnableFor(cmd, r); err != nil {
					return err
				}
			}
			body := map[string]any{}
			if wipeAllDisks {
				// wipe-all-disks implies destructive: wiping bystander
				// disks with the safety check still armed makes no sense.
				destructive = true
				body["wipe_all_disks"] = true
			}
			if destructive {
				body["destructive"] = true
			}
			if recovery {
				body["recovery"] = true
			}
			if via != "" {
				body["via"] = via
			}
			var bodyArg any
			if len(body) > 0 {
				bodyArg = body
			}
			if via == "pdu" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Transport: PDU outlets (deploy power-cycle actuates the device's switched outlets)\n")
			}
			var op model.Operation
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/deploy", bodyArg, &op); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), op)
			}
			msg := "Deploy operation created: %s\n"
			switch {
			case recovery && destructive:
				msg = "Deploy operation created (recovery, destructive): %s\n"
			case recovery:
				msg = "Deploy operation created (recovery): %s\n"
			case wipeAllDisks:
				msg = "Deploy operation created (destructive, wiping ALL disks): %s\n"
			case destructive:
				msg = "Deploy operation created (destructive): %s\n"
			}
			fmt.Fprintf(cmd.OutOrStdout(), msg, op.ID)
			if watch {
				return watchOperation(cmd.Context(), op.ID.String(), timeout, false, false)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", format.DimStyle.Render("Run: slagctl operation watch "+op.ID.String()))
			return nil
		},
	}
	bindMachineArg(cmd)
	cmd.Flags().BoolVar(&destructive, "destructive", false, "skip disk safety check (wipe existing data)")
	cmd.Flags().BoolVar(&wipeAllDisks, "wipe-all-disks", false, "also signature-wipe every disk NOT in the storage layout (clears stale md superblocks/filesystems; implies --destructive)")
	cmd.Flags().BoolVar(&recovery, "recovery", false, "boot into recovery sshd instead of running the deploy agent")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "watch the operation until it completes")
	cmd.Flags().BoolVar(&pxeEnable, "pxe-enable", false, "set pxe_eligible=true first (skips the separate `machine pxe-enable` step)")
	cmd.Flags().StringVar(&via, "via", "",
		"power transport: \"bmc\", \"pdu\" (power-cycle via the device's switched PDU outlets; for BMC-less switches), or \"amt\" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "maximum wait time (with -w)")
	return cmd
}

func machinePreflightCmd() *cobra.Command {
	var opKind string
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "check a host's NetBox model for the config a deploy/commission boot needs (read-only, creates no op)",
		Long: "Runs the same op-creation validation global performs (the real " +
			"snapshot-build matching path) without creating an operation, so you " +
			"can confirm a host is fully modeled before firing a deploy.\n\n" +
			"Validates: primary_ip4 set, a bootable NIC with a MAC, a matching " +
			"network-config script (or plain-DHCP intent), a matching storage " +
			"script (deploy only), and a registered image of the right kind.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opKind != "deploy" && opKind != "commission" {
				return fmt.Errorf("--op must be 'deploy' or 'commission', got %q", opKind)
			}
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			q := url.Values{"op": {opKind}}
			var res api.PreflightResult
			if err := apiClient.Get(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/preflight", q, &res); err != nil {
				return err
			}
			if jsonOut {
				if err := format.PrintJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
				// Exit non-zero on gaps so a `--json` preflight gate in a
				// script fails loudly, matching the human-output path below.
				// ErrSilent: the JSON body already carries the gaps; main
				// suppresses the (empty) error message and just sets the code.
				if !res.OK {
					return ErrSilent
				}
				return nil
			}
			out := cmd.OutOrStdout()
			if res.OK {
				fmt.Fprintf(out, "%s preflight (%s): OK — host is fully modeled\n",
					args[0], opKind)
				return nil
			}
			fmt.Fprintf(out, "%s preflight (%s): %d issue(s):\n", args[0], opKind, len(res.Gaps))
			for _, g := range res.Gaps {
				fmt.Fprintf(out, "  - %s\n", g)
			}
			// Non-zero exit so a preflight gate in a script fails loudly.
			return fmt.Errorf("preflight found %d unmet precondition(s)", len(res.Gaps))
		},
	}
	bindMachineArg(cmd)
	cmd.Flags().StringVar(&opKind, "op", "deploy", "operation to validate for: deploy or commission")
	return cmd
}

func machineMatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "match",
		Short: "show which config scripts (network + storage) a host resolves to (read-only, creates no op)",
		Long: "Reports the network-config script and storage script the owning " +
			"worker would select for this host, by building the same snapshot the " +
			"boot path uses and reading the authoritative match off it (so it can't " +
			"drift from a real deploy). Answers \"what would this host get?\" before " +
			"firing one.\n\n" +
			"A script shown as \"(none)\" means no script of that kind matched — the " +
			"agent falls back to its built-in DHCP netplan (network) or default " +
			"layout (storage). NOTE this proves the MATCH only, not that the script " +
			"will render correct config from the host's NetBox facts.",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var res api.MatchResult
			if err := apiClient.Get(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/match", nil, &res); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), res)
			}
			out := cmd.OutOrStdout()
			name := func(m *api.MatchedScript) string {
				if m == nil {
					return "(none — agent fallback)"
				}
				return m.Name
			}
			host := res.Hostname
			if host == "" {
				host = args[0]
			}
			fmt.Fprintf(out, "%s matches:\n", host)
			fmt.Fprintf(out, "  network: %s\n", name(res.NetworkScript))
			fmt.Fprintf(out, "  storage: %s\n", name(res.StorageScript))
			return nil
		},
	}
	bindMachineArg(cmd)
	return cmd
}

// pxeEnableFor flips pxe_eligible=true ahead of a commission/deploy
// request, for the --pxe-enable convenience flag. Idempotent server-
// side; failures abort the op request so the operator never gets the
// confusing "not PXE-eligible" rejection right after asking us to
// enable it.
func pxeEnableFor(cmd *cobra.Command, r ResolvedMachine) error {
	var m model.Machine
	if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/activate", nil, &m); err != nil {
		return fmt.Errorf("pxe-enable %s: %w", r.Label, err)
	}
	if !jsonOut {
		fmt.Fprintf(cmd.OutOrStdout(), "Enabled PXE for machine %s (%s)\n", m.NetBoxSerial, m.ID)
	}
	return nil
}

// machinePowerCmd is a parent for power subcommands. Each subcommand
// (on/off/cycle/status) is its own cobra.Command sharing the
// positional <machine> arg.
func machinePowerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "power",
		Short: "control machine power state",
		// Reject unknown subcommands instead of cobra's default "print
		// help, return nil". A typo like `machine power reboot` should
		// fail loudly so on-call doesn't think the action ran.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown power subcommand %q (expected on/off/cycle/status/boot-status/boot-set)", args[0])
			}
			return cmd.Help()
		},
		Args: cobra.ArbitraryArgs,
	}
	cmd.AddCommand(
		machinePowerActionCmd("on", "power on a machine", false),
		machinePowerActionCmd("off", "power off a machine", true),
		machinePowerActionCmd("cycle", "power-cycle a machine", false),
		machinePowerActionCmd("status", "show current BMC power state", false),
		machineBootStatusCmd(),
		machineBootSetCmd(),
	)
	return cmd
}

// machineBootSetCmd arms a BMC boot-device override (Supermicro only). One-
// time by default; --persistent arms it for all future boots. --device
// default clears any override (--persistent is rejected there).
func machineBootSetCmd() *cobra.Command {
	var device string
	var persistent bool
	cmd := &cobra.Command{
		Use:   "boot-set",
		Short: "set the BMC's next-boot override (Supermicro only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			switch device {
			case "pxe", "disk", "default":
			default:
				return fmt.Errorf("invalid --device %q (expected pxe, disk, or default)", device)
			}
			if device == "default" && persistent {
				return fmt.Errorf("--persistent is not valid with --device default (clearing an override has no persistence)")
			}
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			reqBody := map[string]any{"device": device, "persistent": persistent}
			var op model.Operation
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/boot", reqBody, &op); err != nil {
				return fmt.Errorf("boot-set for %s: %w", r.Label, err)
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), op)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Boot override op created: %s\n", op.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&device, "device", "", "boot device: pxe, disk, or default (clear)")
	cmd.Flags().BoolVar(&persistent, "persistent", false, "apply to all future boots (default: one-time)")
	_ = cmd.MarkFlagRequired("device")
	bindMachineArg(cmd)
	return cmd
}

// machineBootStatusCmd reads the BMC's current next-boot override. Its own
// command (not folded into `status`) so it doesn't add a second sequential
// BMC RPC to the hot power-status path. Supermicro-only for now: a host
// whose BMC controller isn't a boot-capable one reports "not supported".
func machineBootStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "boot-status",
		Short: "show the BMC's current next-boot override (Supermicro only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var st api.BootStatusResponse
			if err := apiClient.Get(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/boot", nil, &st); err != nil {
				return fmt.Errorf("boot status for %s: %w", r.Label, err)
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), st)
			}
			if !st.Supported {
				// Not an error in the API sense (the read succeeded), but a
				// non-zero exit so scripts can tell "unsupported" from "armed".
				fmt.Fprintf(cmd.ErrOrStderr(),
					"%s: boot-order ops not supported by this host's power controller\n", r.Label)
				if st.Hint != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "hint: %s\n", st.Hint)
				}
				return ErrSilent
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s)\n",
				r.Label, formatBootOverride(st), st.Transport)
			return nil
		},
	}
	bindMachineArg(cmd)
	return cmd
}

// formatBootOverride renders the boot override for human output, e.g.
// "next boot: pxe (one-time, armed)" or "no override armed".
func formatBootOverride(st api.BootStatusResponse) string {
	if !st.Valid {
		return "no override armed"
	}
	persistence := "one-time"
	if st.Persistent {
		persistence = "persistent"
	}
	dev := st.Device
	if st.Device == "other" && st.Raw != "" {
		// Surface the vendor token for a device we don't model.
		dev = "other:" + st.Raw
	}
	return fmt.Sprintf("next boot: %s (%s, armed)", dev, persistence)
}

// machinePowerActionCmd builds one of the power subcommands.
// confirmDestructive triggers the interactive "are you sure" prompt
// (currently only used by `off`); --yes bypasses it for non-tty use.
func machinePowerActionCmd(action, short string, confirmDestructive bool) *cobra.Command {
	var yes bool
	var via string
	cmd := &cobra.Command{
		Use:   action,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			switch via {
			case "", "bmc", "pdu", "amt":
			default:
				return fmt.Errorf("invalid --via %q (expected bmc, pdu, or amt)", via)
			}
			if action == "status" {
				// Status is a read; no operation row is created. The
				// server RPCs the machine's site worker over the WS hub
				// and answers synchronously with the BMC-reported state.
				var st api.PowerStatusResponse
				if err := apiClient.Get(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/power", nil, &st); err != nil {
					return fmt.Errorf("power status for %s: %w", r.Label, err)
				}
				if jsonOut {
					return format.PrintJSON(cmd.OutOrStdout(), st)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s)\n",
					r.Label, format.PowerStateLabel(st.State), st.Transport)
				if st.Hint != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "hint: %s\n", st.Hint)
				}
				return nil
			}
			// PDU is a hard AC actuation on shared infra (an outlet-index
			// error de-powers a different device on the same strip), so
			// surface the transport before acting — the operator should
			// never be surprised that "cycle the switch" meant "yank its
			// power cords" (doc §A actuation consent).
			if via == "pdu" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Transport: PDU outlets (hard AC %s on the device's switched outlets)\n", action)
			}
			if confirmDestructive && !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to power off without confirmation; pass --yes to skip the prompt")
				}
				var confirmed bool
				if err := huh.NewConfirm().
					Title(fmt.Sprintf("Power off machine %s?", r.Label)).
					Value(&confirmed).
					Run(); err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}
			body := map[string]string{"action": action}
			if via != "" {
				body["via"] = via
			}
			var op model.Operation
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/power", body, &op); err != nil {
				transport := via
				if transport == "" {
					transport = "auto: amt or bmc by credential kind"
				}
				// Don't claim the op was or wasn't created — on a
				// timeout the client can't know. The transport error
				// (client.humanizeTransportErr) carries the
				// check-before-retry guidance.
				return fmt.Errorf("power %s for %s (via %s): %w", action, r.Label, transport, err)
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), op)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Power operation created: %s\n", op.ID)
			return nil
		},
	}
	bindMachineArg(cmd)
	if action != "status" {
		cmd.Flags().StringVar(&via, "via", "",
			"power transport: \"bmc\", \"pdu\" (actuate the device's switched PDU outlets; for BMC-less switches or a wedged BMC), or \"amt\" (Intel AMT/vPro). Default: amt when the device's bmc-creds-admin entry is kind=amt, else bmc")
	}
	if confirmDestructive {
		cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation prompt")
	}
	return cmd
}

// machineBMCReconcileCmd creates a bmc_reconcile op: the site worker that
// owns the BMC's OOB network brings its IPMI users, then its time
// configuration, into compliance over the LAN using the device's
// bmc-creds-admin account.
func machineBMCReconcileCmd() *cobra.Command {
	var rotateMonitor bool
	cmd := &cobra.Command{
		Use:   "bmc-reconcile",
		Short: "bring a machine's BMC user accounts and time settings into compliance over the LAN",
		Long: `Create a bmc_reconcile operation. The site worker that owns the BMC's
OOB network logs in over IPMI LAN with the device's pop/<pop>/bmc-creds-admin
entry (keyed by NetBox serial), checks the BMC's chassis serial against
NetBox, then:

  - leaves the slot-2 admin account untouched,
  - ensures exactly one "monitor" account at USER privilege holding the
    password stored for the device in pop/<pop>/bmc-creds-monitor (keyed
    by NetBox serial): the account is tested first and the password is
    written only when the BMC does not already hold it, so a BMC whose
    monitor drifted converges back to the store without rotating
    anything. Only when nothing is stored for the device (a new BMC), or
    with --rotate-monitor, is the password freshly generated and stored
    there; the op result's monitor_rotated and monitor_password_written
    say which happened,
  - disables and clears every other named account,
  - over Redfish with the same account: enables NTP with the ntp_servers
    of the BMC's slagworks-oob prefix as the only servers, sets the BMC
    timezone to UTC, and steps the BMC clock to the site worker's when it
    is more than 60s off.

Boot order and power are never touched, and the host OS is unaffected, so
this is safe on in-service hosts (no unlock needed). The op is refused
unless the admin entry names the BMC's slot-2 account. A time failure
(or a prefix without ntp_servers) is reported in the op result's
time_error and does not fail the op. Follow it with
"slagctl operation watch <op-id>".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var body any
			if rotateMonitor {
				body = map[string]any{"rotate_monitor": true}
			}
			var op model.Operation
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/bmc-reconcile", body, &op); err != nil {
				return fmt.Errorf("bmc-reconcile for %s: %w", r.Label, err)
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), op)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "BMC reconcile operation created: %s\n", op.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", format.DimStyle.Render("Run: slagctl operation watch "+op.ID.String()))
			return nil
		},
	}
	bindMachineArg(cmd)
	cmd.Flags().BoolVar(&rotateMonitor, "rotate-monitor", false,
		"write a freshly generated monitor password and store it in pop/<pop>/bmc-creds-monitor, instead of re-asserting the stored one. "+
			"The oob-gw ipmi_exporter and sel-archiver keep the old password, so this BMC's scrapes fail (ipmi_up=0) until "+
			"\"ansible-playbook oob-gw.yaml --tags sel_archiver,ipmi_exporter\" re-renders their creds")
	return cmd
}

func machineLockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lock",
		Short: "lock a machine to prevent new operations",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var m model.Machine
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/lock", nil, &m); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), m)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Locked machine %s (%s)\n", m.NetBoxSerial, m.ID)
			return nil
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func machineUnlockCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unlock",
		Short: "unlock a machine to allow new operations",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var m model.Machine
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/unlock", nil, &m); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), m)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Unlocked machine %s (%s)\n", m.NetBoxSerial, m.ID)
			return nil
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func machinePXEEnableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pxe-enable",
		Short: "enable PXE boot for a machine",
		Long: `Sets pxe_eligible=true, allowing the machine to be PXE booted for
commission/deploy operations.

Renamed from "activate" 2026-05-19 to make the verb scoped to PXE
rather than ambiguous machine state. PXE-enable is blocked for
locked machines.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetPXEEligible(cmd, args[0], true)
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func machinePXEDisableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pxe-disable",
		Short: "disable PXE boot for a machine",
		Long:  `Sets pxe_eligible=false, preventing the machine from being PXE booted.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetPXEEligible(cmd, args[0], false)
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func machineAbortCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "abort",
		Short: "cancel active operations and reset machine state",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var m model.Machine
			if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/abort", nil, &m); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), m)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Aborted machine %s (%s), state: %s\n", m.NetBoxSerial, m.ID, m.State)
			return nil
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func runSetPXEEligible(cmd *cobra.Command, arg string, eligible bool) error {
	r, err := resolveMachine(ctx(cmd), apiClient, arg)
	if err != nil {
		return err
	}
	// API endpoint names are unchanged: /activate and /deactivate.
	// The CLI verb is the operator surface; the API verb is the
	// backend's idea of state transition. Both are correct in their
	// domain; renaming the API path would force a coordinated
	// client/server release for cosmetic gain.
	apiAction := "activate"
	if !eligible {
		apiAction = "deactivate"
	}
	var m model.Machine
	if err := apiClient.Post(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/"+apiAction, nil, &m); err != nil {
		return err
	}
	if jsonOut {
		return format.PrintJSON(cmd.OutOrStdout(), m)
	}
	verb := "Enabled PXE for"
	if !eligible {
		verb = "Disabled PXE for"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s machine %s (%s)\n", verb, m.NetBoxSerial, m.ID)
	return nil
}
