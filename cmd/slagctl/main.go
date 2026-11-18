package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/cli/clierr"
)

// version is set at build time via -ldflags "-X main.version=<git-sha>"
// (see scripts/build-deb.sh). Defaults to "dev" for `go run` / local
// `go build` invocations.
var version = "dev"

func main() {
	// Commands that log through slog (image sync) write key=value text to
	// stderr, keeping stdout for command output.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cmd.SetClientVersion(version)
	root, finish := cmd.NewRootCmdWithFinish()
	runErr := root.Execute()
	finish()
	if runErr != nil {
		// ErrSilent: the command already printed its own operator-facing
		// message; just exit non-zero without a second (empty) line.
		if !errors.Is(runErr, cmd.ErrSilent) {
			fmt.Fprintln(os.Stderr, runErr)
		}
		code, _ := clierr.Code(runErr)
		os.Exit(code)
	}
}
