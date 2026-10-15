package localmesh

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	LinkALPN           = "wendy-local-mesh/1"
	TunnelMTU          = 1280 // IPv6 minimum; QUIC fragments below are link-local only.
	DatagramLimit      = 1100
	PacketIP      byte = 1
	PacketBabel   byte = 2
	// Routed app sessions use this UDP port on the mesh host VIP. Keep the
	// carrier's bounded admission priority and the app listener in sync.
	AppSessionPort = 43021
	fragmentHeader = 9 // kind, packet ID, total size, offset
)

// EncodeIP fragments one complete IP packet into bounded QUIC datagrams. IDs
// are scoped to a fresh authenticated QUIC connection; no cross-link state.
func EncodeIP(id uint32, packet []byte) ([][]byte, error) {
	if !validIP(packet) {
		return nil, errors.New("invalid tunnel IP packet")
	}
	var out [][]byte
	for offset := 0; offset < len(packet); {
		n := min(DatagramLimit-fragmentHeader, len(packet)-offset)
		d := make([]byte, fragmentHeader+n)
		d[0] = PacketIP
		binary.BigEndian.PutUint32(d[1:5], id)
		binary.BigEndian.PutUint16(d[5:7], uint16(len(packet)))
		binary.BigEndian.PutUint16(d[7:9], uint16(offset))
		copy(d[9:], packet[offset:offset+n])
		out = append(out, d)
		offset += n
	}
	return out, nil
}

func validIP(p []byte) bool {
	if len(p) < 20 || len(p) > TunnelMTU {
		return false
	}
	switch p[0] >> 4 {
	case 4:
		h := int(p[0]&15) * 4
		return h >= 20 && h <= len(p) && int(binary.BigEndian.Uint16(p[2:4])) == len(p)
	case 6:
		return len(p) >= 40 && int(binary.BigEndian.Uint16(p[4:6]))+40 == len(p)
	}
	return false
}

type partialIP struct {
	data          []byte
	first, second bool
	expires       time.Time
}

// IPAssembler is single-caller and bounded to 32 pending packets for one link.
// At most two fixed-position fragments are accepted, avoiding overlaps and
// arbitrary-offset attacks. Loss is ordinary IP packet loss, not a stream stall.
type IPAssembler struct{ pending map[uint32]partialIP }

func (a *IPAssembler) Receive(d []byte, now time.Time) ([]byte, error) {
	if len(d) <= fragmentHeader || len(d) > DatagramLimit || d[0] != PacketIP {
		return nil, errors.New("invalid IP datagram")
	}
	id := binary.BigEndian.Uint32(d[1:5])
	total := int(binary.BigEndian.Uint16(d[5:7]))
	offset := int(binary.BigEndian.Uint16(d[7:9]))
	const chunk = DatagramLimit - fragmentHeader
	if total < 20 || total > TunnelMTU || (offset != 0 && offset != chunk) || offset >= total || len(d)-fragmentHeader != min(chunk, total-offset) {
		return nil, errors.New("invalid IP fragment")
	}
	if total <= chunk {
		p := append([]byte(nil), d[9:]...)
		if !validIP(p) {
			return nil, errors.New("invalid reassembled IP")
		}
		return p, nil
	}
	if a.pending == nil {
		a.pending = map[uint32]partialIP{}
	}
	for key, p := range a.pending {
		if !p.expires.After(now) {
			delete(a.pending, key)
		}
	}
	p, ok := a.pending[id]
	if !ok {
		if len(a.pending) >= 32 {
			return nil, nil
		} // bounded congestion loss
		// A BLE peer can take several seconds to deliver the second half of
		// an IP packet under contention. The 32-packet cap bounds memory.
		p = partialIP{data: make([]byte, total), expires: now.Add(10 * time.Second)}
	}
	if len(p.data) != total {
		delete(a.pending, id)
		return nil, errors.New("conflicting IP fragment length")
	}
	seen := p.first
	if offset != 0 {
		seen = p.second
	}
	if seen {
		for i, b := range d[9:] {
			if p.data[offset+i] != b {
				delete(a.pending, id)
				return nil, errors.New("conflicting duplicate IP fragment")
			}
		}
		return nil, nil
	}
	copy(p.data[offset:], d[9:])
	if offset == 0 {
		p.first = true
	} else {
		p.second = true
	}
	if p.first && p.second {
		delete(a.pending, id)
		if !validIP(p.data) {
			return nil, errors.New("invalid reassembled IP")
		}
		return p.data, nil
	}
	a.pending[id] = p
	return nil, nil
}
