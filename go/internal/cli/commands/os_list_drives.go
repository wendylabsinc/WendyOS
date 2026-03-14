package commands

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newOSListDrivesCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "list-drives",
		Short: "List available drives",
		RunE: func(cmd *cobra.Command, args []string) error {
			var drives []drive
			var err error

			if all {
				drives, err = listAllDrives()
			} else {
				drives, err = listExternalDrives()
			}
			if err != nil {
				return err
			}

			if jsonOutput {
				type jsonDrive struct {
					ID         string `json:"id"`
					Name       string `json:"name"`
					Capacity   int64  `json:"capacity"`
					IsExternal bool   `json:"isExternal"`
				}

				out := make([]jsonDrive, len(drives))
				for i, d := range drives {
					out[i] = jsonDrive{
						ID:         d.DevicePath,
						Name:       d.Name,
						Capacity:   d.SizeBytes,
						IsExternal: d.IsRemovable,
					}
				}

				enc := json.NewEncoder(os.Stdout)
				return enc.Encode(out)
			}

			if len(drives) == 0 {
				fmt.Println("No drives found.")
				return nil
			}

			for _, d := range drives {
				ext := ""
				if d.IsRemovable {
					ext = " (external)"
				}
				fmt.Printf("  %s  %s  %s%s\n", d.DevicePath, d.Name, d.Size, ext)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&all, "all", false, "List all drives, not just external/removable")

	return cmd
}
