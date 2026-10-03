package localmesh

import (
	"errors"
	"net/netip"

	"github.com/wendylabsinc/WendyOS/babel"
)

// Addresses are scoped to a single authenticated organization. The IPv4 range
// preserves the old lab transport addresses, and is NEVER the app-facing VIP.
func Addresses(org, asset int32) (netip.Addr, netip.Addr, error) {
	if org <= 0 || asset <= 0 || asset > 65534 {
		return netip.Addr{}, netip.Addr{}, errors.New("invalid local-mesh identity")
	}
	v4 := netip.AddrFrom4([4]byte{10, 88, byte(asset >> 8), byte(asset)})
	v6 := netip.AddrFrom16([16]byte{0xfd, 0x77, 0x65, 0x6e, 0x64, 0x79, byte(org >> 24), byte(org >> 16), byte(org >> 8), byte(org), 0, 0, 0, 0, byte(asset >> 8), byte(asset)})
	return v4, v6, nil
}

func RouterID(org, asset int32) (babel.RouterID, error) {
	if _, _, err := Addresses(org, asset); err != nil {
		return 0, err
	}
	return babel.RouterID(uint64(uint32(org))<<32 | uint64(uint32(asset))), nil
}

// LinkAddresses may be reused on every point-to-point TUN; the LinkID/interface
// scopes them. Lower asset uses ::1. Parallel NAN/BLE links remain independent.
func LinkAddresses(self, peer int32) (netip.Addr, netip.Addr, error) {
	if self <= 0 || peer <= 0 || self == peer {
		return netip.Addr{}, netip.Addr{}, errors.New("invalid link identities")
	}
	a, b := netip.MustParseAddr("fe80::1"), netip.MustParseAddr("fe80::2")
	if self > peer {
		return b, a, nil
	}
	return a, b, nil
}

// OwnedPrefix validates address allocation, not reachability or a relayed
// router's honesty. Gateway defaults require a separately verified live offer.
func OwnedPrefix(org int32, router babel.RouterID, p netip.Prefix) bool {
	if int32(uint64(router)>>32) != org {
		return false
	}
	asset := uint32(router)
	if asset > 65534 {
		return false
	}
	v4, v6, err := Addresses(org, int32(asset))
	return err == nil && (p == netip.PrefixFrom(v4, 32) || p == netip.PrefixFrom(v6, 128))
}
