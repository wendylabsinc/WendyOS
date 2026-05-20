//go:build windows

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// ResolveESP32SerialPort finds the serial port for an ESP32-C6 device on
// Windows. It queries Win32_PnPEntity via PowerShell for Ports-class entries
// matching the Espressif VID/PID and extracts the COMN identifier from the
// device's Name or Caption field.
func ResolveESP32SerialPort() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	vid := strings.ToUpper(strings.TrimPrefix(models.ESP32VendorID, "0x"))
	pid := strings.ToUpper(strings.TrimPrefix(models.ESP32ProductID, "0x"))

	script := fmt.Sprintf(
		`Get-CimInstance Win32_PnPEntity | Where-Object { $_.PNPClass -eq 'Ports' -and $_.PNPDeviceID -like 'USB\VID_%s&PID_%s*' } | Select-Object Name, PNPDeviceID, Caption | ConvertTo-Json -Compress`,
		vid, pid,
	)

	cmd := exec.CommandContext(ctx, powershellExe, "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("querying Win32_PnPEntity for ESP32 serial port: %w", err)
	}

	return parseESP32SerialPortJSON(string(out))
}

// serialPortRegex matches a parenthesized COM port suffix such as "(COM5)".
var serialPortRegex = regexp.MustCompile(`\(COM\d+\)`)

// parseESP32SerialPortJSON extracts the first COMN port name from a JSON blob
// produced by `Get-CimInstance Win32_PnPEntity | Select-Object Name,
// PNPDeviceID, Caption | ConvertTo-Json`. Returns the bare "COMN" string
// (matching what os_install.go and esptool expect on Windows).
func parseESP32SerialPortJSON(jsonOut string) (string, error) {
	trimmed := strings.TrimSpace(jsonOut)
	if trimmed == "" {
		return "", noESP32SerialPortErr()
	}
	// PowerShell returns a single object (not an array) when there's one result.
	if !strings.HasPrefix(trimmed, "[") {
		trimmed = "[" + trimmed + "]"
	}

	var entries []struct {
		Name        string `json:"Name"`
		PNPDeviceID string `json:"PNPDeviceID"`
		Caption     string `json:"Caption"`
	}
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return "", fmt.Errorf("parsing PowerShell JSON output: %w", err)
	}

	for _, entry := range entries {
		for _, field := range []string{entry.Name, entry.Caption} {
			if match := serialPortRegex.FindString(field); match != "" {
				return strings.Trim(match, "()"), nil
			}
		}
	}

	return "", noESP32SerialPortErr()
}

func noESP32SerialPortErr() error {
	return fmt.Errorf("no ESP32 serial port found (expected COM port with VID %s)", models.ESP32VendorID)
}
