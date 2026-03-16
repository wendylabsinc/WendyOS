package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
)

const defaultAgentPort = 50051

const lanAddressProbeTimeout = 1500 * time.Millisecond

var getAgentVersionAtAddress = func(ctx context.Context, address string) (*agentpb.GetAgentVersionResponse, error) {
	conn, err := connectWithAutoTLS(ctx, address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	return conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
}

// ErrUserCancelled is returned when the user cancels an interactive prompt (e.g. Ctrl+C).
var ErrUserCancelled = errors.New("cancelled")

// hostPort formats a host and port into an address string,
// wrapping IPv6 addresses in brackets as required by RFC 3986.
func hostPort(host string, port int) string {
	if net.ParseIP(host) != nil && strings.Contains(host, ":") {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// lanAgentAddresses returns candidate gRPC addresses for a LAN device.
// Prefer the discovered IP address so commands still work when .local
// hostname resolution is unavailable on the host machine.
func lanAgentAddresses(dev models.LANDevice) []string {
	port := dev.Port
	if port == 0 {
		port = defaultAgentPort
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
// returns the first one that answers GetAgentVersion.
func resolveLANAgentVersion(ctx context.Context, dev models.LANDevice) (string, *agentpb.GetAgentVersionResponse, error) {
	var lastErr error
	for _, address := range lanAgentAddresses(dev) {
		attemptCtx, cancel := context.WithTimeout(ctx, lanAddressProbeTimeout)
		resp, err := getAgentVersionAtAddress(attemptCtx, address)
		cancel()
		if err == nil {
			return address, resp, nil
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no LAN address available for %q", dev.DisplayName)
	}
	return "", nil, lastErr
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
			_, resp, err := resolveLANAgentVersion(ctx, *d)
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
// version metadata and returns the enriched device.
func resolveLANVersion(ctx context.Context, dev models.LANDevice) (models.LANDevice, error) {
	_, resp, err := resolveLANAgentVersion(ctx, dev)
	if err != nil {
		return dev, err
	}
	dev.AgentVersion = resp.GetVersion()
	dev.OS = resp.GetOs()
	dev.OSVersion = resp.GetOsVersion()
	dev.CPUArchitecture = resp.GetCpuArchitecture()
	return dev, nil
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
func resolveDeviceAddress() (string, error) {
	hostname := deviceFlag
	if hostname == "" {
		cfg, err := config.Load()
		if err != nil {
			return "", fmt.Errorf("loading config: %w", err)
		}
		hostname = cfg.DefaultDevice
	}
	if hostname == "" {
		return "", fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}
	return hostPort(hostname, defaultAgentPort), nil
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

	addr, err := resolveDeviceAddress()
	if err == nil {
		conn, connErr := connectWithAutoTLS(ctx, addr)
		if connErr != nil {
			return nil, connErr
		}
		// Preserve the .local mDNS hostname for registry operations so we
		// avoid IPv6 literal formatting issues when pushing container images.
		if strings.HasSuffix(conn.Host, ".local") {
			conn.Hostname = conn.Host
		}
		if !cfg.suppressProvisioningHint {
			suggestProvisioning(conn)
		}
		return conn, nil
	}

	// No device configured — fall back to interactive picker.
	if jsonOutput {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	target, pickErr := pickDevice(ctx, cfg.excludeProviderKeys, cfg.excludeBluetooth)
	if pickErr != nil {
		return nil, pickErr
	}

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
// falling back to plaintext if no certs are available or mTLS connection fails.
func connectWithAutoTLS(ctx context.Context, plaintextAddr string) (*grpcclient.AgentConnection, error) {
	certInfo := loadCLICert()
	if certInfo != nil {
		// Derive the mTLS port (plaintext port + 1).
		host, portStr, _ := net.SplitHostPort(plaintextAddr)
		if port, err := strconv.Atoi(portStr); err == nil {
			mtlsAddr := hostPort(host, port+1)
			conn, tlsErr := grpcclient.ConnectWithTLS(ctx, mtlsAddr, certInfo)
			if tlsErr == nil {
				// grpc.NewClient is lazy — verify the connection actually
				// works with a fast probe before committing to mTLS.
				probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				_, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
				cancel()
				if probeErr == nil {
					return conn, nil
				}
				conn.Close()
			}
			// mTLS failed — fall back to plaintext.
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

// loadCLICert returns the first available certificate from the CLI config, or nil.
func loadCLICert() *config.CertificateInfo {
	auth := loadCLIAuth()
	if auth == nil {
		return nil
	}
	cert := auth.Certificates[0]
	return &cert
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
	nonInteractive           bool
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
	if device == "" {
		cfg, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
		device = cfg.DefaultDevice
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
		conn, err := connectWithAutoTLS(ctx, addr)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(conn.Host, ".local") {
			conn.Hostname = conn.Host
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	// No device specified — run interactive picker if we have a TTY.
	if jsonOutput || cfg.nonInteractive {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	return pickDevice(ctx, cfg.excludeProviderKeys, cfg.excludeBluetooth)
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
	projectType := detectProjectType(dir)
	switch projectType {
	case "python":
		language = "python"
	case "swift":
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
}

// pickDevice runs an interactive TUI that discovers devices across all
// transports and providers, then lets the user select one.
// LAN discovery runs continuously so devices that come online after the
// initial scan still appear in the picker.
// excludeProviders hides the named provider keys from the picker.
func pickDevice(ctx context.Context, excludeProviders map[string]bool, excludeBluetooth bool) (*SelectedDevice, error) {
	picker := tui.NewPicker()
	picker.MergeItem = mergePickerItem
	p := tea.NewProgram(picker)

	// Cancel continuous discovery when the picker exits.
	discoverCtx, discoverCancel := context.WithCancel(ctx)

	// Continuous LAN discovery — devices appear as they're found.
	lanCh := make(chan models.LANDevice, 16)
	go discovery.DiscoverLANContinuous(discoverCtx, lanCh)
	go func() {
		for dev := range lanCh {
			dev, _ = resolveLANVersion(discoverCtx, dev)
			name := dev.DisplayName
			if dev.AgentVersion != "" {
				name += " v" + dev.AgentVersion
			}
			lanDev := dev // copy for stable pointer
			p.Send(tui.PickerAddMsg{Items: []tui.PickerItem{{
				Name:     name,
				Type:     "LAN",
				Address:  preferredLANAddress(dev),
				DedupKey: dev.DisplayName,
				Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
					DisplayName:     dev.DisplayName,
					AgentVersion:    dev.AgentVersion,
					OS:              dev.OS,
					OSVersion:       dev.OSVersion,
					CPUArchitecture: dev.CPUArchitecture,
					LAN:             &lanDev,
				}},
			}}})
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
			addr, _, err := resolveLANAgentVersion(ctx, *d.LAN)
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
				conn.Hostname = d.LAN.Hostname
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
