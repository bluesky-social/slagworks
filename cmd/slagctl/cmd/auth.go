package cmd

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "manage SSO authentication (OIDC)",
	}
	cmd.AddCommand(authLoginCmd(), authLogoutCmd(), authWhoamiCmd())
	return cmd
}

func authLoginCmd() *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "sign in to a slagworks server via SSO",
		Long: `Runs the interactive OIDC login and caches the resulting token.

The server named by --api (or SLAGWORKS_API, or the one saved by the last
login) reports which identity provider and client to use, and a successful
login saves that server as the default for later commands.

By default this uses the loopback flow: a browser opens, you authenticate with
the identity provider, and the code is captured on a local port. Use --device for the
device-authorization flow (prints a URL + code to enter on another device) when
no local browser is available.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			issuer, clientID, err := loginSettings(cmd.Context(), apiURL)
			if err != nil {
				return err
			}
			cfg := clientauth.Config{
				AppName:      appName,
				Issuer:       issuer,
				ClientID:     clientID,
				LoopbackPort: oidcLoopbackPort,
				Device:       device,
			}
			// Force a fresh interactive login (don't reuse cache).
			tok, err := clientauth.Login(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if err := clientauth.WriteCachedToken(cmd.Context(), cfg, tok); err != nil {
				return fmt.Errorf("caching token: %w", err)
			}
			if err := writeSavedConfig(savedConfig{API: apiURL, OIDCIssuer: issuer, OIDCClientID: clientID}); err != nil {
				return fmt.Errorf("saving the server to the slagctl config: %w", err)
			}
			id, _ := clientauth.DecodeIdentity(tok.AccessToken)
			if id != nil && id.PreferredUsername != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Logged in as %s\n", id.PreferredUsername)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Logged in.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "use the device-authorization flow instead of loopback")
	return cmd
}

func authLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "delete the cached SSO token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := clientauth.Logout(cmd.Context(), tokenCacheConfig()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out (cached token removed).")
			return nil
		},
	}
}

func authWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "show the identity in the cached SSO token",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := clientauth.CachedIdentity(tokenCacheConfig())
			if err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), id)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "subject:  %s\n", id.Subject)
			fmt.Fprintf(out, "username: %s\n", id.PreferredUsername)
			fmt.Fprintf(out, "email:    %s\n", id.Email)
			fmt.Fprintf(out, "groups:   %v\n", id.Groups)
			if id.Expiry > 0 {
				exp := time.Unix(id.Expiry, 0)
				fmt.Fprintf(out, "expires:  %s (%s)\n", exp.Format(time.RFC3339), time.Until(exp).Round(time.Second))
			}
			return nil
		},
	}
}
