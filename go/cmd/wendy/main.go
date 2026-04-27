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
	"github.com/wendylabsinc/wendy/internal/shared/version"
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
		fmt.Fprintf(os.Stderr, "Error: %v\n", formatError(err))
		os.Exit(1)
	}
}

// trackCommand emits a single command_executed analytics event describing the
// invocation. The schema is documented in Documentation/Analytics.md; in short:
//
//   - command_name is the canonical cobra path (e.g. "wendy device wifi connect"),
//     never flag values or positional args.
//   - command_root is the top-level token (e.g. "device") to give dashboards a
//     low-cardinality breakdown axis that survives PostHog's 25-row table cap.
//   - duration_ms is the wall-clock time from process start.
//   - On failure, error_class is a bounded enum derived from err — never the
//     error message text, which can leak hostnames or paths.
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
func errorClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, commands.ErrUserCancelled) || errors.Is(err, commands.ErrDefaultCleared) {
		return "user_cancelled"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline"
	}
	msg := err.Error()
	if !strings.Contains(msg, "rpc error: code = ") {
		return "other"
	}
	switch {
	case strings.Contains(msg, "code = Unavailable"):
		return "grpc_unavailable"
	case strings.Contains(msg, "code = DeadlineExceeded"):
		return "grpc_deadline"
	case strings.Contains(msg, "code = Unimplemented"):
		return "grpc_unimplemented"
	default:
		return "grpc_other"
	}
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

	isCloudCall := strings.Contains(prefix, "issuing certificate") || strings.Contains(prefix, "refreshing certificate") || strings.Contains(prefix, "connecting to cloud")

	switch {
	case strings.Contains(msg, "code = Unavailable") && strings.Contains(msg, "connection refused"):
		if isCloudCall {
			return fmt.Errorf("%sCould not connect to Wendy Cloud. Please try again later.", prefix)
		}
		return fmt.Errorf("%sCould not connect to device. Is it powered on and connected to the network?", prefix)
	case strings.Contains(msg, "code = Unavailable"):
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
