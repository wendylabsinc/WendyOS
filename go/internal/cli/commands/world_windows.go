package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newWorldCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "world <command> [arguments]",
		Short:              "World operations require Linux or macOS",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("World operations require Linux or macOS; other Wendy commands remain available on Windows")
		},
	}
}
