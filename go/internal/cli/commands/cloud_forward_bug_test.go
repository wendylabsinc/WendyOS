package commands

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// halfCloseConn wraps a net.Conn and allows its read side and write side to
// be closed independently, simulating TCP half-close for net.Pipe connections.
type halfCloseConn struct {
	net.Conn
	mu       sync.Mutex
	readEOF  bool // true once the read side has been "half-closed" (EOF signalled)
	writeEOF bool // true once the write side has been "half-closed"
}

func (h *halfCloseConn) Read(b []byte) (int, error) {
	h.mu.Lock()
	eof := h.readEOF
	h.mu.Unlock()
	if eof {
		return 0, io.EOF
	}
	return h.Conn.Read(b)
}

func (h *halfCloseConn) Write(b []byte) (int, error) {
	h.mu.Lock()
	eof := h.writeEOF
	h.mu.Unlock()
	if eof {
		return 0, io.ErrClosedPipe
	}
	return h.Conn.Write(b)
}

// CloseRead signals EOF to future Read calls without closing the underlying conn.
func (h *halfCloseConn) CloseRead() {
	h.mu.Lock()
	h.readEOF = true
	h.mu.Unlock()
}

// CloseWrite signals an error to future Write calls without closing the underlying conn.
func (h *halfCloseConn) CloseWrite() {
	h.mu.Lock()
	h.writeEOF = true
	h.mu.Unlock()
}

// TestServeTunnelConn_HalfCloseDataLoss verifies that the relay waits for
// both relay directions before returning.
//
// Root cause (fixed): serveTunnelConn waited on <-done only ONCE, but done
// has capacity 2. When the client finished sending, the client->tunnel
// io.Copy completed and sent to done. The function returned after a single
// <-done, triggering deferred closes that killed the tunnel->client relay
// before the backend response arrived. Result: 0-byte responses.
//
// Fix: wait for BOTH goroutines with two <-done receives.
func TestServeTunnelConn_HalfCloseDataLoss(t *testing.T) {
	const payload = "hello from backend"

	// Create the two pipe pairs.
	clientPipe, tcpPipe := net.Pipe()
	backendPipe, tunnelPipe := net.Pipe()

	// Wrap both relay-owned ends so we can inject half-close behaviour.
	tcpConn := &halfCloseConn{Conn: tcpPipe}
	tunnelConn := &halfCloseConn{Conn: tunnelPipe}

	// responseRecv collects what the client reads from the relay.
	responseRecv := make(chan string, 1)

	// Client goroutine: send request, signal half-close (read-EOF on the
	// relay's tcpConn), then read the response.
	go func() {
		// Send the request.
		_, _ = clientPipe.Write([]byte("request"))
		// Simulate half-close: tell the relay's tcpConn.Read to return EOF.
		// This makes relay(tunnelConn, tcpConn) finish.
		tcpConn.CloseRead()
		// Now read whatever the relay forwards back to us.
		buf := make([]byte, 512)
		_ = clientPipe.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _ := clientPipe.Read(buf)
		responseRecv <- string(buf[:n])
	}()

	// Backend goroutine: read request, pause (to widen the race), send response.
	go func() {
		buf := make([]byte, 512)
		_, _ = backendPipe.Read(buf)
		// Pause to widen the race window: by the time we send the response,
		// the client->tunnel relay goroutine will have finished. With the bug
		// (single <-done), testRelayBothDirs returns here and deferred closes
		// kill tunnelConn before Write below can propagate.
		time.Sleep(30 * time.Millisecond)
		_, _ = backendPipe.Write([]byte(payload))
		// Signal EOF to the relay's tunnel->client direction.
		tunnelConn.CloseRead()
		_ = backendPipe.Close()
	}()

	// Run the relay.
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		testRelayBothDirs(tcpConn, tunnelConn)
	}()

	// Wait for the client to receive its response.
	select {
	case resp := <-responseRecv:
		if resp != payload {
			t.Errorf("client received %q; want %q — response was dropped by premature relay teardown", resp, payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("test timed out waiting for client response")
	}

	select {
	case <-relayDone:
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not finish within timeout")
	}
}

// testRelayBothDirs is the relay logic from serveTunnelConn in
// cloud_forward.go, reproduced here for in-process testing.
// Keep in sync with serveTunnelConn.
func testRelayBothDirs(tcpConn, tunnelConn io.ReadWriteCloser) {
	defer tcpConn.Close()
	defer tunnelConn.Close()

	done := make(chan struct{}, 2)
	relay := func(dst io.Writer, src io.Reader) {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(dst, src)
	}
	go relay(tunnelConn, tcpConn)
	go relay(tcpConn, tunnelConn)
	<-done
	<-done // fix: wait for BOTH relay directions before closing connections
}

// Ensure the standard library's bytes.Buffer implements io.ReadWriteCloser
// (used as a compile-time check only, not in tests).
var _ io.ReadWriteCloser = (*nopCloser)(nil)

type nopCloser struct{ *bytes.Buffer }

func (nopCloser) Close() error { return nil }
