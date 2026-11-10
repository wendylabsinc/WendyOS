package containerd

import (
	"context"
	"fmt"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"
)

const (
	// stopGracePeriod is how long a task gets to exit after SIGTERM before
	// teardown escalates to SIGKILL.
	stopGracePeriod = 10 * time.Second
	// killWaitTimeout bounds how long teardown waits for a task to exit after
	// SIGKILL before proceeding to delete. Delete(WithProcessKill) re-kills the
	// whole group and waits again, so a task that exits late is still reaped.
	killWaitTimeout = 5 * time.Second
)

// Tests shorten this interval to exercise the SIGKILL fallback without a
// ten-second sleep; production uses the same grace as an explicit stop.
var nanReplacementGracePeriod = stopGracePeriod
var npuStopGracePeriod = 30 * time.Second

// teardownTask is the subset of containerd.Task the teardown helpers use,
// factored out so the SIGTERM→SIGKILL escalation and group-kill fallback can
// be unit-tested without a containerd daemon.
type teardownTask interface {
	Kill(ctx context.Context, signal syscall.Signal, opts ...containerd.KillOpts) error
	Wait(ctx context.Context) (<-chan containerd.ExitStatus, error)
	Delete(ctx context.Context, opts ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error)
}

// terminateReplacedTask gives a NAN app time to cancel its publish and NDP
// handles before replacing its container. Those handles live in the host
// supplicant and survive an abrupt app SIGKILL. NPU apps use their owner's
// ordered teardown without forced escalation. Other apps retain the existing
// immediate whole-process-group kill behavior; NAN grace remains bounded.
func (c *Client) terminateReplacedTask(ctx context.Context, task teardownTask, containerID string, oldHadNAN, oldHadNPU bool) error {
	if oldHadNPU {
		return c.terminateNPUTask(ctx, task, containerID)
	}
	if oldHadNAN {
		return c.terminateTask(ctx, task, containerID, syscall.SIGTERM, nanReplacementGracePeriod, killWaitTimeout)
	}
	return c.terminateTask(ctx, task, containerID, syscall.SIGKILL, killWaitTimeout, killWaitTimeout)
}

// terminateNPUTask lets the owner release its FastRPC/HTP resources in order.
// A force-killed loaded Genie process can wedge the IQ9 beyond agent recovery.
// If cleanup cannot be confirmed, retain the task and fail the operation; never
// silently escalate to a group SIGKILL or load a second model over the first.
func (c *Client) terminateNPUTask(ctx context.Context, task teardownTask, containerID string) error {
	waitCh, err := task.Wait(ctx)
	if err != nil {
		return fmt.Errorf("cannot observe NPU task %q exit; refusing forced teardown: %w", containerID, err)
	}
	// Signal only the owner. Its child may have default signal dispositions
	// and must stay alive long enough to process the owner's cleanup protocol.
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil && !taskGone(err) {
		return fmt.Errorf("signalling NPU task %q: %w", containerID, err)
	}
	timer := time.NewTimer(npuStopGracePeriod)
	defer timer.Stop()
	select {
	case exit, ok := <-waitCh:
		if !ok {
			return fmt.Errorf("NPU task %q exit observation ended without a result; refusing forced teardown", containerID)
		}
		if _, _, err := exit.Result(); err != nil {
			return fmt.Errorf("cannot confirm NPU task %q exit; refusing forced teardown: %w", containerID, err)
		}
	case <-timer.C:
		return fmt.Errorf("NPU task %q did not exit within %s; refusing SIGKILL and replacement", containerID, npuStopGracePeriod)
	case <-ctx.Done():
		return ctx.Err()
	}
	// Delete without WithProcessKill: an unreleased descendant must cause a
	// visible failure, not another forced FastRPC teardown.
	if _, err := task.Delete(ctx); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("deleting stopped NPU task %q without forced kill: %w", containerID, err)
	}
	c.logger.Info("NPU task exited without forced teardown", zap.String("container_id", containerID))
	return nil
}

// A missing task can proceed to ordinary container metadata deletion, which
// does not kill processes. Any other lookup failure leaves NPU ownership
// unknown and must not enter the legacy forced runtime-recovery path.
func (c *Client) recoverReplacedTaskLookup(ctx context.Context, containerID string, hadNPU bool, lookupErr error) error {
	if hadNPU {
		if !errdefs.IsNotFound(lookupErr) {
			return fmt.Errorf("cannot observe NPU task %q; refusing forced teardown: %w", containerID, lookupErr)
		}
		return nil
	}
	c.forceDeleteTask(ctx, containerID)
	return nil
}

// Compile-time check that containerd.Task satisfies teardownTask.
var _ teardownTask = containerd.Task(nil)

// terminateTask stops a task and every process it spawned, then deletes it.
//
// It signals the task's whole process group (runc kill --all, which sweeps the
// container cgroup) rather than only the init process. Killing just init lets
// descendants that were double-forked or re-parented survive the teardown and
// keep exclusive resources (device nodes such as /dev/video0, ports, file
// locks) open indefinitely — the failure mode behind WDY-1818, where an app
// process outlived its replaced container by hours and blocked the camera for
// every later consumer.
//
// sig is the initial signal (SIGTERM for graceful stops, SIGKILL for forced
// teardown). If the task has not exited grace after sig, it escalates to
// SIGKILL and waits up to killWait more. The final Delete uses WithProcessKill,
// which SIGKILLs the group once more and waits for exit before deleting, so it
// is correct even when the bounded waits above expired. A failed delete is
// returned to the caller — swallowing it would leave the old task's processes
// alive with no trace in the logs.
func (c *Client) terminateTask(ctx context.Context, task teardownTask, containerID string, sig syscall.Signal, grace, killWait time.Duration) error {
	// Register the waiter before signalling so a fast exit is not missed.
	waitCh, waitErr := task.Wait(ctx)
	if waitErr != nil {
		c.logger.Warn("Failed to wait on task; killing and deleting without exit confirmation",
			zap.String("container_id", containerID),
			zap.Error(waitErr))
	}

	c.signalTaskGroup(ctx, task, containerID, sig)

	exited := false
	if waitErr == nil {
		select {
		case <-waitCh:
			exited = true
		case <-time.After(grace):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if !exited && sig != syscall.SIGKILL {
		c.logger.Warn("Task did not exit within grace period; escalating to SIGKILL",
			zap.String("container_id", containerID),
			zap.Duration("grace", grace))
		c.signalTaskGroup(ctx, task, containerID, syscall.SIGKILL)
		if waitErr == nil {
			select {
			case <-waitCh:
				exited = true
			case <-time.After(killWait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if exited {
		c.logger.Debug("Task exited after signal",
			zap.String("container_id", containerID),
			zap.String("signal", sig.String()))
	} else {
		c.logger.Warn("Task exit not confirmed; deleting with process kill",
			zap.String("container_id", containerID))
	}

	_, delErr := task.Delete(ctx, containerd.WithProcessKill)
	// A half-dead task whose runc state directory has been removed out from
	// under it (isMissingRuncStateDir) wedges this Delete forever — runc's
	// own delete path stats the directory before it will proceed, and
	// nothing else ever recreates it. This is the path that was actually
	// observed wedged on hardware: stopOne (and therefore StopContainer) call
	// terminateTask directly and previously had no recovery at all, unlike
	// the replace path's separate forceDeleteTask fallback.
	delErr = c.recoverMissingRuncStateDir(containerID, delErr, func() error {
		_, retryErr := task.Delete(ctx, containerd.WithProcessKill)
		return retryErr
	})
	if delErr != nil && !errdefs.IsNotFound(delErr) {
		return fmt.Errorf("deleting task for %q: %w", containerID, delErr)
	}
	return nil
}

// signalTaskGroup delivers sig to every process in the task (containerd's
// WithKillAll). When the group kill fails for a reason other than the task
// being gone, it falls back to signalling only the init process so a runtime
// that rejects the all-flag still gets a best-effort kill.
func (c *Client) signalTaskGroup(ctx context.Context, task teardownTask, containerID string, sig syscall.Signal) {
	err := task.Kill(ctx, sig, containerd.WithKillAll)
	if err == nil || taskGone(err) {
		return
	}
	c.logger.Warn("Failed to signal task process group; falling back to init process only",
		zap.String("container_id", containerID),
		zap.String("signal", sig.String()),
		zap.Error(err))
	if err := task.Kill(ctx, sig); err != nil && !taskGone(err) {
		c.logger.Warn("Failed to signal task init process",
			zap.String("container_id", containerID),
			zap.String("signal", sig.String()),
			zap.Error(err))
	}
}

// taskGone reports whether a kill error means the task (or its init process)
// already exited, which teardown treats as success.
func taskGone(err error) bool {
	return errdefs.IsNotFound(err) || errdefs.IsFailedPrecondition(err)
}
