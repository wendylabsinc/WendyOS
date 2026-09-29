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

// appReplaceConfirmWindow and appReplaceConfirmPoll pace appReplaced's
// re-check of an uncertain record. Variables so tests can shorten them.
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

// appRecordUncertain reports whether c, the agent's record of an app that
// stopped or failed a check, may show another deployment replacing the app
// rather than the app's own exit. To replace an app the agent SIGKILLs its
// task, deletes its container, prepares the new image and creates the new
// container, which it starts afterwards; the SIGKILL record may not be listed
// at all (its recording races the delete). So a record can be: the SIGKILL
// record (killedBySIGKILL); nil, the app not listed (the longest phase); or
// the new container created but not started — stopped, or crash-looping while
// the old registration's failure_count is above 0, with no exit recorded. On
// agents with exit reporting (WDY-1819) a crash always records its exit, so a
// stopped app with none recorded is otherwise one stopped by the user (the
// agent hides that exit on purpose); on older agents every exit is unrecorded.
func appRecordUncertain(c *agentpb.AppContainer) bool {
	return c == nil || killedBySIGKILL(c) ||
		(c.GetRunningState() != agentpb.AppRunningState_RUNNING && c.GetTerminationReason() == "")
}

// appReplaced reports whether another deployment replaced an app this run
// deployed or followed, judging c — the agent's latest record of the app
// after it stopped or failed a check — against base. taskEnded reports
// whether the run knows the task it started has exited (an attached run's
// output ended); nil means it cannot know (a follow). The caller announces a
// replacement (noteAppReplaced); otherwise it judges the app by evidence: the
// latest record seen that recorded an exit, else the latest record seen.
//
// A record on another app_version than base's is another deployment's at
// once, and any other record but an uncertain one (appRecordUncertain) is the
// app's own: false at once. An uncertain record is polled again for up to
// appReplaceConfirmWindow, and each poll decides:
//   - replaced: the app no longer listed after a record of it (its container
//     was deleted), listed after it was not (a new container), on another
//     app_version, stopped with no exit recorded after the SIGKILL record (the
//     recorded exit went with the old container), or running or with a
//     recorded exit at or below base's failure_count (a start resets the
//     count; the restart policy raises it above the baseline before every
//     restart);
//   - the app's own: running or with a recorded exit above base's
//     failure_count (restarted by its restart policy);
//   - undecided (poll again): the same uncertain kind of record, or an
//     unreadable list.
//
// When the window ends undecided, the last record decides:
//   - the SIGKILL record: the app's own (it really was killed);
//   - crash-looping with no exit recorded: replaced (only a replacement's
//     container that has not started is listed that way on agents with exit
//     reporting; an older agent's crash loop reads as replaced too);
//   - not listed at all: replaced when taskEnded (nothing of the run's is
//     listed and its task is gone: the new image is still being prepared),
//     otherwise the app's own;
//   - stopped with no exit recorded: the app's own (a stop by the user, or an
//     older agent's exit, as much as a slow-starting replacement).
//
// It returns ctx's error if ctx ends first.
func appReplaced(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, base appBaseline, c *agentpb.AppContainer, taskEnded func() bool) (replaced bool, evidence *agentpb.AppContainer, err error) {
	if base.versionChanged(c) {
		return true, c, nil
	}
	if !appRecordUncertain(c) {
		return false, c, nil
	}
	evidence, latest := c, c
	windowCtx, cancel := context.WithTimeout(ctx, appReplaceConfirmWindow)
	defer cancel()
	ticker := time.NewTicker(appReplaceConfirmPoll)
	defer ticker.Stop()
	for {
		select {
		case <-windowCtx.Done():
			if ctx.Err() != nil {
				return false, evidence, ctx.Err()
			}
			return replacedWhenUndecided(latest, taskEnded), evidence, nil
		case <-ticker.C:
		}
		lookupCtx, lookupCancel := context.WithTimeout(windowCtx, appExitLookupTimeout)
		now, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		lookupCancel()
		if ctx.Err() != nil {
			return false, evidence, ctx.Err()
		}
		if err != nil {
			continue // unreadable: the last record stands
		}
		previous := latest
		latest = now
		if now != nil && (now.GetTerminationReason() != "" || evidence.GetTerminationReason() == "") {
			evidence = now
		}
		switch {
		case now == nil && c == nil:
			continue
		case now == nil, c == nil, base.versionChanged(now):
			return true, evidence, nil
		case killedBySIGKILL(now):
			continue
		case appRecordUncertain(now):
			if previous.GetTerminationReason() != "" {
				return true, evidence, nil
			}
			continue
		case now.GetFailureCount() <= base.failures:
			return true, evidence, nil
		default:
			return false, evidence, nil // restarted (or about to be) by its restart policy
		}
	}
}

// replacedWhenUndecided is appReplaced's verdict when its window ends on
// latest, a record still uncertain; see appReplaced.
func replacedWhenUndecided(latest *agentpb.AppContainer, taskEnded func() bool) bool {
	switch {
	case latest == nil:
		return taskEnded != nil && taskEnded()
	case killedBySIGKILL(latest):
		return false
	default: // no exit recorded
		return latest.GetRunningState() == agentpb.AppRunningState_CRASH_LOOPING
	}
}

// appStoppable reports whether c, the latest record judged of an app that
// failed --wait-ready, shows something of it to stop: not an app the agent no
// longer lists — a stop issued while the new container is being created
// waits for it and then stops it — nor one stopped with no exit recorded:
// stopped by the user already, or a replacement's container not started yet.
func appStoppable(c *agentpb.AppContainer) bool {
	return c != nil && (c.GetRunningState() != agentpb.AppRunningState_STOPPED || c.GetTerminationReason() != "")
}

// agentAppVersion is the app_version the agent records for a deployment of
// appCfg: its wendy.json version, or "latest" when it sets none.
func agentAppVersion(appCfg *appconfig.AppConfig) string {
	if appCfg.Version == "" {
		return "latest"
	}
	return appCfg.Version
}

// attachedExitOutcome decides how an attached run ends once the app's output
// stream closed on its own (io.EOF). The stream carries no exit status — the
// agent closes it with nil whenever the task exits — so the outcome comes from
// ListContainers, whose exit_code/termination_reason the agent records before
// it closes the stream. An abnormal exit returns errAppCrashed so `wendy run`
// exits non-zero. A clean exit keeps the historical "stopped" success, and so
// does anything the CLI cannot prove was a crash: no container service, an
// unreadable list, an app the restart policy already brought back, or one
// another deployment replaced (see appReplaced).
func attachedExitOutcome(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) error {
	if conn != nil && conn.ContainerService != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, appExitLookupTimeout)
		c, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		cancel()
		if err == nil {
			// The output ended, so the task this run started has exited.
			taskEnded := func() bool { return true }
			replaced, evidence, err := appReplaced(ctx, conn, appCfg, startedAppBaseline(appCfg), c, taskEnded)
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
			c = evidence
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
