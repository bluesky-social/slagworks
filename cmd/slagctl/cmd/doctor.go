package cmd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/cli/clicheck"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
	"github.com/bluesky-social/slagworks/internal/cli/clierr"
)

func newDoctorCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:           "doctor",
		Short:         "Check binary version, credentials, and connectivity",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := append(
				clicheck.UpdateChecks(gateConfig()),
				slagctlTokenCheck(),
				slagctlConnectivityCheck(),
			)
			ok := clicheck.RunChecks(cmd.Context(), cmd.OutOrStdout(), checks, asJSON)
			if !ok {
				return clierr.WithExit(fmt.Errorf("one or more checks failed"), 1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit results as JSON array instead of table")
	return cmd
}

// slagctlTokenCheck verifies that an OIDC token is cached and not expired.
// Missing or expired token = Fail with a "run slagctl auth login" nudge.
func slagctlTokenCheck() clicheck.Check {
	return clicheck.Check{
		Name: "slagctl-token",
		Run: func(_ context.Context) clicheck.Result {
			id, err := clientauth.CachedIdentity(tokenCacheConfig())
			if err != nil {
				return clicheck.Fail("run `slagctl auth login`")
			}
			if id.Expiry > 0 && time.Now().After(time.Unix(id.Expiry, 0)) {
				return clicheck.Fail("token expired — run `slagctl auth login`")
			}
			if id.PreferredUsername != "" {
				return clicheck.OK(id.PreferredUsername)
			}
			return clicheck.OK(id.Subject)
		},
	}
}

// slagctlConnectivityCheck makes a cheap GET /readyz against the slagworks
// global API to verify the token is accepted and the server is reachable.
// A missing token surfaces as Fail from slagctlTokenCheck instead.
func slagctlConnectivityCheck() clicheck.Check {
	return clicheck.Check{
		Name: "slagctl-connectivity",
		Run: func(ctx context.Context) clicheck.Result {
			if apiURL == "" {
				return clicheck.Fail("no API server configured — run `slagctl auth login --api <url>`")
			}
			cfg, err := authConfig(apiURL)
			if err != nil {
				return clicheck.Fail(err.Error())
			}
			token, err := clientauth.CachedBearer(ctx, cfg)
			if err != nil {
				return clicheck.Fail("no usable token — run `slagctl auth login`")
			}
			httpClient := &http.Client{Timeout: 5 * time.Second}
			c := slclient.New(slclient.Config{
				BaseURL:    apiURL,
				Token:      token,
				HTTPClient: httpClient,
			})
			var resp struct {
				Ready   bool   `json:"ready"`
				Version string `json:"version"`
			}
			if err := c.Get(ctx, "/readyz", nil, &resp); err != nil {
				return clicheck.Fail(fmt.Sprintf("slagworks API: %v", err))
			}
			if resp.Version != "" {
				return clicheck.OK(fmt.Sprintf("server %s", resp.Version))
			}
			return clicheck.OK("reachable")
		},
	}
}
