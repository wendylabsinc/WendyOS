package vm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A named pipe exercises the same overlapped ReadFile/WriteFile and cancellation
// path as a COM handle, without a serial driver, attached board, or skipped test.
func serialPipe(t *testing.T) (*windowsSerial, *os.File) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\pipe\wendy-serial-%d-%d`, os.Getpid(), time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_OVERLAPPED,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 1024, 1024, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &windowsSerial{handle: h}
	for _, op := range []*serialOp{&p.reader, &p.writer} {
		op.event, err = windows.CreateEvent(nil, 1, 0, nil)
		if err != nil {
			p.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { p.Close() })
	peerHandle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	peer := os.NewFile(uintptr(peerHandle), "serial-peer")
	t.Cleanup(func() { peer.Close() })
	ov := windows.Overlapped{HEvent: p.reader.event}
	if err := windows.ConnectNamedPipe(h, &ov); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		t.Fatal(err)
	}
	return p, peer
}

type serialTransferResult struct {
	n    int
	err  error
	data []byte
}

func awaitSerialTransfer(t *testing.T, done <-chan serialTransferResult) serialTransferResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("overlapped I/O did not complete")
		return serialTransferResult{}
	}
}

//go:noinline
func growSerialStack(depth int) {
	var scratch [4096]byte
	scratch[0] = byte(depth)
	if depth > 0 {
		growSerialStack(depth - 1)
	}
	runtime.KeepAlive(&scratch)
}

// Grow the caller's stack before blocking in transfer. GC may shrink that stack
// while the kernel owns the OVERLAPPED and byte buffer. Neither may live there.
//
//go:noinline
func serialTransferOnLargeStack(p *windowsSerial, write bool, ready chan<- struct{}) serialTransferResult {
	growSerialStack(32)
	var stack [64 * 1024]byte
	stack[0] = 0x7b
	close(ready)
	var n int
	var err error
	if write {
		n, err = p.Write(stack[:])
	} else {
		n, err = p.Read(stack[:])
	}
	runtime.KeepAlive(stack)
	return serialTransferResult{n: n, err: err, data: stack[:n]}
}

func TestWindowsSerialPendingIO(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%v", write), func(t *testing.T) {
			p, peer := serialPipe(t)
			ready := make(chan struct{})
			done := make(chan serialTransferResult, 1)
			go func() { done <- serialTransferOnLargeStack(p, write, ready) }()
			<-ready
			// With no peer read/write, both transfers must remain pending. The
			// write exceeds the pipe's 1 KiB buffer; the read has no input.
			for range 10 {
				runtime.GC()
				select {
				case result := <-done:
					t.Fatalf("I/O completed before peer activity: %+v", result)
				case <-time.After(5 * time.Millisecond):
				}
			}
			if write {
				got := make([]byte, 64*1024)
				if _, err := io.ReadFull(peer, got); err != nil {
					t.Fatal(err)
				}
				want := make([]byte, len(got))
				want[0] = 0x7b
				if !bytes.Equal(got, want) {
					t.Fatal("pending write buffer was corrupted")
				}
				result := awaitSerialTransfer(t, done)
				if result.n != len(want) || result.err != nil {
					t.Fatalf("write = %+v", result)
				}
			} else {
				if _, err := peer.Write([]byte("serial reply")); err != nil {
					t.Fatal(err)
				}
				result := awaitSerialTransfer(t, done)
				if result.n != len("serial reply") || result.err != nil {
					t.Fatalf("read = %+v", result)
				}
				if string(result.data) != "serial reply" {
					t.Fatalf("read bytes = %q", result.data)
				}
			}
		})
	}
}

func TestWindowsSerialCloseCancelsPendingIO(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprintf("write=%v", write), func(t *testing.T) {
			p, _ := serialPipe(t)
			ready := make(chan struct{})
			done := make(chan serialTransferResult, 1)
			go func() { done <- serialTransferOnLargeStack(p, write, ready) }()
			<-ready
			time.Sleep(25 * time.Millisecond)
			runtime.GC()
			var closers sync.WaitGroup
			for range 4 {
				closers.Go(func() {
					if err := p.Close(); err != nil {
						t.Errorf("Close: %v", err)
					}
				})
			}
			result := awaitSerialTransfer(t, done)
			if !errors.Is(result.err, windows.ERROR_OPERATION_ABORTED) && !errors.Is(result.err, io.ErrClosedPipe) {
				t.Fatalf("cancelled transfer = %+v", result)
			}
			closers.Wait()
			if _, err := p.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("write after close: %v", err)
			}
			if _, err := p.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("read after close: %v", err)
			}
		})
	}
}
