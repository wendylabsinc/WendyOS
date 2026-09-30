package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
)

func newMCPGatewayCmd() *cobra.Command {
	var configPath, listen string
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Serve the ChatGPT robot plugin with explicit device and app permissions",
		Long:  "Serve the Wendy robot panel and scoped robot tools. Defaults to stdio for a private MCP connection. --listen enables authenticated Streamable HTTP at /mcp. Configure robots, subject grants, and allowed apps in a gateway JSON file; HTTP additionally requires authentication configuration.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				configPath = os.Getenv("WENDY_ROBOT_GATEWAY_CONFIG")
			}
			if configPath == "" {
				return fmt.Errorf("set --config or WENDY_ROBOT_GATEWAY_CONFIG to an explicit gateway policy file")
			}
			file, err := os.Open(configPath)
			if err != nil {
				return err
			}
			defer file.Close()
			cfg, err := wendymcp.DecodeRobotGatewayConfig(file)
			if err != nil {
				return fmt.Errorf("gateway configuration: %w", err)
			}
			if os.Getenv("WENDY_AGENT_SOCKET") != "" {
				return fmt.Errorf("gateway requires explicit targets; unset WENDY_AGENT_SOCKET so it cannot override device routing")
			}
			for _, robot := range cfg.Robots {
				if _, matched, err := parseCloudDeviceSelector(robot.Device); matched && err != nil {
					return err
				}
			}
			gateway, err := wendymcp.NewRobotGateway(cfg, func(ctx context.Context, device string) (*grpcclient.AgentConnection, error) {
				return connectMCPDevice(ctx, mcpStartupAddress(device))
			}, wendymcp.WithRobotCloudDiscovery(discoverGatewayCloud), wendymcp.WithGatewayLifecycle(installationJobBackend(), wendymcp.ProjectBackend{Validate: validateMCPProject}, simulatorBackend()))
			if err != nil {
				return err
			}
			if listen == "" {
				return gateway.StartStdio(cmd.Context())
			}
			handler, err := gateway.HTTPHandler()
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			defer ln.Close()
			srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32768}
			stopped := make(chan struct{})
			defer close(stopped)
			go func() {
				select {
				case <-cmd.Context().Done():
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = srv.Shutdown(ctx)
				case <-stopped:
				}
			}()
			fmt.Fprintf(cmd.ErrOrStderr(), "Wendy robot gateway listening on %s (MCP: /mcp)\n", ln.Addr())
			if cfg.HTTP.OAuth == nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Development token authentication. Use OAuth before public distribution.")
			}
			err = srv.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Gateway JSON policy file (or WENDY_ROBOT_GATEWAY_CONFIG)")
	cmd.Flags().StringVar(&listen, "listen", "", "HTTP listen address, e.g. 127.0.0.1:8788; requires configured authentication")
	return cmd
}

func newMCPExportRobotToolCmd() *cobra.Command {
	var configPath, robot, app, tool, name string
	cmd := &cobra.Command{Use: "export-robot-tool", Short: "Read an app tool descriptor for review before exporting it to ChatGPT", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if os.Getenv("WENDY_AGENT_SOCKET") != "" {
			return fmt.Errorf("unset WENDY_AGENT_SOCKET to preserve explicit robot routing")
		}
		file, err := os.Open(configPath)
		if err != nil {
			return err
		}
		defer file.Close()
		cfg, err := wendymcp.DecodeRobotGatewayConfig(file)
		if err != nil {
			return err
		}
		g, err := wendymcp.NewRobotGateway(cfg, func(ctx context.Context, device string) (*grpcclient.AgentConnection, error) {
			return connectMCPDevice(ctx, mcpStartupAddress(device))
		}, wendymcp.WithRobotCloudDiscovery(discoverGatewayCloud))
		if err != nil {
			return err
		}
		exported, err := g.InspectExport(cmd.Context(), robot, app, tool, name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(exported)
	}}
	for _, flag := range []struct {
		name        string
		value       *string
		description string
	}{{"config", &configPath, "Gateway configuration file"}, {"robot", &robot, "Robot ID from the gateway configuration"}, {"app", &app, "Allowed app name"}, {"tool", &tool, "Original app tool name"}, {"name", &name, "Stable name to publish through the gateway"}} {
		cmd.Flags().StringVar(flag.value, flag.name, "", flag.description)
		_ = cmd.MarkFlagRequired(flag.name)
	}
	return cmd
}
