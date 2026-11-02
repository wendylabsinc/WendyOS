package meshoperator

import (
	"crypto/sha256"
	"encoding/binary"
	"net/netip"
)

const AddressDomain = "wendy-net/operator-address/v1\x00"

// Prefix is separate from meshid.MeshPrefix's device allocation. It is an
// allocation/policy boundary, not a subnet advertised by every attachment.
var Prefix = netip.MustParsePrefix("fd45:b891:5680::/48")

func InOperatorPool(address netip.Addr) bool {
	return address.Is6() && address.Zone() == "" && Prefix.Contains(address)
}

// Address follows the shared v1 derivation. The full verified principal,
// including tenant and opaque subject, participates without normalization.
func (e Endpoint) Address() netip.Addr {
	if !e.Operator.IsValid() {
		return netip.Addr{}
	}
	h := sha256.New()
	h.Write([]byte(AddressDomain))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(e.Operator.Principal())))
	h.Write(length[:])
	h.Write([]byte(e.Operator.Principal()))
	h.Write(e.Instance[:])
	var address [16]byte
	copy(address[:6], []byte{0xfd, 0x45, 0xb8, 0x91, 0x56, 0x80})
	copy(address[6:], h.Sum(nil)[:10])
	return netip.AddrFrom16(address)
}

func (e Endpoint) Route() netip.Prefix {
	address := e.Address()
	if !address.IsValid() {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(address, 128)
}
