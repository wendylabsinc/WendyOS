// Package networkmanager coordinates reloads of host network ownership rules.
package networkmanager

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const reloadTimeout = 5 * time.Second

var hostReloader = newReloader(exec.LookPath, func(ctx context.Context, path string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, path, args...).CombinedOutput()
})

// Reload serializes the complete manager pair across NAN apps and the mesh
// provider. systemd-networkd also reloads independently, so its specific busy
// response is retried. A successful subsequent reload is required: an in-flight
// reload may have read its rule files before our changes were written.
func Reload(ctx context.Context) error { return hostReloader.reload(ctx) }

type reloader struct {
	gate       chan struct{}
	lookPath   func(string) (string, error)
	run        func(context.Context, string, ...string) ([]byte, error)
	retryDelay time.Duration
}

func newReloader(lookPath func(string) (string, error), run func(context.Context, string, ...string) ([]byte, error)) *reloader {
	return &reloader{gate: make(chan struct{}, 1), lookPath: lookPath, run: run, retryDelay: 100 * time.Millisecond}
}

func (r *reloader) reload(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, reloadTimeout)
	defer cancel()
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, args := range [][]string{{"networkctl", "reload"}, {"nmcli", "general", "reload", "conf"}} {
		path, err := r.lookPath(args[0])
		if errors.Is(err, exec.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		var lastFailure error
		for {
			if err := ctx.Err(); err != nil {
				return errors.Join(lastFailure, err)
			}
			out, err := r.run(ctx, path, args[1:]...)
			if err == nil {
				break
			}
			failure := fmt.Errorf("%s: %w: %s", args[0], err, out)
			lastFailure = failure
			if args[0] != "networkctl" || !alreadyReloading(string(out)) {
				return errors.Join(failure, ctx.Err())
			}
			timer := time.NewTimer(r.retryDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(failure, ctx.Err())
			}
		}
	}
	return nil
}

func alreadyReloading(output string) bool {
	for _, field := range strings.Fields(output) {
		if field == "io.systemd.Network.AlreadyReloading" {
			return true
		}
	}
	return false
}
