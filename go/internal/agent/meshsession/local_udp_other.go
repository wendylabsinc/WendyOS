//go:build !linux && !windows

package meshsession

import (
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
)

func configureLocalUDP(conn *net.UDPConn) error {
	return errors.Join(conn.SetReadBuffer(128<<10), conn.SetWriteBuffer(128<<10))
}
func writeLocalUDP(conn *net.UDPConn, payload []byte) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	err = raw.Control(func(fd uintptr) {
		n, e := unix.SendmsgN(int(fd), payload, nil, nil, unix.MSG_DONTWAIT)
		sendErr = e
		if e == nil && n != len(payload) {
			sendErr = io.ErrShortWrite
		}
	})
	return errors.Join(err, sendErr)
}
func udpReadTruncated(flags int) bool { return flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 }

func localUDPBuffers(conn *net.UDPConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var size int
	var optionErr error
	err = raw.Control(func(fd uintptr) {
		r, e1 := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF)
		w, e2 := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_SNDBUF)
		size = r + w
		optionErr = errors.Join(e1, e2)
	})
	return size, errors.Join(err, optionErr)
}
