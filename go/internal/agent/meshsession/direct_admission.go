package meshsession

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// An upper connection has one initiating endpoint. Counting outgoing setup as
// well as established sockets at BOTH endpoints gives the reuse barrier a
// complete view without teaching TLS listeners about logical BLE epochs.
// Callers acquire before dialing and release only after the connection closes.
// Promotion's own sockets may conservatively count until their normal cleanup.
type directPeerAdmission struct {
	upper    int
	reserved chan struct{}
}

func (m *DirectManager) acquireUpper(ctx context.Context, peer meshid.Device) (func(), error) {
	for {
		m.mu.Lock()
		if m.closed || m.ctx.Err() != nil {
			m.mu.Unlock()
			return nil, net.ErrClosed
		}
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if m.admissions == nil {
			m.admissions = make(map[meshid.Device]*directPeerAdmission)
		}
		e := m.admissions[peer]
		if e == nil {
			e = &directPeerAdmission{}
			m.admissions[peer] = e
		}
		changed := e.reserved
		if !peer.IsValid() {
			for _, other := range m.admissions {
				if other.reserved != nil {
					changed = other.reserved
					break
				}
			}
		}
		if changed != nil {
			if e.upper == 0 && e.reserved == nil {
				delete(m.admissions, peer)
			}
			m.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-m.ctx.Done():
				return nil, net.ErrClosed
			}
		}
		e.upper++
		m.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				e.upper--
				if e.upper == 0 && e.reserved == nil && m.admissions[peer] == e {
					delete(m.admissions, peer)
				}
			})
		}, nil
	}
}

// reserveReuse is a nonblocking coordinator/participant proposal step. A busy
// participant must reply BUSY promptly; it must not hold the other endpoint's
// reservation while waiting for an application dependency to finish.
func (m *DirectManager) reserveReuse(peer meshid.Device) (func(), bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return nil, false
	}
	if current := m.peers[peer]; current != nil {
		select {
		case <-current.session.done:
		default:
			return nil, false
		}
	}
	if m.admissions == nil {
		m.admissions = make(map[meshid.Device]*directPeerAdmission)
	}
	if unknown := m.admissions[meshid.Device{}]; unknown != nil && unknown.upper != 0 {
		return nil, false
	}
	live := 0
	for _, entry := range m.peers {
		select {
		case <-entry.session.done:
		default:
			live++
		}
	}
	for _, gate := range m.admissions {
		if gate.reserved != nil {
			live++
		}
	}
	if m.cfg.MaxPeers > 0 && live >= m.cfg.MaxPeers {
		return nil, false
	}

	e := m.admissions[peer]
	if e != nil && (e.upper != 0 || e.reserved != nil) {
		return nil, false
	}
	if e == nil {
		e = &directPeerAdmission{}
		m.admissions[peer] = e
	}
	e.reserved = make(chan struct{})
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			close(e.reserved)
			e.reserved = nil
			if e.upper == 0 && m.admissions[peer] == e {
				delete(m.admissions, peer)
			}
		})
	}, true
}

// DialUpper tracks a separately owned upper transport across setup, suspension
// and closure. A waiting reuse proposal never revokes an existing connection.
// This is also used for native routed TCP/QUIC sockets outside the app pool.
func (m *DirectManager) DialUpper(ctx context.Context, peer meshid.Device, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	if !peer.IsValid() || dial == nil {
		return nil, errors.New("missing upper connection target")
	}
	release, err := m.acquireUpper(ctx, peer)
	if err != nil {
		return nil, err
	}
	conn, err := dial(ctx)
	if err != nil {
		release()
		return nil, err
	}
	return &directTrackedUpper{Conn: conn, release: release}, nil
}

type directTrackedUpper struct {
	net.Conn
	release func()
	once    sync.Once
	err     error
}

func (c *directTrackedUpper) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close(); c.release() })
	return c.err
}
func (c *directTrackedUpper) CloseWrite() error {
	if h, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return h.CloseWrite()
	}
	return errors.New("upper connection does not support half-close")
}
func (c *directTrackedUpper) CloseRead() error {
	if h, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return h.CloseRead()
	}
	return errors.New("upper connection does not support half-close")
}

// DialUpper keeps native routed sockets under the same peer admission gate as
// app TCP/UDP. The lease includes connection setup, not only established TLS.
func (c *Client) DialUpper(ctx context.Context, peer meshid.Device, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	if c == nil {
		return nil, net.ErrClosed
	}
	c.mu.Lock()
	m, closed := c.admission, c.closed
	c.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if m == nil {
		return dial(ctx)
	}
	return m.DialUpper(ctx, peer, dial)
}
func (c *Client) acquireApplication(ctx context.Context, peer meshid.Device) (func(), error) {
	c.mu.Lock()
	m, closed := c.admission, c.closed
	c.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if m == nil {
		return func() {}, nil
	}
	return m.acquireUpper(ctx, peer)
}

// DialUnclassifiedUpper conservatively gates all peer reuse if a routed native
// socket cannot yet be mapped to a device. It preserves ordinary routing while
// preventing an unclassified pending connection from disappearing from the
// quiescence proof. Normal signed-directory resolution uses DialUpper instead.
func (m *DirectManager) DialUnclassifiedUpper(ctx context.Context, dial func(context.Context) (net.Conn, error)) (net.Conn, error) {
	release, err := m.acquireUpper(ctx, meshid.Device{})
	if err != nil {
		return nil, err
	}
	conn, err := dial(ctx)
	if err != nil {
		release()
		return nil, err
	}
	return &directTrackedUpper{Conn: conn, release: release}, nil
}

func (m *DirectManager) applicationBusyLocked(peer meshid.Device) bool {
	for _, key := range []meshid.Device{peer, {}} {
		if e := m.admissions[key]; e != nil && (e.upper != 0 || e.reserved != nil) {
			return true
		}
	}
	return false
}

// SetAdmissionManager tracks independently opened upper streams without
// changing their protocol selection (for example the operator's explicit
// upper QUIC pool). Install once before the client begins dialing.
func (c *Client) SetAdmissionManager(m *DirectManager) error {
	if m == nil || m.cfg.Credentials != c.credentials {
		return errors.New("admission manager credentials differ from app client")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.admission != nil || len(c.peers) != 0 || len(c.tlsApps) != 0 || len(c.tlsDials) != 0 || len(c.udpSessions) != 0 {
		return errors.New("admission manager must be installed before client use")
	}
	c.admission = m
	return nil
}
