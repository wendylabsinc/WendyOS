//go:build linux

package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const linuxSysDevicesRoot = "/sys/devices"

// ResolveESP32SerialPorts returns all connected serial ports whose USB VID/PID
// match a supported native or USB-to-UART interface, along with each device
// node's modification time as a proxy for when the device was plugged in.
func resolveESP32SerialPorts() ([]SerialPortInfo, error) {
	var entries []string
	for _, pattern := range []string{"/sys/class/tty/ttyACM*", "/sys/class/tty/ttyUSB*"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("globbing tty entries: %w", err)
		}
		entries = append(entries, matches...)
	}

	var result []SerialPortInfo
	for _, entry := range entries {
		deviceSymlink := filepath.Join(entry, "device")
		resolvedIface, err := filepath.EvalSymlinks(deviceSymlink)
		if err != nil {
			continue
		}
		usbID, ok := findSupportedESP32SerialUSBID(resolvedIface, linuxSysDevicesRoot)
		if !ok {
			continue
		}

		devPath := "/dev/" + filepath.Base(entry)
		dev := SerialPortInfo{Port: devPath, Transport: usbID.transport}
		if info, statErr := os.Stat(devPath); statErr == nil {
			dev.ConnectionTime = info.ModTime()
		}
		result = append(result, dev)
	}
	return result, nil
}

// findSupportedESP32SerialUSBID walks from a resolved tty sysfs node toward the
// sysfs device root. Different serial drivers nest tty nodes at different
// depths, so assuming idVendor/idProduct are exactly one parent away works for
// some native ACM devices but not reliably for CP210x ttyUSB devices.
func findSupportedESP32SerialUSBID(start, root string) (serialUSBID, bool) {
	rel, err := filepath.Rel(root, start)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return serialUSBID{}, false
	}

	interfaceNumber := -1
	for current := start; ; current = filepath.Dir(current) {
		if data, err := os.ReadFile(filepath.Join(current, "bInterfaceNumber")); err == nil && interfaceNumber < 0 {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 16, 32); err == nil {
				interfaceNumber = int(n)
			}
		}
		vid, vidErr := os.ReadFile(filepath.Join(current, "idVendor"))
		pid, pidErr := os.ReadFile(filepath.Join(current, "idProduct"))
		if vidErr == nil && pidErr == nil {
			vendorID, parseVIDErr := parseUSBID(string(vid))
			productID, parsePIDErr := parseUSBID(string(pid))
			if parseVIDErr == nil && parsePIDErr == nil {
				if id, ok := supportedESP32SerialUSBID(vendorID, productID); ok {
					return id, serialUSBInterfaceAllowed(id, interfaceNumber)
				}
			}
		}
		if current == root {
			return serialUSBID{}, false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return serialUSBID{}, false
		}
	}
}
