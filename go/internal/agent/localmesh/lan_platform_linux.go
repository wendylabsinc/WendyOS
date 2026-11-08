//go:build linux

package localmesh

import (
	"context"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func scanLANInterfaces(ethernet, wifi bool) ([]lanInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []lanInterface
	for _, iface := range interfaces {
		if iface.Flags&(net.FlagUp|net.FlagRunning|net.FlagMulticast) != net.FlagUp|net.FlagRunning|net.FlagMulticast {
			continue
		}
		if ethernet {
			if addresses, err := iface.Addrs(); err == nil {
				for _, address := range addresses {
					if ipnet, ok := address.(*net.IPNet); ok {
						if usb, ok := usbLANInterface(iface, ipnet); ok {
							result = append(result, usb)
						}
					}
				}
			}
		}
		cost, transport, ok := classifyLANCarrier(iface.Name, "/sys/class/net", ethernet, wifi, managedWiFi)
		if !ok {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				continue
			}
			result = append(result, lanInterface{iface: iface, ip: append(net.IP(nil), ip...), net: ipnet, cost: cost, transport: transport})
		}
	}
	return result, nil
}

func classifyLANInterface(name, sysfs string, ethernet, wifi bool, isManaged func(string) bool) (uint16, bool) {
	cost, _, ok := classifyLANCarrier(name, sysfs, ethernet, wifi, isManaged)
	return cost, ok
}

func classifyLANCarrier(name, sysfs string, ethernet, wifi bool, isManaged func(string) bool) (uint16, string, bool) {
	base := filepath.Join(sysfs, name)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		if wifi && isManaged(name) {
			return WiFiLinkCost, "infrastructure-wifi", true
		}
		return 0, "", false
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		if wifi && isManaged(name) {
			return WiFiLinkCost, "infrastructure-wifi", true
		}
		return 0, "", false
	}
	// A device-backed ARPHRD_ETHER interface excludes veth, bridges, dummy,
	// TUN and other virtual interfaces used by apps or mesh carriers.
	if !ethernet {
		return 0, "", false
	}
	kind, err := os.ReadFile(filepath.Join(base, "type"))
	if err != nil || strings.TrimSpace(string(kind)) != "1" {
		return 0, "", false
	}
	if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
		return 0, "", false
	}
	return EthernetLinkCost, "ethernet", true
}

func managedWiFi(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "iw", "dev", name, "info").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "type managed" {
			link, err := exec.CommandContext(ctx, "iw", "dev", name, "link").Output()
			return err == nil && strings.Contains(string(link), "Connected to ")
		}
	}
	return false
}

func bindToInterface(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		if err := raw.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		}); err != nil {
			return err
		}
		return bindErr
	}
}
