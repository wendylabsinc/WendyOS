package mcp

import (
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/pluginmode"
	"github.com/wendylabsinc/wendy/go/internal/shared/rpcauth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type errorCode string

const (
	errCodeNotConnected      errorCode = "NOT_CONNECTED"
	errCodeInvalidArgument   errorCode = "INVALID_ARGUMENT"
	errCodeDeviceUnreachable errorCode = "DEVICE_UNREACHABLE"
	errCodeEntitlementDenied errorCode = "ENTITLEMENT_DENIED"
	errCodeRPCIdentityDenied errorCode = "RPC_IDENTITY_DENIED"
	errCodeAuthRequired      errorCode = "AUTH_REQUIRED"
	errCodeMultipleSessions  errorCode = "MULTIPLE_SESSIONS"
	errCodeNotFound          errorCode = "NOT_FOUND"
	errCodeTimeout           errorCode = "TIMEOUT"
	errCodeCancelled         errorCode = "CANCELLED"
	errCodeUnsupported       errorCode = "UNSUPPORTED"
	errCodeInternal          errorCode = "INTERNAL"
	errCodeDenied            errorCode = "DENIED" // refused by the user's binding policy (wendy cloud mcp)
	// The call needs the user's agreement first, passed back as an explicit argument.
	errCodeConfirmationRequired errorCode = "CONFIRMATION_REQUIRED"
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
	steps := errorRecoverySteps(code)
	r := mcpgo.NewToolResultStructured(
		map[string]any{"error_code": string(code), "message": msg, "retryable": false, "next_steps": steps},
		fmt.Sprintf("[%s] %s", code, msg),
	)
	r.IsError = true
	return r
}

// A generic failure may follow a mutation that reached the device. Only
// explicitly read-only callers may recommend automatically repeating a call.
func readOnlyErrResult(code errorCode, msg string) *mcpgo.CallToolResult {
	r := errResult(code, msg)
	if code == errCodeDeviceUnreachable || code == errCodeTimeout {
		fields := r.StructuredContent.(map[string]any)
		fields["retryable"] = true
		fields["retry_after_ms"] = 1000
		fields["max_retries"] = 2
	}
	return r
}

func errorRecoverySteps(code errorCode) []string {
	switch code {
	case errCodeNotConnected:
		return []string{"Call device_list, then device_connect with the intended device selector."}
	case errCodeDeviceUnreachable, errCodeTimeout:
		return []string{"Check the connection to the same device. Inspect the operation's state before repeating a command that changes it."}
	case errCodeAuthRequired, errCodeRPCIdentityDenied:
		return []string{"Check the active account and device organization. Sign in with the authorized account before retrying."}
	case errCodeEntitlementDenied, errCodeDenied, errCodeConfirmationRequired:
		return []string{"Resolve the reported permission or confirmation requirement before retrying. Do not weaken device identity checks."}
	case errCodeInvalidArgument:
		return []string{"Correct the arguments using this tool's schema before retrying."}
	case errCodeMultipleSessions:
		return []string{"Select the intended account or session explicitly before retrying."}
	case errCodeUnsupported:
		return []string{"Check the device capabilities and CLI/Agent versions before choosing a supported operation."}
	case errCodeCancelled:
		return []string{"The operation was cancelled. Resume only when requested."}
	default:
		return []string{"Inspect the reported error and current device/app state before retrying."}
	}
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
	case codes.Unavailable:
		return errCodeDeviceUnreachable
	case codes.DeadlineExceeded:
		return errCodeTimeout
	case codes.Canceled:
		return errCodeCancelled
	case codes.PermissionDenied:
		if rpcauth.IsIdentityDenied(err) {
			return errCodeRPCIdentityDenied
		}
		return errCodeEntitlementDenied
	case codes.NotFound:
		return errCodeNotFound
	case codes.InvalidArgument:
		return errCodeInvalidArgument
	case codes.Unimplemented:
		return errCodeUnsupported
	case codes.Unauthenticated:
		// Cloud rejected the session's token, or an agent wants an operator
		// certificate: both are fixed by signing in.
		return errCodeAuthRequired
	default:
		return errCodeInternal
	}
}
