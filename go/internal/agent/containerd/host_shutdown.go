package containerd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
)

// wendyLabelPrefix starts every label this agent puts on a container it
// creates: app containers carry sh.wendy/app.version, the ROS 2 sidecars their
// own sh.wendy/ros2.* labels. A container with none of them belongs to someone
// else using the same containerd namespace, and host shutdown leaves it alone.
const wendyLabelPrefix = "sh.wendy/"

// HostShutdownGrace is how long an app gets to exit after SIGTERM when the
// host is powering off, before the agent sends SIGKILL. It matches
// stopGracePeriod, the grace `wendy device apps stop` gives an app.
const HostShutdownGrace = stopGracePeriod

// shutdownTask is one task StopTasksForHostShutdown stops.
type shutdownTask struct {
	containerID string
	task        teardownTask
}

// StopTasksForHostShutdown stops every task this agent created, all at once,
// because the host is powering off. It returns how many tasks it stopped.
//
// The host would kill these processes anyway, but slowly. At power-off
// systemd-shutdown sends SIGTERM to whatever is left and waits up to 90 s
// before SIGKILL. A container's PID 1 only receives the signals it installed a
// handler for, so a Python app or a `sleep infinity` sidecar ignores that
// SIGTERM, and every power-off with apps running waited the full 90 s. Here
// each task gets SIGTERM to its whole process group, grace to exit, then
// SIGKILL, in parallel, so the wait is one grace period at most.
//
// This is deliberately not StopContainer. That is the user's stop: it marks the
// app stopped-by-user so it stays down across reboots, and releases its
// networking one app at a time. Here nothing persistent changes. Containers,
// their labels and restart policies stay as they are, so ReconcileBootContainers
// brings every app back on the next boot exactly as after a power cut.
//
// The caller must have stopped the restart monitor first. Restarts stay
// suppressed for the stopped tasks for the rest of the process's life.
func (c *Client) StopTasksForHostShutdown(ctx context.Context, grace time.Duration) (int, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx)
	if err != nil {
		return 0, fmt.Errorf("listing containers: %w", err)
	}

	var targets []shutdownTask
	var errs []error
	for _, ctr := range ctrs {
		// The listing already carries each container's labels.
		info, err := ctr.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading container %q: %w", ctr.ID(), err))
			continue
		}
		if !hasWendyLabel(info.Labels) {
			continue
		}
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			if !errdefs.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("getting task for %q: %w", ctr.ID(), err))
			}
			continue // no task: nothing is running
		}
		targets = append(targets, shutdownTask{containerID: ctr.ID(), task: task})
	}

	errs = append(errs, c.terminateTasksConcurrently(ctx, targets, grace, killWaitTimeout))
	return len(targets), errors.Join(errs...)
}

// terminateTasksConcurrently runs terminateTask on every task at once, so
// stopping n tasks takes one grace period rather than n of them.
func (c *Client) terminateTasksConcurrently(ctx context.Context, tasks []shutdownTask, grace, killWait time.Duration) error {
	errs := make([]error, len(tasks))
	var wg sync.WaitGroup
	for i, t := range tasks {
		// Never resumed: the agent is exiting, and a monitor tick that slipped
		// in before the caller stopped it must not start the app again.
		c.suppressRestarts(t.containerID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.terminateTask(ctx, t.task, t.containerID, syscall.SIGTERM, grace, killWait); err != nil {
				c.logger.Warn("Failed to stop container for host shutdown",
					zap.String(logfields.ContainerID, t.containerID), zap.Error(err))
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func hasWendyLabel(labels map[string]string) bool {
	for k := range labels {
		if strings.HasPrefix(k, wendyLabelPrefix) {
			return true
		}
	}
	return false
}
