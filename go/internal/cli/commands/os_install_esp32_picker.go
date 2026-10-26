//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

// selectESP32InstallDevice keeps a previous selection when its port is still
// present. Without a stable USB identity, a replacement port needs an explicit
// selection even if it is the only candidate left.
func selectESP32InstallDevice(devices []discovery.SerialPortInfo, previous discovery.SerialPortInfo, interactive bool, pick func(string, []tui.PickerItem) (string, error)) (discovery.SerialPortInfo, error) {
	if len(devices) == 0 {
		return discovery.SerialPortInfo{}, fmt.Errorf("no ESP32 serial port found")
	}
	for _, device := range devices {
		if device.Port == previous.Port && device.Transport == previous.Transport {
			return device, nil
		}
	}
	if len(devices) == 1 && previous.Port == "" {
		return devices[0], nil
	}

	items := make([]tui.PickerItem, 0, len(devices))
	ports := make([]string, 0, len(devices))
	for _, device := range devices {
		transport := "Native USB"
		if device.Transport == discovery.SerialTransportUARTBridge {
			transport = "USB-UART bridge"
		}
		items = append(items, tui.PickerItem{
			Name:        device.Port,
			Description: transport,
			Value:       device.Port,
		})
		ports = append(ports, device.Port)
	}
	if !interactive {
		return discovery.SerialPortInfo{}, fmt.Errorf("ESP32 device selection required (%s); run in an interactive terminal without --json to select a device, or connect only the intended ESP32 and retry", strings.Join(ports, ", "))
	}
	title := "Select an ESP32 device to flash"
	if previous.Port != "" {
		title = fmt.Sprintf("%s is no longer available. Select an ESP32 device to flash", previous.Port)
	}
	port, err := pick(title, items)
	if err != nil {
		return discovery.SerialPortInfo{}, err
	}
	for _, device := range devices {
		if device.Port == port {
			return device, nil
		}
	}
	return discovery.SerialPortInfo{}, fmt.Errorf("invalid ESP32 device selection %q", port)
}
