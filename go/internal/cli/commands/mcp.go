package commands

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
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
		Long:  "Start a Model Context Protocol server that exposes wendy device tools over stdio.\nConfigure your AI tool to run: wendy mcp serve\nOr run 'wendy mcp setup' to configure automatically.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			srv := wendymcp.New(cfg, connectWithAutoTLS)
			srv.SetLANDiscoverer(func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
				return discovery.CollectLAN(ctx, cliLANStreamOptions(ctx), timeout)
			})
			address := deviceFlag
			if address == "" {
				address = cfg.DefaultDevice
			}
			switch {
			case os.Getenv("WENDY_AGENT_SOCKET") != "":
				// Admin-entitled on-device container: connect over the local
				// agent socket regardless of any configured device. connectFn
				// (connectWithAutoTLS) honors WENDY_AGENT_SOCKET, so the address
				// passed here is ignored.
				srv.SetStartupConnect(func(connectCtx context.Context) {
					if err := srv.ConnectToOnStartup(connectCtx, ""); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not connect to agent socket: %v\n", err)
					}
				})
			case address != "":
				if _, _, err := net.SplitHostPort(address); err != nil {
					address = hostPort(address, defaultAgentPort)
				}
				startupAddress := address
				srv.SetStartupConnect(func(connectCtx context.Context) {
					if err := srv.ConnectToOnStartup(connectCtx, startupAddress); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: could not connect to %s: %v\n", startupAddress, err)
					}
				})
			}
			return srv.Start(ctx)
		},
	}
	cmd.Flags().StringVarP(&deviceFlag, "device", "d", "", "Device name or IP:port to connect on startup")
	return cmd
}
