package liteclient

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"go.bug.st/serial"
	"google.golang.org/protobuf/proto"
)

const escapeChar = 0x10 // CTRL-P, aka DLE (Data Link Escape)

// keepAliveCmd is DLE 'k', WENDY_COM_UART_ESC_CMD_KEEP_ALIVE in firmware: a
// no-op the device can safely receive at any time, used to keep an
// otherwise-idle serial link from going quiet.
const keepAliveCmd = 'k'

var keepAliveInterval = 6 * time.Second // var so tests can shrink it

// monoEpoch anchors lastSend. Storing offsets from it via time.Since keeps
// Go's monotonic clock in play, so a wall-clock step (e.g. NTP) cannot
// stretch or collapse the idle interval.
var monoEpoch = time.Now()

func monoNow() int64 { return int64(time.Since(monoEpoch)) }

// errLinkClosed is returned by writes attempted once close has begun.
var errLinkClosed = errors.New("link closed")

// On Windows, close arms a watchdog that purges pending serial output
// (PURGE_TXCLEAR|PURGE_TXABORT) every closeWatchdogDelay until shutdown reaches
// conn.Close. That aborts an overlapped write stuck on a device that stopped
// draining, so close cannot hang behind it; repeating covers the next write or
// Drain that gets stuck after the first purge. Off on unix: nothing there
// reliably interrupts a blocked write(2). Vars so tests can enable and shrink
// it on any OS.
var (
	closeWatchdogEnabled = runtime.GOOS == "windows"
	closeWatchdogDelay   = 2 * time.Second
)

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

	// keepAliveClaimed is claimed by whichever of startKeepAlive and close
	// runs first; close waits on keepAliveDone only if startKeepAlive won.
	keepAliveClaimed atomic.Bool
	keepAliveStop    chan struct{}
	keepAliveDone    chan struct{} // closed when the keep-alive loop exits
	lastSend         atomic.Int64  // monoNow() at the last successful write

	// closed is set when close begins. Writers check it only while holding
	// writeMu, and close takes writeMu before tearing down, so no write can
	// reach the wire after close's own final one.
	closed atomic.Bool
}

// newDirectLink frames WendyCom over an established byte stream: TCP-TLS, or
// TLS over a BLE L2CAP channel.
func newDirectLink(conn io.ReadWriteCloser) *directLink {
	return &directLink{conn: conn}
}

// newSerialLink frames WendyCom over a serial port, which needs escaping and a
// smaller chunk than a network transport.
func newSerialLink(port serial.Port) *directLink {
	return newSerialLinkConn(port)
}

func newSerialLinkConn(conn io.ReadWriteCloser) *directLink {
	return &directLink{
		conn:          conn,
		isSerial:      true,
		keepAliveStop: make(chan struct{}),
		keepAliveDone: make(chan struct{}),
	}
}

// linkHandshake switches a serial device into WendyCom mode; on TCP-TLS there
// is nothing to set up.
func (l *directLink) linkHandshake() error {
	if !l.isSerial {
		return nil
	}
	if err := serialHandshake(l.conn.(serial.Port)); err != nil {
		return err
	}
	return l.startKeepAlive()
}

// startKeepAlive sends an immediate DLE 'k' so the device can start
// monitoring for the keep-alive right away, then begins sending one every
// keepAliveInterval of silence, so the serial link stays alive when no other
// WendyCom traffic is flowing. If close already claimed the loop, it does
// nothing.
func (l *directLink) startKeepAlive() error {
	if !l.keepAliveClaimed.CompareAndSwap(false, true) {
		return errLinkClosed
	}
	if err := l.sendKeepAlive(l.lastSend.Load()); err != nil {
		close(l.keepAliveDone)
		return err
	}
	go l.keepAliveLoop()
	return nil
}

// keepAliveLoop recomputes the remaining idle wait from lastSend on every
// iteration rather than resetting a shared timer, which sidesteps the races
// inherent in calling Timer.Reset from a goroutine other than the one
// draining it.
func (l *directLink) keepAliveLoop() {
	defer close(l.keepAliveDone)
	for {
		last := l.lastSend.Load()
		wait := keepAliveInterval - time.Duration(monoNow()-last)
		if wait <= 0 {
			if l.sendKeepAlive(last) != nil {
				return
			}
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-l.keepAliveStop:
			timer.Stop()
			return
		}
	}
}

// sendKeepAlive writes a bare DLE 'k' frame, sharing writeMu with send so it
// never interleaves with a real message on the wire. A write error means the
// link is dead; the read loop discovers that independently via recv, so this
// just stops trying; errLinkClosed once close has begun ends the loop too.
//
// last is the lastSend value the caller judged idle. If it changed, a real
// write went out while we waited for writeMu, so the link is no longer idle
// and the keep-alive is skipped.
func (l *directLink) sendKeepAlive(last int64) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if l.closed.Load() {
		return errLinkClosed
	}
	if l.lastSend.Load() != last {
		return nil
	}
	if _, err := l.conn.Write([]byte{escapeChar, keepAliveCmd}); err != nil {
		return err
	}
	l.lastSend.Store(monoNow())
	return nil
}

// serialHandshakePort is the narrow slice of serial.Port used by the console
// mode handshake. Keeping it narrow makes the boot-log interleaving behavior
// deterministic to test without a physical serial device.
type serialHandshakePort interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	SetReadTimeout(time.Duration) error
}

// The gap after each sentinel widens by one step, holding at
// sentinelIntervalMax: 100, 200, 300, 400, 500, 500... The device rejects the
// echo-mode command until its WendyCom agent is running, so early sentinels are
// expected to go unanswered — widening keeps probing without pushing a kilobyte
// of sentinels at a board that is still booting.
//
// sentinelIntervalStep doubles as the read timeout: it is the finest
// granularity the scheduler needs, and it keeps handshakeBudget accurate to one
// step. Vars so tests can shrink them.
var (
	sentinelIntervalStep = 100 * time.Millisecond
	sentinelIntervalMax  = 500 * time.Millisecond
	handshakeBudget      = 3 * time.Second
)

// nextSentinelInterval widens the gap that follows a sentinel by one step,
// holding at sentinelIntervalMax. Zero yields the first gap.
func nextSentinelInterval(current time.Duration) time.Duration {
	if next := current + sentinelIntervalStep; next < sentinelIntervalMax {
		return next
	}
	return sentinelIntervalMax
}

func serialHandshake(port serialHandshakePort) error {
	if err := port.SetReadTimeout(sentinelIntervalStep); err != nil {
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
	var nextSentinel time.Time
	var interval time.Duration
	sendSentinel := func() error {
		var randBytes [16]byte
		if _, err := rand.Read(randBytes[:]); err != nil {
			return fmt.Errorf("serial handshake: generate sentinel: %w", err)
		}
		sentinel = hex.EncodeToString(randBytes[:])
		// Re-arm echo mode with every sentinel: the device refuses the command
		// until its WendyCom agent is running, and applying it is a plain mode
		// assignment, so re-issuing it is idempotent. The device consumes
		// escape bytes without echoing them, so this prefix never reaches the
		// sentinel window. The 16 spaces absorb the case where the trailing 'e'
		// is dropped and the latched escape char swallows the byte after it.
		payload := make([]byte, 0, 5+16+len(sentinel))
		payload = append(payload, escapeChar, escapeChar, escapeChar, escapeChar, 'e')
		payload = append(payload, strings.Repeat(" ", 16)...)
		payload = append(payload, sentinel...)
		if _, err := port.Write(payload); err != nil {
			return fmt.Errorf("serial handshake: send sentinel: %w", err)
		}
		interval = nextSentinelInterval(interval)
		nextSentinel = time.Now().Add(interval)
		return nil
	}
	// Send immediately. Waiting for a quiet read timeout before the first send
	// makes reconnecting after a physical reboot fail whenever boot or
	// auto-started app logs keep the serial stream continuously readable for
	// the entire handshake budget.
	if err := sendSentinel(); err != nil {
		return err
	}

	window := make([]byte, 0, 32)
	oneByte := make([]byte, 1)
	deadline := time.Now().Add(handshakeBudget)
	for time.Now().Before(deadline) {
		// The mode switch may be applied asynchronously by the device after the
		// escape command is consumed, and console mode swallows host input
		// without echoing it. Keep sending while draining output so at least
		// one sentinel lands after that transition even if Read never times out
		// because boot logs are continuous.
		if !time.Now().Before(nextSentinel) {
			if err := sendSentinel(); err != nil {
				return err
			}
		}
		n, err := port.Read(oneByte)
		if err != nil {
			return fmt.Errorf("serial handshake: read: %w", err)
		}
		if n == 0 {
			// Nothing arrived within one step; the scheduler above decides when
			// to resend. The window is deliberately kept: it is a rolling
			// 32-byte match against a random sentinel, so a stale prefix can
			// neither cause a false match nor block a real one, and keeping it
			// lets an echo split across a read timeout still match.
			continue
		}
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
	return fmt.Errorf("serial handshake: sentinel not received within %s", handshakeBudget)
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
	// Once per frame is enough: close takes writeMu before tearing down, so it
	// cannot run in the middle of this loop.
	if l.closed.Load() {
		return errLinkClosed
	}
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
	l.lastSend.Store(monoNow())
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

// close sets closed first, then holds writeMu before tearing down, so every
// writer has either finished or will see the flag and back off. It never waits
// for the keep-alive goroutine while that goroutine could still be queued on
// writeMu behind a write.
//
// On a serial link, a write stuck on a device that stopped draining blocks
// close at writeMu (or in Drain). Reviews keep flagging this as a deadlock and
// suggest closing the port first, but that would not help: the serial API
// offers no way to cancel pending output. On unix, closing the fd does not
// interrupt a write(2) blocked in another goroutine, and the tty close itself
// waits for pending output to drain. The limitation is the API, not the lock.
// On Windows the close watchdog works around it by purging output; on unix
// such a write still blocks close.
func (l *directLink) close() error {
	l.closed.Store(true)
	if !l.isSerial {
		// tls.Conn.Close closes the transport under an in-flight Write, then
		// taking writeMu waits for that writer to leave, so no write outlives
		// close. Over TCP that breaks the Write (Go's netpoller wakes it). Over
		// BLE it may not: on Linux L2CAPSend is a blocking write(2) that
		// closing the fd does not interrupt, and on darwin it is an opaque C
		// call; nor does tls's close_notify deadline help there, since the
		// L2CAP stream ignores write deadlines. A BLE write stuck on a dead
		// link can therefore still block close here.
		err := l.conn.Close()
		l.writeMu.Lock()
		l.writeMu.Unlock() //nolint:staticcheck — barrier, not a critical section
		return err
	}
	stopWatchdog := l.startCloseWatchdog()
	close(l.keepAliveStop)
	l.writeMu.Lock()
	_, _ = l.conn.Write([]byte{escapeChar, 'o'})
	l.writeMu.Unlock()
	if port, ok := l.conn.(serial.Port); ok {
		_ = port.Drain()
	}
	stopWatchdog()
	err := l.conn.Close()
	if !l.keepAliveClaimed.CompareAndSwap(false, true) {
		// startKeepAlive won, so keepAliveDone gets closed by the loop or by a
		// failed first write. Neither can block: keepAliveStop ends the select,
		// closed rejects any later write, and no write was left in flight once
		// close took writeMu.
		<-l.keepAliveDone
	}
	return err
}

// startCloseWatchdog arms the close watchdog when enabled and the transport
// supports purging: it purges every closeWatchdogDelay until stopped. The
// returned stop only returns once the watchdog goroutine has exited, so it can
// never fire after conn.Close.
func (l *directLink) startCloseWatchdog() (stop func()) {
	r, ok := l.conn.(interface{ ResetOutputBuffer() error })
	if !closeWatchdogEnabled || !ok {
		return func() {}
	}
	cancel := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(closeWatchdogDelay)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = r.ResetOutputBuffer()
			case <-cancel:
				return
			}
		}
	}()
	return func() {
		close(cancel)
		<-done
	}
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
