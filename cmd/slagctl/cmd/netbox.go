package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/api"
)

func newNetboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "netbox",
		Short: "NetBox integration commands",
	}
	cmd.AddCommand(netboxBootstrapCmd())
	cmd.AddCommand(netboxMigrateChassisSerialCmd())
	cmd.AddCommand(netboxDiffCmd())
	return cmd
}

func netboxDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "compare a machine's persisted Slagworks bindings with live NetBox",
		Long: strings.TrimSpace(`
Compares the independently persisted machine UUID, NetBox device ID, and
serial. Hostname, site, BMC IP, MACs, lifecycle state, commission time, and
snapshot sequence are included as one-sided context because those values do
not exist independently on both sides.

Exits non-zero for a mismatched binding or a missing side, after printing the
complete human or JSON report.`),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			var diff api.NetBoxDiff
			if err := apiClient.Get(ctx(cmd), "/api/v1/machines/"+r.Slot()+"/netbox-diff", nil, &diff); err != nil {
				return err
			}
			if jsonOut {
				if err := format.PrintJSON(cmd.OutOrStdout(), diff); err != nil {
					return err
				}
				if !diff.Matched {
					return ErrSilent
				}
				return nil
			}

			rows := make([][]string, 0, len(diff.Fields))
			for _, field := range diff.Fields {
				status := format.DimStyle.Render("info")
				if field.Comparable && field.Match != nil {
					if *field.Match {
						status = format.SuccessStyle.Render("match")
					} else {
						status = format.StateFailed.Render("MISMATCH")
					}
				}
				rows = append(rows, []string{field.Name, field.Slagworks, field.NetBox, status})
			}
			format.PrintTable(cmd.OutOrStdout(),
				[]string{"FIELD", "SLAGWORKS", "NETBOX", "RESULT"}, rows)
			if diff.Message != "" {
				fmt.Fprintln(cmd.OutOrStdout(), format.WarnStyle.Render("warning: "+diff.Message))
			}
			if diff.Matched {
				fmt.Fprintln(cmd.OutOrStdout(), format.SuccessStyle.Render("NetBox bindings match."))
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), format.StateFailed.Render("NetBox binding drift detected."))
			return ErrSilent
		},
	}
	bindMachineArg(cmd)
	return cmd
}

func netboxBootstrapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap",
		Short: "ensure required NetBox objects exist (custom fields, webhook, event rules)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resp map[string]any
			if err := apiClient.Post(ctx(cmd), "/api/v1/admin/netbox/bootstrap", map[string]any{}, &resp); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), resp)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Bootstrap complete:")
			if fields, ok := resp["custom_fields"].([]any); ok {
				names := make([]string, len(fields))
				for i, f := range fields {
					names[i] = fmt.Sprint(f)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  Custom fields: %s\n", strings.Join(names, ", "))
			}
			if webhook, ok := resp["webhook"].(string); ok && webhook != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "  Webhook: %s\n", webhook)
			}
			if rules, ok := resp["event_rules"].([]any); ok && len(rules) > 0 {
				names := make([]string, len(rules))
				for i, r := range rules {
					names[i] = fmt.Sprint(r)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  Event rules: %s\n", strings.Join(names, ", "))
			}
			return nil
		},
	}
}

// netboxMigrateChassisSerialCmd reconciles the legacy `chassis_serial`
// custom field with NetBox's built-in `serial` field. Defaults to
// dry-run; pass --apply to write.
func netboxMigrateChassisSerialCmd() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "migrate-chassis-serial",
		Short: "move chassis_serial custom field values to the built-in serial field (dry-run by default)",
		Long: strings.TrimSpace(`
Walks every slagworks-managed device and reconciles the legacy
chassis_serial custom field with the built-in serial field:

  - cf empty / missing                                     -> skip
  - cf set to a DMI placeholder, built-in empty            -> clear cf only
  - cf set, built-in serial empty                          -> write cf -> serial, clear cf
  - cf set, built-in serial == cf                          -> clear cf only
  - cf set, built-in serial != cf, both non-empty          -> conflict, leave untouched

Conflicts are reported for operator triage. Idempotent.

Run order: this should be invoked AFTER the agent rollout that stops
writing chassis_serial. Running it earlier means a re-commission can
re-populate the cf after migration clears it, and divergence between
new chassis serials and old cf values will surface as conflicts on
the next migration run.
`),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"dry_run": !apply}
			var resp map[string]any
			if err := apiClient.Post(ctx(cmd), "/api/v1/admin/netbox/migrate-chassis-serial", body, &resp); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), resp)
			}
			out := cmd.OutOrStdout()
			mode := "DRY RUN"
			if apply {
				mode = "APPLIED"
			}
			fmt.Fprintf(out, "Chassis serial migration (%s):\n", mode)
			if v, ok := resp["examined"].(float64); ok {
				fmt.Fprintf(out, "  Examined: %d\n", int(v))
			}
			if v, ok := resp["skipped"].(float64); ok {
				fmt.Fprintf(out, "  Skipped (no cf):  %d\n", int(v))
			}
			migrated, _ := resp["migrated"].([]any)
			conflicts, _ := resp["conflicts"].([]any)
			fmt.Fprintf(out, "  Migrated:         %d\n", len(migrated))
			fmt.Fprintf(out, "  Conflicts:        %d\n", len(conflicts))
			if len(conflicts) > 0 {
				fmt.Fprintln(out, "\nConflicts (operator review needed):")
				for _, c := range conflicts {
					m, _ := c.(map[string]any)
					fmt.Fprintf(out, "  - device %v %q: built-in=%q chassis_cf=%q\n",
						m["id"], m["name"], m["built_in_serial"], m["chassis_cf_serial"])
				}
			}
			if !apply {
				fmt.Fprintln(out, "\nRerun with --apply to write changes.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write changes (default is dry-run)")
	return cmd
}
