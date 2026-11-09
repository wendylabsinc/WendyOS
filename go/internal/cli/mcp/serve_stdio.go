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
	input := &cancelOnEOF{reader: in, cancel: cancel}
	err := listenWithSkillCatalog(ctx, srv, input, out, catalog)
	if failure := input.failure.Load(); failure != nil && failure.err != io.EOF && (err == nil || errors.Is(err, context.Canceled)) {
		// The SDK may observe cancellation before its read result. Preserve the
		// input failure that caused shutdown instead of treating it as clean EOF.
		return failure.err
	}
	if input.closed.Load() && errors.Is(err, context.Canceled) {
		return nil // the client disconnected
	}
	return err
}

// cancelOnEOF cancels serving when input ends, including failures, so in-flight
// tools stop before the SDK waits for them. Only EOF is a clean disconnect.
type cancelOnEOF struct {
	reader  io.Reader
	cancel  context.CancelFunc
	closed  atomic.Bool
	failure atomic.Pointer[stdioInputFailure]
}

type stdioInputFailure struct {
	err error
}

func (c *cancelOnEOF) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	if err != nil {
		c.failure.Store(&stdioInputFailure{err: err})
		if err == io.EOF {
			c.closed.Store(true)
		}
		c.cancel()
	}
	return n, err
}
