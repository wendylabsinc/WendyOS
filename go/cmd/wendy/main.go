package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/internal/cli/commands"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func main() {
	start := time.Now()
	cmd := commands.NewRootCmd()
	executed, err := cmd.ExecuteC()
	trackCommand(executed, err, time.Since(start))
	analytics.Close()

	if err != nil {
		if errors.Is(err, commands.ErrUserCancelled) || errors.Is(err, commands.ErrDefaultCleared) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, tui.ErrorMessage(formatError(err).Error()))
		os.Exit(1)
	}
}

// trackCommand emits a single command_executed analytics event describing the
// invocation. Properties:
//
//   - command_name: canonical cobra path (e.g. "wendy device wifi connect"),
//     never flag values or positional args.
//   - command_root: top-level token (e.g. "device") for low-cardinality
//     breakdowns that survive PostHog's 25-row table cap.
//   - duration_ms: wall-clock time from process start.
//   - success: bool serialized as "true"/"false".
//   - is_dev_build: true when version.Version == "dev".
//   - error_class (only when err != nil): bounded enum derived from err —
//     never the error message text, which can leak hostnames or paths.
func trackCommand(executed *cobra.Command, err error, dur time.Duration) {
	if executed == nil {
		return
	}
	props := map[string]string{
		"command_name": executed.CommandPath(),
		"command_root": commandRoot(executed),
		"duration_ms":  strconv.FormatInt(dur.Milliseconds(), 10),
		"success":      strconv.FormatBool(err == nil),
		"is_dev_build": strconv.FormatBool(version.Version == "dev"),
	}
	if err != nil {
		props["error_class"] = errorClass(err)
	}
	analytics.Track("command_executed", props)
}

// commandRoot returns the top-level subcommand token under the root, e.g.
// "device" for `wendy device wifi connect`. Returns the root's own name
// (typically "wendy") when invoked without a subcommand.
func commandRoot(c *cobra.Command) string {
	if c == nil {
		return ""
	}
	if !c.HasParent() {
		return c.Name()
	}
	for c.Parent() != nil && c.Parent().HasParent() {
		c = c.Parent()
	}
	return c.Name()
}

// errorClass maps an execution error to a bounded enum suitable for analytics.
// It must never embed the error message, which can contain hostnames, paths,
// or other user input.
//
// User-cancellation sentinels are checked first so an outer wrap never
// reclassifies them. gRPC errors are extracted via status.FromError, which
// walks the wrapped chain — substring matching on err.Error() would miss
// errors wrapped via fmt.Errorf with a custom prefix or any future change to
// grpc-go's stringification.
func errorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, commands.ErrUserCancelled) || errors.Is(err, commands.ErrDefaultCleared) {
		return "user_cancelled"
	}
	// status.FromError returns ok=true only for real gRPC errors (those
	// produced by the grpc package or implementing GRPCStatus()). For
	// non-gRPC errors it returns ok=false with a synthesized Unknown code,
	// which we don't want to claim as a gRPC failure. An explicit
	// Unknown code from a real gRPC error, however, should still bucket
	// under grpc_other.
	if st, ok := status.FromError(err); ok && st.Code() != codes.OK {
		switch st.Code() {
		case codes.Canceled:
			return "context_canceled"
		case codes.DeadlineExceeded:
			return "grpc_deadline"
		case codes.Unavailable:
			return "grpc_unavailable"
		case codes.Unimplemented:
			return "grpc_unimplemented"
		default:
			return "grpc_other"
		}
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline"
	}
	return "other"
}

// formatError converts raw gRPC errors into human-readable messages.
func formatError(err error) error {
	msg := err.Error()
	if !strings.Contains(msg, "rpc error: code = ") {
		return err
	}

	// Extract the context prefix (e.g. "starting agent update: ") before the rpc error.
	prefix := ""
	if idx := strings.Index(msg, "rpc error: code = "); idx > 0 {
		prefix = msg[:idx]
	}

	isPKICoreCall := strings.Contains(prefix, "pki-core")
	isCloudCall := strings.Contains(prefix, "issuing certificate") ||
		strings.Contains(prefix, "refreshing certificate") ||
		strings.Contains(prefix, "creating enrollment token") ||
		strings.Contains(prefix, "connecting to cloud")

	switch {
	case strings.Contains(msg, "code = Unavailable") && strings.Contains(msg, "connection refused"):
		if isPKICoreCall {
			return fmt.Errorf("%sCould not connect to local pki-core. Check that the gRPC endpoint is reachable from this machine.", prefix)
		}
		if isCloudCall {
			return fmt.Errorf("%sCould not connect to Wendy Cloud. Please try again later.", prefix)
		}
		return fmt.Errorf("%sCould not connect to device. Is it powered on and connected to the network?", prefix)
	case strings.Contains(msg, "code = Unavailable"):
		if isPKICoreCall {
			return fmt.Errorf("%sLocal pki-core is unavailable.", prefix)
		}
		if isCloudCall {
			return fmt.Errorf("%sWendy Cloud is unavailable. Please try again later.", prefix)
		}
		// Preserve the server's description when it provides actionable
		// detail (e.g. "WiFi management is not available (nmcli not found)").
		// Only fall back to the generic message for transport-level errors
		// that lack a useful desc.
		if idx := strings.Index(msg, "desc = "); idx >= 0 {
			desc := msg[idx+len("desc = "):]
			return fmt.Errorf("%s%s", prefix, desc)
		}
		return fmt.Errorf("%sDevice is unavailable.", prefix)
	case strings.Contains(msg, "code = DeadlineExceeded"):
		return fmt.Errorf("%sConnection timed out.", prefix)
	case strings.Contains(msg, "code = Unimplemented"):
		return fmt.Errorf("%sNot supported by this agent version. Try updating the agent.", prefix)
	default:
		// Strip transport noise, keep the desc message.
		if idx := strings.Index(msg, "desc = "); idx >= 0 {
			desc := msg[idx+len("desc = "):]
			return fmt.Errorf("%s%s", prefix, desc)
		}
		return err
	}
}
