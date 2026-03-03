package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newDeviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "device",
		Short: "Manage WendyOS devices",
	}

	cmd.AddCommand(
		newDeviceVersionCmd(),
		newDeviceSetDefaultCmd(),
		newDeviceUnsetDefaultCmd(),
		newDeviceSetupCmd(),
		newDeviceUpdateCmd(),
		newWifiCmd(),
	)

	return cmd
}

func newDeviceVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Get the agent version on the target device",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("getting agent version: %w", err)
			}

			if jsonOutput {
				data, err := json.MarshalIndent(map[string]string{
					"version":         resp.GetVersion(),
					"os":              resp.GetOs(),
					"osVersion":       resp.GetOsVersion(),
					"cpuArchitecture": resp.GetCpuArchitecture(),
					"cliVersion":      version.Version,
				}, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			fmt.Printf("Agent Version: %s\n", resp.GetVersion())
			fmt.Printf("OS: %s %s\n", resp.GetOs(), resp.GetOsVersion())
			fmt.Printf("Architecture: %s\n", resp.GetCpuArchitecture())
			fmt.Printf("CLI Version: %s\n", version.Version)

			if resp.GetVersion() != version.Version {
				fmt.Println("\nNote: CLI and agent versions differ. Consider running 'wendy device update'.")
			}

			return nil
		},
	}
}

func newDeviceSetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-default [hostname]",
		Short: "Set the default device hostname",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			cfg.DefaultDevice = args[0]
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("Default device set to: %s\n", args[0])
			return nil
		},
	}
}

func newDeviceUnsetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset-default",
		Short: "Clear the default device",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			cfg.DefaultDevice = ""
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Println("Default device cleared.")
			return nil
		},
	}
}

func newDeviceSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive device provisioning setup",
		Long:  "Walks through provisioning, WiFi configuration, and agent updates for a new device.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			// Step 1: Check provisioning status.
			fmt.Println("Checking device provisioning status...")
			provResp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
			if err != nil {
				return fmt.Errorf("checking provisioning status: %w", err)
			}

			if provResp.GetProvisioned() != nil {
				prov := provResp.GetProvisioned()
				fmt.Printf("Device is provisioned (org: %d, asset: %d, cloud: %s).\n",
					prov.GetOrganizationId(), prov.GetAssetId(), prov.GetCloudHost())
			} else {
				fmt.Println("Device is not provisioned.")
				fmt.Println("To enroll this device with Wendy Cloud:")
				fmt.Println("  1. Run 'wendy auth login' to authenticate")
				fmt.Println("  2. Run 'wendy device setup' again to complete provisioning")
				fmt.Println()
			}

			// Step 2: Check WiFi status.
			fmt.Println("Checking WiFi status...")
			wifiResp, err := conn.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
			if err != nil {
				fmt.Println("Unable to check WiFi status (may not be supported on this device).")
			} else if wifiResp.GetConnected() {
				fmt.Printf("WiFi connected to: %s\n", wifiResp.GetSsid())
			} else {
				fmt.Println("WiFi is not connected.")
				fmt.Println("Scanning for available networks...")

				networks, scanErr := scanWiFiNetworks(ctx, conn)
				if scanErr != nil {
					fmt.Printf("WiFi scan failed: %v\n", scanErr)
				} else if len(networks) > 0 {
					headers := []string{"SSID", "Signal"}
					var rows [][]string
					for _, n := range networks {
						rows = append(rows, []string{
							n.GetSsid(),
							fmt.Sprintf("%d%%", n.GetSignalStrength()),
						})
					}
					fmt.Print(tui.RenderTable(headers, rows))
					fmt.Println("\nTo connect to a WiFi network, run:")
					fmt.Println("  wendy device wifi connect --ssid <SSID> --password <PASSWORD>")
				} else {
					fmt.Println("No WiFi networks found.")
				}
			}

			// Step 3: Check for agent updates.
			fmt.Println("\nChecking agent version...")
			versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				fmt.Printf("Unable to check agent version: %v\n", err)
			} else {
				fmt.Printf("Agent version: %s\n", versionResp.GetVersion())
				if versionResp.GetVersion() != version.Version {
					fmt.Printf("CLI version: %s (differs from agent)\n", version.Version)
					fmt.Println("Consider running 'wendy device update' to update the agent.")
				} else {
					fmt.Println("Agent is up to date.")
				}
			}

			fmt.Println("\nSetup check complete.")
			return nil
		},
	}
}

// scanWiFiNetworks queries the agent for available WiFi networks.
func scanWiFiNetworks(ctx context.Context, conn *grpcclient.AgentConnection) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	resp, err := conn.AgentService.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing WiFi networks: %w", err)
	}
	return resp.GetNetworks(), nil
}

func newDeviceUpdateCmd() *cobra.Command {
	var binaryPath string

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the agent binary on the target device",
		Long:  "Streams a new agent binary to the device and performs an atomic replacement.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if binaryPath == "" {
				return fmt.Errorf("--binary flag is required; specify the path to the agent binary for the target platform")
			}

			// Read the binary file.
			binaryData, err := os.ReadFile(binaryPath)
			if err != nil {
				return fmt.Errorf("reading binary: %w", err)
			}

			// Compute SHA256.
			h := sha256.Sum256(binaryData)
			sha256Hash := hex.EncodeToString(h[:])

			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			s := tui.NewSpinner("Uploading agent binary...")
			p := tea.NewProgram(s)

			go func() {
				uploadErr := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash)
				p.Send(tui.SpinnerDoneMsg{Err: uploadErr})
			}()

			finalModel, runErr := p.Run()
			if runErr != nil {
				return fmt.Errorf("TUI error: %w", runErr)
			}

			model := finalModel.(tui.SpinnerModel)
			_, updateErr := model.Result()
			if updateErr != nil {
				return updateErr
			}

			fmt.Println("Agent updated successfully.")
			return nil
		},
	}

	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to the agent binary to upload")

	return cmd
}

// deviceUpdateUpload streams the binary data to the agent's UpdateAgent RPC.
func deviceUpdateUpload(ctx context.Context, agentService agentpb.WendyAgentServiceClient, binaryData []byte, sha256Hash string) error {
	stream, err := agentService.UpdateAgent(ctx)
	if err != nil {
		return fmt.Errorf("starting agent update: %w", err)
	}

	// Send binary in chunks.
	const chunkSize = 64 * 1024
	for offset := 0; offset < len(binaryData); offset += chunkSize {
		end := offset + chunkSize
		if end > len(binaryData) {
			end = len(binaryData)
		}

		if err := stream.Send(&agentpb.UpdateAgentRequest{
			RequestType: &agentpb.UpdateAgentRequest_Chunk_{
				Chunk: &agentpb.UpdateAgentRequest_Chunk{
					Data: binaryData[offset:end],
				},
			},
		}); err != nil {
			return fmt.Errorf("sending binary chunk: %w", err)
		}
	}

	// Send update control command with SHA256.
	if err := stream.Send(&agentpb.UpdateAgentRequest{
		RequestType: &agentpb.UpdateAgentRequest_Control{
			Control: &agentpb.UpdateAgentRequest_ControlCommand{
				Command: &agentpb.UpdateAgentRequest_ControlCommand_Update_{
					Update: &agentpb.UpdateAgentRequest_ControlCommand_Update{
						Sha256: sha256Hash,
					},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("sending update command: %w", err)
	}

	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("closing send: %w", err)
	}

	// Wait for the Updated response.
	for {
		resp, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return fmt.Errorf("receiving update response: %w", recvErr)
		}
		if resp.GetUpdated() != nil {
			return nil
		}
	}

	return nil
}
