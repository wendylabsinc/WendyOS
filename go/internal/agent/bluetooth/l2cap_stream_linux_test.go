//go:build linux

package bluetooth

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestL2CAPStreamSplitsTLSFlightIntoNegotiatedSDUs(t *testing.T) {
	for _, mtu := range []int{0, 23, 512} {
		t.Run(strconv.Itoa(mtu), func(t *testing.T) {
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
			if err != nil {
				t.Fatal(err)
			}
			conn := &l2capStreamConn{fd: pair[0], sndMTU: mtu}
			defer conn.Close()
			defer unix.Close(pair[1])
			payload := bytes.Repeat([]byte("large certificate flight"), 1000)
			done := make(chan error, 1)
			go func() {
				n, err := conn.Write(payload)
				if err == nil && n != len(payload) {
					err = io.ErrShortWrite
				}
				done <- err
			}()
			effective := mtu
			if effective == 0 {
				effective = 23
			}
			var got []byte
			for len(got) < len(payload) {
				packet := make([]byte, 65536)
				n, err := unix.Read(pair[1], packet)
				if err != nil {
					t.Fatal(err)
				}
				if n > effective {
					t.Fatalf("SDU=%d exceeds MTU=%d", n, effective)
				}
				got = append(got, packet[:n]...)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("TLS stream changed across SDUs")
			}
		})
	}
}
func TestL2CAPCloseInterruptsIdleReadAndIsIdempotent(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[1])
	conn := &l2capStreamConn{fd: pair[0]}
	done := make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); done <- err }()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read succeeded after close")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not interrupt read")
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestL2CAPDeadlinesDoNotTouchClosedDescriptors(t *testing.T) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pair[1])
	conn := &l2capStreamConn{fd: pair[0]}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	for _, set := range []func(time.Time) error{conn.SetDeadline, conn.SetReadDeadline, conn.SetWriteDeadline} {
		if err := set(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("deadline on closed socket: %v", err)
		}
	}
}
