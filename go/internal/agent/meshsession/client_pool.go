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
	quic *quic.Conn
	udp  interface{ Close() error }
}

func (r appRetirement) close(reason string) {
	if r.quic != nil {
		_ = r.quic.CloseWithError(0, reason)
	}
	if r.udp != nil {
		_ = r.udp.Close()
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
	sessions       map[*pooledSession]struct{} // includes retired sessions with active streams
	udpSessions    map[*udpSession]struct{}    // dedicated datagram sessions, including pending setup
	tlsApps        map[*tlsAppConn]struct{}
	tlsDials       map[*tlsAppDial]struct{}
	closed         bool
	stop           chan struct{}
	done           chan struct{}
	dial           func(context.Context, netip.AddrPort, *tls.Config, *quic.Config) (*quic.Conn, error)
	portACKLimit   time.Duration // test override; zero uses pooledPortACKLimit
}

type pooledSession struct {
	peer    meshid.Device
	expires time.Time
	address netip.AddrPort
	ready   chan struct{}
	conn    *quic.Conn
	err     error
	born    time.Time
	used    time.Time
	active  int
	retired bool
}

func NewClient(credentials *localmesh.Credentials, managers ...*udpmessage.Manager) (*Client, error) {
	if credentials == nil {
		return nil, errors.New("missing wendy net app credentials")
	}
	c := &Client{credentials: credentials, peers: make(map[meshid.Device]*pooledSession), sessions: make(map[*pooledSession]struct{}), udpSessions: make(map[*udpSession]struct{}), stop: make(chan struct{}), done: make(chan struct{}),
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
		var stale []*quic.Conn
		c.mu.Lock()
		for device, entry := range c.peers {
			if entry.ready != nil || entry.active != 0 ||
				entry.conn != nil && entry.conn.Context().Err() == nil && now.Sub(entry.used) < clientIdleLimit && now.Sub(entry.born) < clientAgeLimit {
				continue
			}
			delete(c.peers, device)
			entry.retired = true
			if entry.conn != nil {
				stale = append(stale, entry.conn)
			}
		}
		c.mu.Unlock()
		for _, conn := range stale {
			_ = conn.CloseWithError(0, "wendy net app session idle")
		}
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.stop)
	for pending := range c.tlsDials {
		pending.retired = true
		pending.cancel()
	}
	var conns []appRetirement
	for entry := range c.sessions {
		entry.retired = true
		if entry.conn != nil {
			conns = append(conns, appRetirement{quic: entry.conn})
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
	c.peers = make(map[meshid.Device]*pooledSession)
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
	<-c.done
	if c.ownsUDPManager {
		c.udpManager.Close()
	}
	return nil
}

// Invalidate closes every session for a peer, including active and already
// retired streams. Use WithdrawRoute for a temporary routing interruption.
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

// WithdrawRoute prevents new streams from reusing the peer's cached transport,
// but lets already authenticated TCP streams survive carrier reconvergence.
// The routed packet layer still requires a current signed route; this does not
// authorize forwarding during the outage. Certificate lifetime enforcement and
// Client.Close continue to terminate established sessions independently.
// UDP sessions are retired immediately, preserving their existing flow guards.
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
// sessions. Missing routes retire cached transports for new streams while
// preserving active TCP streams. An endpoint mismatch invalidates them outright.
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
	for entry := range c.sessions {
		endpoint, err := ResolveApp(snapshot, entry.peer)
		if err != nil || endpoint.Transport != localmesh.AppQUIC || endpoint.Address != entry.address {
			peers[entry.peer] = entry.address
		}
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
		if err != nil || endpoint.Transport != localmesh.AppQUIC {
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

// Caller holds c.mu. Close is done after unlocking.
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
	if entry := c.peers[peer]; entry != nil {
		entry.retired = true
	}
	delete(c.peers, peer)
	var conns []appRetirement
	for entry := range c.sessions {
		if entry.peer == peer {
			entry.retired = true
			if !preserveActiveTCP || entry.active == 0 {
				conns = append(conns, appRetirement{quic: entry.conn})
			}
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
// endpoint must therefore come from Resolve at dial time.
func (c *Client) Dial(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16) (net.Conn, error) {
	return c.DialWithRetryCheck(ctx, peer, address, port, nil)
}

// DialWithRetryCheck repeats the caller's signed-route check before retrying
// a stale pooled session. The callback must fail closed if the route to the
// target principal has been withdrawn while the old stream was waiting for an
// ACK.
func (c *Client) DialWithRetryCheck(ctx context.Context, peer meshid.Device, address netip.AddrPort, port uint16, retryCheck func() error) (net.Conn, error) {
	if c == nil || port == 0 || !address.IsValid() || address.Port() == 0 {
		return nil, errors.New("invalid pooled wendy net app dial")
	}
	for attempt := 0; attempt < 2; attempt++ {
		entry, reused, err := c.acquire(ctx, peer, address)
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
		if errors.Is(err, ErrDenied) || ctx.Err() != nil {
			c.release(entry)
			return nil, err
		}
		// Any failed port request makes this cached connection suspect. A
		// timeout from the local ACK bound may leave QUIC Context live even
		// though the remote process no longer owns the connection ID.
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

func (c *Client) acquire(ctx context.Context, peer meshid.Device, address netip.AddrPort) (*pooledSession, bool, error) {
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
		if entry != nil && entry.ready != nil {
			ready := entry.ready
			c.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}
		if entry != nil && entry.address == address && entry.conn != nil && entry.conn.Context().Err() == nil &&
			now.Before(entry.expires) && now.Sub(entry.born) < clientAgeLimit && now.Sub(entry.used) < clientIdleLimit {
			entry.active++
			entry.used = now
			c.mu.Unlock()
			return entry, true, nil
		}
		var oldConn *quic.Conn
		if entry != nil {
			delete(c.peers, peer)
			entry.retired = true
			if entry.active == 0 {
				oldConn = entry.conn
			}
		}
		newEntry := &pooledSession{peer: peer, address: address, ready: make(chan struct{}), born: now, used: now}
		ready := newEntry.ready
		c.peers[peer] = newEntry
		c.mu.Unlock()
		if oldConn != nil {
			_ = oldConn.CloseWithError(0, "wendy net app session replaced")
		}
		cfg, err := c.credentials.PeerTLSWithTickets(peer, ALPN, "app-quic")
		var conn *quic.Conn
		if err == nil {
			conn, err = c.dial(ctx, address, cfg, quicConfig())
		}
		if err == nil {
			newEntry.expires, err = limitSessionLifetime(c.credentials, conn)
		}
		c.mu.Lock()
		newEntry.conn, newEntry.err = conn, err
		newEntry.ready = nil
		valid := err == nil && !c.closed && c.peers[peer] == newEntry
		if valid {
			newEntry.active = 1
			c.sessions[newEntry] = struct{}{}
			go func() {
				<-conn.Context().Done()
				c.mu.Lock()
				delete(c.sessions, newEntry)
				c.mu.Unlock()
			}()
		} else {
			if c.peers[peer] == newEntry {
				delete(c.peers, peer)
			}
			newEntry.retired = true
		}
		close(ready)
		c.mu.Unlock()
		if !valid {
			if conn != nil {
				_ = conn.CloseWithError(0, "wendy net app session replaced")
			}
			if err != nil {
				return nil, false, fmt.Errorf("wendy net app QUIC dial: %w", err)
			}
			return nil, false, net.ErrClosed
		}
		return newEntry, false, nil
	}
}

func (c *Client) release(entry *pooledSession) {
	c.mu.Lock()
	entry.active--
	entry.used = time.Now()
	closeConn := entry.retired && entry.active == 0
	c.mu.Unlock()
	if closeConn && entry.conn != nil {
		_ = entry.conn.CloseWithError(0, "wendy net app session retired")
	}
}

func (c *Client) retire(peer meshid.Device, entry *pooledSession) {
	c.mu.Lock()
	if c.peers[peer] == entry {
		delete(c.peers, peer)
	}
	entry.retired = true
	closeConn := entry.active == 0
	c.mu.Unlock()
	if closeConn && entry.conn != nil {
		_ = entry.conn.CloseWithError(0, "wendy net app session failed")
	}
}
