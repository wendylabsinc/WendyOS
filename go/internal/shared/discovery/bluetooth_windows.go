//go:build windows

package discovery

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const (
	wendyBLEServiceUUID     = "7565e9eb-4c20-4b67-9272-d708b397b631"
	wendyLiteBLEServiceUUID = "00004e57-454e-4459-0001-000000000000"
	wendyL2CAPPSM           = 128
)

// discoverBluetooth queries Windows for known Bluetooth devices with "Wendy" in the name.
// Unlike macOS (CoreBluetooth) and Linux (bluetoothctl), this cannot perform active
// BLE scanning without WinRT. Only paired or previously cached devices are found.
func discoverBluetooth(ctx context.Context, _ bool) ([]models.BluetoothDevice, error) {
	// Query both BLE (BTHLE) and classic Bluetooth (BTHENUM) PnP devices.
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-PnpDevice | Where-Object { ($_.InstanceId -like 'BTHLE\*' -or $_.InstanceId -like 'BTHENUM\*') -and $_.FriendlyName -like '*Wendy*' } | Select-Object FriendlyName, InstanceId, Status | ConvertTo-Json -Compress`)
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	if !strings.HasPrefix(trimmed, "[") {
		trimmed = "[" + trimmed + "]"
	}

	var entries []struct {
		FriendlyName string `json:"FriendlyName"`
		InstanceId   string `json:"InstanceId"`
		Status       string `json:"Status"`
	}
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, nil
	}

	var devices []models.BluetoothDevice
	seen := make(map[string]bool)

	for _, entry := range entries {
		address := extractBTAddress(entry.InstanceId)
		if address == "" || seen[address] {
			continue
		}
		seen[address] = true

		isLite := strings.HasPrefix(entry.FriendlyName, "Wendy-")
		psm := uint16(wendyL2CAPPSM)
		if isLite {
			psm = 0
		}

		displayName := entry.FriendlyName
		if displayName == "" {
			if isLite {
				displayName = "Wendy Lite"
			} else {
				displayName = "WendyOS Device"
			}
		}

		devices = append(devices, models.BluetoothDevice{
			ID:            address,
			DisplayName:   displayName,
			Address:       address,
			IsWendyDevice: true,
			L2CAPPSM:      psm,
		})
	}

	return devices, nil
}

// extractBTAddress extracts a Bluetooth MAC address from a Windows PnP InstanceId.
// BTHLE format: BTHLE\Dev_AABBCCDDEEFF\...
// BTHENUM format: BTHENUM\{guid}_VID&..._AABBCCDDEEFF\...
func extractBTAddress(instanceId string) string {
	upper := strings.ToUpper(instanceId)

	// BTHLE format: look for "Dev_" followed by 12 hex chars.
	if idx := strings.Index(upper, "DEV_"); idx >= 0 {
		rest := upper[idx+4:]
		if sepIdx := strings.IndexByte(rest, '\\'); sepIdx >= 0 {
			rest = rest[:sepIdx]
		}
		if len(rest) == 12 && isHexString(rest) {
			return formatBTAddress(rest)
		}
	}

	// BTHENUM format: the address is the last 12-char hex segment before a backslash.
	parts := strings.Split(upper, "_")
	for i := len(parts) - 1; i >= 0; i-- {
		segment := parts[i]
		if sepIdx := strings.IndexByte(segment, '\\'); sepIdx >= 0 {
			segment = segment[:sepIdx]
		}
		if len(segment) == 12 && isHexString(segment) {
			return formatBTAddress(segment)
		}
	}

	return instanceId
}

// formatBTAddress formats "AABBCCDDEEFF" as "AA:BB:CC:DD:EE:FF".
func formatBTAddress(hex string) string {
	if len(hex) != 12 {
		return hex
	}
	return hex[0:2] + ":" + hex[2:4] + ":" + hex[4:6] + ":" + hex[6:8] + ":" + hex[8:10] + ":" + hex[10:12]
}

func isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) > 0
}
