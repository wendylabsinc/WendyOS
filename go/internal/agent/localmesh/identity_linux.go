//go:build linux

package localmesh

import (
	"net"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
)

// Prefer the kernel's permanent address. For drivers that omit IFLA_PERM_ADDRESS,
// a physical interface with NET_ADDR_PERM still exposes its factory address.
// Virtual and administratively randomized addresses are not identity sources.
func persistentMAC(name string) (net.HardwareAddr, bool) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, false
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, false
	}
	if mac := link.Attrs().PermHWAddr; usableMAC(mac) {
		return mac, true
	}
	base := "/sys/class/net/" + name
	if _, err := os.Stat(base + "/device"); err != nil {
		return nil, false
	}
	kind, err := os.ReadFile(base + "/addr_assign_type")
	if err != nil || strings.TrimSpace(string(kind)) != "0" {
		return nil, false
	}
	mac := link.Attrs().HardwareAddr
	return mac, usableMAC(mac)
}
