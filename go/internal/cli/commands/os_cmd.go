package commands

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newOSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "os",
		Short: "Manage the WendyOS operating system",
	}

	cmd.AddCommand(newOSUpdateCmd())
	cmd.AddCommand(newOSListDrivesCmd())
	addOSInstallCmd(cmd)
	addOSDownloadCmd(cmd)
	addOSCacheCmd(cmd)
	return cmd
}

func newOSUpdateCmd() *cobra.Command {
	var artifactURL string
	var nightly bool

	cmd := &cobra.Command{
		Use:   "update [artifact-path]",
		Short: "Update WendyOS on the target device",
		Long: `Update WendyOS using a Mender artifact. Provide a local file path or directory
as a positional argument, or use --artifact-url for a remote URL.

When a local file is provided, the CLI serves it via a temporary HTTP server
so the device can download it directly.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			// Determine the artifact URL: local path, remote URL, or manifest picker.
			if len(args) > 0 && artifactURL != "" {
				return fmt.Errorf("provide either a local artifact path or --artifact-url, not both")
			}

			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			// Use a closure so the defer always closes the current conn even if
			// it is replaced by the agent pre-update step below.
			defer func() { conn.Close() }()

			// Check that the device has mender-update support.
			versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("checking device capabilities: %w", err)
			}

			checkMender := func(resp *agentpb.GetAgentVersionResponse) bool {
				for _, f := range resp.GetFeatureset() {
					if f == "mender" {
						return true
					}
				}
				return false
			}
			if !checkMender(versionResp) {
				return fmt.Errorf("device does not support OTA updates (mender-update not found)")
			}

			// Step 1: Ensure the agent is at the latest release before updating the OS.
			conn, err = ensureAgentUpToDate(ctx, conn, versionResp, nightly)
			if err != nil {
				return err
			}
			// Re-query after the potential agent restart.
			versionResp, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("querying device version after agent update: %w", err)
			}
			if !checkMender(versionResp) {
				return fmt.Errorf("device does not support OTA updates after agent update (mender-update not found)")
			}

			// Step 2: Show current OS version.
			if osVer := versionResp.GetOsVersion(); osVer != "" {
				fmt.Printf("Current OS version: %s\n", osVer)
			}

			// No artifact provided — try to auto-detect from device type, then picker.
			if len(args) == 0 && artifactURL == "" {
				deviceType := versionResp.GetDeviceType()
				if deviceType != "" {
					otaURL, latestVer, autoErr := getLatestOTAInfoForDeviceType(deviceType, nightly)
					if autoErr == nil {
						if osVer := versionResp.GetOsVersion(); osVer != "" && latestVer != "" {
							if version.CompareVersions(latestVer, osVer) <= 0 {
								fmt.Printf("OS is already at the latest version (%s).\n", osVer)
								return nil
							}
							fmt.Printf("Latest OS version: %s\n", latestVer)
						}
						artifactURL = otaURL
					}
				}
				if artifactURL == "" {
					url, pickErr := pickOTAArtifactURL()
					if pickErr != nil {
						return pickErr
					}
					artifactURL = url
				}
			}

			// If a local path is provided, resolve and serve it.
			if len(args) > 0 {
				localPath, err := resolveArtifactPath(args[0])
				if err != nil {
					return err
				}

				// Determine the local IP reachable by the device.
				localIP, err := localIPForHost(conn.Host)
				if err != nil {
					return fmt.Errorf("determining local IP for device %s: %w", conn.Host, err)
				}

				// Start HTTP server bound to the specific local IP reachable by the
				// device (not 0.0.0.0) with a random one-time token in the path.
				listener, err := net.Listen("tcp", net.JoinHostPort(localIP, "0"))
				if err != nil {
					return fmt.Errorf("starting file server: %w", err)
				}
				defer listener.Close()

				// Extract the port assigned by the OS.
				_, portStr, _ := net.SplitHostPort(listener.Addr().String())

				fileName := filepath.Base(localPath)
				escapedFileName := url.PathEscape(fileName)
				urlPath := artifactURLPath(localPath)

				// Generate a random one-time token to prevent unintended access.
				tokenBytes := make([]byte, 16)
				if _, err := rand.Read(tokenBytes); err != nil {
					return fmt.Errorf("generating token: %w", err)
				}
				token := hex.EncodeToString(tokenBytes)

				artifactURL = "http://" + net.JoinHostPort(localIP, portStr) + "/" + urlPath + "/" + token + "/" + escapedFileName

				// Serve the file in the background.
				mux := http.NewServeMux()
				mux.HandleFunc("/"+urlPath+"/"+token+"/"+escapedFileName, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/octet-stream")
					http.ServeFile(w, r, localPath)
				})
				server := &http.Server{Handler: mux}
				go server.Serve(listener) //nolint:errcheck
				defer server.Close()

				fmt.Printf("Serving artifact at: %s\n", artifactURL)
			}

			if artifactURL == "" {
				return fmt.Errorf("provide a local artifact path or --artifact-url")
			}

			stream, err := conn.AgentService.UpdateOS(ctx, &agentpb.UpdateOSRequest{
				ArtifactUrl: artifactURL,
			})
			if err != nil {
				return fmt.Errorf("starting OS update: %w", err)
			}

			spin := tui.NewSpinner("Downloading update...")
			p := tea.NewProgram(spin)

			go func() {
				for {
					resp, err := stream.Recv()
					if err == io.EOF {
						p.Send(tui.SpinnerDoneMsg{})
						return
					}
					if err != nil {
						p.Send(tui.SpinnerDoneMsg{Err: err})
						return
					}

					if progress := resp.GetProgress(); progress != nil {
						label := phaseLabel(progress.GetPhase())
						p.Send(tui.SpinnerUpdateMsg{Label: label})
					}

					if completed := resp.GetCompleted(); completed != nil {
						p.Send(tui.SpinnerDoneMsg{})
						return
					}

					if failed := resp.GetFailed(); failed != nil {
						p.Send(tui.SpinnerDoneMsg{Err: fmt.Errorf("update failed: %s", failed.GetErrorMessage())})
						return
					}
				}
			}()

			finalModel, err := p.Run()
			if err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}

			_, spinErr := finalModel.(tui.SpinnerModel).Result()
			if spinErr != nil {
				return spinErr
			}

			deviceHost := conn.Host
			fmt.Println("WendyOS update applied. Device is rebooting...")
			if err := waitForDeviceOnline(ctx, deviceHost); err != nil {
				return err
			}
			fmt.Println("Device is back online.")
			return nil
		},
	}

	cmd.Flags().StringVar(&artifactURL, "artifact-url", "", "Mender artifact URL (remote)")
	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use the latest nightly (prerelease) build for both agent and OS")

	return cmd
}

// resolveArtifactPath resolves a local file path or directory to a .mender artifact file.
func resolveArtifactPath(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("artifact not found: %w", err)
	}

	if !info.IsDir() {
		return absPath, nil
	}

	// Search directory for a .mender file.
	entries, err := os.ReadDir(absPath)
	if err != nil {
		return "", fmt.Errorf("reading directory: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".mender") || strings.HasSuffix(name, ".mender.xz") {
			fmt.Printf("Found artifact: %s\n", name)
			return filepath.Join(absPath, name), nil
		}
	}

	return "", fmt.Errorf("no .mender file found in directory: %s", absPath)
}

// artifactURLPath generates a short hash prefix for the URL path.
func artifactURLPath(filePath string) string {
	h := sha256.New()
	h.Write([]byte(filePath))
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// pickOTAArtifactURL interactively picks a device and version from the GCS
// manifest and returns the Mender artifact URL for the selected version.
func pickOTAArtifactURL() (string, error) {
	fmt.Println("Fetching available devices...")

	devices, err := getAvailableDevices()
	if err != nil {
		return "", fmt.Errorf("fetching device manifest: %w", err)
	}

	// Filter to devices that have at least one version with an OTA artifact.
	var items []tui.PickerItem
	deviceMap := make(map[string]deviceInfo)
	for _, dev := range devices {
		if dev.Manifest == nil {
			continue
		}
		for _, v := range dev.Manifest.Versions {
			if v.OTAUpdatePath != "" {
				deviceMap[dev.Key] = dev
				items = append(items, tui.PickerItem{
					Name:        dev.Name,
					Description: fmt.Sprintf("(latest: %s)", dev.LatestVersion),
					Value:       dev.Key,
				})
				break
			}
		}
	}
	if len(items) == 0 {
		return "", fmt.Errorf("no devices with OTA update support found in manifest")
	}

	fmt.Println()
	key, err := pickFromItems("Select a device", items)
	if err != nil {
		return "", err
	}
	dev := deviceMap[key]

	// Filter versions to those that have an OTA artifact.
	var versionItems []tui.PickerItem
	for ver, v := range dev.Manifest.Versions {
		if v.OTAUpdatePath == "" {
			continue
		}
		desc := ""
		if v.IsLatest {
			desc = "latest"
		} else if v.IsNightly {
			desc = "nightly"
		}
		versionItems = append(versionItems, tui.PickerItem{
			Name:        ver,
			Description: desc,
			Value:       ver,
		})
	}

	fmt.Println()
	ver, err := pickFromItems("Select a version", versionItems)
	if err != nil {
		return "", err
	}

	return getOTAUpdateURL(dev.Manifest, ver)
}

// waitForDeviceOnline polls the device until it responds to GetAgentVersion,
// or until a 5-minute timeout expires. Shows a spinner while waiting.
func waitForDeviceOnline(ctx context.Context, host string) error {
	addr := hostPort(host, defaultAgentPort)

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	spin := tui.NewSpinner("Waiting for device to come back online...")
	p := tea.NewProgram(spin)

	go func() {
		// Give the device a few seconds to begin rebooting before polling.
		time.Sleep(5 * time.Second)

		for {
			probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
			conn, err := connectWithAutoTLS(probeCtx, addr)
			probeCancel()
			if err == nil {
				probeCtx2, probeCancel2 := context.WithTimeout(ctx, 3*time.Second)
				_, probeErr := conn.AgentService.GetAgentVersion(probeCtx2, &agentpb.GetAgentVersionRequest{})
				probeCancel2()
				conn.Close()
				if probeErr == nil {
					p.Send(tui.SpinnerDoneMsg{})
					return
				}
			}

			select {
			case <-ctx.Done():
				p.Send(tui.SpinnerDoneMsg{Err: fmt.Errorf("timed out waiting for device to come back online")})
				return
			case <-time.After(3 * time.Second):
			}
		}
	}()

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	_, spinErr := finalModel.(tui.SpinnerModel).Result()
	return spinErr
}

// localIPForHost returns the local IP address used to reach the given host.
// This works for any connection type including link-local USB-C addresses.
func localIPForHost(host string) (string, error) {
	// Strip port if present.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// Resolve hostname to IP if needed.
	ips, err := net.LookupHost(host)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", host, err)
	}

	// Prefer IPv4 addresses; fall back to IPv6 if no IPv4 is available.
	var targetIP string
	for _, ip := range ips {
		parsed := net.ParseIP(ip)
		if parsed != nil && parsed.To4() != nil {
			targetIP = ip
			break
		}
	}
	if targetIP == "" && len(ips) > 0 {
		targetIP = ips[0]
	}
	if targetIP == "" {
		return "", fmt.Errorf("no addresses found for %s", host)
	}

	// Determine the network and dial address based on IP version.
	network := "udp4"
	if net.ParseIP(targetIP) == nil || net.ParseIP(targetIP).To4() == nil {
		network = "udp6"
	}
	dialAddr := net.JoinHostPort(targetIP, fmt.Sprintf("%d", defaultAgentPort))

	// Use UDP dial to determine which local IP would be used to reach the target.
	// No actual packets are sent — this just queries the routing table.
	conn, err := net.Dial(network, dialAddr)
	if err != nil {
		return "", fmt.Errorf("determining route to %s: %w", targetIP, err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

// phaseLabel converts a Mender phase string to a user-friendly spinner label.
func phaseLabel(phase string) string {
	switch phase {
	case "downloading":
		return "Downloading update..."
	case "installing":
		return "Installing update..."
	case "finalizing":
		return "Finalizing..."
	default:
		if phase != "" {
			return strings.ToUpper(phase[:1]) + phase[1:] + "..."
		}
		return "Updating WendyOS..."
	}
}

// ensureAgentUpToDate checks the agent version on the device against the latest
// stable GitHub release. If the device is behind, it downloads the latest binary,
// uploads it (causing the agent to restart), waits for it to come back, and
// returns a fresh connection. If the agent is already current or the check fails
// non-fatally, the original connection is returned unchanged.
func ensureAgentUpToDate(ctx context.Context, conn *grpcclient.AgentConnection, versionResp *agentpb.GetAgentVersionResponse, nightly bool) (*grpcclient.AgentConnection, error) {
	agentVer := versionResp.GetVersion()
	arch := versionResp.GetCpuArchitecture()

	fmt.Printf("Agent version: %s — checking for updates...\n", agentVer)

	release, err := fetchAgentRelease(nightly)
	if err != nil {
		fmt.Printf("Could not check for agent updates: %v\n", err)
		return conn, nil
	}

	if version.CompareVersions(release.TagName, agentVer) <= 0 {
		fmt.Printf("Agent is up to date (%s)\n", agentVer)
		return conn, nil
	}

	fmt.Printf("Updating agent: %s → %s\n", agentVer, release.TagName)
	addr := hostPort(conn.Host, defaultAgentPort)
	if err := performAgentUpdate(ctx, conn, arch); err != nil {
		return nil, fmt.Errorf("agent update failed: %w", err)
	}
	conn.Close()

	fmt.Print("Waiting for agent to restart...")
	newConn, err := waitForAgentRestart(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("agent did not come back after update: %w", err)
	}
	fmt.Println(" done.")
	return newConn, nil
}
