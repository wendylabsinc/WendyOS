package meshsharing

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	leaseControlPort = 18867
	leaseControlALPN = "wendy-mesh-internet/1"
	leaseHeartbeat   = byte(1)
	// Certificate chains can take tens of seconds over a BLE mesh leaf.
	// Bound unauthenticated work while allowing the radio and TCP retries.
	leaseHandshakeTimeout = 60 * time.Second
	// After authentication the gateway allows this long to grant a lease, and
	// again to send its answer.
	leaseReplyTimeout = 8 * time.Second
	// The borrower allows the gateway's whole handshake, grant and reply
	// bounds, so it never abandons a slow radio handshake the gateway still
	// accepts. That is longer than one reconciliation step: leaseDialer keeps
	// the negotiation alive across steps instead of restarting it.
	leaseDialTimeout = leaseHandshakeTimeout + 2*leaseReplyTimeout
	// A step waits long enough for a LAN negotiation and leaves the rest of
	// its budget for probes and tunnel setup. A slower negotiation continues
	// in the background and a later step collects it.
	leaseStepWait          = 5 * time.Second
	leaseHeartbeatInterval = 10 * time.Second
	leaseHeartbeatTimeout  = 30 * time.Second
	maxLeaseConnections    = 128
)

type leaseBackend interface {
	// Grant installs the gateway return path before returning the allocated lease.
	Grant(context.Context, meshid.Device) (tunnelLease, error)
	// Revoke retains the slot if cleanup fails; it must never reuse stale NAT state.
	Revoke(context.Context, meshid.Device, tunnelLease) error
}

type leaseServer struct {
	ctx        context.Context
	cancel     context.CancelFunc
	listener   net.Listener
	creds      *localmesh.Credentials
	backend    leaseBackend
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	cleanupErr error
	wg         sync.WaitGroup
	once       sync.Once
}

func startLeaseServer(parent context.Context, creds *localmesh.Credentials, backend leaseBackend) (io.Closer, error) {
	if creds == nil || !creds.Device.IsValid() || backend == nil {
		return nil, errors.New("gateway control requires credentials and a lease backend")
	}
	listener, err := net.Listen("tcp6", net.JoinHostPort(creds.Device.MeshAddress().String(), fmt.Sprint(leaseControlPort)))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	s := &leaseServer{ctx: ctx, cancel: cancel, listener: listener, creds: creds, backend: backend, conns: make(map[net.Conn]struct{})}
	s.wg.Add(1)
	stop := context.AfterFunc(ctx, s.stop)
	go func() {
		defer s.wg.Done()
		defer stop()
		for {
			raw, err := listener.Accept()
			if err != nil {
				s.stop()
				return
			}
			s.mu.Lock()
			if ctx.Err() != nil || len(s.conns) >= maxLeaseConnections {
				s.mu.Unlock()
				raw.Close()
				continue
			}
			s.conns[raw] = struct{}{}
			s.wg.Add(1)
			s.mu.Unlock()
			go func() {
				defer s.wg.Done()
				defer raw.Close()
				defer func() { s.mu.Lock(); delete(s.conns, raw); s.mu.Unlock() }()
				if err := serveGatewayLease(ctx, creds, backend, raw); err != nil {
					// Only backend cleanup failures are returned: routine handshake/EOF
					// errors do not accumulate per unauthenticated connection.
					s.mu.Lock()
					s.cleanupErr = err
					s.mu.Unlock()
				}
			}()
		}
	}()
	return s, nil
}

func (s *leaseServer) stop() {
	s.once.Do(func() {
		s.cancel()
		s.listener.Close()
		s.mu.Lock()
		for conn := range s.conns {
			conn.Close()
		}
		s.mu.Unlock()
	})
}

func (s *leaseServer) Close() error {
	s.stop()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupErr
}

func controlPeer(creds *localmesh.Credentials, state tls.ConnectionState) (localmesh.Identity, error) {
	if state.Version != tls.VersionTLS13 || localmesh.ApplicationProtocol(state.NegotiatedProtocol) != leaseControlALPN {
		return localmesh.Identity{}, errors.New("invalid gateway control TLS protocol")
	}
	chain := make([][]byte, len(state.PeerCertificates))
	for i, cert := range state.PeerCertificates {
		chain[i] = cert.Raw
	}
	peer, err := creds.Verify(chain, time.Now())
	if err != nil {
		return localmesh.Identity{}, err
	}
	if peer.Device == creds.Device || peer.Device.Tenant() != creds.Device.Tenant() {
		return localmesh.Identity{}, errors.New("gateway control requires another device in the same tenant")
	}
	return peer, nil
}

func controlTLS(creds *localmesh.Credentials) *tls.Config {
	config := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{creds.Certificate},
		ClientAuth:   tls.RequireAnyClientCert, SessionTicketsDisabled: true,
		NextProtos:       []string{leaseControlALPN},
		VerifyConnection: func(state tls.ConnectionState) error { _, err := controlPeer(creds, state); return err },
	}
	creds.ConfigureTLSServer(config)
	return config
}

func controlExpiry(creds *localmesh.Credentials, peer localmesh.Identity) (time.Time, error) {
	self, err := creds.Verify(creds.Certificate.Certificate, time.Now())
	if err != nil {
		return time.Time{}, err
	}
	if self.NotAfter.Before(peer.NotAfter) {
		return self.NotAfter, nil
	}
	return peer.NotAfter, nil
}

// serveGatewayLease returns cleanup errors only; all other failure paths close
// the session and revoke any granted lease. The authenticated ULA source binds
// the tunnel endpoint to the principal without requiring signed Babel adverts.
func serveGatewayLease(ctx context.Context, creds *localmesh.Credentials, backend leaseBackend, raw net.Conn) (cleanupErr error) {
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	raw.SetDeadline(time.Now().Add(leaseHandshakeTimeout))
	conn := tls.Server(raw, controlTLS(creds))
	if conn.HandshakeContext(ctx) != nil {
		return nil
	}
	peer, err := controlPeer(creds, conn.ConnectionState())
	if err != nil {
		return nil
	}
	endpoint, err := netip.ParseAddrPort(raw.RemoteAddr().String())
	if err != nil || endpoint.Addr().Unmap() != peer.Device.MeshAddress() {
		return nil
	}
	expiry, err := controlExpiry(creds, peer)
	if err != nil {
		return nil
	}
	sessionCtx, cancel := context.WithDeadline(ctx, expiry)
	defer cancel()
	expire := context.AfterFunc(sessionCtx, func() { raw.Close() })
	defer expire()
	grantCtx, cancelGrant := context.WithTimeout(sessionCtx, leaseReplyTimeout)
	lease, err := backend.Grant(grantCtx, peer.Device)
	cancelGrant()
	if err != nil {
		return nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		cleanupErr = backend.Revoke(cleanup, peer.Device, lease)
	}()
	if !lease.valid() {
		return nil
	}
	// Policy withdrawal (including an uplink change) must interrupt an idle
	// control socket immediately, rather than waiting for the next heartbeat.
	if watcher, ok := backend.(interface {
		LeaseDone(meshid.Device, tunnelLease) <-chan struct{}
	}); ok {
		revoked := watcher.LeaseDone(peer.Device, lease)
		stopWatch, watchDone := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-revoked:
				raw.Close()
			case <-stopWatch:
			}
		}()
		defer func() { close(stopWatch); <-watchDone }()
	}
	bounded := &expiryConn{Conn: conn, expires: expiry}
	bounded.SetDeadline(time.Now().Add(leaseReplyTimeout))
	response := []byte{byte(lease.Slot >> 8), byte(lease.Slot), 0}
	if lease.IPv6 {
		response[2] = 1
	}
	if writeControl(bounded, response) != nil {
		return nil
	}
	var last time.Time
	for {
		bounded.SetDeadline(time.Now().Add(leaseHeartbeatTimeout))
		var b [1]byte
		if _, err := io.ReadFull(bounded, b[:]); err != nil || b[0] != leaseHeartbeat {
			return nil
		}
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < time.Second {
			return nil
		}
		last = now
		if _, err := controlPeer(creds, conn.ConnectionState()); err != nil {
			return nil
		}
		if writeControl(bounded, b[:]) != nil {
			return nil
		}
	}
}

func dialGatewayLease(ctx context.Context, creds *localmesh.Credentials, gateway meshid.Device) (net.Conn, tunnelLease, error) {
	if creds == nil || !gateway.IsValid() || gateway == creds.Device || gateway.Tenant() != creds.Device.Tenant() {
		return nil, tunnelLease{}, errors.New("invalid gateway principal")
	}
	dialCtx, cancel := context.WithTimeout(ctx, leaseDialTimeout)
	defer cancel()
	dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(creds.Device.MeshAddress().AsSlice())}}
	raw, err := dialer.DialContext(dialCtx, "tcp6", net.JoinHostPort(gateway.MeshAddress().String(), fmt.Sprint(leaseControlPort)))
	if err != nil {
		return nil, tunnelLease{}, err
	}
	return negotiateGatewayLease(dialCtx, creds, gateway, raw)
}

// On success the caller owns the connection independently of the dial context.
// It must send heartbeat byte 1 every ten seconds, read the same-byte reply,
// and withdraw borrowed routes immediately if that exchange fails.
func negotiateGatewayLease(ctx context.Context, creds *localmesh.Credentials, gateway meshid.Device, raw net.Conn) (conn net.Conn, lease tunnelLease, err error) {
	defer func() {
		if err != nil {
			raw.Close()
		}
	}()
	cancelDial := context.AfterFunc(ctx, func() { raw.Close() })
	defer cancelDial()
	config, err := creds.PeerTLS(gateway)
	if err != nil {
		return nil, lease, err
	}
	creds.ConfigureTLSClient(config, leaseControlALPN)
	secure := tls.Client(raw, config)
	if err = secure.HandshakeContext(ctx); err != nil {
		return nil, lease, err
	}
	peer, err := controlPeer(creds, secure.ConnectionState())
	if err != nil || peer.Device != gateway {
		return nil, lease, errors.Join(errors.New("gateway identity mismatch"), err)
	}
	expiry, err := controlExpiry(creds, peer)
	if err != nil {
		return nil, lease, err
	}
	conn = &expiryConn{Conn: secure, expires: expiry}
	// TLS 1.3 finishes here before the gateway has received this client's
	// certificate flight. Over a radio that upload, the grant and the reply
	// share the remaining dial budget rather than a fixed LAN-sized bound.
	deadline := time.Now().Add(leaseDialTimeout)
	if bound, ok := ctx.Deadline(); ok && bound.Before(deadline) {
		deadline = bound
	}
	conn.SetDeadline(deadline)
	var response [3]byte
	if _, err = io.ReadFull(conn, response[:]); err != nil {
		return nil, lease, err
	}
	lease = tunnelLease{Slot: uint16(response[0])<<8 | uint16(response[1]), IPv6: response[2] == 1}
	if !lease.valid() || response[2] > 1 {
		return nil, tunnelLease{}, errors.New("invalid gateway lease")
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, tunnelLease{}, err
	}
	return conn, lease, nil
}

// leaseDialer keeps one borrower negotiation alive across reconciliation
// steps. A step's context bounds how long that step waits, not the
// negotiation: over BLE the certificate exchange alone can outlast a step,
// and restarting it on every step never completes. The caller serializes use.
type leaseDialer struct {
	dial    func(context.Context, meshid.Device) (net.Conn, tunnelLease, error)
	pending *leaseAttempt
}

type leaseAttempt struct {
	gateway meshid.Device
	cancel  context.CancelFunc
	done    chan struct{}
	conn    net.Conn
	lease   tunnelLease
	err     error
}

// await returns the lease from gateway, starting a negotiation unless one is
// already in flight. If the step stops waiting first, the negotiation
// continues and errGatewayPending is returned. On success the caller owns
// the connection.
func (d *leaseDialer) await(ctx context.Context, gateway meshid.Device) (net.Conn, tunnelLease, error) {
	if d.pending != nil && d.pending.gateway != gateway {
		d.stop()
	}
	if d.pending == nil {
		// Only stop cancels the attempt; the dial itself bounds its duration.
		attemptCtx, cancel := context.WithCancel(context.Background())
		attempt := &leaseAttempt{gateway: gateway, cancel: cancel, done: make(chan struct{})}
		go func() {
			defer close(attempt.done)
			attempt.conn, attempt.lease, attempt.err = d.dial(attemptCtx, gateway)
		}()
		d.pending = attempt
	}
	attempt := d.pending
	wait, cancel := context.WithTimeout(ctx, leaseStepWait)
	defer cancel()
	select {
	case <-attempt.done:
	case <-wait.Done():
		return nil, tunnelLease{}, errGatewayPending
	}
	d.pending = nil
	attempt.cancel()
	return attempt.conn, attempt.lease, attempt.err
}

// stop cancels a negotiation in flight and closes a lease nobody collected,
// so the gateway revokes it at once rather than after a missed heartbeat.
func (d *leaseDialer) stop() {
	attempt := d.pending
	if attempt == nil {
		return
	}
	d.pending = nil
	attempt.cancel()
	<-attempt.done
	if attempt.conn != nil {
		attempt.conn.Close()
	}
}

// Renewing an I/O deadline must never extend an authenticated lease beyond
// either endpoint's credential validity, even while heartbeats remain healthy.
type expiryConn struct {
	net.Conn
	expires time.Time
}

func (c *expiryConn) cap(t time.Time) time.Time {
	if t.IsZero() || c.expires.Before(t) {
		return c.expires
	}
	return t
}
func (c *expiryConn) SetDeadline(t time.Time) error      { return c.Conn.SetDeadline(c.cap(t)) }
func (c *expiryConn) SetReadDeadline(t time.Time) error  { return c.Conn.SetReadDeadline(c.cap(t)) }
func (c *expiryConn) SetWriteDeadline(t time.Time) error { return c.Conn.SetWriteDeadline(c.cap(t)) }
func writeControl(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
