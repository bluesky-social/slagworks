package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
	"github.com/bluesky-social/slagworks/internal/cli/cliupdate"
)

// ErrSilent signals main to exit non-zero WITHOUT printing the error — for
// commands that already wrote an operator-facing message to stderr and just
// need a non-zero status (e.g. boot-status on an unsupported host).
var ErrSilent = errors.New("")

var (
	apiURL  string
	jsonOut bool
	// apiToken holds the OIDC access token fetched in PersistentPreRunE.
	// Streaming commands (machine logs, machine logs --sol, operation watch)
	// read it directly to build their own authenticated requests outside the
	// shared client.
	apiToken     string
	apiClient    *slclient.Client
	apiClientSet bool // true when injected via SetAPIClient (tests)
)

// NewRootCmd constructs the root slagctl command with all subcommands attached.
func NewRootCmd() *cobra.Command {
	// Reset injection state for each new root (important for tests).
	apiClientSet = false
	root := &cobra.Command{
		Use:          "slagctl",
		Short:        "Operator CLI for the slagworks bare-metal provisioning system",
		SilenceUsage: true,
		// main.go prints the returned error; without this cobra prints
		// it too and every failure shows up twice.
		SilenceErrors: true,
		// Version-string is what cobra renders when --version is
		// passed. Cobra also prints this for `slagctl version`-style
		// behaviour; we additionally register an explicit `version`
		// subcommand below that fetches the server version too.
		Version: clientVersion,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Major-version gate first: don't fetch a token for a binary we're
			// about to refuse. EnforceMajorGate exempts version/help/completion
			// (and the hidden helper); the auth/doctor exemptions below layer
			// on top for the token fetch specifically.
			if err := cliupdate.NewHook(gateConfig()).EnforceMajorGate(cmd); err != nil {
				return err
			}
			// A client injected for testing short-circuits all auth.
			if apiClientSet {
				return nil
			}
			// `auth logout` and `auth whoami` only touch the cached token.
			if isLocalAuthCommand(cmd) {
				return nil
			}
			resolved, err := resolveAPIURL(apiURL)
			if err != nil {
				// doctor reports a missing server as a failed check.
				if isDoctorCommand(cmd) {
					return nil
				}
				return err
			}
			apiURL = resolved
			// The `auth` subcommands manage the token themselves and must
			// NOT require a valid one to run (you run `auth login` precisely
			// because you have none). Skip the bearer fetch for that tree.
			// The `doctor` command does its own token check as one of its
			// diagnostic steps; it must also run without a valid token.
			if isAuthCommand(cmd) || isDoctorCommand(cmd) {
				return nil
			}
			// Operator auth is OIDC-only: obtain a valid access token
			// (cached, silently refreshed, or via interactive login).
			cfg, err := authConfig(apiURL)
			if err != nil {
				return err
			}
			token, err := clientauth.Bearer(cmd.Context(), cfg)
			if err != nil {
				return fmt.Errorf("authentication failed: %w\n  run `slagctl auth login` to sign in", err)
			}
			apiToken = token
			apiClient = slclient.New(slclient.Config{
				BaseURL: apiURL,
				Token:   token,
			})
			if w := apiClient.InsecureWarning(); w != "" {
				fmt.Fprintln(os.Stderr, format.WarnStyle.Render("warning: "+w))
			}
			return nil
		},
	}

	root.PersistentFlags().StringVar(&apiURL, "api", "", "API server URL (default: $SLAGWORKS_API, then the server saved by `slagctl auth login`)")
	root.PersistentFlags().BoolVar(&jsonOut, "json", false, "output as JSON")

	root.AddCommand(
		newSiteCmd(),
		newMachineCmd(),
		newPowerCmd(),
		newOperationCmd(),
		newImageCmd(),
		newStorageScriptCmd(),
		newNetworkScriptCmd(),
		newSSHKeyCmd(),
		newWorkerCmd(),
		newDHCPCmd(),
		newNetboxCmd(),
		newAuthCmd(),
		newVersionCmd(),
		newDoctorCmd(),
	)
	return root
}

// isAuthCommand reports whether cmd is the `auth` command or one of its
// subcommands, walking up the parent chain. Those commands manage the token
// themselves and must run without a valid one.
func isAuthCommand(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "auth" {
			return true
		}
	}
	return false
}

// isLocalAuthCommand reports whether cmd runs without any server: help and
// shell completion, the bare `auth` group, and `auth logout` / `auth whoami`,
// which only touch the cached token.
func isLocalAuthCommand(cmd *cobra.Command) bool {
	switch cmd.Name() {
	case "help", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return true
	case "auth":
		return true
	case "logout", "whoami":
		return cmd.Parent() != nil && cmd.Parent().Name() == "auth"
	}
	for c := cmd.Parent(); c != nil; c = c.Parent() {
		if c.Name() == "completion" {
			return true
		}
	}
	return false
}

// isDoctorCommand reports whether cmd is the `doctor` command. Doctor
// does its own token check as a diagnostic step and must run without
// a valid token so it can surface a clear Fail rather than an opaque error.
func isDoctorCommand(cmd *cobra.Command) bool {
	return cmd.Name() == "doctor"
}

// SetAPIClient injects a pre-built client — used by tests to avoid real HTTP calls.
func SetAPIClient(_ *cobra.Command, c *slclient.Client) {
	apiClient = c
	apiClientSet = true
}

// ctx returns a context from the cobra command.
func ctx(cmd *cobra.Command) context.Context {
	return cmd.Context()
}
