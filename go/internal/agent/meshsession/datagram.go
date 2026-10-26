package meshsession

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const udpIdleTimeout = 30 * time.Second

var udpOpenMagic = [4]byte{'W', 'A', 'U', '2'}
var ErrUDPUnsupported = errors.New("incompatible Wendy Net UDP message protocol or capacity")

type UDPAuthorizer interface {
	UDPToken(uint16) uint64
	UDPDone(uint16, uint64) <-chan struct{}
	WithAuthorizedUDPToken(uint16, uint64, func(netip.AddrPort) error) error
}

func udpOpenRequest(port uint16) []byte {
	b := make([]byte, 16)
	copy(b, "WAU2")
	binary.BigEndian.PutUint16(b[4:6], port)
	binary.BigEndian.PutUint32(b[8:12], MaxUDPMessageSize)
	binary.BigEndian.PutUint16(b[12:14], MaxUDPTransportChunk)
	return b
}
func writeUDPAck(stream *quic.Stream, status byte, selected ...int) error {
	b := make([]byte, 16)
	copy(b, "WUA2")
	b[4], b[5] = status, 2
	maximum := MaxUDPTransportChunk
	if len(selected) > 0 {
		maximum = selected[0]
	}
	binary.BigEndian.PutUint16(b[6:8], uint16(maximum))
	binary.BigEndian.PutUint32(b[8:12], MaxUDPMessageSize)
	_, err := stream.Write(b)
	return err
}
func validateUDPAck(b []byte) error {
	if len(b) != 16 || string(b[:4]) != "WUA2" || b[5] != 2 || binary.BigEndian.Uint16(b[6:8]) < udpmessage.MinChunkSize || binary.BigEndian.Uint16(b[6:8]) > MaxUDPTransportChunk || binary.BigEndian.Uint32(b[8:12]) != MaxUDPMessageSize || binary.BigEndian.Uint32(b[12:16]) != 0 {
		return ErrUDPUnsupported
	}
	switch b[4] {
	case 1:
		return nil
	case 0:
		return ErrDenied
	case 3:
		return udpmessage.ErrCapacity
	default:
		return ErrUDPUnsupported
	}
}

// DatagramFlow exposes complete messages. Transport frames remain unreliable.
type DatagramFlow struct {
	conn        *quic.Conn
	endpoint    *udpmessage.Endpoint
	ownsManager bool
	readMu      udpReceiveLock
	once        sync.Once
}

func DialUDP(ctx context.Context, credentials *localmesh.Credentials, peer meshid.Device, address netip.AddrPort, port uint16, options ...UDPOptions) (*DatagramFlow, error) {
	if credentials == nil || port == 0 || !address.IsValid() {
		return nil, errors.New("invalid net UDP dial")
	}
	cfg, err := credentials.PeerTLSWithTickets(peer, ALPN, "app-quic")
	if err != nil {
		return nil, err
	}
	conn, err := quic.DialAddr(ctx, address.String(), cfg, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("net UDP QUIC dial: %w", err)
	}
	fail := func(err error) (*DatagramFlow, error) {
		_ = conn.CloseWithError(1, "UDP setup failed")
		return nil, err
	}
	if _, err := limitSessionLifetime(credentials, conn); err != nil {
		return fail(err)
	}
	if !udpDatagramsSupported(conn) {
		return fail(ErrUDPUnsupported)
	}
	maximum := MaxUDPTransportChunk
	stream, err := requestAppPort(ctx, conn, port, udpOpenMagic, &maximum)
	if err != nil {
		return fail(err)
	}
	stream.CancelRead(0)
	_ = stream.Close()
	opts := udpOptions(options)
	manager := opts.Manager
	endpoint := opts.Endpoint
	if endpoint != nil {
		if manager != nil && manager != endpoint.Manager() {
			return fail(udpmessage.ErrMalformed)
		}
		manager = endpoint.Manager()
	}
	owns := false
	if manager == nil {
		manager = udpmessage.NewManager()
		owns = true
	}
	if endpoint == nil {
		guard := opts.Guard
		if guard == nil {
			guard = func(send func() error) error {
				if conn.Context().Err() != nil {
					return net.ErrClosed
				}
				return send()
			}
		}
		endpoint, err = manager.Open(peer, guard)
		if err != nil {
			if owns {
				manager.Close()
			}
			return fail(err)
		}
	}
	if err := endpoint.SetTransport(maximum, false, quicUDPAdmission(conn)); err != nil {
		endpoint.Close()
		if owns {
			manager.Close()
		}
		return fail(err)
	}
	f := &DatagramFlow{conn: conn, endpoint: endpoint, ownsManager: owns}
	go func() {
		select {
		case <-conn.Context().Done():
		case <-endpoint.Done():
		case <-opts.Done:
		}
		_ = f.Close()
	}()
	return f, nil
}

// Both message directions require the independently negotiated transport
// capability. A valid WAU2 envelope cannot enable QUIC DATAGRAMs by itself.
func udpDatagramsSupported(conn *quic.Conn) bool {
	capability := conn.ConnectionState().SupportsDatagrams
	return capability.Local && capability.Remote
}

func quicUDPAdmission(conn *quic.Conn) func([]byte) error {
	return func(b []byte) error {
		err := conn.TrySendDatagram(b)
		if errors.Is(err, quic.ErrDatagramQueueFull) {
			return udpmessage.ErrQueueFull
		}
		var large *quic.DatagramTooLargeError
		if errors.As(err, &large) {
			return &udpmessage.ChunkTooLarge{Maximum: int(large.MaxDatagramPayloadSize)}
		}
		return err
	}
}
func (f *DatagramFlow) Send(payload []byte) error {
	if f == nil || f.endpoint == nil {
		return net.ErrClosed
	}
	return f.endpoint.Send(payload)
}
func (f *DatagramFlow) Receive(ctx context.Context) (*udpmessage.Message, error) {
	if f == nil || f.conn == nil {
		return nil, net.ErrClosed
	}
	if err := f.readMu.lock(ctx, f.endpoint.Done()); err != nil {
		return nil, localUDPError(err)
	}
	defer f.readMu.unlock()
	for {
		frame, err := f.conn.ReceiveDatagram(ctx)
		if err != nil {
			return nil, err
		}
		message, err := f.endpoint.ReceiveFrame(frame)
		if err != nil {
			return nil, err
		}
		if message != nil {
			return message, nil
		}
	}
}
func (f *DatagramFlow) Close() error {
	if f == nil || f.conn == nil {
		return nil
	}
	f.once.Do(func() {
		_ = f.conn.CloseWithError(0, "UDP flow closed")
		f.endpoint.Close()
		if f.ownsManager {
			f.endpoint.Manager().Close()
		}
	})
	return nil
}
func (s *Server) serveDatagrams(parent context.Context, conn *quic.Conn, port uint16, stream *quic.Stream, protocol *sessionProtocol) {
	var rest [10]byte
	if _, err := io.ReadFull(stream, rest[:]); err != nil {
		return
	}
	if binary.BigEndian.Uint16(rest[:2]) != 0 || binary.BigEndian.Uint32(rest[2:6]) != MaxUDPMessageSize || binary.BigEndian.Uint16(rest[6:8]) < udpmessage.MinChunkSize || binary.BigEndian.Uint16(rest[8:]) != 0 {
		_ = writeUDPAck(stream, 2)
		return
	}
	if !udpDatagramsSupported(conn) {
		_ = writeUDPAck(stream, 2)
		return
	}
	maximum := min(MaxUDPTransportChunk, int(binary.BigEndian.Uint16(rest[6:8])))
	auth, ok := s.authorizer.(UDPAuthorizer)
	if !ok || port == 0 {
		s.udpManager.RecordDenied()
		_ = writeUDPAck(stream, 0)
		return
	}
	token := auth.UDPToken(port)
	guard := udpmessage.Guard(func(send func() error) error {
		return auth.WithAuthorizedUDPToken(port, token, func(netip.AddrPort) error { return send() })
	})
	var chain [][]byte
	for _, cert := range conn.ConnectionState().TLS.PeerCertificates {
		chain = append(chain, cert.Raw)
	}
	identity, err := s.credentials.Verify(chain, time.Now())
	if err != nil {
		return
	}
	endpoint, err := s.udpManager.Open(identity.Device, guard)
	if err != nil {
		_ = writeUDPAck(stream, 3)
		return
	}
	defer endpoint.Close()
	if err := endpoint.ReserveSocketBytes(512 << 10); err != nil {
		_ = writeUDPAck(stream, 3)
		return
	}
	var local *net.UDPConn
	err = auth.WithAuthorizedUDPToken(port, token, func(address netip.AddrPort) error {
		var err error
		local, err = net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(address))
		return err
	})
	if err != nil {
		s.udpManager.RecordDenied()
		_ = writeUDPAck(stream, 0)
		return
	}
	defer local.Close()
	if err := configureLocalUDP(local); err != nil {
		_ = writeUDPAck(stream, 3)
		return
	}
	socketBytes, err := localUDPBuffers(local)
	if err != nil || endpoint.ReserveSocketBytes(socketBytes) != nil {
		_ = writeUDPAck(stream, 3)
		return
	}
	if !protocol.admit(true) {
		_ = writeUDPAck(stream, 0)
		return
	}
	defer conn.CloseWithError(0, "UDP flow complete")
	if err := endpoint.SetTransport(maximum, false, quicUDPAdmission(conn)); err != nil {
		return
	}
	if err := writeUDPAck(stream, 1, maximum); err != nil {
		return
	}
	_ = stream.Close()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		select {
		case <-auth.UDPDone(port, token):
		case <-conn.Context().Done():
		case <-endpoint.Done():
		case <-ctx.Done():
		case <-closed:
			return
		}
		cancel()
		_ = conn.CloseWithError(0, "UDP authorization ended")
		_ = local.Close()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		buf := make([]byte, MaxUDPMessageSize+1)
		for {
			n, _, flags, _, err := local.ReadMsgUDP(buf, nil)
			if err != nil {
				return
			}
			if udpReadTruncated(flags) || n > MaxUDPMessageSize {
				continue
			}
			if err := endpoint.Send(buf[:n]); err != nil {
				if errors.Is(err, udpmessage.ErrCapacity) {
					continue
				}
				return
			}
		}
	}()
	defer func() { _ = local.Close(); <-done }()
	for {
		receive, stop := context.WithTimeout(ctx, udpIdleTimeout)
		frame, err := conn.ReceiveDatagram(receive)
		stop()
		if err != nil {
			return
		}
		message, err := endpoint.ReceiveFrame(frame)
		if err != nil {
			return
		}
		if message == nil {
			continue
		}
		err = endpoint.Deliver(func() error { return writeLocalUDP(local, message.Payload) })
		message.Release()
		if err != nil {
			return
		}
	}
}
