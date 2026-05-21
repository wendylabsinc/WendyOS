//go:build linux

package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

const sysClassNet = "/sys/class/net"

// discoverEthernet reads /sys/class/net to find Wendy Ethernet interfaces on Linux.
func discoverEthernet(_ context.Context) ([]models.EthernetInterface, error) {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", sysClassNet, err)
	}

	var devices []models.EthernetInterface
	for _, entry := range entries {
		name := entry.Name()

		// Skip loopback and virtual interfaces.
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") {
			continue
		}

		if !strings.Contains(strings.ToLower(name), "wendy") {
			continue
		}

		iface := models.EthernetInterface{
			Name:          name,
			DisplayName:   name,
			IsWendyDevice: true,
		}

		// Read MAC address.
		if data, err := os.ReadFile(filepath.Join(sysClassNet, name, "address")); err == nil {
			mac := strings.TrimSpace(string(data))
			if mac != "" && mac != "00:00:00:00:00:00" {
				iface.MACAddress = mac
			}
		}

		iface.LinkSpeed = linuxInterfaceLinkSpeed(name)

		// Read IP address via Go's net package.
		if netIface, err := net.InterfaceByName(name); err == nil {
			if addrs, err := netIface.Addrs(); err == nil {
				for _, addr := range addrs {
					if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.To4() != nil {
						iface.IPAddress = ipNet.IP.String()
						break
					}
				}
			}
		}

		devices = append(devices, iface)
	}
	return devices, nil
}

func linuxInterfaceLinkSpeed(name string) string {
	data, err := os.ReadFile(filepath.Join(sysClassNet, name, "speed"))
	if err != nil {
		return ""
	}

	speedStr := strings.TrimSpace(string(data))
	var mbps int
	if _, scanErr := fmt.Sscanf(speedStr, "%d", &mbps); scanErr != nil || mbps <= 0 {
		return ""
	}
	if mbps >= 1000 {
		return fmt.Sprintf("%d Gbps", mbps/1000)
	}
	return fmt.Sprintf("%d Mbps", mbps)
}
