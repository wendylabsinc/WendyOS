package services

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// GStreamer Data Protocol 1.0, as gdpdepay reads it and gdppay writes it: a
// 62-byte big-endian header followed by the payload.
//
//	offset  bytes  field
//	0       2      version, 1.0
//	2       1      flags: 0x01 header checksum present, 0x02 payload checksum present
//	4       2      packet type: 1 buffer, 2 caps, 64 + GstEventType for an event
//	6       4      payload length
//	10      8      presentation timestamp, ns
//	18      8      duration, ns
//	26      8      offset
//	34      8      offset end
//	42      2      buffer flags
//	44      8      decode timestamp, ns
//	58      2      checksum of bytes 0 to 57
//	60      2      checksum of the payload
//
// gdpdepay accepts flags 0 with both checksums zero. It needs stream-start,
// caps and segment packets before the first buffer.
const (
	gdpHeaderBytes = 62
	gdpNoValue     = ^uint64(0)

	gdpPacketBuffer uint16 = 1
	gdpPacketCaps   uint16 = 2
	// A GstEventType is (number << 8) | flags. Stream-start is number 40 and
	// segment 70; both are downstream, serialized and sticky (0x0e).
	gdpPacketStreamStart uint16 = 64 + (40<<8 | 0x0e)
	gdpPacketSegment     uint16 = 64 + (70<<8 | 0x0e)

	gdpHeaderHasChecksum = 0x01
	gdpMaxEventBytes     = 64 * 1024

	gdpStreamStartEvent = "GstEventStreamStart, stream-id=(string)wendy-derived, flags=(GstStreamFlags)GST_STREAM_FLAG_NONE, group-id=(uint)1;"
	gdpSegmentFromZero  = `GstEventSegment, segment=(GstSegment)"segment, flags=(GstSegmentFlags)GST_SEGMENT_FLAG_NONE, rate=(double)1, applied-rate=(double)1, format=(GstFormat)time, base=(guint64)0, offset=(guint64)0, start=(guint64)0, stop=(guint64)18446744073709551615, time=(guint64)0, position=(guint64)0, duration=(guint64)18446744073709551615;";`
)

var (
	errGDPNotAPacket      = errors.New("not a GStreamer Data Protocol packet")
	errGDPPayloadTooLarge = errors.New("GStreamer Data Protocol payload exceeds the limit")
)

type gdpWriter struct {
	w      io.Writer
	header [gdpHeaderBytes]byte
}

func (g *gdpWriter) packet(kind uint16, timestamp uint64, payload []byte) error {
	h := g.header[:]
	clear(h)
	h[0] = 1
	binary.BigEndian.PutUint16(h[4:], kind)
	binary.BigEndian.PutUint32(h[6:], uint32(len(payload)))
	binary.BigEndian.PutUint64(h[10:], timestamp)
	if kind == gdpPacketBuffer {
		binary.BigEndian.PutUint64(h[18:], gdpNoValue)
		binary.BigEndian.PutUint64(h[26:], gdpNoValue)
		binary.BigEndian.PutUint64(h[34:], gdpNoValue)
		binary.BigEndian.PutUint64(h[44:], timestamp)
	}
	if _, err := g.w.Write(h); err != nil {
		return err
	}
	_, err := g.w.Write(payload)
	return err
}

func gdpText(s string) []byte { return append([]byte(s), 0) }

func (g *gdpWriter) streamHeader(caps string) error {
	if err := g.packet(gdpPacketStreamStart, gdpNoValue, gdpText(gdpStreamStartEvent)); err != nil {
		return err
	}
	if err := g.packet(gdpPacketCaps, 0, gdpText(caps)); err != nil {
		return err
	}
	return g.packet(gdpPacketSegment, gdpNoValue, gdpText(gdpSegmentFromZero))
}

func (g *gdpWriter) buffer(timestamp uint64, payload []byte) error {
	return g.packet(gdpPacketBuffer, timestamp, payload)
}

type gdpPacket struct {
	kind      uint16
	timestamp uint64
	payload   []byte
}

type gdpReader struct {
	r            io.Reader
	maxPayload   int // of a buffer; caps and events are bounded by gdpMaxEventBytes
	segmentStart uint64
	header       [gdpHeaderBytes]byte
	unread       int64
}

// nextHeader returns io.EOF when the stream ended between packets and
// io.ErrUnexpectedEOF when it ended inside one. The packet's payload is then
// taken with payload; whatever is left of it is passed over by the next call.
func (g *gdpReader) nextHeader() (packet gdpPacket, length int64, err error) {
	if err := g.skipPayload(); err != nil {
		return gdpPacket{}, 0, err
	}
	h := g.header[:]
	if _, err := io.ReadFull(g.r, h); err != nil {
		return gdpPacket{}, 0, err
	}
	if h[0] != 1 || h[1] != 0 {
		return gdpPacket{}, 0, fmt.Errorf("%w: version %d.%d", errGDPNotAPacket, h[0], h[1])
	}
	if h[2]&gdpHeaderHasChecksum != 0 && binary.BigEndian.Uint16(h[58:]) != gdpChecksum(h[:58]) {
		return gdpPacket{}, 0, fmt.Errorf("%w: header checksum mismatch", errGDPNotAPacket)
	}
	g.unread = int64(binary.BigEndian.Uint32(h[6:]))
	return gdpPacket{kind: binary.BigEndian.Uint16(h[4:]), timestamp: binary.BigEndian.Uint64(h[10:])}, g.unread, nil
}

func (g *gdpReader) payload() ([]byte, error) {
	payload := make([]byte, g.unread)
	g.unread = 0
	if _, err := io.ReadFull(g.r, payload); err != nil {
		return nil, endedInsidePacket(err)
	}
	return payload, nil
}

func (g *gdpReader) skipPayload() error {
	unread := g.unread
	g.unread = 0
	if _, err := io.CopyN(io.Discard, g.r, unread); err != nil {
		return endedInsidePacket(err)
	}
	return nil
}

// next reads one whole packet. A payload over its bound is passed over and
// reported as errGDPPayloadTooLarge; the next call returns the packet after it.
func (g *gdpReader) next() (gdpPacket, error) {
	packet, length, err := g.nextHeader()
	if err != nil {
		return gdpPacket{}, err
	}
	limit := gdpMaxEventBytes
	if packet.kind == gdpPacketBuffer {
		limit = g.maxPayload
	}
	if length > int64(limit) {
		if err := g.skipPayload(); err != nil {
			return gdpPacket{}, err
		}
		return packet, fmt.Errorf("%w: %d bytes, limit %d", errGDPPayloadTooLarge, length, limit)
	}
	if packet.payload, err = g.payload(); err != nil {
		return gdpPacket{}, err
	}
	if packet.kind == gdpPacketSegment {
		if start, ok := gdpSegmentStart(packet.payload); ok {
			g.segmentStart = start
		}
	}
	return packet, nil
}

func endedInsidePacket(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (g *gdpReader) nextBufferHeader() (timestamp uint64, length int64, err error) {
	for {
		packet, length, err := g.nextHeader()
		if err != nil {
			return 0, 0, err
		}
		switch {
		case packet.kind == gdpPacketBuffer && length > int64(g.maxPayload):
			if err := g.skipPayload(); err != nil {
				return 0, 0, err
			}
			return packet.timestamp, length, fmt.Errorf("%w: %d bytes, limit %d", errGDPPayloadTooLarge, length, g.maxPayload)
		case packet.kind == gdpPacketBuffer:
			return packet.timestamp, length, nil
		case packet.kind == gdpPacketSegment && length <= gdpMaxEventBytes:
			event, err := g.payload()
			if err != nil {
				return 0, 0, err
			}
			if start, ok := gdpSegmentStart(event); ok {
				g.segmentStart = start
			}
		}
	}
}

func (g *gdpReader) nextBuffer() (timestamp uint64, payload []byte, err error) {
	timestamp, _, err = g.nextBufferHeader()
	if err != nil {
		return 0, nil, err
	}
	payload, err = g.payload()
	return timestamp, payload, err
}

func gdpSegmentStart(segmentEvent []byte) (uint64, bool) {
	const key = " start=(guint64)"
	at := bytes.Index(segmentEvent, []byte(key))
	if at < 0 {
		return 0, false
	}
	digits := segmentEvent[at+len(key):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	start, err := strconv.ParseUint(string(digits[:end]), 10, 64)
	return start, err == nil
}

// gdpChecksum is gst_dp_crc: CRC-16 with polynomial 0x1021 from 0xFFFF,
// reported as 0xFFFF minus the register.
func gdpChecksum(b []byte) uint16 {
	register := uint16(0xFFFF)
	for _, x := range b {
		register ^= uint16(x) << 8
		for bit := 0; bit < 8; bit++ {
			if register&0x8000 != 0 {
				register = register<<1 ^ 0x1021
			} else {
				register <<= 1
			}
		}
	}
	return 0xFFFF - register
}
