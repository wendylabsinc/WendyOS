//go:build linux

package meshsession

import (
	"errors"
	"io"
	"net"

	"golang.org/x/sys/unix"
)

func configureLocalUDP(conn *net.UDPConn) error {
	if err := conn.SetReadBuffer(128 << 10); err != nil {
		return err
	}
	if err := conn.SetWriteBuffer(128 << 10); err != nil {
		return err
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var optionErr error
	err = raw.Control(func(fd uintptr) {
		optionErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MTU_DISCOVER, unix.IP_PMTUDISC_DONT)
	})
	return errors.Join(err, optionErr)
}
func writeLocalUDP(conn *net.UDPConn, payload []byte) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sendErr error
	err = raw.Control(func(fd uintptr) {
		n, err := unix.SendmsgN(int(fd), payload, nil, nil, unix.MSG_DONTWAIT)
		sendErr = err
		if err == nil && n != len(payload) {
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
