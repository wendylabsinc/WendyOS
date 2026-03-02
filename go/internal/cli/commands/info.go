package commands

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/version"
)

func newInfoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Display CLI version and system information",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := map[string]string{
				"version":   version.Version,
				"os":        runtime.GOOS,
				"arch":      runtime.GOARCH,
				"goVersion": runtime.Version(),
			}

			if jsonOutput {
				data, err := json.MarshalIndent(info, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Wendy CLI\n")
			fmt.Printf("  Version:    %s\n", info["version"])
			fmt.Printf("  OS:         %s\n", info["os"])
			fmt.Printf("  Arch:       %s\n", info["arch"])
			fmt.Printf("  Go Version: %s\n", info["goVersion"])
			return nil
		},
	}
}
