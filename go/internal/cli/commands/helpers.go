package commands

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const defaultAgentPort = 50051

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
	return fmt.Sprintf("%s:%d", hostname, defaultAgentPort), nil
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

	target, pickErr := pickDevice(ctx)
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

// resolveTarget inspects the --device flag and returns either an external
// provider device or falls back to the gRPC agent connection. If no device
// is specified and no default is configured, an interactive device picker
// is presented.
func resolveTarget(ctx context.Context) (*SelectedDevice, error) {
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
		addr := fmt.Sprintf("%s:%d", device, defaultAgentPort)
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

	return pickDevice(ctx)
}

// pickerEntry is the value stored in each PickerItem.
type pickerEntry struct {
	lanDevice       *models.LANDevice
	bluetoothDevice *models.BluetoothDevice
	externalDevice  *models.ExternalDevice
	provider        providers.DeviceProvider
}

// pickDevice runs an interactive TUI that discovers devices across all
// transports and providers, then lets the user select one.
func pickDevice(ctx context.Context) (*SelectedDevice, error) {
	picker := tui.NewPicker()
	p := tea.NewProgram(picker)

	// Run discovery in the background, feeding results to the TUI.
	go func() {
		discoverCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()

		// Discover LAN/USB/Ethernet/BLE devices.
		opts := discovery.DiscoveryOptions{Timeout: 5 * time.Second}
		collection, _ := discovery.Discover(discoverCtx, opts)

		if collection != nil {
			var items []tui.PickerItem
			for i := range collection.LANDevices {
				d := &collection.LANDevices[i]
				addr := d.Hostname
				if d.IPAddress != "" {
					addr = d.IPAddress
				}
				name := d.DisplayName
				if d.AgentVersion != "" {
					name += " v" + d.AgentVersion
				}
				items = append(items, tui.PickerItem{
					Name:    name,
					Type:    "LAN",
					Address: fmt.Sprintf("%s:%d", addr, d.Port),
					Value:   &pickerEntry{lanDevice: d},
				})
			}
			for i := range collection.BluetoothDevices {
				d := &collection.BluetoothDevices[i]
				name := d.DisplayName
				if d.AgentVersion != "" {
					name += " v" + d.AgentVersion
				}
				items = append(items, tui.PickerItem{
					Name:    name,
					Type:    "Bluetooth",
					Address: d.Address,
					Value:   &pickerEntry{bluetoothDevice: d},
				})
			}
			if len(items) > 0 {
				p.Send(tui.PickerAddMsg{Items: items})
			}
		}

		// Discover external provider devices.
		for _, prov := range providers.AvailableProviders() {
			devices, err := prov.DiscoverDevices(discoverCtx)
			if err != nil || len(devices) == 0 {
				continue
			}
			var items []tui.PickerItem
			for i := range devices {
				d := &devices[i]
				items = append(items, tui.PickerItem{
					Name:    d.DisplayName,
					Type:    "External",
					Address: fmt.Sprintf("%s: %s", d.ProviderKey, d.ID),
					Value:   &pickerEntry{externalDevice: d, provider: prov},
				})
			}
			p.Send(tui.PickerAddMsg{Items: items})
		}

		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("device picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	sel := pm.Selected()
	if sel == nil {
		return nil, fmt.Errorf("no device selected")
	}

	entry, ok := sel.Value.(*pickerEntry)
	if !ok {
		return nil, fmt.Errorf("invalid picker selection")
	}

	// LAN device → gRPC connection.
	if entry.lanDevice != nil {
		d := entry.lanDevice
		host := d.Hostname
		if d.IPAddress != "" {
			host = d.IPAddress
		}
		addr := fmt.Sprintf("%s:%d", host, d.Port)
		conn, err := grpcclient.Connect(ctx, addr)
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	// Bluetooth device.
	if entry.bluetoothDevice != nil {
		return &SelectedDevice{Bluetooth: entry.bluetoothDevice}, nil
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
