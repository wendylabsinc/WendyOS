//go:build linux

package services

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// readPeerCredentials reads the connecting process's uid and pid from the
// accepted unix socket using SO_PEERCRED. The kernel captures these at connect
// time, so they cannot be spoofed by the peer and are not subject to a
// pid-reuse race for the life of the connection.
func readPeerCredentials(c net.Conn) (peerCredentials, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peerCredentials{}, fmt.Errorf("%w: connection is not a unix socket", errPeerCredUnavailable)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peerCredentials{}, err
	}
	var cred *unix.Ucred
	var credErr error
	controlErr := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if controlErr != nil {
		return peerCredentials{}, controlErr
	}
	if credErr != nil {
		return peerCredentials{}, credErr
	}
	return peerCredentials{UID: cred.Uid, PID: cred.Pid}, nil
}

// readProcCgroup returns the cgroup membership of a pid.
func readProcCgroup(pid int32) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
