//go:build linux

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const (
	// wendyBLEServiceUUID is the 128-bit service UUID advertised by WendyOS BLE devices.
	wendyBLEServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b631"

	// wendyLiteBLEServiceUUID is the GATT service UUID advertised by Wendy Lite (ESP32) devices.
	wendyLiteBLEServiceUUID = "00004e57-454e-4459-0001-000000000000"

	// wendyL2CAPPSM is the L2CAP PSM used for gRPC-over-BLE.
	wendyL2CAPPSM = 128
)

// discoverBluetooth uses bluetoothctl to scan for WendyOS BLE devices on Linux.
// If activeScan is true, an LE scan runs for up to 5 seconds before listing devices.
func discoverBluetooth(ctx context.Context, activeScan bool) ([]models.BluetoothDevice, error) {
	// Check that bluetoothctl is available.
	if _, err := exec.LookPath("bluetoothctl"); err != nil {
		return nil, nil
	}

	if activeScan {
		// Power on the adapter (idempotent).
		_ = exec.CommandContext(ctx, "bluetoothctl", "power", "on").Run()

		// Run an LE scan for 5 seconds.
		scanCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		defer cancel()
		_ = exec.CommandContext(scanCtx, "bluetoothctl", "--timeout", "5", "scan", "on").Run()
	}

	// List all discovered devices.
	listCmd := exec.CommandContext(ctx, "bluetoothctl", "devices")
	out, err := listCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing bluetooth devices: %w", err)
	}

	var devices []models.BluetoothDevice
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
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

		// Query device info to check for the Wendy BLE service UUID.
		dev, ok := queryBluetoothDeviceInfo(ctx, address, name)
		if ok {
			devices = append(devices, dev)
		}
	}

	return devices, nil
}

// queryBluetoothDeviceInfo runs "bluetoothctl info <address>" and checks whether
// the device advertises the Wendy BLE service UUID. Returns the device and true
// if it's a Wendy device.
func queryBluetoothDeviceInfo(ctx context.Context, address, name string) (models.BluetoothDevice, bool) {
	infoCmd := exec.CommandContext(ctx, "bluetoothctl", "info", address)
	out, err := infoCmd.Output()
	if err != nil {
		return models.BluetoothDevice{}, false
	}

	info := strings.ToLower(string(out))

	// Check if either the WendyOS agent or Wendy Lite service UUID is advertised.
	hasAgent := strings.Contains(info, wendyBLEServiceUUID)
	hasLite := strings.Contains(info, wendyLiteBLEServiceUUID)

	// Wendy Lite (ESP32) devices may not advertise their service UUID;
	// fall back to matching the "Wendy-" BLE name prefix.
	if !hasAgent && !hasLite {
		if strings.HasPrefix(name, "Wendy-") {
			hasLite = true
		} else {
			return models.BluetoothDevice{}, false
		}
	}

	// If the agent UUID is present, treat as full agent even if lite UUID also appears.
	isLite := !hasAgent && hasLite

	psm := uint16(wendyL2CAPPSM)
	if isLite {
		psm = 0
	}

	dev := models.BluetoothDevice{
		ID:            address,
		DisplayName:   name,
		Address:       address,
		IsWendyDevice: true,
		L2CAPPSM:      psm,
	}

	// Parse RSSI if available (line like "  RSSI: -45").
	scanner := bufio.NewScanner(strings.NewReader(info))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "RSSI:") {
			fmt.Sscanf(strings.TrimPrefix(line, "RSSI:"), "%d", &dev.RSSI)
		}
		if strings.HasPrefix(line, "Alias:") {
			alias := strings.TrimSpace(strings.TrimPrefix(line, "Alias:"))
			if alias != "" {
				dev.DisplayName = alias
			}
		}
	}

	if dev.DisplayName == "" {
		if isLite {
			dev.DisplayName = "Wendy Lite"
		} else {
			dev.DisplayName = "WendyOS Device"
		}
	}

	return dev, true
}
