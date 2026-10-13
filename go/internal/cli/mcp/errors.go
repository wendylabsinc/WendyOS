package mcp

import (
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type errorCode string

const (
	errCodeNotConnected      errorCode = "NOT_CONNECTED"
	errCodeInvalidArgument   errorCode = "INVALID_ARGUMENT"
	errCodeDeviceUnreachable errorCode = "DEVICE_UNREACHABLE"
	errCodeEntitlementDenied errorCode = "ENTITLEMENT_DENIED"
	errCodeAuthRequired      errorCode = "AUTH_REQUIRED"
	errCodeMultipleSessions  errorCode = "MULTIPLE_SESSIONS"
	errCodeNotFound          errorCode = "NOT_FOUND"
	errCodeTimeout           errorCode = "TIMEOUT"
	errCodeCancelled         errorCode = "CANCELLED"
	errCodeUnsupported       errorCode = "UNSUPPORTED"
	errCodeInternal          errorCode = "INTERNAL"
)

// rewriteCLIHints is pluginmode.RewriteCLIHints; a variable so tests can pose
// as a plugin-managed CLI that is not on PATH.
var rewriteCLIHints = pluginmode.RewriteCLIHints

// errResult builds an error tool result with a machine-readable code and a
// human-readable "[CODE] message" text fallback. 'wendy …' commands quoted in
// msg are made runnable when this CLI is a plugin-managed install that PATH
// does not reach (see pluginmode.CLIInvocation).
func errResult(code errorCode, msg string) *mcpgo.CallToolResult {
	msg = rewriteCLIHints(msg)
	r := mcpgo.NewToolResultStructured(
		map[string]any{"error_code": string(code), "message": msg},
		fmt.Sprintf("[%s] %s", code, msg),
	)
	r.IsError = true
	return r
}

func errResultf(code errorCode, format string, a ...any) *mcpgo.CallToolResult {
	return errResult(code, fmt.Sprintf(format, a...))
}

// codeFromGRPC maps a gRPC status error to an errorCode.
func codeFromGRPC(err error) errorCode {
	st, ok := status.FromError(err)
	if !ok {
		return errCodeInternal
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded:
		return errCodeDeviceUnreachable
	case codes.PermissionDenied:
		return errCodeEntitlementDenied
	case codes.NotFound:
		return errCodeNotFound
	case codes.InvalidArgument:
		return errCodeInvalidArgument
	case codes.Unimplemented:
		return errCodeUnsupported
	default:
		return errCodeInternal
	}
}
