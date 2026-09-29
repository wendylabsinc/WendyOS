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
// re-check of a record before a crash is reported. Variables so tests can
// shorten them.
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
// clean exit ("exited" means exit code 0) whether the app is listed stopped
// or crash-looping (cleanStop), a stopped app whose exit was not recorded
// (agents before WDY-1819, or an app the user stopped — the agent hides that
// exit on purpose), and a nil c.
func appExitFailure(appID string, c *agentpb.AppContainer) error {
	if c == nil || c.GetRunningState() == agentpb.AppRunningState_RUNNING || cleanStop(c) {
		return nil
	}
	if c.GetRunningState() != agentpb.AppRunningState_CRASH_LOOPING && c.GetTerminationReason() == "" {
		return nil
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
// exitsReported says a record the run has seen showed that the agent reports
// how apps exit (reportsExits); a baseline with failures above 0 shows it too.
type appBaseline struct {
	failures      uint32
	version       string
	exitsReported bool
}

// reportsExits reports whether c shows that the agent reports how apps exit:
// an exit recorded (termination_reason) or a restart counted
// (failure_count). Go agents before 2026-07-06 (WDY-1819, WDY-1826) and the
// Mac agent report neither, so on them an exit the restart policy recovered
// from looks like a replacement by another deployment.
func reportsExits(c *agentpb.AppContainer) bool {
	return c.GetTerminationReason() != "" || c.GetFailureCount() > 0
}

// seeing returns base once the run has also seen c.
func (base appBaseline) seeing(c *agentpb.AppContainer) appBaseline {
	base.exitsReported = base.exitsReported || base.failures > 0 || reportsExits(c)
	return base
}

// appVerdict is whose app a run finds on the device when it checks the one
// it deployed or followed (appReplaced, gateTarget.foreign).
type appVerdict int

const (
	// appOwn: the app is the run's own; it is judged by its records.
	appOwn appVerdict = iota
	// appWasReplaced: another deployment replaced the app.
	appWasReplaced
	// appExitUnreported: the app shows a sign of a replacement that an agent
	// reporting no exits (reportsExits) also shows for an exit its restart
	// policy recovered from — the app running with no restart counted past
	// the baseline, or crash-looping with no exit recorded — and no record
	// the run saw showed that this agent reports exits. The run cannot tell
	// which (noteExitUnreported, unreportedExitOutcome).
	appExitUnreported
)

// replacement is the verdict of a sign of a replacement that appVerdict's
// appExitUnreported describes.
func (base appBaseline) replacement() appVerdict {
	if base.exitsReported {
		return appWasReplaced
	}
	return appExitUnreported
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

// noteExitUnreported tells the developer that the run's app stopped and the
// device did not report how (appExitUnreported). Each run prints it at most
// once, where it ends on that outcome.
func noteExitUnreported(appCfg *appconfig.AppConfig) {
	cliNotice("Application %s stopped; the device did not report how it exited, so it may have crashed and been restarted by its restart policy, or another deployment may have replaced it.", containerDisplayName(appCfg))
}

// noteFollowedAppEnded is how a run that only follows an app it did not start
// reports the app's end when followedExitStands does not hold: such a run
// cannot tell a crash the restart policy recovered from from a replacement
// by another deployment (the agent keeps an app's earlier exits, and the
// failure_count it started from may already include restarts), so it says
// neither. Each run prints it at most once.
func noteFollowedAppEnded(appCfg *appconfig.AppConfig) {
	cliNotice("Application %s stopped; it may have been replaced by another deployment or restarted by its restart policy.", containerDisplayName(appCfg))
}

// followedExitStands reports whether a run that only follows an app may judge
// the app's end by its recorded exit: no sign of a replacement was seen
// (replaced is false: appReplaced's verdict was appOwn), and the last record,
// at the end of the confirmation window, still shows the app stopped with an
// exit recorded. Otherwise — the app running again at any failure_count, not
// listed, or any replacement sign — the run reports the end neutrally
// (noteFollowedAppEnded).
func followedExitStands(replaced bool, last *agentpb.AppContainer) bool {
	return !replaced && last != nil && last.GetRunningState() != agentpb.AppRunningState_RUNNING && last.GetTerminationReason() != ""
}

// appReplaced reports whether another deployment replaced an app this run
// deployed or followed (its verdict), judging c — the agent's latest record of
// the app, which the caller is about to report as a crash or as restarted by
// its restart policy, or which shows no exit or no app — against base. A caller
// confirms every such verdict this way, since no single record proves it: to
// replace an app the agent SIGKILLs its task, deletes its container, prepares
// the new image (the app is not listed: the longest phase) and creates the
// new container, which it starts afterwards, listing it as stopped — or
// crash-looping while the old registration's failure_count is above 0 — with
// no exit recorded. Its restart monitor counts the kill as a failure, so the
// new app can be listed running with that raised count until the start
// resets it to 0, and the kill's exit (137, "crashed") may be recorded late,
// on the new container, or never. The agent also never clears an exit it
// recorded, so the killed app can be listed with any exit it recorded
// before, even one its restart policy recovered from. On a device the whole
// replace can take a fifth of a second. The agent lists a task stopped
// before it records that task's exit, so a stopped record can show the
// previous exit, or none, until a later poll shows the fresh one. taskEnded
// reports whether the run knows the task it started has exited (an attached
// run's output ended); nil means it cannot know (a follow). The caller
// announces a replacement (noteAppReplaced), or that it cannot tell
// (appExitUnreported); otherwise it judges the app by evidence: the latest
// record seen that recorded an exit, else the latest record seen. last is the
// last record seen, which a follow judges instead (followedExitStands).
//
// A record on another app_version than base's is another deployment's at
// once, and an app running with no restart counted past base's
// failure_count is the app's own at once. Any other record is polled again
// for up to appReplaceConfirmWindow, and a poll decides at once only for a
// replacement:
//   - the app no longer listed after a record of it (its container was
//     deleted), or listed after it was not (a new container);
//   - a record on another app_version;
//   - a failure_count below the previous record's (a start reset it; the
//     restart policy only raises it before each restart), or an app running
//     with a failure_count at or below base's;
//   - no exit recorded after a record that was running or had one recorded
//     (a new container, created but not started).
//
// Anything else is polled again: a recorded exit — the same one, or a newly
// recorded one, which becomes the evidence (the exit the agent had not
// recorded yet) — the SIGKILL record, no exit recorded again, an unreadable
// list, or an app running with a count raised past base's, which is the
// app's own only if nothing later in the window says otherwise. When the
// window ends undecided, the last record decides:
//   - running with a raised count, or a recorded exit (the SIGKILL record
//     included): the app's own;
//   - crash-looping with no exit recorded: replaced (on agents with exit
//     reporting, WDY-1819, only a replacement's container that has not
//     started is listed that way; the Mac agent lists its crash loops that
//     way too: see below);
//   - not listed at all: replaced when taskEnded (nothing of the run's is
//     listed and its task is gone: the new image is still being prepared),
//     otherwise the app's own;
//   - stopped with no exit recorded: the app's own — how the agent lists an
//     app the user stopped (it hides that exit on purpose), and any exit on
//     an older agent, as much as a slow-starting replacement.
//
// The app running with no restart counted past base's, and a window ending
// on the app crash-looping with no exit recorded, mean replaced only once a
// record seen (base included) showed that the agent reports exits
// (reportsExits): on an agent that reports none, an exit its restart policy
// recovered from looks the same, so until then the verdict is
// appExitUnreported. The other signs need no such record — the app gone or
// back, another app_version, a count that fell — or show it themselves: a
// record that was running there had a restart counted.
//
// It returns ctx's error if ctx ends first.
func appReplaced(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, base appBaseline, c *agentpb.AppContainer, taskEnded func() bool) (verdict appVerdict, evidence, last *agentpb.AppContainer, err error) {
	if base.versionChanged(c) {
		return appWasReplaced, c, c, nil
	}
	if c.GetRunningState() == agentpb.AppRunningState_RUNNING && c.GetFailureCount() <= base.failures {
		return appOwn, c, c, nil
	}
	base = base.seeing(c)
	evidence, latest := c, c
	windowCtx, cancel := context.WithTimeout(ctx, appReplaceConfirmWindow)
	defer cancel()
	ticker := time.NewTicker(appReplaceConfirmPoll)
	defer ticker.Stop()
	for {
		select {
		case <-windowCtx.Done():
			if ctx.Err() != nil {
				return appOwn, evidence, latest, ctx.Err()
			}
			return undecidedVerdict(base, latest, taskEnded), evidence, latest, nil
		case <-ticker.C:
		}
		lookupCtx, lookupCancel := context.WithTimeout(windowCtx, appExitLookupTimeout)
		now, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		lookupCancel()
		if ctx.Err() != nil {
			return appOwn, evidence, latest, ctx.Err()
		}
		if err != nil {
			continue // unreadable: the last record stands
		}
		base = base.seeing(now)
		previous := latest
		latest = now
		if now != nil && (now.GetTerminationReason() != "" || evidence.GetTerminationReason() == "") {
			evidence = now
		}
		switch {
		case now == nil && c == nil:
			continue
		case now == nil, c == nil, base.versionChanged(now),
			now.GetFailureCount() < previous.GetFailureCount():
			return appWasReplaced, evidence, latest, nil
		case now.GetRunningState() == agentpb.AppRunningState_RUNNING:
			if now.GetFailureCount() <= base.failures {
				return base.replacement(), evidence, latest, nil
			}
			continue // restarted by its restart policy, unless a later record says otherwise
		case killedBySIGKILL(now):
			continue
		case now.GetTerminationReason() == "":
			if previous.GetRunningState() == agentpb.AppRunningState_RUNNING || previous.GetTerminationReason() != "" {
				return appWasReplaced, evidence, latest, nil
			}
			continue
		default:
			continue // a recorded exit: the app's own, unless a later record says otherwise
		}
	}
}

// undecidedVerdict is appReplaced's verdict when its window ends on latest,
// a record still undecided; see appReplaced.
func undecidedVerdict(base appBaseline, latest *agentpb.AppContainer, taskEnded func() bool) appVerdict {
	switch {
	case latest == nil:
		if taskEnded != nil && taskEnded() {
			return appWasReplaced
		}
		return appOwn
	case latest.GetRunningState() == agentpb.AppRunningState_RUNNING, latest.GetTerminationReason() != "":
		return appOwn
	case latest.GetRunningState() == agentpb.AppRunningState_CRASH_LOOPING: // no exit recorded
		return base.replacement()
	default: // stopped, no exit recorded
		return appOwn
	}
}

// cleanStop reports whether c records a clean exit (code 0) of an app that
// is not running: listed stopped, or crash-looping — failure_count counts
// every restart by the restart policy until the app's next start, so once the
// policy has restarted an app, the agent lists a clean exit it will restart
// as crash-looping until that restart. A success for a run without
// --wait-ready, which reports it at once rather than confirm it first
// (appReplaced).
func cleanStop(c *agentpb.AppContainer) bool {
	state := c.GetRunningState()
	return (state == agentpb.AppRunningState_STOPPED || state == agentpb.AppRunningState_CRASH_LOOPING) && c.GetTerminationReason() == "exited"
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
// unreadable list, an app the restart policy already brought back, one
// another deployment replaced, or one the device did not report the exit of
// in a way that tells the two apart (appExitUnreported). appReplaced confirms
// every other verdict first, the restart policy's included: a replace
// briefly lists the new app running with the kill counted as a restart.
func attachedExitOutcome(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) error {
	if conn != nil && conn.ContainerService != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, appExitLookupTimeout)
		c, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		cancel()
		if err == nil && !cleanStop(c) {
			// The output ended, so the task this run started has exited.
			taskEnded := func() bool { return true }
			verdict, evidence, _, err := appReplaced(ctx, conn, appCfg, startedAppBaseline(appCfg), c, taskEnded)
			if err != nil {
				// Ctrl-C or SIGTERM while confirming. The app this run started
				// has exited; what runs now may be another deployment's, so
				// nothing is stopped.
				return ErrUserCancelled
			}
			switch verdict {
			case appWasReplaced:
				noteAppReplaced(appCfg)
				return nil
			case appExitUnreported:
				noteExitUnreported(appCfg)
				return nil
			}
			c = evidence
			if failure := appExitFailure(appCfg.AppID, c); failure != nil {
				return failure
			}
			if c != nil && c.GetRunningState() == agentpb.AppRunningState_RUNNING && c.GetFailureCount() > 0 {
				// The monitor restarted the app before this lookup (the raised
				// count lasted the confirmation window), and a running app's
				// exit labels are hidden. Unprovable either way.
				cliNotice("Application %s exited and was restarted by its restart policy; its exit status is no longer available.", containerDisplayName(appCfg))
				return nil
			}
		}
	}
	cliLogln("\nApplication %s stopped.", containerDisplayName(appCfg))
	return nil
}
