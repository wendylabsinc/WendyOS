package commands

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
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
		newDeviceLogsCmd(),
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

func newDeviceLogsCmd() *cobra.Command {
	var appName string
	var serviceName string
	var minSeverity int32

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Stream logs from containers on the device",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			req := &agentpb.StreamLogsRequest{}
			if appName != "" {
				req.AppName = &appName
			}
			if serviceName != "" {
				req.ServiceName = &serviceName
			}
			if minSeverity > 0 {
				req.MinSeverity = &minSeverity
			}
			stream, err := conn.TelemetryService.StreamLogs(ctx, req)
			if err != nil {
				return fmt.Errorf("starting log stream: %w", err)
			}

			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("receiving logs: %w", err)
				}

				logs := resp.GetLogs()
				if logs == nil {
					continue
				}

				for _, rl := range logs.GetResourceLogs() {
					serviceName := resourceServiceName(rl.GetResource())
					for _, sl := range rl.GetScopeLogs() {
						for _, lr := range sl.GetLogRecords() {
							printLogRecord(serviceName, lr)
						}
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "Filter by application name")
	cmd.Flags().StringVar(&serviceName, "service", "", "Filter by service name")
	cmd.Flags().Int32Var(&minSeverity, "min-severity", 0, "Minimum log severity level")

	return cmd
}

var (
	logTraceStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	logDebugStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	logInfoStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("34"))
	logWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	logErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	logFatalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	logTimeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	logAppStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	logMetaStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

func severityLabel(sev otelpb.SeverityNumber) (string, lipgloss.Style) {
	switch {
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_FATAL:
		return "FATAL", logFatalStyle
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_ERROR:
		return "ERROR", logErrorStyle
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_WARN:
		return "WARN ", logWarnStyle
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_INFO:
		return "INFO ", logInfoStyle
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_DEBUG:
		return "DEBUG", logDebugStyle
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_TRACE:
		return "TRACE", logTraceStyle
	default:
		return "     ", logInfoStyle
	}
}

func resourceServiceName(res *otelpb.Resource) string {
	if res == nil {
		return ""
	}
	for _, attr := range res.GetAttributes() {
		if attr.GetKey() == "service.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

func anyValueString(v *otelpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch v.Value.(type) {
	case *otelpb.AnyValue_StringValue:
		return v.GetStringValue()
	case *otelpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", v.GetIntValue())
	case *otelpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%g", v.GetDoubleValue())
	case *otelpb.AnyValue_BoolValue:
		return fmt.Sprintf("%t", v.GetBoolValue())
	default:
		return fmt.Sprintf("%v", v)
	}
}

func printLogRecord(service string, lr *otelpb.LogRecord) {
	ts := time.Unix(0, int64(lr.GetTimeUnixNano())).Local().Format("15:04:05.000")
	label, style := severityLabel(lr.GetSeverityNumber())

	var b strings.Builder
	b.WriteString(logTimeStyle.Render(ts))
	b.WriteByte(' ')
	b.WriteString(style.Render(label))
	if service != "" {
		b.WriteByte(' ')
		b.WriteString(logAppStyle.Render("[" + service + "]"))
	}

	body := lr.GetBody()
	if body != nil {
		b.WriteByte(' ')
		b.WriteString(body.GetStringValue())
	}

	attrs := lr.GetAttributes()
	if len(attrs) > 0 {
		b.WriteByte(' ')
		for i, kv := range attrs {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(logMetaStyle.Render(kv.GetKey() + "=" + anyValueString(kv.GetValue())))
		}
	}

	fmt.Println(b.String())
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubReleaseFull struct {
	TagName    string               `json:"tag_name"`
	Prerelease bool                 `json:"prerelease"`
	Assets     []githubReleaseAsset `json:"assets"`
}

func fetchAgentRelease(nightly bool) (*githubReleaseFull, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	if !nightly {
		resp, err := client.Get(githubReleasesURL)
		if err != nil {
			return nil, fmt.Errorf("fetching latest release: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
		}

		var release githubReleaseFull
		if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
			return nil, fmt.Errorf("decoding release: %w", err)
		}
		return &release, nil
	}

	// For nightly, list releases and find the latest prerelease.
	resp, err := client.Get("https://api.github.com/repos/wendylabsinc/wendy-agent/releases")
	if err != nil {
		return nil, fmt.Errorf("fetching releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var releases []githubReleaseFull
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding releases: %w", err)
	}

	for _, r := range releases {
		if r.Prerelease {
			return &r, nil
		}
	}

	return nil, fmt.Errorf("no nightly (prerelease) found")
}

func downloadAgentBinary(asset githubReleaseAsset) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}

	resp, err := client.Get(asset.BrowserDownloadURL)
	if err != nil {
		return nil, fmt.Errorf("downloading asset: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("opening gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}

		if hdr.Typeflag == tar.TypeReg && strings.HasSuffix(hdr.Name, "wendy-agent") {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("reading binary from tar: %w", err)
			}
			return data, nil
		}
	}

	return nil, fmt.Errorf("wendy-agent binary not found in tarball")
}

func newDeviceUpdateCmd() *cobra.Command {
	var binaryPath string
	var nightly bool

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the agent binary on the target device",
		Long:  "Downloads the latest agent binary from GitHub and uploads it to the device. Use --binary to provide a local binary instead.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			conn, err := connectToAgent(ctx, ExcludeProviders("local", "docker", "wendy-lite"), ExcludeBluetooth())
			if err != nil {
				return err
			}
			defer conn.Close()

			var binaryData []byte

			if binaryPath != "" {
				binaryData, err = os.ReadFile(binaryPath)
				if err != nil {
					return fmt.Errorf("reading binary: %w", err)
				}
			} else {
				// Auto-download: detect arch, fetch release, download binary.
				fmt.Println("Detecting device architecture...")
				versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
				if err != nil {
					return fmt.Errorf("getting device info: %w", err)
				}

				arch := versionResp.GetCpuArchitecture()
				if arch == "" {
					return fmt.Errorf("device did not report CPU architecture; use --binary to provide the binary manually")
				}
				fmt.Printf("Device architecture: %s\n", arch)

				releaseType := "stable"
				if nightly {
					releaseType = "nightly"
				}
				fmt.Printf("Fetching latest %s release...\n", releaseType)

				release, err := fetchAgentRelease(nightly)
				if err != nil {
					return fmt.Errorf("fetching release: %w", err)
				}
				fmt.Printf("Found release: %s\n", release.TagName)

				// Find matching asset: wendy-agent-linux-{arch}-*.tar.gz
				assetPrefix := fmt.Sprintf("wendy-agent-linux-%s-", arch)
				var matchedAsset *githubReleaseAsset
				for _, a := range release.Assets {
					if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, ".tar.gz") {
						matchedAsset = &a
						break
					}
				}
				if matchedAsset == nil {
					return fmt.Errorf("no asset found for linux/%s in release %s", arch, release.TagName)
				}

				fmt.Printf("Downloading %s...\n", matchedAsset.Name)
				binaryData, err = downloadAgentBinary(*matchedAsset)
				if err != nil {
					return fmt.Errorf("downloading binary: %w", err)
				}
			}

			// Compute SHA256.
			h := sha256.Sum256(binaryData)
			sha256Hash := hex.EncodeToString(h[:])

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

	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to a local agent binary to upload (skips download)")
	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use the latest nightly (prerelease) build")

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
