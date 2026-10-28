package meshsession

import (
	"context"
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
)

// WAU3 carries complete datagrams over one authenticated TLS byte stream.
// Unlike QUIC DATAGRAM, frames are ordered and reliable until this connection
// ends. A two-byte length preserves empty messages and message boundaries.
// Setup negotiates the smaller complete-message capacity of the two peers.
type TLSDatagramFlow struct {
	conn        *tlsAppConn
	endpoint    *udpmessage.Endpoint
	maximum     uint16
	readMu      udpReceiveLock
	writeMu     sync.Mutex
	done        chan struct{}
	failed      chan struct{}
	failOnce    sync.Once
	cleanup     sync.Once
	ownsManager bool
	header      [2]byte
	headerUsed  int
	payload     []byte
	payloadUsed int
}

func writeTLSUDPFrame(conn net.Conn, mu *sync.Mutex, payload []byte, maximum uint16) error {
	if len(payload) > int(maximum) {
		return udpmessage.ErrCapacity
	}
	mu.Lock()
	defer mu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(udpIdleTimeout)); err != nil {
		return err
	}
	var header [2]byte
	binary.BigEndian.PutUint16(header[:], uint16(len(payload)))
	for _, part := range [][]byte{header[:], payload} {
		for len(part) != 0 {
			n, err := conn.Write(part)
			if err != nil || n <= 0 {
				// A partial frame cannot be retried on a different flow.
				if err != nil {
					return err
				}
				return io.ErrShortWrite
			}
			part = part[n:]
		}
	}
	return conn.SetWriteDeadline(time.Time{})
}

func DialUDPTLS(ctx context.Context, credentials *localmesh.Credentials, peer meshid.Device, address netip.AddrPort, port uint16, options ...UDPOptions) (*TLSDatagramFlow, error) {
	maximum := uint16(MaxUDPMessageSize)
	conn, err := dialTLSRequest(ctx, credentials, peer, address, port, &maximum)
	if err != nil {
		return nil, err
	}
	opts := udpOptions(options)
	manager, endpoint := opts.Manager, opts.Endpoint
	if endpoint != nil {
		if manager != nil && manager != endpoint.Manager() {
			conn.Close()
			return nil, udpmessage.ErrMalformed
		}
		manager = endpoint.Manager()
	}
	owns := manager == nil
	if owns {
		manager = udpmessage.NewManager()
	}
	if endpoint == nil {
		guard := opts.Guard
		if guard == nil {
			guard = func(send func() error) error { return send() }
		}
		endpoint, err = manager.Open(peer, guard)
		if err != nil {
			conn.Close()
			if owns {
				manager.Close()
			}
			return nil, err
		}
	}
	f := &TLSDatagramFlow{conn: conn, endpoint: endpoint, maximum: maximum, done: make(chan struct{}), failed: make(chan struct{}), ownsManager: owns}
	finish := func() {
		f.cleanup.Do(func() {
			endpoint.Close()
			if owns {
				manager.Close()
			}
			close(f.done)
		})
	}
	if !conn.register(finish) {
		finish()
		return nil, net.ErrClosed
	}
	// Account for the bounded partial receive buffer before reading frames.
	if err := endpoint.ReserveSocketBytes(int(maximum) + 2); err != nil {
		conn.Close()
		return nil, err
	}
	// Preserve the endpoint's existing source guard. TLS writes complete
	// messages directly, so this transport needs no unreliable reassembly.
	if err := endpoint.SetTransport(MaxUDPTransportChunk, true, func(b []byte) error {
		select {
		case <-f.failed:
			return net.ErrClosed
		default:
		}
		err := writeTLSUDPFrame(conn, &f.writeMu, b, maximum)
		if err != nil {
			f.failOnce.Do(func() { close(f.failed) })
		}
		return err
	}); err != nil {
		conn.Close()
		return nil, err
	}
	go func() {
		select {
		case <-opts.Done:
		case <-endpoint.Done():
		case <-f.done:
		case <-f.failed:
		}
		f.Close()
	}()
	return f, nil
}

func (f *TLSDatagramFlow) Maximum() int          { return int(f.maximum) }
func (f *TLSDatagramFlow) Done() <-chan struct{} { return f.done }
func (f *TLSDatagramFlow) Close() error          { return f.conn.Close() }
func (f *TLSDatagramFlow) Send(b []byte) error {
	if len(b) > int(f.maximum) {
		return udpmessage.ErrCapacity
	}
	return localUDPError(f.endpoint.Send(b))
}

func (f *TLSDatagramFlow) Receive(ctx context.Context) (*udpmessage.Message, error) {
	if err := f.readMu.lock(ctx, f.done); err != nil {
		return nil, localUDPError(err)
	}
	defer f.readMu.unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := f.endpoint.WithGuard(func() error { return nil }); err != nil {
			return nil, localUDPError(err)
		}
		deadline := time.Now().Add(100 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := f.conn.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		if f.headerUsed != len(f.header) {
			n, err := f.conn.Read(f.header[f.headerUsed:])
			f.headerUsed += n
			if err != nil {
				if e, ok := err.(net.Error); ok && e.Timeout() {
					continue
				}
				return nil, err
			}
			if f.headerUsed != len(f.header) {
				continue
			}
		}
		if f.payload == nil {
			size := int(binary.BigEndian.Uint16(f.header[:]))
			if size > int(f.maximum) {
				f.Close()
				return nil, ErrUDPUnsupported
			}
			f.payload = make([]byte, size)
		}
		if f.payloadUsed != len(f.payload) {
			n, err := f.conn.Read(f.payload[f.payloadUsed:])
			f.payloadUsed += n
			if err != nil {
				if e, ok := err.(net.Error); ok && e.Timeout() {
					continue
				}
				return nil, err
			}
			if f.payloadUsed != len(f.payload) {
				continue
			}
		}
		message, err := f.endpoint.RetainReceived(f.payload)
		f.payload, f.payloadUsed, f.headerUsed = nil, 0, 0
		if errors.Is(err, udpmessage.ErrCapacity) {
			continue
		}
		return message, localUDPError(err)
	}
}

// Keep TLS UDP in the same route/credential retirement registry as QUIC UDP.
func (c *Client) DialUDPTLS(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, options ...UDPOptions) (*TLSDatagramFlow, error) {
	if c == nil {
		return nil, net.ErrClosed
	}
	if peer == c.credentials.Device {
		return nil, ErrNoRoute
	}
	setup, cancel := context.WithCancel(ctx)
	defer cancel()
	entry := &udpSession{peer: peer, address: address, cancel: cancel, tls: true}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	c.udpSessions[entry] = struct{}{}
	c.mu.Unlock()
	opts := udpOptions(options)
	if opts.Manager == nil {
		if opts.Endpoint != nil {
			opts.Manager = opts.Endpoint.Manager()
		} else {
			opts.Manager = c.udpManager
		}
	}
	flow, err := DialUDPTLS(setup, c.credentials, peer, address, port, opts)
	c.mu.Lock()
	valid := err == nil && !c.closed && !entry.retired
	if valid {
		entry.flow, entry.cancel = flow, nil
	} else {
		delete(c.udpSessions, entry)
	}
	c.mu.Unlock()
	if !valid {
		if flow != nil {
			flow.Close()
		}
		if err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
	go func() { <-flow.Done(); c.mu.Lock(); delete(c.udpSessions, entry); c.mu.Unlock() }()
	return flow, nil
}

func (s *Server) serveTLSDatagrams(ctx context.Context, secure *tlsRecordConn, port uint16) {
	var rest [2]byte
	if _, err := io.ReadFull(secure, rest[:]); err != nil {
		return
	}
	maximum := min(binary.BigEndian.Uint16(rest[:]), uint16(MaxUDPMessageSize))
	ack := func(status byte) error {
		var b [8]byte
		copy(b[:4], "WUA3")
		b[4], b[5] = status, 3
		binary.BigEndian.PutUint16(b[6:], maximum)
		_, err := secure.Write(b[:])
		return err
	}
	if maximum == 0 || port == 0 {
		_ = ack(2)
		return
	}
	auth, ok := s.authorizer.(UDPAuthorizer)
	if !ok {
		_ = ack(0)
		return
	}
	token := auth.UDPToken(port)
	guard := udpmessage.Guard(func(send func() error) error {
		return auth.WithAuthorizedUDPToken(port, token, func(netip.AddrPort) error { return send() })
	})
	var chain [][]byte
	for _, cert := range secure.ConnectionState().PeerCertificates {
		chain = append(chain, cert.Raw)
	}
	identity, err := s.credentials.Verify(chain, time.Now())
	if err != nil {
		return
	}
	endpoint, err := s.udpManager.Open(identity.Device, guard)
	if err != nil {
		_ = ack(3)
		return
	}
	defer endpoint.Close()
	if endpoint.ReserveSocketBytes(512<<10) != nil {
		_ = ack(3)
		return
	}
	var local *net.UDPConn
	err = auth.WithAuthorizedUDPToken(port, token, func(address netip.AddrPort) error {
		var err error
		local, err = net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(address))
		return err
	})
	if err != nil {
		_ = ack(0)
		return
	}
	defer local.Close()
	if configureLocalUDP(local) != nil {
		_ = ack(3)
		return
	}
	socketBytes, err := localUDPBuffers(local)
	if err != nil || endpoint.ReserveSocketBytes(socketBytes+2*int(maximum)+4) != nil {
		_ = ack(3)
		return
	}
	if ack(1) != nil {
		return
	}
	_ = secure.SetDeadline(time.Time{})
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-auth.UDPDone(port, token):
		case <-ctx.Done():
		case <-finished:
		}
		secure.Close()
		local.Close()
	}()
	result := make(chan error, 2)
	go func() {
		var header [2]byte
		buf := make([]byte, maximum)
		for {
			_ = secure.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			if _, err := io.ReadFull(secure, header[:]); err != nil {
				result <- err
				return
			}
			n := int(binary.BigEndian.Uint16(header[:]))
			if n > len(buf) {
				result <- ErrUDPUnsupported
				return
			}
			if _, err := io.ReadFull(secure, buf[:n]); err != nil {
				result <- err
				return
			}
			err := guard(func() error { return writeLocalUDP(local, buf[:n]) })
			if err != nil {
				result <- err
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, int(maximum)+1)
		var writeMu sync.Mutex
		for {
			_ = local.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, _, flags, _, err := local.ReadMsgUDP(buf, nil)
			if err != nil {
				result <- err
				return
			}
			if n > int(maximum) || udpReadTruncated(flags) {
				continue
			}
			if err := guard(func() error { return writeTLSUDPFrame(secure, &writeMu, buf[:n], maximum) }); err != nil {
				result <- err
				return
			}
		}
	}()
	<-result
	secure.Close()
	local.Close()
	<-result
}
