package cmd

import (
	"crypto/sha256"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/model"
)

// scriptWithVersion mirrors internal/api's update-response shape: the head
// read-model row plus the version number the update just minted. Defined
// here (not imported) since internal/api is not a dependency of the CLI.
type scriptWithVersion struct {
	model.StorageScript
	Version int `json:"version"`
}

// networkScriptWithVersion is the network-script twin of scriptWithVersion.
type networkScriptWithVersion struct {
	model.NetworkScript
	Version int `json:"version"`
}

func findStorageScript(scripts []model.StorageScript, name string) (model.StorageScript, bool) {
	for _, s := range scripts {
		if s.Name == name {
			return s, true
		}
	}
	return model.StorageScript{}, false
}

func findNetworkScript(scripts []model.NetworkScript, name string) (model.NetworkScript, bool) {
	for _, s := range scripts {
		if s.Name == name {
			return s, true
		}
	}
	return model.NetworkScript{}, false
}

// printScriptHistory renders a version history table, oldest first. Content
// is deliberately omitted from table output (--json carries it in full) —
// history is meant to be scanned, and script bodies can be arbitrarily long.
func printScriptHistory(cmd *cobra.Command, versions []model.ScriptVersion) {
	rows := make([][]string, len(versions))
	for i, v := range versions {
		head := ""
		if v.Head {
			head = "*"
		}
		sum := sha256.Sum256([]byte(v.Content))
		rows[i] = []string{
			fmt.Sprint(v.Version),
			head,
			fmt.Sprint(v.Priority),
			format.FormatBytes(int64(len(v.Content))),
			fmt.Sprintf("%x", sum[:8]),
			format.HumanDuration(v.CreatedAt),
		}
	}
	format.PrintTable(cmd.OutOrStdout(),
		[]string{"VERSION", "HEAD", "PRIORITY", "SIZE", "SHA256", "CREATED"},
		rows)
}
