package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// appExitLookupTimeout bounds the ListContainers call an attached run makes
// after its output stream ends, so a wedged agent cannot hang the exit.
const appExitLookupTimeout = 5 * time.Second

// appReplaceConfirmWindow and appReplaceConfirmPoll pace appReplacedAfterKill's
// re-check of a SIGKILL exit. Variables so tests can shorten them.
var (
	appReplaceConfirmWindow = 3 * time.Second
	appReplaceConfirmPoll   = 250 * time.Millisecond
)

// lookupAppContainer returns appID's entry from the agent's container list,
// or nil with a nil error when the agent does not report the app. Unlike
// fetchAppContainer it surfaces RPC errors and matches the app name
// case-insensitively.
func lookupAppContainer(ctx context.Context, conn *grpcclient.AgentConnection, appID string) (*agentpb.AppContainer, error) {
	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return nil, err
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if c := resp.GetContainer(); c != nil && strings.EqualFold(c.GetAppName(), appID) {
			return c, nil
		}
	}
}

// appExitFailure returns an errAppCrashed error when c shows that appID
// stopped abnormally: a crash loop, or a stop whose recorded termination
// reason is anything but a clean exit. It returns nil for a running app, a
// clean exit ("exited" means exit code 0), an app whose exit was not recorded
// (agents before WDY-1819, or an app the user stopped — the agent hides that
// exit on purpose), and a nil c.
func appExitFailure(appID string, c *agentpb.AppContainer) error {
	if c == nil || c.GetRunningState() == agentpb.AppRunningState_RUNNING {
		return nil
	}
	if c.GetRunningState() != agentpb.AppRunningState_CRASH_LOOPING {
		if reason := c.GetTerminationReason(); reason == "" || reason == "exited" {
			return nil
		}
	}
	return appCrashedError(appID, c)
}

// appCrashedError reports c's recorded exit as an errAppCrashed error naming
// the exit code, the agent's termination reason, and where the logs are.
func appCrashedError(appID string, c *agentpb.AppContainer) error {
	what := "stopped unexpectedly"
	if c.GetRunningState() == agentpb.AppRunningState_CRASH_LOOPING {
		what = "is crash-looping"
	}
	detail := "no exit status was recorded"
	if reason := c.GetTerminationReason(); reason != "" {
		detail = fmt.Sprintf("exit code %d, termination reason %q", c.GetExitCode(), reason)
	}
	return commandErrorf(errAppCrashed, "app %s %s (%s); see its logs with `wendy device logs --app %s`", appID, what, detail, appID)
}

// appBaseline is what a run knows about the app instance it watches, so it
// can tell a replacement by another deployment from a crash — ListContainers
// reports no container ID. failures is the failure_count the instance started
// from: the agent's restart monitor raises it before every restart it makes,
// and only a start (the agent's Register) resets it, to 0. version is the
// app_version the instance runs; "" means unknown and is never compared.
type appBaseline struct {
	failures uint32
	version  string
}

// startedAppBaseline is the baseline of an app this run started: the start
// reset its failure_count to 0, and it runs the version this run deployed
// (unknown when wendy.json sets none; the agent then records "latest").
func startedAppBaseline(appCfg *appconfig.AppConfig) appBaseline {
	return appBaseline{version: appCfg.Version}
}

// observedAppBaseline is the baseline of an app a run follows without
// starting it: c, the record the run saw when it decided to follow, whose
// failure_count may already be above 0 (after a device reboot, say).
func observedAppBaseline(c *agentpb.AppContainer) appBaseline {
	return appBaseline{failures: c.GetFailureCount(), version: c.GetAppVersion()}
}

// killedBySIGKILL reports whether c is the agent's record of a task SIGKILL
// ended: exit code 137, termination reason "crashed" (an OOM kill is recorded
// as "oom_killed" instead), stopped or crash-looping. It is also what the
// agent records when another deployment replaces the app, since its replace
// path SIGKILLs the old task — which ends an attached run's output stream too
// — and while it replaces the app it can list that record as crash-looping
// (its restart decision ignores the replace in progress).
func killedBySIGKILL(c *agentpb.AppContainer) bool {
	if c == nil || c.GetTerminationReason() != "crashed" || c.GetExitCode() != 137 {
		return false
	}
	state := c.GetRunningState()
	return state == agentpb.AppRunningState_STOPPED || state == agentpb.AppRunningState_CRASH_LOOPING
}

// versionChanged reports whether c runs another app_version than base: a
// different deployment's app. An unknown version on either side never counts.
func (base appBaseline) versionChanged(c *agentpb.AppContainer) bool {
	return base.version != "" && c.GetAppVersion() != "" && c.GetAppVersion() != base.version
}

// noteAppReplaced tells the developer that another deployment replaced the
// run's app. Each run prints it at most once, where it ends on that outcome.
func noteAppReplaced(appCfg *appconfig.AppConfig) {
	cliNotice("Application %s was replaced by another deployment.", containerDisplayName(appCfg))
}

// appReplacedAfterKill reports whether c, the record an app this run
// deployed or followed left when it stopped, shows that another deployment
// replaced it; the caller announces that (noteAppReplaced). A record on
// another app_version than base's is another deployment's outright.
// Otherwise only a SIGKILL record (killedBySIGKILL) is in doubt; for any
// other c it returns false at once. For one, it polls the agent for up to
// appReplaceConfirmWindow: an app no longer reported, on another app_version,
// stopped with no recorded exit (created, not started), or otherwise past the
// SIGKILL record with a failure_count at or below base's was replaced — a
// start resets the count, while the restart policy raises it above the
// baseline before every restart. A restart by the restart policy, or the
// SIGKILL record for the whole window, means the app really was killed:
// false, so the caller reports c as a crash. Unreadable polls keep the
// SIGKILL record's verdict. It returns ctx's error if ctx ends first.
func appReplacedAfterKill(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, base appBaseline, c *agentpb.AppContainer) (bool, error) {
	if base.versionChanged(c) {
		return true, nil
	}
	if !killedBySIGKILL(c) {
		return false, nil
	}
	windowCtx, cancel := context.WithTimeout(ctx, appReplaceConfirmWindow)
	defer cancel()
	ticker := time.NewTicker(appReplaceConfirmPoll)
	defer ticker.Stop()
	for {
		select {
		case <-windowCtx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
		lookupCtx, lookupCancel := context.WithTimeout(windowCtx, appExitLookupTimeout)
		now, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		lookupCancel()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		switch {
		case err != nil:
			continue // still the killed task's record, as far as we can tell
		case now == nil, base.versionChanged(now):
			return true, nil
		case killedBySIGKILL(now):
			continue
		case now.GetRunningState() == agentpb.AppRunningState_STOPPED && now.GetTerminationReason() == "",
			now.GetFailureCount() <= base.failures:
			return true, nil
		default:
			return false, nil // restarted (or about to be) by its restart policy
		}
	}
}

// attachedExitOutcome decides how an attached run ends once the app's output
// stream closed on its own (io.EOF). The stream carries no exit status — the
// agent closes it with nil whenever the task exits — so the outcome comes from
// ListContainers, whose exit_code/termination_reason the agent records before
// it closes the stream. An abnormal exit returns errAppCrashed so `wendy run`
// exits non-zero. A clean exit keeps the historical "stopped" success, and so
// does anything the CLI cannot prove was a crash: no container service, an
// unreadable list, an app the restart policy already brought back, or one
// another deployment replaced (see appReplacedAfterKill).
func attachedExitOutcome(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) error {
	if conn != nil && conn.ContainerService != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, appExitLookupTimeout)
		c, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		cancel()
		if err == nil {
			replaced, err := appReplacedAfterKill(ctx, conn, appCfg, startedAppBaseline(appCfg), c)
			if err != nil {
				// Ctrl-C or SIGTERM while confirming. The app this run started
				// has exited; what runs now may be another deployment's, so
				// nothing is stopped.
				return ErrUserCancelled
			}
			if replaced {
				noteAppReplaced(appCfg)
				return nil
			}
			if failure := appExitFailure(appCfg.AppID, c); failure != nil {
				return failure
			}
			if c != nil && c.GetRunningState() == agentpb.AppRunningState_RUNNING && c.GetFailureCount() > 0 {
				// The monitor restarted the app before this lookup, and a
				// running app's exit labels are hidden. Unprovable either way.
				cliNotice("Application %s exited and was restarted by its restart policy; its exit status is no longer available.", containerDisplayName(appCfg))
				return nil
			}
		}
	}
	cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
	return nil
}
