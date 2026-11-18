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
	catalog, err := embeddedSkillCatalog()
	if err != nil {
		return err
	}
	return serveStdioStreamsWithSkills(ctx, srv, in, out, catalog)
}

func serveStdioStreamsWithSkills(ctx context.Context, srv *server.MCPServer, in io.Reader, out io.Writer, catalog *skillCatalog) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	input := &cancelOnInputEnd{reader: in, cancel: cancel}
	err := listenWithSkillCatalog(ctx, srv, input, out, catalog)
	// The SDK may observe cancellation before its read result. If input ended,
	// use the recorded result while preserving other errors from Listen.
	terminal := input.terminal.Load()
	if terminal == nil || (err != nil && !errors.Is(err, context.Canceled)) {
		return err
	}
	if terminal.err == io.EOF {
		return nil
	}
	return terminal.err
}

// cancelOnInputEnd cancels serving on EOF or a read failure so in-flight tools
// can start shutting down. It records the read error before cancelling.
type cancelOnInputEnd struct {
	reader   io.Reader
	cancel   context.CancelFunc
	terminal atomic.Pointer[stdioReadError]
}

type stdioReadError struct {
	err error
}

func (c *cancelOnInputEnd) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if err != nil {
		c.terminal.Store(&stdioReadError{err: err})
		c.cancel()
	}
	return n, err
}
