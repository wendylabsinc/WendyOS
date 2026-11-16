package meshsession

import (
	"net"
	"sync"
	"syscall"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// directTLSDatagramConn is used only after the upper stream has authenticated
// and completed the retained OPEN binding. Its payload format matches WAU3 and
// Lite: a big-endian uint16 length followed by one complete datagram. No direct
// DATA/ACK metadata survives promotion. Deadlines belong to the application.
// A timed-out read retains its partial frame; a failed write closes the stream
// because an ambiguously submitted datagram cannot be retried as a new frame.
type directTLSDatagramConn struct {
	endpoint *udpmessage.Endpoint
	once     sync.Once
	closed   bool // protected by readMu
	net.Conn
	readMu, writeMu sync.Mutex
	maximum         int
	reader          tlsDatagramReader
}

func newDirectTLSDatagramConn(raw net.Conn, maximum uint16) (*directTLSDatagramConn, error) {
	if raw == nil || maximum == 0 {
		return nil, syscall.EINVAL
	}
	return &directTLSDatagramConn{Conn: raw, maximum: int(maximum), reader: tlsDatagramReader{payload: make([]byte, int(maximum))}}, nil
}

// Reserve before allocating. This is separate from the guest receive scratch,
// queued writers, and direct reassembly storage that overlaps during cutover.
func newAccountedDirectTLSDatagramConn(raw net.Conn, maximum uint16, manager *udpmessage.Manager, peer meshid.Device) (*directTLSDatagramConn, error) {
	if raw == nil || maximum == 0 || manager == nil {
		return nil, syscall.EINVAL
	}
	endpoint, err := manager.Open(peer, func(action func() error) error { return action() })
	if err != nil {
		return nil, err
	}
	if err = endpoint.ReserveSocketBytes(int(maximum)); err != nil {
		endpoint.Close()
		return nil, err
	}
	c, err := newDirectTLSDatagramConn(raw, maximum)
	if err != nil {
		endpoint.Close()
		return nil, err
	}
	c.endpoint = endpoint
	go func() { <-endpoint.Done(); c.Close() }()
	return c, nil
}
func (c *directTLSDatagramConn) Close() error {
	c.once.Do(func() {
		// Unblock a reader before taking its lock; release only after it has stopped
		// using the retained partial frame.
		c.Conn.Close()
		c.readMu.Lock()
		c.closed = true
		c.reader.payload = nil
		if c.endpoint != nil {
			c.endpoint.Close()
		}
		c.readMu.Unlock()
	})
	return nil
}

func (c *directTLSDatagramConn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	for {
		payload, complete, err := c.reader.read(c.Conn, c.maximum)
		if err != nil {
			if err == syscall.EMSGSIZE {
				c.Conn.Close()
			}
			return 0, err
		}
		if complete {
			return copy(b, payload), nil
		}
	}
}

func (c *directTLSDatagramConn) Write(b []byte) (int, error) {
	if len(b) > c.maximum {
		return 0, syscall.EMSGSIZE
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := writeTLSDatagramFrame(c.Conn, b); err != nil {
		c.Conn.Close()
		return 0, err
	}
	return len(b), nil
}
