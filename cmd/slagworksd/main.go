package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is set at build time via -ldflags "-X main.version=<git-sha>".
// Reported on WS hello; site workers compare against their own and
// auto-upgrade when SLAGWORKS_AUTO_UPGRADE=1.
var version = "dev"

// packageVersion is the deb package version scripts/build-deb.sh builds
// this binary as, set via -ldflags "-X main.packageVersion=<deb-version>"
// and empty for any other build. Global sends it to site workers next to
// version so their auto-upgrade installs exactly this package build.
var packageVersion string

func main() {
	root := &cobra.Command{
		Use:   "slagworksd",
		Short: "Slagworks bare-metal provisioning daemon",
	}

	root.AddCommand(globalCmd(), siteCmd(), versionCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(exitCodeFor(err))
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build SHA and exit",
		Run: func(*cobra.Command, []string) {
			fmt.Println(version)
		},
	}
}
