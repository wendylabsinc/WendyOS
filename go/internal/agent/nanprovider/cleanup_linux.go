//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// A configuration change can already be draining when systemd stops the
// agent's entire control group. Retry an interrupted owned cleanup once, using
// the same budget; ordinary helper failures must keep their fail-closed result.
func runCleanupHelper(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), nanHelperTimeout)
	defer cancel()
	return retryInterruptedCleanup(ctx, func(ctx context.Context) error { return runHelper(ctx, args...) })
}

func retryInterruptedCleanup(ctx context.Context, attempt func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := attempt(ctx)
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.ProcessState == nil {
		return err
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		return err
	}
	return attempt(ctx)
}
