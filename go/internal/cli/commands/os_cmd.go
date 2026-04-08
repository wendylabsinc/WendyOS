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
	"sort"
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

			// Step 1: Ensure the agent is at the latest release before updating the OS.
			versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("querying device version: %w", err)
			}
			conn, err = ensureAgentUpToDate(ctx, conn, versionResp, nightly)
			if err != nil {
				return err
			}
			// Re-query after the potential agent restart.
			versionResp, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
			if err != nil {
				return fmt.Errorf("querying device version after agent update: %w", err)
			}

			// Check mender support after the agent update (a fresh agent may have it).
			hasMender := false
			for _, f := range versionResp.GetFeatureset() {
				if f == "mender" {
					hasMender = true
					break
				}
			}
			if !hasMender {
				return fmt.Errorf("device does not support OTA updates (mender-update not found)")
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
							// Strip the "WendyOS-" display prefix before comparing so that
							// "WendyOS-0.10.4" and "0.12.0-nightly" compare correctly.
							normalizedOsVer := strings.TrimPrefix(osVer, "WendyOS-")
							alreadyCurrent := nightly && latestVer == normalizedOsVer ||
								!nightly && version.CompareVersions(latestVer, normalizedOsVer) <= 0
							if alreadyCurrent {
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

				servedURL, cleanup, err := serveLocalArtifact(localPath, localIP)
				if err != nil {
					return err
				}
				defer cleanup()
				artifactURL = servedURL
				fmt.Printf("Serving artifact at: %s\n", artifactURL)
			} else if artifactURL != "" && !deviceHasWiFi(ctx, conn) {
				// Device has no WiFi connection — it cannot reach GCP directly.
				// Download the artifact on the Mac and serve it over a local HTTP
				// server so the device can fetch it from the Mac instead.
				fmt.Println("Device is not connected to WiFi — downloading artifact to serve locally...")
				localPath, err := downloadArtifactToTemp(artifactURL)
				if err != nil {
					return fmt.Errorf("downloading artifact: %w", err)
				}

				localIP, err := localIPForHost(conn.Host)
				if err != nil {
					return fmt.Errorf("determining local IP for device %s: %w", conn.Host, err)
				}

				servedURL, cleanup, err := serveLocalArtifact(localPath, localIP)
				if err != nil {
					return err
				}
				defer cleanup()
				artifactURL = servedURL
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
	// Sort newest-first for a stable, predictable picker.
	sort.Slice(versionItems, func(i, j int) bool {
		vi, _ := versionItems[i].Value.(string)
		vj, _ := versionItems[j].Value.(string)
		return version.CompareVersions(vi, vj) > 0
	})

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
// The returned string is suitable for net.Listen: for IPv6 link-local addresses
// it includes the zone identifier (e.g. "fe80::1%en0"). Use ipForURL to convert
// it to a safe form for embedding in HTTP URLs.
func localIPForHost(host string) (string, error) {
	// Strip port if present.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	// dialHost is what we pass to net.Dial (may include an IPv6 zone).
	dialHost := host

	// Detect whether the host is already an IP literal. For IPv6 link-local
	// addresses the zone identifier (e.g. "%en0") must be stripped before
	// calling net.ParseIP, but preserved for dialing.
	var parsedIP net.IP
	if i := strings.Index(host, "%"); i != -1 {
		parsedIP = net.ParseIP(host[:i])
	} else {
		parsedIP = net.ParseIP(host)
	}

	if parsedIP == nil {
		// Not an IP literal — resolve via DNS.
		ips, err := net.LookupHost(host)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", host, err)
		}
		// Prefer IPv4; fall back to the first result.
		for _, ip := range ips {
			if p := net.ParseIP(ip); p != nil && p.To4() != nil {
				parsedIP = p
				dialHost = ip
				break
			}
		}
		if parsedIP == nil && len(ips) > 0 {
			dialHost = ips[0]
			parsedIP = net.ParseIP(ips[0])
		}
		if parsedIP == nil {
			return "", fmt.Errorf("no addresses found for %s", host)
		}
	}

	network := "udp4"
	if parsedIP.To4() == nil {
		network = "udp6"
	}
	dialAddr := net.JoinHostPort(dialHost, fmt.Sprintf("%d", defaultAgentPort))

	// Use UDP dial to determine which local IP would be used to reach the target.
	// No actual packets are sent — this just queries the routing table.
	conn, err := net.Dial(network, dialAddr)
	if err != nil {
		return "", fmt.Errorf("determining route to %s: %w", dialHost, err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr.Zone != "" {
		return localAddr.IP.String() + "%" + localAddr.Zone, nil
	}
	return localAddr.IP.String(), nil
}

// ipForURL converts a local IP (possibly "fe80::1%en0") to the host component
// for an HTTP URL. IPv6 zone IDs are percent-encoded per RFC 6874 so the raw
// '%' does not produce an invalid URL.
func ipForURL(ip string) string {
	if i := strings.Index(ip, "%"); i != -1 {
		return ip[:i] + "%25" + ip[i+1:]
	}
	return ip
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

	// For nightly builds, update whenever the device isn't already running that
	// exact tag — a semver comparison would incorrectly treat nightly pre-release
	// tags as older than a stable release of the same base version.
	alreadyCurrent := nightly && release.TagName == agentVer ||
		!nightly && version.CompareVersions(release.TagName, agentVer) <= 0
	if alreadyCurrent {
		fmt.Printf("Agent is up to date (%s)\n", agentVer)
		return conn, nil
	}

	fmt.Printf("Updating agent: %s → %s\n", agentVer, release.TagName)
	addr := hostPort(conn.Host, defaultAgentPort)
	if err := performAgentUpdate(ctx, conn, arch, nightly); err != nil {
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

// serveLocalArtifact starts a temporary HTTP server bound to localIP that
// serves the file at localPath. It returns the URL at which the file is
// accessible and a cleanup function that shuts down the server.
func serveLocalArtifact(localPath, localIP string) (string, func(), error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(localIP, "0"))
	if err != nil {
		return "", nil, fmt.Errorf("starting file server: %w", err)
	}

	_, portStr, _ := net.SplitHostPort(listener.Addr().String())
	fileName := filepath.Base(localPath)
	escapedFileName := url.PathEscape(fileName)
	urlPath := artifactURLPath(localPath)

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		listener.Close()
		return "", nil, fmt.Errorf("generating token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	servedURL := "http://" + net.JoinHostPort(ipForURL(localIP), portStr) + "/" + urlPath + "/" + token + "/" + escapedFileName

	mux := http.NewServeMux()
	mux.HandleFunc("/"+urlPath+"/"+token+"/"+escapedFileName, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, localPath)
	})
	server := &http.Server{Handler: mux}
	go server.Serve(listener) //nolint:errcheck

	cleanup := func() {
		server.Close()
		listener.Close()
	}
	return servedURL, cleanup, nil
}

// downloadArtifactToTemp downloads a remote artifact URL to a temporary file,
// showing a progress bar. The caller is responsible for removing the file.
func downloadArtifactToTemp(artifactURL string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(artifactURL) //nolint:noctx
	if err != nil {
		return "", fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	cacheDir, err := osCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving cache dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(cacheDir, "wendyos-*.mender")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	total := resp.ContentLength
	prog := tui.NewProgress("Downloading artifact...")
	p := tea.NewProgram(prog)

	go func() {
		var downloaded int64
		buf := make([]byte, 64*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
					p.Send(tui.ProgressDoneMsg{Err: writeErr})
					return
				}
				downloaded += int64(n)
				if total > 0 {
					p.Send(tui.ProgressUpdateMsg{
						Percent: float64(downloaded) / float64(total),
						Written: downloaded,
						Total:   total,
					})
				}
			}
			if readErr == io.EOF {
				p.Send(tui.ProgressDoneMsg{})
				return
			}
			if readErr != nil {
				p.Send(tui.ProgressDoneMsg{Err: readErr})
				return
			}
		}
	}()

	finalModel, err := p.Run()
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("progress TUI: %w", err)
	}

	model := finalModel.(tui.ProgressModel)
	if model.Err() != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", model.Err()
	}

	tmpFile.Close()
	return tmpFile.Name(), nil
}

// deviceHasWiFi returns true if the device reports an active WiFi connection.
// On error (e.g. older firmware that doesn't support the call) it returns true
// so we fall back to the GCP URL rather than breaking the update flow.
func deviceHasWiFi(ctx context.Context, conn *grpcclient.AgentConnection) bool {
	status, err := conn.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		return true
	}
	return status.GetConnected()
}
