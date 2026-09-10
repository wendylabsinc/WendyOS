package liteclient

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"go.bug.st/serial"
	"google.golang.org/protobuf/proto"
)

const escapeChar = 0x10 // CTRL-P, aka DLE (Data Link Escape)

// WendyCom frame header: magic, version, four reserved bytes, then a 16-bit
// big-endian body length. directLink owns this framing — the cloud tunnel does
// not use it, because there the broker frames instead.
const (
	headerMagic   = 0xA5
	headerVersion = 0x02
	headerSize    = 8
)

// maxTLSRecordSize is the largest TLS record we allow ourselves to emit. The
// device rejects any record whose plaintext exceeds its
// MBEDTLS_SSL_IN_CONTENT_LEN and has no way to ask for a smaller one — TLS can
// negotiate this (RFC 6066, RFC 8449) but crypto/tls implements neither
// extension — so bounding our own writes is the only control available.
//
// It has to be enforced rather than assumed: crypto/tls sizes a record at
// min(len(write), maxPayload), and maxPayload jumps from one TCP segment to 16
// KiB once a connection has carried 128 KiB. Without a cap, an oversized
// message would succeed on a fresh session and kill the link on a long-lived
// one.
const maxTLSRecordSize = 8192

// directLink frames WendyComMessages with the 8-byte link header over a
// direct connection (TCP-TLS or serial). The header channel byte stays 0:
// direct links always use the default channel.
type directLink struct {
	conn     io.ReadWriteCloser
	isSerial bool
	writeMu  sync.Mutex // serializes frames across command goroutines
}

// newDirectLink frames WendyCom over an established byte stream: TCP-TLS, or
// TLS over a BLE L2CAP channel.
func newDirectLink(conn io.ReadWriteCloser) *directLink {
	return &directLink{conn: conn}
}

// newSerialLink frames WendyCom over a serial port, which needs escaping and a
// smaller chunk than a network transport.
func newSerialLink(port serial.Port) *directLink {
	return &directLink{conn: port, isSerial: true}
}

// linkHandshake switches a serial device into WendyCom mode; on TCP-TLS there
// is nothing to set up.
func (l *directLink) linkHandshake() error {
	if !l.isSerial {
		return nil
	}
	return serialHandshake(l.conn.(serial.Port))
}

// serialHandshakePort is the narrow slice of serial.Port used by the console
// mode handshake. Keeping it narrow makes the boot-log interleaving behavior
// deterministic to test without a physical serial device.
type serialHandshakePort interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	SetReadTimeout(time.Duration) error
}

func serialHandshake(port serialHandshakePort) error {
	if _, err := port.Write([]byte{escapeChar, escapeChar, escapeChar, escapeChar, 'e'}); err != nil {
		return fmt.Errorf("serial handshake: send escape: %w", err)
	}

	if err := port.SetReadTimeout(100 * time.Millisecond); err != nil {
		return fmt.Errorf("serial handshake: set timeout: %w", err)
	}

	// Every send carries a fresh sentinel, and only the most recent one is
	// matched below. The sentinel is a stream position marker, not a liveness
	// probe: because the echo is FIFO, seeing the *latest* one come back proves
	// nothing the host wrote earlier is still in flight, which is the
	// precondition for switching to WendyCom mode. Repeating one sentinel would
	// only prove that some send was echoed, leaving the echoes of the later
	// sends queued for the frame parser, which rejects them as a bad magic byte.
	var sentinel string
	sendSentinel := func() error {
		var randBytes [16]byte
		if _, err := rand.Read(randBytes[:]); err != nil {
			return fmt.Errorf("serial handshake: generate sentinel: %w", err)
		}
		sentinel = hex.EncodeToString(randBytes[:])
		if _, err := port.Write([]byte(strings.Repeat(" ", 16) + sentinel)); err != nil {
			return fmt.Errorf("serial handshake: send sentinel: %w", err)
		}
		return nil
	}
	// Send immediately. Waiting for a quiet read timeout before the first send
	// makes reconnecting after a physical reboot fail whenever boot or
	// auto-started app logs keep the serial stream continuously readable for
	// the entire 3-second handshake budget.
	if err := sendSentinel(); err != nil {
		return err
	}
	// The mode switch may be applied asynchronously by the device after the
	// escape command is consumed, and console mode swallows host input without
	// echoing it. Keep sending while draining output so at least one sentinel
	// lands after that transition even if Read never times out because boot
	// logs are continuous.
	nextSentinel := time.Now().Add(100 * time.Millisecond)

	window := make([]byte, 0, 32)
	oneByte := make([]byte, 1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !time.Now().Before(nextSentinel) {
			if err := sendSentinel(); err != nil {
				return err
			}
			nextSentinel = time.Now().Add(100 * time.Millisecond)
		}
		n, err := port.Read(oneByte)
		if err != nil {
			return fmt.Errorf("serial handshake: read: %w", err)
		}
		if n == 0 {
			// The echo may have been lost while the device was switching modes,
			// or to a dropped byte (USB Serial JTAG has no flow control);
			// resend whenever the receive buffer goes quiet.
			window = window[:0]
			if err := sendSentinel(); err != nil {
				return err
			}
			nextSentinel = time.Now().Add(100 * time.Millisecond)
			continue
		}
		if n == 1 {
			if len(window) < 32 {
				window = append(window, oneByte[0])
			} else {
				copy(window, window[1:])
				window[31] = oneByte[0]
			}
			if len(window) == 32 && string(window) == sentinel {
				if err := port.SetReadTimeout(serial.NoTimeout); err != nil {
					return fmt.Errorf("serial handshake: clear timeout: %w", err)
				}
				if _, err := port.Write([]byte{escapeChar, 'm'}); err != nil {
					return fmt.Errorf("serial handshake: send mode switch: %w", err)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("serial handshake: sentinel not received within 3 seconds")
}

func (l *directLink) send(req *wendypb.WendyComMessage) error {
	body, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if len(body) > 0xFFFF {
		return fmt.Errorf("message too large: %d bytes exceeds 65535", len(body))
	}
	msg := make([]byte, headerSize+len(body))
	msg[0] = headerMagic
	msg[1] = headerVersion
	binary.BigEndian.PutUint16(msg[6:8], uint16(len(body)))
	copy(msg[headerSize:], body)
	if l.isSerial {
		msg = bytes.ReplaceAll(msg, []byte{escapeChar}, []byte{escapeChar, '_'})
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	for len(msg) > 0 {
		// A record never spans more than one Write, so capping the write caps
		// the record. Serial is exempt: it carries no TLS, and its payload has
		// already been escape-expanded above.
		end := len(msg)
		if !l.isSerial && end > maxTLSRecordSize {
			end = maxTLSRecordSize
		}
		n, err := l.conn.Write(msg[:end])
		if err != nil {
			return fmt.Errorf("send: %w", err)
		}
		msg = msg[n:]
	}
	return nil
}

func (l *directLink) recv(timeout time.Duration) (*wendypb.WendyComMessage, error) {
	raw, err := l.readRawMessage(timeout)
	if err != nil {
		return nil, err
	}
	msg := &wendypb.WendyComMessage{}
	if err := proto.Unmarshal(raw, msg); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return msg, nil
}

func (l *directLink) preferredChunkSize() int {
	if l.isSerial {
		return chunkSizeForSerial
	}
	return chunkSize
}

func (l *directLink) close() error {
	if l.isSerial {
		if port, ok := l.conn.(serial.Port); ok {
			_, _ = port.Write([]byte{escapeChar, 'o'})
			_ = port.Drain()
		}
	}
	return l.conn.Close()
}

// readRawMessage reads one framed message of any kind (response, event, handshake)
// and returns its raw body. Frames on a non-zero category or channel are
// consumed and discarded. A timeout <= 0 means no deadline.
func (l *directLink) readRawMessage(timeout time.Duration) ([]byte, error) {
	for {
		header := make([]byte, headerSize)
		if err := l.readFull(header, timeout); err != nil {
			return nil, fmt.Errorf("reading header: %w", err)
		}
		if header[0] != headerMagic {
			return nil, fmt.Errorf("unexpected magic byte: 0x%02X", header[0])
		}
		if header[1] != headerVersion {
			return nil, fmt.Errorf("unexpected protocol version: 0x%02X", header[1])
		}
		bodyLen := binary.BigEndian.Uint16(header[6:8])
		if bodyLen == 0 {
			return nil, fmt.Errorf("invalid frame: zero-length body")
		}
		body := make([]byte, bodyLen)
		if err := l.readFull(body, timeout); err != nil {
			return nil, fmt.Errorf("reading body: %w", err)
		}
		if header[2] != 0 || header[3] != 0 {
			// Not the default category/channel: skip this frame.
			continue
		}
		return body, nil
	}
}

// readFull reads exactly len(buf) bytes from the connection within timeout.
// A zero timeout means no deadline.
//
// For net.Conn, it sets SetReadDeadline for the duration of the call.
//
// For serial.Port, SetReadTimeout makes Read return (0, nil) on timeout
// instead of an error, which would cause io.ReadFull to spin indefinitely.
// readFull therefore loops manually, trimming the per-Read call to the
// remaining time until the deadline, and converts (0, nil) to an error.
func (l *directLink) readFull(buf []byte, timeout time.Duration) error {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if !l.isSerial {
		if nc, ok := l.conn.(net.Conn); ok && !deadline.IsZero() {
			_ = nc.SetReadDeadline(deadline)
			defer nc.SetReadDeadline(time.Time{}) //nolint:errcheck
		}
		_, err := io.ReadFull(l.conn, buf)
		return err
	}
	sp := l.conn.(serial.Port)
	defer sp.SetReadTimeout(serial.NoTimeout) //nolint:errcheck
	for len(buf) > 0 {
		perRead := serial.NoTimeout
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return fmt.Errorf("read timeout")
			}
			perRead = remaining
		}
		_ = sp.SetReadTimeout(perRead)
		n, err := sp.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("read timeout")
		}
		buf = buf[n:]
	}
	return nil
}
