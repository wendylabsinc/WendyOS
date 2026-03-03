//go:build darwin

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// system_profiler SPUSBDataType -json output structure.
type spUSBData struct {
	SPUSBDataType []spUSBBus `json:"SPUSBDataType"`
}

type spUSBBus struct {
	Name  string      `json:"_name"`
	Items []spUSBItem `json:"_items"`
}

type spUSBItem struct {
	Name         string      `json:"_name"`
	VendorID     string      `json:"vendor_id"`
	ProductID    string      `json:"product_id"`
	SerialNum    string      `json:"serial_num"`
	Manufacturer string      `json:"manufacturer"`
	BusPower     string      `json:"bus_power"`      // max available mA
	BusPowerUsed string      `json:"bus_power_used"` // actual mA
	Speed        string      `json:"device_speed"`   // e.g. "Up to 480 Mb/sec"
	Items        []spUSBItem `json:"_items"`         // nested hubs
}

// discoverUSB uses system_profiler to find USB-connected Wendy devices on macOS.
func discoverUSB(ctx context.Context) ([]models.USBDevice, error) {
	cmd := exec.CommandContext(ctx, "system_profiler", "SPUSBDataType", "-json")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running system_profiler: %w", err)
	}

	var data spUSBData
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("parsing system_profiler output: %w", err)
	}

	var devices []models.USBDevice
	for _, bus := range data.SPUSBDataType {
		collectUSBDevices(bus.Items, &devices)
	}
	return devices, nil
}

// collectUSBDevices recursively walks the USB tree (hubs contain child items)
// and collects devices whose name contains "Wendy".
func collectUSBDevices(items []spUSBItem, devices *[]models.USBDevice) {
	for _, item := range items {
		// Recurse into hubs first.
		if len(item.Items) > 0 {
			collectUSBDevices(item.Items, devices)
		}

		if !strings.Contains(strings.ToLower(item.Name), "wendy") {
			continue
		}

		dev := models.USBDevice{
			Name:          item.Name,
			DisplayName:   item.Name,
			IsWendyDevice: true,
		}

		// vendor_id may contain a suffix like "0x05ac  (Apple Inc.)"
		if vid := strings.Fields(item.VendorID); len(vid) > 0 {
			dev.VendorID = vid[0]
		}
		if pid := strings.Fields(item.ProductID); len(pid) > 0 {
			dev.ProductID = pid[0]
		}

		dev.SerialNumber = item.SerialNum

		// Parse power consumption.
		if item.BusPowerUsed != "" {
			var mA int
			fmt.Sscanf(item.BusPowerUsed, "%d", &mA)
			dev.MaxPowerMilliamps = mA
		}

		// Map device_speed to a USB version string.
		dev.USBVersion = parseUSBSpeed(item.Speed)

		*devices = append(*devices, dev)
	}
}

func parseUSBSpeed(speed string) string {
	s := strings.ToLower(speed)
	switch {
	case strings.Contains(s, "20 gb"):
		return "USB 3.2 Gen 2x2"
	case strings.Contains(s, "10 gb"):
		return "USB 3.2 Gen 2"
	case strings.Contains(s, "5 gb"):
		return "USB 3.0"
	case strings.Contains(s, "480 mb"):
		return "USB 2.0"
	case strings.Contains(s, "12 mb"):
		return "USB 1.1"
	case strings.Contains(s, "1.5 mb"):
		return "USB 1.0"
	default:
		return ""
	}
}
