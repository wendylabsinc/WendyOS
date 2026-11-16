package meshsession

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// directQUICDatagramConn wraps an already authenticated, bound multiplexer
// association. Payloads use QUIC DATAGRAM and the existing bounded fragment
// scheduler; its reliable stream carries binding/lifetime only, never payload.
type directQUICDatagramConn struct {
	owner                       net.Conn
	flow                        *muxUDPFlow
	maximum                     int
	readMu                      sync.Mutex
	mu                          sync.Mutex
	readDeadline, writeDeadline time.Time
	changed                     chan struct{}
	once                        sync.Once
}

func newDirectQUICDatagramConn(owner net.Conn, flow *muxUDPFlow, maximum uint16) *directQUICDatagramConn {
	c := &directQUICDatagramConn{owner: owner, flow: flow, maximum: int(maximum), changed: make(chan struct{})}
	go func() { <-flow.done; c.Close() }()
	return c
}
func (c *directQUICDatagramConn) LocalAddr() net.Addr  { return c.owner.LocalAddr() }
func (c *directQUICDatagramConn) RemoteAddr() net.Addr { return c.owner.RemoteAddr() }
func (c *directQUICDatagramConn) Close() error {
	c.once.Do(func() { c.flow.close(); c.owner.Close() })
	return nil
}
func (c *directQUICDatagramConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		c.mu.Lock()
		deadline, changed := c.readDeadline, c.changed
		c.mu.Unlock()
		select {
		case <-c.flow.done:
			return 0, net.ErrClosed
		default:
		}
		var timeout <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			timeout = timer.C
		}
		stop := func() {
			if timer != nil {
				timer.Stop()
			}
		}
		select {
		case <-c.flow.done:
			stop()
			return 0, net.ErrClosed
		case <-changed:
			stop()
			continue
		case <-timeout:
			continue
		case message := <-c.flow.messages:
			stop()
			if len(message.Payload) > c.maximum {
				message.Release()
				continue
			}
			n := copy(p, message.Payload)
			message.Release()
			return n, nil
		}
	}
}
func (c *directQUICDatagramConn) Write(p []byte) (int, error) {
	if len(p) > c.maximum {
		return 0, syscall.EMSGSIZE
	}
	c.mu.Lock()
	deadline := c.writeDeadline
	c.mu.Unlock()
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	if err := c.flow.endpoint.Send(p); err != nil {
		return 0, localUDPError(err)
	}
	return len(p), nil
}
func (c *directQUICDatagramConn) deadline(t time.Time, read, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if read {
		c.readDeadline = t
	}
	if write {
		c.writeDeadline = t
	}
	close(c.changed)
	c.changed = make(chan struct{})
	return nil
}
func (c *directQUICDatagramConn) SetDeadline(t time.Time) error { return c.deadline(t, true, true) }
func (c *directQUICDatagramConn) SetReadDeadline(t time.Time) error {
	return c.deadline(t, true, false)
}
func (c *directQUICDatagramConn) SetWriteDeadline(t time.Time) error {
	return c.deadline(t, false, true)
}

// This factory is invoked only after the full exporter/OPEN binding succeeds.
// Peer is the retained session's authenticated identity, never a wire address.
type directDatagramFactory interface {
	openDatagrams(net.Conn, meshid.Device, uint16) (net.Conn, error)
}
