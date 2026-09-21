// Package rtps is a minimal, read-oriented RTPS 2.x client: enough to discover
// a writer on a DDS domain and receive its samples, and nothing more.
//
// It is not a DDS implementation. There is no reliable-reader state machine
// (a BEST_EFFORT reader matches a RELIABLE writer under the RxO rule, which is
// what ROS 2's default QoS offers), bounded DATA_FRAG reassembly, no security,
// no content filtering, and discovery endpoint disposal only.
package rtps

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ErrShort reports a datagram that ended mid-structure.
var ErrShort = errors.New("rtps: message too short")

// protocolID is the four bytes every RTPS datagram opens with.
var protocolID = [4]byte{'R', 'T', 'P', 'S'}

// Submessage kinds, from the RTPS spec. Only the ones this client reads or
// writes are named.
const (
	subACKNACK   = 0x06
	subHEARTBEAT = 0x07
	subINFO_TS   = 0x09
	subINFO_SRC  = 0x0c
	subINFO_DST  = 0x0e
	subDATA      = 0x15
	subDATAFRAG  = 0x16
)

// Builtin entity IDs. The suffix encodes the entity kind, so these constants
// are the spec's values verbatim rather than anything derived.
const (
	entityParticipant       = 0x000001c1
	entitySPDPWriter        = 0x000100c2
	entitySPDPReader        = 0x000100c7
	entitySEDPPubWriter     = 0x000003c2
	entitySEDPPubReader     = 0x000003c7
	entitySEDPSubWriter     = 0x000004c2
	entitySEDPSubReader     = 0x000004c7
	entityUnknown           = 0x00000000
	entityUserReaderNoKey   = 0x00000004 // kind byte for a user reader, no key
	entityAppReaderIDPrefix = 0x00000100 // arbitrary app-assigned entity key
)

// Parameter IDs used in the PL_CDR parameter lists that carry SPDP and SEDP.
const (
	pidSentinel                  = 0x0001
	pidParticipantLeaseDuration  = 0x0002
	pidTopicName                 = 0x0005
	pidTypeName                  = 0x0007
	pidProtocolVersion           = 0x0015
	pidVendorID                  = 0x0016
	pidReliability               = 0x001a
	pidDurability                = 0x001d
	pidUnicastLocator            = 0x002f
	pidMulticastLocator          = 0x0030
	pidDefaultUnicastLocator     = 0x0031
	pidMetatrafficUnicastLocator = 0x0032
	pidParticipantGUID           = 0x0050
	pidBuiltinEndpointSet        = 0x0058
	pidEndpointGUID              = 0x005a
	pidKeyHash                   = 0x0070
	pidStatusInfo                = 0x0071
)

// Builtin endpoint set bits advertised in SPDP: what this participant can do.
// We announce ourselves, detect other participants, detect their publications,
// and announce our own subscriptions. We deliberately do not claim to be a
// publication announcer or subscription detector — we publish no user data and
// do not care who else subscribes.
const builtinEndpoints = 0x01 | 0x02 | 0x08 | 0x10

// reliabilityBestEffort / reliabilityReliable are the QoS kind values as they
// appear on the wire. Note these are 1-based, unlike most DDS enums.
const (
	reliabilityBestEffort = 1
	reliabilityReliable   = 2
)

// locatorKindUDPv4 identifies an IPv4 locator; its address field is 12 zero
// bytes followed by the 4 address bytes.
const locatorKindUDPv4 = 1

// GUIDPrefix identifies a participant. Endpoint GUIDs are this plus a 4-byte
// entity ID.
type GUIDPrefix [12]byte

// GUID is a fully qualified endpoint identity.
type GUID struct {
	Prefix   GUIDPrefix
	EntityID uint32
}

func (g GUID) String() string {
	return fmt.Sprintf("%x.%08x", g.Prefix, g.EntityID)
}

// Locator is a transport address a remote endpoint can be reached at.
type Locator struct {
	Kind int32
	Port uint32
	Addr [16]byte
}

// UDPAddr renders an IPv4 locator as a dialable address, reporting ok only for
// a usable UDPv4 locator with a non-zero port.
func (l Locator) UDPAddr() (*net.UDPAddr, bool) {
	if l.Kind != locatorKindUDPv4 || l.Port == 0 || l.Port > 65535 {
		return nil, false
	}
	ip := net.IPv4(l.Addr[12], l.Addr[13], l.Addr[14], l.Addr[15])
	if ip.IsUnspecified() {
		return nil, false
	}
	return &net.UDPAddr{IP: ip, Port: int(l.Port)}, true
}

// udpv4Locator builds a UDPv4 locator for an address and port.
func udpv4Locator(ip net.IP, port int) Locator {
	l := Locator{Kind: locatorKindUDPv4, Port: uint32(port)}
	if v4 := ip.To4(); v4 != nil {
		copy(l.Addr[12:], v4)
	}
	return l
}

// SequenceNumber is an RTPS sequence number: a signed 64-bit value transmitted
// as a high int32 and a low uint32.
type SequenceNumber int64

// Submessage is one parsed submessage: its kind, flags, and raw body.
type Submessage struct {
	Kind   uint8
	Flags  uint8
	Body   []byte
	Endian binary.ByteOrder
}

// littleEndianFlag is bit 0 of every submessage's flags octet.
func endianOf(flags uint8) binary.ByteOrder {
	if flags&0x01 != 0 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

// Message is a parsed RTPS datagram.
type Message struct {
	Prefix      GUIDPrefix
	Submessages []Submessage
}

// ParseMessage splits a datagram into submessages. It stops at the first
// malformed submessage rather than erroring, so a datagram carrying one
// submessage this client does not understand still yields the ones before it.
func ParseMessage(buf []byte) (*Message, error) {
	if len(buf) < 20 {
		return nil, ErrShort
	}
	if buf[0] != protocolID[0] || buf[1] != protocolID[1] ||
		buf[2] != protocolID[2] || buf[3] != protocolID[3] {
		return nil, errors.New("rtps: not an RTPS datagram")
	}
	m := &Message{}
	copy(m.Prefix[:], buf[8:20])

	pos := 20
	for pos+4 <= len(buf) {
		kind := buf[pos]
		flags := buf[pos+1]
		order := endianOf(flags)
		length := int(order.Uint16(buf[pos+2 : pos+4]))
		pos += 4

		// A zero length on the last submessage means "to the end of the
		// datagram" (RTPS 8.3.3.2.3).
		if length == 0 {
			length = len(buf) - pos
		}
		if pos+length > len(buf) {
			break
		}
		m.Submessages = append(m.Submessages, Submessage{
			Kind:   kind,
			Flags:  flags,
			Body:   buf[pos : pos+length],
			Endian: order,
		})
		pos += length
	}
	return m, nil
}

// DataSubmessage is a parsed DATA submessage.
type DataSubmessage struct {
	ReaderID  uint32
	WriterID  uint32
	WriterSN  SequenceNumber
	InlineQoS []byte
	Endian    binary.ByteOrder
	Payload   []byte // serialized payload, including its encapsulation header
}

// DataFragSubmessage is a parsed DATA_FRAG submessage. Fragment numbers are
// one-based, as they are on the RTPS wire. Payload contains the serialized
// bytes carried by the consecutive fragments in this submessage.
type DataFragSubmessage struct {
	ReaderID              uint32
	WriterID              uint32
	WriterSN              SequenceNumber
	FragmentStartingNum   uint32
	FragmentsInSubmessage uint16
	FragmentSize          uint16
	SampleSize            uint32
	InlineQoS             []byte
	Payload               []byte
}

// ParseData decodes a DATA submessage body.
//
// Layout: extraFlags(2) octetsToInlineQos(2) readerId(4) writerId(4)
// writerSN(8), then optionally an inline QoS parameter list, then the
// serialized payload. octetsToInlineQos is measured from just after that field
// itself, which is what lets a reader skip vendor extensions it does not know.
func ParseData(s Submessage) (*DataSubmessage, error) {
	b, order := s.Body, s.Endian
	if len(b) < 20 {
		return nil, ErrShort
	}
	octetsToInlineQos := int(order.Uint16(b[2:4]))

	// EntityId_t is NOT byte-order swapped. It is four octets — a 3-byte entity
	// key followed by a 1-byte entity kind — so it reads big-endian regardless
	// of the submessage's endianness flag. Reading it with the submessage order
	// silently turns 0x000100c2 into 0xc2000100 on a little-endian submessage,
	// which matches no builtin writer and makes every SPDP announcement look
	// like unaddressed user data.
	d := &DataSubmessage{
		Endian:   order,
		ReaderID: binary.BigEndian.Uint32(b[4:8]),
		WriterID: binary.BigEndian.Uint32(b[8:12]),
	}
	hi := int64(int32(order.Uint32(b[12:16])))
	lo := int64(order.Uint32(b[16:20]))
	d.WriterSN = SequenceNumber(hi<<32 | lo)

	// Skip from the end of octetsToInlineQos (offset 4) by the advertised
	// count. For a stock DATA that lands exactly at offset 24.
	pos := 4 + octetsToInlineQos
	if pos < 20 || pos > len(b) {
		return nil, fmt.Errorf("rtps: octetsToInlineQos %d out of range", octetsToInlineQos)
	}

	hasInlineQoS := s.Flags&0x02 != 0
	hasData := s.Flags&0x04 != 0
	hasKey := s.Flags&0x08 != 0

	if hasInlineQoS {
		n, err := parameterListLength(b[pos:], order)
		if err != nil {
			return nil, fmt.Errorf("rtps: inline QoS: %w", err)
		}
		d.InlineQoS = b[pos : pos+n]
		pos += n
	}
	if hasData || hasKey {
		d.Payload = b[pos:]
	}
	return d, nil
}

// ParseDataFrag decodes a DATA_FRAG submessage body (RTPS 2.3, 8.3.7.3.4).
// octetsToInlineQos has the same origin as DATA: immediately after the field
// itself, so a stock header's value 28 lands at byte offset 32.
func ParseDataFrag(s Submessage) (*DataFragSubmessage, error) {
	b, order := s.Body, s.Endian
	if len(b) < 32 {
		return nil, ErrShort
	}
	octetsToInlineQos := int(order.Uint16(b[2:4]))
	f := &DataFragSubmessage{
		ReaderID:              binary.BigEndian.Uint32(b[4:8]),
		WriterID:              binary.BigEndian.Uint32(b[8:12]),
		FragmentStartingNum:   order.Uint32(b[20:24]),
		FragmentsInSubmessage: order.Uint16(b[24:26]),
		FragmentSize:          order.Uint16(b[26:28]),
		SampleSize:            order.Uint32(b[28:32]),
	}
	hi := int64(int32(order.Uint32(b[12:16])))
	lo := int64(order.Uint32(b[16:20]))
	f.WriterSN = SequenceNumber(hi<<32 | lo)

	if f.FragmentStartingNum == 0 || f.FragmentsInSubmessage == 0 || f.FragmentSize == 0 || f.SampleSize == 0 {
		return nil, errors.New("rtps: invalid DATA_FRAG dimensions")
	}
	pos := 4 + octetsToInlineQos
	if pos < 32 || pos > len(b) {
		return nil, fmt.Errorf("rtps: DATA_FRAG octetsToInlineQos %d out of range", octetsToInlineQos)
	}
	if s.Flags&0x02 != 0 {
		n, err := parameterListLength(b[pos:], order)
		if err != nil {
			return nil, fmt.Errorf("rtps: DATA_FRAG inline QoS: %w", err)
		}
		f.InlineQoS = b[pos : pos+n]
		pos += n
	}
	f.Payload = b[pos:]
	return f, nil
}

// buildMessage assembles a datagram: the RTPS header followed by submessages.
func buildMessage(prefix GUIDPrefix, subs ...[]byte) []byte {
	out := make([]byte, 0, 20+len(subs)*64)
	out = append(out, protocolID[0], protocolID[1], protocolID[2], protocolID[3])
	out = append(out, 2, 2) // protocol version 2.2
	out = append(out, 0x01, 0x0f)
	out = append(out, prefix[:]...)
	for _, s := range subs {
		out = append(out, s...)
	}
	return out
}

// buildSubmessage frames a submessage body with its header. Everything this
// client writes is little-endian, so the endianness flag is always set.
func buildSubmessage(kind uint8, extraFlags uint8, body []byte) []byte {
	out := make([]byte, 4, 4+len(body))
	out[0] = kind
	out[1] = 0x01 | extraFlags
	binary.LittleEndian.PutUint16(out[2:4], uint16(len(body)))
	return append(out, body...)
}

// HeartbeatSubmessage is a parsed HEARTBEAT.
type HeartbeatSubmessage struct {
	ReaderID uint32
	WriterID uint32
	FirstSN  SequenceNumber
	LastSN   SequenceNumber
	Count    uint32
}

// ParseHeartbeat decodes a HEARTBEAT submessage body:
// readerId(4) writerId(4) firstSN(8) lastSN(8) count(4).
func ParseHeartbeat(s Submessage) (*HeartbeatSubmessage, error) {
	b, order := s.Body, s.Endian
	if len(b) < 28 {
		return nil, ErrShort
	}
	sn := func(off int) SequenceNumber {
		hi := int64(int32(order.Uint32(b[off : off+4])))
		lo := int64(order.Uint32(b[off+4 : off+8]))
		return SequenceNumber(hi<<32 | lo)
	}
	return &HeartbeatSubmessage{
		ReaderID: binary.BigEndian.Uint32(b[0:4]),
		WriterID: binary.BigEndian.Uint32(b[4:8]),
		FirstSN:  sn(8),
		LastSN:   sn(16),
		Count:    order.Uint32(b[24:28]),
	}, nil
}

// acknackMaxBits caps how many sequence numbers one ACKNACK can request. The
// RTPS SequenceNumberSet is limited to 256 bits.
const acknackMaxBits = 256

// buildAcknack frames an ACKNACK requesting everything from base through last.
//
// SEDP builtin endpoints are RELIABLE and TRANSIENT_LOCAL, so a writer
// announces its history to a late-joining reader with a HEARTBEAT and only
// replays the DATA once the reader asks. Without this, discovery receives
// heartbeats indefinitely and never a single publication.
func buildAcknack(readerID, writerID uint32, base, last SequenceNumber, count uint32) []byte {
	if base < 1 {
		base = 1
	}
	n := int(last - base + 1)
	if n < 0 {
		n = 0
	}
	if n > acknackMaxBits {
		n = acknackMaxBits
	}

	words := (n + 31) / 32
	body := make([]byte, 0, 8+8+4+words*4+4)

	idb := make([]byte, 8)
	binary.BigEndian.PutUint32(idb[0:4], readerID)
	binary.BigEndian.PutUint32(idb[4:8], writerID)
	body = append(body, idb...)

	// readerSNState: bitmapBase, numBits, bitmap. Every bit set means "I have
	// none of these, send them all".
	snb := make([]byte, 12)
	binary.LittleEndian.PutUint32(snb[0:4], uint32(int32(base>>32)))
	binary.LittleEndian.PutUint32(snb[4:8], uint32(base))
	binary.LittleEndian.PutUint32(snb[8:12], uint32(n))
	body = append(body, snb...)

	for w := 0; w < words; w++ {
		var bits uint32
		for bit := 0; bit < 32; bit++ {
			if w*32+bit < n {
				// Bit 0 is the most significant bit of the word.
				bits |= 1 << (31 - bit)
			}
		}
		tmp := make([]byte, 4)
		binary.LittleEndian.PutUint32(tmp, bits)
		body = append(body, tmp...)
	}

	cnt := make([]byte, 4)
	binary.LittleEndian.PutUint32(cnt, count)
	body = append(body, cnt...)

	// Flags: endianness only. Leaving the Final bit clear asks the writer to
	// respond rather than stay silent.
	return buildSubmessage(subACKNACK, 0x00, body)
}

// buildInfoDst frames an INFO_DST naming the participant a following
// submessage is addressed to. A receiver otherwise has to assume the
// destination is itself; several implementations decline to assume and drop
// the submessage instead, so an ACKNACK without INFO_DST can be silently
// ignored.
func buildInfoDst(prefix GUIDPrefix) []byte {
	return buildSubmessage(subINFO_DST, 0x00, prefix[:])
}

// buildData frames a DATA submessage carrying a serialized payload.
func buildData(readerID, writerID uint32, sn SequenceNumber, payload []byte) []byte {
	body := make([]byte, 24, 24+len(payload))
	binary.LittleEndian.PutUint16(body[0:2], 0)  // extraFlags
	binary.LittleEndian.PutUint16(body[2:4], 16) // octetsToInlineQos
	// EntityId_t is octets, not an integer: always big-endian on the wire.
	binary.BigEndian.PutUint32(body[4:8], readerID)
	binary.BigEndian.PutUint32(body[8:12], writerID)
	binary.LittleEndian.PutUint32(body[12:16], uint32(int32(sn>>32)))
	binary.LittleEndian.PutUint32(body[16:20], uint32(sn))
	body = body[:20]
	body = append(body, payload...)
	// 0x04 is the D (data present) flag.
	return buildSubmessage(subDATA, 0x04, body)
}

// discoveryDisposal accepts both inline key hashes and serialized discovery keys.
// StatusInfo is four octets, with DISPOSED/UNREGISTERED in the last octet.
func discoveryDisposal(d *DataSubmessage) (bool, GUID) {
	order := d.Endian
	if order == nil {
		order = binary.LittleEndian
	}
	inline := []byte{0, 3, 0, 0}
	if order == binary.BigEndian {
		inline[1] = 2
	}
	inline = append(inline, d.InlineQoS...)
	params, _, _ := parseParameterList(inline)
	payload, _, _ := parseParameterList(d.Payload)
	var disposed bool
	var guid GUID
	for _, p := range append(params, payload...) {
		switch p.id {
		case pidStatusInfo:
			disposed = disposed || (len(p.value) >= 4 && p.value[3]&3 != 0)
		case pidKeyHash, pidEndpointGUID, pidParticipantGUID:
			if g, ok := paramGUID(p.value, order); ok {
				guid = g
			}
		}
	}
	return disposed, guid
}

func buildDispose(writer uint32, sn SequenceNumber, guid GUID) []byte {
	qos := newPLBuilder()
	qos.addGUID(pidKeyHash, guid)
	qos.add(pidStatusInfo, []byte{0, 0, 0, 3})
	data := buildData(entityUnknown, writer, sn, nil)
	body := append(data[4:], qos.finish()[4:]...)
	return buildSubmessage(subDATA, 0x02, body)
}
