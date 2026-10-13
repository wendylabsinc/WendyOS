//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	btProtoL2CAP   = 0
	btAddrLEPublic = 1
	btAddrLERandom = 2
	// Linux bluetooth.h: SOL_BLUETOOTH=274, BT_SECURITY=4,
	// BT_SECURITY_LOW=1. TLS supplies the authenticated encryption; low link
	// security avoids an unrelated pairing/bonding prerequisite.
	solBluetooth = 274
	btSecurity   = 4
	// A CoC socket is SOCK_SEQPACKET. One Write is one SDU, while TLS expects
	// a byte stream. Keep each SDU below the smallest MPS observed on our
	// controllers (247 bytes) so the controller need not fragment TLS writes.
	// Read retains surplus SDU bytes for the next TLS read.
	maxWriteSDU = 240
	maxReadSDU  = 65535
	// Linux LE CoC sendmsg queues an SDU before radio credits transmit it.
	// Bound that kernel FIFO so short mesh packets do not wait behind many
	// seconds of older IP traffic. Linux clamps this 1024-byte request to an
	// effective 4608 bytes on the tested Jetson/Pi kernels.
	meshSendBuffer = 1024
	// LE CoC initial credits derive from the socket receive MTU: the kernel
	// default 672-byte IMTU grants 3 credits (672/247+1), locking every bulk
	// channel to ~1 SDU per connection event (~4KB/s measured). Raising IMTU
	// lets one anchor carry a full event of SDUs; per-SDU top-ups sustain it.
	meshReceiveMTU = 65535
	// A frame's total budget may be longer, but a blocked CoC writer must
	// release the link if the peer stops granting credits or draining data.
	defaultWriteIdleTimeout = 5 * time.Second
)

type l2addr struct {
	Address string
	Type    uint8
	PSM     uint16
}

func (a l2addr) Network() string { return "l2cap-le" }
func (a l2addr) String() string {
	return fmt.Sprintf("%s/%s/%04x", a.Address, map[uint8]string{1: "public", 2: "random"}[a.Type], a.PSM)
}

func parseAddress(address, addressType string, psm uint16) (l2addr, [6]byte, error) {
	var addr [6]byte
	if !ValidPSM(psm) {
		return l2addr{}, addr, errors.New("invalid LE PSM")
	}
	parts := strings.Split(address, ":")
	if len(parts) != 6 {
		return l2addr{}, addr, errors.New("invalid Bluetooth address")
	}
	for i, part := range parts {
		if len(part) != 2 {
			return l2addr{}, addr, errors.New("invalid Bluetooth address")
		}
		v, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return l2addr{}, addr, errors.New("invalid Bluetooth address")
		}
		addr[i] = byte(v)
	}
	var kind uint8
	switch addressType {
	case "public":
		kind = btAddrLEPublic
	case "random":
		kind = btAddrLERandom
	default:
		return l2addr{}, addr, errors.New("invalid Bluetooth address type")
	}
	return l2addr{Address: strings.ToUpper(address), Type: kind, PSM: psm}, addr, nil
}

func newL2Socket() (int, error) {
	fd, err := unix.Socket(unix.AF_BLUETOOTH, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, btProtoL2CAP)
	if err != nil {
		return -1, err
	}
	if err = unix.SetsockoptString(fd, solBluetooth, btSecurity, string([]byte{1, 0})); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("setting unpaired LE link security: %w", err)
	}
	if err = boundL2CAPSendBuffer(fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func boundL2CAPSendBuffer(fd int) error {
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, meshSendBuffer); err != nil {
		return fmt.Errorf("bound L2CAP send buffer: %w", err)
	}
	actual, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF)
	if err != nil {
		return fmt.Errorf("inspect L2CAP send buffer: %w", err)
	}
	if actual < maxWriteSDU*2 || actual > max(meshSendBuffer*4, 4608) {
		return fmt.Errorf("unexpected L2CAP send buffer %d", actual)
	}
	return nil
}

type l2Listener struct {
	fdMu      sync.RWMutex
	fd        int
	psm       uint16
	closed    atomic.Bool
	closeOnce sync.Once
}

// l2capReceiveMTULadder tries receive MTUs from large to small. The kernel
// rejects L2CAP_OPTIONS before bind and caps imtu per version; the settled
// value is reported once so fleets show what bulk channels actually grant.
var l2capReceiveMTULadder = []int{65535, 32768, 16384, 8192, 4096, 2048}

const l2capOptionsOpt = 1

// settleLog receives settle outcomes; the provider wires its logger at
// startup. Package default is nop so tests and fixtures stay quiet.
var settleLog func(mtu int, err error) = func(int, error) {}

func reportSettle(mtu int, err error) {
	if settleLog != nil {
		settleLog(mtu, err)
	}
}

// settleL2CAPReceiveMTU raises the socket receive MTU post-bind so LE CoC
// initial credits cover a full connection event of SDUs instead of 3. It is
// best-effort: failure keeps the kernel default (slower bulk, same
// correctness). Every attempt reports its outcome via report (nil keeps it
// silent for tests); the field reading of initial RSP credits decides.
func settleL2CAPReceiveMTU(fd int, report func(mtu int, err error)) int {
	var firstErr error
	for _, mtu := range l2capReceiveMTULadder {
		if err := setL2CAPReceiveMTU(fd, mtu); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if report != nil {
			report(mtu, nil)
		}
		return mtu
	}
	if report != nil {
		report(0, firstErr)
	}
	return 0
}

// setL2CAPReceiveMTU stages imtu through get-modify-set-verify and reports
// which stage failed, so field logs distinguish kernel rejection from
// transport impossible states.
func setL2CAPReceiveMTU(fd, mtu int) error {
	var opts [11]byte
	length := uint32(len(opts))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(unix.SOL_L2CAP), uintptr(l2capOptionsOpt),
		uintptr(unsafe.Pointer(&opts[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno != 0 {
		return fmt.Errorf("l2cap options inspect: %w", errno)
	}
	if length < 4 {
		return fmt.Errorf("l2cap options short read %d", length)
	}
	opts[2] = byte(mtu & 0xff)
	opts[3] = byte(mtu >> 8)
	_, _, errno = unix.Syscall6(unix.SYS_SETSOCKOPT, uintptr(fd), uintptr(unix.SOL_L2CAP), uintptr(l2capOptionsOpt),
		uintptr(unsafe.Pointer(&opts[0])), uintptr(length), 0)
	if errno != 0 {
		return fmt.Errorf("l2cap imtu %d rejected: %w", mtu, errno)
	}
	for i := range opts {
		opts[i] = 0
	}
	length = uint32(len(opts))
	_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), uintptr(unix.SOL_L2CAP), uintptr(l2capOptionsOpt),
		uintptr(unsafe.Pointer(&opts[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno != 0 {
		return fmt.Errorf("l2cap options verify: %w", errno)
	}
	if got := int(opts[2]) | int(opts[3])<<8; got != mtu {
		return fmt.Errorf("l2cap imtu read back %d, want %d", got, mtu)
	}
	return nil
}

func listenL2CAP(psm uint16) (*l2Listener, error) {
	if !ValidPSM(psm) {
		return nil, errors.New("invalid LE PSM")
	}
	fd, err := newL2Socket()
	if err != nil {
		return nil, fmt.Errorf("L2CAP socket: %w", err)
	}
	good := false
	defer func() {
		if !good {
			_ = unix.Close(fd)
		}
	}()
	if err = unix.Bind(fd, &unix.SockaddrL2{PSM: psm, AddrType: btAddrLEPublic}); err != nil {
		return nil, fmt.Errorf("L2CAP bind PSM %d: %w", psm, err)
	}
	settleL2CAPReceiveMTU(fd, reportSettle)
	if err = unix.Listen(fd, 8); err != nil {
		return nil, fmt.Errorf("L2CAP listen: %w", err)
	}
	good = true
	return &l2Listener{fd: fd, psm: psm}, nil
}

func (l *l2Listener) Accept(ctx context.Context) (net.Conn, error) {
	for {
		if l.closed.Load() {
			return nil, net.ErrClosed
		}
		if err := pollFDLive(ctx, l.fd, unix.POLLIN, func() int64 { return 0 }, &l.fdMu, &l.closed); err != nil {
			return nil, err
		}
		l.fdMu.RLock()
		if l.closed.Load() {
			l.fdMu.RUnlock()
			return nil, net.ErrClosed
		}
		fd, sa, err := unix.Accept4(l.fd, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		l.fdMu.RUnlock()
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := boundL2CAPSendBuffer(fd); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		remote := l2addr{PSM: l.psm}
		if peer, ok := sa.(*unix.SockaddrL2); ok {
			remote = acceptedL2Addr(peer, l.psm)
		}
		return newPacketConn(fd, l2addr{Address: "any", Type: btAddrLEPublic, PSM: l.psm}, remote), nil
	}
}

func (l *l2Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		l.fdMu.Lock()
		defer l.fdMu.Unlock()
		err = unix.Close(l.fd)
	})
	return err
}

func dialL2CAP(ctx context.Context, address, addressType string, psm uint16) (net.Conn, error) {
	remote, mac, err := parseAddress(address, addressType, psm)
	if err != nil {
		return nil, err
	}
	fd, err := newL2Socket()
	if err != nil {
		return nil, fmt.Errorf("L2CAP socket: %w", err)
	}
	good := false
	defer func() {
		if !good {
			_ = unix.Close(fd)
		}
	}()
	// Linux selects LE credit-based mode from the local address type and PSM
	// before connect. A zero-PSM LE bind allocates a local dynamic PSM; without
	// it the socket stays in basic mode and sends TLS without CoC SDU framing.
	if err = unix.Bind(fd, &unix.SockaddrL2{AddrType: btAddrLEPublic}); err != nil {
		return nil, fmt.Errorf("L2CAP bind outbound LE socket: %w", err)
	}
	settleL2CAPReceiveMTU(fd, reportSettle)
	err = unix.Connect(fd, &unix.SockaddrL2{PSM: psm, Addr: mac, AddrType: remote.Type})
	if err != nil && err != unix.EINPROGRESS {
		return nil, fmt.Errorf("L2CAP connect %s: %w", remote, err)
	}
	if err == unix.EINPROGRESS {
		if err = pollFD(ctx, fd, unix.POLLOUT, 0); err != nil {
			return nil, err
		}
		status, statusErr := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if statusErr != nil {
			return nil, statusErr
		}
		if status != 0 {
			return nil, fmt.Errorf("L2CAP connect %s: %w", remote, syscall.Errno(status))
		}
	}
	good = true
	return newPacketConn(fd, l2addr{Address: "local", Type: btAddrLEPublic, PSM: psm}, remote), nil
}

// x/sys/unix v0.47.0 reverses canonical SockaddrL2.Addr bytes on
// connect, but its accept decoder copies the kernel's little-endian bdaddr_t
// bytes unchanged. Normalize only accepted addresses; outbound parseAddress
// already uses the canonical order expected by unix.Connect.
func acceptedL2Addr(peer *unix.SockaddrL2, psm uint16) l2addr {
	var canonical [6]byte
	for i := range canonical {
		canonical[i] = peer.Addr[len(canonical)-1-i]
	}
	return l2addr{Address: formatAddress(canonical), Type: peer.AddrType, PSM: psm}
}

func formatAddress(a [6]byte) string {
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", a[0], a[1], a[2], a[3], a[4], a[5])
}

// packetConn adapts L2CAP SDUs to the byte-stream contract TLS requires.
type packetConn struct {
	fd                          int
	local, remote               l2addr
	rmu, wmu                    sync.Mutex
	fdMu                        sync.RWMutex
	readBuf                     []byte
	pending                     []byte
	readDeadline, writeDeadline atomic.Int64
	writeIdleTimeout            time.Duration
	closed                      atomic.Bool
	closeOnce                   sync.Once
}

func newPacketConn(fd int, local, remote l2addr) *packetConn {
	return &packetConn{fd: fd, local: local, remote: remote, readBuf: make([]byte, maxReadSDU), writeIdleTimeout: defaultWriteIdleTimeout}
}

// CoCReceiveMTU reports the socket receive MTU backing LE CoC credits,
// or 0 when unavailable. Wrapper layers (TLS, metering) are unwrapped. It
// verifies what initial-credit grants actually apply, since the kernel caps
// imtu per version.
func CoCReceiveMTU(conn net.Conn) int {
	for i := 0; i < 4; i++ {
		packet, ok := conn.(*packetConn)
		if ok {
			return packet.receiveMTU()
		}
		unwrapper, ok := conn.(interface{ NetConn() net.Conn })
		if !ok || unwrapper.NetConn() == nil {
			return 0
		}
		conn = unwrapper.NetConn()
	}
	return 0
}

func (c *packetConn) receiveMTU() int {
	c.fdMu.RLock()
	defer c.fdMu.RUnlock()
	if c.closed.Load() {
		return 0
	}
	var opts [11]byte
	length := uint32(len(opts))
	_, _, errno := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(c.fd), uintptr(unix.SOL_L2CAP), uintptr(l2capOptionsOpt),
		uintptr(unsafe.Pointer(&opts[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno != 0 || length < 4 {
		return 0
	}
	return int(opts[2]) | int(opts[3])<<8
}

// SockQueue reports raw Linux Bluetooth socket ioctl readings. SIOCINQ is
// the next queued skb's length (zero when none is queued), not total unread
// bytes. SIOCOUTQ is available send-buffer budget, clamped at zero, not an
// unsent-byte count. These samples alone cannot attribute a stalled stream.
// Returns an error when either ioctl fails or the descriptor is closed.
func (c *packetConn) SockQueue() (inQ, outQ int, err error) {
	c.fdMu.RLock()
	defer c.fdMu.RUnlock()
	if c.closed.Load() {
		return 0, 0, net.ErrClosed
	}
	inQ, err = unix.IoctlGetInt(c.fd, unix.SIOCINQ)
	if err != nil {
		return 0, 0, err
	}
	outQ, err = unix.IoctlGetInt(c.fd, unix.SIOCOUTQ)
	if err != nil {
		return 0, 0, err
	}
	return inQ, outQ, nil
}

func (c *packetConn) Read(dst []byte) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	for len(c.pending) == 0 {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		if err := c.poll(unix.POLLIN, c.readDeadline.Load); err != nil {
			return 0, err
		}
		c.fdMu.RLock()
		if c.closed.Load() {
			c.fdMu.RUnlock()
			return 0, net.ErrClosed
		}
		n, err := unix.Read(c.fd, c.readBuf)
		c.fdMu.RUnlock()
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		c.pending = c.readBuf[:n]
	}
	n := copy(dst, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *packetConn) Write(src []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	written := 0
	idleDeadline := time.Now().Add(c.writeIdleTimeout).UnixNano()
	for written < len(src) {
		if c.closed.Load() {
			return written, net.ErrClosed
		}
		if err := c.poll(unix.POLLOUT, func() int64 {
			deadline := c.writeDeadline.Load()
			if deadline == 0 || idleDeadline < deadline {
				return idleDeadline
			}
			return deadline
		}); err != nil {
			return written, err
		}
		end := min(len(src), written+maxWriteSDU)
		c.fdMu.RLock()
		if c.closed.Load() {
			c.fdMu.RUnlock()
			return written, net.ErrClosed
		}
		n, err := unix.Write(c.fd, src[written:end])
		c.fdMu.RUnlock()
		if err == unix.EAGAIN || err == unix.EINTR {
			continue
		}
		if err != nil {
			return written, err
		}
		if n != end-written {
			return written, io.ErrShortWrite
		}
		written += n
		idleDeadline = time.Now().Add(c.writeIdleTimeout).UnixNano()
	}
	return written, nil
}

func (c *packetConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		// A bounded poll or nonblocking syscall may still use this descriptor.
		// Join it before close makes the number available to another socket.
		c.fdMu.Lock()
		defer c.fdMu.Unlock()
		_ = unix.Shutdown(c.fd, unix.SHUT_RDWR)
		err = unix.Close(c.fd)
	})
	return err
}
func (c *packetConn) LocalAddr() net.Addr  { return c.local }
func (c *packetConn) RemoteAddr() net.Addr { return c.remote }
func (c *packetConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	c.SetWriteDeadline(t)
	return nil
}
func (c *packetConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(deadlineValue(t))
	return nil
}
func (c *packetConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.Store(deadlineValue(t))
	return nil
}

func deadlineValue(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func pollFD(ctx context.Context, fd int, events int16, deadline int64) error {
	return pollFDLive(ctx, fd, events, func() int64 { return deadline }, nil, nil)
}

func (c *packetConn) poll(events int16, deadline func() int64) error {
	return pollFDLive(context.Background(), c.fd, events, deadline, &c.fdMu, &c.closed)
}

// Reload deadlines after every bounded poll, including when another goroutine
// extends or clears a deadline on an already pending Read or Write. The optional
// descriptor lock also covers SO_ERROR so Close cannot race descriptor reuse.
func pollFDLive(ctx context.Context, fd int, events int16, deadlineValue func() int64, fdMu *sync.RWMutex, closed *atomic.Bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if closed != nil && closed.Load() {
			return net.ErrClosed
		}
		timeout := 250
		deadline := deadlineValue()
		if deadline != 0 {
			remaining := time.Until(time.Unix(0, deadline))
			if remaining <= 0 {
				return osTimeout{}
			}
			if ms := int(remaining.Milliseconds()); ms < timeout {
				timeout = max(1, ms)
			}
		}
		n, revents, err := pollDescriptor(fd, events, timeout, fdMu, closed)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if revents&(events|unix.POLLHUP) != 0 {
			return nil
		}
	}
}

func pollDescriptor(fd int, events int16, timeout int, fdMu *sync.RWMutex, closed *atomic.Bool) (int, int16, error) {
	if fdMu != nil {
		fdMu.RLock()
		defer fdMu.RUnlock()
	}
	if closed != nil && closed.Load() {
		return 0, 0, net.ErrClosed
	}
	fds := []unix.PollFd{{Fd: int32(fd), Events: events}}
	n, err := unix.Poll(fds, timeout)
	if err != nil {
		return n, 0, err
	}
	if fds[0].Revents&(unix.POLLNVAL|unix.POLLERR) != 0 {
		status, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return n, 0, err
		}
		if status != 0 {
			return n, 0, syscall.Errno(status)
		}
		return n, 0, net.ErrClosed
	}
	return n, fds[0].Revents, nil
}

type osTimeout struct{}

func (osTimeout) Error() string   { return "L2CAP I/O timeout" }
func (osTimeout) Timeout() bool   { return true }
func (osTimeout) Temporary() bool { return true }
