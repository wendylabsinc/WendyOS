//go:build linux

package bluetooth

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
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
	// Wrap the raw fd as a net.Conn so the TLS library can use it.
	// net.FileConn dups the fd and registers it with the runtime poller, giving
	// proper SetDeadline support. We close the os.File to release the original fd.
	f := os.NewFile(uintptr(fd), "l2cap")
	rawConn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		logger.Warn("BLE l2cap conn wrap failed", zap.Error(err))
		return
	}

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
