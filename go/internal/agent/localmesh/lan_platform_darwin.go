//go:build darwin

package localmesh

import (
	"context"
	"golang.org/x/sys/unix"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func hardwarePorts(output string) map[string]bool {
	ports := map[string]bool{}
	wifi := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "Hardware Port: "); ok {
			wifi = strings.Contains(name, "Wi-Fi") || strings.Contains(name, "AirPort")
		}
		if name, ok := strings.CutPrefix(line, "Device: "); ok && strings.HasPrefix(name, "en") {
			ports[name] = wifi
		}
	}
	return ports
}
func scanLANInterfaces(ethernet, wifi bool) ([]lanInterface, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/networksetup", "-listallhardwareports").Output()
	if err != nil {
		return nil, err
	}
	ports := hardwarePorts(string(out))
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []lanInterface
	for _, iface := range interfaces {
		isWiFi, known := ports[iface.Name]
		if !known || iface.Flags&(net.FlagUp|net.FlagRunning|net.FlagMulticast) != net.FlagUp|net.FlagRunning|net.FlagMulticast {
			continue
		}
		cost, transport := EthernetLinkCost, "ethernet"
		if isWiFi {
			if !wifi {
				continue
			}
			cost, transport = WiFiLinkCost, "infrastructure-wifi"
		} else if !ethernet {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, addr := range addresses {
			network, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := network.IP.To4()
			if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				continue
			}
			result = append(result, lanInterface{iface: iface, ip: ip, net: network, cost: cost, transport: transport})
		}
	}
	return result, nil
}
func bindToInterface(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return err
		}
		var bindErr error
		err = raw.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, iface.Index)
		})
		if err != nil {
			return err
		}
		return bindErr
	}
}
