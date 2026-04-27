package commands

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
)

const defaultAgentPort = 50051

const lanAddressProbeTimeout = 1500 * time.Millisecond

var getAgentVersionAtAddress = func(ctx context.Context, address string) (bool, *agentpb.GetAgentVersionResponse, error) {
	conn, err := connectWithAutoTLS(ctx, address)
	if err != nil {
		return false, nil, err
	}
	defer conn.Close()

	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	return conn.IsMTLS, resp, err
}

var isInteractiveTerminalFn = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

var runAgentConnectionSpinner = func(ctx context.Context, label string, fn func(context.Context) (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	prog := tea.NewProgram(tui.NewSpinner(label))

	var (
		conn   *grpcclient.AgentConnection
		runErr error
		doneCh = make(chan struct{})
	)
	go func() {
		defer close(doneCh)
		conn, runErr = fn(ctx)
		// Keep spinner teardown quiet; callers handle the returned error.
		prog.Send(tui.SpinnerDoneMsg{})
	}()

	finalModel, err := prog.Run()
	if err != nil {
		cancel()
		<-doneCh
		if conn != nil {
			conn.Close()
		}
		return nil, fmt.Errorf("spinner TUI: %w", err)
	}

	if sm, ok := finalModel.(tui.SpinnerModel); ok && !sm.Done() {
		cancel()
		<-doneCh
		if conn != nil {
			conn.Close()
		}
		return nil, ErrUserCancelled
	}

	<-doneCh
	return conn, runErr
}

// ErrUserCancelled is returned when the user cancels an interactive prompt (e.g. Ctrl+C).
var ErrUserCancelled = errors.New("cancelled")

// ErrDefaultCleared is returned after the user chooses to unset the default
// device from the recovery menu. main.go treats this as a graceful exit (code 0).
var ErrDefaultCleared = errors.New("default device cleared")

// hostPort formats a host and port into an address string,
// wrapping IPv6 addresses in brackets as required by RFC 3986.
// Uses netip.ParseAddr so IPv6 link-local addresses with zone IDs
// (e.g. fe80::1%en0) are correctly detected and bracketed.
func hostPort(host string, port int) string {
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// resolveHostPreferIPv4 resolves a hostname to a concrete IP address,
// preferring IPv4 over global IPv6. If the input is already an IP address
// or resolution fails, it returns the input unchanged.
func resolveHostPreferIPv4(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return host // already an IP
	}

	addrs, err := net.LookupHost(host)
	if err != nil || len(addrs) == 0 {
		return host
	}

	var globalIPv6 string
	for _, a := range addrs {
		addr, parseErr := netip.ParseAddr(a)
		if parseErr != nil {
			continue
		}
		if addr.Is4() {
			return a
		}
		if !addr.IsLinkLocalUnicast() && globalIPv6 == "" {
			globalIPv6 = addr.WithZone("").String()
		}
	}
	if globalIPv6 != "" {
		return globalIPv6
	}
	return host // only link-local IPv6 found — keep hostname for zone-aware dial
}

// lanAgentAddresses returns candidate gRPC addresses for a LAN device.
// Prefer the discovered IP address so commands still work when .local
// hostname resolution is unavailable on the host machine.
//
// For provisioned (mTLS) devices the Avahi advertisement carries the mTLS
// port. connectWithAutoTLS derives the mTLS port as plaintext+1, so we
// subtract 1 here to keep that convention working correctly.
func lanAgentAddresses(dev models.LANDevice) []string {
	port := dev.Port
	if port == 0 {
		port = defaultAgentPort
	}
	if dev.IsMTLS && dev.Port != 0 && port > 1 {
		port-- // advertised port is mTLS; connectWithAutoTLS will add 1 back
	}

	var addresses []string
	seen := make(map[string]bool)
	for _, host := range []string{strings.TrimSpace(dev.IPAddress), strings.TrimSpace(dev.Hostname)} {
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		addresses = append(addresses, hostPort(host, port))
	}

	return addresses
}

// preferredLANAddress returns the best available address for display and
// follow-up connection attempts. It prefers IPs over mDNS hostnames.
func preferredLANAddress(dev models.LANDevice) string {
	addresses := lanAgentAddresses(dev)
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0]
}

// resolveLANAgentVersion tries the discovered LAN addresses in order and
// returns the first one that answers GetAgentVersion, along with whether that
// connection used mTLS.
func resolveLANAgentVersion(ctx context.Context, dev models.LANDevice) (string, bool, *agentpb.GetAgentVersionResponse, error) {
	var lastErr error
	for _, address := range lanAgentAddresses(dev) {
		attemptCtx, cancel := context.WithTimeout(ctx, lanAddressProbeTimeout)
		isMTLS, resp, err := getAgentVersionAtAddress(attemptCtx, address)
		cancel()
		if err == nil {
			return address, isMTLS, resp, nil
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no LAN address available for %q", dev.DisplayName)
	}
	return "", false, nil, lastErr
}

// resolveLANVersions queries each LAN device's gRPC endpoint concurrently to
// populate AgentVersion, OS, OSVersion, and CPUArchitecture.
// Devices stay in the returned slice even when the metadata probe fails.
func resolveLANVersions(ctx context.Context, devices []models.LANDevice) []models.LANDevice {
	type indexedResult struct {
		index int
		resp  *agentpb.GetAgentVersionResponse
	}

	ch := make(chan *indexedResult, len(devices))
	for i := range devices {
		go func(idx int) {
			d := &devices[idx]
			_, _, resp, err := resolveLANAgentVersion(ctx, *d)
			if err != nil {
				ch <- &indexedResult{index: idx}
				return
			}
			ch <- &indexedResult{index: idx, resp: resp}
		}(i)
	}

	for range devices {
		r := <-ch
		if r != nil && r.resp != nil {
			devices[r.index].AgentVersion = r.resp.GetVersion()
			devices[r.index].OS = r.resp.GetOs()
			devices[r.index].OSVersion = r.resp.GetOsVersion()
			devices[r.index].CPUArchitecture = r.resp.GetCpuArchitecture()
		}
	}
	return devices
}

// resolveLANVersion queries a single LAN device's gRPC endpoint to populate
// version metadata. It also returns whether that connection used mTLS.
func resolveLANVersion(ctx context.Context, dev models.LANDevice) (models.LANDevice, bool, error) {
	_, isMTLS, resp, err := resolveLANAgentVersion(ctx, dev)
	if err != nil {
		return dev, false, err
	}
	dev.AgentVersion = resp.GetVersion()
	dev.OS = resp.GetOs()
	dev.OSVersion = resp.GetOsVersion()
	dev.CPUArchitecture = resp.GetCpuArchitecture()
	return dev, isMTLS, nil
}

// SelectedDevice represents either a gRPC agent, BLE device, or an external provider device.
type SelectedDevice struct {
	// Exactly one of Agent/Bluetooth/External is set.
	Agent     *grpcclient.AgentConnection
	Bluetooth *models.BluetoothDevice
	External  *models.ExternalDevice
	Provider  providers.DeviceProvider
}

// Close releases any resources held by this SelectedDevice.
func (s *SelectedDevice) Close() {
	if s.Agent != nil {
		s.Agent.Close()
	}
}

// resolveDeviceAddress returns the gRPC address for the target device.
// It checks the --device flag first, then the default device from config.
// The returned isDefault flag is true when the address came from the saved
// default device (not the --device flag).
func resolveDeviceAddress() (addr string, isDefault bool, err error) {
	hostname := deviceFlag
	if hostname == "" {
		cfg, loadErr := config.Load()
		if loadErr != nil {
			return "", false, fmt.Errorf("loading config: %w", loadErr)
		}
		hostname = cfg.DefaultDevice
		isDefault = hostname != ""
	}
	if hostname == "" {
		return "", false, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}
	return hostPort(hostname, defaultAgentPort), isDefault, nil
}

// recoveryChoice represents the user's selection in the default-device recovery menu.
type recoveryChoice int

const (
	recoveryDiscover     recoveryChoice = iota // run device discovery picker
	recoveryUnsetDefault                       // clear the default device
	recoveryExit                               // exit with the original error
)

// recoveryModel is a minimal Bubble Tea model for the default-device recovery menu.
type recoveryModel struct {
	choices  []string
	cursor   int
	chosen   int
	hostname string
	quit     bool
}

func (m recoveryModel) Init() tea.Cmd { return nil }

func (m recoveryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "enter":
			m.chosen = m.cursor
			return m, tea.Quit
		case "q", "ctrl+c":
			m.chosen = len(m.choices) - 1 // treat as Exit
			m.quit = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m recoveryModel) View() string {
	if m.quit {
		return ""
	}

	warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")) // amber
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	selectStyle := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)

	var sb strings.Builder
	sb.WriteString(warnStyle.Render(fmt.Sprintf("Attempting to reach default device %q but it is unavailable.", m.hostname)))
	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("Would you like to:"))
	sb.WriteString("\n")

	for i, choice := range m.choices {
		if i == m.cursor {
			sb.WriteString(selectStyle.Render("  > " + choice))
		} else {
			sb.WriteString(dimStyle.Render("    " + choice))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// promptDefaultDeviceRecovery shows an interactive menu when the saved default
// device is unreachable. It returns the user's chosen recovery action.
func promptDefaultDeviceRecovery(hostname string) recoveryChoice {
	m := recoveryModel{
		hostname: hostname,
		choices: []string{
			"Discover another device",
			"Unset the default device",
			"Exit",
		},
	}
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return recoveryExit
	}
	fm, ok := final.(recoveryModel)
	if !ok {
		return recoveryExit
	}
	return recoveryChoice(fm.chosen)
}

// isInteractiveTerminal returns true when both stdin and stdout are TTYs,
// meaning it is safe to show interactive Bubble Tea prompts.
func isInteractiveTerminal() bool {
	return isInteractiveTerminalFn()
}

// handleDefaultDeviceRecovery runs the recovery flow after a default device
// connection failure. Shows a warning and immediately opens the device picker
// where the user can select a new device and optionally set/unset default
// via 'd'/'x' shortcuts.
func handleDefaultDeviceRecovery(ctx context.Context, hostname string, elapsed time.Duration, _ error, excludeProviders map[string]bool, excludeBluetooth bool) (*SelectedDevice, error) {
	warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	fmt.Println(warnStyle.Render(fmt.Sprintf("⚠ Default device %q is unreachable after %s.", hostname, formatElapsedSeconds(elapsed))))
	fmt.Println()

	return pickDevice(ctx, excludeProviders, excludeBluetooth, false)
}

func defaultDeviceSearchLabel(hostname string) string {
	return fmt.Sprintf("Searching for default device %q...", hostname)
}

func formatElapsedSeconds(elapsed time.Duration) string {
	roundedElapsed := elapsed.Round(10 * time.Millisecond)
	seconds := roundedElapsed.Seconds()
	unit := "seconds"
	if roundedElapsed == time.Second {
		unit = "second"
	}
	return fmt.Sprintf("%.2f %s", seconds, unit)
}

func connectAgentAtAddress(ctx context.Context, addr string, probePlaintext bool) (*grpcclient.AgentConnection, error) {
	conn, err := connectWithAutoTLS(ctx, addr)
	if err != nil {
		return nil, err
	}
	if probePlaintext && !conn.IsMTLS {
		// gRPC plaintext connections are lazy — probe to detect unreachable
		// default devices early so recovery can be offered immediately.
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		cancel()
		if probeErr != nil {
			conn.Close()
			return nil, probeErr
		}
	}
	return conn, nil
}

func connectResolvedAgent(ctx context.Context, hostname, addr string, isDefault bool) (*grpcclient.AgentConnection, error) {
	if isDefault && !jsonOutput && isInteractiveTerminal() {
		return runAgentConnectionSpinner(ctx, defaultDeviceSearchLabel(hostname), func(spinCtx context.Context) (*grpcclient.AgentConnection, error) {
			return connectAgentAtAddress(spinCtx, addr, true)
		})
	}
	return connectAgentAtAddress(ctx, addr, isDefault)
}

// connectToAgent establishes a gRPC connection to the target device.
// If the CLI has auth certs, it connects via mTLS on the secure port.
// Otherwise, it falls back to plaintext on the default port.
// If no device is specified via --device or config default, an interactive
// device picker is presented (unless running in --json mode).
func connectToAgent(ctx context.Context, opts ...resolveOption) (*grpcclient.AgentConnection, error) {
	cfg := resolveConfig{excludeProviderKeys: make(map[string]bool)}
	for _, o := range opts {
		o(&cfg)
	}

	addr, isDefault, err := resolveDeviceAddress()
	if err == nil {
		startedAt := time.Now()
		hostname := addr
		if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
			hostname = host
		}
		conn, connErr := connectResolvedAgent(ctx, hostname, addr, isDefault)
		if connErr != nil {
			if errors.Is(connErr, ErrUserCancelled) {
				return nil, connErr
			}
			// Default device is unreachable — offer interactive recovery.
			if isDefault && !jsonOutput && isInteractiveTerminal() {
				hostname, _, _ := net.SplitHostPort(addr)
				target, recErr := handleDefaultDeviceRecovery(ctx, hostname, time.Since(startedAt), connErr, cfg.excludeProviderKeys, cfg.excludeBluetooth)
				if recErr != nil {
					return nil, recErr
				}
				return connectFromSelectedDevice(target, cfg)
			}
			return nil, connErr
		}
		if !cfg.suppressProvisioningHint {
			suggestProvisioning(conn)
		}
		if !cfg.suppressUpdateCheck {
			var updateErr error
			conn, updateErr = checkAndOfferUpdate(ctx, conn)
			if updateErr != nil {
				return nil, updateErr
			}
		}
		return conn, nil
	}

	// No device configured — fall back to interactive picker.
	if jsonOutput {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	target, pickErr := pickDevice(ctx, cfg.excludeProviderKeys, cfg.excludeBluetooth, cfg.suppressUpdateCheck)
	if pickErr != nil {
		return nil, pickErr
	}

	return connectFromSelectedDevice(target, cfg)
}

// connectFromSelectedDevice converts a SelectedDevice from the picker into a
// gRPC AgentConnection. Returns an error if the selected device does not
// support gRPC.
func connectFromSelectedDevice(target *SelectedDevice, cfg resolveConfig) (*grpcclient.AgentConnection, error) {
	if target.Agent != nil {
		if !cfg.suppressProvisioningHint {
			suggestProvisioning(target.Agent)
		}
		return target.Agent, nil
	}

	// The user picked a Bluetooth device — connectToAgent only supports gRPC.
	// Callers that support BLE should use resolveTarget() instead.
	if target.Bluetooth != nil {
		return nil, fmt.Errorf("selected device (%s) is a Bluetooth device; this command requires a LAN connection. Use 'wendy wifi connect' which supports BLE", target.Bluetooth.DisplayName)
	}

	// The user picked a non-gRPC device (e.g. external provider) which
	// doesn't support agent commands like wifi/apps/hardware.
	if target.External != nil {
		return nil, fmt.Errorf("selected device (%s) does not support this command; select a WendyOS LAN device instead", target.External.DisplayName)
	}

	return nil, fmt.Errorf("selected device does not support gRPC agent commands")
}

// connectWithAutoTLS tries to connect using mTLS if the CLI has auth certs,
// falling back to plaintext if no certs are available or all mTLS attempts fail.
// It tries each stored certificate in order so that both production and local
// pki-core certs are attempted.
func connectWithAutoTLS(ctx context.Context, plaintextAddr string) (*grpcclient.AgentConnection, error) {
	allCerts := loadAllCLICerts()
	if len(allCerts) > 0 {
		host, portStr, _ := net.SplitHostPort(plaintextAddr)
		if port, err := strconv.Atoi(portStr); err == nil {
			mtlsAddr := hostPort(host, port+1)
			for i := range allCerts {
				conn, tlsErr := grpcclient.ConnectWithTLS(ctx, mtlsAddr, &allCerts[i])
				if tlsErr != nil {
					continue
				}
				// grpc.NewClient is lazy — verify the connection actually
				// works with a fast probe before committing to mTLS.
				// 8s allows time for mDNS (.local) resolution + TCP + TLS handshake.
				probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				_, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
				cancel()
				if probeErr == nil {
					return conn, nil
				}
				conn.Close()
			}
		}
	}
	return grpcclient.Connect(ctx, plaintextAddr)
}

// suggestProvisioning prints a hint when the connection is not using mTLS,
// nudging the user to provision the device.
func suggestProvisioning(conn *grpcclient.AgentConnection) {
	if conn.IsMTLS || jsonOutput {
		return
	}
	fmt.Fprintf(os.Stderr, "Hint: connected without mTLS. Run 'wendy device setup' to provision this device.\n")
}

// checkAndOfferUpdate probes the agent version and, when the agent is behind
// the CLI, either warns (non-interactive) or prompts [Y/n] (interactive). If
// the user accepts, it downloads the latest release, uploads it, and waits for
// the agent to restart, returning a fresh connection. On decline, or if the
// upload fails, the original conn is returned unchanged. If the upload succeeds
// but the agent does not come back, conn is closed and an error is returned.
func checkAndOfferUpdate(ctx context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
	if jsonOutput {
		return conn, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	resp, err := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	cancel()
	if err != nil {
		return conn, nil
	}

	agentVer := resp.GetVersion()
	// Dev CLI builds skip the update check entirely.
	if version.Version == "dev" {
		return conn, nil
	}
	// Unknown agent version — skip to avoid spurious update prompts.
	if agentVer == "" {
		return conn, nil
	}
	if version.CompareVersions(version.Version, agentVer) <= 0 {
		return conn, nil
	}

	if !isInteractiveTerminal() {
		fmt.Fprintf(os.Stderr, "Warning: agent is behind the CLI (agent: %s, CLI: %s). Run 'wendy device update' to update.\n", agentVer, version.Version)
		return conn, nil
	}
	warn := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))

	fmt.Fprintf(os.Stderr, warn.Render("Agent is behind the CLI (agent: %s, CLI: %s). Update now? [Y/n] "), agentVer, version.Version)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	if answer != "" && answer != "y" && answer != "yes" {
		return conn, nil
	}

	arch := resp.GetCpuArchitecture()
	addr := hostPort(conn.Host, defaultAgentPort)

	if err := performAgentUpdate(ctx, conn, arch, false); err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\nContinuing with existing connection.\n", err)
		return conn, nil
	}

	conn.Close()

	fmt.Fprintf(os.Stderr, "Waiting for agent to restart...")
	newConn, err := waitForAgentRestart(ctx, addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, " failed.\n")
		return nil, fmt.Errorf("agent did not come back after update: %w", err)
	}
	fmt.Fprintf(os.Stderr, " ready.\n")
	return newConn, nil
}

// performAgentUpdate downloads the latest release for the given arch and uploads
// it to conn. Pass nightly=true to fetch the latest prerelease instead of stable.
// The agent will restart after this returns successfully.
func performAgentUpdate(ctx context.Context, conn *grpcclient.AgentConnection, arch string, nightly bool) error {
	if arch == "" {
		return fmt.Errorf("device did not report CPU architecture")
	}
	fmt.Fprintf(os.Stderr, "Fetching latest release...\n")
	release, err := fetchAgentRelease(nightly)
	if err != nil {
		return fmt.Errorf("fetching release: %w", err)
	}

	assetPrefix := fmt.Sprintf("wendy-agent-linux-%s-", arch)
	var matchedAsset *githubReleaseAsset
	for _, a := range release.Assets {
		if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, ".tar.gz") {
			matchedAsset = &a
			break
		}
	}
	if matchedAsset == nil {
		return fmt.Errorf("no asset for linux/%s in release %s", arch, release.TagName)
	}

	fmt.Fprintf(os.Stderr, "Downloading %s...\n", matchedAsset.Name)
	binaryData, err := downloadAgentBinary(*matchedAsset)
	if err != nil {
		return fmt.Errorf("downloading binary: %w", err)
	}

	h := sha256.Sum256(binaryData)
	sha256Hash := hex.EncodeToString(h[:])

	fmt.Fprintf(os.Stderr, "Uploading to device...\n")
	return deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash)
}

// waitForAgentRestart polls addr with connectWithAutoTLS until the agent answers
// GetAgentVersion or 60 s elapse. Returns a fresh connection on success.
func waitForAgentRestart(ctx context.Context, addr string) (*grpcclient.AgentConnection, error) {
	deadline := time.Now().Add(60 * time.Second)
	time.Sleep(time.Second) // give the agent a moment to begin shutdown
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := connectWithAutoTLS(ctx, addr)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		cancel()
		if probeErr == nil {
			return conn, nil
		}
		conn.Close()
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("timed out waiting for agent to restart")
}

// loadCLICert returns the first available certificate from the CLI config, or nil.
func loadCLICert() *config.CertificateInfo {
	auth := loadCLIAuth()
	if auth == nil {
		return nil
	}
	cert := auth.Certificates[0]
	return &cert
}

// loadAllCLICerts returns the first certificate from each auth entry that has
// one. Used by connectWithAutoTLS to try all available certs in order.
func loadAllCLICerts() []config.CertificateInfo {
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil
	}
	var out []config.CertificateInfo
	for _, auth := range cfg.Auth {
		if len(auth.Certificates) > 0 {
			out = append(out, auth.Certificates[0])
		}
	}
	return out
}

// loadCLIAuth returns the first auth entry that has certificates, or nil.
func loadCLIAuth() *config.AuthConfig {
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil
	}
	for _, auth := range cfg.Auth {
		if len(auth.Certificates) > 0 {
			return &auth
		}
	}
	return nil
}

// resolveOption configures resolveTarget behaviour.
type resolveOption func(*resolveConfig)

type resolveConfig struct {
	excludeProviderKeys      map[string]bool
	excludeBluetooth         bool
	suppressProvisioningHint bool
	suppressUpdateCheck      bool
	nonInteractive           bool
}

// SuppressUpdateCheck prevents connectToAgent from running the automatic
// agent-version check. Use this for commands that manage updates explicitly
// (e.g. "wendy device update") to avoid a double-prompt.
func SuppressUpdateCheck() resolveOption {
	return func(c *resolveConfig) {
		c.suppressUpdateCheck = true
	}
}

// SuppressProvisioningHint prevents connectToAgent from printing the
// "run 'wendy device setup'" hint when connected without mTLS.
func SuppressProvisioningHint() resolveOption {
	return func(c *resolveConfig) {
		c.suppressProvisioningHint = true
	}
}

// NonInteractive prevents resolveTarget from opening an interactive device
// picker. When no device is specified in non-interactive mode, a clear error
// is returned instead of attempting to open a TTY.
func NonInteractive() resolveOption {
	return func(c *resolveConfig) {
		c.nonInteractive = true
	}
}

// ExcludeBluetooth skips the BLE scan and filters out BLE-only devices
// (those with no LAN or external endpoint) from the interactive device picker.
func ExcludeBluetooth() resolveOption {
	return func(c *resolveConfig) {
		c.excludeBluetooth = true
	}
}

// ExcludeProviders prevents the named provider keys from appearing in the
// interactive device picker.
func ExcludeProviders(keys ...string) resolveOption {
	return func(c *resolveConfig) {
		for _, k := range keys {
			c.excludeProviderKeys[k] = true
		}
	}
}

// resolveTarget inspects the --device flag and returns either an external
// provider device or falls back to the gRPC agent connection. If no device
// is specified and no default is configured, an interactive device picker
// is presented.
func resolveTarget(ctx context.Context, opts ...resolveOption) (*SelectedDevice, error) {
	cfg := resolveConfig{excludeProviderKeys: make(map[string]bool)}
	for _, o := range opts {
		o(&cfg)
	}
	device := deviceFlag
	isDefault := false
	if device == "" {
		loadedCfg, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
		device = loadedCfg.DefaultDevice
		isDefault = device != ""
	}

	// Check if the device flag matches a known provider key.
	if device != "" {
		if p := providers.ProviderForKey(device); p != nil {
			devices, err := p.DiscoverDevices(ctx)
			if err != nil {
				return nil, fmt.Errorf("discovering %s devices: %w", p.DisplayName(), err)
			}
			if len(devices) == 0 {
				return nil, fmt.Errorf("no %s devices found", p.DisplayName())
			}
			return &SelectedDevice{
				External: &devices[0],
				Provider: p,
			}, nil
		}
	}

	// Check if the device flag matches a discovered device ID (e.g. "adb:emulator-5554").
	if device != "" {
		if sel := findDeviceByID(ctx, device); sel != nil {
			return sel, nil
		}
	}

	// If a device hostname was given, connect via gRPC (with mTLS if authenticated).
	if device != "" {
		addr := hostPort(device, defaultAgentPort)
		startedAt := time.Now()
		conn, err := connectResolvedAgent(ctx, device, addr, isDefault)
		if err != nil {
			if errors.Is(err, ErrUserCancelled) {
				return nil, err
			}
			// Default device is unreachable — offer interactive recovery.
			if isDefault && !jsonOutput && !cfg.nonInteractive && isInteractiveTerminal() {
				return handleDefaultDeviceRecovery(ctx, device, time.Since(startedAt), err, cfg.excludeProviderKeys, cfg.excludeBluetooth)
			}
			return nil, err
		}
		if !cfg.suppressUpdateCheck {
			var updateErr error
			conn, updateErr = checkAndOfferUpdate(ctx, conn)
			if updateErr != nil {
				return nil, updateErr
			}
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	// No device specified — run interactive picker if we have a TTY.
	if jsonOutput || cfg.nonInteractive {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	return pickDevice(ctx, cfg.excludeProviderKeys, cfg.excludeBluetooth, cfg.suppressUpdateCheck)
}

// findDeviceByID searches all available providers for a device whose ID
// matches the given string (e.g. "adb:emulator-5554").
func findDeviceByID(ctx context.Context, id string) *SelectedDevice {
	for _, p := range providers.AvailableProviders() {
		devices, err := p.DiscoverDevices(ctx)
		if err != nil {
			continue
		}
		for _, d := range devices {
			if d.ID == id {
				d := d // copy for stable pointer
				return &SelectedDevice{
					External: &d,
					Provider: p,
				}
			}
		}
	}
	return nil
}

// ensureAppConfig loads wendy.json from cfgPath. If the file does not exist
// and stdin is a TTY (or autoAccept is true), a default config is created automatically.
func ensureAppConfig(cfgPath string, autoAccept bool) (*appconfig.AppConfig, error) {
	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err == nil {
		return cfg, nil
	}

	// If the error is anything other than "file not found", return it as-is
	// (e.g. a JSON parse error).
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	dir := filepath.Dir(cfgPath)
	dirName := filepath.Base(dir)

	if !autoAccept {
		// File doesn't exist. If we're not in an interactive terminal, give a
		// helpful error message.
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return nil, fmt.Errorf("wendy.json not found; run 'wendy init <app-id>' to create one")
		}

		fmt.Println("No wendy.json found in current directory.")
		fmt.Printf("Create one with app ID %q? [Y/n] ", dirName)

		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))

		if answer != "" && answer != "y" && answer != "yes" {
			return nil, fmt.Errorf("wendy.json is required; run 'wendy init <app-id>' to create one")
		}
	}

	// Detect language from the project files on disk.
	language := ""
	projectType, _ := detectProjectType(dir) // ignore multiple-xcodeproj error for config init
	switch projectType {
	case "python":
		language = "python"
	case "swift":
		language = "swift"
	case "xcode":
		language = "swift"
	}

	entitlements := defaultEntitlements(language, "")

	newCfg := &appconfig.AppConfig{
		AppID:        dirName,
		Version:      "0.1.0",
		Language:     language,
		Entitlements: entitlements,
	}

	data, marshalErr := json.MarshalIndent(newCfg, "", "  ")
	if marshalErr != nil {
		return nil, fmt.Errorf("marshaling config: %w", marshalErr)
	}

	if writeErr := os.WriteFile(cfgPath, data, 0o644); writeErr != nil {
		return nil, fmt.Errorf("writing wendy.json: %w", writeErr)
	}

	fmt.Printf("Created wendy.json for %s\n", dirName)
	return newCfg, nil
}

// pickerItemDeviceID extracts a hostname or provider key from a picker item,
// suitable for storing as the default device (must be resolvable by resolveDeviceAddress).
func pickerItemDeviceID(item tui.PickerItem) string {
	entry, ok := item.Value.(*pickerEntry)
	if !ok {
		return ""
	}
	// For LAN devices, use the mDNS hostname (matches what pickDeviceForDefault returns).
	if entry.mergedDevice != nil && entry.mergedDevice.LAN != nil {
		addr := entry.mergedDevice.LAN.Hostname
		if addr == "" {
			addr = entry.mergedDevice.LAN.IPAddress
		}
		return addr
	}
	if entry.externalDevice != nil {
		return entry.externalDevice.ProviderKey
	}
	if entry.mergedDevice != nil && entry.mergedDevice.External != nil {
		return entry.mergedDevice.External.ProviderKey
	}
	return ""
}

// pickerEntry is the value stored in each PickerItem.
type pickerEntry struct {
	mergedDevice   *models.DiscoveredDevice
	externalDevice *models.ExternalDevice
	provider       providers.DeviceProvider
}

// mergePickerItem merges a newly discovered transport into an existing picker
// item for the same physical device. It combines connection types, prefers
// LAN addresses, and merges the underlying DiscoveredDevice fields.
func mergePickerItem(existing *tui.PickerItem, incoming tui.PickerItem) {
	e, eOK := existing.Value.(*pickerEntry)
	n, nOK := incoming.Value.(*pickerEntry)
	if !eOK || !nOK || e.mergedDevice == nil || n.mergedDevice == nil {
		return
	}

	md := e.mergedDevice
	nd := n.mergedDevice

	if nd.LAN != nil && md.LAN == nil {
		md.LAN = nd.LAN
		existing.Address = incoming.Address
	}
	if nd.Bluetooth != nil && md.Bluetooth == nil {
		md.Bluetooth = nd.Bluetooth
	}
	if nd.External != nil && md.External == nil {
		md.External = nd.External
		if md.LAN == nil {
			existing.Address = incoming.Address
		}
	}

	if md.AgentVersion == "" {
		md.AgentVersion = nd.AgentVersion
	}
	if md.OS == "" {
		md.OS = nd.OS
	}
	if md.OSVersion == "" {
		md.OSVersion = nd.OSVersion
	}
	if md.CPUArchitecture == "" {
		md.CPUArchitecture = nd.CPUArchitecture
	}

	// Prefer the name that includes the version suffix.
	if len(incoming.Name) > len(existing.Name) {
		existing.Name = incoming.Name
	}

	// Rebuild the type string from the merged transports.
	existing.Type = md.ConnectionTypes()

	// Propagate security status: LAN probes determine mTLS, BLE doesn't. Once
	// we know a device is insecure (or secure), update the existing item.
	if nd.LAN != nil {
		existing.Insecure = incoming.Insecure
	}
}

// pickDevice runs an interactive TUI that discovers devices across all
// transports and providers, then lets the user select one.
// LAN discovery runs continuously so devices that come online after the
// initial scan still appear in the picker.
// excludeProviders hides the named provider keys from the picker.
func pickDevice(ctx context.Context, excludeProviders map[string]bool, excludeBluetooth bool, suppressUpdateCheck bool) (*SelectedDevice, error) {
	picker := tui.NewPicker()
	picker.MergeItem = mergePickerItem

	// Load current default device to show ★ indicator.
	if loadedCfg, err := config.Load(); err == nil && loadedCfg.DefaultDevice != "" {
		picker.DefaultKey = strings.ToLower(loadedCfg.DefaultDevice)
	}

	// Allow 'd' to set default and 'x' to unset default from the picker.
	picker.OnSetDefault = func(item tui.PickerItem) {
		deviceID := pickerItemDeviceID(item)
		if deviceID == "" {
			return
		}
		if cfg, err := config.Load(); err == nil {
			cfg.DefaultDevice = deviceID
			_ = config.Save(cfg)
		}
	}
	picker.OnUnsetDefault = func() {
		if cfg, err := config.Load(); err == nil {
			cfg.DefaultDevice = ""
			_ = config.Save(cfg)
		}
	}

	p := tea.NewProgram(picker)

	// Cancel continuous discovery when the picker exits.
	discoverCtx, discoverCancel := context.WithCancel(ctx)

	// Continuous LAN discovery — devices appear as they're found.
	lanCh := make(chan models.LANDevice, 16)
	go discovery.DiscoverLANContinuous(discoverCtx, lanCh)
	sendLANItem := func(dev models.LANDevice, insecure bool) {
		name := dev.DisplayName
		if dev.AgentVersion != "" {
			name += " v" + dev.AgentVersion
		}
		devCopy := dev
		p.Send(tui.PickerAddMsg{Items: []tui.PickerItem{{
			Name:     name,
			Type:     "LAN",
			Address:  preferredLANAddress(dev),
			DedupKey: dev.DisplayName,
			Insecure: insecure,
			Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
				DisplayName:     dev.DisplayName,
				AgentVersion:    dev.AgentVersion,
				OS:              dev.OS,
				OSVersion:       dev.OSVersion,
				CPUArchitecture: dev.CPUArchitecture,
				LAN:             &devCopy,
			}},
		}}})
	}
	go func() {
		for rawDev := range lanCh {
			resolved, isMTLS, err := resolveLANVersion(discoverCtx, rawDev)
			sendLANItem(resolved, err == nil && !isMTLS)
			if err != nil {
				// Version probe failed on first attempt. Retry in background so
				// the version appears once the device becomes responsive, without
				// requiring it to be rediscovered via mDNS.
				go func(d models.LANDevice) {
					for attempt := 0; attempt < 5; attempt++ {
						select {
						case <-discoverCtx.Done():
							return
						case <-time.After(2 * time.Second):
						}
						if updated, isMTLS, retryErr := resolveLANVersion(discoverCtx, d); retryErr == nil {
							sendLANItem(updated, !isMTLS)
							return
						}
					}
				}(rawDev)
			}
		}
	}()

	// Continuous provider discovery — re-scan every 3 seconds.
	for _, prov := range providers.AvailableProviders() {
		if excludeProviders[prov.Key()] {
			continue
		}
		prov := prov
		go func() {
			for {
				devices, err := prov.DiscoverDevices(discoverCtx)
				if err == nil && len(devices) > 0 {
					var items []tui.PickerItem
					for i := range devices {
						if prov.Key() == "wendy-lite" {
							items = append(items, tui.PickerItem{
								Name:     devices[i].DisplayName,
								DedupKey: devices[i].DisplayName,
								Type:     "LAN (Lite)",
								Address:  devices[i].ConnectionInfo["ip"],
								Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
									DisplayName:     devices[i].DisplayName,
									CPUArchitecture: devices[i].CPUArchitecture,
									External:        &devices[i],
								}},
							})
						} else {
							items = append(items, tui.PickerItem{
								Name:    devices[i].DisplayName,
								Type:    prov.DisplayName(),
								Address: fmt.Sprintf("%s: %s", devices[i].ProviderKey, devices[i].ID),
								Value:   &pickerEntry{externalDevice: &devices[i], provider: prov},
							})
						}
					}
					if len(items) > 0 {
						p.Send(tui.PickerAddMsg{Items: items})
					}
				}

				select {
				case <-discoverCtx.Done():
					return
				case <-time.After(3 * time.Second):
				}
			}
		}()
	}

	// Continuous Bluetooth discovery — re-scan every 5 seconds.
	if !excludeBluetooth {
		go func() {
			for {
				bleDevices, err := discovery.DiscoverBluetooth(discoverCtx, true)
				if err == nil && len(bleDevices) > 0 {
					var items []tui.PickerItem
					for i := range bleDevices {
						connType := "Bluetooth"
						if !bleDevices[i].IsWendyAgent() {
							connType = "BLE (Lite)"
						}
						items = append(items, tui.PickerItem{
							Name:     bleDevices[i].DisplayName,
							DedupKey: bleDevices[i].DisplayName,
							Type:     connType,
							Address:  bleDevices[i].Address,
							Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
								DisplayName:     bleDevices[i].DisplayName,
								AgentVersion:    bleDevices[i].AgentVersion,
								OS:              bleDevices[i].OS,
								OSVersion:       bleDevices[i].OSVersion,
								CPUArchitecture: bleDevices[i].CPUArchitecture,
								Bluetooth:       &bleDevices[i],
							}},
						})
					}
					p.Send(tui.PickerAddMsg{Items: items})
				}

				select {
				case <-discoverCtx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}

	finalModel, err := p.Run()
	discoverCancel() // stop all background discovery
	if err != nil {
		return nil, fmt.Errorf("device picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return nil, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return nil, fmt.Errorf("no device selected")
	}

	entry, ok := sel.Value.(*pickerEntry)
	if !ok {
		return nil, fmt.Errorf("invalid picker selection")
	}

	// Merged LAN/Bluetooth/External device — prefer LAN (gRPC), fall back to BLE/External.
	if entry.mergedDevice != nil {
		d := entry.mergedDevice
		if d.LAN != nil {
			addr, _, _, err := resolveLANAgentVersion(ctx, *d.LAN)
			if err != nil {
				// LAN metadata lookups can fail on provisioned devices without CLI certs.
				// In that case, still try the preferred address once before falling back.
				addr = preferredLANAddress(*d.LAN)
			}
			if addr == "" {
				if d.Bluetooth != nil {
					return &SelectedDevice{Bluetooth: d.Bluetooth}, nil
				}
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("selected LAN device has no usable address")
			}
			conn, err := connectWithAutoTLS(ctx, addr)
			if err == nil {
				if !suppressUpdateCheck {
					var updateErr error
					conn, updateErr = checkAndOfferUpdate(ctx, conn)
					if updateErr != nil {
						return nil, updateErr
					}
				}
				return &SelectedDevice{Agent: conn}, nil
			}
			// LAN failed — fall back to BLE if available.
			if d.Bluetooth != nil {
				return &SelectedDevice{Bluetooth: d.Bluetooth}, nil
			}
			return nil, err
		}

		// Wendy Lite device — set both BLE and External+Provider when
		// available so callers can pick the right transport.
		sel := &SelectedDevice{}
		if d.Bluetooth != nil {
			sel.Bluetooth = d.Bluetooth
		}
		if d.External != nil {
			sel.External = d.External
			sel.Provider = providers.ProviderForKey(d.External.ProviderKey)
		}
		if sel.Bluetooth != nil || sel.External != nil {
			return sel, nil
		}
	}

	// External provider device.
	if entry.externalDevice != nil && entry.provider != nil {
		return &SelectedDevice{
			External: entry.externalDevice,
			Provider: entry.provider,
		}, nil
	}

	return nil, fmt.Errorf("selected device type is not yet supported")
}

// resolveAgentPlatform determines the target platform string from the user's
// wendy.json platform field, the agent's OS, and the agent's CPU architecture.
//
// Rules:
//   - If cfgPlatform is a full "os/arch" string, use it as-is.
//   - If cfgPlatform is OS-only (e.g., "linux" or "darwin"), append the agent arch.
//   - If cfgPlatform is empty, default to the agent's OS and architecture.
func resolveAgentPlatform(cfgPlatform, agentOS, agentArch string) string {
	if cfgPlatform == "" {
		return agentOS + "/" + agentArch
	}
	if strings.Contains(cfgPlatform, "/") {
		return cfgPlatform
	}
	// OS-only: append agent architecture.
	return cfgPlatform + "/" + agentArch
}

// registryPort returns the OCI registry port for the given agent OS.
// macOS uses 5555 to avoid conflicts with AirPlay Receiver which binds *:5000.
// All other platforms use the standard 5000.
func registryPort(agentOS string) int {
	if agentOS == "darwin" {
		return 5555
	}
	return 5000
}

// platformOS extracts the OS component from a platform string like "linux/arm64".
func platformOS(platform string) string {
	if i := strings.IndexByte(platform, '/'); i >= 0 {
		return platform[:i]
	}
	return platform
}
