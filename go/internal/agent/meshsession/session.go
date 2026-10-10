// Package meshsession carries app TCP flows in end-to-end QUIC streams over
// the Babel-routed local mesh. Hop links cannot read these streams.
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
)

const ALPN = "wendy-app-mesh/1"
const Port = 43021
const appSessionSetupTimeout = 60 * time.Second

var ErrNoRoute = errors.New("no authenticated local mesh route")
var ErrDenied = errors.New("mesh app ingress denied")

// Authorizer admits only ingress ports belonging to running mesh-mode apps
// and holds authorization until the loopback connection has been opened.
type Authorizer interface {
	DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error)
}

func quicConfig() *quic.Config {
	// A contended BLE hop may need several retransmissions for the QUIC
	// Initial and TLS flight. Keep handshake idle bounded, but allow the
	// carrier to drain competing mesh-app setup packets.
	return &quic.Config{HandshakeIdleTimeout: 30 * time.Second, MaxIdleTimeout: 45 * time.Second,
		MaxIncomingStreams: 8, MaxIncomingUniStreams: -1, Allow0RTT: false,
		// The routed TUN carries at most 1280 total IPv4 bytes. quic-go's
		// default 1280-byte UDP payload exceeds that once IP/UDP headers are
		// added; the kernel drops the Initial before it reaches the TUN.
		InitialPacketSize: 1200, DisablePathMTUDiscovery: true}
}

// Resolve uses both an origin-signed live manifest and a usable Babel route.
// The app endpoint uses its own fixed UDP port, separate from the hop link.
func Resolve(snapshot localmesh.NodeSnapshot, org, asset int32) (netip.AddrPort, error) {
	address, _, err := localmesh.Addresses(org, asset)
	if err != nil {
		return netip.AddrPort{}, err
	}
	router, _ := localmesh.RouterID(org, asset)
	manifest := false
	for _, m := range snapshot.Devices {
		if m.Org == org && m.Asset == asset && !m.Withdraw && m.Expires > time.Now().UnixMilli() {
			manifest = true
			break
		}
	}
	if !manifest {
		return netip.AddrPort{}, ErrNoRoute
	}
	prefix := netip.PrefixFrom(address, 32)
	for _, r := range snapshot.Routes {
		if r.Prefix == prefix && r.RouterID == router && !r.Unreachable && !r.Local {
			return netip.AddrPortFrom(address, Port), nil
		}
	}
	return netip.AddrPort{}, ErrNoRoute
}

// Dial authenticates the intended asset, opens one QUIC stream, and waits for
// the destination's port authorization before returning the byte pipe.
func Dial(ctx context.Context, credentials *localmesh.Credentials, peer int32, address netip.AddrPort, port uint16) (net.Conn, error) {
	if credentials == nil || port == 0 || address.Port() == 0 {
		return nil, errors.New("invalid mesh app dial")
	}
	cfg, err := credentials.PeerTLSWithTickets(peer, ALPN, "app-quic")
	if err != nil {
		return nil, err
	}
	conn, err := quic.DialAddr(ctx, address.String(), cfg, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("mesh app QUIC dial: %w", err)
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
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("mesh app stream open: %w", err)
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
	var hello [6]byte
	copy(hello[:4], "WAS1")
	binary.BigEndian.PutUint16(hello[4:], port)
	if _, err = stream.Write(hello[:]); err != nil {
		return nil, fmt.Errorf("mesh app port request: %w", err)
	}
	var ack [1]byte
	if _, err = io.ReadFull(stream, ack[:]); err != nil {
		return nil, fmt.Errorf("mesh app port ACK: %w", err)
	}
	if ack[0] != 1 {
		return nil, ErrDenied
	}
	_ = stream.SetDeadline(time.Time{})
	ok = true
	return stream, nil
}

// Server accepts mutually authenticated app sessions. It rejects a requested
// port before dialing loopback unless the live app registry owns that port.
type Server struct {
	credentials *localmesh.Credentials
	authorizer  Authorizer
	tickets     *localmesh.TicketStore
	listener    *quic.Listener
	packets     net.PacketConn
	mu          sync.Mutex
	closed      bool
}

func NewServer(credentials *localmesh.Credentials, authorizer Authorizer) (*Server, error) {
	if credentials == nil || authorizer == nil {
		return nil, errors.New("missing mesh app credentials or authorizer")
	}
	return &Server{credentials: credentials, authorizer: authorizer, tickets: localmesh.NewTicketStore()}, nil
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
			identity, err := s.credentials.Verify(chain, time.Now())
			if err != nil {
				return err
			}
			if identity.Org != s.credentials.Org || identity.Asset == s.credentials.Asset {
				return errors.New("mesh app peer is not another asset in this organization")
			}
			return nil
		}}
	s.tickets.Configure(config)
	return config
}

func (s *Server) Run(ctx context.Context, addr string) error {
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
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.listener != nil {
		return errors.Join(s.listener.Close(), s.packets.Close())
	}
	return nil
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
				s.serveStream(parent, conn, stream)
			}()
		default:
			stream.CancelRead(1)
			stream.CancelWrite(1)
		}
	}
}

func (s *Server) serveStream(parent context.Context, conn *quic.Conn, stream *quic.Stream) {
	// Both halves must retire before quic-go returns incoming stream credit.
	// Early rejection never reads the peer's FIN/reset, so explicitly abandon
	// receive on every terminal path. Graceful Close preserves a queued ACK;
	// CancelWrite here could discard it. Admitted TCP flows reach these defers
	// only after both copy directions finish, retaining legitimate half-close.
	defer stream.CancelRead(0)
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(appSessionSetupTimeout))
	var hello [6]byte
	if _, err := io.ReadFull(stream, hello[:]); err != nil || string(hello[:4]) != "WAS1" {
		return
	}
	port := binary.BigEndian.Uint16(hello[4:])
	local, err := s.authorizer.DialAuthorized(port, func() (net.Conn, error) {
		return net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), 5*time.Second)
	})
	if err != nil {
		deny(stream)
		return
	}
	defer local.Close()
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
