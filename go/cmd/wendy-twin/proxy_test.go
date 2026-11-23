package main

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"
)

var errUnexpectedProxyFastPath = errors.New("proxy used an optional zero-copy interface")

type proxyFastReader struct {
	io.Reader
}

func (proxyFastReader) WriteTo(io.Writer) (int64, error) { return 0, errUnexpectedProxyFastPath }

type proxyFastWriter struct {
	io.Writer
}

func (proxyFastWriter) ReadFrom(io.Reader) (int64, error) { return 0, errUnexpectedProxyFastPath }

func TestProxyCopyPreservesBytesWithoutEitherZeroCopyInterface(t *testing.T) {
	payload := bytes.Repeat([]byte("\x00HTTP body\xff\r\n"), 10000)
	for _, interfaces := range []string{"both", "reader", "writer"} {
		t.Run(interfaces, func(t *testing.T) {
			var output bytes.Buffer
			var src io.Reader = struct{ io.Reader }{bytes.NewReader(payload)}
			var dst io.Writer = struct{ io.Writer }{&output}
			if interfaces != "writer" {
				src = proxyFastReader{src}
			}
			if interfaces != "reader" {
				dst = proxyFastWriter{dst}
			}
			n, err := copyProxyStream(dst, src)
			if err != nil || n != int64(len(payload)) || !bytes.Equal(output.Bytes(), payload) {
				t.Fatalf("copy returned %d bytes, %v; payload preserved=%v", n, err, bytes.Equal(output.Bytes(), payload))
			}
		})
	}
}

func TestProxyCopyUnblocksWhenDownstreamCloses(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		_, err := copyProxyStream(proxyFastWriter{writer}, proxyFastReader{bytes.NewReader(make([]byte, 128<<10))})
		done <- err
	}()
	// The consumer never drains its output. Closing it must terminate the
	// pending copy, allowing main to return and release the network socket.
	reader.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("copy error = %v, want closed pipe", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("copy remained blocked after downstream closed")
	}
}
