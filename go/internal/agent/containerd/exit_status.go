package containerd

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	cgroupv2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"go.uber.org/zap"
)

// Termination reasons recorded in labelKeyExitReason. Short, stable, and
// machine-readable; the CLI maps them to friendlier text. They answer the
// question a stopped container otherwise can't: *why* did it stop.
const (
	exitReasonExited            = "exited"             // clean exit (code 0)
	exitReasonCrashed           = "crashed"            // non-zero exit code
	exitReasonOOMKilled         = "oom_killed"         // the cgroup OOM killer fired
	exitReasonStartFailed       = "start_failed"       // the task never started (image/OCI error)
	exitReasonEntitlementDenied = "entitlement_denied" // start blocked by a missing/denied entitlement
	exitReasonDeviceUnavailable = "device_unavailable" // hardware the app asked for is not present on the host
)

// ErrDeviceUnavailable marks a start refused because hardware the container was
// built around is not on the host any more.
//
// It exists to keep this failure out of the "crashed" bucket. Without it, an app
// whose GPU has gone starts normally and dies inside the CUDA runtime, so what
// reaches an operator is exit 139 and a rising failure count — indistinguishable
// from a null-pointer bug in the app itself, and diagnosable only by reading the
// container's own logs and recognising a driver error message. Refusing the
// start and naming the reason turns that into a fact the app list can show.
var ErrDeviceUnavailable = errors.New("device unavailable")

// exitCodeDidNotStart is stored as the exit code when a container failed before
// its task ran, so callers can distinguish "failed to start" from "exited 0".
const exitCodeDidNotStart = -1

// classifyExit maps a process exit code to a termination reason. An OOM kill
// overrides a plain non-zero exit, since exit 137 alone can't distinguish an
// out-of-memory kill from any other SIGKILL.
func classifyExit(code uint32, oomKilled bool) string {
	switch {
	case oomKilled:
		return exitReasonOOMKilled
	case code == 0:
		return exitReasonExited
	default:
		return exitReasonCrashed
	}
}

// entitlementErrorMarkers are substrings the agent uses in the errors it raises
// when a start is blocked by an entitlement (see ApplyEntitlements and the
// per-entitlement start checks). We match these specific phrasings rather than
// the bare word "entitlement" so an unrelated failure — e.g. pulling an image
// named "entitlement-manager" — isn't misclassified as an entitlement denial.
var entitlementErrorMarkers = []string{"applying entitlements", "entitlement requires"}

// classifyStartError maps a task create/start failure to a termination reason.
// An entitlement problem is called out explicitly; everything else is a generic
// start failure. Note: the reason is a display/diagnostic label, never an
// authorization decision.
func classifyStartError(err error) string {
	if errors.Is(err, ErrDeviceUnavailable) {
		return exitReasonDeviceUnavailable
	}
	if err != nil {
		msg := strings.ToLower(err.Error())
		for _, marker := range entitlementErrorMarkers {
			if strings.Contains(msg, marker) {
				return exitReasonEntitlementDenied
			}
		}
	}
	return exitReasonStartFailed
}

// taskOOMKilled best-effort reports whether the container's cgroup recorded an
// OOM kill. Call it right after the task exits but before it is deleted (the
// cgroup still exists then). It returns false on any error so it can never
// misreport a plain crash as an OOM.
func taskOOMKilled(ctx context.Context, task containerd.Task) bool {
	if task == nil {
		return false
	}
	metric, err := task.Metrics(ctx)
	if err != nil || metric == nil {
		return false
	}
	if !typeurl.Is(metric.Data, (*cgroupv2.Metrics)(nil)) {
		return false // cgroup v1 has no equivalent counter we rely on here
	}
	m := &cgroupv2.Metrics{}
	if err := typeurl.UnmarshalTo(metric.Data, m); err != nil {
		return false
	}
	return m.GetMemoryEvents().GetOomKill() > 0
}

// recordContainerExit persists why containerID's last run ended, as container
// labels, so a stopped/crashed container can still explain itself after its
// task (and any live output stream) are gone. Best-effort: a missing container
// or a label-write failure is logged, never fatal — recording diagnostics must
// not perturb the lifecycle it observes.
func (c *Client) recordContainerExit(ctx context.Context, containerID string, code int32, reason string, at time.Time) {
	ctx = c.withNamespace(ctx)
	ctr, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		if !errdefs.IsNotFound(err) {
			c.logger.Warn("Failed to load container to record exit",
				zap.String("container_id", containerID), zap.Error(err))
		}
		return
	}
	updateErr := ctr.Update(ctx, func(ctx context.Context, client *containerd.Client, cc *containers.Container) error {
		if cc.Labels == nil {
			cc.Labels = map[string]string{}
		}
		cc.Labels[labelKeyExitCode] = strconv.Itoa(int(code))
		cc.Labels[labelKeyExitReason] = reason
		cc.Labels[labelKeyExitAt] = at.UTC().Format(time.RFC3339)
		return nil
	})
	if updateErr != nil {
		c.logger.Warn("Failed to record container exit labels",
			zap.String("container_id", containerID), zap.Error(updateErr))
	}
}

// parseExitLabels reads the exit-diagnostics labels off a container's label
// map. ok is false when no exit was recorded (reason label absent), which the
// caller treats as "unknown" rather than "exited cleanly".
func parseExitLabels(labels map[string]string) (code int32, reason string, ok bool) {
	reason = labels[labelKeyExitReason]
	if reason == "" {
		return 0, "", false
	}
	if cs := labels[labelKeyExitCode]; cs != "" {
		if v, err := strconv.Atoi(cs); err == nil {
			code = int32(v)
		}
	}
	return code, reason, true
}

// recordStartFailure records that containerID's task never started, classifying
// the cause into a termination reason. The raw error is NOT persisted (it can
// carry registry URLs / host paths); it is already returned to the caller and
// logged on the normal error path. Best-effort.
func (c *Client) recordStartFailure(ctx context.Context, containerID string, cause error) {
	// The caller's ctx just produced an error and may already be cancelled,
	// which would drop the label write. Detach with a short timeout so the
	// diagnostic still lands — mirrors the exit-recording path in streamOutput.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	c.recordContainerExit(recCtx, containerID, exitCodeDidNotStart, classifyStartError(cause), time.Now())
}
