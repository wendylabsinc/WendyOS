package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// Keep the SDK's sessions, worker pool, notifications, and cancellation. Only
// extension requests are consumed here, before the SDK's method dispatcher.
func listenWithSkills(ctx context.Context, srv *server.MCPServer, in io.Reader, out io.Writer) error {
	catalog, err := embeddedSkillCatalog()
	if err != nil {
		return err
	}
	writer := &skillStdioWriter{out: out}
	reader := &skillStdioReader{ctx: ctx, in: bufio.NewReader(in), out: writer, catalog: catalog}
	return server.NewStdioServer(srv).Listen(ctx, reader, writer)
}

type skillStdioWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *skillStdioWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

type skillStdioReader struct {
	ctx     context.Context
	in      *bufio.Reader
	out     io.Writer
	catalog *skillCatalog
	pending []byte
	err     error
}

func (r *skillStdioReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if r.err != nil {
			return 0, r.err
		}
		line, err := r.in.ReadBytes('\n')
		r.err = err
		if len(line) == 0 {
			continue
		}
		response, handled := r.catalog.dispatch(line)
		if !handled {
			r.pending = line
			break
		}
		if response != nil {
			data, err := json.Marshal(response)
			if err != nil {
				return 0, err
			}
			if _, err := r.out.Write(append(data, '\n')); err != nil {
				return 0, err
			}
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
