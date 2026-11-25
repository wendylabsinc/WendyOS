package commands

import (
	"context"

	"github.com/mark3labs/mcp-go/server"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// DesktopEmbeddedMCP supplies the CLI's authenticated transport and catalog to
// native hosts which cannot spawn a command process, such as iOS.
func DesktopEmbeddedMCP(ctx context.Context) (*server.MCPServer, func(), error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if auth, authErr := config.ResolveAuth(cfg, "", nil); authErr == nil {
		if err := wendymcp.RealmAllowsMCP(auth); err != nil {
			return nil, nil, err
		}
	}
	srv, err := newMCPServer("", []string{"all"})
	if err != nil {
		return nil, nil, err
	}
	return srv.Embedded(ctx)
}
