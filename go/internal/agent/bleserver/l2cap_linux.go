//go:build linux

package bleserver

import (
	"fmt"
	"net"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	afBluetooth  = 31 // AF_BLUETOOTH
	btprotoL2CAP = 0  // BTPROTO_L2CAP

	// BLE address type for public addresses.
	bdaddrLEPublic = 1 // BDADDR_LE_PUBLIC
)

// sockaddrL2 matches the C struct sockaddr_l2 for BLE L2CAP.
//
//	struct sockaddr_l2 {
//	    sa_family_t    l2_family;   // AF_BLUETOOTH
//	    __le16         l2_psm;      // PSM (little-endian)
//	    bdaddr_t       l2_bdaddr;   // 6 bytes, BDADDR_ANY = 00:00:00:00:00:00
//	    __le16         l2_cid;      // CID
//	    uint8_t        l2_bdaddr_type; // address type
//	};
type sockaddrL2 struct {
	Family     uint16
	PSM        uint16 // little-endian
	BDAddr     [6]byte
	CID        uint16
	BDAddrType uint8
	_          [1]byte // padding to 14 bytes
}

// l2capListener wraps a raw L2CAP socket file descriptor for accept operations.
type l2capListener struct {
	fd int
}

// newL2CAPListener creates a BLE L2CAP listener on the given PSM.
func newL2CAPListener(psm uint16) (*l2capListener, error) {
	fd, err := unix.Socket(afBluetooth, unix.SOCK_SEQPACKET, btprotoL2CAP)
	if err != nil {
		return nil, fmt.Errorf("creating L2CAP socket: %w", err)
	}

	addr := sockaddrL2{
		Family:     afBluetooth,
		PSM:        psm, // kernel expects little-endian; on LE systems this is fine
		BDAddrType: bdaddrLEPublic,
	}

	_, _, errno := unix.Syscall(
		unix.SYS_BIND,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("binding L2CAP socket to PSM %d: %w", psm, errno)
	}

	if err := unix.Listen(fd, 1); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("listening on L2CAP socket: %w", err)
	}

	return &l2capListener{fd: fd}, nil
}

// setRecvTimeout sets SO_RCVTIMEO on the listen socket for interruptible accept.
func (l *l2capListener) setRecvTimeout(tv unix.Timeval) error {
	return unix.SetsockoptTimeval(l.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
}

// accept blocks until a new L2CAP connection arrives, returning a net.Conn.
// Returns an error wrapping EAGAIN/EWOULDBLOCK if the receive timeout expires.
func (l *l2capListener) accept() (net.Conn, error) {
	nfd, _, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}

	f := os.NewFile(uintptr(nfd), "l2cap-conn")
	if f == nil {
		unix.Close(nfd)
		return nil, fmt.Errorf("failed to create file from fd %d", nfd)
	}

	conn, err := net.FileConn(f)
	f.Close() // FileConn dups the fd, so close the original
	if err != nil {
		return nil, fmt.Errorf("creating net.Conn from L2CAP fd: %w", err)
	}

	return conn, nil
}

// close shuts down the listener socket.
func (l *l2capListener) close() error {
	return unix.Close(l.fd)
}
