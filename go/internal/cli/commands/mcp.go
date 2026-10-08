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
	cmd.AddCommand(newMCPGatewayCmd())
	cmd.AddCommand(newMCPExportRobotToolCmd())
	return cmd
}

func newMCPServeCmd() *cobra.Command {
	var deviceFlag string
	var toolGroups []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server on stdio",
		Long:  "Start a Model Context Protocol server that exposes wendy device tools over stdio.\nConfigure your AI tool to run: wendy mcp serve\nOr run 'wendy mcp setup' to configure automatically.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Long-lived: read credentials fresh rather than from the snapshot
			// loaded below (see wendymcp.EnableConfigReload).
			wendymcp.EnableConfigReload(config.Load)
			ctx := cmd.Context()
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			srv := wendymcp.New(cfg, connectMCPDevice)
			srv.SetCLIUpdateChecker(checkCLIUpdateIfDue)
			if err := srv.SetToolGroups(toolGroups); err != nil {
				return err
			}
			srv.SetInstallationBackend(installationJobBackend())
			srv.SetSimulatorBackend(simulatorBackend())
			srv.SetProjectBackend(wendymcp.ProjectBackend{Validate: validateMCPProject})
			srv.SetLANDiscoverer(func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
				return discovery.CollectLAN(ctx, cliLANStreamOptions(ctx), timeout)
			})
			srv.SetLoginStarter(mcpLoginStarter)
			srv.SetUSBSetupNotice(pendingUSBSetupNotice)
			address := mcpStartupDevice(deviceFlag, cfg)
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
				address = mcpStartupAddress(address)
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
	cmd.Flags().StringSliceVar(&toolGroups, "tool-groups", []string{"core"}, "Advertised tools: core, setup, simulator, hardware, robotics, observability, cloud, all (comma-separated; groups can also be selected with wendy_tools)")
	return cmd
}

func mcpStartupAddress(address string) string {
	if _, matched, _ := parseCloudDeviceSelector(address); matched {
		return address
	}
	if _, matched, err := simulatorName(address); matched || err != nil {
		return address
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return hostPort(address, defaultAgentPort)
	}
	return address
}
