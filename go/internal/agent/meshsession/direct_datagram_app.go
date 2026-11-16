package meshsession

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// directDatagramApp adapts the retained association to the ordinary application
// message API. The original conn survives both BLE replacement and promotion.
// Reservations cover the receive scratch plus two bounded writer messages.
type directDatagramApp struct {
	conn     *directstream.DatagramConn
	endpoint *udpmessage.Endpoint
	maximum  int
	readMu   udpReceiveLock
	once     sync.Once
	done     chan struct{}
}

// Success transfers conn ownership; failure leaves it with the caller.
func newDirectDatagramApp(conn *directstream.DatagramConn, peer meshid.Device, maximum uint16, opts UDPOptions) (*directDatagramApp, error) {
	if conn == nil || maximum == 0 || opts.Manager == nil {
		return nil, ErrDenied
	}
	maximum = min(maximum, uint16(conn.Maximum()))
	endpoint := opts.Endpoint
	if endpoint != nil && endpoint.Manager() != opts.Manager {
		return nil, ErrDenied
	}
	var err error
	if endpoint == nil {
		guard := opts.Guard
		if guard == nil {
			guard = func(send func() error) error { return send() }
		}
		endpoint, err = opts.Manager.Open(peer, guard)
		if err != nil {
			return nil, err
		}
	}
	f := &directDatagramApp{conn: conn, endpoint: endpoint, maximum: int(maximum), done: make(chan struct{})}
	// This is the same transport allowance used by ordinary TLS UDP. It is
	// independent of the direct actor's TX/assembly/ready reservations.
	if err = endpoint.ReserveSocketBytes(3 * (int(maximum) + 2)); err != nil {
		endpoint.Close()
		return nil, err
	}
	failed, err := attachQueuedUDPTransport(endpoint, func(payload []byte) error {
		if len(payload) > int(maximum) {
			return syscall.EMSGSIZE
		}
		if err := conn.SetWriteDeadline(time.Now().Add(udpIdleTimeout)); err != nil {
			return err
		}
		n, err := conn.Write(payload)
		if err == nil && n != len(payload) {
			err = io.ErrShortWrite
		}
		return err
	})
	if err != nil {
		endpoint.Close()
		return nil, err
	}
	go func() {
		select {
		case <-conn.Done():
		case <-endpoint.Done():
		case <-opts.Done:
		case <-failed:
		}
		f.Close()
	}()
	return f, nil
}
func (f *directDatagramApp) Close() error {
	f.once.Do(func() { f.conn.Close(); f.endpoint.Close(); close(f.done) })
	return nil
}
func (f *directDatagramApp) Send(p []byte) error {
	if len(p) > f.maximum {
		return syscall.EMSGSIZE
	}
	return localUDPError(f.endpoint.Send(p))
}
func (f *directDatagramApp) Receive(ctx context.Context) (*udpmessage.Message, error) {
	if err := f.readMu.lock(ctx, f.done); err != nil {
		return nil, localUDPError(err)
	}
	defer f.readMu.unlock()
	buffer := make([]byte, f.maximum+1)
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
		n, err := f.conn.Read(buffer)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return nil, err
		}
		if n > f.maximum {
			continue
		}
		var message *udpmessage.Message
		err = f.endpoint.WithGuard(func() error { var err error; message, err = f.endpoint.RetainReceived(buffer[:n]); return err })
		if errors.Is(err, udpmessage.ErrCapacity) {
			continue
		}
		return message, localUDPError(err)
	}
}

// Only absence/disabled policy or never-admitted promotion permits fallback.
// Any OPEN refusal, timeout, or adapter failure belongs to the retained epoch.
func (c *Client) dialDirectDatagram(ctx context.Context, entry *udpSession, peer meshid.Device, port uint16, opts UDPOptions) (*directDatagramApp, bool, error) {
	c.mu.Lock()
	m, closed := c.direct, c.closed
	c.mu.Unlock()
	if closed {
		return nil, true, net.ErrClosed
	}
	fallback := func() (*directDatagramApp, bool, error) {
		c.mu.Lock()
		entry.direct = false
		retired := entry.retired || c.closed
		c.mu.Unlock()
		if retired {
			return nil, true, net.ErrClosed
		}
		return nil, false, nil
	}
	if m == nil || m.cfg.DatagramBytes == 0 {
		return fallback()
	}
	raw, err := m.Dial(ctx, peer, directstream.OpenRequest{Service: directstream.ServiceUDP, Port: port, DatagramSize: m.cfg.DatagramMaximum})
	if errors.Is(err, ErrNoDirectSession) || errors.Is(err, ErrDirectUseUpper) {
		return fallback()
	}
	if err != nil {
		return nil, true, err
	}
	conn, ok := raw.(*directstream.DatagramConn)
	if !ok {
		raw.Close()
		return nil, true, ErrUDPUnsupported
	}
	app, err := newDirectDatagramApp(conn, peer, m.cfg.DatagramMaximum, opts)
	if err != nil {
		conn.Close()
	}
	return app, true, err
}
