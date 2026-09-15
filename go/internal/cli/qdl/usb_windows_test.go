//go:build windows

package qdl

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
	"golang.org/x/sys/windows"
)

func TestWindowsEDLFilter(t *testing.T) {
	d := winusb.Device{VID: VendorQualcomm, PID: ProductEDL, CompatibleIDs: []string{`USB\Class_FF&SubClass_FF&Prot_10`}}
	if !isWindowsEDL(d) {
		t.Fatal("EDL rejected")
	}
	d.PID = 0x900e
	if isWindowsEDL(d) {
		t.Fatal("non-9008 device accepted")
	}
	d.PID = ProductEDL
	d.CompatibleIDs = []string{`USB\Class_02&SubClass_02&Prot_01`}
	if isWindowsEDL(d) {
		t.Fatal("ordinary serial interface accepted")
	}
}

// Exercise the real Firehose retry path over Windows' write adapter, not just
// error classification: partial delivery must never resend an XML command.
type timeoutWriteConn struct {
	calls             int
	failCall, partial int
}

func (c *timeoutWriteConn) Read([]byte, time.Duration) (int, error) {
	return 0, io.EOF // no backlog; send may retry only an entirely unsent request
}
func (c *timeoutWriteConn) Close() error { return nil }
func (c *timeoutWriteConn) Write(p []byte, _ time.Duration) (int, error) {
	return edlWrite(p, 512, func(b []byte) (int, error) {
		c.calls++
		if c.calls == c.failCall {
			return c.partial, windows.ERROR_SEM_TIMEOUT
		}
		return len(b), nil
	})
}

func TestWindowsFirehoseWriteRetry(t *testing.T) {
	// send adds this envelope around the command.
	overhead := len("<?xml version=\"1.0\" ?>\n<data>\n  \n</data>\n")
	for _, tc := range []struct {
		name                    string
		size, failCall, partial int
		retry                   bool
	}{
		{"no delivery", 100, 1, 0, true},
		{"partial first transfer", 1024, 1, 512, false},
		{"timeout after completed chunk", 32768, 2, 0, false},
		{"trailing ZLP", 512, 2, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &timeoutWriteConn{failCall: tc.failCall, partial: tc.partial}
			err := NewSession(c, nil).send(strings.Repeat("x", tc.size-overhead))
			if tc.retry {
				if err != nil || c.calls != 2 {
					t.Fatalf("unsent request: calls=%d err=%v", c.calls, err)
				}
			} else if err == nil || errors.Is(err, ErrTimeout) || c.calls != tc.failCall {
				t.Fatalf("delivered request was retryable/replayed: calls=%d err=%v", c.calls, err)
			}
		})
	}
}

func TestWindowsEDLWriteFraming(t *testing.T) {
	for _, size := range []int{0, 511, 512, 16384, 32768, 32769} {
		var writes []int
		n, err := edlWrite(make([]byte, size), 512, func(b []byte) (int, error) { writes = append(writes, len(b)); return len(b), nil })
		if err != nil || n != size {
			t.Fatalf("size %d: %d, %v", size, n, err)
		}
		zlps := 0
		for i, n := range writes {
			if n == 0 {
				zlps++
				if i != len(writes)-1 {
					t.Fatal("mid-transfer ZLP")
				}
			}
			if n > 16384 {
				t.Fatal("oversized WinUSB transfer")
			}
		}
		want := 0
		if size > 0 && size%512 == 0 {
			want = 1
		}
		if zlps != want {
			t.Fatalf("size %d: got %d ZLPs, want %d", size, zlps, want)
		}
	}
}

func TestWindowsEDLTransferErrors(t *testing.T) {
	n, err := edlWrite(make([]byte, 32), 512, func(b []byte) (int, error) { return 7, nil })
	if n != 7 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %d, %v", n, err)
	}
	n, err = edlWrite(make([]byte, 32), 512, func(b []byte) (int, error) { return 0, windows.ERROR_SEM_TIMEOUT })
	if n != 0 || !errors.Is(err, ErrTimeout) {
		t.Fatalf("write timeout: %d, %v", n, err)
	}
	n, err = edlReadResult(12, windows.ERROR_SEM_TIMEOUT)
	if n != 12 || err != nil {
		t.Fatalf("partial timeout read lost: %d, %v", n, err)
	}
	_, err = edlReadResult(0, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatal("empty read must be a polling timeout")
	}
	_, err = edlReadResult(12, windows.ERROR_DEVICE_NOT_CONNECTED)
	if err == nil {
		t.Fatal("disconnect must not become a successful short read")
	}
}
