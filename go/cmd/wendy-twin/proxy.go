package main

import "io"

// copyProxyStream preserves backpressure on container exec's inherited pipes.
// Go's Linux splice fast path can busy-loop on EAGAIN when stdout is a full,
// blocking FIFO: that inherited descriptor is not registered with the netpoller.
// Hide both optional copy interfaces so each direction uses blocking Read/Write.
func copyProxyStream(dst io.Writer, src io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{dst}, struct{ io.Reader }{src})
}
