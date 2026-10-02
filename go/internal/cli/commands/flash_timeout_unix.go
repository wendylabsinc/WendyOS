//go:build !windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// errCommandTimedOut marks a command runWithTimeout gave up on.
var errCommandTimedOut = errors.New("timed out")

// runWithTimeout runs a command like CombinedOutput but gives up after timeout.
// It sends SIGTERM, then WaitDelay kills it and stops a surviving child's open
// pipe from blocking Wait. A process stuck in kernel I/O outlives even that, so
// past a hard deadline it is abandoned rather than waited for.
func runWithTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timedOut := fmt.Errorf("%s %s: %w after %s", name, strings.Join(args, " "), errCommandTimedOut, timeout)
	select {
	case err := <-done:
		if ctx.Err() == context.DeadlineExceeded {
			return out.Bytes(), timedOut
		}
		return out.Bytes(), err
	case <-time.After(timeout + 5*time.Second):
		return nil, timedOut
	}
}
