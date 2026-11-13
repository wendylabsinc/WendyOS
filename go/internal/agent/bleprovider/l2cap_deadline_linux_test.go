//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Start an operation that cannot finish until its deadline or peer changes.
// The full socket fixture exercises actual poll backpressure on Write.
func blockedPacketOperation(t *testing.T, write bool, initial time.Time) (*packetConn, <-chan error, func()) {
	t.Helper()
	a, b := packetPair(t)
	if write {
		if err := boundL2CAPSendBuffer(a.fd); err != nil {
			t.Fatal(err)
		}
		for {
			_, err := unix.Write(a.fd, make([]byte, maxWriteSDU))
			if errors.Is(err, unix.EAGAIN) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	a.writeIdleTimeout = 3 * time.Second
	_ = a.SetDeadline(initial)
	done := make(chan error, 1)
	go func() {
		var err error
		if write {
			_, err = a.Write([]byte("test"))
		} else {
			_, err = a.Read(make([]byte, 4))
		}
		done <- err
	}()
	var mu *sync.Mutex
	if write {
		mu = &a.wmu
	} else {
		mu = &a.rmu
	}
	limit := time.Now().Add(time.Second)
	for mu.TryLock() {
		mu.Unlock()
		if time.Now().After(limit) {
			t.Fatal("operation did not start")
		}
		time.Sleep(time.Millisecond)
	}
	// Let the operation enter poll with its original deadline. A late start
	// cannot turn a broken implementation into a false pass for these tests.
	time.Sleep(40 * time.Millisecond)
	unblock := func() {
		if write {
			go func() { _, _ = io.Copy(io.Discard, b) }()
		} else {
			if _, err := b.Write([]byte("test")); err != nil {
				t.Fatal(err)
			}
		}
	}
	return a, done, unblock
}

func requirePacketResult(t *testing.T, done <-chan error, timeout bool) {
	t.Helper()
	select {
	case err := <-done:
		if timeout {
			var n net.Error
			if !errors.As(err, &n) || !n.Timeout() {
				t.Fatalf("wanted timeout, got %v", err)
			}
		} else if err != nil {
			t.Fatalf("operation failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending operation did not finish within bounded poll interval")
	}
}

func TestPacketConnPendingDeadlineUpdates(t *testing.T) {
	for _, write := range []bool{false, true} {
		name := "Read"
		if write {
			name = "Write"
		}
		for _, change := range []string{"set", "shorten", "extend", "clear"} {
			t.Run(name+"/"+change, func(t *testing.T) {
				initial := time.Time{}
				if change == "shorten" {
					initial = time.Now().Add(2 * time.Second)
				}
				if change == "extend" || change == "clear" {
					initial = time.Now().Add(180 * time.Millisecond)
				}
				a, done, unblock := blockedPacketOperation(t, write, initial)
				deadline := time.Now().Add(30 * time.Millisecond)
				if change == "extend" {
					deadline = time.Now().Add(2 * time.Second)
				}
				if change == "clear" {
					deadline = time.Time{}
				}
				if write {
					_ = a.SetWriteDeadline(deadline)
				} else {
					_ = a.SetReadDeadline(deadline)
				}
				if change == "set" || change == "shorten" {
					requirePacketResult(t, done, true)
					return
				}
				// The previous deadline must expire without finishing the call.
				select {
				case err := <-done:
					t.Fatalf("used obsolete deadline: %v", err)
				case <-time.After(time.Until(initial.Add(100 * time.Millisecond))):
				}
				unblock()
				requirePacketResult(t, done, false)
			})
		}
	}
}

func TestPacketConnCloseUnblocksPendingIO(t *testing.T) {
	for _, write := range []bool{false, true} {
		name := "Read"
		if write {
			name = "Write"
		}
		t.Run(name, func(t *testing.T) {
			a, done, _ := blockedPacketOperation(t, write, time.Time{})
			closed := make(chan error, 1)
			go func() { closed <- a.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Close did not unblock")
			}
			select {
			case err := <-done:
				if !errors.Is(err, net.ErrClosed) {
					t.Fatalf("wanted closed, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("I/O did not unblock after Close")
			}
		})
	}
}

func TestPacketConnCloseProtectsDescriptorLifetime(t *testing.T) {
	a, _ := packetPair(t)
	// Model an in-flight syscall holding a descriptor reference. Close must
	// mark the connection closed, then join that reference before fd reuse.
	a.fdMu.RLock()
	released := false
	defer func() {
		if !released {
			a.fdMu.RUnlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- a.Close() }()
	limit := time.Now().Add(time.Second)
	for !a.closed.Load() {
		if time.Now().After(limit) {
			t.Fatal("Close did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := unix.FcntlInt(uintptr(a.fd), unix.F_GETFD, 0); err != nil {
		t.Fatalf("descriptor closed during syscall: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("Close finished with live descriptor reference: %v", err)
	default:
	}
	a.fdMu.RUnlock()
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Reuse the exact number for a new socket; stale connection operations
	// must not consume or write bytes on that unrelated socket.
	f, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(f[1])
	if f[0] != a.fd {
		if err := unix.Dup3(f[0], a.fd, unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		_ = unix.Close(f[0])
	}
	defer unix.Close(a.fd)
	if _, err := unix.Write(f[1], []byte("fresh")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(make([]byte, 5)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stale Read = %v", err)
	}
	if _, err := a.Write([]byte("stale")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("stale Write = %v", err)
	}
	buf := make([]byte, 5)
	if n, err := unix.Read(a.fd, buf); err != nil || string(buf[:n]) != "fresh" {
		t.Fatalf("fresh socket payload changed: %d %q %v", n, buf, err)
	}
	if _, err := unix.Read(f[1], buf); !errors.Is(err, unix.EAGAIN) {
		t.Fatalf("stale write reached reused socket: %v", err)
	}
}

func TestPacketConnClosedReadDoesNotDrainBufferedSDU(t *testing.T) {
	a, b := packetPair(t)
	if _, err := b.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := a.Read(buf); err != nil || string(buf) != "abc" {
		t.Fatalf("read = %q %v", buf, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := a.Read(buf); n != 0 || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read after Close = %d %v", n, err)
	}
}

func unixPacketListener(t *testing.T) (*l2Listener, string) {
	t.Helper()
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/listener.sock"
	if err = unix.Bind(fd, &unix.SockaddrUnix{Name: path}); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	if err = unix.Listen(fd, 4); err != nil {
		unix.Close(fd)
		t.Fatal(err)
	}
	l := &l2Listener{fd: fd}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func TestL2ListenerCloseUnblocksAccept(t *testing.T) {
	l, _ := unixPacketListener(t)
	done := make(chan error, 1)
	go func() {
		c, err := l.Accept(context.Background())
		if c != nil {
			c.Close()
		}
		done <- err
	}()
	time.Sleep(40 * time.Millisecond)
	closed := make(chan error, 1)
	go func() { closed <- l.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener Close blocked")
	}
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept stayed blocked after Close")
	}
}

func TestL2ListenerCloseProtectsDescriptorReuse(t *testing.T) {
	l, _ := unixPacketListener(t)
	l.fdMu.RLock()
	released := false
	defer func() {
		if !released {
			l.fdMu.RUnlock()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- l.Close() }()
	limit := time.Now().Add(time.Second)
	for !l.closed.Load() {
		if time.Now().After(limit) {
			t.Fatal("Close did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := unix.FcntlInt(uintptr(l.fd), unix.F_GETFD, 0); err != nil {
		t.Fatalf("listener fd closed with syscall live: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("Close completed with live fd reference: %v", err)
	default:
	}
	l.fdMu.RUnlock()
	released = true
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Reuse the exact fd for a different live listener with an incoming client.
	next, path := unixPacketListener(t)
	if next.fd != l.fd {
		if err := unix.Dup3(next.fd, l.fd, unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		unix.Close(next.fd)
		next.fd = l.fd
	}
	client, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(client)
	if err = unix.Connect(client, &unix.SockaddrUnix{Name: path}); err != nil {
		t.Fatal(err)
	}
	if c, err := l.Accept(context.Background()); !errors.Is(err, net.ErrClosed) {
		if c != nil {
			c.Close()
		}
		t.Fatalf("stale listener accepted reused fd: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := next.Accept(ctx)
	if err != nil {
		t.Fatalf("new listener lost incoming client: %v", err)
	}
	c.Close()
}
