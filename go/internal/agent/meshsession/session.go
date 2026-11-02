// Package meshsession carries app TCP streams and UDP datagrams in end-to-end
// QUIC sessions over the Babel-routed local mesh. Hop links cannot read them.
package meshsession

import (
	"context"
	"crypto/tls"
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

const ALPN = "wendy-app-mesh/1"
const Port = localmesh.AppSessionPort
const appSessionSetupTimeout = 60 * time.Second

var ErrNoRoute = errors.New("no authenticated local wendy net route")
var ErrDenied = errors.New("wendy net app ingress denied")

// Authorizer admits only ingress ports belonging to running mesh-mode apps
// and holds authorization until its verified container endpoint is connected.
type Authorizer interface {
	DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error)
}

func quicConfig() *quic.Config {
	// A contended BLE hop may need several retransmissions for the QUIC
	// Initial and TLS flight. Keep handshake idle bounded, but allow the
	// carrier to drain competing mesh-app setup packets.
	// Normal multi-hop BLE replies can exceed the upstream 200ms initial
	// probe timeout. A measured RTT replaces this conservative estimate.
	// Bound the initial burst on BLE paths with limited buffering; normal
	// congestion control grows the window as packets are acknowledged.
	return &quic.Config{MaxCryptoStreamSize: localmesh.MaxQUICCryptoStreamSize, InitialRTT: time.Second, InitialCongestionWindowPackets: 4, HandshakeIdleTimeout: 30 * time.Second, MaxIdleTimeout: 45 * time.Second,
		MaxIncomingStreams: 8, MaxIncomingUniStreams: -1, EnableDatagrams: true, Allow0RTT: false,
		// The routed TUN carries at most 1320 bytes end to end. quic-go's
		// default 1280-byte UDP payload exceeds that once IPv6/UDP headers
		// are added; the kernel drops the Initial before it reaches the TUN.
		InitialPacketSize: 1200, DisablePathMTUDiscovery: true}
}

// Resolve computes the app endpoint for a target device principal. The
// destination follows from the target UUID alone — it is available before any
// manifest for that device arrives, and no directory data is consulted. The
// full principal is what authentication pins, not the address.
//
// A usable Babel route is still required: deterministic addressing says where
// a device lives, not that the mesh can currently deliver to it.
func Resolve(snapshot localmesh.NodeSnapshot, target meshid.Device) (netip.AddrPort, error) {
	address := target.MeshAddress()
	prefix := netip.PrefixFrom(address, 128)
	routes := snapshot.ForwardingRoutes
	if routes == nil {
		routes = snapshot.Routes
	} // Older/synthetic snapshot producers.
	for _, r := range routes {
		if r.Prefix == prefix && !r.Unreachable && !r.Local {
			return netip.AddrPortFrom(address, Port), nil
		}
	}
	return netip.AddrPort{}, ErrNoRoute
}

// Dial authenticates the intended device principal, opens one QUIC stream,
// and waits for the destination's port authorization before returning the
// byte pipe. The TLS configuration pins peer's full principal, so a device
// answering at peer's address with any other identity fails the handshake.
func Dial(ctx context.Context, credentials *localmesh.Credentials, peer meshid.Device, address netip.AddrPort, port uint16) (net.Conn, error) {
	if credentials == nil || port == 0 || address.Port() == 0 {
		return nil, errors.New("invalid wendy net app dial")
	}
	cfg, err := credentials.PeerTLSWithTickets(peer, ALPN, "app-quic")
	if err != nil {
		return nil, err
	}
	conn, err := dialAppQUIC(ctx, credentials, address, cfg, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("wendy net app QUIC dial: %w", err)
	}
	if _, err := limitSessionLifetime(credentials, conn); err != nil {
		_ = conn.CloseWithError(1, "net app credentials invalid")
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.CloseWithError(1, "app dial failed")
		}
	}()
	stream, err := requestPort(ctx, conn, port)
	if err != nil {
		return nil, err
	}
	ok = true
	return &streamConn{Stream: stream, conn: conn}, nil
}

func requestPort(ctx context.Context, conn *quic.Conn, port uint16) (*quic.Stream, error) {
	return requestAppPort(ctx, conn, port, [4]byte{'W', 'A', 'S', '1'})
}

func requestAppPort(ctx context.Context, conn *quic.Conn, port uint16, magic [4]byte, udpMaximum ...*int) (*quic.Stream, error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("wendy net app stream open: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			stream.CancelRead(1)
			stream.CancelWrite(1)
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	// Stream I/O has no context parameter. A caller may cancel without a
	// deadline, or before its deadline expires, while the peer withholds its ACK.
	// Wake that I/O, and join the callback before handing the stream to its owner
	// so a completed dial continues to outlive its setup context.
	cancellationDone := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = stream.SetDeadline(time.Now())
		close(cancellationDone)
	})
	var cancellationJoined sync.Once
	joinCancellation := func() {
		cancellationJoined.Do(func() {
			if !stopCancellation() {
				<-cancellationDone
			}
		})
	}
	defer joinCancellation()
	hello := make([]byte, 6)
	copy(hello[:4], magic[:])
	binary.BigEndian.PutUint16(hello[4:], port)
	ackSize := 1
	if magic == udpOpenMagic {
		hello = udpOpenRequest(port)
		ackSize = 16
	}
	if _, err = stream.Write(hello); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("wendy net app port request: %w", err)
	}
	ack := make([]byte, ackSize)
	if _, err = io.ReadFull(stream, ack); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if magic == udpOpenMagic && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
			return nil, fmt.Errorf("%w: incomplete UDP control ACK", ErrUDPUnsupported)
		}
		return nil, fmt.Errorf("wendy net app port ACK: %w", err)
	}
	if magic == udpOpenMagic {
		if err := validateUDPAck(ack); err != nil {
			return nil, err
		}
		if len(udpMaximum) > 0 && udpMaximum[0] != nil {
			*udpMaximum[0] = int(binary.BigEndian.Uint16(ack[6:8]))
		}
	} else if ack[0] != 1 {
		return nil, ErrDenied
	}
	joinCancellation()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_ = stream.SetDeadline(time.Time{})
	ok = true
	return stream, nil
}

// Server accepts mutually authenticated app sessions. It rejects a requested
// port unless the live app registry owns its verified container endpoint.
type Server struct {
	credentials    *localmesh.Credentials
	authorizer     Authorizer
	tickets        *localmesh.TicketStore
	listener       *quic.Listener
	tlsListener    net.Listener
	tlsConnections map[*tls.Conn]struct{}
	packets        net.PacketConn
	mu             sync.Mutex
	closed         bool
	udpManager     *udpmessage.Manager
	ownsUDPManager bool
}

func NewServer(credentials *localmesh.Credentials, authorizer Authorizer, managers ...*udpmessage.Manager) (*Server, error) {
	if credentials == nil || authorizer == nil {
		return nil, errors.New("missing wendy net app credentials or authorizer")
	}
	manager := udpmessage.NewManager()
	owns := true
	if len(managers) > 0 && managers[0] != nil {
		manager = managers[0]
		owns = false
	}
	return &Server{credentials: credentials, authorizer: authorizer, tickets: credentials.ServerTicketStore(ALPN, "app-quic"), udpManager: manager, ownsUDPManager: owns}, nil
}

func (s *Server) TLSConfig() *tls.Config {
	config := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		NextProtos: []string{ALPN}, Certificates: []tls.Certificate{s.credentials.Certificate},
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(state tls.ConnectionState) error {
			chain := make([][]byte, 0, len(state.PeerCertificates))
			for _, cert := range state.PeerCertificates {
				chain = append(chain, cert.Raw)
			}
			identity, err := s.credentials.VerifyTLS(chain, time.Now())
			if err != nil {
				return err
			}
			// The address a peer was dialled through is derived from its UUID,
			// so the only thing that can vouch for it is the authenticated
			// principal. Same tenant, different device, and never self.
			if identity.Device.Tenant() != s.credentials.Device.Tenant() || identity.Device == s.credentials.Device {
				return errors.New("wendy net app peer is not another device in this tenant")
			}
			return nil
		}}
	s.credentials.ConfigureTLSServer(config)
	s.tickets.Configure(config)
	return config
}

// Run calls ready, when supplied, after binding the QUIC listener.
func (s *Server) Run(ctx context.Context, addr string, ready ...func()) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	packets, err := listenAppPackets(runCtx, s.credentials, addr)
	if err != nil {
		return err
	}
	defer packets.Close()
	listener, err := quic.Listen(packets, s.TLSConfig(), quicConfig())
	if err != nil {
		cancel()
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = listener.Close()
		return net.ErrClosed
	}
	s.packets = packets
	s.listener = listener
	s.mu.Unlock()
	if len(ready) != 0 && ready[0] != nil {
		ready[0]()
	}
	sem := make(chan struct{}, 64)
	var wg sync.WaitGroup
	defer func() { cancel(); _ = s.Close(); wg.Wait() }()
	go func() { <-runCtx.Done(); _ = s.Close() }()
	for {
		conn, err := listener.Accept(runCtx)
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				s.serve(runCtx, conn)
			}()
		default:
			_ = conn.CloseWithError(1, "server busy")
		}
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	listener, packets := s.listener, s.packets
	tlsListener := s.tlsListener
	tlsConnections := make([]*tls.Conn, 0, len(s.tlsConnections))
	for conn := range s.tlsConnections {
		tlsConnections = append(tlsConnections, conn)
	}
	s.mu.Unlock()
	if tlsListener != nil {
		_ = tlsListener.Close()
	}
	for _, conn := range tlsConnections {
		_ = conn.Close()
	}
	var err error
	if listener != nil {
		err = errors.Join(listener.Close(), packets.Close())
	}
	if s.ownsUDPManager {
		s.udpManager.Close()
	}
	return err
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) serve(parent context.Context, conn *quic.Conn) {
	if _, err := limitSessionLifetime(s.credentials, conn); err != nil {
		_ = conn.CloseWithError(1, "net app credentials invalid")
		return
	}
	var streams sync.WaitGroup
	defer func() { _ = conn.CloseWithError(0, "app session complete"); streams.Wait() }()
	doneConn := make(chan struct{})
	defer close(doneConn)
	go func() {
		select {
		case <-parent.Done():
			_ = conn.CloseWithError(1, "server stopping")
		case <-doneConn:
		}
	}()
	protocol := &sessionProtocol{}
	sem := make(chan struct{}, 8)
	for {
		stream, err := conn.AcceptStream(conn.Context())
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
			streams.Add(1)
			go func() {
				defer streams.Done()
				defer func() { <-sem }()
				s.serveStream(parent, conn, stream, protocol)
			}()
		default:
			stream.CancelRead(1)
			stream.CancelWrite(1)
		}
	}
}

// UDP message fragments have no destination identifier. A connection can carry
// pooled TCP streams or exactly one admitted UDP flow, never both. Keep the
// decision across stream goroutines, including after a control stream closes.
type sessionProtocol struct {
	mu       sync.Mutex
	tcp, udp bool
}

func (p *sessionProtocol) admit(udp bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if udp {
		if p.tcp || p.udp {
			return false
		}
		p.udp = true
	} else {
		if p.udp {
			return false
		}
		p.tcp = true
	}
	return true
}

func (s *Server) serveStream(parent context.Context, conn *quic.Conn, stream *quic.Stream, protocol *sessionProtocol) {
	// Both halves must retire before quic-go returns incoming stream credit.
	// Early rejection never reads the peer's FIN/reset, so explicitly abandon
	// receive on every terminal path. Graceful Close preserves a queued ACK;
	// CancelWrite here could discard it. Admitted TCP flows reach these defers
	// only after both copy directions finish, retaining legitimate half-close.
	defer stream.CancelRead(0)
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(appSessionSetupTimeout))
	var hello [6]byte
	if _, err := io.ReadFull(stream, hello[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(hello[4:])
	if string(hello[:4]) == "WAU2" {
		s.serveDatagrams(parent, conn, port, stream, protocol)
		return
	}
	if string(hello[:4]) == "WAU1" {
		writeUDPAck(stream, 2)
		return
	}
	if string(hello[:4]) != "WAS1" {
		return
	}
	local, err := s.authorizer.DialAuthorized(port, func(address string) (net.Conn, error) {
		return net.DialTimeout("tcp4", address, 5*time.Second)
	})
	if err != nil {
		deny(stream)
		return
	}
	defer local.Close()
	if !protocol.admit(false) {
		deny(stream)
		return
	}
	var revoked <-chan struct{}
	if owned, ok := local.(interface{ Done() <-chan struct{} }); ok {
		revoked = owned.Done()
	}
	localDone := make(chan struct{})
	defer close(localDone)
	go func() {
		select {
		case <-parent.Done():
			_ = local.Close()
		case <-conn.Context().Done():
			_ = local.Close()
		case <-revoked:
			// Revocation closes this stream, not other apps sharing the session.
			stream.CancelRead(1)
			stream.CancelWrite(1)
		case <-localDone:
		}
	}()
	if _, err := stream.Write([]byte{1}); err != nil {
		return
	}
	_ = stream.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(local, stream)
		if tcp, ok := local.(interface{ CloseWrite() error }); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(stream, local)
		_ = stream.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}

func deny(stream *quic.Stream) {
	_, _ = stream.Write([]byte{0})
	_ = stream.Close()
	// The authenticated session remains available to other app streams. Close
	// queues the denial and FIN; no timer or write cancellation is needed.
}

type streamConn struct {
	*quic.Stream
	conn    *quic.Conn
	release func()
	once    sync.Once
}

func (c *streamConn) Close() error {
	var err error
	c.once.Do(func() {
		c.Stream.CancelRead(0)
		if c.release != nil {
			err = c.Stream.Close()
			c.release()
		} else {
			err = c.conn.CloseWithError(0, "done")
		}
	})
	return err
}
func (c *streamConn) CloseWrite() error    { return c.Stream.Close() }
func (c *streamConn) LocalAddr() net.Addr  { return c.conn.LocalAddr() }
func (c *streamConn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

var _ net.Conn = (*streamConn)(nil)
