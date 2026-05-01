//go:build linux

package bluetooth

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const (
	wendyL2CAPPSM = 128
	maxFrameSize  = 65536
)

// l2capStreamConn wraps a raw LE CoC L2CAP file descriptor as a net.Conn with
// stream semantics. LE CoC sockets are SOCK_SEQPACKET (message-based), so each
// unix.Read returns exactly one complete L2CAP SDU. TLS expects stream semantics
// and reads arbitrary byte counts, so we buffer received SDUs to allow partial
// reads without discarding data.
//
// net.FileConn cannot be used: it calls getsockname internally, which fails for
// AF_BLUETOOTH with "address family not supported by protocol".
type l2capStreamConn struct {
	fd   int
	rbuf []byte // bytes remaining from the last received SDU
}

func (c *l2capStreamConn) Read(b []byte) (int, error) {
	if len(c.rbuf) > 0 {
		n := copy(b, c.rbuf)
		c.rbuf = c.rbuf[n:]
		return n, nil
	}
	msg := make([]byte, 65536)
	n, err := unix.Read(c.fd, msg)
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

func (c *l2capStreamConn) Write(b []byte) (int, error) {
	n, err := unix.Write(c.fd, b)
	if err != nil {
		return n, &net.OpError{Op: "write", Net: "l2cap", Err: err}
	}
	return n, nil
}

func (c *l2capStreamConn) Close() error { return unix.Close(c.fd) }

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
	tv := l2capTimeval(t)
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

func (c *l2capStreamConn) SetWriteDeadline(t time.Time) error {
	tv := l2capTimeval(t)
	return unix.SetsockoptTimeval(c.fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
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

	tlsConn := tls.Server(rawConn, tlsConfig)
	defer tlsConn.Close()

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		logger.Warn("BLE mTLS handshake failed", zap.Error(err))
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-check context every 30 s via a read deadline.
		if err := tlsConn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return
		}

		payload, err := readMessageFromConn(tlsConn)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			return
		}

		if err := tlsConn.SetReadDeadline(time.Time{}); err != nil {
			return
		}

		var cmd agentpb.BluetoothCommand
		if err := proto.Unmarshal(payload, &cmd); err != nil {
			writeErrFrameToConn(tlsConn, logger, "malformed protobuf")
			return
		}

		resp := d.Dispatch(ctx, &cmd)
		respBytes, err := proto.Marshal(resp)
		if err != nil {
			writeErrFrameToConn(tlsConn, logger, "marshal error")
			return
		}
		if err := writeFrameToConn(tlsConn, respBytes); err != nil {
			return
		}
	}
}

// readMessageFromConn reads one length-prefixed message from a stream reader.
// The format is: 2-byte big-endian length | payload bytes.
func readMessageFromConn(r io.Reader) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	msgLen := binary.BigEndian.Uint16(header[:])
	if msgLen == 0 {
		return nil, fmt.Errorf("empty message")
	}
	if int(msgLen) > maxFrameSize-2 {
		return nil, fmt.Errorf("message too large: %d bytes", msgLen)
	}
	body := make([]byte, msgLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// writeFrameToConn writes a 2-byte big-endian length prefix followed by data.
func writeFrameToConn(w io.Writer, data []byte) error {
	if len(data) > maxFrameSize-2 {
		return fmt.Errorf("frame too large: %d bytes", len(data))
	}
	frame := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(data)))
	copy(frame[2:], data)
	_, err := w.Write(frame)
	return err
}

func writeErrFrameToConn(w io.Writer, logger *zap.Logger, msg string) {
	resp := errResp(msg)
	if b, err := proto.Marshal(resp); err == nil {
		if wErr := writeFrameToConn(w, b); wErr != nil {
			logger.Debug("l2cap write error frame failed", zap.Error(wErr))
		}
	}
}
