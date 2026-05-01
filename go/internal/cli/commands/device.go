package commands

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

func newDeviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "device",
		Short: "Manage WendyOS devices",
	}

	cmd.AddGroup(
		&cobra.Group{ID: "manage", Title: "Device Management:"},
		&cobra.Group{ID: "monitor", Title: "Monitoring:"},
		&cobra.Group{ID: "hardware", Title: "Hardware:"},
		&cobra.Group{ID: "data", Title: "Apps & Storage:"},
	)

	addToGroup := func(groupID string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = groupID
			cmd.AddCommand(c)
		}
	}

	addToGroup("manage",
		newDeviceInfoCmd(),
		newDeprecatedDeviceVersionCmd(),
		newDeviceSetDefaultCmd(),
		newDeviceUnsetDefaultCmd(),
		newDeviceSetupCmd(),
		newDeviceEnrollCmd(),
		newDeviceUpdateCmd(),
	)
	addToGroup("monitor",
		newDeviceLogsCmd(),
		newDeviceDashboardCmd(),
		newDeviceTelemetryStreamCmd(),
	)
	addToGroup("hardware",
		newWifiCmd(),
		newBluetoothCmd(),
		newAudioCmd(),
		newVideoCmd(),
		newHardwareCmd(),
	)
	addToGroup("data",
		newAppsCmd(),
		newVolumesCmd(),
	)

	return cmd
}

func newDeviceInfoCmd() *cobra.Command {
	return newDeviceInfoLikeCmd("info", false)
}

func newDeprecatedDeviceVersionCmd() *cobra.Command {
	return newDeviceInfoLikeCmd("version", true)
}

func newDeviceInfoLikeCmd(use string, deprecated bool) *cobra.Command {
	var checkUpdates bool
	var prerelease bool

	cmd := &cobra.Command{
		Use:    use,
		Short:  "Show agent version, OS, architecture, GPU, and hardware info for the target device",
		Hidden: deprecated,
		RunE: func(cmd *cobra.Command, args []string) error {
			if deprecated && !jsonOutput {
				cmd.PrintErrln("Warning: 'wendy device version' is deprecated; use 'wendy device info' instead.")
			}

			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			var agentVersion, osName, osVersion, cpuArch, deviceType, storageMedium, gpuVendor, jetpackVersion, cudaVersion string
			var hasGPU bool

			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				cliLogln("Connecting to %s via Bluetooth...", target.Bluetooth.DisplayName)
				bleClient, bleErr := connectBLEAgent(target.Bluetooth)
				if bleErr != nil {
					return bleErr
				}
				defer bleClient.Close()
				bleResp, bleErr := bleClient.AgentVersion()
				if bleErr != nil {
					return fmt.Errorf("getting agent version: %w", bleErr)
				}
				agentVersion = bleResp.GetVersion()
				osName = bleResp.GetOs()
				osVersion = bleResp.GetOsVersion()
				cpuArch = bleResp.GetCpuArchitecture()
			} else if target.Agent != nil {
				resp, respErr := target.Agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
				if respErr != nil {
					return fmt.Errorf("getting agent version: %w", respErr)
				}
				agentVersion = resp.GetVersion()
				osName = resp.GetOs()
				osVersion = resp.GetOsVersion()
				cpuArch = resp.GetCpuArchitecture()
				deviceType = resp.GetDeviceType()
				storageMedium = resp.GetStorageMedium()
				hasGPU = resp.GetHasGpu()
				gpuVendor = resp.GetGpuVendor()
				jetpackVersion = resp.GetJetpackVersion()
				cudaVersion = resp.GetCudaVersion()
			} else {
				return fmt.Errorf("selected device does not support this command")
			}

			var latestVersion string
			if checkUpdates {
				release, err := fetchAgentRelease(prerelease)
				if err != nil {
					return fmt.Errorf("checking for updates: %w", err)
				}
				latestVersion = release.TagName
			}

			if jsonOutput {
				out := map[string]any{
					"version":         agentVersion,
					"os":              osName,
					"osVersion":       osVersion,
					"cpuArchitecture": cpuArch,
					"deviceType":      deviceType,
					"cliVersion":      version.Version,
					"hasGpu":          hasGPU,
				}
				if storageMedium != "" {
					out["storageMedium"] = storageMedium
				}
				if gpuVendor != "" {
					out["gpuVendor"] = gpuVendor
				}
				if jetpackVersion != "" {
					out["jetpackVersion"] = jetpackVersion
				}
				if cudaVersion != "" {
					out["cudaVersion"] = cudaVersion
				}
				if checkUpdates {
					out["latestVersion"] = latestVersion
					out["updateAvailable"] = version.CompareVersions(latestVersion, agentVersion) > 0
				}
				data, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			cliLogln("Agent Version: %s", agentVersion)
			cliLogln("OS: %s %s", osName, osVersion)
			cliLogln("Architecture: %s", cpuArch)
			if deviceType != "" {
				cliLogln("Device Type: %s", deviceType)
			}
			if storageMedium != "" {
				cliLogln("Storage: %s", storageMedium)
			}
			if hasGPU {
				vendor := gpuVendor
				if vendor == "" {
					vendor = "unknown"
				}
			cliLogln("GPU: %s", vendor)
				if jetpackVersion != "" {
					cliLogln("JetPack: %s", jetpackVersion)
				}
				if cudaVersion != "" {
					cliLogln("CUDA: %s", cudaVersion)
				}
			}
			cliLogln("CLI Version: %s", version.Version)

			warn := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
			if cmp := version.CompareVersions(version.Version, agentVersion); cmp > 0 {
			cliNotice("\nNote: Agent is behind the CLI. Consider running 'wendy device update'.")
			} else if cmp < 0 {
				fmt.Println(warn.Render("\nCLI is behind the agent — consider updating the CLI."))
			}

			if checkUpdates {
				if version.CompareVersions(latestVersion, agentVersion) > 0 {
				cliNotice("\nUpdate available: %s (you have %s)\nUpdate with: wendy device update", latestVersion, agentVersion)
				} else {
					cliSuccess("\nAgent is up to date.")
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&checkUpdates, "check-updates", false, "Check for available agent updates on GitHub")
	cmd.Flags().BoolVar(&prerelease, "prerelease", false, "Include prerelease (nightly) builds when checking for updates")

	return cmd
}

func newDeviceSetDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-default [hostname]",
		Short: "Set the default device hostname",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var device string
			if len(args) > 0 {
				device = args[0]
			} else {
				sel, err := pickDeviceForDefault(cmd.Context())
				if err != nil {
					return err
				}
				device = sel
			}

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			cfg.DefaultDevice = device
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			cliSuccess("Default device set to: %s", device)
			return nil
		},
	}
}

// pickDeviceForDefault runs the interactive device picker and returns a
// hostname or provider key suitable for storing as the default device.
func pickDeviceForDefault(ctx context.Context) (string, error) {
	selected, err := pickDevice(ctx, nil, false, false)
	if err != nil {
		return "", err
	}
	defer selected.Close()

	if selected.Agent != nil {
		return selected.Agent.Host, nil
	}
	if selected.External != nil {
		return selected.External.ProviderKey, nil
	}
	return "", fmt.Errorf("no device selected")
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

			cliSuccess("Default device cleared.")
			return nil
		},
	}
}

func newDeviceSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Interactive device setup: enroll, name, and configure WiFi",
		Long:  "Walks through enrollment (with device naming) and WiFi configuration for a new device.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			reader := bufio.NewReader(os.Stdin)

			// Step 1: Enroll (and name) the device.
			provResp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
			if err != nil {
				return fmt.Errorf("checking enrollment status: %w", err)
			}

			if provResp.GetProvisioned() != nil {
				prov := provResp.GetProvisioned()
			cliSuccess("Device is already enrolled (org: %d, asset: %d, cloud: %s).",
					prov.GetOrganizationId(), prov.GetAssetId(), prov.GetCloudHost())
			} else {
				cliNotice("Device is not enrolled.")
				if loadCLICert() == nil {
					cliNotice("You are not logged in to Wendy Cloud.")
					fmt.Print("Log in now? [Y/n] ")
					answer, _ := reader.ReadString('\n')
					answer = strings.TrimSpace(strings.ToLower(answer))
					if answer == "" || answer == "y" || answer == "yes" {
						if loginErr := performLogin(ctx, defaultCloudDashboard, defaultCloudGRPC); loginErr != nil {
							return fmt.Errorf("login failed: %w", loginErr)
						}
					}
				}

				if auth := loadCLIAuth(); auth != nil {
					// Collect the device name before enrolling (name cannot be changed after).
					fmt.Print("Device name: ")
					line, _ := reader.ReadString('\n')
					deviceName := strings.TrimSpace(line)
					if deviceName == "" {
						return fmt.Errorf("device name is required")
					}
					if enrollErr := runEnrollDevice(ctx, conn, auth, deviceName); enrollErr != nil {
						cliNotice("Enrollment failed: %v", enrollErr)
					}
				}
				fmt.Println()
			}

			// Step 2: WiFi setup.
			target := &SelectedDevice{Agent: conn}
			ssid, pickErr := pickWifiNetwork(ctx, target)
			if pickErr != nil {
				if errors.Is(pickErr, ErrUserCancelled) {
					cliLogln("WiFi setup skipped.")
				} else {
					cliNotice("WiFi scan failed: %v", pickErr)
				}
			} else {
				fmt.Print("Password (leave empty for open networks): ")
				passwordBytes, readErr := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Println()
				if readErr != nil {
					cliNotice("Failed to read password: %v", readErr)
				} else {
					password := strings.TrimSpace(string(passwordBytes))
					cliLogln("Connecting to %s...", ssid)
					wifiConnResp, connectErr := conn.AgentService.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
						Ssid:     ssid,
						Password: password,
					})
					if connectErr != nil {
						cliNotice("Failed to connect to WiFi: %v", connectErr)
					} else if !wifiConnResp.GetSuccess() {
						cliNotice("Failed to connect: %s", wifiConnResp.GetErrorMessage())
					} else {
						cliSuccess("Connected to %s.", ssid)
					}
				}
			}

			// Step 3: Check agent version.
			fmt.Println()
			versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				cliNotice("Unable to check agent version: %v", err)
			} else {
				cliLogln("Agent version: %s", versionResp.GetVersion())
				if cmp := version.CompareVersions(version.Version, versionResp.GetVersion()); cmp > 0 {
					cliNotice("Agent is behind the CLI — consider running 'wendy device update'.")
				}
			}

			cliSuccess("\nSetup complete.")
			return nil
		},
	}
}

func newDeviceEnrollCmd() *cobra.Command {
	var name string
	var cloudGRPC string

	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Enroll this device with Wendy Cloud or a local pki-core",
		Long:  "Creates an enrollment token using your stored auth session and provisions the connected device with mTLS certificates. Run 'wendy auth login' first.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			promptWifiIfNeeded(ctx, conn)

			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}

			return runEnrollDevice(ctx, conn, auth, name)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Device name")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud/pki-core gRPC endpoint to use (required when multiple auth sessions exist)")
	return cmd
}

// promptWifiIfNeeded checks whether the device is connected to WiFi, and if
// not, offers an interactive flow to connect before enrollment. Errors from the
// status check are silently ignored so the function degrades gracefully on
// devices that don't support WiFi (e.g. local, docker).
func promptWifiIfNeeded(ctx context.Context, conn *grpcclient.AgentConnection) {
	if !isInteractiveTerminal() {
		return
	}

	statusResp, err := conn.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
	if err != nil || statusResp.GetConnected() {
		return
	}

	fmt.Println("No WiFi connection detected on the device.")
	fmt.Print("Set up WiFi before enrolling? [Y/n] ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	answer := strings.TrimSpace(strings.ToLower(line))
	if answer != "" && answer != "y" && answer != "yes" {
		return
	}

	target := &SelectedDevice{Agent: conn}
	ssid, pickErr := pickWifiNetwork(ctx, target)
	if pickErr != nil {
		if !errors.Is(pickErr, ErrUserCancelled) {
			fmt.Printf("WiFi setup failed: %v\n", pickErr)
		}
		return
	}

	fmt.Print("Password (leave empty for open networks): ")
	passwordBytes, readErr := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if readErr != nil {
		fmt.Printf("Failed to read password: %v\n", readErr)
		return
	}
	password := strings.TrimSpace(string(passwordBytes))

	fmt.Printf("Connecting to %s...\n", ssid)
	wifiResp, connectErr := conn.AgentService.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
		Ssid:     ssid,
		Password: password,
	})
	if connectErr != nil {
		fmt.Printf("WiFi connection failed: %v\n", connectErr)
	} else if !wifiResp.GetSuccess() {
		fmt.Printf("WiFi connection failed: %s\n", wifiResp.GetErrorMessage())
	} else {
		fmt.Printf("Connected to %s.\n", ssid)
	}
}

// runEnrollDevice creates an enrollment token via the stored auth session and
// calls StartProvisioning on the connected device agent. name is optional; the
// user is prompted interactively when it is empty.
func runEnrollDevice(ctx context.Context, conn *grpcclient.AgentConnection, auth *config.AuthConfig, name string) error {
	if len(auth.Certificates) == 0 {
		return fmt.Errorf("selected auth entry has no certificates; re-run 'wendy auth login'")
	}

	if name == "" {
		if !isInteractiveTerminal() {
			return fmt.Errorf("device name is required; pass --name when not running interactively")
		}
		fmt.Print("Device name: ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		name = strings.TrimSpace(line)
		if name == "" {
			return fmt.Errorf("device name is required")
		}
	}

	if auth == nil || len(auth.Certificates) == 0 {
		return fmt.Errorf("missing authentication certificate in selected auth entry")
	}
	cert := auth.Certificates[0]

	var cloudTransport grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		tlsCfg, err := certs.LoadTLSConfig(
			cert.PemCertificate,
			cert.PemCertificateChain,
			cert.PemPrivateKey,
			"",
		)
		if err != nil {
			return fmt.Errorf("loading TLS config: %w", err)
		}
		cloudTransport = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		cloudTransport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	cloudConn, err := grpc.NewClient(auth.CloudGRPC, cloudTransport)
	if err != nil {
		return fmt.Errorf("connecting to cloud: %w", err)
	}
	defer cloudConn.Close()

	tokenCtx := cloudContext(ctx, auth)

	certClient := cloudpb.NewCertificateServiceClient(cloudConn)
	tokenResp, err := certClient.CreateAssetEnrollmentToken(tokenCtx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: int32(cert.OrganizationID),
		Name:           name,
		TtlSeconds:     600,
	})
	if err != nil {
		return fmt.Errorf("creating enrollment token: %w", err)
	}

	cliLogln("Enrolling device...")
	_, err = conn.ProvisioningService.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		OrganizationId:  tokenResp.GetOrganizationId(),
		AssetId:         tokenResp.GetAssetId(),
		EnrollmentToken: tokenResp.GetEnrollmentToken(),
		CloudHost:       auth.CloudGRPC,
	})
	if err != nil {
		return fmt.Errorf("enrolling device: %w", err)
	}

	cliSuccess("Device enrolled (org: %d, asset: %d).",
		tokenResp.GetOrganizationId(), tokenResp.GetAssetId())
	return nil
}

// pickAuthEntry returns the auth config entry to use for enrollment.
// If cloudGRPC is specified it must match an existing entry. When no cloudGRPC
// is given and multiple sessions exist, an error is returned requiring the user
// to specify --cloud-grpc explicitly.
func pickAuthEntry(cloudGRPC string) (*config.AuthConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	if len(cfg.Auth) == 0 {
		return nil, fmt.Errorf("not logged in; run 'wendy auth login' first")
	}
	if cloudGRPC != "" {
		for i := range cfg.Auth {
			if cfg.Auth[i].CloudGRPC == cloudGRPC {
				return &cfg.Auth[i], nil
			}
		}
		return nil, fmt.Errorf("no auth session for %s; run 'wendy auth login --cloud-grpc %s' first", cloudGRPC, cloudGRPC)
	}
	if len(cfg.Auth) > 1 {
		return nil, fmt.Errorf("multiple auth sessions exist; pass --cloud-grpc to select one")
	}
	if len(cfg.Auth[0].Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	return &cfg.Auth[0], nil
}

// scanWiFiNetworks queries the agent for available WiFi networks.
func scanWiFiNetworks(ctx context.Context, conn *grpcclient.AgentConnection) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	resp, err := conn.AgentService.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
	if err != nil {
		return nil, fmt.Errorf("listing WiFi networks: %w", err)
	}
	return resp.GetNetworks(), nil
}

// parseSeverityLevel converts a severity name (e.g. "trace", "info") to its
// OpenTelemetry severity number. Returns 0 if the name is not recognized.
func parseSeverityLevel(name string) int32 {
	switch strings.ToLower(name) {
	case "trace":
		return int32(otelpb.SeverityNumber_SEVERITY_NUMBER_TRACE)
	case "debug":
		return int32(otelpb.SeverityNumber_SEVERITY_NUMBER_DEBUG)
	case "info":
		return int32(otelpb.SeverityNumber_SEVERITY_NUMBER_INFO)
	case "warn", "warning":
		return int32(otelpb.SeverityNumber_SEVERITY_NUMBER_WARN)
	case "error":
		return int32(otelpb.SeverityNumber_SEVERITY_NUMBER_ERROR)
	case "fatal":
		return int32(otelpb.SeverityNumber_SEVERITY_NUMBER_FATAL)
	default:
		return 0
	}
}

func newDeviceLogsCmd() *cobra.Command {
	var appName string
	var serviceName string
	var minSeverity int32
	var level string

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

			// --level takes precedence over --min-severity when both are set.
			if level != "" {
				if sev := parseSeverityLevel(level); sev > 0 {
					minSeverity = sev
				} else {
					return fmt.Errorf("unknown log level %q (use trace, debug, info, warn, error, or fatal)", level)
				}
			}

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
					svcName := resourceServiceName(rl.GetResource())
					for _, sl := range rl.GetScopeLogs() {
						for _, lr := range sl.GetLogRecords() {
							if jsonOutput {
								printLogRecordJSON(svcName, lr)
							} else {
								printLogRecord(svcName, lr)
							}
						}
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "Filter by application name")
	cmd.Flags().StringVar(&serviceName, "service", "", "Filter by service name")
	cmd.Flags().Int32Var(&minSeverity, "min-severity", 0, "Minimum log severity number")
	cmd.Flags().StringVar(&level, "level", "", "Minimum log level (trace, debug, info, warn, error, fatal)")

	return cmd
}

var (
	logTraceStyle = lipgloss.NewStyle().Foreground(tui.ColorDim)
	logDebugStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	logInfoStyle  = lipgloss.NewStyle().Foreground(tui.Emerald400)
	logWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	logErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	logFatalStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	logTimeStyle  = lipgloss.NewStyle().Foreground(tui.ColorDim)
	logAppStyle   = lipgloss.NewStyle().Foreground(tui.Emerald300)
	logMetaStyle  = lipgloss.NewStyle().Foreground(tui.ColorDim)
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

func printLogRecordJSON(service string, lr *otelpb.LogRecord) {
	entry := map[string]any{
		"timestamp": time.Unix(0, int64(lr.GetTimeUnixNano())).UTC().Format(time.RFC3339Nano),
		"severity":  lr.GetSeverityText(),
	}
	if service != "" {
		entry["service"] = service
	}
	if body := lr.GetBody(); body != nil {
		entry["body"] = body.GetStringValue()
	}
	if attrs := lr.GetAttributes(); len(attrs) > 0 {
		meta := make(map[string]string, len(attrs))
		for _, kv := range attrs {
			meta[kv.GetKey()] = anyValueString(kv.GetValue())
		}
		entry["attributes"] = meta
	}
	data, _ := json.Marshal(entry)
	fmt.Println(string(data))
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

func newDeviceTelemetryStreamCmd() *cobra.Command {
	var appName string
	var serviceName string
	var enableLogs bool
	var enableMetrics bool
	var enableTraces bool

	cmd := &cobra.Command{
		Use:   "telemetry-stream",
		Short: "Stream telemetry data (logs, metrics, traces) as JSONL",
		RunE: func(cmd *cobra.Command, args []string) error {
			// If no flags were explicitly set, enable all streams.
			if !cmd.Flags().Changed("logs") && !cmd.Flags().Changed("metrics") && !cmd.Flags().Changed("traces") {
				enableLogs = true
				enableMetrics = true
				enableTraces = true
			}

			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			enc := json.NewEncoder(os.Stdout)

			var mu sync.Mutex
			emit := func(v any) {
				mu.Lock()
				defer mu.Unlock()
				enc.Encode(v) //nolint:errcheck
			}

			var wg sync.WaitGroup
			errc := make(chan error, 3)

			if enableLogs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					logReq := &agentpb.StreamLogsRequest{}
					if appName != "" {
						logReq.AppName = &appName
					}
					if serviceName != "" {
						logReq.ServiceName = &serviceName
					}
					stream, err := conn.TelemetryService.StreamLogs(ctx, logReq)
					if err != nil {
						errc <- fmt.Errorf("starting log stream: %w", err)
						return
					}
					for {
						resp, err := stream.Recv()
						if err == io.EOF {
							return
						}
						if err != nil {
							errc <- fmt.Errorf("receiving logs: %w", err)
							return
						}
						logs := resp.GetLogs()
						if logs == nil {
							continue
						}
						for _, rl := range logs.GetResourceLogs() {
							res := kvMapFromResource(rl.GetResource())
							svc := res["service.name"]
							for _, sl := range rl.GetScopeLogs() {
								for _, lr := range sl.GetLogRecords() {
									sev, sevNum := severityTextAndNumber(lr.GetSeverityNumber())
									emit(telemetryLogEntry{
										Type:           "log",
										Timestamp:      formatNanoUTC(lr.GetTimeUnixNano()),
										TimestampNano:  lr.GetTimeUnixNano(),
										Severity:       sev,
										SeverityNumber: sevNum,
										Service:        svc,
										Resource:       res,
										Body:           anyValueString(lr.GetBody()),
										Attributes:     kvMapFromKeyValues(lr.GetAttributes()),
									})
								}
							}
						}
					}
				}()
			}

			if enableMetrics {
				wg.Add(1)
				go func() {
					defer wg.Done()
					metricReq := &agentpb.StreamMetricsRequest{}
					if appName != "" {
						metricReq.AppName = &appName
					}
					if serviceName != "" {
						metricReq.ServiceName = &serviceName
					}
					stream, err := conn.TelemetryService.StreamMetrics(ctx, metricReq)
					if err != nil {
						errc <- fmt.Errorf("starting metrics stream: %w", err)
						return
					}
					for {
						resp, err := stream.Recv()
						if err == io.EOF {
							return
						}
						if err != nil {
							errc <- fmt.Errorf("receiving metrics: %w", err)
							return
						}
						metrics := resp.GetMetrics()
						if metrics == nil {
							continue
						}
						for _, rm := range metrics.GetResourceMetrics() {
							res := kvMapFromResource(rm.GetResource())
							svc := res["service.name"]
							for _, sm := range rm.GetScopeMetrics() {
								for _, m := range sm.GetMetrics() {
									emitMetricDataPoints(emit, m, svc, res)
								}
							}
						}
					}
				}()
			}

			if enableTraces {
				wg.Add(1)
				go func() {
					defer wg.Done()
					traceReq := &agentpb.StreamTracesRequest{}
					if appName != "" {
						traceReq.AppName = &appName
					}
					if serviceName != "" {
						traceReq.ServiceName = &serviceName
					}
					stream, err := conn.TelemetryService.StreamTraces(ctx, traceReq)
					if err != nil {
						errc <- fmt.Errorf("starting traces stream: %w", err)
						return
					}
					for {
						resp, err := stream.Recv()
						if err == io.EOF {
							return
						}
						if err != nil {
							errc <- fmt.Errorf("receiving traces: %w", err)
							return
						}
						traces := resp.GetTraces()
						if traces == nil {
							continue
						}
						for _, rs := range traces.GetResourceSpans() {
							res := kvMapFromResource(rs.GetResource())
							svc := res["service.name"]
							for _, ss := range rs.GetScopeSpans() {
								for _, span := range ss.GetSpans() {
									startNano := span.GetStartTimeUnixNano()
									endNano := span.GetEndTimeUnixNano()
									durationMs := float64(endNano-startNano) / 1e6

									status := telemetryTraceStatus{Code: "UNSET"}
									if s := span.GetStatus(); s != nil {
										status.Code = s.GetCode().String()
										status.Message = s.GetMessage()
									}

									emit(telemetryTraceEntry{
										Type:          "span",
										TraceID:       hex.EncodeToString(span.GetTraceId()),
										SpanID:        hex.EncodeToString(span.GetSpanId()),
										ParentSpanID:  hex.EncodeToString(span.GetParentSpanId()),
										Name:          span.GetName(),
										Kind:          span.GetKind().String(),
										StartTime:     formatNanoUTC(startNano),
										EndTime:       formatNanoUTC(endNano),
										StartTimeNano: startNano,
										EndTimeNano:   endNano,
										DurationMs:    durationMs,
										Status:        status,
										Service:       svc,
										Attributes:    kvMapFromKeyValues(span.GetAttributes()),
										Resource:      res,
									})
								}
							}
						}
					}
				}()
			}

			// Wait for all goroutines, return first error if any.
			go func() {
				wg.Wait()
				close(errc)
			}()

			for err := range errc {
				if err != nil && ctx.Err() == nil {
					return err
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "Filter by application name")
	cmd.Flags().StringVar(&serviceName, "service", "", "Filter by service name")
	cmd.Flags().BoolVar(&enableLogs, "logs", false, "Include logs")
	cmd.Flags().BoolVar(&enableMetrics, "metrics", false, "Include metrics")
	cmd.Flags().BoolVar(&enableTraces, "traces", false, "Include traces")

	return cmd
}

type telemetryLogEntry struct {
	Type           string            `json:"type"`
	Timestamp      string            `json:"timestamp"`
	TimestampNano  uint64            `json:"timestampNano"`
	Severity       string            `json:"severity"`
	SeverityNumber int32             `json:"severityNumber"`
	Service        string            `json:"service"`
	Resource       map[string]string `json:"resource"`
	Body           string            `json:"body"`
	Attributes     map[string]string `json:"attributes"`
}

type telemetryMetricEntry struct {
	Type       string            `json:"type"`
	Timestamp  string            `json:"timestamp"`
	Service    string            `json:"service"`
	Resource   map[string]string `json:"resource,omitempty"`
	Name       string            `json:"name"`
	Value      float64           `json:"value"`
	MetricType string            `json:"metricType"`
	Unit       string            `json:"unit,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type telemetryTraceStatus struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type telemetryTraceEntry struct {
	Type          string               `json:"type"`
	TraceID       string               `json:"traceId"`
	SpanID        string               `json:"spanId"`
	ParentSpanID  string               `json:"parentSpanId,omitempty"`
	Name          string               `json:"name"`
	Kind          string               `json:"kind"`
	StartTime     string               `json:"startTime"`
	EndTime       string               `json:"endTime"`
	StartTimeNano uint64               `json:"startTimeNano"`
	EndTimeNano   uint64               `json:"endTimeNano"`
	DurationMs    float64              `json:"durationMs"`
	Status        telemetryTraceStatus `json:"status"`
	Service       string               `json:"service"`
	Attributes    map[string]string    `json:"attributes,omitempty"`
	Resource      map[string]string    `json:"resource,omitempty"`
}

// emitMetricDataPoints extracts the latest value from a metric and emits one
// telemetryMetricEntry per metric (using the last data point's value).
func emitMetricDataPoints(emit func(any), m *otelpb.Metric, svc string, res map[string]string) {
	var value float64
	var metricType string
	var attrs map[string]string

	switch {
	case m.GetGauge() != nil:
		metricType = "gauge"
		dps := m.GetGauge().GetDataPoints()
		if len(dps) > 0 {
			dp := dps[len(dps)-1]
			value = numberDataPointValue(dp)
			attrs = kvMapFromKeyValues(dp.GetAttributes())
		}
	case m.GetSum() != nil:
		metricType = "sum"
		dps := m.GetSum().GetDataPoints()
		if len(dps) > 0 {
			dp := dps[len(dps)-1]
			value = numberDataPointValue(dp)
			attrs = kvMapFromKeyValues(dp.GetAttributes())
		}
	case m.GetHistogram() != nil:
		metricType = "histogram"
		dps := m.GetHistogram().GetDataPoints()
		if len(dps) > 0 {
			dp := dps[len(dps)-1]
			if dp.GetSum() != 0 && dp.GetCount() != 0 {
				value = dp.GetSum() / float64(dp.GetCount())
			}
			attrs = kvMapFromKeyValues(dp.GetAttributes())
		}
	case m.GetSummary() != nil:
		metricType = "summary"
		dps := m.GetSummary().GetDataPoints()
		if len(dps) > 0 {
			dp := dps[len(dps)-1]
			if dp.GetCount() != 0 {
				value = dp.GetSum() / float64(dp.GetCount())
			}
			attrs = kvMapFromKeyValues(dp.GetAttributes())
		}
	default:
		metricType = "unknown"
	}

	emit(telemetryMetricEntry{
		Type:       "metric",
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Service:    svc,
		Resource:   res,
		Name:       m.GetName(),
		Value:      value,
		MetricType: metricType,
		Unit:       m.GetUnit(),
		Attributes: attrs,
	})
}

// numberDataPointValue extracts the numeric value from a NumberDataPoint.
func numberDataPointValue(dp *otelpb.NumberDataPoint) float64 {
	switch dp.GetValue().(type) {
	case *otelpb.NumberDataPoint_AsDouble:
		return dp.GetAsDouble()
	case *otelpb.NumberDataPoint_AsInt:
		return float64(dp.GetAsInt())
	default:
		return 0
	}
}

func formatNanoUTC(nanos uint64) string {
	return time.Unix(0, int64(nanos)).UTC().Format(time.RFC3339Nano)
}

func severityTextAndNumber(sev otelpb.SeverityNumber) (string, int32) {
	num := int32(sev)
	switch {
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_FATAL:
		return "FATAL", num
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_ERROR:
		return "ERROR", num
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_WARN:
		return "WARN", num
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_INFO:
		return "INFO", num
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_DEBUG:
		return "DEBUG", num
	case sev >= otelpb.SeverityNumber_SEVERITY_NUMBER_TRACE:
		return "TRACE", num
	default:
		return "UNSPECIFIED", num
	}
}

func kvMapFromResource(res *otelpb.Resource) map[string]string {
	m := make(map[string]string)
	if res == nil {
		return m
	}
	for _, attr := range res.GetAttributes() {
		m[attr.GetKey()] = anyValueString(attr.GetValue())
	}
	return m
}

func kvMapFromKeyValues(kvs []*otelpb.KeyValue) map[string]string {
	m := make(map[string]string)
	for _, kv := range kvs {
		m[kv.GetKey()] = anyValueString(kv.GetValue())
	}
	return m
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

			conn, err := connectToAgent(ctx, ExcludeProviders("local", "docker", "wendy-lite"), ExcludeBluetooth(), SuppressUpdateCheck())
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

				// Validate the binary's ELF architecture against the device.
				// If the device is provisioned and only exposes ProvisioningService
				// on plaintext, GetAgentVersion may be unavailable — skip arch
				// validation in that case rather than blocking the upload.
				versionResp, versionErr := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
				if versionErr == nil {
					deviceArch := versionResp.GetCpuArchitecture()
					if deviceArch != "" {
						if err := checkELFArchitecture(binaryData, deviceArch); err != nil {
							return err
						}
					}
				}
			} else {
				// Auto-download: detect arch, fetch release, download binary.
				if !jsonOutput {
					cliLogln("Detecting device architecture...")
				}
				versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
				if err != nil {
					return fmt.Errorf("getting device info: %w", err)
				}

				arch := versionResp.GetCpuArchitecture()
				if arch == "" {
					return fmt.Errorf("device did not report CPU architecture; use --binary to provide the binary manually")
				}
				if !jsonOutput {
					cliLogln("Device architecture: %s", arch)
				}

				releaseType := "stable"
				if nightly {
					releaseType = "nightly"
				}
				if !jsonOutput {
					cliLogln("Fetching latest %s release...", releaseType)
				}

				release, err := fetchAgentRelease(nightly)
				if err != nil {
					return fmt.Errorf("fetching release: %w", err)
				}
				if !jsonOutput {
					cliLogln("Found release: %s", release.TagName)
				}

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

				if !jsonOutput {
					cliLogln("Downloading %s...", matchedAsset.Name)
				}
				binaryData, err = downloadAgentBinary(*matchedAsset)
				if err != nil {
					return fmt.Errorf("downloading binary: %w", err)
				}
			}

			// Compute SHA256.
			h := sha256.Sum256(binaryData)
			sha256Hash := hex.EncodeToString(h[:])

			if isInteractiveTerminal() && !jsonOutput {
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
			} else if !jsonOutput {
				cliLogln("Uploading agent binary...")
				if err := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash); err != nil {
					return err
				}
			} else {
				if err := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash); err != nil {
					return err
				}
			}

			if jsonOutput {
				resp := map[string]string{
					"status":  "success",
					"message": "Agent updated successfully.",
				}
				b, err := json.Marshal(resp)
				if err != nil {
					return fmt.Errorf("failed to marshal JSON response: %w", err)
				}
				fmt.Println(string(b))
			} else {
				cliSuccess("Agent updated successfully.")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to a local agent binary to upload (skips download)")
	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use the latest nightly (prerelease) build")

	return cmd
}

// checkELFArchitecture reads the ELF e_machine field from data and returns an
// error if it does not match the device's reported GOARCH (e.g. "amd64", "arm64").
// Non-ELF binaries (e.g. scripts) are accepted without complaint.
func checkELFArchitecture(data []byte, deviceArch string) error {
	// Only amd64 and arm64 are supported targets.
	switch deviceArch {
	case "amd64", "arm64":
		// supported, continue
	default:
		return fmt.Errorf("device reports unsupported architecture %q; only amd64 and arm64 are supported", deviceArch)
	}

	// ELF magic + header fields up to e_machine occupy 20 bytes.
	if len(data) < 20 {
		return nil
	}
	if data[0] != 0x7f || data[1] != 'E' || data[2] != 'L' || data[3] != 'F' {
		return nil // not an ELF binary — skip check
	}

	// Respect EI_DATA (byte 5) when reading the 2-byte e_machine field at offset 18.
	const (
		elfDataLittle = 1 // ELFDATA2LSB
		elfDataBig    = 2 // ELFDATA2MSB

		emX86_64  = 62  // EM_X86_64  → amd64
		emAArch64 = 183 // EM_AARCH64 → arm64
	)

	var machine uint16
	switch data[5] {
	case elfDataLittle:
		machine = uint16(data[18]) | uint16(data[19])<<8
	case elfDataBig:
		machine = uint16(data[18])<<8 | uint16(data[19])
	default:
		return nil // unknown ELF endianness — skip check
	}

	var binaryArch string
	switch machine {
	case emX86_64:
		binaryArch = "amd64"
	case emAArch64:
		binaryArch = "arm64"
	default:
		return nil // unrecognised ELF machine type — let the device decide
	}

	if binaryArch != deviceArch {
		return fmt.Errorf("binary is %s but device is %s; provide the correct binary with --binary", binaryArch, deviceArch)
	}
	return nil
}

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
