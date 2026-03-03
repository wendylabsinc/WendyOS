package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newHardwareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hardware",
		Short: "Query hardware capabilities on the target device",
	}

	cmd.AddCommand(newHardwareListCmd())
	return cmd
}

func newHardwareListCmd() *cobra.Command {
	var category string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List hardware capabilities",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			req := &agentpb.ListHardwareCapabilitiesRequest{}
			if category != "" {
				req.CategoryFilter = &category
			}
			resp, err := conn.AgentService.ListHardwareCapabilities(ctx, req)
			if err != nil {
				return fmt.Errorf("listing hardware capabilities: %w", err)
			}

			caps := resp.GetCapabilities()
			if jsonOutput {
				data, err := json.MarshalIndent(caps, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			if len(caps) == 0 {
				fmt.Println("No hardware capabilities found.")
				return nil
			}

			headers := []string{"Category", "Device", "Description", "Properties"}
			var rows [][]string
			for _, c := range caps {
				props := formatProperties(c.GetProperties())
				rows = append(rows, []string{
					c.GetCategory(),
					c.GetDevicePath(),
					c.GetDescription(),
					props,
				})
			}
			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}

	cmd.Flags().StringVar(&category, "category", "", "Filter by category (e.g., gpu, audio, camera)")
	return cmd
}

func formatProperties(props map[string]string) string {
	if len(props) == 0 {
		return ""
	}
	var parts []string
	for k, v := range props {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	return strings.Join(parts, ", ")
}
