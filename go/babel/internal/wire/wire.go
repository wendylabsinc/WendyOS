// Package wire implements the RFC 8966 packet codec and RFC 9229 AE 4.
// Decoder state is strictly packet-local. No protocol state lives here.
package wire

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

const Infinity uint16 = 65535
const (
	AckRequest = 2
	Ack        = 3
	Hello      = 4
	IHU        = 5
	Router     = 6
	NextHop    = 7
	Update     = 8
	Request    = 9
	SeqRequest = 10
)

type Message struct {
	Type                            uint8
	Unicast                         bool
	Wildcard                        bool
	V4ViaV6                         bool
	Seqno, Metric, Interval, Opaque uint16
	HopCount                        uint8
	RouterID                        uint64
	Prefix                          netip.Prefix
	Address                         netip.Addr
}

var ErrPacket = errors.New("malformed Babel packet")

func validID(id uint64) bool { return id != 0 && id != ^uint64(0) }
func u16(b []byte) uint16    { return binary.BigEndian.Uint16(b) }

// Decode ignores the trailer (no supported trailer extensions). Invalid outer
// framing rejects the packet; unsupported or invalid individual TLVs are skipped.
func Decode(packet []byte, source netip.Addr) ([]Message, error) {
	if len(packet) < 4 || packet[0] != 42 || packet[1] != 2 || int(u16(packet[2:])) > len(packet)-4 {
		return nil, ErrPacket
	}
	body := packet[4 : 4+int(u16(packet[2:]))]
	var out []Message
	var rid uint64
	var previous [5]netip.Prefix
	var hops [2]netip.Addr
	if source.Is4() {
		hops[0] = source
	} else if source.Is6() {
		hops[1] = source
	}
	for len(body) > 0 {
		t := body[0]
		body = body[1:]
		if t == 0 {
			continue
		}
		if len(body) < 1 || int(body[0]) > len(body)-1 {
			return nil, ErrPacket
		}
		p := body[1 : 1+int(body[0])]
		body = body[1+int(body[0]):]
		m := Message{Type: t}
		n := 0
		switch t {
		case AckRequest, Hello:
			if len(p) < 6 {
				continue
			}
			n = 6
			m.Seqno = u16(p[2:])
			m.Opaque = m.Seqno
			m.Interval = u16(p[4:])
			m.Unicast = p[0]&128 != 0
			if t == AckRequest && m.Interval == 0 {
				continue
			}
		case Ack:
			if len(p) < 2 {
				continue
			}
			n = 2
			m.Opaque = u16(p)
		case IHU, NextHop:
			base := 2
			if t == IHU {
				base = 6
			}
			if len(p) < base {
				continue
			}
			a, size, ok := address(p[0], p[base:])
			if !ok || (t == NextHop && p[0] == 0) {
				continue
			}
			n = base + size
			m.Address = a
			if t == NextHop {
				if a.Is4() {
					hops[0] = a
				} else {
					hops[1] = a
				}
			} else {
				m.Metric = u16(p[2:])
				m.Interval = u16(p[4:])
				if m.Interval == 0 {
					continue
				}
			}
		case Router:
			if len(p) < 10 {
				continue
			}
			n = 10
			rid = binary.BigEndian.Uint64(p[2:])
			m.RouterID = rid
		case Update:
			if len(p) < 10 {
				continue
			}
			ae, flags, plen, omitted := p[0], p[1], int(p[2]), int(p[3])
			m.Interval = u16(p[4:])
			m.Seqno = u16(p[6:])
			m.Metric = u16(p[8:])
			m.V4ViaV6 = ae == 4
			if ae == 0 {
				if plen != 0 || omitted != 0 || m.Metric != Infinity {
					continue
				}
				n = 10
				m.Wildcard = true
			} else {
				if ae > 4 {
					continue
				}
				prefix, size, ok := prefix(ae, plen, omitted, p[10:], previous[ae])
				if !ok {
					continue
				}
				n = 10 + size
				m.Prefix = prefix
				if flags&128 != 0 && ae != 3 {
					previous[ae] = prefix
				}
				if flags&64 != 0 {
					a := prefix.Addr().As16()
					rid = binary.BigEndian.Uint64(a[8:])
					if prefix.Addr().Is4() {
						a4 := prefix.Addr().As4()
						rid = uint64(binary.BigEndian.Uint32(a4[:]))
					}
				}
				m.RouterID = rid
				family := 1
				if ae == 1 {
					family = 0
				}
				m.Address = hops[family]
				if m.Metric != Infinity && (!validID(rid) || !m.Address.IsValid()) {
					continue
				}
			}
			if m.Interval == 0 {
				continue
			}
		case Request, SeqRequest:
			base := 2
			if t == SeqRequest {
				base = 14
			}
			if len(p) < base {
				continue
			}
			ae := p[0]
			if ae == 0 {
				if t != Request || p[1] != 0 {
					continue
				}
				m.Wildcard = true
				n = base
			} else {
				pre, size, ok := prefix(ae, int(p[1]), 0, p[base:], netip.Prefix{})
				if !ok {
					continue
				}
				m.Prefix = pre
				n = base + size
			}
			if t == SeqRequest {
				m.Seqno = u16(p[2:])
				m.HopCount = p[4]
				m.RouterID = binary.BigEndian.Uint64(p[6:])
				if m.HopCount == 0 || !validID(m.RouterID) {
					continue
				}
			}
		default:
			continue
		}
		// RFC 8966 §4.5: update parser context BEFORE mandatory sub-TLV filtering.
		if !subTLVs(p[n:]) {
			continue
		}
		if t != Router && t != NextHop {
			out = append(out, m)
		}
	}
	return out, nil
}

func subTLVs(p []byte) bool {
	for len(p) > 0 {
		t := p[0]
		p = p[1:]
		if t == 0 {
			continue
		}
		if len(p) < 1 || int(p[0]) > len(p)-1 {
			return false
		}
		if t&128 != 0 {
			return false
		}
		p = p[1+int(p[0]):]
	}
	return true
}

func address(ae byte, p []byte) (netip.Addr, int, bool) {
	switch ae {
	case 0:
		return netip.Addr{}, 0, true
	case 1:
		if len(p) >= 4 {
			return netip.AddrFrom4([4]byte(p[:4])), 4, true
		}
	case 2:
		if len(p) >= 16 {
			return netip.AddrFrom16([16]byte(p[:16])), 16, true
		}
	case 3:
		if len(p) >= 8 {
			a := [16]byte{0xfe, 0x80}
			copy(a[8:], p[:8])
			return netip.AddrFrom16(a), 8, true
		}
	}
	return netip.Addr{}, 0, false
}

func prefix(ae byte, bits, omitted int, p []byte, prev netip.Prefix) (netip.Prefix, int, bool) {
	max := 128
	if ae == 1 || ae == 4 {
		max = 32
	}
	if ae < 1 || ae > 4 || bits > max {
		return netip.Prefix{}, 0, false
	}
	start := 0
	var a [16]byte
	if ae == 3 {
		if omitted != 0 || bits < 64 {
			return netip.Prefix{}, 0, false
		}
		a[0] = 0xfe
		a[1] = 0x80
		start = 8
	}
	n := (bits+7)/8 - start - omitted
	if n < 0 || n > len(p) || omitted < 0 {
		return netip.Prefix{}, 0, false
	}
	if omitted > 0 {
		if !prev.IsValid() {
			return netip.Prefix{}, 0, false
		}
		copy(a[:], prev.Addr().AsSlice())
	}
	copy(a[start+omitted:], p[:n])
	var addr netip.Addr
	if max == 32 {
		addr = netip.AddrFrom4([4]byte(a[:4]))
	} else {
		addr = netip.AddrFrom16(a)
	}
	return netip.PrefixFrom(addr, bits).Masked(), n, true
}

func put16(p []byte, x uint16)    { binary.BigEndian.PutUint16(p, x) }
func tlv(t byte, p []byte) []byte { return append([]byte{t, byte(len(p))}, p...) }

// Encode emits self-contained messages, deliberately without prefix compression.
// Each Update includes its Router-ID and explicit next hop where needed, so it
// can be split into packets without leaking parser state across packet boundaries.
func Encode(m Message) []byte {
	var p []byte
	var before []byte
	switch m.Type {
	case Hello, AckRequest:
		p = make([]byte, 6)
		if m.Type == Hello && m.Unicast {
			p[0] = 128
		}
		put16(p[2:], m.Seqno)
		if m.Type == AckRequest {
			put16(p[2:], m.Opaque)
		}
		put16(p[4:], m.Interval)
	case Ack:
		p = make([]byte, 2)
		put16(p, m.Opaque)
	case IHU:
		p = make([]byte, 6)
		put16(p[2:], m.Metric)
		put16(p[4:], m.Interval)
		if m.Address.IsValid() {
			ae, a := encodeAddress(m.Address)
			p[0] = ae
			p = append(p, a...)
		}
	case Update:
		p = make([]byte, 10)
		put16(p[4:], m.Interval)
		put16(p[6:], m.Seqno)
		put16(p[8:], m.Metric)
		if !m.Wildcard {
			ae, a, bits := encodePrefix(m.Prefix)
			if ae == 1 && m.V4ViaV6 {
				ae = 4
			}
			p[0] = ae
			p[2] = bits
			p = append(p, a...)
		}
		if m.Metric != Infinity {
			r := make([]byte, 10)
			binary.BigEndian.PutUint64(r[2:], m.RouterID)
			before = tlv(Router, r)
			if m.Address.IsValid() {
				ae, a := encodeAddress(m.Address)
				before = append(before, tlv(NextHop, append([]byte{ae, 0}, a...))...)
			}
		}
	case Request, SeqRequest:
		base := 2
		if m.Type == SeqRequest {
			base = 14
		}
		p = make([]byte, base)
		if !m.Wildcard {
			ae, a, bits := encodePrefix(m.Prefix)
			p[0] = ae
			p[1] = bits
			p = append(p, a...)
		}
		if m.Type == SeqRequest {
			put16(p[2:], m.Seqno)
			p[4] = m.HopCount
			binary.BigEndian.PutUint64(p[6:], m.RouterID)
		}
	}
	return append(before, tlv(m.Type, p)...)
}
func encodeAddress(a netip.Addr) (byte, []byte) {
	if a.Is4() {
		return 1, a.AsSlice()
	}
	if a.IsLinkLocalUnicast() {
		s := a.As16()
		if s[2] == 0 && s[3] == 0 && s[4] == 0 && s[5] == 0 && s[6] == 0 && s[7] == 0 {
			return 3, s[8:]
		}
	}
	return 2, a.AsSlice()
}
func encodePrefix(p netip.Prefix) (byte, []byte, byte) {
	ae := byte(2)
	if p.Addr().Is4() {
		ae = 1
	}
	return ae, p.Masked().Addr().AsSlice()[:(p.Bits()+7)/8], byte(p.Bits())
}

func Pack(messages []Message, max int) [][]byte {
	var packets [][]byte
	packet := []byte{42, 2, 0, 0}
	flush := func() {
		if len(packet) > 4 {
			put16(packet[2:], uint16(len(packet)-4))
			packets = append(packets, packet)
			packet = []byte{42, 2, 0, 0}
		}
	}
	for _, m := range messages {
		b := Encode(m)
		if len(packet)+len(b) > max {
			flush()
		}
		packet = append(packet, b...)
	}
	flush()
	return packets
}
