package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/wendylabsinc/wendy/go/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ExecutionErrorClass maps an execution error to a bounded enum suitable for analytics.
// It must never embed the error message, which can contain hostnames, paths,
// or other user input.
//
// Cancellation and deadlines take precedence over operation categories.
// Command categories identify known failures; the remaining errors use typed
// gRPC, network, filesystem, or subprocess causes. No message matching is used.
func ExecutionErrorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrUserCancelled) || errors.Is(err, ErrDefaultCleared) ||
		errors.Is(err, swifttoolchain.ErrUserCancelled) || errors.Is(err, tui.ErrCancelled) {
		return "user_cancelled"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline"
	}
	st, isGRPC := status.FromError(err)
	if isGRPC {
		switch st.Code() {
		case codes.Canceled:
			return "context_canceled"
		case codes.DeadlineExceeded:
			return "grpc_deadline"
		}
	}
	if class := ErrorClass(err); class != "" {
		return class
	}
	var unknownFlag *pflag.NotExistError
	var missingValue *pflag.ValueRequiredError
	var invalidValue *pflag.InvalidValueError
	var invalidSyntax *pflag.InvalidSyntaxError
	if errors.As(err, &unknownFlag) || errors.As(err, &missingValue) ||
		errors.As(err, &invalidValue) || errors.As(err, &invalidSyntax) {
		return "cli_usage"
	}
	// status.FromError returns ok=true only for real gRPC errors (those
	// produced by the grpc package or implementing GRPCStatus()). For
	// non-gRPC errors it returns ok=false with a synthesized Unknown code,
	// which we don't want to claim as a gRPC failure. An explicit
	// Unknown code from a real gRPC error, however, should still bucket
	// under grpc_unknown.
	if isGRPC && st.Code() != codes.OK {
		switch st.Code() {
		case codes.Unavailable:
			return "grpc_unavailable"
		case codes.Unimplemented:
			return "grpc_unimplemented"
		case codes.InvalidArgument:
			return "grpc_invalid_argument"
		case codes.NotFound:
			return "grpc_not_found"
		case codes.AlreadyExists:
			return "grpc_already_exists"
		case codes.PermissionDenied:
			return "grpc_permission_denied"
		case codes.ResourceExhausted:
			return "grpc_resource_exhausted"
		case codes.FailedPrecondition:
			return "grpc_failed_precondition"
		case codes.Aborted:
			return "grpc_aborted"
		case codes.OutOfRange:
			return "grpc_out_of_range"
		case codes.Internal:
			return "grpc_internal"
		case codes.DataLoss:
			return "grpc_data_loss"
		case codes.Unauthenticated:
			return "grpc_unauthenticated"
		case codes.Unknown:
			return "grpc_unknown"
		default:
			return "grpc_other"
		}
	}
	var dnsErr *net.DNSError
	var netErr net.Error
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &dnsErr):
		return "network_dns"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "network_timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "network_refused"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "network_unreachable"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection_closed"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		return "file_not_found"
	case errors.Is(err, syscall.ENOSPC):
		return "disk_full"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.As(err, &exitErr):
		return "process_failed"
	}
	return "other"
}
