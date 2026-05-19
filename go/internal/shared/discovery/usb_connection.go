package discovery

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

var linuxUSBInterfaceNameRE = regexp.MustCompile(`^en[a-z0-9]*u[0-9]+`)

func setLANNetworkInterface(dev *models.LANDevice, interfaceName, displayName, linkSpeed string) {
	interfaceName = strings.TrimSpace(interfaceName)
	if interfaceName == "" {
		return
	}

	dev.NetworkInterface = interfaceName
	if dev.USB == "" {
		dev.USB = usbConnectionSummary(interfaceName, displayName, linkSpeed)
	}
}

func usbConnectionSummary(interfaceName, displayName, linkSpeed string) string {
	if !looksLikeUSBConnection(interfaceName, displayName) {
		return ""
	}

	label := interfaceName
	if displayName != "" && !strings.EqualFold(displayName, interfaceName) {
		label = fmt.Sprintf("%s (%s)", displayName, interfaceName)
	}
	if linkSpeed != "" {
		return label + " " + linkSpeed
	}
	return label
}

func looksLikeUSBConnection(interfaceName, displayName string) bool {
	name := strings.ToLower(strings.TrimSpace(interfaceName))
	display := strings.ToLower(strings.TrimSpace(displayName))
	combined := name + " " + display

	switch {
	case strings.Contains(combined, "wendy"):
		return true
	case strings.Contains(combined, "usb"):
		return true
	case strings.Contains(combined, "rndis"):
		return true
	case strings.Contains(combined, "ecm"):
		return true
	case strings.Contains(combined, "gadget"):
		return true
	case strings.HasPrefix(name, "enx"):
		return true
	case linuxUSBInterfaceNameRE.MatchString(name):
		return true
	default:
		return false
	}
}

func appendPreferredLANDevice(devices []models.LANDevice, indexes map[string]int, key string, dev models.LANDevice) []models.LANDevice {
	if idx, ok := indexes[key]; ok {
		if preferDiscoveredLANDevice(dev, devices[idx]) {
			devices[idx] = dev
		}
		return devices
	}

	indexes[key] = len(devices)
	return append(devices, dev)
}

func preferDiscoveredLANDevice(candidate, existing models.LANDevice) bool {
	if (candidate.USB != "") != (existing.USB != "") {
		return candidate.USB != ""
	}
	if existing.IPAddress == "" && candidate.IPAddress != "" {
		return true
	}
	if existing.NetworkInterface == "" && candidate.NetworkInterface != "" {
		return true
	}
	return lanDeviceDiscoveryScore(candidate) > lanDeviceDiscoveryScore(existing)
}

func lanDeviceDiscoveryScore(dev models.LANDevice) int {
	score := 0
	if dev.ID != "" {
		score++
	}
	if dev.DisplayName != "" {
		score++
	}
	if dev.Hostname != "" {
		score++
	}
	if dev.IPAddress != "" {
		score++
	}
	if dev.Port != 0 {
		score++
	}
	if dev.NetworkInterface != "" {
		score++
	}
	if dev.USB != "" {
		score += 2
	}
	if dev.IsMTLS {
		score++
	}
	return score
}
