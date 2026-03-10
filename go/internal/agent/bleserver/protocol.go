package bleserver

import (
	"encoding/binary"
	"fmt"
	"net"
)

// maxMessageSize is the maximum BLE L2CAP message size: 2-byte length prefix + max uint16 payload.
const maxMessageSize = 2 + 65535

// readMessage reads a single length-prefixed protobuf message from an L2CAP SOCK_SEQPACKET connection.
// Each Read() returns exactly one complete L2CAP message (message boundaries are preserved).
func readMessage(conn net.Conn) ([]byte, error) {
	buf := make([]byte, maxMessageSize)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < 2 {
		return nil, fmt.Errorf("message too short: %d bytes", n)
	}
	msgLen := binary.BigEndian.Uint16(buf[:2])
	if int(msgLen) > n-2 {
		return nil, fmt.Errorf("length mismatch: header says %d, got %d payload bytes", msgLen, n-2)
	}
	return buf[2 : 2+msgLen], nil
}

// writeMessage writes a single length-prefixed protobuf message to an L2CAP SOCK_SEQPACKET connection.
// A single Write() produces a single L2CAP message.
func writeMessage(conn net.Conn, data []byte) error {
	if len(data) > 65535 {
		return fmt.Errorf("message too large: %d bytes (max 65535)", len(data))
	}
	frame := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(frame[:2], uint16(len(data)))
	copy(frame[2:], data)
	_, err := conn.Write(frame)
	return err
}
