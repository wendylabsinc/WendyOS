//go:build linux

package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// ResolveESP32SerialPort finds the serial port for an ESP32-C6 device on Linux.
// It walks /sys/class/tty/ttyACM* and matches the Espressif VID/PID.
func ResolveESP32SerialPort() (string, error) {
	entries, err := filepath.Glob("/sys/class/tty/ttyACM*")
	if err != nil {
		return "", fmt.Errorf("globbing tty entries: %w", err)
	}

	wantVID := strings.TrimPrefix(models.ESP32VendorID, "0x")
	wantPID := strings.TrimPrefix(models.ESP32ProductID, "0x")

	for _, entry := range entries {
		vidPath := filepath.Join(entry, "device", "..", "idVendor")
		pidPath := filepath.Join(entry, "device", "..", "idProduct")

		vid, err := os.ReadFile(vidPath)
		if err != nil {
			continue
		}
		pid, err := os.ReadFile(pidPath)
		if err != nil {
			continue
		}

		if strings.TrimSpace(string(vid)) == wantVID &&
			strings.TrimSpace(string(pid)) == wantPID {
			devName := filepath.Base(entry)
			return "/dev/" + devName, nil
		}
	}

	return "", fmt.Errorf("no ESP32 serial port found (expected /dev/ttyACM* with VID %s)", models.ESP32VendorID)
}
