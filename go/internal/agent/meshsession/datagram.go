package meshsession

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

const MaxUDPPayload = 1000
const udpIdleTimeout = 30 * time.Second

var udpOpenMagic = [4]byte{'W', 'A', 'U', '1'}

// UDPAuthorizer checks each datagram while a live mesh-mode app owns the
// protocol-specific host port. Revocation cannot race a local app write.
type UDPAuthorizer interface {
	UDPToken(uint16) uint64
	WithAuthorizedUDPToken(uint16, uint64, func() error) error
}

// DatagramFlow is one app UDP source/destination tuple. Its QUIC connection is
// authenticated for the intended peer and cannot switch destination port.
type DatagramFlow struct{ conn *quic.Conn }

func DialUDP(ctx context.Context, credentials *localmesh.Credentials, peer int32, address netip.AddrPort, port uint16) (*DatagramFlow, error) {
	if credentials == nil || port == 0 || !address.IsValid() {
		return nil, errors.New("invalid mesh UDP dial")
	}
	cfg, err := credentials.PeerTLSWithTickets(peer, ALPN, "app-quic")
	if err != nil {
		return nil, err
	}
	conn, err := quic.DialAddr(ctx, address.String(), cfg, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("mesh UDP QUIC dial: %w", err)
	}
	if _, err := limitSessionLifetime(credentials, conn); err != nil {
		_ = conn.CloseWithError(1, "mesh app credentials invalid")
		return nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(1, "UDP open failed")
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	var open [6]byte
	copy(open[:4], udpOpenMagic[:])
	binary.BigEndian.PutUint16(open[4:], port)
	if _, err := stream.Write(open[:]); err != nil {
		_ = conn.CloseWithError(1, "UDP open failed")
		return nil, err
	}
	var ack [1]byte
	if _, err := io.ReadFull(stream, ack[:]); err != nil {
		_ = conn.CloseWithError(1, "UDP open failed")
		return nil, err
	}
	_ = stream.Close()
	if ack[0] != 1 {
		_ = conn.CloseWithError(1, "UDP denied")
		return nil, ErrDenied
	}
	return &DatagramFlow{conn: conn}, nil
}

func (f *DatagramFlow) Send(payload []byte) error {
	if f == nil || f.conn == nil {
		return net.ErrClosed
	}
	if len(payload) == 0 || len(payload) > MaxUDPPayload {
		return errors.New("invalid mesh UDP payload size")
	}
	return f.conn.SendDatagram(payload)
}

func (f *DatagramFlow) Receive(ctx context.Context) ([]byte, error) {
	if f == nil || f.conn == nil {
		return nil, net.ErrClosed
	}
	payload, err := f.conn.ReceiveDatagram(ctx)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 || len(payload) > MaxUDPPayload {
		return nil, errors.New("invalid mesh UDP reply size")
	}
	return payload, nil
}

func (f *DatagramFlow) Close() error {
	if f == nil || f.conn == nil {
		return nil
	}
	return f.conn.CloseWithError(0, "UDP flow closed")
}

func (s *Server) serveDatagrams(parent context.Context, conn *quic.Conn, port uint16, stream *quic.Stream) {
	auth, ok := s.authorizer.(UDPAuthorizer)
	if !ok || port == 0 {
		return
	}
	token := auth.UDPToken(port)
	var local *net.UDPConn
	err := auth.WithAuthorizedUDPToken(port, token, func() error {
		var dialErr error
		local, dialErr = net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
		return dialErr
	})
	if err != nil {
		deny(stream)
		return
	}
	defer local.Close()
	if _, err := stream.Write([]byte{1}); err != nil {
		return
	}
	_ = stream.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, MaxUDPPayload+1)
		for {
			_ = local.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := local.Read(buf)
			if err != nil {
				return
			}
			if n == 0 || n > MaxUDPPayload {
				return
			}
			response := append([]byte(nil), buf[:n]...)
			if err := auth.WithAuthorizedUDPToken(port, token, func() error {
				return conn.SendDatagram(response)
			}); err != nil {
				return
			}
		}
	}()
	defer func() { _ = local.Close(); <-done }()
	for {
		recvCtx, recvCancel := context.WithTimeout(ctx, udpIdleTimeout)
		payload, err := conn.ReceiveDatagram(recvCtx)
		recvCancel()
		if err != nil {
			return
		}
		if len(payload) == 0 || len(payload) > MaxUDPPayload {
			continue
		}
		if err := auth.WithAuthorizedUDPToken(port, token, func() error {
			_ = local.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_, writeErr := local.Write(payload)
			return writeErr
		}); err != nil {
			return
		}
	}
}
