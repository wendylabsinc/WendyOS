package mcp

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
)

// serveStdioProcess serves MCP on this process's stdin and stdout until the
// client disconnects or the process is interrupted or terminated.
func serveStdioProcess(srv *server.MCPServer) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveStdioStreams(ctx, srv, os.Stdin, os.Stdout)
}

// serveStdioStreams serves MCP until ctx is done or the client disconnects.
// Either way the context of in-flight tool calls is cancelled, so a running
// `wendy run` gets its graceful stop (stopRunOnCancel) instead of deploying for
// a client that is gone; Listen returns once those calls have finished.
func serveStdioStreams(ctx context.Context, srv *server.MCPServer, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	input := &cancelOnEOF{reader: in, cancel: cancel}
	err := server.NewStdioServer(srv).Listen(ctx, input, out)
	if input.closed.Load() && errors.Is(err, context.Canceled) {
		return nil // the client disconnected
	}
	return err
}

// cancelOnEOF cancels the serving context when the client's input ends.
type cancelOnEOF struct {
	reader io.Reader
	cancel context.CancelFunc
	closed atomic.Bool
}

func (c *cancelOnEOF) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if err != nil {
		c.closed.Store(true)
		c.cancel()
	}
	return n, err
}
