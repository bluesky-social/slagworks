package cmd

import (
	"fmt"
	"net/url"
	"os"
	"strconv"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
)

// newPowerCmd builds the `slagctl power` parent and its subcommand tree.
func newPowerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "power",
		Short: "PDU-centric power control",
		// Reject unknown subcommands so a typo doesn't silently succeed.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown power subcommand %q (expected: outlet)", args[0])
			}
			return cmd.Help()
		},
		Args: cobra.ArbitraryArgs,
	}
	cmd.AddCommand(newPowerOutletCmd())
	return cmd
}

// newPowerOutletCmd builds the `slagctl power outlet` subcommand group
// with cycle/on/off verbs.
func newPowerOutletCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "outlet",
		Short: "control a PDU outlet by address",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown outlet subcommand %q (expected: cycle, on, off)", args[0])
			}
			return cmd.Help()
		},
		Args: cobra.ArbitraryArgs,
	}
	cmd.AddCommand(
		outletVerbCmd("cycle", "power-cycle a PDU outlet"),
		outletVerbCmd("on", "turn a PDU outlet on"),
		outletVerbCmd("off", "turn a PDU outlet off (strands device until an explicit 'on')"),
	)
	return cmd
}

// outletVerbCmd builds one of the cycle/on/off verb commands for
// `slagctl power outlet`.
func outletVerbCmd(verb, short string) *cobra.Command {
	var yes bool
	var allowSelf bool
	cmd := &cobra.Command{
		Use:   verb + " <pdu-name> <outlet>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pduIdent := args[0]
			outletN, err := strconv.Atoi(args[1])
			if err != nil || outletN < 1 {
				return fmt.Errorf("outlet %q: must be an integer >= 1", args[1])
			}
			// --allow-self is the INV-2 self-power override: it only makes
			// sense for cycle/on (a worker cycling its OWN feed briefly drops
			// itself and comes back). `off` has no serving-worker-off path
			// (INV-1), so reject the flag there before any server round-trip.
			if allowSelf && verb == "off" {
				return fmt.Errorf("--allow-self is not valid for `off` (there is no serving-worker-off path)")
			}

			// Step 1: resolve outlet — GET .../resolve?verb=<verb>
			resolvePath := "/api/v1/pdus/" + url.PathEscape(pduIdent) +
				"/outlet/" + strconv.Itoa(outletN) + "/power/resolve"
			q := url.Values{"verb": {verb}}
			var resolution api.OutletResolution
			if err := apiClient.Get(ctx(cmd), resolvePath, q, &resolution); err != nil {
				return fmt.Errorf("resolve outlet %d on %s: %w", outletN, pduIdent, err)
			}

			// Step 2: hard stop if the outlet has no cabled downstream device.
			// The interlock can't identify what it cuts, so this is a refusal,
			// not a confirmable warning.
			if !resolution.Connected {
				fmt.Fprintf(cmd.ErrOrStderr(), "error: %s\n", resolution.Warning)
				return fmt.Errorf("outlet %d on %s: unconnected (no downstream device in NetBox)", outletN, pduIdent)
			}

			// Step 3: print the resolved facts + server warning so the operator
			// sees what is about to happen.
			stderr := cmd.ErrOrStderr()
			fmt.Fprintf(stderr, "PDU:        %s (%s)\n", resolution.PDUName, resolution.PDUMgmtIP)
			fmt.Fprintf(stderr, "Outlet:     %d\n", resolution.Outlet)
			fmt.Fprintf(stderr, "Downstream: %s (port %s)\n", resolution.DownstreamDevice, resolution.DownstreamPort)
			fmt.Fprintf(stderr, "Warning:    %s\n", resolution.Warning)
			if verb == "off" {
				fmt.Fprintf(stderr, "Note:       outlet will remain off until an explicit 'power outlet on'\n")
			}
			if allowSelf {
				fmt.Fprintf(stderr, "WARNING:    --allow-self is set: if this outlet feeds the serving worker itself,\n")
				fmt.Fprintf(stderr, "            it will actuate its OWN feed and briefly drop itself.\n")
			}

			// Step 4: confirmation.
			if !yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to actuate without confirmation; pass --yes")
				}
				var confirmed bool
				confirmTitle := fmt.Sprintf("%s outlet %d on %s (feeds %s)?",
					verb, outletN, resolution.PDUName, resolution.DownstreamDevice)
				if err := huh.NewConfirm().
					Title(confirmTitle).
					Value(&confirmed).
					Run(); err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			// Step 5: POST to create the power op.
			postPath := "/api/v1/pdus/" + url.PathEscape(pduIdent) +
				"/outlet/" + strconv.Itoa(outletN) + "/power"
			postBody := map[string]any{
				"verb":            verb,
				"preflight_token": resolution.PreflightToken,
				"allow_self":      allowSelf,
			}
			var op model.Operation
			if err := apiClient.Post(ctx(cmd), postPath, postBody, &op); err != nil {
				return fmt.Errorf("power %s outlet %d on %s: %w", verb, outletN, pduIdent, err)
			}

			// Step 6: success output.
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), op)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Power operation created: %s\n", op.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n",
				format.DimStyle.Render("Run: slagctl operation watch "+op.ID.String()))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation prompt")
	// --allow-self is the INV-2 self-power override, valid only on cycle/on
	// (a single-worker site deliberately cycling its own feed). Not registered
	// on off — the flag is meaningless there (INV-1), and the RunE guard above
	// double-checks in case a caller wires it differently.
	if verb == "cycle" || verb == "on" {
		cmd.Flags().BoolVar(&allowSelf, "allow-self", false,
			"allow cycling an outlet that feeds the serving worker itself (single-worker site; the worker briefly drops itself)")
	}
	return cmd
}
