//go:build linux

package bluetooth

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestL2CAPRetriesPreemptionWithoutEndingSession(t *testing.T) {
	calls := 0
	n, err := retryL2CAPInterrupted(func() (int, error) {
		calls++
		if calls <= 3 {
			return -1, fmt.Errorf("socket: %w", unix.EINTR)
		}
		return 42, nil
	})
	if n != 42 || err != nil || calls != 4 {
		t.Fatalf("result = %d, %v after %d calls", n, err, calls)
	}
}

func TestL2CAPDeadlineSurvivesInterruptions(t *testing.T) {
	for _, write := range []bool{false, true} {
		name := "read"
		if write {
			name = "write"
		}
		t.Run(name, func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[0])
			defer unix.Close(fds[1])
			if write {
				// Fill the send queue so Write must wait for its deadline.
				if err := unix.SetNonblock(fds[0], true); err != nil {
					t.Fatal(err)
				}
				for {
					_, err := unix.Write(fds[0], []byte("queued"))
					if errors.Is(err, unix.EAGAIN) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := unix.SetNonblock(fds[0], false); err != nil {
					t.Fatal(err)
				}
			}
			c := &l2capStreamConn{fd: fds[0]}

			// Target the thread blocked in the syscall with Go's preemption
			// signal. Stop after a second so a broken retry loop also finishes.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			tid := unix.Gettid()
			stop, done := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(20 * time.Millisecond)
				defer ticker.Stop()
				limit := time.NewTimer(time.Second)
				defer limit.Stop()
				for {
					select {
					case <-stop:
						return
					case <-limit.C:
						return
					case <-ticker.C:
						_ = unix.Tgkill(os.Getpid(), tid, unix.SIGURG)
					}
				}
			}()
			defer func() { close(stop); <-done }()

			start := time.Now()
			if err := c.SetDeadline(start.Add(200 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
			if write {
				_, err = c.Write([]byte("blocked"))
			} else {
				_, err = c.Read(make([]byte, 1))
			}
			elapsed := time.Since(start)
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Fatalf("expected timeout, got %v", err)
			}
			if elapsed < 150*time.Millisecond {
				t.Fatalf("200ms deadline expired early after %v", elapsed)
			}
			if elapsed > 700*time.Millisecond {
				t.Fatalf("interruptions extended 200ms deadline to %v", elapsed)
			}
		})
	}
}

func TestL2CAPDeadlineCanBeCleared(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	c := &l2capStreamConn{fd: fds[0]}
	if err := c.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("out")); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(fds[1], []byte("in")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	if n, err := c.Read(b); err != nil || n != 2 || string(b) != "in" {
		t.Fatalf("read after clearing deadline = %q, %d, %v", b, n, err)
	}
}

func TestL2CAPDoesNotRetryDisconnectOrTimeout(t *testing.T) {
	for _, failure := range []error{unix.ECONNABORTED, unix.ECONNRESET, unix.EAGAIN} {
		calls := 0
		n, err := retryL2CAPInterrupted(func() (int, error) {
			calls++
			return -1, failure
		})
		if n != -1 || !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("%v: result = %d, %v after %d calls", failure, n, err, calls)
		}
	}
}
