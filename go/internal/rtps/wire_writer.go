package rtps

import (
	"encoding/binary"
	"errors"
	"time"
)

// Submessages only a writer sends or receives. A participant that does not
// publish never builds or parses these.

// putSequenceNumber writes a sequence number as its high int32 and low uint32.
func putSequenceNumber(b []byte, sn SequenceNumber) {
	binary.LittleEndian.PutUint32(b[0:4], uint32(int32(sn>>32)))
	binary.LittleEndian.PutUint32(b[4:8], uint32(sn))
}

// buildHeartbeat frames a HEARTBEAT saying which sequence numbers a writer
// still has: first through last, or nothing when first is last+1. With final
// set the reader answers only if it is missing something; without it the
// reader must always answer.
func buildHeartbeat(readerID, writerID uint32, first, last SequenceNumber, count uint32, final bool) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[0:4], readerID)
	binary.BigEndian.PutUint32(body[4:8], writerID)
	putSequenceNumber(body[8:16], first)
	putSequenceNumber(body[16:24], last)
	binary.LittleEndian.PutUint32(body[24:28], count)
	var flags uint8
	if final {
		flags = 0x02
	}
	return buildSubmessage(subHEARTBEAT, flags, body)
}

// buildGap frames a GAP telling a reader that sequence numbers start through
// end will never be sent, so it stops waiting for them.
func buildGap(readerID, writerID uint32, start, end SequenceNumber) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[0:4], readerID)
	binary.BigEndian.PutUint32(body[4:8], writerID)
	putSequenceNumber(body[8:16], start)
	// gapList: everything from gapStart up to its base is irrelevant, plus the
	// bits set in its bitmap. One contiguous run needs no bitmap.
	putSequenceNumber(body[16:24], end+1)
	binary.LittleEndian.PutUint32(body[24:28], 0)
	return buildSubmessage(subGAP, 0, body)
}

// buildInfoTS frames an INFO_TS giving the source time of the submessages
// that follow it: seconds, then a fraction in units of 2^-32 seconds.
func buildInfoTS(t time.Time) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], uint32(t.Unix()))
	binary.LittleEndian.PutUint32(body[4:8], uint32((uint64(t.Nanosecond())<<32)/uint64(time.Second)))
	return buildSubmessage(subINFO_TS, 0, body)
}

// AcknackSubmessage is a parsed ACKNACK: a reader has everything below Base
// and asks for the sequence numbers in Missing.
type AcknackSubmessage struct {
	ReaderID uint32
	WriterID uint32
	Base     SequenceNumber
	Missing  []SequenceNumber
	Count    uint32
	Final    bool
}

// ParseAcknack decodes an ACKNACK submessage body:
// readerId(4) writerId(4) bitmapBase(8) numBits(4) bitmap(4 per 32 bits) count(4).
func ParseAcknack(s Submessage) (*AcknackSubmessage, error) {
	b, order := s.Body, s.Endian
	if len(b) < 24 {
		return nil, ErrShort
	}
	numBits := order.Uint32(b[16:20])
	if numBits > acknackMaxBits {
		return nil, errors.New("rtps: ACKNACK sequence number set exceeds 256 bits")
	}
	words := int(numBits+31) / 32
	if len(b) < 24+4*words {
		return nil, ErrShort
	}
	hi := int64(int32(order.Uint32(b[8:12])))
	lo := int64(order.Uint32(b[12:16]))
	ack := &AcknackSubmessage{
		ReaderID: binary.BigEndian.Uint32(b[0:4]),
		WriterID: binary.BigEndian.Uint32(b[4:8]),
		Base:     SequenceNumber(hi<<32 | lo),
		Count:    order.Uint32(b[20+4*words : 24+4*words]),
		Final:    s.Flags&0x02 != 0,
	}
	for bit := 0; bit < int(numBits); bit++ {
		// Bit 0 is the most significant bit of the first word.
		if order.Uint32(b[20+4*(bit/32):24+4*(bit/32)])&(1<<(31-bit%32)) != 0 {
			ack.Missing = append(ack.Missing, ack.Base+SequenceNumber(bit))
		}
	}
	return ack, nil
}
