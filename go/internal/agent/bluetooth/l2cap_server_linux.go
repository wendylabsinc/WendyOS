//go:build linux

package bluetooth

import (
	"context"
	"encoding/binary"
	"fmt"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

const (
	wendyL2CAPPSM = 128
	maxFrameSize  = 65536
)

func startL2CAPServer(ctx context.Context, logger *zap.Logger, d *Dispatcher) error {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET, unix.BTPROTO_L2CAP)
	if err != nil {
		return fmt.Errorf("l2cap socket: %w", err)
	}

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		unix.Close(fd)
		return fmt.Errorf("l2cap setsockopt: %w", err)
	}

	// SockaddrL2 implements unix.Sockaddr for AF_BLUETOOTH/L2CAP.
	// PSM=128 (0x0080) is the Wendy LE CoC PSM; AddrType=BDADDR_LE_PUBLIC tells
	// BlueZ this is an LE CoC socket (not Classic BT), which is required for
	// CoreBluetooth's openL2CAPChannel: to get a response.
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
			// Poll the listener fd with a 1 s timeout so we notice ctx cancellation.
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

			connFd, _, err := unix.Accept(fd)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					logger.Warn("l2cap accept error", zap.Error(err))
					continue
				}
			}
			go serveL2CAPConn(ctx, connFd, logger, d)
		}
	}()

	return nil
}

func serveL2CAPConn(ctx context.Context, fd int, logger *zap.Logger, d *Dispatcher) {
	defer unix.Close(fd)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Poll with a 30 s timeout so we can re-check context cancellation.
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfd, 30_000)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		if n == 0 {
			continue
		}

		// Read one complete length-prefixed message (single L2CAP SDU).
		payload, err := readMessage(fd)
		if err != nil {
			return
		}

		// Decode command.
		var cmd agentpb.BluetoothCommand
		if err := proto.Unmarshal(payload, &cmd); err != nil {
			writeErrFrame(fd, logger, "malformed protobuf")
			return
		}

		// Dispatch and send response.
		resp := d.Dispatch(ctx, &cmd)
		respBytes, err := proto.Marshal(resp)
		if err != nil {
			writeErrFrame(fd, logger, "marshal error")
			return
		}
		if err := writeFrame(fd, respBytes); err != nil {
			return
		}
	}
}

// readMessage reads one complete length-prefixed message from fd.
// On SEQPACKET sockets each unix.Read returns exactly one SDU.
func readMessage(fd int) ([]byte, error) {
	buf := make([]byte, maxFrameSize)
	n, err := unix.Read(fd, buf)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("connection closed")
	}
	if n < 2 {
		return nil, fmt.Errorf("frame too short: %d bytes", n)
	}
	msgLen := binary.BigEndian.Uint16(buf[:2])
	if int(msgLen) != n-2 {
		return nil, fmt.Errorf("frame length mismatch: header=%d actual=%d", msgLen, n-2)
	}
	return buf[2 : 2+msgLen], nil
}

// writeFrame writes a 2-byte big-endian length prefix followed by data as a single write.
func writeFrame(fd int, data []byte) error {
	if len(data) > maxFrameSize-2 {
		return fmt.Errorf("frame too large: %d bytes", len(data))
	}
	frame := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(data)))
	copy(frame[2:], data)
	_, err := unix.Write(fd, frame)
	return err
}

// writeErrFrame sends an error response frame and returns.
func writeErrFrame(fd int, logger *zap.Logger, msg string) {
	resp := errResp(msg)
	if b, err := proto.Marshal(resp); err == nil {
		if wErr := writeFrame(fd, b); wErr != nil {
			logger.Debug("l2cap write error frame failed", zap.Error(wErr))
		}
	}
}
