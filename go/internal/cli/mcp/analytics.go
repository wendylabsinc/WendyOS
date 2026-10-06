package mcp

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// Register after the built-in tools, before discovering container tools. Only
// built-in names may enter telemetry; app and tool names can contain user data.
func registerToolAnalytics(srv *server.MCPServer) {
	builtins := srv.ListTools()
	srv.Use(func(next server.ToolHandlerFunc) server.ToolHandlerFunc {
		return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			name := req.Params.Name
			if _, ok := builtins[name]; !ok {
				name = "container_tool"
			}
			start := time.Now()
			result, err := next(ctx, req)
			props := map[string]string{
				"command_name": "wendy mcp " + name,
				"command_root": "mcp",
				"duration_ms":  strconv.FormatInt(time.Since(start).Milliseconds(), 10),
				"success":      strconv.FormatBool(err == nil && result != nil && !result.IsError),
				"is_dev_build": strconv.FormatBool(version.IsDev(version.Version)),
			}
			if class := toolErrorClass(result, err); class != "" {
				props["error_class"] = class
			}
			analytics.Track("command_executed", props)
			return result, err
		}
	})
}

func toolErrorClass(result *mcpgo.CallToolResult, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline"
	case err != nil || result == nil:
		return "other"
	case !result.IsError:
		return ""
	}
	// Inspect only the bounded error code. Never send result text, arguments,
	// or arbitrary error codes returned by app-provided tools.
	if content, ok := result.StructuredContent.(map[string]any); ok {
		code, _ := content["error_code"].(string)
		switch errorCode(code) {
		case errCodeNotConnected, errCodeInvalidArgument, errCodeDeviceUnreachable,
			errCodeEntitlementDenied, errCodeAuthRequired, errCodeMultipleSessions,
			errCodeNotFound, errCodeTimeout, errCodeCancelled, errCodeUnsupported,
			errCodeInternal:
			return strings.ToLower(code)
		}
	}
	return "tool_error"
}
