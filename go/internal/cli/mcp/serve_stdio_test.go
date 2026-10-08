package mcp

import (
	"context"
	"io"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// When the client disconnects (stdin EOF) or the server is told to stop, an
// in-flight run is cancelled, so the spawned CLI gets its graceful stop,
// instead of building and deploying for a client that is gone. The server
// returns once the run has stopped, and a disconnect is a clean exit.
func TestServeStdioCancelsInFlightRunsOnShutdown(t *testing.T) {
	for _, trigger := range []string{"client disconnect", "server stop"} {
		t.Run(trigger, func(t *testing.T) {
			s := New(&config.Config{}, nil)
			started, stopped := make(chan struct{}), make(chan struct{})
			s.runCommandFn = func(ctx context.Context, _ []string, _ commandTarget, _ int) (string, bool, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return "", false, ctx.Err()
			}
			srv := server.NewMCPServer("wendy", "test", server.WithToolCapabilities(true))
			srv.AddTool(mcpgo.NewTool("run"), s.handleRun)
			in, client := io.Pipe()
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			served := make(chan error, 1)
			go func() { served <- serveStdioStreams(ctx, srv, in, io.Discard) }()
			for _, line := range []string{
				`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
				`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run","arguments":{"project_path":"` + t.TempDir() + `","device":"vm:test"}}}`,
			} {
				if _, err := io.WriteString(client, line+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("run did not start")
			}
			if trigger == "client disconnect" {
				_ = client.Close()
			} else {
				stop()
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("in-flight run was not cancelled")
			}
			select {
			case err := <-served:
				if trigger == "client disconnect" && err != nil {
					t.Fatalf("disconnect is a clean shutdown, got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("server did not return after the run stopped")
			}
		})
	}
}
