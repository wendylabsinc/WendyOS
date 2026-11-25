//go:build linux

package appservice

import (
	"errors"
	"net"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"golang.org/x/sys/unix"
)

type linuxPeerPin struct {
	mu sync.Mutex
	fd int
}

const peerPollMaxAttempts = 8

// SO_PEERPIDFD pins the original peer held by the socket. pidfd_open(PID) is
// intentionally not a fallback: that can pin a different process after reuse.
func pinOriginalPeer(conn *net.UnixConn) (peerPin, error) {
	if conn == nil {
		return nil, ErrPeer
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return nil, ErrPeer
	}
	fd := -1
	var socketErr error
	err = raw.Control(func(socket uintptr) {
		fd, socketErr = unix.GetsockoptInt(int(socket), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	})
	if err != nil || socketErr != nil || fd < 0 {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return nil, ErrPeer
	}
	unix.CloseOnExec(fd)
	pin := &linuxPeerPin{fd: fd}
	if pin.Alive() != nil {
		_ = pin.Close()
		return nil, ErrPeer
	}
	return pin, nil
}
func (p *linuxPeerPin) Alive() error {
	err, _ := p.aliveDiagnostic()
	return err
}

func (p *linuxPeerPin) aliveDiagnostic() (error, pinObservation) {
	return p.observeWithPoll(unix.Poll)
}

// Keep the original pidfd locked through every immediate retry, including
// against Close. EINTR says the poll was interrupted, not that the peer died.
// A retry never opens another pidfd or reuses an earlier liveness result.
// The function argument is a per-call test seam, not mutable process state.
func (p *linuxPeerPin) aliveWithPoll(poll func([]unix.PollFd, int) (int, error)) error {
	err, _ := p.observeWithPoll(poll)
	return err
}

func (p *linuxPeerPin) observeWithPoll(poll func([]unix.PollFd, int) (int, error)) (error, pinObservation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	observation := pinObservation{}
	deny := func(reason rpctrace.RefusalReason) (error, pinObservation) {
		observation.reason = reason
		return ErrPeer, observation
	}
	if p.fd < 0 {
		return deny(rpctrace.ReasonPinClosed)
	}
	if poll == nil {
		return deny(rpctrace.ReasonPinUnknown)
	}
	for attempt := 0; attempt < peerPollMaxAttempts; attempt++ {
		polls := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
		n, err := poll(polls, 0)
		if polls[0].Revents != 0 || n > 0 {
			if polls[0].Revents&unix.POLLIN != 0 && polls[0].Revents&(unix.POLLERR|unix.POLLNVAL) == 0 {
				return deny(rpctrace.ReasonPinDead)
			}
			return deny(rpctrace.ReasonPinEvent)
		}
		if errors.Is(err, unix.EINTR) {
			observation.interruptions++
			continue
		}
		if err != nil || n != 0 {
			return deny(rpctrace.ReasonPinSyscall)
		}
		return nil, observation
	}
	// An interruption storm never becomes an assumed live peer.
	return deny(rpctrace.ReasonPinInterruptLimit)
}
func (p *linuxPeerPin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}

func readPeerCredentials(conn *net.UnixConn) (Peer, error) {
	if conn == nil {
		return Peer{}, ErrPeer
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return Peer{}, ErrPeer
	}
	var cred *unix.Ucred
	var socketErr error
	if err = raw.Control(func(fd uintptr) { cred, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil || socketErr != nil || cred == nil || cred.Pid <= 0 {
		return Peer{}, ErrPeer
	}
	return Peer{PID: cred.Pid, UID: cred.Uid, GID: cred.Gid}, nil
}
