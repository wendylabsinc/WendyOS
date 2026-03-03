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
	"golang.org/x/term"
)

const defaultAgentPort = 50051

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
// If no device is specified via --device or config default, an interactive
// device picker is presented (unless running in --json mode).
func connectToAgent(ctx context.Context) (*grpcclient.AgentConnection, error) {
	addr, err := resolveDeviceAddress()
	if err == nil {
		return grpcclient.Connect(ctx, addr)
	}

	// No device configured — fall back to interactive picker.
	if jsonOutput {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	target, pickErr := pickDevice(ctx, nil)
	if pickErr != nil {
		return nil, pickErr
	}

	if target.Agent != nil {
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

// resolveOption configures resolveTarget behaviour.
type resolveOption func(*resolveConfig)

type resolveConfig struct {
	excludeProviderKeys map[string]bool
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

	// If a device hostname was given, connect via gRPC.
	if device != "" {
		addr := hostPort(device, defaultAgentPort)
		conn, err := grpcclient.Connect(ctx, addr)
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	// No device specified — run interactive picker if we have a TTY.
	if jsonOutput {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	return pickDevice(ctx, cfg.excludeProviderKeys)
}

// ensureAppConfig loads wendy.json from cfgPath. If the file does not exist
// and stdin is a TTY, the user is prompted to create a default one.
func ensureAppConfig(cfgPath string) (*appconfig.AppConfig, error) {
	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err == nil {
		return cfg, nil
	}

	// If the error is anything other than "file not found", return it as-is
	// (e.g. a JSON parse error).
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// File doesn't exist. If we're not in an interactive terminal, give a
	// helpful error message.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("wendy.json not found; run 'wendy init <app-id>' to create one")
	}

	dir := filepath.Dir(cfgPath)
	dirName := filepath.Base(dir)

	fmt.Println("No wendy.json found in current directory.")
	fmt.Printf("Create one with app ID %q? [Y/n] ", dirName)

	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))

	if answer != "" && answer != "y" && answer != "yes" {
		return nil, fmt.Errorf("wendy.json is required; run 'wendy init <app-id>' to create one")
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

// pickDevice runs an interactive TUI that discovers devices across all
// transports and providers, then lets the user select one.
// excludeProviders hides the named provider keys from the picker.
func pickDevice(ctx context.Context, excludeProviders map[string]bool) (*SelectedDevice, error) {
	picker := tui.NewPicker()
	p := tea.NewProgram(picker)

	// Run discovery in the background, feeding results to the TUI.
	go func() {
		discoverCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		// Discover LAN/USB/Ethernet/BLE devices.
		opts := discovery.DiscoveryOptions{Timeout: 5 * time.Second}
		collection, _ := discovery.Discover(discoverCtx, opts)
		if collection == nil {
			collection = &models.DevicesCollection{}
		}

		// Discover external provider devices. Microwasm devices are added
		// to the collection so MergedDevices() can merge them with BLE Lite.
		var nonMicrowasmExternals []struct {
			device   *models.ExternalDevice
			provider providers.DeviceProvider
		}
		for _, prov := range providers.AvailableProviders() {
			if excludeProviders[prov.Key()] {
				continue
			}
			devices, err := prov.DiscoverDevices(discoverCtx)
			if err != nil || len(devices) == 0 {
				continue
			}
			if prov.Key() == "microwasm" {
				collection.ExternalDevices = append(collection.ExternalDevices, devices...)
			} else {
				for i := range devices {
					nonMicrowasmExternals = append(nonMicrowasmExternals, struct {
						device   *models.ExternalDevice
						provider providers.DeviceProvider
					}{device: &devices[i], provider: prov})
				}
			}
		}

		// Build merged picker items (LAN + BLE + microwasm).
		merged := collection.MergedDevices()
		var items []tui.PickerItem
		for i := range merged {
			d := &merged[i]
			name := d.DisplayName
			if d.AgentVersion != "" {
				name += " v" + d.AgentVersion
			}
			addr := d.Address()
			if d.Port() > 0 {
				addr = hostPort(addr, d.Port())
			}
			items = append(items, tui.PickerItem{
				Name:    name,
				Type:    d.ConnectionTypes(),
				Address: addr,
				Value:   &pickerEntry{mergedDevice: d},
			})
		}
		if len(items) > 0 {
			p.Send(tui.PickerAddMsg{Items: items})
		}

		// Add remaining non-microwasm external devices.
		var extItems []tui.PickerItem
		for _, ext := range nonMicrowasmExternals {
			extItems = append(extItems, tui.PickerItem{
				Name:    ext.device.DisplayName,
				Type:    "External",
				Address: fmt.Sprintf("%s: %s", ext.device.ProviderKey, ext.device.ID),
				Value:   &pickerEntry{externalDevice: ext.device, provider: ext.provider},
			})
		}
		if len(extItems) > 0 {
			p.Send(tui.PickerAddMsg{Items: extItems})
		}

		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
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
			addr := hostPort(d.LAN.Hostname, d.LAN.Port)
			conn, err := grpcclient.Connect(ctx, addr)
			if err == nil {
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
