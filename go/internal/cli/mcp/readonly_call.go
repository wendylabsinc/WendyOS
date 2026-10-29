package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// readOnlyCallMeta is the _meta key with which a client marks a tool call
// read-only. wendy chat sets it on every call in a turn a watch report
// started (chat.ReadOnlyCallMeta).
const readOnlyCallMeta = "wendy.dev/read-only"

// readOnlyCallMiddleware runs a call marked read-only only if its tool is
// marked read-only, and with a context in which the tool's agent RPCs may only
// read (grpcclient.WithReadOnly). A tool marked read-only by mistake then still
// cannot change the device: its first write fails before it is sent.
func readOnlyCallMiddleware(srv *server.MCPServer) server.ToolHandlerMiddleware {
	return func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if req.Params.Meta == nil {
				return next(ctx, req)
			}
			if marked, _ := req.Params.Meta.AdditionalFields[readOnlyCallMeta].(bool); !marked {
				return next(ctx, req)
			}
			registered := srv.GetTool(req.Params.Name)
			if registered == nil || registered.Tool.Annotations.ReadOnlyHint == nil || !*registered.Tool.Annotations.ReadOnlyHint {
				return errResultf(errCodeDenied, "%s is not a read-only tool, and this call may only read", req.Params.Name), nil
			}
			return next(grpcclient.WithReadOnly(ctx), req)
		}
	}
}
