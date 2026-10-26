package mesh

import (
	"encoding/binary"
	"errors"
	"net/netip"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

func udpReplyPacket(payload []byte, source, destination netip.AddrPort) ([]byte, error) {
	if !source.Addr().Is4() || !destination.Addr().Is4() || source.Port() == 0 || destination.Port() == 0 || len(payload) > meshsession.MaxUDPMessageSize {
		return nil, errors.New("invalid net UDP reply packet")
	}
	// Only the UDP header is supplied. Linux creates and, when necessary,
	// fragments the IPv4 packet while retaining this checksum across fragments.
	packet := make([]byte, 8+len(payload))
	src, dst := source.Addr().As4(), destination.Addr().As4()
	udp := packet
	binary.BigEndian.PutUint16(udp[0:2], source.Port())
	binary.BigEndian.PutUint16(udp[2:4], destination.Port())
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	// IPv4 pseudo-header: source, destination, zero, protocol and UDP length.
	pseudo := uint32(binary.BigEndian.Uint16(src[:2])) + uint32(binary.BigEndian.Uint16(src[2:])) + uint32(binary.BigEndian.Uint16(dst[:2])) + uint32(binary.BigEndian.Uint16(dst[2:])) + 17 + uint32(len(udp))
	checksum := internetChecksum(udp, pseudo)
	if checksum == 0 {
		checksum = 0xffff // RFC 768: zero on the wire means no checksum.
	}
	binary.BigEndian.PutUint16(udp[6:8], checksum)
	return packet, nil
}

func internetChecksum(data []byte, sum uint32) uint16 {
	for len(data) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(data[:2]))
		data = data[2:]
	}
	if len(data) != 0 {
		sum += uint32(data[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
