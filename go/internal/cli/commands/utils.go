package commands

import "github.com/spf13/cobra"

func newUtilsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "utils",
		Short: "Utility commands",
	}

	cmd.AddCommand(newOpenBrowserCmd())
	return cmd
}
