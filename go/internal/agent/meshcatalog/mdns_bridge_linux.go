//go:build linux

package meshcatalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

type mdnsQueryReply struct {
	data    []byte
	source  net.IP
	ifindex int
	err     error
}

// Run owns the app bridge's UDP 5353 socket. SO_BINDTODEVICE and packet
// interface checks prevent an app's announcements from crossing bridge scope.
// It never grants the app control over a system Avahi daemon.
func (b *MDNSBridge) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("missing mDNS bridge context")
	}
	iface, err := net.InterfaceByIndex(b.scope.BridgeIndex)
	if err != nil {
		return err
	}
	control := func(_ string, _ string, c syscall.RawConn) error {
		var inner error
		if err := c.Control(func(fd uintptr) {
			inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if inner == nil {
				inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}
			if inner == nil {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface.Name)
			}
		}); err != nil {
			return err
		}
		return inner
	}
	packet, err := (&net.ListenConfig{Control: control}).ListenPacket(ctx, "udp4", "0.0.0.0:5353")
	if err != nil {
		return fmt.Errorf("listen app bridge mDNS: %w", err)
	}
	defer packet.Close()
	conn := ipv4.NewPacketConn(packet)
	if err := conn.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true); err != nil {
		return err
	}
	if err := conn.JoinGroup(iface, mdnsGroup); err != nil {
		return err
	}
	defer conn.LeaveGroup(iface, mdnsGroup)
	if err := conn.SetMulticastInterface(iface); err != nil {
		return err
	}
	if err := conn.SetMulticastTTL(255); err != nil {
		return err
	}
	// Many app DNS-SD libraries unicast answers to a query's source port.
	// Avahi also owns host UDP 5353, so SO_REUSEPORT can deliver that unicast
	// reply to Avahi instead of this bridge. Give queries a dedicated port;
	// retain the 5353 socket for app multicast and projected announcements.
	queryPacket, err := (&net.ListenConfig{Control: control}).ListenPacket(ctx, "udp4", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen app mDNS query replies: %w", err)
	}
	queryConn := ipv4.NewPacketConn(queryPacket)
	if err := queryConn.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		queryPacket.Close()
		return err
	}
	if err := queryConn.SetMulticastInterface(iface); err != nil {
		queryPacket.Close()
		return err
	}
	if err := queryConn.SetMulticastTTL(255); err != nil {
		queryPacket.Close()
		return err
	}
	replies := make(chan mdnsQueryReply, 128)
	queryDone := make(chan struct{})
	go readMDNSQueryReplies(queryConn, replies, queryDone)
	defer func() { _ = queryPacket.Close(); <-queryDone }()
	defer func() {
		_ = b.withdraw(time.Now())
		b.sendGoodbyes(conn, b.projectionGoodbyes(nil))
	}()
	buffer := make([]byte, 9000)
	nextProbe := time.Time{}
	nextRefresh := time.Time{}
	for {
		if ctx.Err() != nil {
			return nil
		}
		if now := time.Now(); !now.Before(nextProbe) {
			if err := b.sendQuery(queryConn, "_services._dns-sd._udp.local."); err != nil {
				return err
			}
			for _, typ := range b.knownTypes(now) {
				if err := b.sendQuery(queryConn, typ+".local."); err != nil {
					return err
				}
			}
			nextProbe = now.Add(5 * time.Second)
		}
		_ = packet.SetReadDeadline(time.Now().Add(time.Second))
		n, cm, source, err := conn.ReadFrom(buffer)
		if err != nil {
			if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
				return err
			}
		} else if cm != nil && cm.IfIndex == iface.Index {
			udp, ok := source.(*net.UDPAddr)
			if ok && udp.IP.Equal(b.scope.AppIP) {
				var msg dns.Msg
				if n > 0 && msg.Unpack(buffer[:n]) == nil {
					if msg.Response {
						if err := b.handleAppResponse(queryConn, &msg, udp.IP, cm.IfIndex); err != nil {
							return err
						}
					} else if len(msg.Question) > 0 {
						b.observeAppClaims(&msg, udp.IP, cm.IfIndex, time.Now())
						if err := b.respond(conn, &msg, udp); err != nil {
							return err
						}
					}
				}
			}
		}
		for i := 0; i < cap(replies); i++ {
			select {
			case reply := <-replies:
				if reply.err != nil {
					return reply.err
				}
				if reply.ifindex != iface.Index || !reply.source.Equal(b.scope.AppIP) {
					continue
				}
				var msg dns.Msg
				if msg.Unpack(reply.data) == nil && msg.Response {
					if err := b.handleAppResponse(queryConn, &msg, reply.source, reply.ifindex); err != nil {
						return err
					}
				}
			default:
				i = cap(replies)
			}
		}
		if err := b.sweep(time.Now()); err != nil {
			return err
		}
		now := time.Now()
		projected, err := b.projection(now)
		if err != nil {
			return err
		}
		added, gone := b.projectionDelta(projected)
		b.sendGoodbyes(conn, gone)
		if len(projected) == 0 {
			nextRefresh = time.Time{}
			continue
		}
		if !now.Before(nextRefresh) {
			added = projected
			nextRefresh = now.Add(announcementRefreshInterval(projected))
		}
		if err := b.sendAnnouncements(conn, added); err != nil {
			return err
		}
	}
}

func readMDNSQueryReplies(conn *ipv4.PacketConn, replies chan<- mdnsQueryReply, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 9000)
	for {
		n, cm, source, err := conn.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				select {
				case replies <- mdnsQueryReply{err: err}:
				default:
				}
			}
			return
		}
		udp, ok := source.(*net.UDPAddr)
		if !ok || cm == nil {
			continue
		}
		reply := mdnsQueryReply{data: append([]byte(nil), buf[:n]...), source: udp.IP, ifindex: cm.IfIndex}
		select {
		case replies <- reply:
		default:
		}
	}
}

func (b *MDNSBridge) handleAppResponse(queryConn *ipv4.PacketConn, msg *dns.Msg, source net.IP, ifindex int) error {
	for _, typ := range b.learnTypes(msg, time.Now()) {
		if err := b.sendQuery(queryConn, typ+".local."); err != nil {
			return err
		}
	}
	return b.observe(msg, source, ifindex, time.Now())
}

func (b *MDNSBridge) sendAnnouncements(conn *ipv4.PacketConn, records []dns.RR) error {
	packets, err := packMDNSAnnouncements(records)
	if err != nil {
		return err
	}
	for _, data := range packets {
		if _, err := conn.WriteTo(data, &ipv4.ControlMessage{IfIndex: b.scope.BridgeIndex}, mdnsGroup); err != nil {
			return err
		}
	}
	return nil
}

func (b *MDNSBridge) sendQuery(conn *ipv4.PacketConn, name string) error {
	query := new(dns.Msg)
	query.Question = []dns.Question{{Name: name, Qtype: dns.TypePTR, Qclass: dns.ClassINET}}
	data, err := query.Pack()
	if err != nil {
		return err
	}
	_, err = conn.WriteTo(data, &ipv4.ControlMessage{IfIndex: b.scope.BridgeIndex}, mdnsGroup)
	return err
}

func (b *MDNSBridge) respond(conn *ipv4.PacketConn, query *dns.Msg, source *net.UDPAddr) error {
	projected, err := b.projection(time.Now())
	if err != nil {
		return err
	}
	response := AnswerMDNS(query, projected)
	if response == nil {
		return nil
	}
	unicast := source.Port != 5353
	for _, q := range query.Question {
		if q.Qclass&0x8000 != 0 {
			unicast = true
			break
		}
	}
	destination := net.Addr(mdnsGroup)
	if unicast {
		destination = source
	}
	data, err := packMDNSResponse(response, source.Port != 5353)
	if err != nil {
		return err
	}
	_, err = conn.WriteTo(data, &ipv4.ControlMessage{IfIndex: b.scope.BridgeIndex}, destination)
	return err
}

func (b *MDNSBridge) sendGoodbyes(conn *ipv4.PacketConn, gone []dns.RR) {
	packets, err := packMDNSAnnouncements(b.unclaimedGoodbyes(gone, time.Now()))
	if err != nil {
		return
	}
	for _, data := range packets {
		_, _ = conn.WriteTo(data, &ipv4.ControlMessage{IfIndex: b.scope.BridgeIndex}, mdnsGroup)
	}
}
