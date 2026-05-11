//go:build darwin

package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ResolveESP32SerialPort finds the serial port for an ESP32-C6 device on macOS.
// It globs /dev/cu.usbmodem* and returns the most recently connected.
func ResolveESP32SerialPort() (string, error) {
	matches, err := filepath.Glob("/dev/cu.usbmodem*")
	if err != nil {
		return "", fmt.Errorf("globbing serial ports: %w", err)
	}

	if len(matches) == 0 {
		return "", fmt.Errorf("no ESP32 serial port found (expected /dev/cu.usbmodem*)")
	}

	best := matches[0]
	bestMtime := time.Time{}
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().After(bestMtime) {
			bestMtime = info.ModTime()
			best = m
		}
	}

	return best, nil
}
