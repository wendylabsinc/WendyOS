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

func scanLANInterfaces(ethernet, wifi bool) (lanScan, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return lanScan{}, err
	}
	result := lanScan{hardware: make(map[string]bool, 2)}
	if wifi {
		// A PHY can exist without a station interface, for example while Wi-Fi
		// is administratively down. That is waiting, not missing hardware.
		phys, err := os.ReadDir("/sys/class/ieee80211")
		result.hardware[CarrierInfrastructureWiFi] = len(phys) > 0 || (err != nil && !os.IsNotExist(err))
	}
	for _, iface := range interfaces {
		cost, transport, physical := classifyLANCarrier(iface.Name, "/sys/class/net", ethernet, wifi, func(string) bool { return true })
		if physical {
			result.hardware[transport] = true
		}
		if iface.Flags&(net.FlagUp|net.FlagRunning|net.FlagMulticast) != net.FlagUp|net.FlagRunning|net.FlagMulticast {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		eligible := physical && (transport != CarrierInfrastructureWiFi || managedWiFi(iface.Name))
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ethernet {
				if usb, ok := usbLANInterface(iface, ipnet); ok {
					result.hardware[CarrierEthernet] = true
					result.interfaces = append(result.interfaces, usb)
				}
			}
			ip := ipnet.IP.To4()
			if !eligible || ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				continue
			}
			result.interfaces = append(result.interfaces, lanInterface{iface: iface, ip: append(net.IP(nil), ip...), net: ipnet, cost: cost, transport: transport})
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
			return WiFiLinkCost, CarrierInfrastructureWiFi, true
		}
		return 0, "", false
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		if wifi && isManaged(name) {
			return WiFiLinkCost, CarrierInfrastructureWiFi, true
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
	return EthernetLinkCost, CarrierEthernet, true
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
