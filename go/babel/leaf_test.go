package babel

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel/internal/wire"
)

func TestLeafPacketConfinement(t *testing.T) {
	own := netip.MustParsePrefix("fd45:b891:567f::1/128")
	other := netip.MustParsePrefix("fd45:b891:567f::2/128")
	peer := netip.MustParseAddr("fe80::2")
	for _, tc := range []struct {
		name    string
		message wire.Message
		valid   bool
	}{
		{"own origin", wire.Message{Type: wire.Update, Prefix: own, RouterID: 7, Interval: 400, Seqno: 3}, true},
		{"own retraction", wire.Message{Type: wire.Update, Prefix: own, Interval: 400, Metric: Infinity}, true},
		{"other origin", wire.Message{Type: wire.Update, Prefix: other, RouterID: 7, Interval: 400}, false},
		{"other retraction", wire.Message{Type: wire.Update, Prefix: other, Interval: 400, Metric: Infinity}, false},
		{"aggregate", wire.Message{Type: wire.Update, Prefix: netip.MustParsePrefix("fd45:b891:567f::/48"), RouterID: 7, Interval: 400}, false},
		{"wildcard retraction", wire.Message{Type: wire.Update, Wildcard: true, Interval: 400, Metric: Infinity}, false},
		{"foreign next hop", wire.Message{Type: wire.Update, Prefix: own, RouterID: 7, Address: netip.MustParseAddr("fe80::9"), Interval: 400}, false},
		{"wildcard route request", wire.Message{Type: wire.Request, Wildcard: true}, true},
		{"foreign sequence request", wire.Message{Type: wire.SeqRequest, Prefix: other, RouterID: 7, HopCount: 1}, false},
		{"own sequence request", wire.Message{Type: wire.SeqRequest, Prefix: own, RouterID: 7, HopCount: 1}, true},
		{"hello", wire.Message{Type: wire.Hello, Interval: 100}, true},
		{"IHU", wire.Message{Type: wire.IHU, Interval: 200, Metric: 256}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := wire.Encode(tc.message)
			packet := append([]byte{42, 2, 0, 0}, body...)
			binary.BigEndian.PutUint16(packet[2:4], uint16(len(body)))
			if err := ValidateLeafPacket(packet, peer, own); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	if err := ValidateLeafPacket([]byte{42, 2, 0, 10}, peer, own); err == nil {
		t.Fatal("truncated packet passed leaf validation")
	}
}

func TestLeafPacketChecksCompressedPrefixContext(t *testing.T) {
	own := netip.MustParsePrefix("fd45:b891:567f::1/128")
	peer := netip.MustParseAddr("fe80::2")
	first := wire.Encode(wire.Message{Type: wire.Update, Prefix: own, RouterID: 7, Interval: 400})
	// Router-ID TLV occupies 12 bytes. Set the first Update's prefix-context
	// flag, then use 15 omitted address bytes in the following Update.
	first[12+3] = 0x80
	second := []byte{wire.Update, 11, 2, 0, 128, 15, 1, 144, 0, 0, 0, 0, 1}
	for _, suffix := range []byte{1, 2} {
		second[len(second)-1] = suffix
		body := append(append([]byte(nil), first...), second...)
		packet := append([]byte{42, 2, 0, 0}, body...)
		binary.BigEndian.PutUint16(packet[2:4], uint16(len(body)))
		err := ValidateLeafPacket(packet, peer, own)
		if (err == nil) != (suffix == 1) {
			t.Fatalf("compressed suffix %d: %v", suffix, err)
		}
	}
}
