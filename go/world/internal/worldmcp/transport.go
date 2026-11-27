package worldmcp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Serve is the bounded stdio path used by world-mcp. Embedded callers using New
// with their own SDK transport must impose equivalent framing limits themselves.
func Serve(ctx context.Context, server *mcp.Server, reader io.ReadCloser, writer io.WriteCloser) error {
	return server.Run(ctx, &mcp.IOTransport{Reader: reader, Writer: &frameWriter{writer: writer}, MaxLineLength: MaxInputBytes})
}

// The SDK writes a complete NDJSON frame in one Write, including batched
// responses. Buffering here also enforces the cap if a future SDK splits writes;
// no partial or oversized frame is ever emitted to the caller.
type frameWriter struct {
	mu      sync.Mutex
	writer  io.WriteCloser
	pending []byte
	closed  bool
}

func (w *frameWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	total := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		n := len(p)
		if end >= 0 {
			n = end + 1
		}
		if len(w.pending)+n > MaxOutputBytes {
			w.closed = true
			w.pending = nil
			return 0, errors.New("MCP output frame exceeds bound")
		}
		w.pending = append(w.pending, p[:n]...)
		p = p[n:]
		if end >= 0 {
			written, err := w.writer.Write(w.pending)
			if err != nil || written != len(w.pending) {
				w.closed = true
				w.pending = nil
				if err != nil {
					return 0, err
				}
				return 0, io.ErrShortWrite
			}
			w.pending = w.pending[:0]
		}
	}
	return total, nil
}
func (w *frameWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.pending = nil
	return w.writer.Close()
}
