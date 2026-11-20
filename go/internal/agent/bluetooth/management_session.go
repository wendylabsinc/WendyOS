package bluetooth

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

const maxFrameSize = 65536

// serveManagementStream is the production command loop, independent of the
// radio socket adapter so its TLS admission and framing can be tested directly.
func serveManagementStream(ctx context.Context, rawConn net.Conn, logger *zap.Logger, d *Dispatcher, tlsConfig *tls.Config) {
	tlsConn := tls.Server(rawConn, tlsConfig)
	defer tlsConn.Close()

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		logger.Warn("BLE mTLS handshake failed", zap.Error(err))
		return
	}

	// A successful operator handshake cannot keep management authority alive
	// after the presented credential expires. Closing also interrupts an idle read.
	if peers := tlsConn.ConnectionState().PeerCertificates; len(peers) != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, peers[0].NotAfter)
		defer cancel()
	}
	stop := context.AfterFunc(ctx, func() { _ = rawConn.Close() })
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Cancellation/credential expiry closes rawConn through AfterFunc.
		// A polling read timeout could discard part of a framed message and
		// reinterpret its remaining body as a new frame header.
		payload, err := readMessageFromConn(tlsConn)
		if err != nil {
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
