package mesh

import (
	"bytes"
	"encoding/hex"
	"net/netip"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

func TestUDPReplyPacketWireVectors(t *testing.T) {
	source := netip.MustParseAddrPort("10.99.0.1:7777")
	destination := netip.MustParseAddrPort("10.79.42.2:41000")
	// Independently calculated UDP pseudo-header checksum; the
	// odd-length payload must pad only the checksum, never the wire packet.
	packet, err := udpReplyPacket([]byte("hello"), source, destination)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "1e61a028000dbec368656c6c6f"
	if hex.EncodeToString(packet) != golden {
		t.Fatalf("packet=%x", packet)
	}
	// This exact two-byte payload calculates to checksum zero. RFC 768
	// requires 0xffff on the wire, reserving 0x0000 for disabled checking.
	packet, err = udpReplyPacket([]byte{0x02, 0x9c}, source, destination)
	if err != nil {
		t.Fatal(err)
	}
	if packet[6] != 0xff || packet[7] != 0xff {
		t.Fatalf("zero checksum encoding=%x", packet[6:8])
	}
	packet, err = udpReplyPacket(bytes.Repeat([]byte{0x87}, meshsession.MaxUDPMessageSize), source, destination)
	if err != nil || len(packet) != 65515 {
		t.Fatalf("maximum UDP datagram: %d %v", len(packet), err)
	}
}

func TestUDPReplyPacketRejectsInvalidTuplesAndPayloads(t *testing.T) {
	valid := netip.MustParseAddrPort("10.99.0.1:7777")
	for _, address := range []netip.AddrPort{{}, netip.MustParseAddrPort("[::1]:7777"), netip.MustParseAddrPort("10.99.0.1:0")} {
		if _, err := udpReplyPacket([]byte("x"), address, valid); err == nil {
			t.Fatalf("accepted source %s", address)
		}
		if _, err := udpReplyPacket([]byte("x"), valid, address); err == nil {
			t.Fatalf("accepted destination %s", address)
		}
	}
	for _, payload := range [][]byte{make([]byte, meshsession.MaxUDPMessageSize+1)} {
		if _, err := udpReplyPacket(payload, valid, valid); err == nil {
			t.Fatalf("accepted payload length %d", len(payload))
		}
	}
}
