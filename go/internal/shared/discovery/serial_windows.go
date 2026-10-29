//go:build windows

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

// ResolveESP32SerialPorts returns all connected serial ports whose USB VID/PID
// match a supported native or USB-to-UART interface. ConnectionTime is not
// available on Windows and is left as the zero value.
func resolveESP32SerialPorts() ([]SerialPortInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	script := `Get-CimInstance Win32_PnPEntity | Where-Object { $_.PNPClass -eq 'Ports' -and $_.PNPDeviceID -like 'USB\VID_*' } | Select-Object Name, PNPDeviceID, Caption | ConvertTo-Json -Compress`

	cmd := exec.CommandContext(ctx, env.PowershellExe(), "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("querying Win32_PnPEntity for ESP32 serial port: %w", err)
	}

	return parseESP32SerialPortsJSON(string(out))
}

// serialInterfaceRegex identifies the function of a composite USB device.
var serialInterfaceRegex = regexp.MustCompile(`(?i)&MI_([0-9a-f]{2})(?:\\|&)`)

// serialPortRegex matches a parenthesized COM port suffix such as "(COM5)".
var serialPortRegex = regexp.MustCompile(`\(COM\d+\)`)

// parseESP32SerialPortsJSON extracts all COMN port names from a JSON blob
// produced by `Get-CimInstance Win32_PnPEntity | Select-Object Name,
// PNPDeviceID, Caption | ConvertTo-Json`. Returns bare "COMN" strings with
// zero ConnectionTime (not available via Win32_PnPEntity).
func parseESP32SerialPortsJSON(jsonOut string) ([]SerialPortInfo, error) {
	trimmed := strings.TrimSpace(jsonOut)
	if trimmed == "" {
		return nil, nil
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
		return nil, fmt.Errorf("parsing PowerShell JSON output: %w", err)
	}

	var result []SerialPortInfo
	for _, entry := range entries {
		vendorIDText, productIDText := parseVIDPID(entry.PNPDeviceID)
		vendorID, vidErr := parseUSBID(vendorIDText)
		productID, pidErr := parseUSBID(productIDText)
		if vidErr != nil || pidErr != nil {
			continue
		}
		usbID, ok := supportedESP32SerialUSBID(vendorID, productID)
		if !ok {
			continue
		}
		iface := -1
		if match := serialInterfaceRegex.FindStringSubmatch(entry.PNPDeviceID); len(match) == 2 {
			if n, err := strconv.ParseInt(match[1], 16, 32); err == nil {
				iface = int(n)
			}
		}
		if !serialUSBInterfaceAllowed(usbID, iface) {
			continue
		}
		for _, field := range []string{entry.Name, entry.Caption} {
			if match := serialPortRegex.FindString(field); match != "" {
				result = append(result, SerialPortInfo{
					Port:      strings.Trim(match, "()"),
					Transport: usbID.transport,
				})
				break
			}
		}
	}
	return result, nil
}

// parseESP32SerialPortJSON extracts the first COMN port name from a JSON blob
// produced by `Get-CimInstance Win32_PnPEntity | Select-Object Name,
// PNPDeviceID, Caption | ConvertTo-Json`. Returns the bare "COMN" string
// (matching what os_install.go and esptool expect on Windows).
func parseESP32SerialPortJSON(jsonOut string) (string, error) {
	devices, err := parseESP32SerialPortsJSON(jsonOut)
	if err != nil {
		return "", err
	}
	if len(devices) == 0 {
		return "", noESP32SerialPortErr()
	}
	return devices[0].Port, nil
}

func noESP32SerialPortErr() error {
	return fmt.Errorf("no ESP32 serial port found (expected a supported native USB or CP210x COM port)")
}
