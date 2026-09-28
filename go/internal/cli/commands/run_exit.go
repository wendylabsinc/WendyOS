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

// lookupAppContainer returns appID's entry from the agent's container list,
// or nil with a nil error when the agent does not report the app. Unlike
// fetchAppContainer it surfaces RPC errors and matches the app name
// case-insensitively, like lookupAppState.
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

// attachedExitOutcome decides how an attached run ends once the app's output
// stream closed on its own (io.EOF). The stream carries no exit status — the
// agent closes it with nil whenever the task exits — so the outcome comes from
// ListContainers, whose exit_code/termination_reason the agent records before
// it closes the stream. An abnormal exit returns errAppCrashed so `wendy run`
// exits non-zero. A clean exit keeps the historical "stopped" success, and so
// does anything the CLI cannot prove was a crash: no container service, an
// unreadable list, or an app the restart policy already brought back.
func attachedExitOutcome(ctx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig) error {
	if conn != nil && conn.ContainerService != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, appExitLookupTimeout)
		c, err := lookupAppContainer(lookupCtx, conn, appCfg.AppID)
		cancel()
		if err == nil {
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
