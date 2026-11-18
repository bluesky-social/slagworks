package cmd

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/internal/cli/cliupdate"
)

var updateCfg = cliupdate.Config{
	Owner: "bluesky-social",
	Repo:  "slagworks",
	Tool:  "slagctl",
	// Releases are public: the check needs no GitHub token and never nudges
	// for one.
	Visibility: cliupdate.RepoPublic,
	// Releases publish a cask to the public tap (.goreleaser.yaml).
	UpgradeCommand: "brew upgrade slagctl",
	// CurrentVersion is filled by gateConfig using clientVersion, which
	// main.go sets via SetClientVersion before building the root command.
}

// updateFailClosed is stamped via ldflags at release build time to flip the
// major-version gate to fail-closed. Default "false" = fail open.
var updateFailClosed = "false"

// gateConfig returns the cliupdate.Config for the startup major-version gate,
// with the current client version and fail-open/closed posture resolved. Used
// by NewRootCmd's PersistentPreRunE, doctor, and NewRootCmdWithFinish.
func gateConfig() cliupdate.Config {
	cfg := updateCfg
	cfg.CurrentVersion = clientVersion
	cfg.FailClosedOnUnknown = updateFailClosed == "true"
	return cfg
}

// NewRootCmdWithFinish returns the root cobra command with the hidden update
// helper registered, and a finish function main() calls after Execute()
// returns: it prints any pending upgrade notice and spawns the background
// release check when one is due.
//
// Use this in main; use NewRootCmd for tests and docgen (no background check).
func NewRootCmdWithFinish() (*cobra.Command, func()) {
	root := NewRootCmd()
	cfg := gateConfig()
	root.AddCommand(cliupdate.NewHelperCommand(cfg))

	finish := func() {
		hook := cliupdate.NewHook(cfg)
		hook.PrintPendingNotice(os.Stderr)
		hook.MaybeSpawnChild(os.Args[0])
	}
	return root, finish
}
