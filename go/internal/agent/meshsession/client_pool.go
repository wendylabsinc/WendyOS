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
)

const (
	clientIdleLimit = 45 * time.Second
	clientAgeLimit  = 10 * time.Minute
	// A reused QUIC connection may survive a remote agent restart without a
	// Babel withdrawal. Bound its app-port ACK below the QUIC idle timeout,
	// while leaving cold handshakes their full caller budget on slow BLE paths.
	pooledPortACKLimit = 20 * time.Second
)

// Client reuses one authenticated QUIC session per destination asset. Each
// flow opens a separately authorized stream. Build a new Client whenever
// credentials or trust change; Close stops the pool at agent shutdown.
type Client struct {
	credentials  *localmesh.Credentials
	mu           sync.Mutex
	peers        map[int32]*pooledSession
	closed       bool
	stop         chan struct{}
	done         chan struct{}
	dial         func(context.Context, netip.AddrPort, *tls.Config, *quic.Config) (*quic.Conn, error)
	portACKLimit time.Duration // test override; zero uses pooledPortACKLimit
}

type pooledSession struct {
	address netip.AddrPort
	ready   chan struct{}
	conn    *quic.Conn
	err     error
	born    time.Time
	used    time.Time
	active  int
	retired bool
}

func NewClient(credentials *localmesh.Credentials) (*Client, error) {
	if credentials == nil {
		return nil, errors.New("missing mesh app credentials")
	}
	c := &Client{credentials: credentials, peers: make(map[int32]*pooledSession), stop: make(chan struct{}), done: make(chan struct{}),
		dial: func(ctx context.Context, endpoint netip.AddrPort, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
			return quic.DialAddr(ctx, endpoint.String(), tlsConfig, config)
		}}
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
		for asset, entry := range c.peers {
			if entry.ready != nil || entry.active != 0 ||
				entry.conn != nil && entry.conn.Context().Err() == nil && now.Sub(entry.used) < clientIdleLimit && now.Sub(entry.born) < clientAgeLimit {
				continue
			}
			delete(c.peers, asset)
			entry.retired = true
			if entry.conn != nil {
				stale = append(stale, entry.conn)
			}
		}
		c.mu.Unlock()
		for _, conn := range stale {
			_ = conn.CloseWithError(0, "mesh app session idle")
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
	var conns []*quic.Conn
	for _, entry := range c.peers {
		entry.retired = true
		if entry.conn != nil {
			conns = append(conns, entry.conn)
		}
	}
	c.peers = make(map[int32]*pooledSession)
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.CloseWithError(0, "mesh app client stopped")
	}
	<-c.done
	return nil
}

// Invalidate closes a cached session as soon as the signed manifest or Babel
// route to that peer is withdrawn. A QUIC connection can otherwise remain
// apparently live until its idle timeout, and a restored route would reuse
// that dead path for every new app stream.
func (c *Client) Invalidate(peer int32) {
	if c == nil {
		return
	}
	c.mu.Lock()
	conn := c.invalidateLocked(peer)
	c.mu.Unlock()
	if conn != nil {
		_ = conn.CloseWithError(0, "mesh app route withdrawn")
	}
}

// PruneRoutes applies one authenticated node snapshot to all cached app
// sessions. The caller invokes it periodically so a route withdrawal without
// an app dial still closes a pooled QUIC connection before a later heal.
func (c *Client) PruneRoutes(snapshot localmesh.NodeSnapshot) {
	if c == nil {
		return
	}
	var stale []*quic.Conn
	c.mu.Lock()
	for peer, entry := range c.peers {
		address, err := Resolve(snapshot, c.credentials.Org, peer)
		if err != nil || address != entry.address {
			if conn := c.invalidateLocked(peer); conn != nil {
				stale = append(stale, conn)
			}
		}
	}
	c.mu.Unlock()
	for _, conn := range stale {
		_ = conn.CloseWithError(0, "mesh app route withdrawn")
	}
}

// Caller holds c.mu. Close is done after unlocking, including for sessions
// with active streams: those streams have lost their authorized data path.
func (c *Client) invalidateLocked(peer int32) *quic.Conn {
	entry := c.peers[peer]
	if entry == nil {
		return nil
	}
	delete(c.peers, peer)
	entry.retired = true
	return entry.conn
}

// Dial requires a fresh caller-side manifest and route check before each call.
// The passed endpoint must therefore come from Resolve at dial time.
func (c *Client) Dial(ctx context.Context, peer int32, address netip.AddrPort, port uint16) (net.Conn, error) {
	return c.DialWithRetryCheck(ctx, peer, address, port, nil)
}

// DialWithRetryCheck repeats the caller's signed-route check before retrying
// a stale pooled session. The callback must fail closed if the route or signed
// manifest has been withdrawn while the old stream was waiting for an ACK.
func (c *Client) DialWithRetryCheck(ctx context.Context, peer int32, address netip.AddrPort, port uint16, retryCheck func() error) (net.Conn, error) {
	if c == nil || port == 0 || !address.IsValid() || address.Port() == 0 {
		return nil, errors.New("invalid pooled mesh app dial")
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

func (c *Client) acquire(ctx context.Context, peer int32, address netip.AddrPort) (*pooledSession, bool, error) {
	if _, _, err := localmesh.Addresses(c.credentials.Org, peer); err != nil || peer == c.credentials.Asset {
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
			now.Sub(entry.born) < clientAgeLimit && now.Sub(entry.used) < clientIdleLimit {
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
		newEntry := &pooledSession{address: address, ready: make(chan struct{}), born: now, used: now}
		ready := newEntry.ready
		c.peers[peer] = newEntry
		c.mu.Unlock()
		if oldConn != nil {
			_ = oldConn.CloseWithError(0, "mesh app session replaced")
		}
		cfg, err := c.credentials.PeerTLSWithTickets(peer, ALPN, "app-quic")
		var conn *quic.Conn
		if err == nil {
			conn, err = c.dial(ctx, address, cfg, quicConfig())
		}
		c.mu.Lock()
		newEntry.conn, newEntry.err = conn, err
		newEntry.ready = nil
		valid := err == nil && !c.closed && c.peers[peer] == newEntry
		if valid {
			newEntry.active = 1
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
				_ = conn.CloseWithError(0, "mesh app session replaced")
			}
			if err != nil {
				return nil, false, fmt.Errorf("mesh app QUIC dial: %w", err)
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
		_ = entry.conn.CloseWithError(0, "mesh app session retired")
	}
}

func (c *Client) retire(peer int32, entry *pooledSession) {
	c.mu.Lock()
	if c.peers[peer] == entry {
		delete(c.peers, peer)
	}
	entry.retired = true
	closeConn := entry.active == 0
	c.mu.Unlock()
	if closeConn && entry.conn != nil {
		_ = entry.conn.CloseWithError(0, "mesh app session failed")
	}
}
