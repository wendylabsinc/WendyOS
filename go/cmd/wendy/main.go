package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/internal/cli/commands"
)

func main() {
	cmd := commands.NewRootCmd()
	if err := cmd.Execute(); err != nil {
		if errors.Is(err, commands.ErrUserCancelled) {
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

	switch {
	case strings.Contains(msg, "code = Unavailable") && strings.Contains(msg, "connection refused"):
		return fmt.Errorf("%sCould not connect to device. Is it powered on and connected to the network?", prefix)
	case strings.Contains(msg, "code = Unavailable"):
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
