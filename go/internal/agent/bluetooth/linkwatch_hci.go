package bluetooth

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// HCI packet codes the link watcher reads and writes (Bluetooth Core
// Specification 5.4, Vol 4 Part E §5.4 and §7.7).
const (
	hciCommandPkt = 0x01
	hciEventPkt   = 0x04

	hciEvtConnComplete    = 0x03
	hciEvtDisconnComplete = 0x05
	hciEvtCmdStatus       = 0x0F
	hciEvtLEMeta          = 0x3E

	hciLEConnComplete         = 0x01
	hciLEConnUpdateComplete   = 0x03
	hciLEEnhancedConnComplete = 0x0A

	hciOpLEConnUpdate uint16 = 0x2013

	// Command Status results a colliding connection update produces.
	hciStatusCommandDisallowed = 0x0C
	hciStatusControllerBusy    = 0x3A

	hciReasonConnectionTimeout = 0x08

	// Link types from the kernel's struct hci_conn_info. hciLinkUnknown is
	// ours, for a link the kernel no longer lists.
	hciLinkSCO     = 0x00
	hciLinkACL     = 0x01
	hciLinkESCO    = 0x02
	hciLinkLE      = 0x80
	hciLinkUnknown = 0xFF

	// hciLinkModeCentral is HCI_LM_MASTER in hci_conn_info.link_mode.
	hciLinkModeCentral = 0x0001

	// Connection states in hci_conn_info.state (enum in
	// include/net/bluetooth/bluetooth.h) for a connection still being set
	// up, and the handle such a connection has on newer kernels.
	hciStateConnect  = 5 // BT_CONNECT
	hciStateConnect2 = 6 // BT_CONNECT2
	hciHandleUnset   = 0xFFFF
)

// Sizes of the kernel's HCI socket ioctl structures
// (include/net/bluetooth/hci_sock.h).
const (
	hciConnListHdrSize = 4  // struct hci_conn_list_req without entries
	hciConnInfoSize    = 16 // struct hci_conn_info
	hciDevInfoSize     = 92 // struct hci_dev_info
)

type hciEventKind uint8

const (
	hciConnComplete hciEventKind = iota + 1 // LE
	hciConnUpdateComplete
	hciDisconnComplete
	hciCmdStatus
	hciClassicConnComplete
)

// connParams are LE connection parameters in controller units: Interval in
// 1.25 ms steps, Latency in connection events, Timeout in 10 ms steps.
type connParams struct {
	Interval uint16
	Latency  uint16
	Timeout  uint16
}

// connUpdate is an LE Connection Update request in controller units.
type connUpdate struct {
	IntervalMin uint16
	IntervalMax uint16
	Latency     uint16
	Timeout     uint16
}

// hciEvent is one decoded controller event the link watcher acts on.
type hciEvent struct {
	Kind     hciEventKind
	Status   uint8
	Handle   uint16     // every kind except hciCmdStatus
	Central  bool       // hciConnComplete: the local controller is central
	Params   connParams // hciConnComplete, hciConnUpdateComplete
	Reason   uint8      // hciDisconnComplete
	Opcode   uint16     // hciCmdStatus
	Address  string     // hciClassicConnComplete
	LinkType uint8      // hciClassicConnComplete: hciLinkACL or hciLinkSCO
}

// connInfo is one entry of the kernel's connection list for an adapter.
type connInfo struct {
	Handle   uint16
	Address  string
	LinkType uint8
	Central  bool
}

// errShortHCIPacket reports a packet shorter than its declared or required
// length.
var errShortHCIPacket = errors.New("short HCI packet")

// decodeHCIEvent decodes one packet read from a raw HCI socket, packet type
// byte first. ok is false for well-formed packets the watcher does not use.
func decodeHCIEvent(pkt []byte) (ev hciEvent, ok bool, err error) {
	if len(pkt) < 1 || pkt[0] != hciEventPkt {
		return hciEvent{}, false, nil
	}
	if len(pkt) < 3 || len(pkt) < 3+int(pkt[2]) {
		return hciEvent{}, false, errShortHCIPacket
	}
	p := pkt[3 : 3+int(pkt[2])]
	switch pkt[1] {
	case hciEvtConnComplete:
		// status(1) handle(2) bdaddr(6) link type(1) encryption(1). A
		// Classic address is the peer's real one, so no lookup is needed.
		if len(p) < 10 {
			return hciEvent{}, false, errShortHCIPacket
		}
		return hciEvent{
			Kind: hciClassicConnComplete, Status: p[0], Handle: le16(p[1:]) & 0x0FFF,
			Address: bdaddrString(p[3:9]), LinkType: p[9],
		}, true, nil
	case hciEvtDisconnComplete:
		if len(p) < 4 {
			return hciEvent{}, false, errShortHCIPacket
		}
		return hciEvent{Kind: hciDisconnComplete, Status: p[0], Handle: le16(p[1:]) & 0x0FFF, Reason: p[3]}, true, nil
	case hciEvtCmdStatus:
		if len(p) < 4 {
			return hciEvent{}, false, errShortHCIPacket
		}
		return hciEvent{Kind: hciCmdStatus, Status: p[0], Opcode: le16(p[2:])}, true, nil
	case hciEvtLEMeta:
		if len(p) < 1 {
			return hciEvent{}, false, errShortHCIPacket
		}
		return decodeLEMeta(p[0], p[1:])
	}
	return hciEvent{}, false, nil
}

func decodeLEMeta(sub byte, p []byte) (hciEvent, bool, error) {
	switch sub {
	case hciLEConnComplete, hciLEEnhancedConnComplete:
		// Both begin status(1) handle(2) role(1) peer type(1) peer
		// address(6); the enhanced event then carries two 6-byte private
		// addresses before interval(2) latency(2) timeout(2).
		off := 11
		if sub == hciLEEnhancedConnComplete {
			off = 23
		}
		if len(p) < off+6 {
			return hciEvent{}, false, errShortHCIPacket
		}
		return hciEvent{
			Kind:    hciConnComplete,
			Status:  p[0],
			Handle:  le16(p[1:]) & 0x0FFF,
			Central: p[3] == 0x00,
			Params:  connParams{Interval: le16(p[off:]), Latency: le16(p[off+2:]), Timeout: le16(p[off+4:])},
		}, true, nil
	case hciLEConnUpdateComplete:
		if len(p) < 9 {
			return hciEvent{}, false, errShortHCIPacket
		}
		return hciEvent{
			Kind:   hciConnUpdateComplete,
			Status: p[0],
			Handle: le16(p[1:]) & 0x0FFF,
			Params: connParams{Interval: le16(p[3:]), Latency: le16(p[5:]), Timeout: le16(p[7:])},
		}, true, nil
	}
	return hciEvent{}, false, nil
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }

// encodeLEConnUpdate builds the HCI LE Connection Update command packet,
// packet type byte first, with zero connection-event lengths.
func encodeLEConnUpdate(handle uint16, u connUpdate) []byte {
	pkt := make([]byte, 4+14)
	pkt[0] = hciCommandPkt
	binary.LittleEndian.PutUint16(pkt[1:], hciOpLEConnUpdate)
	pkt[3] = 14
	binary.LittleEndian.PutUint16(pkt[4:], handle)
	binary.LittleEndian.PutUint16(pkt[6:], u.IntervalMin)
	binary.LittleEndian.PutUint16(pkt[8:], u.IntervalMax)
	binary.LittleEndian.PutUint16(pkt[10:], u.Latency)
	binary.LittleEndian.PutUint16(pkt[12:], u.Timeout)
	return pkt
}

// encodeHCIFilter is the HCI_FILTER value for the watcher's raw socket: event
// packets only; Classic Connection Complete, Disconnection Complete, Command
// Status and LE Meta events only; Command Status only for LE Connection Update. The layout is the
// kernel's struct hci_ufilter (u32 type mask, two u32 event masks, le16
// opcode, padded to 16 bytes) in the little-endian byte order of every board
// WendyOS ships on.
func encodeHCIFilter() []byte {
	f := make([]byte, 16)
	binary.LittleEndian.PutUint32(f[0:], 1<<hciEventPkt)
	var events uint64 = 1<<hciEvtConnComplete | 1<<hciEvtDisconnComplete | 1<<hciEvtCmdStatus | 1<<hciEvtLEMeta
	binary.LittleEndian.PutUint32(f[4:], uint32(events))
	binary.LittleEndian.PutUint32(f[8:], uint32(events>>32))
	binary.LittleEndian.PutUint16(f[12:], hciOpLEConnUpdate)
	return f
}

// parseConnList decodes an HCIGETCONNLIST result: u16 device id, u16 entry
// count, then that many struct hci_conn_info (u16 handle, bdaddr_t, u8 type,
// u8 out, u16 state, u32 link_mode), in host byte order. It drops
// connections still being set up, such as BlueZ reconnecting to a trusted
// device that is switched off: they have no handle yet (0xFFFF, or 0 before
// Linux 5.18), and an LE one can carry a resolvable private address.
func parseConnList(buf []byte) ([]connInfo, error) {
	if len(buf) < hciConnListHdrSize {
		return nil, errShortHCIPacket
	}
	n := int(binary.LittleEndian.Uint16(buf[2:]))
	if len(buf) < hciConnListHdrSize+n*hciConnInfoSize {
		return nil, errShortHCIPacket
	}
	conns := make([]connInfo, 0, n)
	for i := range n {
		e := buf[hciConnListHdrSize+i*hciConnInfoSize:]
		handle := binary.LittleEndian.Uint16(e[0:])
		if state := binary.LittleEndian.Uint16(e[10:]); state == hciStateConnect || state == hciStateConnect2 || handle == hciHandleUnset {
			continue
		}
		conns = append(conns, connInfo{
			Handle:   handle,
			Address:  bdaddrString(e[2:8]),
			LinkType: e[8],
			Central:  binary.LittleEndian.Uint32(e[12:])&hciLinkModeCentral != 0,
		})
	}
	return conns, nil
}

// devInfoAddress extracts the adapter address from an HCIGETDEVINFO result
// (struct hci_dev_info: u16 dev_id, char name[8], bdaddr_t bdaddr, ...).
func devInfoAddress(buf []byte) (string, error) {
	if len(buf) < 16 {
		return "", errShortHCIPacket
	}
	return bdaddrString(buf[10:16]), nil
}

// bdaddrString formats a kernel bdaddr_t, least significant byte first, the
// way BlueZ does: "AA:BB:CC:DD:EE:FF".
func bdaddrString(b []byte) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[5], b[4], b[3], b[2], b[1], b[0])
}

// disconnectReasonText names the HCI error codes a disconnect commonly
// carries (Core Specification 5.4, Vol 1 Part F).
func disconnectReasonText(code uint8) string {
	switch code {
	case 0x05:
		return "authentication failure"
	case 0x08:
		return "connection timeout"
	case 0x13:
		return "remote user terminated connection"
	case 0x14:
		return "remote device terminated connection: low resources"
	case 0x15:
		return "remote device terminated connection: power off"
	case 0x16:
		return "connection terminated by local host"
	case 0x1F:
		return "unspecified error"
	case 0x22:
		return "link layer response timeout"
	case 0x28:
		return "instant passed"
	case 0x3B:
		return "unacceptable connection parameters"
	case 0x3D:
		return "connection terminated: MIC failure"
	case 0x3E:
		return "connection failed to be established"
	}
	return "unknown"
}

// linkTypeName labels a kernel link type for logs.
func linkTypeName(t uint8) string {
	switch t {
	case hciLinkLE:
		return "le"
	case hciLinkACL:
		return "classic"
	case hciLinkSCO, hciLinkESCO:
		return "sco"
	case hciLinkUnknown:
		return "unknown"
	}
	return fmt.Sprintf("0x%02x", t)
}
