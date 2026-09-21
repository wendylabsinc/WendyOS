package rtps

import (
	"fmt"
	"net"
	"strings"
)

// InterfaceCandidate is the portion of an interface needed for automatic discovery.
type InterfaceCandidate struct {
	Name    string
	Flags   net.Flags
	HasIPv4 bool
}

// IsWirelessInterface recognizes Linux predictable and common driver names.
func IsWirelessInterface(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range []string{"wl", "wifi", "ath", "ra"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func virtualInterface(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range []string{"docker", "br-", "veth", "cni", "flannel", "virbr", "kube", "nerdctl", "tap", "tun"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// EligibleInterfaces excludes wireless automatically. Container interfaces are
// usable inside isolated namespaces, but virtual interfaces are excluded on host.
// Explicit interface selections bypass this policy.
func EligibleInterfaces(ifaces []InterfaceCandidate, host bool) []string {
	var names []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagMulticast == 0 || !iface.HasIPv4 || IsWirelessInterface(iface.Name) || (host && virtualInterface(iface.Name)) {
			continue
		}
		names = append(names, iface.Name)
	}
	return names
}

func interfaceCandidates() ([]InterfaceCandidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]InterfaceCandidate, 0, len(ifaces))
	for _, iface := range ifaces {
		_, err := firstIPv4(&iface)
		out = append(out, InterfaceCandidate{Name: iface.Name, Flags: iface.Flags, HasIPv4: err == nil})
	}
	return out, nil
}

// HostInterfaces returns every eligible wired interface in enumeration order.
func HostInterfaces() ([]string, error) {
	ifaces, err := interfaceCandidates()
	return EligibleInterfaces(ifaces, true), err
}

// DiscoveryInterfaces provides loopback plus wired coverage for an app graph.
// Host-network apps cover all eligible host interfaces; isolated apps select one
// non-wireless container interface. The PID is verified around namespace entry.
func DiscoveryInterfaces(cfg Config) ([]string, error) {
	ns, err := captureNetworkNamespace(cfg.NetworkNamespacePID, cfg.VerifyNetworkNamespace)
	if err != nil {
		return nil, err
	}
	defer ns.close()
	var names []string
	err = ns.run(func() error {
		ifaces, err := interfaceCandidates()
		if err != nil {
			return err
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 && iface.Flags&net.FlagUp != 0 && iface.HasIPv4 {
				names = append(names, iface.Name)
				break
			}
		}
		wired := EligibleInterfaces(ifaces, ns.host)
		if !ns.host && len(wired) > 1 {
			wired = wired[:1]
		}
		names = append(names, wired...)
		return nil
	})
	if err == nil {
		err = ns.verify()
	}
	return names, err
}

func resolveTarget(cfg Config) (*resolvedTarget, error) {
	if cfg.DomainID < 0 || cfg.DomainID > 232 {
		return nil, fmt.Errorf("rtps: invalid domain %d", cfg.DomainID)
	}
	ns, err := captureNetworkNamespace(cfg.NetworkNamespacePID, cfg.VerifyNetworkNamespace)
	if err != nil {
		return nil, err
	}
	var iface *net.Interface
	err = ns.run(func() error {
		if cfg.Interface == "" {
			ifaces, err := interfaceCandidates()
			if err != nil {
				return err
			}
			names := EligibleInterfaces(ifaces, ns.host)
			if len(names) == 0 {
				return fmt.Errorf("rtps: no wired multicast-capable IPv4 interface")
			}
			cfg.Interface = names[0]
		}
		var err error
		iface, err = net.InterfaceByName(cfg.Interface)
		if err != nil {
			return err
		}
		_, err = firstIPv4(iface)
		return err
	})
	if err != nil {
		ns.close()
		return nil, err
	}
	cfg.namespace = ns
	return &resolvedTarget{key: poolKey{namespace: ns.identity, iface: iface.Index, domain: cfg.DomainID}, cfg: cfg, ns: ns}, nil
}
