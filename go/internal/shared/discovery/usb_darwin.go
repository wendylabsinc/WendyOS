//go:build darwin

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// system_profiler -json output for the two USB reports. macOS 26 dropped
// SPUSBDataType for SPUSBHostDataType, which names its keys differently.
type spUSBData struct {
	SPUSBHostDataType []spUSBHostItem `json:"SPUSBHostDataType"`
	SPUSBDataType     []spUSBBus      `json:"SPUSBDataType"`
}

// spUSBHostItem is a bus, hub or device in the SPUSBHostDataType report.
type spUSBHostItem struct {
	Name      string          `json:"_name"`
	VendorID  string          `json:"USBDeviceKeyVendorID"`
	ProductID string          `json:"USBDeviceKeyProductID"`
	SerialNum string          `json:"USBDeviceKeySerialNumber"`
	LinkSpeed string          `json:"USBDeviceKeyLinkSpeed"` // e.g. "5 Gb/s"
	Items     []spUSBHostItem `json:"_items"`                // devices on a bus or hub
}

// item converts h and everything below it to the older report's shape, which
// collectUSBDevices reads. The newer report carries no power figures.
func (h spUSBHostItem) item() spUSBItem {
	item := spUSBItem{
		Name:      h.Name,
		VendorID:  h.VendorID,
		ProductID: h.ProductID,
		SerialNum: h.SerialNum,
		Speed:     h.LinkSpeed,
	}
	for _, child := range h.Items {
		item.Items = append(item.Items, child.item())
	}
	return item
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
// It asks for both USB reports in one call: system_profiler answers a report
// it does not have with an empty list, so each macOS release fills in its own.
func discoverUSB(ctx context.Context) ([]models.USBDevice, error) {
	out, err := exec.CommandContext(ctx, "system_profiler", "SPUSBHostDataType", "SPUSBDataType", "-json").Output()
	if err != nil {
		// In case an older release refuses a report name it does not know.
		out, err = exec.CommandContext(ctx, "system_profiler", "SPUSBDataType", "-json").Output()
	}
	if err != nil {
		return nil, fmt.Errorf("running system_profiler: %w", err)
	}
	devices, err := parseSystemProfilerUSB(out)
	if err != nil || len(devices) == 0 {
		return devices, err
	}
	if ports, err := darwinHardwarePorts(ctx); err == nil {
		setUSBNetworkInterfaces(devices, ports)
	}
	return devices, nil
}

// parseSystemProfilerUSB returns the Wendy devices in system_profiler's JSON
// USB report. The older report is read only when the newer one lists none, so
// a release that answers both never lists a board twice.
func parseSystemProfilerUSB(out []byte) ([]models.USBDevice, error) {
	var data spUSBData
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("parsing system_profiler output: %w", err)
	}

	var devices []models.USBDevice
	for _, entry := range data.SPUSBHostDataType {
		// Top-level entries are buses; collectUSBDevices walks down from them.
		collectUSBDevices([]spUSBItem{entry.item()}, &devices)
	}
	if len(devices) > 0 {
		return devices, nil
	}
	for _, bus := range data.SPUSBDataType {
		collectUSBDevices(bus.Items, &devices)
	}
	return devices, nil
}

// setUSBNetworkInterfaces records the network interface each device's USB
// network link appears as. macOS names that hardware port after the USB
// product string, so a name held by exactly one device and one port joins
// them. A name that several devices or ports share proves nothing and is left
// unset.
func setUSBNetworkInterfaces(devices []models.USBDevice, ports []darwinHardwarePort) {
	devicesNamed := make(map[string]int, len(devices))
	for i := range devices {
		devicesNamed[devices[i].Name]++
	}
	portsNamed := make(map[string]int, len(ports))
	interfaceNamed := make(map[string]string, len(ports))
	for _, port := range ports {
		portsNamed[port.displayName]++
		interfaceNamed[port.displayName] = port.bsdName
	}
	for i := range devices {
		name := devices[i].Name
		if devicesNamed[name] == 1 && portsNamed[name] == 1 {
			devices[i].NetworkInterface = interfaceNamed[name]
		}
	}
}

// collectUSBDevices recursively walks the USB tree (hubs contain child items)
// and collects devices whose name contains "Wendy", excluding Espressif
// ESP32 boards (by VID:PID) even when their name also matches — those are
// Wendy Lite boards, listed separately via serial/WendyCom discovery, and
// must never appear here too.
func collectUSBDevices(items []spUSBItem, devices *[]models.USBDevice) {
	for _, item := range items {
		// Recurse into hubs first.
		if len(item.Items) > 0 {
			collectUSBDevices(item.Items, devices)
		}

		isWendy := strings.Contains(strings.ToLower(item.Name), "wendy")
		isESP32 := isESP32Device(item.VendorID, item.ProductID)

		if !isWendy || isESP32 {
			continue
		}

		dev := models.USBDevice{
			Name:          item.Name,
			DisplayName:   item.Name,
			IsWendyDevice: isWendy,
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

// isESP32Device checks if the VID/PID matches an Espressif ESP32 board.
func isESP32Device(vendorID, productID string) bool {
	vidFields := strings.Fields(vendorID)
	if len(vidFields) == 0 {
		return false
	}
	pidFields := strings.Fields(productID)
	if len(pidFields) == 0 {
		return false
	}
	vid := strings.ToLower(vidFields[0])
	pid := strings.ToLower(pidFields[0])
	return vid == models.ESP32VendorID && pid == models.ESP32ProductID
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
