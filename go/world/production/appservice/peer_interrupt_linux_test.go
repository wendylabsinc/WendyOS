//go:build linux

package appservice

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxPeerPollRetriesOnlyInterruptions(t *testing.T) {
	type result struct {
		n      int
		err    error
		events int16
	}
	interrupted := result{n: -1, err: unix.EINTR}
	storm := make([]result, peerPollMaxAttempts)
	for i := range storm {
		storm[i] = interrupted
	}
	lastAttempt := append([]result(nil), storm...)
	lastAttempt[len(lastAttempt)-1] = result{}
	for name, tc := range map[string]struct {
		results []result
		alive   bool
	}{
		"live":                    {[]result{{}}, true},
		"interrupted then live":   {[]result{interrupted, {}}, true},
		"last permitted attempt":  {lastAttempt, true},
		"bounded storm":           {storm, false},
		"dead":                    {[]result{{n: 1, events: unix.POLLIN}}, false},
		"hangup":                  {[]result{{n: 1, events: unix.POLLHUP}}, false},
		"poll error":              {[]result{{n: 1, events: unix.POLLERR}}, false},
		"invalid descriptor":      {[]result{{n: 1, events: unix.POLLNVAL}}, false},
		"bad descriptor syscall":  {[]result{{n: -1, err: unix.EBADF}}, false},
		"invalid syscall":         {[]result{{n: -1, err: unix.EINVAL}}, false},
		"faulted syscall":         {[]result{{n: -1, err: unix.EFAULT}}, false},
		"allocation failure":      {[]result{{n: -1, err: unix.ENOMEM}}, false},
		"negative without error":  {[]result{{n: -1}}, false},
		"count without event":     {[]result{{n: 1}}, false},
		"event without count":     {[]result{{events: unix.POLLIN}}, false},
		"interrupted then death":  {[]result{interrupted, {n: 1, events: unix.POLLIN}}, false},
		"interrupted then badfd":  {[]result{interrupted, {n: -1, err: unix.EBADF}}, false},
		"event with interruption": {[]result{{n: -1, err: unix.EINTR, events: unix.POLLIN}}, false},
		"count with interruption": {[]result{{n: 1, err: unix.EINTR}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			// No Close: this number is a poll-function fixture, not an open fd.
			p := &linuxPeerPin{fd: 37}
			calls := 0
			err := p.aliveWithPoll(func(fds []unix.PollFd, timeout int) (int, error) {
				if len(fds) != 1 || fds[0].Fd != 37 || fds[0].Events != unix.POLLIN || fds[0].Revents != 0 || timeout != 0 {
					t.Fatal("retry changed the original descriptor or immediate poll contract")
				}
				if calls >= len(tc.results) {
					t.Fatal("unexpected retry after terminal result")
				}
				r := tc.results[calls]
				calls++
				fds[0].Revents = r.events
				return r.n, r.err
			})
			if calls != len(tc.results) || (err == nil) != tc.alive || err != nil && !errors.Is(err, ErrPeer) {
				t.Fatalf("calls=%d want=%d alive=%v want=%v err=%v", calls, len(tc.results), err == nil, tc.alive, err)
			}
		})
	}
}

func TestLinuxPeerPollNeverCachesLiveness(t *testing.T) {
	p := &linuxPeerPin{fd: 37}
	calls := 0
	poll := func(fds []unix.PollFd, timeout int) (int, error) {
		calls++
		switch calls {
		case 1:
			return 0, nil
		case 2:
			return -1, unix.EINTR
		default:
			fds[0].Revents = unix.POLLIN
			return 1, nil
		}
	}
	if err := p.aliveWithPoll(poll); err != nil {
		t.Fatal(err)
	}
	if err := p.aliveWithPoll(poll); !errors.Is(err, ErrPeer) || calls != 3 {
		t.Fatalf("earlier live result reused: calls=%d err=%v", calls, err)
	}
	p.fd = -1
	if err := p.aliveWithPoll(poll); !errors.Is(err, ErrPeer) || calls != 3 {
		t.Fatal("closed descriptor was polled")
	}
	if err := (&linuxPeerPin{fd: 37}).aliveWithPoll(nil); !errors.Is(err, ErrPeer) {
		t.Fatal("nil poll accepted")
	}
}

// The interruption is injected deterministically; the successful/dead result
// after it comes from polling the original kernel SO_PEERPIDFD for a real child.
func TestLinuxPeerPollInterruptedOriginalProcess(t *testing.T) {
	for _, exitBetween := range []bool{false, true} {
		name := "still-live"
		if exitBetween {
			name = "exited-between-attempts"
		}
		t.Run(name, func(t *testing.T) {
			p, exitPeer := interruptedPollPeer(t)
			original := p.fd
			calls := 0
			err := p.aliveWithPoll(func(fds []unix.PollFd, timeout int) (int, error) {
				calls++
				if fds[0].Fd != int32(original) || timeout != 0 {
					t.Fatal("poll did not retain original peer fd")
				}
				if calls == 1 {
					if exitBetween {
						exitPeer()
					}
					return -1, unix.EINTR
				}
				return unix.Poll(fds, timeout)
			})
			if calls < 2 || calls > peerPollMaxAttempts || (err == nil) == exitBetween {
				t.Fatalf("polls=%d exited=%v err=%v", calls, exitBetween, err)
			}
			exitPeer()
			if err := p.Alive(); !errors.Is(err, ErrPeer) {
				t.Fatal("exited original peer accepted")
			}
		})
	}
}

func TestLinuxPeerPollCloseWaitsAcrossRetries(t *testing.T) {
	p, _ := interruptedPollPeer(t)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var release sync.Once
	open := func() { release.Do(func() { close(unblock) }) }
	t.Cleanup(open)
	aliveDone, closeDone := make(chan error, 1), make(chan error, 1)
	go func() {
		calls := 0
		aliveDone <- p.aliveWithPoll(func(fds []unix.PollFd, timeout int) (int, error) {
			calls++
			if calls == 1 {
				close(entered)
				<-unblock
				return -1, unix.EINTR
			}
			return unix.Poll(fds, timeout)
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not enter")
	}
	if p.mu.TryLock() {
		p.mu.Unlock()
		t.Fatal("poll did not hold its close fence")
	}
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close crossed active poll: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	open()
	for _, done := range []<-chan error{aliveDone, closeDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("poll/Close failed to release its fence")
		}
	}
	if err := p.Alive(); !errors.Is(err, ErrPeer) {
		t.Fatal("closed original pin remained live")
	}
}

func interruptedPollPeer(t *testing.T) (*linuxPeerPin, func()) {
	t.Helper()
	path := t.TempDir() + "/peer.sock"
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_ = listener.SetDeadline(time.Now().Add(2 * time.Second))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestLinuxPublicationPeerProcess$")
	cmd.Env = append(os.Environ(), "WENDY_WORLD_TEST_PEER_SOCKET="+path, "WENDY_WORLD_TEST_PIN_PEER=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	exitPeer := func() {
		t.Helper()
		once.Do(func() {
			_ = input.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("peer helper exit: %v: %s", err, output.String())
			}
		})
	}
	t.Cleanup(exitPeer)
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	pin, err := pinOriginalPeer(conn)
	if err != nil {
		t.Fatal("Linux SO_PEERPIDFD required", err)
	}
	t.Cleanup(func() { _ = pin.Close() })
	p, ok := pin.(*linuxPeerPin)
	if !ok {
		t.Fatal("unexpected peer pin implementation")
	}
	return p, exitPeer
}
