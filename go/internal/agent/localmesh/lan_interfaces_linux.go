//go:build linux

package localmesh

import "net"

// ScanPhysicalLANInterfaces shares carrier discovery's device and managed
// Wi-Fi policy with local DNS-SD browsing.
func ScanPhysicalLANInterfaces(ethernet, wifi bool) ([]PhysicalLANInterface, error) {
	selected, err := scanLANInterfaces(ethernet, wifi)
	if err != nil {
		return nil, err
	}
	result := make([]PhysicalLANInterface, 0, len(selected))
	for _, iface := range selected {
		result = append(result, PhysicalLANInterface{Name: iface.iface.Name, Index: iface.iface.Index, Cost: iface.cost,
			IP: append(net.IP(nil), iface.ip...), Net: &net.IPNet{IP: append(net.IP(nil), iface.net.IP...), Mask: append(net.IPMask(nil), iface.net.Mask...)}})
	}
	return result, nil
}
