package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/internal/cli/commands"
)

func main() {
	cmd := commands.NewRootCmd()
	err := cmd.Execute()

	// Track the executed command after it completes, so we know success/failure.
	if activeCmd, _, findErr := cmd.Find(os.Args[1:]); findErr == nil && activeCmd != nil {
		success := "true"
		if err != nil {
			success = "false"
		}
		props := map[string]string{
			"command": activeCmd.CommandPath(),
			"success": success,
		}
		analytics.Track("command_executed", props)
		if err != nil {
			analytics.Track("command_error", props)
		}
	}
	analytics.Close()

	if err != nil {
		if errors.Is(err, commands.ErrUserCancelled) || errors.Is(err, commands.ErrDefaultCleared) {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", formatError(err))
		os.Exit(1)
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
