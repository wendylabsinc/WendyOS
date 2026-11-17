package containerd

import "net"

// The generated CycloneDDS config uses its default IPv4 transport. Interfaces
// that are down, loopback, unaddressed or IPv6-only cannot prevent autodetermine
// from choosing lo again. Keep automatic external selection only when a usable
// candidate exists, preserving CycloneDDS's ranking on ordinary hosts.
var ros2HasExternalIPv4 = func() (bool, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false, err
	}
	return ros2ExternalIPv4(interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
}

func ros2ExternalIPv4(interfaces []net.Interface, addresses func(net.Interface) ([]net.Addr, error)) (bool, error) {
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := addresses(iface)
		if err != nil {
			return false, err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip.To4() != nil && !ip.IsUnspecified() && !ip.IsLoopback() {
				return true, nil
			}
		}
	}
	return false, nil
}
