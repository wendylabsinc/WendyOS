//go:build linux

package bluetooth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	wendyL2CAPPSM = 128
)

type l2capStreamConn struct {
	ioMu      sync.RWMutex
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
	fd        int
	rbuf      []byte // bytes remaining from the last received SDU
	sndMTU    int    // peer-negotiated maximum SDU; zero uses the LE minimum

	readDeadline  l2capDeadline
	writeDeadline l2capDeadline
}

func (c *l2capStreamConn) Read(b []byte) (int, error) {
	c.ioMu.RLock()
	defer c.ioMu.RUnlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	if len(c.rbuf) > 0 {
		n := copy(b, c.rbuf)
		c.rbuf = c.rbuf[n:]
		return n, nil
	}
	msg := make([]byte, 65536)
	n, err := retryL2CAPInterrupted(func() (int, error) {
		if err := c.readDeadline.prepare(c.fd, unix.SO_RCVTIMEO); err != nil {
			return 0, err
		}
		return unix.Read(c.fd, msg)
	})
	if err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return 0, &net.OpError{Op: "read", Net: "l2cap", Err: &l2capTimeoutError{}}
		}
		return 0, &net.OpError{Op: "read", Net: "l2cap", Err: err}
	}
	if n == 0 {
		return 0, io.EOF
	}
	copied := copy(b, msg[:n])
	if copied < n {
		c.rbuf = make([]byte, n-copied)
		copy(c.rbuf, msg[copied:n])
	}
	return copied, nil
}

// One write on SOCK_SEQPACKET is one SDU. A TLS record (in particular an
// ML-DSA certificate flight) can exceed the negotiated MTU. Split the stream
// just as the central transport does; the reader already reassembles SDUs.
func (c *l2capStreamConn) Write(b []byte) (int, error) {
	c.ioMu.RLock()
	defer c.ioMu.RUnlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	mtu := c.sndMTU
	if mtu < 23 {
		mtu = 23
	}
	written := 0
	for written < len(b) {
		end := min(written+mtu, len(b))
		n, err := retryL2CAPInterrupted(func() (int, error) {
			if err := c.writeDeadline.prepare(c.fd, unix.SO_SNDTIMEO); err != nil {
				return 0, err
			}
			return unix.Write(c.fd, b[written:end])
		})
		written += n
		if err != nil {
			return written, &net.OpError{Op: "write", Net: "l2cap", Err: err}
		}
		if n == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

// Go's async preemption can interrupt these raw socket syscalls. A signal is
// not a peer disconnect: retry it without discarding the encrypted session.
func retryL2CAPInterrupted(operation func() (int, error)) (int, error) {
	for {
		n, err := operation()
		if !errors.Is(err, unix.EINTR) {
			return n, err
		}
	}
}

func (c *l2capStreamConn) Close() error {
	c.closeOnce.Do(func() {
		// Wake a blocked read when the operator credential expires or the
		// server stops, before releasing the descriptor for reuse.
		c.closed.Store(true)
		_ = unix.Shutdown(c.fd, unix.SHUT_RDWR)
		c.ioMu.Lock()
		defer c.ioMu.Unlock()
		c.closeErr = unix.Close(c.fd)
	})
	return c.closeErr
}

func (c *l2capStreamConn) LocalAddr() net.Addr  { return l2capAddr("local") }
func (c *l2capStreamConn) RemoteAddr() net.Addr { return l2capAddr("remote") }

func (c *l2capStreamConn) SetDeadline(t time.Time) error {
	re := c.SetReadDeadline(t)
	we := c.SetWriteDeadline(t)
	if re != nil {
		return re
	}
	return we
}

func (c *l2capStreamConn) SetReadDeadline(t time.Time) error {
	return c.readDeadline.set(c.fd, unix.SO_RCVTIMEO, t)
}

func (c *l2capStreamConn) SetWriteDeadline(t time.Time) error {
	return c.writeDeadline.set(c.fd, unix.SO_SNDTIMEO, t)
}

// Socket timeouts are relative to each syscall. Retain the absolute deadline
// so an EINTR retry waits only for the remaining time, not the full timeout.
type l2capDeadline struct {
	mu sync.Mutex
	at time.Time
}

func (d *l2capDeadline) set(fd, option int, t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tv := l2capTimeval(t)
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, option, &tv); err != nil {
		return err
	}
	d.at = t
	return nil
}

func (d *l2capDeadline) prepare(fd, option int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.at.IsZero() {
		return nil
	}
	if !time.Now().Before(d.at) {
		return &l2capTimeoutError{}
	}
	tv := l2capTimeval(d.at)
	return unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, option, &tv)
}

type l2capAddr string

func (a l2capAddr) Network() string { return "l2cap" }
func (a l2capAddr) String() string  { return string(a) }

type l2capTimeoutError struct{}

func (e *l2capTimeoutError) Error() string   { return "i/o timeout" }
func (e *l2capTimeoutError) Timeout() bool   { return true }
func (e *l2capTimeoutError) Temporary() bool { return true }

// l2capTimeval converts an absolute deadline to a SO_RCVTIMEO/SO_SNDTIMEO
// timeval. A zero time clears the timeout.
func l2capTimeval(t time.Time) unix.Timeval {
	if t.IsZero() {
		return unix.Timeval{}
	}
	d := time.Until(t)
	if d <= 0 {
		return unix.Timeval{Sec: 0, Usec: 1}
	}
	return unix.NsecToTimeval(d.Nanoseconds())
}

// startL2CAPServer binds an LE CoC L2CAP socket and dispatches protobuf-framed
// commands over mTLS. tlsConfig must be non-nil; the server refuses to start
// without a provisioned certificate so the BLE channel is never open without auth.
func startL2CAPServer(ctx context.Context, logger *zap.Logger, d *Dispatcher, tlsConfig *tls.Config) error {
	if tlsConfig == nil {
		return fmt.Errorf("BLE L2CAP server requires mTLS: device is not provisioned")
	}

	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, unix.BTPROTO_L2CAP)
	if err != nil {
		return fmt.Errorf("l2cap socket: %w", err)
	}

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		unix.Close(fd)
		return fmt.Errorf("l2cap setsockopt: %w", err)
	}

	// PSM=128 (0x0080) is the Wendy LE CoC PSM; AddrType=BDADDR_LE_PUBLIC tells
	// BlueZ this is an LE CoC socket (not Classic BT).
	sa := &unix.SockaddrL2{PSM: wendyL2CAPPSM, AddrType: unix.BDADDR_LE_PUBLIC}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return fmt.Errorf("l2cap bind: %w", err)
	}

	if err := unix.Listen(fd, 8); err != nil {
		unix.Close(fd)
		return fmt.Errorf("l2cap listen: %w", err)
	}

	logger.Info("BLE L2CAP server listening", zap.Uint16("psm", wendyL2CAPPSM))

	go func() {
		defer unix.Close(fd)
		for {
			pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			n, err := unix.Poll(pfd, 1000)
			if err != nil {
				if err == unix.EINTR {
					continue
				}
				logger.Error("l2cap poll error", zap.Error(err))
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			if n == 0 {
				continue
			}

			connFd, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					logger.Warn("l2cap accept error", zap.Error(err))
					continue
				}
			}
			go serveL2CAPConn(ctx, connFd, logger, d, tlsConfig)
		}
	}()

	return nil
}

func serveL2CAPConn(ctx context.Context, fd int, logger *zap.Logger, d *Dispatcher, tlsConfig *tls.Config) {
	// Wrap the raw fd as a stream-like net.Conn for TLS.
	// net.FileConn cannot be used here: it calls getsockname internally, which
	// returns EAFNOSUPPORT for AF_BLUETOOTH sockets. l2capStreamConn bypasses
	// that and also converts SEQPACKET message semantics to stream semantics by
	// buffering received SDUs so TLS partial reads don't discard data.
	rawConn := &l2capStreamConn{fd: fd}
	// SOL_BLUETOOTH / BT_SNDMTU, shared with the Linux central transport.
	if mtu, err := unix.GetsockoptInt(fd, 274, 12); err == nil && mtu >= 23 {
		rawConn.sndMTU = mtu
	}

	serveManagementStream(ctx, rawConn, logger, d, tlsConfig)
}
