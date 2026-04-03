package commands

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newVolumesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "volumes",
		Short: "Manage persistent volumes on the device",
	}

	cmd.AddCommand(
		newVolumesListCmd(),
		newVolumesRemoveCmd(),
	)
	return cmd
}

func newVolumesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List persistent volumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			if target.Agent == nil {
				return fmt.Errorf("volume management requires a WendyOS device")
			}

			resp, err := target.Agent.ContainerService.ListVolumes(ctx, &agentpb.ListVolumesRequest{})
			if err != nil {
				return fmt.Errorf("listing volumes: %w", err)
			}

			if jsonOutput {
				type jsonVolume struct {
					Name      string   `json:"name"`
					Path      string   `json:"path"`
					SizeBytes int64    `json:"sizeBytes"`
					Size      string   `json:"size"`
					CreatedAt string   `json:"createdAt"`
					UsedBy    []string `json:"usedBy"`
				}

				vols := make([]jsonVolume, len(resp.GetVolumes()))
				for i, v := range resp.GetVolumes() {
					vols[i] = jsonVolume{
						Name:      v.GetName(),
						Path:      v.GetPath(),
						SizeBytes: v.GetSizeBytes(),
						Size:      formatBytes(v.GetSizeBytes()),
						CreatedAt: v.GetCreatedAt(),
						UsedBy:    v.GetUsedBy(),
					}
				}

				data, err := json.MarshalIndent(vols, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			volumes := resp.GetVolumes()
			if len(volumes) == 0 {
				fmt.Println("No persistent volumes found.")
				return nil
			}

			headers := []string{"Name", "Size", "Created", "Used By"}
			var rows [][]string
			for _, v := range volumes {
				usedBy := "-"
				if apps := v.GetUsedBy(); len(apps) > 0 {
					usedBy = ""
					for i, a := range apps {
						if i > 0 {
							usedBy += ", "
						}
						usedBy += a
					}
				}
				rows = append(rows, []string{
					v.GetName(),
					formatBytes(v.GetSizeBytes()),
					v.GetCreatedAt(),
					usedBy,
				})
			}

			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}
}

func newVolumesRemoveCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "remove [name]",
		Short: "Remove a persistent volume",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			if target.Agent == nil {
				return fmt.Errorf("volume management requires a WendyOS device")
			}

			var volumeName string
			if len(args) > 0 {
				volumeName = args[0]
			} else {
				// List volumes and let user pick.
				resp, err := target.Agent.ContainerService.ListVolumes(ctx, &agentpb.ListVolumesRequest{})
				if err != nil {
					return fmt.Errorf("listing volumes: %w", err)
				}

				volumes := resp.GetVolumes()
				if len(volumes) == 0 {
					fmt.Println("No persistent volumes found.")
					return nil
				}

				var items []tui.PickerItem
				for _, v := range volumes {
					desc := formatBytes(v.GetSizeBytes())
					if apps := v.GetUsedBy(); len(apps) > 0 {
						desc += " — used by: "
						for i, a := range apps {
							if i > 0 {
								desc += ", "
							}
							desc += a
						}
					}
					items = append(items, tui.PickerItem{
						Name:        v.GetName(),
						Description: desc,
						Value:       v.GetName(),
					})
				}

				volumeName, err = pickFromItems("Select a volume to remove", items)
				if err != nil {
					return err
				}
			}

			if !force {
				confirmed, err := tui.Confirm(fmt.Sprintf("Remove volume %q? All data will be permanently deleted.", volumeName))
				if err != nil {
					if errors.Is(err, tui.ErrCancelled) {
						return ErrUserCancelled
					}
					return err
				}
				if !confirmed {
					fmt.Println("Cancelled.")
					return nil
				}
			}

			_, err = target.Agent.ContainerService.RemoveVolume(ctx, &agentpb.RemoveVolumeRequest{
				Name: volumeName,
			})
			if err != nil {
				return fmt.Errorf("removing volume: %w", err)
			}

			fmt.Printf("Volume %q removed.\n", volumeName)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")
	return cmd
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
