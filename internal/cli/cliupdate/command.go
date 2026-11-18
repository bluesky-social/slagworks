package cliupdate

import (
	"github.com/spf13/cobra"
)

// NewHelperCommand returns the hidden sub-command the tool registers on its
// root cobra.Command. The command is hidden from help output and is only
// invoked by the detached child process.
//
// Wire it like:
//
//	root.AddCommand(cliupdate.NewHelperCommand(cfg))
func NewHelperCommand(cfg Config) *cobra.Command {
	return &cobra.Command{
		Use:    HelperCommandName,
		Hidden: true,
		// Shadow the root's PersistentPreRunE (cobra runs only the nearest
		// one): a root hook that authenticates to the tool's API would make
		// this detached child start an interactive login, and fail before
		// RunHelper whenever the operator has no session. Release discovery
		// needs only a GitHub token.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			RunHelper(cfg)
			return nil
		},
	}
}
