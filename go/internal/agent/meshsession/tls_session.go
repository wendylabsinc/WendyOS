package meshsession

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/tsarelay"
)

const TLSALPN = "wendy-app-mesh-tls/1"
const TLSPort = localmesh.AppSessionTLSPort

// TLSAppSessionSetupTimeout bounds TLS and the application port acknowledgement.
// Routed ESP32 BLE certificate flights can exceed a minute. Outer TCP/UDP VIP
// proxies share this ceiling so they do not cancel otherwise viable setup.
// Caller cancellation still applies when a shorter deadline is needed.
const TLSAppSessionSetupTimeout = 120 * time.Second

// TCP/TLS carries one independently authorized app stream per connection.
// Its WAS1 port request and ACK are identical to one QUIC app stream. Neither
// discovery hints nor a failed QUIC connection authorize this transport.
type tlsAppConn struct {
	*tlsRecordConn
	peer    meshid.Device
	mu      sync.Mutex
	closed  bool
	expiry  *time.Timer
	onClose func()
}

func (c *tlsAppConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.expiry != nil {
		c.expiry.Stop()
	}
	callback := c.onClose
	c.mu.Unlock()
	err := c.Conn.Close()
	if callback != nil {
		callback()
	}
	return err
}
func (c *tlsAppConn) register(callback func()) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	c.onClose = callback
	return true
}

func dialTLS(ctx context.Context, credentials *localmesh.Credentials, peer meshid.Device, address netip.AddrPort, port uint16) (*tlsAppConn, error) {
	return dialTLSRequest(ctx, credentials, peer, address, port, nil)
}

func dialTLSRequest(ctx context.Context, credentials *localmesh.Credentials, peer meshid.Device, address netip.AddrPort, port uint16, datagrams *uint16) (*tlsAppConn, error) {
	if credentials == nil || port == 0 || !address.IsValid() || address.Port() == 0 {
		return nil, errors.New("invalid TLS app dial")
	}
	config, err := credentials.PeerTLS(peer)
	if err != nil {
		return nil, err
	}
	config.NextProtos = []string{TLSALPN}
	config.ClientSessionCache = nil
	config.SessionTicketsDisabled = true
	bounded, cancel := context.WithTimeout(ctx, TLSAppSessionSetupTimeout)
	defer cancel()
	dialer := net.Dialer{}
	if meshid.MeshPrefix.Contains(address.Addr()) {
		dialer.LocalAddr = &net.TCPAddr{IP: net.IP(credentials.Device.MeshAddress().AsSlice())}
	}
	raw, err := dialer.DialContext(bounded, "tcp", address.String())
	if err != nil {
		return nil, err
	}
	secure := tls.Client(raw, config)
	stop := context.AfterFunc(bounded, func() { _ = raw.Close() })
	ok := false
	defer func() {
		if !stop() && bounded.Err() != nil {
			_ = raw.Close()
		}
		if !ok {
			_ = secure.Close()
		}
	}()
	if deadline, has := bounded.Deadline(); has {
		_ = secure.SetDeadline(deadline)
	}
	if err = secure.HandshakeContext(bounded); err != nil {
		return nil, err
	}
	state := secure.ConnectionState()
	if state.Version != tls.VersionTLS13 || state.DidResume || state.NegotiatedProtocol != TLSALPN {
		return nil, errors.New("invalid TLS app protocol")
	}
	expires, err := verifiedSessionExpiry(credentials, state)
	if err != nil {
		return nil, err
	}
	conn := &tlsAppConn{tlsRecordConn: &tlsRecordConn{Conn: secure}, peer: peer}
	conn.mu.Lock()
	conn.expiry = time.AfterFunc(time.Until(expires), func() { _ = conn.Close() })
	conn.mu.Unlock()
	var hello [8]byte
	copy(hello[:4], "WAS1")
	binary.BigEndian.PutUint16(hello[4:], port)
	helloSize := 6
	if datagrams != nil {
		copy(hello[:4], "WAU3")
		binary.BigEndian.PutUint16(hello[6:], *datagrams)
		helloSize = len(hello)
	}
	if n, e := secure.Write(hello[:helloSize]); e != nil || n != helloSize {
		conn.Close()
		if e == nil {
			e = io.ErrShortWrite
		}
		return nil, e
	}
	var ack [8]byte
	ackSize := 1
	if datagrams != nil {
		ackSize = len(ack)
	}
	if _, err = io.ReadFull(secure, ack[:ackSize]); err != nil {
		conn.Close()
		return nil, err
	}
	status := ack[0]
	if datagrams != nil {
		selected := binary.BigEndian.Uint16(ack[6:])
		if string(ack[:4]) != "WUA3" || ack[5] != 3 || selected == 0 || selected > *datagrams {
			conn.Close()
			return nil, ErrUDPUnsupported
		}
		*datagrams, status = selected, ack[4]
	}
	if status != 1 {
		conn.Close()
		if datagrams != nil {
			if status == 3 {
				return nil, udpmessage.ErrCapacity
			}
			if status != 0 {
				return nil, ErrUDPUnsupported
			}
		}
		return nil, ErrDenied
	}
	if bounded.Err() != nil {
		conn.Close()
		return nil, bounded.Err()
	}
	if !stop() {
		conn.Close()
		return nil, bounded.Err()
	}
	if err = secure.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	ok = true
	return conn, nil
}

// DialTLS tracks active TCP streams through credential replacement and shutdown.
// As for QUIC TCP streams, a temporary carrier withdrawal alone does not revoke
// an authenticated flow; signed packet admission still gates every routed hop.
func (c *Client) DialTLS(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16) (net.Conn, error) {
	if c == nil {
		return nil, ErrNoRoute
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	setup, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.stop:
			cancel()
		case <-setup.Done():
		}
	}()
	conn, err := dialTLS(setup, c.credentials, peer, address, port)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		conn.Close()
		return nil, net.ErrClosed
	}
	if c.tlsApps == nil {
		c.tlsApps = map[*tlsAppConn]struct{}{}
	}
	if !conn.register(func() { c.mu.Lock(); delete(c.tlsApps, conn); c.mu.Unlock() }) {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	c.tlsApps[conn] = struct{}{}
	c.mu.Unlock()
	return conn, nil
}

func (s *Server) TLSAddr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tlsListener == nil {
		return nil
	}
	return s.tlsListener.Addr()
}

func (s *Server) RunTLS(ctx context.Context, addr string) error {
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := listenAppTCP(run, s.credentials, addr)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || s.tlsListener != nil {
		s.mu.Unlock()
		listener.Close()
		return net.ErrClosed
	}
	s.tlsListener = listener
	if s.tlsConnections == nil {
		s.tlsConnections = map[*tls.Conn]struct{}{}
	}
	s.mu.Unlock()
	config := s.TLSConfig()
	config.NextProtos = []string{TLSALPN}
	config.SessionTicketsDisabled = true
	connections := make(chan struct{}, 32)
	handshakes := make(chan struct{}, 4)
	var workers sync.WaitGroup
	stop := context.AfterFunc(run, func() {
		listener.Close()
		s.mu.Lock()
		var conns []*tls.Conn
		for conn := range s.tlsConnections {
			conns = append(conns, conn)
		}
		s.mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
	})
	defer func() {
		cancel()
		stop()
		listener.Close()
		s.mu.Lock()
		var conns []*tls.Conn
		for conn := range s.tlsConnections {
			conns = append(conns, conn)
		}
		s.tlsListener = nil
		s.mu.Unlock()
		for _, conn := range conns {
			conn.Close()
		}
		workers.Wait()
	}()
	for {
		raw, err := listener.Accept()
		if err != nil {
			if run.Err() != nil {
				return nil
			}
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		select {
		case handshakes <- struct{}{}:
		default:
			<-connections
			raw.Close()
			continue
		}
		secure := tls.Server(raw, config)
		s.mu.Lock()
		closed := s.closed
		if !closed {
			s.tlsConnections[secure] = struct{}{}
		}
		s.mu.Unlock()
		if closed {
			secure.Close()
			<-connections
			<-handshakes
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-connections; s.mu.Lock(); delete(s.tlsConnections, secure); s.mu.Unlock() }()
			defer secure.Close()
			setup, done := context.WithTimeout(run, TLSAppSessionSetupTimeout)
			deadline, _ := setup.Deadline()
			_ = secure.SetDeadline(deadline)
			err := secure.HandshakeContext(setup)
			done()
			<-handshakes
			if err != nil {
				return
			}
			state := secure.ConnectionState()
			if state.Version != tls.VersionTLS13 || state.DidResume || state.NegotiatedProtocol != TLSALPN {
				return
			}
			expires, err := verifiedSessionExpiry(s.credentials, state)
			if err != nil {
				return
			}
			s.serveTLS(run, secure, expires)
		}()
	}
}

func (s *Server) serveTLS(parent context.Context, secure *tls.Conn, expires time.Time) {
	records := &tlsRecordConn{Conn: secure}
	session, cancel := context.WithDeadline(parent, expires)
	defer cancel()
	stopSession := context.AfterFunc(session, func() { secure.Close() })
	defer stopSession()
	var hello [6]byte
	if _, err := io.ReadFull(secure, hello[:]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(hello[4:])
	if string(hello[:4]) == "WAU3" {
		s.serveTLSDatagrams(session, records, port)
		return
	}
	if string(hello[:4]) != "WAS1" {
		return
	}
	if port == 0 || session.Err() != nil {
		return
	}
	if port == tsarelay.Port {
		if _, err := secure.Write([]byte{1}); err == nil {
			_ = tsarelay.ServeConnection(session, records)
		}
		return
	}
	local, err := s.authorizer.DialAuthorized(port, func(address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(session, "tcp4", address)
	})
	if err != nil {
		_, _ = secure.Write([]byte{0})
		return
	}
	defer local.Close()
	if session.Err() != nil {
		return
	}
	stop := context.AfterFunc(session, func() { secure.Close(); local.Close() })
	defer stop()
	var revoked <-chan struct{}
	if owner, ok := local.(interface{ Done() <-chan struct{} }); ok {
		revoked = owner.Done()
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-revoked:
			secure.Close()
			local.Close()
		case <-session.Done():
		case <-finished:
		}
	}()
	if _, err := secure.Write([]byte{1}); err != nil {
		return
	}
	if err := secure.SetDeadline(time.Time{}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(local, secure)
		if half, ok := local.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() { _, _ = io.Copy(records, local); _ = records.CloseWrite(); done <- struct{}{} }()
	<-done
	<-done
}
