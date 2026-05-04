package commands

import (
	"fmt"
	"net"
	"os"

	"github.com/spf13/cobra"
	wendymcp "github.com/wendylabsinc/wendy/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server for AI assistant access to wendy devices",
	}
	cmd.AddCommand(newMCPServeCmd())
	cmd.AddCommand(newMCPSetupCmd())
	return cmd
}

func newMCPServeCmd() *cobra.Command {
	var deviceFlag string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server on stdio",
		Long:  "Start a Model Context Protocol server that exposes wendy device tools over stdio.\nConfigure Claude Desktop, Claude Code, or Codex to run: wendy mcp serve",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			srv := wendymcp.New(cfg, connectWithAutoTLS)
			address := deviceFlag
			if address == "" {
				address = cfg.DefaultDevice
			}
			if address != "" {
				if _, _, err := net.SplitHostPort(address); err != nil {
					address = hostPort(address, defaultAgentPort)
				}
				if err := srv.ConnectTo(ctx, address); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: could not connect to %s: %v\n", address, err)
				}
			}
			return srv.Start(ctx)
		},
	}
	cmd.Flags().StringVarP(&deviceFlag, "device", "d", "", "Device name or IP:port to connect on startup")
	return cmd
}
