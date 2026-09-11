//go:build linux

package central

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

// Connection is two halves that share only the peer's address.
//
// The L2CAP half is a raw AF_BLUETOOTH socket: no cgo, no D-Bus, and no link
// until OpenL2CAP. The GATT half (gatt_linux.go, bluez_linux.go) goes through
// BlueZ over D-Bus and stays nil until the first GATT call, so a connection
// that only carries a channel allocates no bus connection and starts no
// goroutine.
type Connection struct {
	fd        int     // -1 once closed, so Close is idempotent
	addr      [6]byte // MSB-first; x/sys reverses it into the kernel's bdaddr_t
	addrType  uint8   // unix.BDADDR_LE_PUBLIC / BDADDR_LE_RANDOM
	addrKnown bool    // false: nothing authoritative has told us the type yet
	l2capOpen bool    // an open channel forbids tearing the ACL link down
	sndMTU    int     // negotiated max SDU for L2CAPSend; 0 until a channel is open

	address string // "AA:BB:CC:DD:EE:FF", uppercased, for BlueZ lookups

	g *gattSession // nil until the first GATT call
}

// Connect parses the Bluetooth address and creates an AF_BLUETOOTH socket.
//
// It deliberately touches neither the radio nor D-Bus, and ignores its timeout:
// no link comes up here. OpenL2CAP brings one up through the kernel, and
// DiscoverServices brings one up through BlueZ, and which of those a caller
// wants is not knowable yet.
//
// Staying lazy is what keeps the pure-L2CAP callers working against a device
// BlueZ has no object for — it evicts unpaired devices roughly 30s after
// discovery stops, while the kernel's L2CAP connect does its own
// scan-and-connect and needs no cached object at all.
func Connect(peripheralAddress string, _ int) (*Connection, error) {
	addr, err := parseBTAddr(peripheralAddress)
	if err != nil {
		return nil, fmt.Errorf("parse BT address: %w", err)
	}

	fd, err := newL2CAPSocket()
	if err != nil {
		return nil, err
	}

	return &Connection{
		fd:       fd,
		addr:     addr,
		addrType: unix.BDADDR_LE_PUBLIC,
		address:  strings.ToUpper(peripheralAddress),
	}, nil
}

// newL2CAPSocket creates an L2CAP socket and binds it as an LE one.
//
// The bind is not optional and its address is not the point: binding is what
// sets the channel's source address type, and l2cap_sock_connect only switches
// the channel into LE credit-based flow control when that type is LE. Without
// it the channel stays in basic L2CAP mode, and the kernel then writes each
// payload as a bare PDU with no 2-byte SDU-length header — so the peer reads
// the first two bytes of the data as the SDU length, gets a nonsense value, and
// drops the channel. A TLS ClientHello starts 0x16 0x03, which reads as an SDU
// of 790 bytes; anything over the peer's MTU is an instant disconnect.
//
// BDADDR_ANY leaves the controller choice to the kernel. The peripheral side in
// internal/agent/bluetooth/l2cap_server_linux.go binds the same way, for the
// same reason.
func newL2CAPSocket() (int, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, unix.BTPROTO_L2CAP)
	if err != nil {
		return -1, fmt.Errorf("create L2CAP socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrL2{AddrType: unix.BDADDR_LE_PUBLIC}); err != nil {
		unix.Close(fd) //nolint:errcheck
		return -1, fmt.Errorf("bind L2CAP socket as LE: %w", err)
	}
	return fd, nil
}

// addressProbeTimeout bounds the best-effort BlueZ lookup OpenL2CAP does to
// learn the peer's LE address type. It is one D-Bus round trip against a local
// socket, so anything longer would only be waiting out a wedged bluetoothd —
// and the fallback below handles that fine.
const addressProbeTimeout = 2 * time.Second

// Socket options for the negotiated LE CoC SDU sizes. They are absent from
// x/sys/unix, so the values come from the kernel's bluetooth.h.
const (
	solBluetooth = 274
	btSndMTU     = 12
)

// l2capMinMTU is the smallest SDU an LE credit-based channel may negotiate
// (Core Specification, Vol 3, Part A, 4.22). It is the fallback when the
// kernel will not report the negotiated value: slow, but never wrong.
const l2capMinMTU = 23

// OpenL2CAP connects the socket to the remote device on the given PSM, using
// non-blocking connect + Poll to respect the timeout.
//
// The LE address type has to match the one the peer advertises with, or the
// controller pages an address nobody answers. BlueZ knows it, so ask — but
// treat a miss as "unknown" rather than an error, because a device BlueZ has
// never seen is exactly the case this path exists to serve.
func (c *Connection) OpenL2CAP(psm uint16, timeoutSeconds int) error {
	if c.fd < 0 {
		return fmt.Errorf("connection is closed")
	}
	if !c.addrKnown {
		// Lazy, not in Connect: a caller that only reads GATT never needs this,
		// and DiscoverServices resolves the device anyway and fills it in.
		if t, ok := probeAddressType(c.address); ok {
			c.addrType, c.addrKnown = t, true
		}
	}

	if c.addrKnown {
		if err := c.connectL2CAP(psm, c.addrType, timeoutSeconds); err != nil {
			return err
		}
		c.noteChannelOpen()
		return nil
	}

	// Nothing authoritative to go on. Try public first — the WendyOS agent and
	// the Wendy Lite firmware both use public addresses — then random. A wrong
	// guess usually times out rather than failing fast, so split the caller's
	// budget instead of spending it twice and blowing through the deadline.
	each := timeoutSeconds / 2
	if each < 1 {
		each = 1
	}
	publicErr := c.connectL2CAP(psm, unix.BDADDR_LE_PUBLIC, each)
	if publicErr == nil {
		c.addrType, c.addrKnown = unix.BDADDR_LE_PUBLIC, true
		c.noteChannelOpen()
		return nil
	}
	// A socket whose connect failed cannot be reconnected; start clean.
	if err := c.resetSocket(); err != nil {
		return fmt.Errorf("connect as public address: %w (could not retry as random: %v)", publicErr, err)
	}
	if randomErr := c.connectL2CAP(psm, unix.BDADDR_LE_RANDOM, each); randomErr != nil {
		return fmt.Errorf("connect as public address: %w; as random address: %w", publicErr, randomErr)
	}
	c.addrType, c.addrKnown = unix.BDADDR_LE_RANDOM, true
	c.noteChannelOpen()
	return nil
}

// noteChannelOpen records a freshly opened channel and reads back the SDU size
// the two ends negotiated, which is what L2CAPSend has to chunk to.
func (c *Connection) noteChannelOpen() {
	c.l2capOpen = true
	c.sndMTU = l2capMinMTU
	// Only known once the channel is up: it comes from the peer's half of the
	// LE credit-based connection response.
	if mtu, err := unix.GetsockoptInt(c.fd, solBluetooth, btSndMTU); err == nil && mtu >= l2capMinMTU {
		c.sndMTU = mtu
	}
}

// pollFD waits for events on fd, retrying when a signal interrupts the wait.
//
// The Go runtime preempts goroutines by sending SIGURG, so any poll long enough
// to matter gets interrupted sooner or later on a busy process. Reporting that
// as a failure turned an ordinary idle wait into "interrupted system call" —
// which is what an L2CAP receive did whenever nothing arrived promptly.
func pollFD(fd int, events int16, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: events}}
		n, err := unix.Poll(pfd, int(remaining.Milliseconds()))
		if err == unix.EINTR {
			continue
		}
		return n, err
	}
}

// connectL2CAP performs one non-blocking connect attempt with a specific LE
// address type, leaving the socket blocking on success.
func (c *Connection) connectL2CAP(psm uint16, addrType uint8, timeoutSeconds int) error {
	if err := unix.SetNonblock(c.fd, true); err != nil {
		return fmt.Errorf("set nonblock: %w", err)
	}

	sa := &unix.SockaddrL2{
		PSM:      psm,
		Addr:     c.addr,
		AddrType: addrType,
	}

	err := unix.Connect(c.fd, sa)
	if err != nil && err != unix.EINPROGRESS {
		return fmt.Errorf("connect: %w", err)
	}

	if err == unix.EINPROGRESS {
		// Wait for connection to complete.
		n, pollErr := pollFD(c.fd, unix.POLLOUT, time.Duration(timeoutSeconds)*time.Second)
		if pollErr != nil {
			return fmt.Errorf("poll connect: %w", pollErr)
		}
		if n == 0 {
			return fmt.Errorf("connect timeout after %ds", timeoutSeconds)
		}
		// Check if the connection actually succeeded.
		errno, sockErr := unix.GetsockoptInt(c.fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if sockErr != nil {
			return fmt.Errorf("getsockopt: %w", sockErr)
		}
		if errno != 0 {
			return fmt.Errorf("connect failed: %w", unix.Errno(errno))
		}
	}

	// Switch back to blocking mode for subsequent reads/writes.
	if err := unix.SetNonblock(c.fd, false); err != nil {
		return fmt.Errorf("set blocking: %w", err)
	}

	return nil
}

// resetSocket replaces the socket after a failed connect, which leaves it
// unusable for another attempt.
func (c *Connection) resetSocket() error {
	if c.fd >= 0 {
		unix.Close(c.fd) //nolint:errcheck
		c.fd = -1
	}
	fd, err := newL2CAPSocket()
	if err != nil {
		return fmt.Errorf("recreate L2CAP socket: %w", err)
	}
	c.fd = fd
	c.l2capOpen, c.sndMTU = false, 0
	return nil
}

// L2CAPSend sends raw bytes over the L2CAP channel, splitting them across as
// many SDUs as the negotiated MTU requires.
//
// The socket is SOCK_SEQPACKET, so one write is one SDU: the kernel either
// takes the whole buffer or fails with EMSGSIZE, and never reports a partial
// write. A loop over the remainder therefore cannot chunk anything on its own —
// it just fails on the first oversized buffer, which is what a TLS handshake
// record is. Splitting here is safe because the channel is a byte stream to
// everything above (see NewL2CAPStream): both peers reassemble by the length
// prefix, not by SDU boundary.
//
// Framing (length prefix) is handled by the caller (agent_client.go).
func (c *Connection) L2CAPSend(data []byte) error {
	if c.fd < 0 {
		return fmt.Errorf("connection is closed")
	}
	chunk := c.sndMTU
	if chunk <= 0 {
		chunk = l2capMinMTU
	}

	for written := 0; written < len(data); {
		end := written + chunk
		if end > len(data) {
			end = len(data)
		}
		n, err := unix.Write(c.fd, data[written:end])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("write: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("write: no progress")
		}
		written += n
	}
	return nil
}

// L2CAPRecv receives one L2CAP SDU with a timeout.
// Returns the raw bytes (including any framing added by the caller).
func (c *Connection) L2CAPRecv(timeoutSeconds int) ([]byte, error) {
	if c.fd < 0 {
		return nil, fmt.Errorf("connection is closed")
	}
	// Poll with timeout.
	n, err := pollFD(c.fd, unix.POLLIN, time.Duration(timeoutSeconds)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("poll recv: %w", err)
	}
	if n == 0 {
		return nil, fmt.Errorf("%w after %ds", ErrRecvTimeout, timeoutSeconds)
	}

	buf := make([]byte, 65536)
	nRead, err := unix.Read(c.fd, buf)
	for err == unix.EINTR {
		nRead, err = unix.Read(c.fd, buf)
	}
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if nRead == 0 {
		return nil, fmt.Errorf("connection closed by peer")
	}
	result := make([]byte, nRead)
	copy(result, buf[:nRead])
	return result, nil
}

// Close releases both halves. It is idempotent: the fd is cleared, so a second
// call cannot close an unrelated descriptor that has since taken the number.
func (c *Connection) Close() {
	if c.g != nil {
		// Whether the ACL link goes down with it depends on who brought it up
		// and whether a channel is still riding on it — see gattSession.close.
		c.g.close(c.l2capOpen)
		c.g = nil
	}
	if c.fd >= 0 {
		unix.Close(c.fd) //nolint:errcheck
		c.fd = -1
	}
	c.l2capOpen, c.sndMTU = false, 0
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// probeAddressType asks BlueZ for a device's LE address type over a short-lived
// bus connection. Best-effort by design: every failure — no bus, no adapter, no
// cached device — reports "unknown" so the caller falls back to guessing rather
// than refusing to connect.
//
// The bus is dropped again immediately. An L2CAP-only session must not be left
// holding a connection it will never use.
func probeAddressType(address string) (uint8, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), addressProbeTimeout)
	defer cancel()

	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return unix.BDADDR_LE_PUBLIC, false
	}
	defer bus.Close() //nolint:errcheck

	managed, err := bluez.GetManagedObjects(ctx, bus)
	if err != nil {
		return unix.BDADDR_LE_PUBLIC, false
	}
	// An adapter that cannot be resolved is not fatal here: an unrestricted
	// lookup still finds the device on whichever controller saw it.
	adapter, _ := bluez.ResolveAdapterPath(managed)
	_, _, props, ok := bluez.FindDeviceByAddress(bluez.RestrictToAdapter(managed, adapter), address)
	if !ok {
		return unix.BDADDR_LE_PUBLIC, false
	}
	return addressTypeFromProps(props)
}

// addressTypeFromProps maps org.bluez.Device1.AddressType onto the constant
// SockaddrL2 wants. ok is false when BlueZ reported nothing usable, which the
// caller must treat as "guess" rather than as "public".
func addressTypeFromProps(props map[string]dbus.Variant) (uint8, bool) {
	s, _ := bluez.StringProp(props, "AddressType")
	switch strings.ToLower(s) {
	case "public":
		return unix.BDADDR_LE_PUBLIC, true
	case "random":
		return unix.BDADDR_LE_RANDOM, true
	default:
		return unix.BDADDR_LE_PUBLIC, false
	}
}

// parseBTAddr parses "AA:BB:CC:DD:EE:FF" into the [6]byte unix.SockaddrL2
// takes: most-significant byte first, the order the address is written in.
//
// That is deliberately NOT the kernel's byte order. A bdaddr_t is little-endian,
// but x/sys/unix reverses SockaddrL2.Addr while marshalling the struct, so it
// wants the human order and does the conversion itself. Handing it an
// already-reversed address cancels the reversal out and aims the connect at a
// byte-reversed peer, which nothing answers — every PSM then times out
// identically instead of being refused, which is what this looked like.
func parseBTAddr(s string) ([6]byte, error) {
	var addr [6]byte
	s = strings.ToUpper(s)
	if len(s) != 17 {
		return addr, fmt.Errorf("invalid BT address length: %q", s)
	}
	for i, offset := range []int{0, 3, 6, 9, 12, 15} {
		// Guard on offset, not on i: the pair at offset 0 is the one with no
		// separator in front of it. Guarding on i once indexed s[-1] on every
		// valid address, which panicked instead of parsing.
		if offset > 0 && s[offset-1] != ':' {
			return addr, fmt.Errorf("invalid BT address separator at position %d: %q", offset-1, s)
		}
		var b byte
		if _, err := fmt.Sscanf(s[offset:offset+2], "%02X", &b); err != nil {
			return addr, fmt.Errorf("invalid BT address byte at position %d: %w", offset, err)
		}
		addr[i] = b
	}
	return addr, nil
}
