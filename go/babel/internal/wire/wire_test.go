package wire

import (
	"encoding/hex"
	"net/netip"
	"reflect"
	"testing"
)

var peer = netip.MustParseAddr("fe80::2")

func raw(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIndependentVectors(t *testing.T) {
	tests := []struct {
		name, hex string
		want      []Message
	}{
		{"hello", "2a02000804068000ffff0064", []Message{{Type: Hello, Unicast: true, Seqno: 65535, Opaque: 65535, Interval: 100}}},
		{"ack", "2a02000403021234", []Message{{Type: Ack, Opaque: 0x1234}}},
		{"wildcard", "2a020010080a0000000000640000ffff09020000", []Message{{Type: Update, Wildcard: true, Interval: 100, Metric: Infinity}, {Type: Request, Wildcard: true}}},
		{"v4via6", "2a02001c060a00000000000000000001080e04002000006400070000c0000201", []Message{{Type: Update, Prefix: netip.MustParsePrefix("192.0.2.1/32"), RouterID: 1, Seqno: 7, Interval: 100, V4ViaV6: true, Address: peer}}},
		{"trailer ignored", "2a0200040302123404068000ffff0064", []Message{{Type: Ack, Opaque: 0x1234}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode(raw(t, tt.hex), peer)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v err %v want %+v", got, err, tt.want)
			}
		})
	}
}

func TestParserContextAndMandatory(t *testing.T) {
	// First /32 update establishes AE4 compression state but is suppressed by
	// unknown mandatory sub-TLV. The following compressed update must use it.
	router := raw(t, "060a00000000000000000001")
	first := raw(t, "081004802000006400010000c00002018000")
	second := raw(t, "080b0400200300640001000002")
	body := append(append(router, first...), second...)
	packet := append([]byte{42, 2, 0, byte(len(body))}, body...)
	got, err := Decode(packet, peer)
	if err != nil || len(got) != 1 || got[0].Prefix.String() != "192.0.2.2/32" {
		t.Fatalf("%+v %v", got, err)
	}
	// AE1 cannot borrow AE4 compression state.
	packet[len(packet)-len(second)+2] = 1
	got, err = Decode(packet, peer)
	if err != nil || len(got) != 0 {
		t.Fatalf("AE context leaked: %+v %v", got, err)
	}
	// Packet-local router-ID state cannot leak from a prior packet either.
	packet = append([]byte{42, 2, 0, byte(len(second))}, second...)
	got, err = Decode(packet, peer)
	if err != nil || len(got) != 0 {
		t.Fatalf("packet context leaked: %+v %v", got, err)
	}
}

func TestNextHopMandatoryState(t *testing.T) {
	body := raw(t, "060c000000000000000000018000070c030000000000000000038000080a02000000006400010000")
	got, err := Decode(append([]byte{42, 2, 0, byte(len(body))}, body...), peer)
	if err != nil || len(got) != 1 || got[0].RouterID != 1 || got[0].Address.String() != "fe80::3" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestMalformedAndTruncation(t *testing.T) {
	p := Pack([]Message{{Type: Update, Prefix: netip.MustParsePrefix("fd00::1234/128"), RouterID: 1, Interval: 100, Seqno: 3}}, 512)[0]
	for n := 0; n < len(p); n++ {
		if _, err := Decode(p[:n], peer); err == nil {
			t.Fatalf("accepted truncated packet at %d", n)
		}
	}
	for _, s := range []string{"00020000", "2a010000", "2a02000108", "2a020003080aff"} {
		if _, err := Decode(raw(t, s), peer); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestPackBoundAndRoundTrip(t *testing.T) {
	var ms []Message
	for i := 0; i < 100; i++ {
		ms = append(ms, Message{Type: Update, Prefix: netip.MustParsePrefix("fd00::1234/128"), RouterID: 1, Seqno: uint16(i), Interval: 100, Metric: 123})
	}
	var got []Message
	for _, p := range Pack(ms, 512) {
		if len(p) > 512 {
			t.Fatal("oversized")
		}
		m, err := Decode(p, peer)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, m...)
	}
	if len(got) != 100 {
		t.Fatal(len(got))
	}
	for i, m := range got {
		if m.Seqno != uint16(i) || m.Prefix != ms[i].Prefix {
			t.Fatal(m)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, m := range []Message{{Type: Hello, Interval: 100}, {Type: Update, Prefix: netip.MustParsePrefix("fd00::/64"), RouterID: 1, Interval: 100}, {Type: Request, Wildcard: true}} {
		f.Add(Pack([]Message{m}, 512)[0])
	}
	f.Fuzz(func(t *testing.T, p []byte) {
		if len(p) > 65535 {
			return
		}
		a, ea := Decode(p, peer)
		b, eb := Decode(p, peer)
		if (ea == nil) != (eb == nil) || !reflect.DeepEqual(a, b) {
			t.Fatal("nondeterministic decode")
		}
		for _, m := range a {
			if m.Prefix.IsValid() && m.Prefix != m.Prefix.Masked() {
				t.Fatal("noncanonical prefix")
			}
		}
	})
}

func TestAddressEncodingsAndRequestFamilies(t *testing.T) {
	for _, tc := range []struct {
		prefix, hop string
		via         bool
	}{{"192.0.2.1/32", "192.0.2.2", false}, {"192.0.2.1/32", "fe80::2", true}, {"fd00::1/128", "fe80::2", false}, {"fd00::/64", "2001:db8::1", false}} {
		m := Message{Type: Update, Prefix: netip.MustParsePrefix(tc.prefix), Address: netip.MustParseAddr(tc.hop), RouterID: 1, Seqno: 42, Metric: 96, Interval: 400, V4ViaV6: tc.via}
		p := Pack([]Message{m}, 512)[0]
		got, err := Decode(p, peer)
		if err != nil || len(got) != 1 || got[0] != m {
			t.Fatal(tc, got, err)
		}
	}
	for _, s := range []string{"192.0.2.1/32", "fd00::1/128"} {
		p := netip.MustParsePrefix(s)
		for _, kind := range []uint8{Request, SeqRequest} {
			m := Message{Type: kind, Prefix: p}
			if kind == SeqRequest {
				m.RouterID = 1
				m.HopCount = 64
				m.Seqno = 7
			}
			encoded := Pack([]Message{m}, 512)[0]
			got, err := Decode(encoded, peer)
			if err != nil || len(got) != 1 || got[0] != m {
				t.Fatal(got, err)
			}
			if p.Addr().Is4() {
				encoded[6] = 4
				got, err = Decode(encoded, peer)
				if err != nil || len(got) != 1 || got[0].Prefix != p {
					t.Fatal("AE4 request", got, err)
				}
			}
		}
	}
}

func TestRouterFlagAndPrefixMasking(t *testing.T) {
	// R flag derives the router ID from the masked IPv4 prefix.
	p := raw(t, "2a020010080e01401f00006400070000c0000203")
	got, err := Decode(p, netip.MustParseAddr("192.0.2.4"))
	if err != nil || len(got) != 1 || got[0].Prefix.String() != "192.0.2.2/31" || got[0].RouterID != 0xc0000202 {
		t.Fatal(got, err)
	}
}
