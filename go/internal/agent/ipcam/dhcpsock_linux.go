//go:build linux

package ipcam

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The socket layer for the camera link, Linux only. Everything here is bound to a
// single interface with SO_BINDTODEVICE: a DHCP server that answered on the wrong
// link would be exactly the disruption the guard exists to prevent, so the binding
// is enforced by the kernel rather than by our own filtering.

// bindUDPToLink opens a UDP socket on port 67 bound to one interface.
func bindUDPToLink(link string) (*net.UDPConn, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return nil, fmt.Errorf("opening dhcp socket: %w", err)
	}
	// On any error past this point the descriptor is closed here, because it has
	// not yet been handed to os.NewFile.
	closeOnError := func(err error) (*net.UDPConn, error) {
		unix.Close(fd) //nolint:errcheck
		return nil, err
	}

	// SO_BINDTODEVICE is the guarantee that this server can never answer on the
	// uplink, whatever else goes wrong.
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, link); err != nil {
		return closeOnError(fmt.Errorf("binding dhcp socket to %s: %w", link, err))
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return closeOnError(fmt.Errorf("setting reuseaddr: %w", err))
	}
	// Replies go to 255.255.255.255 because the client has no address yet.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
		return closeOnError(fmt.Errorf("enabling broadcast: %w", err))
	}
	if err := unix.Bind(fd, &unix.SockaddrInet4{Port: dhcpServerPort}); err != nil {
		return closeOnError(fmt.Errorf("binding port %d: %w", dhcpServerPort, err))
	}

	file := os.NewFile(uintptr(fd), "dhcp-"+link)
	defer file.Close() //nolint:errcheck — FileConn dups the descriptor
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, fmt.Errorf("wrapping dhcp socket: %w", err)
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("dhcp socket is not UDP")
	}
	return udp, nil
}

// watchDHCP observes DHCP traffic on a link without answering any of it. This is
// what feeds the guard while a link is still being judged.
//
// It has to be a packet socket rather than a UDP one, for two reasons that both
// only show up on real hardware:
//
//   - The link has no IPv4 address yet, and that is the whole point: the kernel
//     will not deliver IPv4 datagrams to a socket on an interface with no
//     address, so a UDP listener sees nothing at all.
//   - A competing server's OFFER is addressed to the client port, 68. A socket
//     bound to the server port would never see it, which would defeat the guard
//     that exists to detect exactly that.
//
// AF_PACKET with SOCK_DGRAM strips the ethernet header, so parsing starts at the
// IP header.
func watchDHCP(ctx context.Context, link string, onPacket func(*Packet)) error {
	iface, err := net.InterfaceByName(link)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", link, err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return fmt.Errorf("opening packet socket for %s: %w", link, err)
	}
	// Ignore replies by kernel packet direction, not by the DHCP server IP:
	// another host may use the same camera subnet and server address as us.
	if err := unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1); err != nil {
		unix.Close(fd) //nolint:errcheck
		return fmt.Errorf("filtering outgoing DHCP packets on %s: %w", link, err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  iface.Index,
	}); err != nil {
		unix.Close(fd) //nolint:errcheck
		return fmt.Errorf("binding packet socket to %s: %w", link, err)
	}

	file := os.NewFile(uintptr(fd), "dhcpwatch-"+link)
	defer file.Close() //nolint:errcheck

	go func() {
		<-ctx.Done()
		file.Close() //nolint:errcheck
	}()

	buf := make([]byte, 1500)
	for {
		n, err := file.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading dhcp traffic on %s: %w", link, err)
		}
		payload, ok := udpPayload(buf[:n])
		if !ok {
			continue
		}
		p, err := ParsePacket(payload)
		if err != nil {
			continue
		}
		onPacket(p)
	}
}

// htons converts a port or protocol number to network byte order, which
// AF_PACKET requires. Passing host order silently matches nothing.
func htons(v uint16) uint16 {
	return v<<8 | v>>8
}

// udpPayload extracts the DHCP payload from a raw IPv4 datagram, reporting
// whether it was DHCP traffic at all. Bounds are checked at every step: this
// reads packets from an untrusted link.
func udpPayload(frame []byte) ([]byte, bool) {
	const (
		minIPHeader  = 20
		protocolUDP  = 17
		udpHeaderLen = 8
	)
	if len(frame) < minIPHeader {
		return nil, false
	}
	if frame[0]>>4 != 4 {
		return nil, false
	}
	ihl := int(frame[0]&0x0f) * 4
	if ihl < minIPHeader || len(frame) < ihl+udpHeaderLen {
		return nil, false
	}
	if frame[9] != protocolUDP {
		return nil, false
	}
	udp := frame[ihl:]
	srcPort := uint16(udp[0])<<8 | uint16(udp[1])
	dstPort := uint16(udp[2])<<8 | uint16(udp[3])
	if !isDHCPPort(srcPort) || !isDHCPPort(dstPort) {
		return nil, false
	}
	return udp[udpHeaderLen:], true
}

func isDHCPPort(p uint16) bool {
	return p == dhcpServerPort || p == dhcpClientPort
}

// serveDHCP answers DISCOVER and REQUEST on a claimed link.
func serveDHCP(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error {
	if ctx.Err() != nil {
		return nil
	}
	conn, err := bindUDPToLink(link)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck

	go func() {
		<-ctx.Done()
		conn.Close() //nolint:errcheck
	}()

	broadcastTo := &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpClientPort}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading dhcp request on %s: %w", link, err)
		}
		req, err := ParsePacket(buf[:n])
		if err != nil {
			continue
		}
		// Only client requests get answered; our own replies come back to us
		// because the socket shares the port with the watcher.
		if req.Op != bootRequest {
			continue
		}

		var kind MessageType
		var addr net.IP
		switch req.Type {
		case Discover:
			kind = Offer
			addr, err = pool.Lease(req.CHAddr, req.RequestedIP)
		case Request:
			kind, addr, err = requestLease(req, seg.ServerIP, pool)
		case Release, Decline:
			if err := endClientLease(req, seg.ServerIP, pool); err != nil {
				return err
			}
			continue
		default:
			continue
		}

		if errors.Is(err, ErrPoolExhausted) {
			continue // Keep serving existing leases until a reservation expires.
		}
		if err != nil {
			return fmt.Errorf("leasing an address on %s for %s: %w", link, req.CHAddr, err)
		}
		if kind == 0 {
			continue
		}
		if ctx.Err() != nil {
			return nil // A competing server may have appeared during the write.
		}
		var remaining time.Duration
		if kind != Nak {
			remaining = pool.remaining(req.CHAddr)
			if remaining <= 0 {
				continue
			}
		}
		reply := BuildReply(req, kind, ReplyConfig{
			ServerIP:  seg.ServerIP,
			ClientIP:  addr,
			Mask:      seg.Mask,
			Broadcast: seg.Broadcast,
			Lease:     remaining,
		})
		if err := conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return fmt.Errorf("setting write deadline: %w", err)
		}
		if _, err := conn.WriteToUDP(reply, broadcastTo); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("sending %s on %s: %w", kind, link, err)
		}
		if kind == Ack && onLease != nil {
			// The lease is only real once acknowledged.
			onLease(req.CHAddr, addr, req.Hostname)
		}
	}
}

// delLinkAddress removes an address we previously configured. A missing address is
// success: the desired end state is that it is gone.
func delLinkAddress(link, cidr string) error {
	out, err := exec.Command("ip", "addr", "del", cidr, "dev", link).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Cannot assign requested address") ||
			strings.Contains(string(out), "No such") {
			return nil
		}
		return fmt.Errorf("removing %s from %s: %w (%s)", cidr, link, err, out)
	}
	return nil
}

// addLinkAddress brings a link up and gives it an address on the camera segment.
func addLinkAddress(link, cidr string) error {
	if out, err := exec.Command("ip", "link", "set", link, "up").CombinedOutput(); err != nil {
		return fmt.Errorf("bringing %s up: %w (%s)", link, err, out)
	}
	out, err := exec.Command("ip", "addr", "add", cidr, "dev", link).CombinedOutput()
	if err != nil {
		// An address that is already present is the desired state, not a failure:
		// this runs again after an agent restart.
		if strings.Contains(string(out), "File exists") {
			return nil
		}
		return fmt.Errorf("adding %s to %s: %w (%s)", cidr, link, err, out)
	}
	return nil
}
