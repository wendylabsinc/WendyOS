//go:build darwin

package discovery

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// ResolveESP32SerialPort finds the serial port for an ESP32-C6 device on macOS.
// It globs /dev/cu.usbmodem* and returns the first match.
// If multiple ports are found the caller must disambiguate.
func ResolveESP32SerialPort() (string, error) {
	matches, err := filepath.Glob("/dev/cu.usbmodem*")
	if err != nil {
		return "", fmt.Errorf("globbing serial ports: %w", err)
	}

	// Filter to likely ESP32 candidates (exclude known non-ESP patterns).
	var candidates []string
	for _, m := range matches {
		candidates = append(candidates, m)
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no ESP32 serial port found (expected /dev/cu.usbmodem*)")
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	// Multiple ports — try to pick one that looks like an Espressif device.
	for _, c := range candidates {
		if strings.Contains(c, models.ESP32VendorID) || strings.Contains(c, "303A") {
			return c, nil
		}
	}

	// Fall back to first match.
	return candidates[0], nil
}
