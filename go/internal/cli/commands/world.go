//go:build !windows

package commands

import (
	"github.com/spf13/cobra"
	worldcommand "github.com/wendylabsinc/wendy/go/world/command"
)

func newWorldCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "world <command> [arguments]",
		Short: "Manage World observations, evidence and reviewed missions",
		Long:  worldcommand.Help,
		// Preserve the existing World parser and exact signed-file contract.
		// In particular, endpoint and credentials flags belong to World, not
		// to implicit device selection in the enclosing CLI.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return worldcommand.Run(cmd.Context(), args, worldcommand.Streams{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
		},
	}
}
