//go:build linux

package bluetooth

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// BlueZManager manages Bluetooth peripherals via bluetoothctl on Linux.
// This avoids a direct D-Bus dependency while providing the same functionality.
// For a full D-Bus implementation, use github.com/godbus/dbus/v5.
type BlueZManager struct {
	logger *zap.Logger
}

func newPlatformManager(logger *zap.Logger) Manager {
	return &BlueZManager{logger: logger}
}

// Scan starts a Bluetooth scan and returns discovered devices on the channel.
func (m *BlueZManager) Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral, 10)

	go func() {
		defer close(ch)

		// Start scanning via bluetoothctl.
		scanCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		// Power on the adapter.
		if out, err := exec.CommandContext(scanCtx, "bluetoothctl", "power", "on").CombinedOutput(); err != nil {
			m.logger.Warn("Failed to power on Bluetooth adapter", zap.Error(err), zap.String("output", string(out)))
			return
		}

		// Start scan.
		scanCmd := exec.CommandContext(scanCtx, "bluetoothctl", "--timeout", "8", "scan", "on")
		if out, err := scanCmd.CombinedOutput(); err != nil {
			m.logger.Debug("Bluetooth scan completed", zap.String("output", string(out)))
		}

		// List discovered devices.
		listCmd := exec.CommandContext(ctx, "bluetoothctl", "devices")
		output, err := listCmd.CombinedOutput()
		if err != nil {
			m.logger.Warn("Failed to list Bluetooth devices", zap.Error(err))
			return
		}

		var peripherals []*agentpb.DiscoveredBluetoothPeripheral
		for _, line := range strings.Split(string(output), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Format: "Device XX:XX:XX:XX:XX:XX DeviceName"
			parts := strings.SplitN(line, " ", 3)
			if len(parts) < 3 || parts[0] != "Device" {
				continue
			}

			address := parts[1]
			name := parts[2]

			peripheral := &agentpb.DiscoveredBluetoothPeripheral{
				Address: address,
				Name:    name,
			}

			// Check if paired/connected.
			infoOutput, infoErr := exec.CommandContext(ctx, "bluetoothctl", "info", address).CombinedOutput()
			if infoErr == nil {
				info := string(infoOutput)
				peripheral.Paired = strings.Contains(info, "Paired: yes")
				peripheral.Connected = strings.Contains(info, "Connected: yes")
				if strings.Contains(info, "Icon: audio") {
					peripheral.DeviceType = "audio"
				}
			}

			peripherals = append(peripherals, peripheral)
		}

		if len(peripherals) > 0 {
			select {
			case ch <- peripherals:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}

// Connect connects to a Bluetooth peripheral by address.
func (m *BlueZManager) Connect(ctx context.Context, address string, pair, trust bool) error {
	if trust {
		if out, err := exec.CommandContext(ctx, "bluetoothctl", "trust", address).CombinedOutput(); err != nil {
			m.logger.Warn("Failed to trust device", zap.Error(err), zap.String("output", string(out)))
		}
	}

	if pair {
		if out, err := exec.CommandContext(ctx, "bluetoothctl", "pair", address).CombinedOutput(); err != nil {
			return fmt.Errorf("pairing with %s: %w (output: %s)", address, err, string(out))
		}
	}

	out, err := exec.CommandContext(ctx, "bluetoothctl", "connect", address).CombinedOutput()
	if err != nil {
		return fmt.Errorf("connecting to %s: %w (output: %s)", address, err, string(out))
	}

	m.logger.Info("Connected to Bluetooth device", zap.String("address", address))
	return nil
}

// Disconnect disconnects from a Bluetooth peripheral.
func (m *BlueZManager) Disconnect(ctx context.Context, address string) error {
	out, err := exec.CommandContext(ctx, "bluetoothctl", "disconnect", address).CombinedOutput()
	if err != nil {
		return fmt.Errorf("disconnecting from %s: %w (output: %s)", address, err, string(out))
	}

	m.logger.Info("Disconnected from Bluetooth device", zap.String("address", address))
	return nil
}

// Forget removes a paired Bluetooth peripheral.
func (m *BlueZManager) Forget(ctx context.Context, address string) error {
	out, err := exec.CommandContext(ctx, "bluetoothctl", "remove", address).CombinedOutput()
	if err != nil {
		return fmt.Errorf("removing device %s: %w (output: %s)", address, err, string(out))
	}

	m.logger.Info("Forgot Bluetooth device", zap.String("address", address))
	return nil
}
