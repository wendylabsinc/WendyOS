package rtps

import (
	"encoding/binary"
	"math"
	"sort"
)

// Active reader announcements stay available for the life of the reader.
// Retain a bounded tail of disposals for peers that missed an unsubscribe;
// older sequence numbers are explicitly retired through HEARTBEAT/GAP.
const maxSubscriptionDisposals = 64

func (p *Participant) retainSubscriptionDisposalLocked(sn SequenceNumber, packet []byte) {
	p.subHistory[sn] = packet
	p.subDisposals = append(p.subDisposals, sn)
	if len(p.subDisposals) > maxSubscriptionDisposals {
		delete(p.subHistory, p.subDisposals[0])
		p.subDisposals = p.subDisposals[1:]
	}
}

func putSequence(dst []byte, sn SequenceNumber) {
	binary.LittleEndian.PutUint32(dst[:4], uint32(int32(sn>>32)))
	binary.LittleEndian.PutUint32(dst[4:8], uint32(sn))
}

func (p *Participant) subscriptionHeartbeatLocked() []byte {
	last := p.seqByWriter[entitySEDPSubWriter]
	first := last
	for sn := range p.subHistory {
		if sn < first {
			first = sn
		}
	}
	p.subHeartbeat++
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[:4], entitySEDPSubReader)
	binary.BigEndian.PutUint32(body[4:8], entitySEDPSubWriter)
	putSequence(body[8:16], first)
	putSequence(body[16:24], last)
	binary.LittleEndian.PutUint32(body[24:28], p.subHeartbeat)
	return buildMessage(p.prefix, buildSubmessage(subHEARTBEAT, 0, body))
}

func subscriptionGap(first, end SequenceNumber) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[:4], entitySEDPSubReader)
	binary.BigEndian.PutUint32(body[4:8], entitySEDPSubWriter)
	putSequence(body[8:16], first)
	putSequence(body[16:24], end) // exclusive; empty bitmap
	return buildSubmessage(subGAP, 0, body)
}

// Replay in sequence order, including explicit gaps for superseded instances.
// Merely resending active DATA leaves a new reliable reader waiting forever
// for the subscriptions from before its publisher restarted.
func (p *Participant) subscriptionReplayLocked() [][]byte {
	if len(p.subHistory) == 0 {
		return nil
	}
	seqs := make([]SequenceNumber, 0, len(p.subHistory))
	for sn := range p.subHistory {
		seqs = append(seqs, sn)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	packets := make([][]byte, 0, len(seqs)+2)
	next := SequenceNumber(1)
	for _, sn := range seqs {
		if sn > next {
			packets = append(packets, buildMessage(p.prefix, subscriptionGap(next, sn)))
		}
		packets = append(packets, p.subHistory[sn])
		next = sn + 1
	}
	packets = append(packets, p.subscriptionHeartbeatLocked())
	return packets
}

// Only known discovery peers may request our subscription history. A request
// is bounded to the RTPS 256-bit window, directed to our builtin writer, and
// answered at the peer's advertised listening locator (never its output port).
func (p *Participant) handleSubscriptionAck(prefix GUIDPrefix, s Submessage) {
	b, order := s.Body, s.Endian
	if len(b) < 24 || binary.BigEndian.Uint32(b[:4]) != entitySEDPSubReader || binary.BigEndian.Uint32(b[4:8]) != entitySEDPSubWriter {
		return
	}
	base := SequenceNumber(int64(int32(order.Uint32(b[8:12])))<<32 | int64(order.Uint32(b[12:16])))
	bits := order.Uint32(b[16:20])
	if base < 1 || bits > acknackMaxBits || base > math.MaxInt64-SequenceNumber(bits) {
		return
	}
	words := (bits + 31) / 32
	if len(b) != 24+int(words)*4 {
		return
	}
	count := order.Uint32(b[20+words*4 : 24+words*4])
	p.mu.Lock()
	defer p.mu.Unlock()
	locators, known := p.peers[prefix]
	if !known {
		return
	}
	if previous, seen := p.subAckCount[prefix]; seen && int32(count-previous) <= 0 {
		return
	}
	p.subAckCount[prefix] = count
	last := p.seqByWriter[entitySEDPSubWriter]
	var packets [][]byte
	for bit := uint32(0); bit < bits; bit++ {
		word := order.Uint32(b[20+(bit/32)*4 : 24+(bit/32)*4])
		sn := base + SequenceNumber(bit)
		if word&(1<<(31-bit%32)) == 0 || sn > last {
			continue
		}
		if packet, ok := p.subHistory[sn]; ok {
			packets = append(packets, packet)
		} else {
			packets = append(packets, buildMessage(p.prefix, buildInfoDst(prefix), subscriptionGap(sn, sn+1)))
		}
	}
	// An empty acknowledgement must not start an ACK/HEARTBEAT reply loop.
	if len(packets) == 0 {
		return
	}
	packets = append(packets, p.subscriptionHeartbeatLocked())
	for _, packet := range packets {
		for _, locator := range locators {
			if addr, ok := locator.UDPAddr(); ok {
				p.sendTo(addr, packet)
			}
		}
	}
}
