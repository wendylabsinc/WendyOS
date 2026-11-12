package discovery

import (
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// USBNetworkInterfaceNames returns the names of non-loopback network interfaces
// that are Wendy USB gadget links, classified as device discovery does (on
// Linux by the USB device's identity, see IsWendyUSBGadget). The CLI's USB-C
// auto-setup flow uses it to locate the host's gadget interface.
func USBNetworkInterfaceNames() ([]string, error) {
	ifaces, err := netInterfacesFn()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}
	var out []string
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 {
			continue
		}
		if looksLikeUSBConnection(ifaces[i].Name, "") {
			out = append(out, ifaces[i].Name)
		}
	}
	return out, nil
}

// ansiEscapeRE matches ANSI/VT100 escape sequences (e.g. colour codes).
var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// SanitiseDisplayName sanitises a device name or version string sourced from
// the network (mDNS, TXT records) before it is rendered in the terminal.
func SanitiseDisplayName(s string) string { return sanitiseNetworkString(s, 64) }

// sanitiseNetworkString strips ANSI escape sequences and C0/C1/DEL control
// characters from a string sourced from the network, then truncates to maxLen
// runes. This prevents terminal injection from rogue mDNS/DNS-SD advertisers.
func sanitiseNetworkString(s string, maxLen int) string {
	s = ansiEscapeRE.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		}
	}
	s = strings.TrimSpace(b.String())
	if maxLen > 0 {
		runes := []rune(s)
		if len(runes) > maxLen {
			s = string(runes[:maxLen])
		}
	}
	return s
}

func setLANNetworkInterface(dev *models.LANDevice, interfaceName, displayName, linkSpeed string) {
	interfaceName = strings.TrimSpace(sanitiseNetworkString(interfaceName, 64))
	if interfaceName == "" {
		return
	}

	dev.NetworkInterface = interfaceName
	if dev.USB == "" {
		dev.USB = usbConnectionSummary(interfaceName, sanitiseNetworkString(displayName, 64), sanitiseNetworkString(linkSpeed, 32))
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

// looksLikeUSBConnection reports whether an interface is a USB link to a Wendy
// device. On Linux the USB identity decides; elsewhere only the interface and
// display names are available.
func looksLikeUSBConnection(interfaceName, displayName string) bool {
	if usbGadgetIdentityFn != nil {
		return usbGadgetIdentityFn(strings.TrimSpace(interfaceName))
	}
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
	case strings.Contains(combined, "ndis"):
		return true
	case strings.Contains(combined, "ecm"):
		return true
	case strings.Contains(combined, "ncm"):
		return true
	case strings.Contains(combined, "gadget"):
		return true
	case strings.HasPrefix(name, "enx"):
		return true
	default:
		return false
	}
}
