package discovery

import (
	"net"
	"runtime"
	"strconv"
	"time"
)

// WellKnownUSBAddr is the fixed IPv6 link-local address every WendyOS device
// assigns to its USB gadget interface (0x57 0x41 = "WA"). Dialing it on a
// USB-backed host interface reaches the device with zero host configuration —
// no DHCP lease, no mDNS resolution, no NetworkManager profile. See
// specs/2026-08-07-usb-deterministic-ncm-design.md.
const WellKnownUSBAddr = "fe80::5741:1"

// USBDirectCandidate identifies one USB-backed host interface on which the
// well-known device address can be dialed.
type USBDirectCandidate struct {
	// Interface is the host interface name, used for display and for the
	// LANDevice USB annotation.
	Interface string
	// Zone is the IPv6 zone identifier for dialing: the interface name on
	// unix-likes (or its bridge, when the port is bridged — see
	// LinkLocalDialZone), the numeric interface index on Windows (Windows
	// zone IDs are indexes, and Go's dialer passes them through verbatim).
	Zone string
}

// HostPort returns the dialable "[fe80::5741:1%zone]:port" address.
func (c USBDirectCandidate) HostPort(port int) string {
	return net.JoinHostPort(WellKnownUSBAddr+"%"+c.Zone, strconv.Itoa(port))
}

// netInterfacesFn lists host network interfaces; a package var so tests can
// inject fixtures (mirrors osLookupHostFn-style seams elsewhere in the CLI).
var netInterfacesFn = net.Interfaces

// usbDisplayNameTimeout bounds the single system query a display-name resolver
// makes (networksetup on macOS, Get-NetAdapter on Windows).
const usbDisplayNameTimeout = 3 * time.Second

// USBDirectCandidates returns one dial candidate per up, non-loopback,
// USB-backed network interface on this host. An empty result means no USB
// gadget link is present.
func USBDirectCandidates() []USBDirectCandidate {
	ifaces, err := netInterfacesFn()
	if err != nil {
		return nil
	}
	return usbDirectCandidatesFrom(ifaces, runtime.GOOS, usbDisplayNameResolver())
}

// usbDirectCandidatesFrom classifies host interfaces into dial candidates.
// displayName maps an interface name to its platform display name (the macOS
// "Hardware Port", the Windows adapter description); it may be nil where the
// interface name alone is enough. It is consulted only for interfaces the cheap
// name heuristics reject, so a host whose links are all classified by name
// never pays for the system query behind it.
func usbDirectCandidatesFrom(ifaces []net.Interface, goos string, displayName func(string) string) []USBDirectCandidate {
	var out []USBDirectCandidate
	for i := range ifaces {
		if ifaces[i].Flags&net.FlagLoopback != 0 || ifaces[i].Flags&net.FlagUp == 0 {
			continue
		}
		if !looksLikeUSBConnection(ifaces[i].Name, "") {
			// A macOS BSD name ("en5") and a Windows friendly name
			// ("Ethernet 3") carry no USB signal at all — only the display
			// name does, so ask for it before giving up on this interface.
			if displayName == nil {
				continue
			}
			if !looksLikeUSBConnection(ifaces[i].Name, displayName(ifaces[i].Name)) {
				continue
			}
		}
		zone := ifaces[i].Name
		if goos == "windows" {
			zone = strconv.Itoa(ifaces[i].Index)
		} else {
			zone = LinkLocalDialZone(zone)
		}
		out = append(out, USBDirectCandidate{Interface: ifaces[i].Name, Zone: zone})
	}
	return out
}
