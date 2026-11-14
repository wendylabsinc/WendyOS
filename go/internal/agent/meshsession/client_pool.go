package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	clientIdleLimit = 45 * time.Second
	clientAgeLimit  = 10 * time.Minute
	// A reused QUIC connection may survive a remote agent restart without a
	// Babel withdrawal. Bound its app-port ACK below the QUIC idle timeout,
	// while leaving cold handshakes their full caller budget on slow BLE paths.
	pooledPortACKLimit = 20 * time.Second
)

type appRetirement struct {
	quic   *quic.Conn
	udp    interface{ Close() error }
	finish func()
}

func (r appRetirement) close(reason string) {
	if r.quic != nil {
		_ = r.quic.CloseWithError(0, reason)
	}
	if r.udp != nil {
		_ = r.udp.Close()
	}
	if r.finish != nil {
		r.finish()
	}
}

// Client reuses one authenticated QUIC session per destination device
// principal. Each flow opens a separately authorized stream. Build a new
// Client whenever credentials or trust change; Close stops the pool at agent
// shutdown.
type Client struct {
	credentials    *localmesh.Credentials
	udpManager     *udpmessage.Manager
	ownsUDPManager bool
	mu             sync.Mutex
	peers          map[meshid.Device]*pooledSession
	udpSessions    map[*udpSession]struct{} // QUIC/TLS datagram flows, including pending setup
	tlsApps        map[*tlsAppConn]struct{}
	tlsDials       map[*tlsAppDial]struct{}
	closed         bool
	stop           chan struct{}
	done           chan struct{}
	closeDone      chan struct{}
	dial           func(context.Context, netip.AddrPort, *tls.Config, *quic.Config) (*quic.Conn, error)
	portACKLimit   time.Duration // test override; zero uses pooledPortACKLimit
}

type pooledSession struct {
	peer      meshid.Device
	lifetime  *sessionLifetime
	address   netip.AddrPort
	ready     chan struct{}
	closing   chan struct{}
	cancel    context.CancelFunc
	conn      *quic.Conn
	err       error
	born      time.Time
	used      time.Time
	active    int
	retired   bool
	withdrawn bool // pending/closing generation lost its caller-side route proof
	mux       *udpMultiplexer
}

func NewClient(credentials *localmesh.Credentials, managers ...*udpmessage.Manager) (*Client, error) {
	if credentials == nil {
		return nil, errors.New("missing wendy net app credentials")
	}
	c := &Client{credentials: credentials, peers: make(map[meshid.Device]*pooledSession), udpSessions: make(map[*udpSession]struct{}), stop: make(chan struct{}), done: make(chan struct{}), closeDone: make(chan struct{}),
		dial: func(ctx context.Context, endpoint netip.AddrPort, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
			return dialAppQUIC(ctx, credentials, endpoint, tlsConfig, config)
		}}
	c.udpManager = udpmessage.NewManager()
	c.ownsUDPManager = true
	if len(managers) > 0 && managers[0] != nil {
		c.udpManager = managers[0]
		c.ownsUDPManager = false
	}
	go c.reap()
	return c, nil
}

func (c *Client) reap() {
	defer close(c.done)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		var stale []appRetirement
		c.mu.Lock()
		for _, entry := range c.peers {
			if entry.ready != nil || entry.closing != nil {
				continue
			}
			credentialLive := false
			if entry.lifetime != nil {
				_, err := entry.lifetime.current()
				credentialLive = err == nil
			}
			if credentialLive && entry.conn != nil && entry.conn.Context().Err() == nil &&
				(entry.active > 0 || now.Sub(entry.used) < clientIdleLimit && now.Sub(entry.born) < clientAgeLimit) {
				continue
			}
			stale = append(stale, c.retireEntryLocked(entry)...)
		}
		c.mu.Unlock()
		for _, conn := range stale {
			conn.close("wendy net app session idle")
		}
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		done := c.closeDone
		c.mu.Unlock()
		<-done
		return nil
	}
	c.closed = true
	close(c.stop)
	for pending := range c.tlsDials {
		pending.retired = true
		pending.cancel()
	}
	var conns []appRetirement
	var barriers []<-chan struct{}
	for _, entry := range c.peers {
		conns = append(conns, c.retireEntryLocked(entry)...)
		if entry.ready != nil {
			barriers = append(barriers, entry.ready)
		} else if entry.closing != nil {
			barriers = append(barriers, entry.closing)
		}
	}
	for entry := range c.udpSessions {
		entry.retired = true
		if entry.cancel != nil {
			entry.cancel()
		}
		if entry.flow != nil {
			conns = append(conns, appRetirement{udp: entry.flow})
		}
	}
	tlsApps := make([]*tlsAppConn, 0, len(c.tlsApps))
	for conn := range c.tlsApps {
		tlsApps = append(tlsApps, conn)
	}
	c.mu.Unlock()
	for _, conn := range tlsApps {
		_ = conn.Close()
	}
	for _, conn := range conns {
		conn.close("wendy net app client stopped")
	}
	// Joining pending handshakes also makes a completed Client.Close a safe
	// boundary for replacing credentials and constructing a new client.
	for _, barrier := range barriers {
		<-barrier
	}
	<-c.done
	if c.ownsUDPManager {
		c.udpManager.Close()
	}
	close(c.closeDone)
	return nil
}

// Invalidate closes the peer session, including active and pending streams.
// Use WithdrawRoute for a temporary routing interruption.
func (c *Client) Invalidate(peer meshid.Device) {
	if c == nil {
		return
	}
	c.mu.Lock()
	conns := c.invalidateLocked(peer)
	var tlsApps []*tlsAppConn
	for conn := range c.tlsApps {
		if conn.peer == peer {
			tlsApps = append(tlsApps, conn)
		}
	}
	c.mu.Unlock()
	for _, conn := range tlsApps {
		_ = conn.Close()
	}
	for _, conn := range conns {
		conn.close("wendy net app route withdrawn")
	}
}

// WithdrawRoute cancels UDP flows and pending handshakes, but retains a live
// authenticated QUIC session so existing TCP streams can survive reconvergence.
// Every new dial still requires a fresh caller-side signed-route check. The
// routed packet layer does not forward during the outage; credential lifetime
// enforcement and Client.Close remain independent.
func (c *Client) WithdrawRoute(peer meshid.Device) {
	if c == nil {
		return
	}
	c.mu.Lock()
	conns := c.retirePeerLocked(peer, true)
	c.mu.Unlock()
	for _, conn := range conns {
		conn.close("wendy net app route withdrawn")
	}
}

// PruneRoutes applies one authenticated node snapshot to all cached app
// sessions. Missing routes retain authenticated QUIC transports while canceling
// UDP flows. Missing/expired manifests or endpoint changes invalidate them.
func (c *Client) PruneRoutes(snapshot localmesh.NodeSnapshot) {
	if c == nil {
		return
	}
	var stale []appRetirement
	c.mu.Lock()
	for pending := range c.tlsDials {
		endpoint, err := ResolveApp(snapshot, pending.peer)
		if err != nil || endpoint.Transport != localmesh.AppTLS || endpoint.Address != pending.address {
			pending.retired = true
			pending.cancel()
		}
	}
	peers := make(map[meshid.Device]netip.AddrPort)
	for peer, entry := range c.peers {
		peers[peer] = entry.address
	}
	for entry := range c.udpSessions {
		endpoint, err := ResolveApp(snapshot, entry.peer)
		transport := localmesh.AppQUIC
		if entry.tls {
			transport = localmesh.AppTLS
		}
		if err != nil || endpoint.Transport != transport || endpoint.Address != entry.address {
			entry.retired = true
			if entry.cancel != nil {
				entry.cancel()
			}
			if entry.flow != nil {
				stale = append(stale, appRetirement{udp: entry.flow})
			}
		}
	}
	for peer, address := range peers {
		endpoint, err := ResolveApp(snapshot, peer)
		if !quicProofCurrent(snapshot, peer, address) {
			stale = append(stale, c.invalidateLocked(peer)...)
		} else if err != nil || endpoint.Transport != localmesh.AppQUIC {
			stale = append(stale, c.retirePeerLocked(peer, true)...)
		} else if endpoint.Address != address {
			stale = append(stale, c.invalidateLocked(peer)...)
		}
	}
	c.mu.Unlock()
	for _, conn := range stale {
		conn.close("wendy net app route withdrawn")
	}
}

// quicProofCurrent deliberately ignores route reachability. A temporary carrier
// outage does not change the authenticated destination or listener. Expired or
// withdrawn signed destination proof, however, must not preserve the session.
func quicProofCurrent(snapshot localmesh.NodeSnapshot, peer meshid.Device, address netip.AddrPort) bool {
	for _, device := range snapshot.Devices {
		if device.Device != peer.UUID() {
			continue
		}
		if device.Withdraw || !device.LeaseDeadline().After(time.Now()) {
			return false
		}
		for _, endpoint := range device.AppEndpoints {
			if endpoint.Transport == localmesh.AppQUIC && endpoint.Port != 0 && address == netip.AddrPortFrom(peer.MeshAddress(), endpoint.Port) {
				return true
			}
		}
		return false
	}
	return false
}

// Caller holds c.mu. Close is done after unlocking. The slot stays in peers
// until CloseWithError completes, so concurrent acquires cannot overlap live
// outgoing QUIC sessions, even while an old app stream is active.
func (c *Client) retireEntryLocked(entry *pooledSession) []appRetirement {
	entry.retired = true
	if entry.cancel != nil {
		entry.cancel()
	}
	if entry.ready != nil || entry.closing != nil {
		return nil // the handshake/closure owner releases its own barrier
	}
	if entry.conn == nil {
		if c.peers[entry.peer] == entry {
			delete(c.peers, entry.peer)
		}
		return nil
	}
	entry.closing = make(chan struct{})
	done := entry.closing
	return []appRetirement{{quic: entry.conn, finish: func() {
		c.mu.Lock()
		if c.peers[entry.peer] == entry {
			delete(c.peers, entry.peer)
		}
		entry.closing = nil
		close(done)
		c.mu.Unlock()
	}}}
}

func (c *Client) invalidateLocked(peer meshid.Device) []appRetirement {
	return c.retirePeerLocked(peer, false)
}

func (c *Client) retirePeerLocked(peer meshid.Device, preserveActiveTCP bool) []appRetirement {
	for pending := range c.tlsDials {
		if pending.peer == peer {
			pending.retired = true
			pending.cancel()
		}
	}
	var conns []appRetirement
	if entry := c.peers[peer]; entry != nil {
		if preserveActiveTCP && (entry.ready != nil || entry.closing != nil) {
			entry.withdrawn = true
		}
		if !preserveActiveTCP || entry.ready != nil {
			conns = append(conns, c.retireEntryLocked(entry)...)
		}
	}
	for entry := range c.udpSessions {
		if entry.peer != peer {
			continue
		}
		entry.retired = true
		if entry.cancel != nil {
			entry.cancel()
		}
		if entry.flow != nil {
			conns = append(conns, appRetirement{udp: entry.flow})
		}
	}
	return conns
}

// Dial requires a fresh caller-side route check before each call. The passed
// endpoint must therefore come from Resolve at dial time. It never rechecks the
// route and always uses conservative QUIC startup; agent app dials use
// DialWithRouteSnapshot instead.
func (c *Client) Dial(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16) (net.Conn, error) {
	return c.DialWithRetryCheck(ctx, peer, address, port, nil)
}

// DialWithRetryCheck repeats the caller's signed-route check before retrying
// a stale pooled session. The callback must fail closed if the route to the
// target principal has been withdrawn while the old stream was waiting for an
// ACK. It does not recheck before a cold handshake and always uses
// conservative QUIC startup; agent app dials use DialWithRouteSnapshot instead.
func (c *Client) DialWithRetryCheck(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, retryCheck func() error) (net.Conn, error) {
	return c.dialWithConfig(ctx, peer, address, port, retryCheck, nil)
}

// DialWithRouteSnapshot rechecks a current authenticated snapshot before each
// cold handshake or stale-session retry. As with Dial, the caller resolves
// address from a current snapshot immediately before calling; reusing a live
// pooled session takes no further snapshot. Only a selected direct LAN carrier
// qualifies for faster QUIC startup. The callback must return the current node
// snapshot, not a saved discovery result.
func (c *Client) DialWithRouteSnapshot(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, snapshot func() localmesh.NodeSnapshot) (net.Conn, error) {
	if snapshot == nil {
		return nil, ErrNoRoute
	}
	config := routeQUICConfig(peer, address, snapshot)
	check := func() error { _, err := config(); return err }
	return c.dialWithConfig(ctx, peer, address, port, check, config)
}

func (c *Client) dialWithConfig(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, retryCheck func() error, config func() (*quic.Config, error)) (net.Conn, error) {
	if c == nil || port == 0 || !address.IsValid() || address.Port() == 0 {
		return nil, errors.New("invalid pooled wendy net app dial")
	}
	for attempt := 0; attempt < 2; attempt++ {
		entry, reused, err := c.acquireWithConfig(ctx, peer, address, config)
		if err != nil {
			return nil, err
		}
		requestCtx := ctx
		cancel := func() {}
		if reused {
			limit := c.portACKLimit
			if limit <= 0 {
				limit = pooledPortACKLimit
			}
			requestCtx, cancel = context.WithTimeout(ctx, limit)
		}
		stream, err := requestPort(requestCtx, entry.conn, port)
		cancel()
		if err == nil {
			return &streamConn{Stream: stream, conn: entry.conn, release: func() { c.release(entry) }}, nil
		}
		dead := entry.conn.Context().Err() != nil
		if errors.Is(err, ErrDenied) || ctx.Err() != nil || !dead && !stalePortACK(err) {
			c.release(entry)
			return nil, err
		}
		// A fully written request that gets no ACK can leave QUIC Context
		// live even though a restarted peer no longer owns the connection ID.
		// Stream credit waits and explicit per-stream rejection do not make
		// a healthy shared connection suspect.
		c.retire(peer, entry)
		c.release(entry)
		if attempt != 0 || !reused && !dead {
			return nil, err
		}
		if retryCheck != nil {
			if err := retryCheck(); err != nil {
				return nil, err
			}
		}
	}
	return nil, ErrNoRoute
}

func (c *Client) acquireWithConfig(ctx context.Context, peer meshid.Device, address netip.AddrPort, config func() (*quic.Config, error)) (*pooledSession, bool, error) {
	// This device's own apps are served by the ingress registry, not by a
	// session to itself: there is no Babel route to one's own address.
	if peer == c.credentials.Device {
		return nil, false, ErrNoRoute
	}
	for {
		now := time.Now()
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, false, net.ErrClosed
		}
		entry := c.peers[peer]
		if entry != nil && (entry.ready != nil || entry.closing != nil) {
			ready := entry.ready
			if entry.closing != nil {
				ready = entry.closing
			}
			c.mu.Unlock()
			select {
			case <-ready:
				c.mu.Lock()
				withdrawn := entry.withdrawn
				c.mu.Unlock()
				if withdrawn {
					// This caller checked the route before waiting on a
					// generation that was withdrawn. Require a fresh dial
					// and signed-route check rather than replaying it.
					return nil, false, ErrNoRoute
				}
				continue
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}
		credentialLive := false
		if entry != nil && entry.lifetime != nil {
			_, err := entry.lifetime.current()
			credentialLive = err == nil
		}
		if entry != nil && entry.address == address && entry.conn != nil && entry.conn.Context().Err() == nil &&
			credentialLive && (entry.active > 0 || now.Sub(entry.born) < clientAgeLimit && now.Sub(entry.used) < clientIdleLimit) {
			entry.active++
			entry.used = now
			c.mu.Unlock()
			return entry, true, nil
		}
		if entry != nil {
			stale := c.retireEntryLocked(entry)
			c.mu.Unlock()
			for _, conn := range stale {
				conn.close("wendy net app session replaced")
			}
			continue
		}
		dialCtx, dialCancel := context.WithCancel(ctx)
		newEntry := &pooledSession{peer: peer, address: address, ready: make(chan struct{}), cancel: dialCancel, born: now, used: now}
		ready := newEntry.ready
		c.peers[peer] = newEntry
		c.mu.Unlock()
		cfg, err := c.credentials.PeerTLSWithTickets(peer, multiplexALPN, "app-quic")
		var conn *quic.Conn
		routeFailed := false
		if err == nil {
			c.credentials.ConfigureTLSClient(cfg, multiplexALPN, ALPN)
			qc := quicConfig()
			if config != nil {
				qc, err = config()
				routeFailed = err != nil
			}
			if err == nil {
				conn, err = c.dial(dialCtx, address, cfg, qc)
			}
		}
		if err == nil {
			newEntry.lifetime, err = limitSessionLifetime(c.credentials, conn)
		}
		dialCancel()
		c.mu.Lock()
		newEntry.conn, newEntry.err = conn, err
		newEntry.cancel = nil
		valid := err == nil && !newEntry.retired && !c.closed && c.peers[peer] == newEntry
		if valid {
			newEntry.active = 1
			if localmesh.ApplicationProtocol(conn.ConnectionState().TLS.NegotiatedProtocol) == multiplexALPN {
				newEntry.mux = newUDPMultiplexer(conn)
				go newEntry.mux.receive()
			}
		}
		if !valid {
			newEntry.retired = true
			c.mu.Unlock()
			// Keep ready installed until any returned connection is closed. A
			// canceled handshake can still return a successfully established
			// connection concurrently with route/identity invalidation.
			if conn != nil {
				_ = conn.CloseWithError(0, "wendy net app session replaced")
			}
			c.mu.Lock()
			if c.peers[peer] == newEntry {
				delete(c.peers, peer)
			}
			newEntry.ready = nil
			close(ready)
			c.mu.Unlock()
			if routeFailed {
				// No handshake was attempted; report the route result as is.
				return nil, false, err
			}
			if err != nil {
				return nil, false, fmt.Errorf("wendy net app QUIC dial: %w", err)
			}
			return nil, false, net.ErrClosed
		}
		newEntry.ready = nil
		close(ready)
		c.mu.Unlock()
		return newEntry, false, nil
	}
}

func (c *Client) release(entry *pooledSession) {
	c.mu.Lock()
	entry.active--
	entry.used = time.Now()
	c.mu.Unlock()
}

func (c *Client) retire(peer meshid.Device, entry *pooledSession) {
	c.mu.Lock()
	stale := c.retireEntryLocked(entry)
	c.mu.Unlock()
	for _, conn := range stale {
		conn.close("wendy net app session failed")
	}
}
