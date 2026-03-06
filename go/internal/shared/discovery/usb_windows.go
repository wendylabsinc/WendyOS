//go:build windows

package discovery

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// discoverUSB uses PowerShell and WMI to find USB-connected Wendy devices on Windows.
// It queries Win32_PnPEntity for USB devices and filters by name or ESP32 VID:PID.
func discoverUSB(ctx context.Context) ([]models.USBDevice, error) {
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command",
		`Get-CimInstance Win32_PnPEntity | Where-Object { $_.PNPDeviceID -like 'USB\VID_*' } | Select-Object Name, PNPDeviceID, Manufacturer | ConvertTo-Json -Compress`)
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	// PowerShell returns a single object (not an array) when there's one result.
	if !strings.HasPrefix(trimmed, "[") {
		trimmed = "[" + trimmed + "]"
	}

	var entries []struct {
		Name         string `json:"Name"`
		PNPDeviceID  string `json:"PNPDeviceID"`
		Manufacturer string `json:"Manufacturer"`
	}
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, nil
	}

	var devices []models.USBDevice
	for _, entry := range entries {
		vid, pid := parseVIDPID(entry.PNPDeviceID)

		isWendy := strings.Contains(strings.ToLower(entry.Name), "wendy")
		isESP32 := strings.EqualFold(vid, models.ESP32VendorID) && strings.EqualFold(pid, models.ESP32ProductID)

		if !isWendy && !isESP32 {
			continue
		}

		dev := models.USBDevice{
			Name:          entry.Name,
			DisplayName:   entry.Name,
			VendorID:      vid,
			ProductID:     pid,
			IsWendyDevice: true,
			IsESP32:       isESP32,
		}

		if isESP32 {
			dev.DisplayName = "ESP32-C6"
		}

		// Extract serial number from PNPDeviceID (third segment).
		// Format: USB\VID_XXXX&PID_XXXX\serial_number
		parts := strings.Split(entry.PNPDeviceID, `\`)
		if len(parts) >= 3 {
			dev.SerialNumber = parts[2]
		}

		devices = append(devices, dev)
	}

	return devices, nil
}

// parseVIDPID extracts vendor and product IDs from a Windows PNP device ID.
// Format: "USB\VID_303A&PID_1001\serial_number"
func parseVIDPID(pnpDeviceID string) (vid, pid string) {
	upper := strings.ToUpper(pnpDeviceID)

	if idx := strings.Index(upper, "VID_"); idx >= 0 {
		rest := upper[idx+4:]
		end := strings.IndexAny(rest, `\&`)
		if end >= 0 {
			vid = "0x" + strings.ToLower(rest[:end])
		} else if len(rest) >= 4 {
			vid = "0x" + strings.ToLower(rest[:4])
		}
	}

	if idx := strings.Index(upper, "PID_"); idx >= 0 {
		rest := upper[idx+4:]
		end := strings.IndexAny(rest, `\&`)
		if end >= 0 {
			pid = "0x" + strings.ToLower(rest[:end])
		} else if len(rest) >= 4 {
			pid = "0x" + strings.ToLower(rest[:4])
		}
	}

	return vid, pid
}
