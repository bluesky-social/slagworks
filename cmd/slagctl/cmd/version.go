package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
)

// clientVersion is set by main.go's SetClientVersion at process start
// from main.version (build-time -X linker flag). Local "go run" hits
// the "dev" default.
var clientVersion = "dev"

// SetClientVersion is called from main() before Execute(); keeps the
// linker-flag-fed version pinned in this package.
func SetClientVersion(v string) {
	if v != "" {
		clientVersion = v
	}
}

// readyzResponse mirrors the shape produced by api.RegisterHealthz —
// a minimal subset (we only need `version`).
type readyzResponse struct {
	Ready      bool   `json:"ready"`
	Version    string `json:"version"`
	InstanceID string `json:"instance_id"`
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print client and server (global API) versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// Server lookup is best-effort: if global is unreachable,
			// print "(unreachable: <err>)" rather than failing — the
			// client side is still useful info.
			var server readyzResponse
			serverErr := apiClient.Get(ctx(cmd), "/readyz", nil, &server)
			if jsonOut {
				return format.PrintJSON(out, map[string]any{
					"client": clientVersion,
					"server": map[string]any{
						"version":     server.Version,
						"instance_id": server.InstanceID,
						"reachable":   serverErr == nil,
						"error":       errString(serverErr),
					},
					"api": apiURL,
				})
			}
			fmt.Fprintf(out, "client: %s\n", clientVersion)
			if serverErr != nil {
				fmt.Fprintf(out, "server: (unreachable: %v)\n", serverErr)
				fmt.Fprintf(out, "api:    %s\n", apiURL)
				return nil
			}
			fmt.Fprintf(out, "server: %s\n", server.Version)
			if server.InstanceID != "" {
				fmt.Fprintf(out, "        instance_id=%s\n", server.InstanceID)
			}
			fmt.Fprintf(out, "api:    %s\n", apiURL)
			return nil
		},
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
