package ipcam

import (
	"errors"
	"fmt"
	"net"
	"time"
)

// Minimal Dynamic Host Configuration Protocol (DHCP) support, enough to hand an
// address to a camera cabled straight into the device.
//
// This is deliberately a small implementation rather than a dnsmasq dependency in
// the root filesystem: it adds nothing to the image, it is unit-testable against
// canned packets, and the lease table becomes a direct input to the camera
// registry instead of a file to scrape.
//
// Wire format is RFC 2131 section 2: a fixed 236-byte header, a four-byte magic
// cookie, then type-length-value options terminated by 255.
const (
	dhcpHeaderLen = 236
	dhcpMinLen    = dhcpHeaderLen + 4 // header plus magic cookie

	dhcpServerPort = 67
	dhcpClientPort = 68
)

// dhcpMagicCookie identifies the option area (RFC 2131 section 3).
var dhcpMagicCookie = [4]byte{99, 130, 83, 99}

// BOOTP operation codes.
const (
	bootRequest = 1
	bootReply   = 2
)

// htypeEthernet is the RFC 1700 hardware type for 10 megabit Ethernet, which is
// what every camera on a wired link reports.
const (
	htypeEthernet = 1
	hlenEthernet  = 6
)

// MessageType is the DHCP message type carried in option 53.
type MessageType byte

const (
	Discover MessageType = 1
	Offer    MessageType = 2
	Request  MessageType = 3
	Decline  MessageType = 4
	Ack      MessageType = 5
	Nak      MessageType = 6
	Release  MessageType = 7
	Inform   MessageType = 8
)

func (m MessageType) String() string {
	switch m {
	case Discover:
		return "DISCOVER"
	case Offer:
		return "OFFER"
	case Request:
		return "REQUEST"
	case Decline:
		return "DECLINE"
	case Ack:
		return "ACK"
	case Nak:
		return "NAK"
	case Release:
		return "RELEASE"
	case Inform:
		return "INFORM"
	default:
		return fmt.Sprintf("type(%d)", byte(m))
	}
}

// DHCP option codes we read or write.
const (
	optSubnetMask     = 1
	optRouter         = 3
	optHostname       = 12
	optRequestedIP    = 50
	optLeaseTime      = 51
	optMessageType    = 53
	optServerID       = 54
	optParamRequest   = 55
	optClientID       = 61
	optEnd            = 255
	optPad            = 0
	optBroadcastAddr  = 28
	optRenewalTime    = 58
	optRebindingTime  = 59
	optMaxMessageSize = 57
)

// ErrNotDHCP is returned for a datagram that is not a DHCP message. Port 67 sees
// other traffic, so this is an ordinary outcome rather than a failure.
var ErrNotDHCP = errors.New("not a DHCP packet")

// Packet is the subset of a DHCP message this package needs.
type Packet struct {
	Op       byte
	XID      uint32
	Flags    uint16
	CIAddr   net.IP
	YIAddr   net.IP
	SIAddr   net.IP
	GIAddr   net.IP
	CHAddr   net.HardwareAddr
	Type     MessageType
	Hostname string

	// RequestedIP is option 50, the address a client asks to keep across a
	// rebind. ServerID is option 54, which identifies whose offer a REQUEST is
	// accepting.
	RequestedIP net.IP
	ServerID    net.IP
}

// Broadcast reports whether the client asked for broadcast replies, which it does
// while it has no address to receive a unicast on.
func (p *Packet) Broadcast() bool { return p.Flags&0x8000 != 0 }

// ParsePacket decodes a DHCP datagram.
func ParsePacket(data []byte) (*Packet, error) {
	if len(data) < dhcpMinLen {
		return nil, ErrNotDHCP
	}
	if data[dhcpHeaderLen] != dhcpMagicCookie[0] ||
		data[dhcpHeaderLen+1] != dhcpMagicCookie[1] ||
		data[dhcpHeaderLen+2] != dhcpMagicCookie[2] ||
		data[dhcpHeaderLen+3] != dhcpMagicCookie[3] {
		return nil, ErrNotDHCP
	}
	if data[1] != htypeEthernet {
		return nil, fmt.Errorf("%w: hardware type %d", ErrNotDHCP, data[1])
	}
	hlen := int(data[2])
	if hlen != hlenEthernet {
		return nil, fmt.Errorf("%w: hardware address length %d", ErrNotDHCP, hlen)
	}

	p := &Packet{
		Op:     data[0],
		XID:    uint32(data[4])<<24 | uint32(data[5])<<16 | uint32(data[6])<<8 | uint32(data[7]),
		Flags:  uint16(data[10])<<8 | uint16(data[11]),
		CIAddr: net.IP(append([]byte(nil), data[12:16]...)),
		YIAddr: net.IP(append([]byte(nil), data[16:20]...)),
		SIAddr: net.IP(append([]byte(nil), data[20:24]...)),
		GIAddr: net.IP(append([]byte(nil), data[24:28]...)),
		CHAddr: net.HardwareAddr(append([]byte(nil), data[28:28+hlen]...)),
	}

	// Options are length-prefixed, so a truncated or hostile packet must not be
	// able to drive a read past the end of the buffer.
	for i := dhcpMinLen; i < len(data); {
		code := data[i]
		if code == optEnd {
			break
		}
		if code == optPad {
			i++
			continue
		}
		if i+1 >= len(data) {
			break
		}
		length := int(data[i+1])
		if i+2+length > len(data) {
			break
		}
		value := data[i+2 : i+2+length]
		switch code {
		case optMessageType:
			if length == 1 {
				p.Type = MessageType(value[0])
			}
		case optHostname:
			p.Hostname = string(value)
		case optRequestedIP:
			if length == 4 {
				p.RequestedIP = net.IP(append([]byte(nil), value...))
			}
		case optServerID:
			if length == 4 {
				p.ServerID = net.IP(append([]byte(nil), value...))
			}
		}
		i += 2 + length
	}
	if p.Type == 0 {
		return nil, fmt.Errorf("%w: no message type", ErrNotDHCP)
	}
	return p, nil
}

// ReplyConfig is everything a reply needs beyond the request.
type ReplyConfig struct {
	ServerIP  net.IP        // our address on the camera link, also the router
	ClientIP  net.IP        // the address being offered or confirmed
	Mask      net.IPMask    // subnet mask for the link
	Broadcast net.IP        // broadcast address for the link
	Lease     time.Duration // lease time
}

// requestLease validates the REQUEST state before confirming its exact address.
// A zero message type means remain silent (RFC 2131 section 4.3.2).
func requestLease(req *Packet, serverIP net.IP, pool *LeasePool) (MessageType, net.IP, error) {
	selecting := req.ServerID != nil
	if selecting && !req.ServerID.Equal(serverIP) {
		return 0, nil, nil // The client selected another server's offer.
	}
	current := req.CIAddr.To4()
	hasCurrent := current != nil && !current.IsUnspecified()
	requested := req.RequestedIP
	if requested != nil {
		if hasCurrent || requested.To4() == nil || requested.IsUnspecified() {
			return 0, nil, nil
		}
	} else {
		if selecting || !hasCurrent {
			return 0, nil, nil
		}
		requested = current // RENEWING or REBINDING.
	}
	addr, err := pool.confirmLease(req.CHAddr, requested)
	switch {
	case errors.Is(err, errUnknownLease):
		if selecting {
			return Nak, nil, nil // Our offer is no longer reserved.
		}
		return 0, nil, nil // No binding: do not override another server's lease.
	case errors.Is(err, errLeaseMismatch):
		return Nak, nil, nil
	case err != nil:
		return 0, nil, err
	default:
		return Ack, addr, nil
	}
}

// BuildReply encodes an OFFER, ACK, or NAK for the given request.
func BuildReply(req *Packet, kind MessageType, cfg ReplyConfig) []byte {
	buf := make([]byte, dhcpMinLen)
	buf[0] = bootReply
	buf[1] = htypeEthernet
	buf[2] = hlenEthernet
	buf[4] = byte(req.XID >> 24)
	buf[5] = byte(req.XID >> 16)
	buf[6] = byte(req.XID >> 8)
	buf[7] = byte(req.XID)
	// Echo the client's broadcast flag: it cannot receive a unicast reply until
	// it has configured the address we are handing it.
	buf[10] = byte(req.Flags >> 8)
	buf[11] = byte(req.Flags)
	if kind != Nak {
		copy(buf[16:20], cfg.ClientIP.To4())
		copy(buf[20:24], cfg.ServerIP.To4())
	}
	copy(buf[24:28], req.GIAddr.To4())
	copy(buf[28:34], req.CHAddr)
	copy(buf[dhcpHeaderLen:], dhcpMagicCookie[:])

	opts := []byte{optMessageType, 1, byte(kind)}
	opts = append(opts, optServerID, 4)
	opts = append(opts, cfg.ServerIP.To4()...)
	if kind == Nak {
		// A NAK supplies no address, lease, or network configuration.
		buf[10] |= 0x80
		return append(buf, append(opts, optEnd)...)
	}
	opts = append(opts, optLeaseTime, 4)
	opts = append(opts, secondsBE(cfg.Lease)...)
	// Renewal at half and rebinding at seven eighths, per RFC 2131 section 4.4.5.
	opts = append(opts, optRenewalTime, 4)
	opts = append(opts, secondsBE(cfg.Lease/2)...)
	opts = append(opts, optRebindingTime, 4)
	opts = append(opts, secondsBE(cfg.Lease/8*7)...)
	opts = append(opts, optSubnetMask, 4)
	opts = append(opts, cfg.Mask...)
	if cfg.Broadcast != nil {
		opts = append(opts, optBroadcastAddr, 4)
		opts = append(opts, cfg.Broadcast.To4()...)
	}
	// The device is the only route off this link. Cameras that insist on a
	// gateway before finishing configuration need it present.
	opts = append(opts, optRouter, 4)
	opts = append(opts, cfg.ServerIP.To4()...)
	opts = append(opts, optEnd)

	return append(buf, opts...)
}

// secondsBE encodes a duration as four big-endian seconds.
func secondsBE(d time.Duration) []byte {
	s := uint32(d / time.Second)
	return []byte{byte(s >> 24), byte(s >> 16), byte(s >> 8), byte(s)}
}

// BuildDiscover encodes a DISCOVER. Only tests need it, but keeping it beside the
// parser means both sides of the wire format are exercised by the same code.
func BuildDiscover(xid uint32, mac net.HardwareAddr, hostname string) []byte {
	buf := make([]byte, dhcpMinLen)
	buf[0] = bootRequest
	buf[1] = htypeEthernet
	buf[2] = hlenEthernet
	buf[4] = byte(xid >> 24)
	buf[5] = byte(xid >> 16)
	buf[6] = byte(xid >> 8)
	buf[7] = byte(xid)
	buf[10] = 0x80 // broadcast flag
	copy(buf[28:34], mac)
	copy(buf[dhcpHeaderLen:], dhcpMagicCookie[:])

	opts := []byte{optMessageType, 1, byte(Discover)}
	if hostname != "" {
		opts = append(opts, optHostname, byte(len(hostname)))
		opts = append(opts, hostname...)
	}
	opts = append(opts, optParamRequest, 4, optSubnetMask, optRouter, optHostname, optBroadcastAddr)
	opts = append(opts, optEnd)
	return append(buf, opts...)
}
