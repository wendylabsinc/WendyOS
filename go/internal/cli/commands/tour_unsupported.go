//go:build !darwin && !linux && !windows

package commands

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newTourCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tour",
		Short: "Interactive guided setup tour for new users",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("The Wendy tour is available on macOS, Linux, and Windows.")
			fmt.Println("Visit https://wendy.sh/docs to get started.")
			return nil
		},
	}
}
