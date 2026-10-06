package timesync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
)

const (
	multicastGroup           = "239.255.87.84"
	multicastPort            = 5887
	udpMaxPktSize            = 65536
	multicastRefreshInterval = 5 * time.Second
)

type multicastPacketConn interface {
	JoinGroup(*net.Interface, net.Addr) error
	LeaveGroup(*net.Interface, net.Addr) error
	ReadFrom([]byte) (int, *ipv4.ControlMessage, net.Addr, error)
	SetReadDeadline(time.Time) error
	Close() error
}

// RunMulticast joins the WendyDatagram multicast group and dispatches incoming
// packets to the Manager. Memberships are refreshed every 5 s so interfaces can
// become usable after startup. Socket failures reconnect after 5 s.
// Blocks until ctx is done.
func (m *Manager) RunMulticast(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		m.listenMulticast(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.after(multicastRefreshInterval):
		}
	}
}

func (m *Manager) listenMulticast(ctx context.Context) {
	group := &net.UDPAddr{IP: net.ParseIP(multicastGroup), Port: multicastPort}
	conn, err := m.openMulticast()
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("timesync: multicast listen failed", zap.Error(err))
		}
		return
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	interval := multicastRefreshInterval
	if m.multicastInterval > 0 {
		interval = m.multicastInterval
	}
	joined := make(map[int]net.Interface)
	nextRefresh := time.Now()

	buf := make([]byte, udpMaxPktSize)
	for ctx.Err() == nil {
		if !time.Now().Before(nextRefresh) {
			m.refreshMulticast(conn, group, joined)
			nextRefresh = time.Now().Add(interval)
		}
		if err := conn.SetReadDeadline(nextRefresh); err != nil {
			return
		}
		n, _, source, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				continue // also refresh memberships when no packets arrive
			}
			if m.logger != nil {
				m.logger.Debug("timesync: multicast read failed; reopening socket", zap.Error(err))
			}
			return
		}

		t, err := SafeProcessPacket(buf[:n])
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
			m.logger.Info("timesync: received verified multicast time proof",
				zap.Time("midpoint", t), zap.String("source", source.String()))
		}
		m.applyTime(t)
	}
}

func (m *Manager) openMulticast() (multicastPacketConn, error) {
	if m.multicastListen != nil {
		return m.multicastListen()
	}
	// A wildcard bind is not a multicast subscription. JoinGroup below makes
	// membership explicit on each interface, instead of choosing one by route.
	conn, err := net.ListenPacket("udp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(multicastPort)))
	if err != nil {
		return nil, err
	}
	return ipv4.NewPacketConn(conn), nil
}

func (m *Manager) refreshMulticast(conn multicastPacketConn, group net.Addr, joined map[int]net.Interface) {
	list := net.Interfaces
	if m.multicastInterfaces != nil {
		list = m.multicastInterfaces
	}
	ifaces, err := list()
	if err != nil {
		if m.logger != nil {
			m.logger.Debug("timesync: listing multicast interfaces failed", zap.Error(err))
		}
		return // retain working memberships after a transient enumeration failure
	}

	wanted := make(map[int]net.Interface)
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagMulticast != 0 && iface.Flags&net.FlagLoopback == 0 {
			wanted[iface.Index] = iface
		}
	}
	for index, iface := range joined {
		if current, ok := wanted[index]; ok && current.Name == iface.Name {
			continue
		}
		if err := conn.LeaveGroup(&iface, group); err != nil && m.logger != nil {
			m.logger.Debug("timesync: leaving multicast group failed",
				zap.String("interface", iface.Name), zap.Error(err))
		}
		// The kernel may have already removed the membership with the interface.
		// Forget it even if LeaveGroup fails, so a replacement can be joined.
		delete(joined, index)
	}
	for index, iface := range wanted {
		if _, ok := joined[index]; ok {
			continue
		}
		// IPv4 may not be configured yet even on an up interface. A failed join
		// is retried at the next refresh without disrupting other memberships.
		if err := conn.JoinGroup(&iface, group); err != nil {
			if m.logger != nil {
				m.logger.Debug("timesync: joining multicast group failed",
					zap.String("interface", iface.Name), zap.Error(err))
			}
			continue
		}
		joined[index] = iface
		if m.logger != nil {
			m.logger.Info("timesync: joined multicast time group", zap.String("interface", iface.Name))
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
