// Package mesh implements the WendyOS mesh data plane: the deterministic
// device-ID↔VIP mapping, the per-app DNS server that answers
// device-n.mesh.wendy.internal names, and the transparent TCP proxy that carries
// mesh VIP connections to peer devices.
package mesh

import (
	"fmt"
	"net/netip"
)

// DefaultServiceCIDR is the mesh service CIDR v1 operates on. The wendy.json
// schema accepts other CIDRs (the route/ACCEPT plumbing honors them), but DNS
// answers and VIP→device resolution assume this network.
const DefaultServiceCIDR = "10.99.0.0/16"

var meshPrefix = netip.MustParsePrefix(DefaultServiceCIDR)

// Device IDs 0 and 65535 map to the CIDR's network and broadcast addresses,
// so the valid range excludes them.
const (
	MinDeviceID = 1
	MaxDeviceID = 65534
)

// VIPForDevice maps a cloud asset ID to its mesh VIP: device N →
// 10.99.<N/256>.<N%256>. Pure function; no allocation state exists anywhere.
func VIPForDevice(deviceID int32) (netip.Addr, error) {
	if deviceID < MinDeviceID || deviceID > MaxDeviceID {
		return netip.Addr{}, fmt.Errorf("mesh: device ID %d outside VIP range [%d, %d]", deviceID, MinDeviceID, MaxDeviceID)
	}
	base := meshPrefix.Addr().As4()
	return netip.AddrFrom4([4]byte{base[0], base[1], byte(deviceID >> 8), byte(deviceID)}), nil
}

// DeviceForVIP is the inverse of VIPForDevice.
func DeviceForVIP(vip netip.Addr) (int32, error) {
	if !vip.Is4() || !meshPrefix.Contains(vip) {
		return 0, fmt.Errorf("mesh: %s is outside the mesh service CIDR %s", vip, DefaultServiceCIDR)
	}
	b := vip.As4()
	id := int32(b[2])<<8 | int32(b[3])
	if id < MinDeviceID || id > MaxDeviceID {
		return 0, fmt.Errorf("mesh: %s maps to invalid device ID %d", vip, id)
	}
	return id, nil
}
