package timesync

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	multicastGroup = "239.255.87.84"
	multicastPort  = 5887
	udpMaxPktSize  = 65536
)

// Exported so the CLI sender targets exactly what the agent listens on.
const (
	// DatagramPort is the UDP port WendyDatagram time proofs are sent to, over
	// both IPv4 and IPv6.
	DatagramPort = multicastPort
	// LinkLocalGroupV6 is the IPv6 link-local multicast group for time proofs.
	// IPv6 link-local is the one addressing every USB gadget link is
	// guaranteed to have, and unlike IPv4 it is not subject to reverse-path
	// filtering of a host whose source address is off the device's subnet.
	LinkLocalGroupV6 = "ff02::5741:5887"
)

// RunMulticast listens for WendyDatagram time proofs on the IPv4 multicast
// group and, in parallel, on UDP6 — the IPv6 link-local group plus unicast
// (the CLI also sends straight to fe80::5741:1 on USB links). On listen
// failure or socket error each listener reconnects after 5 s (polling
// approach — no netlink). Blocks until ctx is done.
func (m *Manager) RunMulticast(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runListener(ctx, m.listenMulticast)
	}()
	m.runListener(ctx, m.listenUDP6)
	<-done
}

func (m *Manager) runListener(ctx context.Context, listen func(context.Context)) {
	for {
		if ctx.Err() != nil {
			return
		}
		listen(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (m *Manager) listenMulticast(ctx context.Context) {
	group := &net.UDPAddr{IP: net.ParseIP(multicastGroup), Port: multicastPort}
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("timesync: multicast listen failed", zap.Error(err))
		}
		return
	}
	defer conn.Close()

	// ListenMulticastUDP with a nil interface joins only the kernel's default
	// multicast interface — the Wi-Fi/LTE uplink, never a USB gadget link — so
	// `wendy device sync-time` from a USB-tethered host never arrived. Join on
	// every multicast interface, re-checking each read cycle so a link that
	// comes up later (USB plugged in after boot) is picked up too.
	pc := ipv4.NewPacketConn(conn)
	joined := make(map[string]bool)
	m.serveTimePackets(ctx, conn, func() { joinMulticastInterfaces(pc, group, joined, m.logger) })
}

// listenUDP6 binds [::]:DatagramPort (IPV6_V6ONLY, so it does not collide with
// the IPv4 socket) and joins LinkLocalGroupV6 on every multicast interface.
// Being unbound to any address, it also receives unicast proofs.
func (m *Manager) listenUDP6(ctx context.Context) {
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6unspecified, Port: multicastPort})
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("timesync: udp6 listen failed", zap.Error(err))
		}
		return
	}
	defer conn.Close()

	group := &net.UDPAddr{IP: net.ParseIP(LinkLocalGroupV6)}
	pc := ipv6.NewPacketConn(conn)
	joined := make(map[string]bool)
	m.serveTimePackets(ctx, conn, func() { joinMulticastInterfaces(pc, group, joined, m.logger) })
}

// processPacketFn is a seam for tests.
var processPacketFn = SafeProcessPacket

// serveTimePackets reads and applies time proofs from conn until ctx is done
// or the socket fails. beforeRead runs every read cycle (5 s at most).
func (m *Manager) serveTimePackets(ctx context.Context, conn *net.UDPConn, beforeRead func()) {
	// Close on cancel so shutdown releases the port at once instead of after
	// the current read deadline expires.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	buf := make([]byte, udpMaxPktSize)
	for {
		beforeRead()
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Timeout or transient error — loop back and check context.
			continue
		}

		t, err := processPacketFn(buf[:n])
		if err != nil {
			if m.logger != nil {
				m.logger.Debug("timesync: invalid multicast packet", zap.Error(err))
			}
			continue
		}
		if t.IsZero() {
			continue // unknown msg_type — forward-compat, silently ignore
		}

		if m.logger != nil {
			m.logger.Info("timesync: synced via multicast relay", zap.Time("midpoint", t))
		}
		m.applyTime(t)
	}
}

// multicastGroupJoiner is the subset of *ipv4.PacketConn / *ipv6.PacketConn
// used to join groups;
// an interface so tests can record joins without real sockets.
type multicastGroupJoiner interface {
	JoinGroup(*net.Interface, net.Addr) error
}

// listMulticastInterfacesFn is a seam for tests.
var listMulticastInterfacesFn = net.Interfaces

// joinMulticastInterfaces joins group on every up, non-loopback,
// multicast-capable interface not already in joined. A failed join (e.g. the
// default interface, which ListenMulticastUDP already joined) is recorded too,
// so it is not retried every cycle; an interface that disappears is forgotten
// so it is re-joined if it returns.
func joinMulticastInterfaces(pc multicastGroupJoiner, group *net.UDPAddr, joined map[string]bool, logger *zap.Logger) {
	ifaces, err := listMulticastInterfacesFn()
	if err != nil {
		return
	}
	present := make(map[string]bool, len(ifaces))
	for i := range ifaces {
		iface := ifaces[i]
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		present[iface.Name] = true
		if joined[iface.Name] {
			continue
		}
		joined[iface.Name] = true
		if err := pc.JoinGroup(&iface, group); err != nil && logger != nil {
			logger.Debug("timesync: multicast join failed", zap.String("interface", iface.Name), zap.Error(err))
		}
	}
	for name := range joined {
		if !present[name] {
			delete(joined, name)
		}
	}
}

// SafeProcessPacket wraps ProcessMulticastPacket with a recover() so that any
// unexpected panic in the untrusted-input parser cannot crash the agent process.
// Time-sync failures are always non-fatal; a panic here is treated as a parse error.
func SafeProcessPacket(pkt []byte) (t time.Time, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic processing multicast packet: %v", r)
		}
	}()
	return ProcessMulticastPacket(pkt)
}

// ProcessMulticastPacket parses and verifies a WendyDatagram UDP packet.
//
// Returns the verified midpoint time, a zero time for unknown msg_types (not
// an error — forward compatibility), or an error for malformed/invalid packets.
//
// In relay mode the Mac does not transmit the nonce separately. IETF responses
// include it at top level, and VerifyResponse validates it against the signed
// Merkle root.
func ProcessMulticastPacket(pkt []byte) (time.Time, error) {
	dg, err := roughtime.Decode(pkt)
	if err != nil {
		return time.Time{}, fmt.Errorf("datagram: %w", err)
	}

	if dg.MsgType != roughtime.MsgTypeRoughtime {
		return time.Time{}, nil // unknown type: silently ignore for forward compat
	}

	rp, err := roughtime.DecodeRoughtimePayload(dg.Payload)
	if err != nil {
		return time.Time{}, fmt.Errorf("roughtime payload: %w", err)
	}

	if int(rp.ServerIndex) >= len(Servers) {
		return time.Time{}, fmt.Errorf("server_index %d out of range (have %d servers)", rp.ServerIndex, len(Servers))
	}
	srv := Servers[rp.ServerIndex]

	nonce := rp.Nonce
	if len(nonce) == 0 {
		var err error
		nonce, err = roughtime.ExtractNonceFromResponse(rp.Response)
		if err != nil {
			return time.Time{}, fmt.Errorf("extract nonce: %w", err)
		}
	}

	result, err := roughtime.VerifyResponse(rp.Response, nonce, srv)
	if err != nil {
		return time.Time{}, fmt.Errorf("verify: %w", err)
	}

	return result.Midpoint, nil
}
