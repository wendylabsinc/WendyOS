package mcp

import (
	"context"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
)

// Embedded starts the same protocol and tool catalog without stdio or a child
// process. The caller must close it before releasing its device session.
func (s *mcpServer) Embedded(ctx context.Context) (*server.MCPServer, func(), error) {
	srv, err := s.newProtocolServer()
	if err != nil {
		return nil, nil, err
	}
	srv.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return next(cloudrequest.WithLocalMCP(ctx), req)
		}
	})
	life, cancel := context.WithCancel(ctx)
	stopApps := s.startContainerMCP(life, srv)
	var once sync.Once
	close := func() {
		once.Do(func() {
			cancel()
			stopApps()
			s.closeCloudTunnels()
			s.SetConn(nil)
		})
	}
	return srv, close, nil
}
