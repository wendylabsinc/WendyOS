package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCP server for AI assistant access to wendy devices",
	}
	cmd.AddCommand(newMCPServeCmd())
	cmd.AddCommand(newMCPSetupCmd())
	cmd.AddCommand(newMCPManifestCmd())
	return cmd
}

// newMCPManifestCmd prints the tool manifest published to service-protos
// (docs/relay-mcp-tools.json) for the hosted relay.
func newMCPManifestCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "manifest",
		Short:  "Print the MCP tool manifest JSON published for the hosted relay",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := wendymcp.Manifest()
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			return enc.Encode(m)
		},
	}
}

func newMCPServeCmd() *cobra.Command {
	var deviceFlag string
	var toolGroups []string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server on stdio",
		Long:  "Start a Model Context Protocol server that exposes wendy device tools over stdio.\nConfigure your AI tool to run: wendy mcp serve\nOr run 'wendy mcp setup' to configure automatically.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := enterLocalMCPChannel(); err != nil {
				return err
			}
			srv, err := newMCPServer(deviceFlag, toolGroups)
			if err != nil {
				return err
			}
			return srv.Start(cmd.Context())
		},
	}
	cmd.Flags().StringVarP(&deviceFlag, "device", "d", "", "Device name or IP:port to connect on startup")
	cmd.Flags().StringSliceVar(&toolGroups, "tool-groups", []string{"core"}, "Advertised tools: core, setup, simulator, hardware, robotics, observability, cloud, all (comma-separated; groups can also be selected with wendy_tools)")
	return cmd
}

// newMCPServer builds the wendy MCP server shared by `wendy mcp serve` and
// `wendy cloud mcp`.
func newMCPServer(deviceFlag string, toolGroups []string) (*wendymcp.Server, error) {
	// Long-lived: read credentials fresh rather than from the snapshot
	// loaded below (see wendymcp.EnableConfigReload).
	wendymcp.EnableConfigReload(config.Load)
	// The server outlives wendy-auth's one-hour access token: renew it per
	// RPC, as terminal commands do.
	wendymcp.SetDPoPTokenProvider(cliDPoPTokenProvider)
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	srv := wendymcp.New(cfg, connectMCPDevice)
	srv.SetCLIUpdateChecker(checkCLIUpdateIfDue)
	if err := srv.SetToolGroups(toolGroups); err != nil {
		return nil, err
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
	return srv, nil
}

// enterLocalMCPChannel refuses a local MCP server when the session's realm
// turned Allow MCP off, then marks this process's signed Cloud calls, and
// those of the CLI children its tools start, as MCP_LOCAL. Only a resolvable
// session can be checked here; the cloud tools also check the session each
// call resolves to.
func enterLocalMCPChannel() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if auth, err := config.ResolveAuth(cfg, "", nil); err == nil {
		if err := wendymcp.RealmAllowsMCP(auth); err != nil {
			return err
		}
	}
	return cloudrequest.SetChannel(cloudpbv2.RequestChannel_REQUEST_CHANNEL_MCP_LOCAL, "", "")
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
