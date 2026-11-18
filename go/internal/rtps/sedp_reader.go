package rtps

import (
	"encoding/binary"
	"errors"
	"time"
)

// A publishing participant has to learn every reader on the domain, including
// the ones announced before it joined. Their participant does not resend those
// unprompted: it names the range in a HEARTBEAT and waits to be asked. This
// file is the reliable-reader side of that exchange, for subscription
// announcements only.

// rxWindowBits is how far past the first missing sequence number arrivals are
// remembered. Anything further is asked for again once the window reaches it.
const rxWindowBits = 256

// acknackInterval is the least time between two ACKNACKs to one writer. It
// bounds the traffic a misbehaving writer can provoke with heartbeats.
const acknackInterval = 100 * time.Millisecond

// rxWindow tracks which sequence numbers of one remote reliable writer have
// arrived, so an ACKNACK asks only for what is missing. The zero value has
// received nothing and expects sequence number 1.
type rxWindow struct {
	// done counts the leading sequence numbers that are settled: received,
	// or declared gone by the writer.
	done SequenceNumber
	// got has bit i set when sequence number done+1+i has arrived.
	got [rxWindowBits / 64]uint64
}

// next is the lowest sequence number that is still outstanding.
func (w *rxWindow) next() SequenceNumber { return w.done + 1 }

// mark records that sn arrived or will never come.
func (w *rxWindow) mark(sn SequenceNumber) {
	off := sn - w.next()
	if off < 0 || off >= rxWindowBits {
		return
	}
	w.got[off/64] |= 1 << (off % 64)
	for w.got[0]&1 != 0 {
		w.shift(1)
	}
}

// advanceTo settles every sequence number below first.
func (w *rxWindow) advanceTo(first SequenceNumber) {
	if n := first - w.next(); n > 0 {
		w.shift(n)
	}
	for w.got[0]&1 != 0 {
		w.shift(1)
	}
}

// shift settles the next n sequence numbers.
func (w *rxWindow) shift(n SequenceNumber) {
	w.done += n
	if n >= rxWindowBits {
		w.got = [rxWindowBits / 64]uint64{}
		return
	}
	words, bits := int(n/64), uint(n%64)
	for i := range w.got {
		var v uint64
		if i+words < len(w.got) {
			v = w.got[i+words] >> bits
			if bits != 0 && i+words+1 < len(w.got) {
				v |= w.got[i+words+1] << (64 - bits)
			}
		}
		w.got[i] = v
	}
}

// missing lists the outstanding sequence numbers up to last that fall inside
// the window.
func (w *rxWindow) missing(last SequenceNumber) []SequenceNumber {
	var out []SequenceNumber
	for off := SequenceNumber(0); off < rxWindowBits && w.next()+off <= last; off++ {
		if w.got[off/64]&(1<<(off%64)) == 0 {
			out = append(out, w.next()+off)
		}
	}
	return out
}

// sedpSubReader is our reader of one peer's subscription announcements.
type sedpSubReader struct {
	window rxWindow
	// count numbers our ACKNACKs to this writer; it must keep increasing or
	// the writer discards them as duplicates.
	count   uint32
	lastAck time.Time
}

// subReaderLocked returns the receive state for a known peer's subscription
// writer, or nil for a participant that never announced itself.
func (p *Participant) subReaderLocked(prefix GUIDPrefix) *sedpSubReader {
	if r := p.subReaders[prefix]; r != nil {
		return r
	}
	if _, known := p.peers[prefix]; !known {
		return nil
	}
	r := &sedpSubReader{}
	p.subReaders[prefix] = r
	return r
}

// markSubscriptionReceived notes one arrived subscription announcement.
func (p *Participant) markSubscriptionReceived(prefix GUIDPrefix, sn SequenceNumber) {
	p.mu.Lock()
	if r := p.subReaderLocked(prefix); r != nil {
		r.window.mark(sn)
	}
	p.mu.Unlock()
}

// handleSubscriptionHeartbeat asks a peer for the subscription announcements
// it has and we do not. A heartbeat marked final is answered only when
// something is missing; one that is not must be answered either way, and the
// acknowledgement is what lets the writer stop sending them.
//
// The answer goes to the locators the peer announced for discovery traffic,
// not to the address the heartbeat came from: Fast DDS sends from a socket it
// never reads.
func (p *Participant) handleSubscriptionHeartbeat(prefix GUIDPrefix, hb *HeartbeatSubmessage, final bool) {
	if hb.FirstSN < 1 || hb.LastSN < hb.FirstSN-1 {
		return
	}
	p.mu.Lock()
	r := p.subReaderLocked(prefix)
	if r == nil {
		p.mu.Unlock()
		return
	}
	r.window.advanceTo(hb.FirstSN)
	missing := r.window.missing(hb.LastSN)
	now := time.Now()
	if (len(missing) == 0 && final) || now.Sub(r.lastAck) < acknackInterval {
		p.mu.Unlock()
		return
	}
	r.lastAck = now
	r.count++
	ack := buildAcknackSet(entitySEDPSubReader, entitySEDPSubWriter, r.window.next(), missing, r.count)
	meta := p.peers[prefix]
	p.mu.Unlock()
	dgram := buildMessage(p.prefix, buildInfoDst(prefix), ack)
	for _, l := range meta {
		if addr, ok := l.UDPAddr(); ok && p.sendTo(addr, dgram) {
			p.statAcksSent.Add(1)
		}
	}
}

// handleSubscriptionGap settles the sequence numbers a peer says it will not
// send, so they are not asked for again.
func (p *Participant) handleSubscriptionGap(prefix GUIDPrefix, gap *GapSubmessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.subReaderLocked(prefix)
	if r == nil {
		return
	}
	if gap.Start <= r.window.next() {
		r.window.advanceTo(gap.ListBase)
	} else {
		for sn := gap.Start; sn < gap.ListBase && sn < r.window.next()+rxWindowBits; sn++ {
			r.window.mark(sn)
		}
	}
	for _, sn := range gap.Extra {
		r.window.mark(sn)
	}
}

// buildAcknackSet frames an ACKNACK that has everything below base and asks
// for exactly the sequence numbers in missing, which must be ascending and
// within 256 of base. With nothing missing it is a plain acknowledgement,
// marked final because no answer is wanted.
func buildAcknackSet(readerID, writerID uint32, base SequenceNumber, missing []SequenceNumber, count uint32) []byte {
	var numBits uint32
	if len(missing) > 0 {
		numBits = uint32(missing[len(missing)-1]-base) + 1
	}
	words := int(numBits+31) / 32
	body := make([]byte, 24+4*words)
	binary.BigEndian.PutUint32(body[0:4], readerID)
	binary.BigEndian.PutUint32(body[4:8], writerID)
	putSequenceNumber(body[8:16], base)
	binary.LittleEndian.PutUint32(body[16:20], numBits)
	for _, sn := range missing {
		bit := int(sn - base)
		// Bit 0 is the most significant bit of the first word.
		word := body[20+4*(bit/32) : 24+4*(bit/32)]
		binary.LittleEndian.PutUint32(word, binary.LittleEndian.Uint32(word)|1<<(31-bit%32))
	}
	binary.LittleEndian.PutUint32(body[20+4*words:], count)
	var flags uint8
	if len(missing) == 0 {
		flags = 0x02
	}
	return buildSubmessage(subACKNACK, flags, body)
}

// GapSubmessage is a parsed GAP: the writer will never send the sequence
// numbers from Start up to but not including ListBase, nor those in Extra.
type GapSubmessage struct {
	ReaderID uint32
	WriterID uint32
	Start    SequenceNumber
	ListBase SequenceNumber
	Extra    []SequenceNumber
}

// ParseGap decodes a GAP submessage body:
// readerId(4) writerId(4) gapStart(8) bitmapBase(8) numBits(4) bitmap(4 per 32 bits).
func ParseGap(s Submessage) (*GapSubmessage, error) {
	b, order := s.Body, s.Endian
	if len(b) < 28 {
		return nil, ErrShort
	}
	numBits := order.Uint32(b[24:28])
	if numBits > acknackMaxBits {
		return nil, errors.New("rtps: GAP sequence number set exceeds 256 bits")
	}
	if len(b) < 28+4*(int(numBits+31)/32) {
		return nil, ErrShort
	}
	sn := func(off int) SequenceNumber {
		hi := int64(int32(order.Uint32(b[off : off+4])))
		lo := int64(order.Uint32(b[off+4 : off+8]))
		return SequenceNumber(hi<<32 | lo)
	}
	gap := &GapSubmessage{
		ReaderID: binary.BigEndian.Uint32(b[0:4]),
		WriterID: binary.BigEndian.Uint32(b[4:8]),
		Start:    sn(8),
		ListBase: sn(16),
	}
	for bit := 0; bit < int(numBits); bit++ {
		if order.Uint32(b[28+4*(bit/32):32+4*(bit/32)])&(1<<(31-bit%32)) != 0 {
			gap.Extra = append(gap.Extra, gap.ListBase+SequenceNumber(bit))
		}
	}
	return gap, nil
}
