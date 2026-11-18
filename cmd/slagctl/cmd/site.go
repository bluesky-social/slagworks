package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/model"
)

// Sites are NetBox-owned. Slagworks keeps a sparse per-site knob
// table (site_configs). The slug is the free-form worker identity (PK);
// netbox_site is the NetBox site mirrored, and netbox_location optionally
// narrows scope to one location within that site (dual-cage support).
// This file exposes that table to operators.
func newSiteCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "site-config", Short: "manage slagworks per-site config"}
	cmd.AddCommand(siteConfigListCmd(), siteConfigCreateCmd(), siteConfigShowCmd(), siteConfigUpdateCmd(), siteConfigDeleteCmd())
	return cmd
}

func siteConfigListCmd() *cobra.Command {
	return watchable(&cobra.Command{
		Use:   "list",
		Short: "list all site configs",
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfgs []model.SiteConfig
			if err := apiClient.Get(ctx(cmd), "/api/v1/site-configs", nil, &cfgs); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), cfgs)
			}
			rows := make([][]string, len(cfgs))
			for i, c := range cfgs {
				flags := ""
				if c.Promiscuous {
					flags += "promiscuous"
				}
				location := c.NetBoxLocation
				if location == "" {
					location = "(whole site)"
				}
				rows[i] = []string{
					c.Slug,
					c.NetBoxSite,
					location,
					c.EphemeralPoolStart + " - " + c.EphemeralPoolEnd,
					fmt.Sprintf("%d", len(c.RelayPools)),
					fmt.Sprintf("%d", c.LastSnapshotSeq),
					flags,
					format.HumanDuration(c.CreatedAt),
				}
			}
			format.PrintTable(cmd.OutOrStdout(),
				[]string{"SLUG", "NETBOX_SITE", "NETBOX_LOCATION", "EPHEMERAL_POOL", "RELAY_POOLS", "LAST_SEQ", "FLAGS", "CREATED"},
				rows)
			return nil
		},
	})
}

func siteConfigCreateCmd() *cobra.Command {
	var slug, netboxSite, netboxLocation, poolStart, poolEnd string
	var promiscuous bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "create a site config",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default netbox_site to the slug for the whole-site shape
			// (pop1/pop3 style, where slug == NetBox site slug).
			if netboxSite == "" {
				netboxSite = slug
			}
			cfg := model.SiteConfig{
				Slug:               slug,
				NetBoxSite:         netboxSite,
				NetBoxLocation:     netboxLocation,
				EphemeralPoolStart: poolStart,
				EphemeralPoolEnd:   poolEnd,
				Promiscuous:        promiscuous,
			}
			var created model.SiteConfig
			if err := apiClient.Post(ctx(cmd), "/api/v1/site-configs", cfg, &created); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), created)
			}
			scope := created.NetBoxSite
			if created.NetBoxLocation != "" {
				scope += "/" + created.NetBoxLocation
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created site config %s (scope %s, ephemeral pool %s-%s)\n",
				created.Slug, scope, created.EphemeralPoolStart, created.EphemeralPoolEnd)
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "worker identity (PK); defaults netbox-site to this if --netbox-site omitted")
	cmd.Flags().StringVar(&netboxSite, "netbox-site", "", "NetBox site slug to mirror (default: --slug)")
	cmd.Flags().StringVar(&netboxLocation, "netbox-location", "", "NetBox location slug to narrow scope to (optional; empty = whole site)")
	cmd.Flags().StringVar(&poolStart, "pool-start", "", "first IP in ephemeral (PXE) pool")
	cmd.Flags().StringVar(&poolEnd, "pool-end", "", "last IP in ephemeral (PXE) pool")
	cmd.Flags().BoolVar(&promiscuous, "promiscuous", false, "serve DHCP to unknown MACs")
	cmd.MarkFlagRequired("slug")
	cmd.MarkFlagRequired("pool-start")
	cmd.MarkFlagRequired("pool-end")
	return cmd
}

func siteConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [slug]",
		Short: "show site config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var cfg model.SiteConfig
			if err := apiClient.Get(ctx(cmd), "/api/v1/site-configs/"+args[0], nil, &cfg); err != nil {
				return err
			}
			return format.PrintJSON(cmd.OutOrStdout(), cfg)
		},
	}
}

func siteConfigUpdateCmd() *cobra.Command {
	var netboxSite, netboxLocation, poolStart, poolEnd string
	var relayPools []string
	var promiscuous, clearPromiscuous, clearRelayPools, clearLocation bool
	cmd := &cobra.Command{
		Use:   "update [slug]",
		Short: "update a site config",
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if netboxSite != "" {
				body["netbox_site"] = netboxSite
			}
			if clearLocation && netboxLocation != "" {
				return fmt.Errorf("--netbox-location and --clear-location are mutually exclusive")
			}
			if clearLocation {
				body["netbox_location"] = ""
			}
			if netboxLocation != "" {
				body["netbox_location"] = netboxLocation
			}
			if poolStart != "" {
				body["ephemeral_pool_start"] = poolStart
			}
			if poolEnd != "" {
				body["ephemeral_pool_end"] = poolEnd
			}
			if promiscuous {
				body["promiscuous"] = true
			}
			if clearPromiscuous {
				body["promiscuous"] = false
			}
			if clearRelayPools && len(relayPools) > 0 {
				return fmt.Errorf("--relay-pool and --clear-relay-pools are mutually exclusive")
			}
			if clearRelayPools {
				body["relay_pools"] = []model.RelayPool{}
			}
			if len(relayPools) > 0 {
				// Full-replace: the flags given on this invocation become
				// the site's complete relay pool list.
				pools := make([]model.RelayPool, 0, len(relayPools))
				for _, spec := range relayPools {
					p, err := parseRelayPool(spec)
					if err != nil {
						return err
					}
					pools = append(pools, p)
				}
				body["relay_pools"] = pools
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to update")
			}
			var cfg model.SiteConfig
			if err := apiClient.Patch(ctx(cmd), "/api/v1/site-configs/"+args[0], body, &cfg); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), cfg)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated site config %s\n", cfg.Slug)
			return nil
		},
	}
	cmd.Args = cobra.ExactArgs(1)
	cmd.Flags().StringVar(&netboxSite, "netbox-site", "", "change the mirrored NetBox site slug")
	cmd.Flags().StringVar(&netboxLocation, "netbox-location", "", "narrow scope to this NetBox location slug")
	cmd.Flags().BoolVar(&clearLocation, "clear-location", false, "widen scope back to the whole NetBox site")
	cmd.Flags().StringVar(&poolStart, "pool-start", "", "first IP in ephemeral (PXE) pool")
	cmd.Flags().StringVar(&poolEnd, "pool-end", "", "last IP in ephemeral (PXE) pool")
	cmd.Flags().StringArrayVar(&relayPools, "relay-pool", nil,
		"relayed DHCP pool as subnet,start,end,gateway (e.g. 10.254.27.0/24,10.254.27.100,10.254.27.150,10.254.27.1); repeatable, REPLACES the full list")
	cmd.Flags().BoolVar(&clearRelayPools, "clear-relay-pools", false, "remove all relay pools")
	cmd.Flags().BoolVar(&promiscuous, "promiscuous", false, "enable promiscuous DHCP")
	cmd.Flags().BoolVar(&clearPromiscuous, "no-promiscuous", false, "disable promiscuous DHCP")
	return cmd
}

// parseRelayPool parses the --relay-pool flag shape
// "subnet,start,end,gateway" into a model.RelayPool. Field-count and
// per-field validation happen server-side too; this just catches the
// obvious shape mistake with a usable message.
func parseRelayPool(spec string) (model.RelayPool, error) {
	parts := strings.Split(spec, ",")
	if len(parts) != 4 {
		return model.RelayPool{}, fmt.Errorf(
			"--relay-pool %q: want subnet,start,end,gateway (4 comma-separated fields, got %d)", spec, len(parts))
	}
	return model.RelayPool{
		Subnet:     strings.TrimSpace(parts[0]),
		RangeStart: strings.TrimSpace(parts[1]),
		RangeEnd:   strings.TrimSpace(parts[2]),
		Gateway:    strings.TrimSpace(parts[3]),
	}, nil
}

func siteConfigDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete [slug]",
		Short: "delete a site config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := apiClient.Delete(ctx(cmd), "/api/v1/site-configs/"+args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Site config deleted.")
			return nil
		},
	}
}

// knownMachineStates is the exhaustive set of valid machine state values.
// See internal/model/machine.go for the canonical list.
var knownMachineStates = map[string]bool{
	"new": true, "commissioning": true, "commissioned": true,
	"deploying": true, "running": true, "failed": true,
}

func validateStateFlag(state string) error {
	if state != "" && !knownMachineStates[state] {
		return fmt.Errorf("invalid state %q, must be one of: new, commissioning, commissioned, deploying, running, failed", state)
	}
	return nil
}
