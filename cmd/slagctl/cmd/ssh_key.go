package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/format"
	"github.com/bluesky-social/slagworks/internal/model"
)

func newSSHKeyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ssh-key", Short: "manage global SSH authorized keys"}
	cmd.AddCommand(sshKeyListCmd(), sshKeyAddCmd(), sshKeyRemoveCmd())
	return cmd
}

func sshKeyListCmd() *cobra.Command {
	return watchable(&cobra.Command{
		Use:   "list",
		Short: "list SSH keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			var keys []model.SSHKey
			if err := apiClient.Get(ctx(cmd), "/api/v1/ssh-keys", nil, &keys); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), keys)
			}
			rows := make([][]string, len(keys))
			for i, k := range keys {
				short := k.PublicKey
				if len(short) > 50 {
					short = short[:50] + "..."
				}
				rows[i] = []string{
					k.ID.String(),
					short,
					k.Comment,
					format.HumanDuration(k.CreatedAt),
				}
			}
			format.PrintTable(cmd.OutOrStdout(),
				[]string{"ID", "PUBLIC KEY", "COMMENT", "CREATED"},
				rows)
			return nil
		},
	})
}

func sshKeyAddCmd() *cobra.Command {
	var comment string
	cmd := &cobra.Command{
		Use:   "add <public-key>",
		Short: "add an SSH public key",
		Long:  "Add an SSH public key. Pass the full key as a single argument (quote it).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pubkey := strings.TrimSpace(args[0])
			body := map[string]string{
				"public_key": pubkey,
				"comment":    comment,
			}
			var key model.SSHKey
			if err := apiClient.Post(ctx(cmd), "/api/v1/ssh-keys", body, &key); err != nil {
				return err
			}
			if jsonOut {
				return format.PrintJSON(cmd.OutOrStdout(), key)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added SSH key %s\n", key.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "optional label for the key")
	return cmd
}

func sshKeyRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "remove an SSH key by ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := apiClient.Delete(ctx(cmd), "/api/v1/ssh-keys/"+args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed SSH key %s\n", args[0])
			return nil
		},
	}
}
