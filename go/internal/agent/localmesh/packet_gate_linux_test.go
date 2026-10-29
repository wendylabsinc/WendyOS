//go:build linux

package localmesh

import (
	"net/netip"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestMeshPacketGateRejectsRawIPv4(t *testing.T) {
	destination := netip.MustParseAddr("fd45:b891:567f::42")
	n := &Node{forwardRoutes: []babel.Route{{Prefix: netip.PrefixFrom(destination, 128), Link: 7}}}
	packet := make([]byte, 40)
	packet[0] = 0x60
	copy(packet[24:], destination.AsSlice())
	if !tunCarries(n, 7, packet) || tunCarries(n, 8, packet) || !linkAdmits(n, packet) {
		t.Fatal("IPv6 payload did not follow its selected Babel route")
	}
	packet[0] = 0x45
	for _, address := range []string{"10.88.0.1", "8.8.8.8", "192.168.1.1"} {
		copy(packet[12:16], netip.MustParseAddr(address).AsSlice())
		copy(packet[16:20], netip.MustParseAddr(address).AsSlice())
		if tunCarries(n, 7, packet) || linkAdmits(n, packet) {
			t.Fatalf("raw IPv4 %s admitted by the IPv6 mesh", address)
		}
	}
	for _, short := range [][]byte{nil, {0x60}, make([]byte, 39)} {
		if tunCarries(n, 7, short) || linkAdmits(n, short) {
			t.Fatal("truncated packet admitted")
		}
	}
}
