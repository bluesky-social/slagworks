package cmd

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/api"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func newDHCPCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dhcp", Short: "inspect site-worker DHCP state"}
	cmd.AddCommand(dhcpLeasesCmd())
	cmd.AddCommand(dhcpHistoryCmd())
	return cmd
}

// dhcpLeaseRow flattens one worker's lease into a table row, carrying
// the worker attribution alongside it so leases from every worker in
// an HA cage sort and print together.
type dhcpLeaseRow struct {
	worker string // fqdn, falling back to worker_id
	lease  ws.DHCPLeaseInfo
}

func dhcpLeasesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leases <slug>",
		Short: "show DHCP leases currently held by a site's worker(s)",
		Long: `Fans out to every connected site worker for <slug> and shows the
leases each currently holds. An HA cage's two workers each own their
own dynamic slice, so both matter — a worker that errors or times out
is reported as a warning, not a fatal error, so the other worker's
leases still print.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			var resp api.DHCPLeasesResponse
			if err := apiClient.Get(ctx(cmd), "/api/v1/site-configs/"+slug+"/dhcp-leases", nil, &resp); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), resp)
			}

			out := cmd.OutOrStdout()
			var rows []dhcpLeaseRow
			for _, w := range resp.Workers {
				label := w.FQDN
				if label == "" {
					label = w.WorkerID
				}
				if w.Error != "" {
					fmt.Fprintf(out, "%s\n", format.WarnStyle.Render(fmt.Sprintf("warning: worker %s: %s", label, w.Error)))
				}
				for _, l := range w.Leases {
					rows = append(rows, dhcpLeaseRow{worker: label, lease: l})
				}
			}

			sort.Slice(rows, func(i, j int) bool {
				return ipLess(rows[i].lease.IP, rows[j].lease.IP)
			})

			if len(resp.Workers) == 0 {
				fmt.Fprintln(out, "no site workers connected for", slug)
				return nil
			}

			tableRows := make([][]string, len(rows))
			for i, r := range rows {
				tableRows[i] = []string{
					r.lease.IP,
					r.lease.MAC,
					r.lease.Kind,
					dhcpLeaseMachineLabel(r.lease),
					r.lease.Pool,
					r.worker,
					dhcpLeaseExpiryLabel(r.lease.ExpiresAt),
				}
			}
			format.PrintTable(out,
				[]string{"IP", "MAC", "KIND", "MACHINE", "POOL", "WORKER", "EXPIRES"},
				tableRows)
			return nil
		},
	}
	return watchable(cmd)
}

// dhcpLeaseMachineLabel prefers the hostname, falls back to a
// shortened machine UUID, and finally "-" for a MAC that resolved to
// no known machine (legal on a promiscuous OOB segment).
func dhcpLeaseMachineLabel(l ws.DHCPLeaseInfo) string {
	if l.Hostname != "" {
		return l.Hostname
	}
	if l.MachineID != "" {
		return shortID(l.MachineID)
	}
	return "-"
}

// dhcpLeaseExpiryLabel renders a relative "expires in Xm" / "expired
// Xm ago" string. ShortDuration gives the compact no-suffix form;
// wrapped here since none of the existing format helpers render a
// FUTURE time (HumanDuration is past-only "Xm ago").
func dhcpLeaseExpiryLabel(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return fmt.Sprintf("expired %s ago", format.ShortDuration(-d))
	}
	return fmt.Sprintf("in %s", format.ShortDuration(d))
}

// ipLess orders two dotted-quad IPv4 strings numerically (not
// lexically — "10.99.1.9" must sort before "10.99.1.100"). Falls back
// to a plain string compare for anything that doesn't parse as IPv4
// (defensive; the server always sends valid dotted-quads).
func ipLess(a, b string) bool {
	ipA := net.ParseIP(a).To4()
	ipB := net.ParseIP(b).To4()
	if ipA == nil || ipB == nil {
		return a < b
	}
	for i := range ipA {
		if ipA[i] != ipB[i] {
			return ipA[i] < ipB[i]
		}
	}
	return false
}

func dhcpHistoryCmd() *cobra.Command {
	var macFilter, ipFilter string
	var limit int
	cmd := &cobra.Command{
		Use:   "history <slug>",
		Short: "show DHCP lease history for a site, from the global ledger",
		Long: `Reads the global lease-history ledger (Phase 3): every lease
allocate/renew/delete a site worker has ever reported, folded into
open (currently held) and closed (superseded or deleted) rows. Unlike
"dhcp leases" this never touches a live worker — it answers from
Postgres even when every site worker is offline.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			q := url.Values{}
			if macFilter != "" {
				q.Set("mac", macFilter)
			}
			if ipFilter != "" {
				q.Set("ip", ipFilter)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			var resp api.DHCPLeaseHistoryResponse
			if err := apiClient.Get(ctx(cmd), "/api/v1/site-configs/"+slug+"/dhcp-lease-history", q, &resp); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), resp)
			}

			out := cmd.OutOrStdout()
			if len(resp.Entries) == 0 {
				fmt.Fprintln(out, "no lease history for", slug)
				return nil
			}

			rows := make([][]string, len(resp.Entries))
			for i, e := range resp.Entries {
				rows[i] = []string{
					e.IP,
					e.MAC,
					e.Kind,
					dhcpHistoryMachineLabel(e),
					e.Pool,
					e.WorkerID,
					format.HumanDuration(e.FirstSeen),
					format.HumanDuration(e.LastSeen),
					e.Status(),
				}
			}
			format.PrintTable(out,
				[]string{"IP", "MAC", "KIND", "MACHINE", "POOL", "WORKER", "FIRST_SEEN", "LAST_SEEN", "STATUS"},
				rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&macFilter, "mac", "", "filter by MAC address")
	cmd.Flags().StringVar(&ipFilter, "ip", "", "filter by IP address")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows to return (server default 100, cap 1000)")
	return watchable(cmd)
}

// dhcpHistoryMachineLabel falls back to a shortened machine UUID, and
// finally "-" for a MAC that resolved to no known machine (legal on a
// promiscuous OOB segment). The ledger carries no hostname (unlike
// the live dhcp_leases RPC's DHCPLeaseInfo), so there's no
// hostname-preferred branch here.
func dhcpHistoryMachineLabel(e model.DHCPLeaseLedgerEntry) string {
	if e.MachineID != nil {
		return shortID(e.MachineID.String())
	}
	return "-"
}
