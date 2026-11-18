package cmd

import (
	"fmt"
	"net/url"
	"os"

	"charm.land/huh/v2"
	"github.com/aymanbagabas/go-udiff"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/snapshot"
)

func newNetworkScriptCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "network-script", Short: "manage host network-config scripts"}
	cmd.AddCommand(networkScriptListCmd(), networkScriptUploadCmd(), networkScriptUpdateCmd(),
		networkScriptHistoryCmd(), networkScriptDeleteCmd())
	return cmd
}

func networkScriptListCmd() *cobra.Command {
	return watchable(&cobra.Command{
		Use:   "list",
		Short: "list network-config scripts",
		RunE: func(cmd *cobra.Command, args []string) error {
			var scripts []model.NetworkScript
			if err := apiClient.Get(ctx(cmd), "/api/v1/network-scripts", nil, &scripts); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), scripts)
			}
			rows := make([][]string, len(scripts))
			for i, s := range scripts {
				rows[i] = []string{
					s.ID.String(),
					s.Name,
					fmt.Sprint(s.Priority),
					format.HumanDuration(s.CreatedAt),
				}
			}
			format.PrintTable(cmd.OutOrStdout(),
				[]string{"ID", "NAME", "PRIORITY", "CREATED"},
				rows)
			return nil
		},
	})
}

func networkScriptUploadCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "upload <file>",
		Short: "upload a network-config script from a file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			content, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}
			body := map[string]any{
				"name":    name,
				"content": string(content),
			}
			var script model.NetworkScript
			if err := apiClient.Post(ctx(cmd), "/api/v1/network-scripts", body, &script); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), script)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Uploaded network script %q (id=%s, priority=%d)\n",
				script.Name, script.ID, script.Priority)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "script name (required)")
	cmd.MarkFlagRequired("name")
	return cmd
}

func networkScriptUpdateCmd() *cobra.Command {
	var name string
	var yes bool
	cmd := &cobra.Command{
		Use:   "update <file>",
		Short: "mint a new version of a network-config script's content, by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			newContent, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}

			var scripts []model.NetworkScript
			if err := apiClient.Get(ctx(cmd), "/api/v1/network-scripts", nil, &scripts); err != nil {
				return err
			}
			current, ok := findNetworkScript(scripts, name)
			if !ok {
				return fmt.Errorf("network script %q not found", name)
			}

			parsed, err := snapshot.ParseNetworkScript(args[0], newContent)
			if err != nil {
				return fmt.Errorf("parse script headers: %w", err)
			}

			if current.Content == string(newContent) {
				if jsonOut {
					return format.PrintJSON(cmd.OutOrStdout(), map[string]any{"changed": false})
				}
				fmt.Fprintln(cmd.OutOrStdout(), "no changes")
				return nil
			}

			if !jsonOut {
				diff := udiff.Unified(
					fmt.Sprintf("%s (remote, id=%s, priority=%d)", name, current.ID, current.Priority),
					fmt.Sprintf("%s (local, priority=%d)", args[0], parsed.Priority),
					current.Content, string(newContent))
				fmt.Fprint(cmd.OutOrStdout(), diff)
				if parsed.Priority != current.Priority {
					fmt.Fprintf(cmd.OutOrStdout(), "priority: %d -> %d\n", current.Priority, parsed.Priority)
				}
			}

			if !yes {
				if jsonOut {
					return fmt.Errorf("refusing interactive confirmation with --json; pass --yes")
				}
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("refusing to update without confirmation; pass --yes")
				}
				var confirmed bool
				if err := huh.NewConfirm().
					Title(fmt.Sprintf("Apply this update to network script %q?", name)).
					Value(&confirmed).
					Run(); err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			body := map[string]any{
				"content": string(newContent),
			}
			var script networkScriptWithVersion
			if err := apiClient.Put(ctx(cmd), "/api/v1/network-scripts/"+url.PathEscape(name), body, &script); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), script)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated network script %s to version %d (id=%s, priority=%d)\n",
				script.Name, script.Version, script.ID, script.Priority)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "script name to update (required)")
	cmd.MarkFlagRequired("name")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")
	return cmd
}

func networkScriptHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history <name>",
		Short: "list every version of a network-config script",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var versions []model.ScriptVersion
			if err := apiClient.Get(ctx(cmd), "/api/v1/network-scripts/"+url.PathEscape(args[0])+"/history", nil, &versions); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), versions)
			}
			printScriptHistory(cmd, versions)
			return nil
		},
	}
	return cmd
}

func networkScriptDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "delete a network-config script by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := apiClient.Delete(ctx(cmd), "/api/v1/network-scripts/"+args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted network script %s\n", args[0])
			return nil
		},
	}
}
