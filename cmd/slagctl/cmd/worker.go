package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
)

func newWorkerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "worker", Short: "manage site workers"}
	cmd.AddCommand(workerListCmd(), workerRegisterCmd(), workerDeleteCmd())
	return cmd
}

func workerDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <ident>",
		Short: "delete a site worker registration",
		Long: `Deletes a site worker's registration row, by uuid, worker-id, or fqdn.
Refused if the worker is currently connected, or has an in-flight
operation claimed. This is the teardown path for a retired site worker
(e.g. decommissioned scaffolding) — the row is what authenticates the
worker's WS connection to global.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ident := args[0]
			var workers []model.SiteWorker
			if err := apiClient.Get(ctx(cmd), "/api/v1/workers", nil, &workers); err != nil {
				return err
			}
			var matches []model.SiteWorker
			for _, w := range workers {
				if w.ID.String() == ident || w.WorkerID == ident || w.FQDN == ident {
					matches = append(matches, w)
				}
			}
			if len(matches) == 0 {
				return fmt.Errorf("no site worker matches %q", ident)
			}
			if len(matches) > 1 {
				return fmt.Errorf("%q matches %d site workers; use the uuid to disambiguate", ident, len(matches))
			}
			w := matches[0]

			if !yes {
				if jsonOut {
					return fmt.Errorf("refusing interactive confirmation with --json; pass --yes")
				}
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to delete without confirmation; pass --yes")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "worker_id=%s fqdn=%s site=%s last_seen=%s\n",
					w.WorkerID, w.FQDN, w.SiteSlug, lastSeenStatus(w.LastSeenAt))
				var confirmed bool
				if err := huh.NewConfirm().
					Title(fmt.Sprintf("Delete site worker %s/%s?", w.SiteSlug, w.WorkerID)).
					Value(&confirmed).
					Run(); err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			if err := apiClient.Delete(ctx(cmd), "/api/v1/site-workers/"+w.ID.String()); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(),
					map[string]any{"deleted": true, "id": w.ID, "site_slug": w.SiteSlug, "worker_id": w.WorkerID})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted site worker %s/%s\n", w.SiteSlug, w.WorkerID)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}

func workerRegisterCmd() *cobra.Command {
	var slug, workerID, fqdn string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "mint a bearer token for a site worker",
		Long: `Mints (or rotates) a bearer token for a (site_slug, worker_id) pair.
The plaintext token is returned once; hand it to the site worker via
SLAGWORKS_TOKEN. Re-registering the same pair rotates the token.

A worker connected with the token receives the site's device credentials
with every operation it is sent, so a server that sets
SLAGWORKS_OIDC_CREDENTIAL_GROUPS mints tokens only for operators who are
also in one of those groups, as for "slagctl machine creds"; anyone else
gets a 403 naming that setting.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{
				"site_slug": slug,
				"worker_id": workerID,
				"fqdn":      fqdn,
			}
			var resp struct {
				SiteSlug string `json:"site_slug"`
				WorkerID string `json:"worker_id"`
				FQDN     string `json:"fqdn"`
				Token    string `json:"token"`
			}
			if err := apiClient.Post(ctx(cmd), "/api/v1/site-workers", body, &resp); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), resp)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Registered site worker %s/%s\n", resp.SiteSlug, resp.WorkerID)
			fmt.Fprintf(cmd.OutOrStdout(), "%s\n",
				format.SuccessStyle.Render(fmt.Sprintf("Token (save this, it will not be shown again): %s", resp.Token)))
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "site-config slug (see 'slagctl site-config list'; equals the NetBox site slug only for whole-site configs)")
	cmd.Flags().StringVar(&workerID, "worker-id", "", "stable worker identifier")
	cmd.Flags().StringVar(&fqdn, "fqdn", "", "worker host FQDN (optional)")
	cmd.MarkFlagRequired("slug")
	cmd.MarkFlagRequired("worker-id")
	return cmd
}

func workerListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list connected site workers",
		RunE: func(cmd *cobra.Command, args []string) error {
			var workers []api.WorkerListEntry
			if err := apiClient.Get(ctx(cmd), "/api/v1/workers", nil, &workers); err != nil {
				return err
			}
			sort.Slice(workers, func(i, j int) bool {
				a := workers[i].FQDN
				if a == "" {
					a = workers[i].WorkerID
				}
				b := workers[j].FQDN
				if b == "" {
					b = workers[j].WorkerID
				}
				return a < b
			})
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), workers)
			}
			// The SMCIPMITool column only appears when some site has
			// Supermicro hosts; for everyone else the tool is irrelevant.
			showSMC := false
			for _, w := range workers {
				if w.SupermicroMachines > 0 {
					showSMC = true
				}
			}
			headers := []string{"WORKER", "SITE", "FIRST SEEN", "LAST SEEN"}
			if showSMC {
				headers = append(headers, "SMCIPMITOOL")
			}
			rows := make([][]string, len(workers))
			var missing, failing []string
			for i, w := range workers {
				fqdn := w.FQDN
				if fqdn == "" {
					fqdn = w.WorkerID
				}
				rows[i] = []string{
					fqdn,
					w.SiteSlug,
					format.HumanDuration(w.FirstSeenAt),
					lastSeenStatus(w.LastSeenAt),
				}
				if showSMC {
					rows[i] = append(rows[i], smcToolCell(w))
				}
				if w.SupermicroMachines > 0 {
					switch w.SMCIPMITool {
					case smcipmitool.StatusMissing:
						missing = append(missing, fqdn)
					case smcipmitool.StatusMissingRequired:
						failing = append(failing, fqdn)
					}
				}
			}
			format.PrintTable(cmd.OutOrStdout(), headers, rows)
			if len(failing) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"\nwarning: %d worker(s) require SMCIPMITool but don't have it installed (%s); "+
						"Supermicro power operations there will fail. See %s\n",
					len(failing), strings.Join(failing, ", "), smcipmitool.InstallDoc)
			}
			if len(missing) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"\nhint: %d worker(s) manage Supermicro hosts without SMCIPMITool installed (%s); "+
						"they fall back to the generic IPMI path. Install it from Supermicro: see %s\n",
					len(missing), strings.Join(missing, ", "), smcipmitool.InstallDoc)
			}
			return nil
		},
	}
	return watchable(cmd)
}

// smcToolCell renders a worker's SMCIPMITool status for the worker list:
// "-" when the worker is offline (no live status) or its site has no
// Supermicro hosts, so the column only draws attention where it matters.
func smcToolCell(w api.WorkerListEntry) string {
	if w.SMCIPMITool == "" || w.SupermicroMachines == 0 {
		return "-"
	}
	return w.SMCIPMITool
}

func lastSeenStatus(t time.Time) string {
	ago := time.Since(t)
	s := format.HumanDuration(t)
	if ago < 30*time.Second {
		return s + " (online)"
	}
	if ago < 2*time.Minute {
		return s + " (recent)"
	}
	return fmt.Sprintf("%s (offline?)", s)
}
